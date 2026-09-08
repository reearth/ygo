package redis

import (
	"fmt"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
)

// Transport's zero value must be PubSub so an existing Config keeps working.
func TestUnit_Streams_TransportZeroValueIsPubSub(t *testing.T) {
	require.Equal(t, PubSub, Transport(0))
}

func TestUnit_Streams_ResolveDefaults(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	got, err := resolveStreamCfg(c, Config{Transport: Streams})
	require.NoError(t, err)

	require.Equal(t, "ygo:stream:", got.prefix)
	require.Equal(t, 60*time.Second, got.retention)
	require.Equal(t, int64(4096), got.maxLen)
	require.Equal(t, int64(64), got.awMaxLen)
	require.Equal(t, 10*time.Second, got.awRetention)
	require.Equal(t, 4, got.readers)
	require.Equal(t, 30*time.Second, got.trimInterval)
	require.Equal(t, 5*time.Second, got.readBlock)
}

// An undersized pool starves publishes: every blocking XREAD holds a pool
// connection for its whole ReadBlock. Failing construction is much kinder
// than presenting as mysterious publish latency later.
func TestUnit_Streams_RejectsPoolSmallerThanReaders(t *testing.T) {
	mr := newMiniRedis(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), PoolSize: 2})
	t.Cleanup(func() { _ = c.Close() })

	_, err := resolveStreamCfg(c, Config{Transport: Streams, Readers: 8})
	require.ErrorContains(t, err, "PoolSize")
}

// A sweeper slower than the window it enforces cannot enforce it.
func TestUnit_Streams_RejectsTrimIntervalAboveRetention(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	_, err := resolveStreamCfg(c, Config{
		Transport:       Streams,
		StreamRetention: 10 * time.Second,
		TrimInterval:    30 * time.Second,
	})
	require.ErrorContains(t, err, "TrimInterval")
}

func TestUnit_Streams_RejectsUnknownTransport(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	_, err := resolveStreamCfg(c, Config{Transport: Transport(99)})
	require.ErrorContains(t, err, "Transport")
}

// PubSub mode must not validate stream settings at all — an existing caller
// has never set them and must not start failing construction.
func TestUnit_Streams_PubSubModeSkipsStreamValidation(t *testing.T) {
	mr := newMiniRedis(t)
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), PoolSize: 1})
	t.Cleanup(func() { _ = c.Close() })

	_, err := resolveStreamCfg(c, Config{Readers: 999})
	require.NoError(t, err)
}

// Round-tripping through native stream fields is what lets the reader read
// seq without decoding the payload.
func TestUnit_Streams_EntryRoundTrip(t *testing.T) {
	nodeID := []byte("0123456789abcdef")
	fields := streamFields(nodeID, 42, cluster.KindSync, []byte("payload"))

	vals := asRedisValues(fields)

	gotNode, gotSeq, gotKind, gotData, err := decodeStreamEntry(vals)
	require.NoError(t, err)
	require.Equal(t, nodeID, gotNode)
	require.Equal(t, uint64(42), gotSeq)
	require.Equal(t, cluster.KindSync, gotKind)
	require.Equal(t, []byte("payload"), gotData)
}

// room is deliberately NOT a field: the key is authoritative and every
// omitted byte is multiplied by retention x rate.
func TestUnit_Streams_EntryOmitsRoom(t *testing.T) {
	fields := streamFields([]byte("n"), 1, cluster.KindSync, []byte("d"))
	for i := 0; i < len(fields); i += 2 {
		require.NotEqual(t, "room", fields[i])
	}
	require.Len(t, fields, 8) // exactly n, s, k, d
}

// asRedisValues rebuilds what XREAD hands back from what XADD was given.
//
// The conversion is the point: XADD takes []byte happily, but Redis stores
// bulk strings and go-redis surfaces XMessage.Values as map[string]any holding
// STRINGS. A test that fed []byte straight back in would pass against a
// decoder that accepts []byte and then fail against real Redis.
func asRedisValues(fields []any) map[string]any {
	out := make(map[string]any, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		k, _ := fields[i].(string)
		switch v := fields[i+1].(type) {
		case []byte:
			out[k] = string(v)
		case string:
			out[k] = v
		default:
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

func TestUnit_Streams_DecodeRejectsMissingFields(t *testing.T) {
	_, _, _, _, err := decodeStreamEntry(map[string]any{"n": "x"})
	require.Error(t, err)
}

func TestUnit_Streams_DecodeRejectsGarbageSeq(t *testing.T) {
	_, _, _, _, err := decodeStreamEntry(map[string]any{
		"n": "x", "s": "not-a-number", "k": "0", "d": "d",
	})
	require.ErrorContains(t, err, "seq")
}

// seq must be monotonic per relay and safe under concurrent Publish, which
// the Relay contract explicitly permits for distinct rooms.
func TestUnit_Streams_SeqIsMonotonicUnderConcurrency(t *testing.T) {
	r := &Relay{}
	const n = 200
	got := make(chan uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); got <- r.nextSeq() }()
	}
	wg.Wait()
	close(got)

	seen := map[uint64]bool{}
	for s := range got {
		require.False(t, seen[s], "seq %d issued twice", s)
		seen[s] = true
	}
	require.Len(t, seen, n)
}

// A separate type, not extra fields on Stats: a counter that is permanently
// zero for the tier you are running is what makes a dashboard untrustworthy.
func TestUnit_StreamStats_SnapshotsEveryCounter(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.Equal(t, StreamStats{}, r.StreamStats(), "a fresh relay has zero of everything")

	r.replayed.Add(3)
	r.gaps.Add(1)
	r.restarts.Add(2)
	r.trimmed.Add(10)
	r.stalled.Add(4)

	require.Equal(t, StreamStats{
		Replayed: 3, Gaps: 1, Restarts: 2, Trimmed: 10, Stalled: 4,
	}, r.StreamStats())
}

// Stats() is the pub/sub tier's and must not grow stream fields.
// Verify that a pub/sub-mode relay reports zero stream activity, and that
// the pub/sub tier's Stats() method still works correctly.
func TestUnit_StreamStats_PubSubStatsUnchanged(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// A pub/sub relay's StreamStats must be all zeros
	require.Equal(t, StreamStats{}, r.StreamStats(),
		"a pub/sub relay reports zero stream activity")

	// The pub/sub tier's Stats() must also report zero values (no events have occurred)
	require.Equal(t, Stats{}, r.Stats(),
		"a fresh pub/sub relay has zero degraded-path activity")
}
