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

// usesPubSub reports whether this transport reads from and writes to
// channels. Publish consults this to decide whether the pub/sub hand-off
// runs at all: Streams-only mode must skip PUBLISH entirely, not merely
// ignore its result, or a Streams deployment would still pay for and depend
// on the at-most-once channel it exists to replace.
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
// ~5000-argument command every cycle. keyBatches (streams_reader.go) enforces
// this; the reader task that issues XREAD lands later and consumes both.
const maxKeysPerRead = 512

// maxReadBlock is the largest Config.ReadBlock this package accepts, and also
// its default. resolveStreamCfg REJECTS a larger value rather than capping it
// silently, so the knob can never be set to a number that does not happen.
//
// The ceiling exists because a blocked XREAD cannot be interrupted. go-redis
// arms the socket read deadline from ctx.Deadline() only
// (internal/pool.(*Conn).deadline, v9.18.0; withConn has no cancellation
// watcher), so cancelling a reader's context mid-read does nothing. The
// interval between one reader's reads is therefore BOTH of these latencies at
// once, and each has a hard requirement:
//
//   - Shutdown. Close closes r.done and joins r.wg, so a reader parked in an
//     XREAD holds Close open for the rest of that block. At a 5s ReadBlock
//     this was measured at 4.80s — a five-second stall on every
//     Server.Shutdown, in a path already fixed twice (#202, #229).
//   - Activation. A reader's key set is fixed when its XREAD is issued, so a
//     room activated after that cannot be read until the block expires. A
//     room joining and then seeing no remote edits for seconds is the
//     pub/sub tier's instant delivery visibly regressed.
//
// Neither is satisfiable at a multi-second block without waking a reader out
// of its read, and the only mechanism for that is closing its connection —
// connection churn proportional to room churn, which at this tier's 10k-room
// target is a worse trade than a few extra idle XREADs per second. So the
// block stays short and ReadBlock's range is honest about it: 250ms x 4
// readers is 16 XREADs a second on an idle node, and an XREAD that finds
// nothing is cheap.
//
// Lowering ReadBlock below this is a real and supported choice (faster
// shutdown and activation, more commands); raising it is not offered, because
// it could not be delivered.
const maxReadBlock = 250 * time.Millisecond

// stalledBackoffBase is the first wait after a room's cursor advance is
// declined for lane backpressure. It doubles per consecutive stall, capped at
// ReadBlock. Not configurable, for the same reason as maxKeysPerRead. The
// reader task that applies backpressure backoff lands later and consumes it.
//
//nolint:unused // consumed by a later task, see the sentence above
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
	if sc.readBlock <= 0 {
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
	// threshold is worse than one with a documented range. See maxReadBlock
	// for why the ceiling is where it is.
	if sc.readBlock > maxReadBlock {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: ReadBlock (%s) must not exceed %s: the interval between a reader's XREADs is also how long Close and a newly activated room wait, and a blocked XREAD cannot be interrupted",
			sc.readBlock, maxReadBlock)
	}
	if sc.trimInterval >= sc.retention {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: TrimInterval (%s) must be less than StreamRetention (%s): a sweeper slower than the window it enforces cannot enforce it",
			sc.trimInterval, sc.retention)
	}
	return sc, nil
}

// Stream-kind discriminators. The room name is APPENDED to one of these, so
// the discriminator sits immediately after the prefix, ahead of every
// caller-supplied byte.
//
// That placement is the whole point of the layout. internal/roomname.Valid
// deliberately accepts every printable character, ":" included, to match the
// y-websocket JS server, so a room name may contain anything a key may. Under
// the earlier layout — syncKey = prefix+room, awKey = prefix+"aw:"+room — a
// room named "aw:foo" produced byte-for-byte room "foo"'s awareness key, so
// that room's SYNC traffic and room "foo"'s PRESENCE traffic shared one Redis
// stream, each reader interpreting the other room's entries under its own
// kind.
//
// With the discriminator first, a collision would require "s:"+x == "a:"+y
// for some room names x and y. Those two strings differ in their first byte,
// so no pair of room names can satisfy it: the room name can no longer forge
// a discriminator, because it is never in a position to be read as one.
const (
	kindDiscrimSync      = "s:"
	kindDiscrimAwareness = "a:"
)

// syncKey is the room's sync stream key: prefix + "s:" + room. publishStream
// XADDs to it and readBatch XREADs it.
//
// See the discriminator constants above for why "s:" precedes the room name
// instead of the two key shapes differing by a suffix on one of them.
func (s streamCfg) syncKey(room string) string { return s.prefix + kindDiscrimSync + room }

// awKey is the room's awareness stream key: prefix + "a:" + room. A separate
// stream from syncKey — see Config.AwarenessMaxLen — and provably a separate
// KEY for every possible pair of room names, per the discriminator constants.
func (s streamCfg) awKey(room string) string { return s.prefix + kindDiscrimAwareness + room }

// parseStreamKey recovers the room and the stream kind from a stream key,
// exactly or not at all.
//
// Exact because of the layout above: after the prefix the next two bytes are
// the discriminator, and every remaining byte is the room name verbatim.
// There is no second reading to weigh, because one key cannot be both a sync
// key and an awareness key, and the room name never occupies the
// discriminator's position. Under the earlier suffix layout this operation was
// genuinely ambiguous, which is why the reader carries a streamTarget forward
// rather than parsing (see streamTarget); this function serves the diagnostic
// path, where a key the reader did not ask for turns up and the useful thing
// to log is what that key claims to be.
//
// A key matching neither discriminator returns ok=false and must be IGNORED,
// never guessed at. Guessing is what would attribute a foreign key's entries
// to a real room, and this file already records what filing a payload under
// the wrong interpretation costs (see decodeStreamEntry).
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
// for: both streams of every room in streamRooms.
//
// Built in the forward direction — room names through syncKey/awKey — rather
// than by parsing keys back into rooms, so it holds for any room name without
// depending on the key layout being reversible at all.
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

// Stream entry field names. Single letters on purpose: every byte is
// multiplied by retention x rate x rooms.
//
// room is absent because the stream KEY is authoritative — XRANGE shows it,
// and there is no route by which an entry could reach the wrong room's key.
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
// The counter must exist from the first release: it cannot be retrofitted,
// and without it gap detection is impossible. XREAD from a trimmed ID returns
// the next surviving entry with NO error, and stream IDs are ms-seq rather
// than contiguous, so trimming is indistinguishable from ordinary
// advancement by ID arithmetic alone.
//
// It is per node PER STREAM, keyed by the stream key this entry is about to be
// written to. A single per-node counter — the earlier design — is not merely
// coarser, it is wrong, and it breaks the counter's only consumer. A reader
// watching one stream sees only the subset of a publisher's entries that
// landed in THAT stream, so a node publishing to rooms A and B writes seqs
// [1 3 5] into A's stream and [2 4 6] into B's, and both readers see a
// sequence full of holes. noteSeq classifies seq > prev+1 as a gap, and
// StreamStats.Gaps is documented "ALERT ON PRESENCE… a single gap means data
// was lost" — so a per-node counter makes the tier's headline signal fire
// constantly on every healthy multi-room node, which is the same as having no
// signal at all. Per stream, one node's entries in one stream are contiguous,
// and a jump can only mean entries were trimmed before the reader reached
// them.
//
// Counters live in memory, so they restart at 0 when the process does, and a
// reader treats a DECREASE as a restart rather than a gap — see noteSeq.
//
// streamMu rather than an atomic per counter: the map lookup has to be
// serialised anyway, and once it is, incrementing a plain uint64 under that
// same lock costs one arithmetic op and saves a per-stream heap allocation.
// The lock is cheap here in absolute terms and cheaper still in context —
// streamMu is never held across I/O anywhere (that is why it exists separately
// from mu), and the caller is about to make a network round trip.
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
// longer holds, in one pass, and keeps every counter whose room is still live.
//
// Bounding is needed because the map grows with the streams this node has
// ever published to, and room churn over a long-lived process is unbounded
// even though the live set is not. It mirrors evictStaleCursorsLocked's policy
// exactly, for the same reason: if every counter is live the map is left above
// seqLimit, which is correct, because the residual is then bounded by real
// load (streamsPerRoom x resident rooms) rather than by history.
//
// Dropping a stale counter is safe in the direction that matters. A room this
// node has released is a room it has stopped publishing to — the Relay
// contract has the server activate every room it hosts — so if it is ever
// reactivated here the counter restarts at 1, and a reader classifies a
// DECREASE as a restart, never as a gap. The eviction therefore cannot
// manufacture the alarm this counter exists to raise — at worst it adds one
// Restarts increment, a counter documented as informational. A caller that
// published without ever activating (nothing does in production; some tests
// do) would trade the same way: extra Restarts, never a Gap.
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

// decodeStreamEntry reads one XREAD entry's fields.
//
// Every field is required. A malformed entry is rejected rather than
// defaulted: the pub/sub router already learned (see its unrecognised-kind
// handling) that guessing at a payload can cost a room its legitimate
// updates, because a non-V1 blob makes the lane's MergeUpdatesV1 fail.
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
// Trimming is inline MAXLEN ~ rather than a separate XTRIM call: it costs
// nothing extra on a write that is already happening, and it is the memory
// half of the guarantee. The time half is the MINID sweeper, because MAXLEN
// alone gives no age bound — a hot room's 4096 entries might be two seconds.
//
// The approximate form (~) is deliberate. Exact trimming is O(n) per XADD on
// a hot stream, and Redis recommends ~ for exactly this reason. Approximate
// trimming keeps MORE entries than asked, never fewer, so it can overshoot on
// memory but can never shrink the delivery window.
//
// ctx is the caller's: Server.Shutdown cancels the relay context and then
// joins the lane workers, so a publish that ignored cancellation would stall
// that join and leave a worker running past Shutdown (#202).
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
		// nextSeq is passed the key this entry is about to be written to:
		// the counter is per stream, because a reader of one stream sees only
		// that stream's entries. See nextSeq.
		Values: streamFields(r.nodeID, r.nextSeq(key), out.Kind, out.Data),
	}).Err()
}
