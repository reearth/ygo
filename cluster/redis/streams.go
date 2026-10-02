// Package-internal: the Redis Streams delivery tier. See Config.Transport.
package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/reearth/ygo/cluster"
)

// Transport selects how a Relay moves payloads between nodes.
type Transport int

const (
	// PubSub is the zero value: Redis pub/sub, at-most-once. Redis drops slow
	// subscribers server-side, so a payload missed is a payload gone.
	PubSub Transport = iota
	// Streams is the at-least-once tier: one Redis stream per room, replayed
	// from the oldest retained entry, bounded by retention and length.
	Streams
	// Both publishes to and reads from both tiers, for zero-downtime
	// migration. Needs no deduplication: V1 updates are idempotent.
	Both
)

// String makes Transport readable in logs and test failures.
func (t Transport) String() string {
	switch t {
	case PubSub:
		return "pubsub"
	case Streams:
		return "streams"
	case Both:
		return "both"
	default:
		return fmt.Sprintf("Transport(%d)", int(t))
	}
}

// usesStreams reports whether this transport reads from and writes to streams.
func (t Transport) usesStreams() bool { return t == Streams || t == Both }

// usesPubSub reports whether this transport reads from and writes to channels.
// Publish consults it to skip the pub/sub hand-off entirely in Streams-only
// mode, rather than merely ignoring its result: a Streams deployment must not
// still depend on the at-most-once channel it exists to replace.
func (t Transport) usesPubSub() bool { return t == PubSub || t == Both }

// Stream tier defaults. See the corresponding Config fields for rationale.
const (
	defaultStreamPrefix       = "ygo:stream:"
	defaultStreamRetention    = 60 * time.Second
	defaultStreamMaxLen       = int64(4096)
	defaultAwarenessMaxLen    = int64(64)
	defaultAwarenessRetention = 10 * time.Second
	defaultReaders            = 4
	defaultTrimInterval       = 30 * time.Second
	defaultReadBlock          = maxReadBlock
)

// maxKeysPerRead bounds how many stream keys go into one XREAD. Not
// configurable: an operator has no basis for choosing it, and Readers already
// controls concurrency. Without it, 10k rooms across 4 readers would build a
// ~5000-argument command every cycle. keyBatches enforces it.
const maxKeysPerRead = 512

// maxReadBlock is the largest Config.ReadBlock this package accepts, and its
// default. resolveStreamCfg REJECTS a larger value rather than capping it, so
// the knob can never be set to a number that does not happen.
//
// The ceiling exists because A BLOCKED XREAD CANNOT BE INTERRUPTED: go-redis
// arms the socket read deadline from ctx.Deadline() only
// (internal/pool.(*Conn).deadline, v9.18.0; withConn has no cancellation
// watcher). One reader's read interval is therefore two latencies at once: how
// long Close waits for a reader parked in an XREAD (measured 4.80s at a 5s
// ReadBlock, in a path already fixed twice, #202/#229), and how long a room
// activated after a reader's key set was fixed waits to be read at all.
//
// Waking a reader out of its read would mean closing its connection — churn
// proportional to room churn, a worse trade at this tier's 10k-room target
// than a few extra idle XREADs per second. The idle cost is Readers x
// ceil(keys-per-reader / maxKeysPerRead) XREADs per ReadBlock: 16/s with one
// batch per reader, ~160/s at 10k rooms across 4 readers. So lowering ReadBlock
// is supported; raising it is not offered, because it could not be delivered.
const maxReadBlock = 250 * time.Millisecond

// minReadBlock is the smallest Config.ReadBlock this package accepts.
// resolveStreamCfg REJECTS a smaller value rather than degrading it silently.
//
// The floor exists because go-redis builds the BLOCK argument as
// int64(block / time.Millisecond), so a sub-millisecond value truncates to 0,
// and Redis reads BLOCK 0 as block forever: the reader would never return and
// Close's wg.Wait would hang, with nothing able to wake it (see maxReadBlock).
const minReadBlock = 1 * time.Millisecond

// stalledBackoffBase is the first wait after a room's cursor advance is
// declined for lane backpressure. It doubles per consecutive stalled cycle and
// is capped at ReadBlock; see stallBackoff. Not configurable, for the same
// reason as maxKeysPerRead.
const stalledBackoffBase = 50 * time.Millisecond

// streamCfg is Config's stream half with defaults resolved, so no code past
// construction has to reason about zero values.
type streamCfg struct {
	transport    Transport
	prefix       string
	retention    time.Duration
	maxLen       int64
	awMaxLen     int64
	awRetention  time.Duration
	readers      int
	trimInterval time.Duration
	readBlock    time.Duration
}

// resolveStreamCfg fills defaults and rejects configurations that would fail
// confusingly later rather than obviously now.
//
// It validates NOTHING in PubSub mode: an existing caller has never set these
// fields, and must not start failing construction because a new field exists.
func resolveStreamCfg(client *goredis.Client, cfg Config) (streamCfg, error) {
	switch cfg.Transport {
	case PubSub, Streams, Both:
	default:
		return streamCfg{}, fmt.Errorf("cluster/redis: unknown Config.Transport %d", int(cfg.Transport))
	}

	sc := streamCfg{
		transport:    cfg.Transport,
		prefix:       cfg.StreamPrefix,
		retention:    cfg.StreamRetention,
		maxLen:       cfg.StreamMaxLen,
		awMaxLen:     cfg.AwarenessMaxLen,
		awRetention:  cfg.AwarenessRetention,
		readers:      cfg.Readers,
		trimInterval: cfg.TrimInterval,
		readBlock:    cfg.ReadBlock,
	}
	if sc.prefix == "" {
		sc.prefix = defaultStreamPrefix
	}
	if sc.retention <= 0 {
		sc.retention = defaultStreamRetention
	}
	if sc.maxLen <= 0 {
		sc.maxLen = defaultStreamMaxLen
	}
	if sc.awMaxLen <= 0 {
		sc.awMaxLen = defaultAwarenessMaxLen
	}
	if sc.awRetention <= 0 {
		sc.awRetention = defaultAwarenessRetention
	}
	if sc.readers <= 0 {
		sc.readers = defaultReaders
	}
	if sc.trimInterval <= 0 {
		sc.trimInterval = defaultTrimInterval
	}
	// Only the UNSET value is defaulted: a negative ReadBlock must reach the
	// minReadBlock rejection below, as its doc promises.
	if sc.readBlock == 0 {
		sc.readBlock = defaultReadBlock
	}

	if !sc.transport.usesStreams() {
		return sc, nil
	}

	if pool := client.Options().PoolSize; pool <= sc.readers {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: PoolSize (%d) must exceed Readers (%d): a reader holds a pool connection for as long as its XREAD blocks (up to ReadBlock, %s), so a pool no larger than Readers leaves publishes and the trim sweeper waiting on a connection every cycle",
			pool, sc.readers, sc.readBlock)
	}
	// Rejected, not capped: a knob whose value is silently ignored above some
	// threshold is worse than one with a documented range. See maxReadBlock.
	if sc.readBlock > maxReadBlock {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: ReadBlock (%s) must not exceed %s: the interval between a reader's XREADs is also how long Close and a newly activated room wait, and a blocked XREAD cannot be interrupted",
			sc.readBlock, maxReadBlock)
	}
	if sc.readBlock < minReadBlock {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: ReadBlock (%s) must not be less than %s: go-redis truncates sub-millisecond values to 0, which Redis reads as BLOCK 0 (block forever), so the reader would hang and Close would deadlock waiting for it",
			sc.readBlock, minReadBlock)
	}
	if sc.trimInterval >= sc.retention {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: TrimInterval (%s) must be less than StreamRetention (%s): a sweeper slower than the window it enforces cannot enforce it",
			sc.trimInterval, sc.retention)
	}
	return sc, nil
}

// Stream-kind discriminators. The room name is APPENDED to one of these, so the
// discriminator sits ahead of every caller-supplied byte.
//
// internal/roomname.Valid accepts every printable character, ":" included, to
// match the y-websocket JS server, so a room name may contain anything a key
// may. Under a suffix layout — awKey = prefix+"aw:"+room — a room named
// "aw:foo" produced byte-for-byte room "foo"'s awareness key, sharing one
// stream between that room's SYNC traffic and "foo"'s PRESENCE traffic. With
// the discriminator first, a collision needs "s:"+x == "a:"+y, which differ in
// their first byte.
const (
	kindDiscrimSync      = "s:"
	kindDiscrimAwareness = "a:"
)

// syncKey is the room's sync stream key: prefix + "s:" + room. See the
// discriminator constants for why "s:" precedes the room name.
func (s streamCfg) syncKey(room string) string { return s.prefix + kindDiscrimSync + room }

// awKey is the room's awareness stream key: prefix + "a:" + room. A separate
// stream from syncKey (see Config.AwarenessMaxLen) and provably a separate KEY
// for every possible pair of room names, per the discriminator constants.
func (s streamCfg) awKey(room string) string { return s.prefix + kindDiscrimAwareness + room }

// parseStreamKey recovers the room and the stream kind from a stream key,
// exactly or not at all — after the prefix the next two bytes are the
// discriminator and every remaining byte is the room name verbatim.
//
// For the DIAGNOSTIC path only: the reader carries a streamTarget forward
// rather than parsing. A key matching neither discriminator returns ok=false
// and must be IGNORED, never guessed at — guessing would attribute a foreign
// key's entries to a real room.
func (s streamCfg) parseStreamKey(key string) (room string, isAwareness, ok bool) {
	rest, found := strings.CutPrefix(key, s.prefix)
	if !found {
		return "", false, false
	}
	if room, found := strings.CutPrefix(rest, kindDiscrimSync); found {
		return room, false, true
	}
	if room, found := strings.CutPrefix(rest, kindDiscrimAwareness); found {
		return room, true, true
	}
	return "", false, false
}

// liveStreamKeysLocked is the set of stream keys this node still has a room
// for: both streams of every room in streamRooms. Built forward — room names
// through syncKey/awKey — rather than by parsing keys back into rooms, so it
// holds for any room name without depending on the layout being reversible.
//
// Caller must hold streamMu (which guards streamRooms).
func (r *Relay) liveStreamKeysLocked() map[string]struct{} {
	live := make(map[string]struct{}, len(r.streamRooms)*streamsPerRoom)
	for room := range r.streamRooms {
		live[r.scfg.syncKey(room)] = struct{}{}
		live[r.scfg.awKey(room)] = struct{}{}
	}
	return live
}

// Stream entry field names. Single letters on purpose: every byte is multiplied
// by retention x rate x rooms. room is absent because the stream KEY is
// authoritative, and there is no route by which an entry could reach the wrong
// room's key.
const (
	fieldNode = "n" // publisher nodeID, for the self-delivery filter
	fieldSeq  = "s" // per-node, per-stream monotonic sequence, for gap detection
	fieldKind = "k" // cluster.Kind
	fieldData = "d" // payload
)

// seqLimit bounds how many per-stream sequence counters this node keeps. See
// evictStaleSeqsLocked for which entries go, and why losing one is safe.
const seqLimit = 4096

// nextSeq issues this node's next sequence number FOR ONE STREAM.
//
// It cannot be retrofitted, and without it gap detection is impossible: XREAD
// from a trimmed ID returns the next surviving entry with NO error, and stream
// IDs are ms-seq rather than contiguous, so trimming is indistinguishable from
// ordinary advancement by ID arithmetic alone.
//
// Per node PER STREAM. A single per-node counter is not merely coarser, it is
// wrong: a reader of one stream sees only the publisher's entries that landed
// in THAT stream, so a node publishing to rooms A and B writes [1 3 5] into A's
// stream and [2 4 6] into B's, and both readers see holes — and noteSeq
// classifies seq > prev+1 as a gap, so per-node would fire StreamStats.Gaps
// ("ALERT ON PRESENCE") constantly on every healthy multi-room node. Counters
// restart at 0 with the process; a reader reads a DECREASE as a restart.
//
// streamMu rather than an atomic per counter: the map lookup is serialised
// anyway, so incrementing under it costs one arithmetic op and saves a
// per-stream allocation. streamMu is never held across I/O.
func (r *Relay) nextSeq(streamKey string) uint64 {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()

	if _, known := r.seqs[streamKey]; !known && len(r.seqs) >= seqLimit {
		r.evictStaleSeqsLocked()
	}
	r.seqs[streamKey]++
	return r.seqs[streamKey]
}

// evictStaleSeqsLocked drops the counters of streams whose room this node no
// longer holds: the map grows with every stream ever published to, and room
// churn over a long-lived process is unbounded even though the live set is not.
// Same policy as evictStaleCursorsLocked, including leaving the map above
// seqLimit when every counter is live.
//
// Safe in the direction that matters: a room this node released is one it
// stopped publishing to — the Relay contract has the server activate every room
// it hosts — so a reactivation restarts the counter at 1, and a reader reads a
// DECREASE as a restart, never a gap. At worst it adds one Restarts increment.
//
// Caller must hold streamMu.
func (r *Relay) evictStaleSeqsLocked() {
	live := r.liveStreamKeysLocked()
	for key := range r.seqs {
		if _, ok := live[key]; !ok {
			delete(r.seqs, key)
		}
	}
}

// streamFields builds the XADD field list for one entry.
func streamFields(nodeID []byte, seq uint64, kind cluster.Kind, data []byte) []any {
	return []any{
		fieldNode, nodeID,
		fieldSeq, strconv.FormatUint(seq, 10),
		fieldKind, strconv.Itoa(int(kind)),
		fieldData, data,
	}
}

// decodeStreamEntry reads one XREAD entry's fields. Every field is required: a
// malformed entry is rejected rather than defaulted, because guessing at a
// payload can cost a room its legitimate updates — a non-V1 blob makes the
// lane's MergeUpdatesV1 fail.
func decodeStreamEntry(vals map[string]any) (nodeID []byte, seq uint64, kind cluster.Kind, data []byte, err error) {
	str := func(k string) (string, error) {
		v, ok := vals[k]
		if !ok {
			return "", fmt.Errorf("missing field %q", k)
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("field %q is %T, want string", k, v)
		}
		return s, nil
	}

	n, err := str(fieldNode)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	sRaw, err := str(fieldSeq)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	s, err := strconv.ParseUint(sRaw, 10, 64)
	if err != nil {
		return nil, 0, 0, nil, fmt.Errorf("parse seq %q: %w", sRaw, err)
	}
	kRaw, err := str(fieldKind)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	k, err := strconv.Atoi(kRaw)
	if err != nil {
		return nil, 0, 0, nil, fmt.Errorf("parse kind %q: %w", kRaw, err)
	}
	d, err := str(fieldData)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	return []byte(n), s, cluster.Kind(k), []byte(d), nil
}

// publishStream appends one payload to its room's stream.
//
// Trimming is inline MAXLEN ~ rather than a separate XTRIM: free on a write
// already happening, and the memory half of the guarantee. The time half is the
// MINID sweeper, because MAXLEN alone gives no age bound — a hot room's 4096
// entries might be two seconds. The approximate form (~) avoids O(n) per XADD
// on a hot stream, and keeps MORE entries than asked, never fewer, so it can
// overshoot on memory but can never shrink the delivery window.
//
// ctx is the caller's: Server.Shutdown cancels the relay context and then joins
// the lane workers, so a publish that ignored cancellation would stall that
// join and leave a worker running past Shutdown (#202).
func (r *Relay) publishStream(ctx context.Context, out cluster.Outbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	key, maxLen := r.scfg.syncKey(out.Room), r.scfg.maxLen
	if out.Kind == cluster.KindAwareness {
		key, maxLen = r.scfg.awKey(out.Room), r.scfg.awMaxLen
	}

	return r.client.XAdd(ctx, &goredis.XAddArgs{
		Stream: key,
		MaxLen: maxLen,
		Approx: true,
		// nextSeq is passed the key this entry is about to be written to: the
		// counter is per stream. See nextSeq.
		Values: streamFields(r.nodeID, r.nextSeq(key), out.Kind, out.Data),
	}).Err()
}
