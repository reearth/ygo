// Package-internal: the Redis Streams delivery tier. See Config.Transport.
package redis

import (
	"context"
	"fmt"
	"strconv"
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
	defaultReadBlock          = 5 * time.Second
)

// maxKeysPerRead bounds how many stream keys go into one XREAD. Not
// configurable: an operator has no basis for choosing it, and Readers already
// controls concurrency. Without it, 10k rooms across 4 readers would build a
// ~5000-argument command every cycle. The reader task that issues XREAD
// lands later and consumes it.
//
//nolint:unused // consumed by a later task, see the sentence above
const maxKeysPerRead = 512

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
			"cluster/redis: PoolSize (%d) must exceed Readers (%d): every blocking XREAD holds a pool connection for its whole ReadBlock, so an equal or smaller pool starves publishes",
			pool, sc.readers)
	}
	if sc.trimInterval >= sc.retention {
		return streamCfg{}, fmt.Errorf(
			"cluster/redis: TrimInterval (%s) must be less than StreamRetention (%s): a sweeper slower than the window it enforces cannot enforce it",
			sc.trimInterval, sc.retention)
	}
	return sc, nil
}

// syncKey is the room's sync stream key. publishStream XADDs to it; the
// reader task that XREADs it lands later.
func (s streamCfg) syncKey(room string) string { return s.prefix + room }

// awKey is the room's awareness stream key. Separate from syncKey — see
// Config.AwarenessMaxLen. publishStream XADDs to it; the reader task that
// XREADs it lands later.
func (s streamCfg) awKey(room string) string { return s.prefix + "aw:" + room }

// Stream entry field names. Single letters on purpose: every byte is
// multiplied by retention x rate x rooms.
//
// room is absent because the stream KEY is authoritative — XRANGE shows it,
// and there is no route by which an entry could reach the wrong room's key.
const (
	fieldNode = "n" // publisher nodeID, for the self-delivery filter
	fieldSeq  = "s" // per-node monotonic sequence, for gap detection
	fieldKind = "k" // cluster.Kind
	fieldData = "d" // payload
)

// nextSeq issues this relay's next sequence number.
//
// The counter is per-node and monotonic, and it must exist from the first
// release: it cannot be retrofitted, and without it gap detection is
// impossible. XREAD from a trimmed ID returns the next surviving entry with
// NO error, and stream IDs are ms-seq rather than contiguous, so trimming is
// indistinguishable from ordinary advancement by ID arithmetic alone.
//
// It lives in memory, so it restarts at 0 when the process does. A reader
// treats a DECREASE as a restart rather than a gap — see the reader's
// gap-detection notes.
func (r *Relay) nextSeq() uint64 { return r.seq.Add(1) }

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
		Values: streamFields(r.nodeID, r.nextSeq(), out.Kind, out.Data),
	}).Err()
}
