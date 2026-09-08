// Package-internal: the Redis Streams delivery tier. See Config.Transport.
package redis

import (
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
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
// This task adds Transport's full API; the reader/writer tasks that dispatch
// on it land later.
//
//nolint:unused // consumed by a later task, see the sentence above
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

// syncKey is the room's sync stream key. The reader/writer tasks that
// XADD/XREAD by key land later and consume it.
//
//nolint:unused // consumed by a later task, see the sentence above
func (s streamCfg) syncKey(room string) string { return s.prefix + room }

// awKey is the room's awareness stream key. Separate from syncKey — see
// Config.AwarenessMaxLen. The reader/writer tasks that XADD/XREAD by key
// land later and consume it.
//
//nolint:unused // consumed by a later task, see the sentence above
func (s streamCfg) awKey(room string) string { return s.prefix + "aw:" + room }
