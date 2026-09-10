package redis

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
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
	require.Equal(t, 250*time.Millisecond, got.readBlock)
	require.Equal(t, maxReadBlock, got.readBlock,
		"the default must be a value the reader actually uses: it is the ceiling too")
}

// ReadBlock above the ceiling is REJECTED, not capped. A knob silently
// ignored above some threshold is worse than one with a documented range —
// and the operator who set 5s wanted something (fewer commands) that this
// package cannot deliver without extending shutdown and activation latency
// by the same 5s. See maxReadBlock.
func TestUnit_Streams_RejectsReadBlockAboveMax(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	_, err := resolveStreamCfg(c, Config{Transport: Streams, ReadBlock: 5 * time.Second})
	require.ErrorContains(t, err, "ReadBlock")
	require.ErrorContains(t, err, "250ms", "the error must name the ceiling")

	// At the ceiling exactly, and below it, are both fine.
	for _, ok := range []time.Duration{maxReadBlock, 10 * time.Millisecond} {
		got, err := resolveStreamCfg(c, Config{Transport: Streams, ReadBlock: ok})
		require.NoError(t, err)
		require.Equal(t, ok, got.readBlock, "an accepted value must be used verbatim, not capped")
	}

	// PubSub mode validates nothing: an existing caller must not start
	// failing construction because a Streams-only field exists.
	got, err := resolveStreamCfg(c, Config{ReadBlock: time.Hour})
	require.NoError(t, err)
	require.Equal(t, time.Hour, got.readBlock)
}

func TestUnit_Streams_RejectsReadBlockBelowMin(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	// A sub-millisecond value truncates to BLOCK 0 in go-redis, which Redis
	// reads as block forever. The reader would hang and Close would deadlock.
	_, err := resolveStreamCfg(c, Config{Transport: Streams, ReadBlock: 500 * time.Microsecond})
	require.ErrorContains(t, err, "ReadBlock")
	require.ErrorContains(t, err, "1ms", "the error must name the floor")

	// At the floor exactly, and above it, are both fine.
	for _, ok := range []time.Duration{minReadBlock, 100 * time.Millisecond} {
		got, err := resolveStreamCfg(c, Config{Transport: Streams, ReadBlock: ok})
		require.NoError(t, err)
		require.Equal(t, ok, got.readBlock, "an accepted value must be used verbatim, not adjusted")
	}

	// PubSub mode validates nothing: an existing caller must not start
	// failing construction because a Streams-only field exists.
	got, err := resolveStreamCfg(c, Config{ReadBlock: 100 * time.Microsecond})
	require.NoError(t, err)
	require.Equal(t, 100*time.Microsecond, got.readBlock)
}

// A NEGATIVE ReadBlock must reach the same rejection as a sub-millisecond
// one. Defaulting everything <= 0 turned -5ms into the 250ms default, so the
// documented "New fails if it is smaller than 1ms" did not hold for the values
// most likely to be a typo.
func TestUnit_Streams_RejectsNegativeReadBlock(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)

	_, err := resolveStreamCfg(c, Config{Transport: Streams, ReadBlock: -5 * time.Millisecond})
	require.ErrorContains(t, err, "ReadBlock")
	require.ErrorContains(t, err, "1ms", "the error must name the floor")

	// Only the UNSET value may be defaulted.
	got, err := resolveStreamCfg(c, Config{Transport: Streams})
	require.NoError(t, err)
	require.Equal(t, defaultReadBlock, got.readBlock)

	// PubSub mode validates nothing: an existing caller must not start
	// failing construction because a Streams-only field exists.
	got, err = resolveStreamCfg(c, Config{ReadBlock: -5 * time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, -5*time.Millisecond, got.readBlock)
}

// An undersized pool leaves publishes waiting on a connection: a reader holds
// one for as long as its XREAD blocks. Failing construction is much kinder
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

// seq must be monotonic and gap-free WITHIN EACH STREAM, and safe under
// concurrent Publish, which the Relay contract explicitly permits for
// distinct rooms.
//
// Two streams, interleaved concurrently, is the shape that broke the earlier
// per-relay counter: it issued each stream a non-contiguous subset of one
// series, which is exactly what the reader's gap detection reports as loss.
func TestUnit_Streams_SeqIsMonotonicPerStreamUnderConcurrency(t *testing.T) {
	r := &Relay{seqs: map[string]uint64{}}
	const perStream = 200
	keys := []string{"ygo:stream:s:room1", "ygo:stream:s:room2"}

	got := make(chan [2]any, perStream*len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		for i := 0; i < perStream; i++ {
			wg.Add(1)
			go func(key string) {
				defer wg.Done()
				got <- [2]any{key, r.nextSeq(key)}
			}(key)
		}
	}
	wg.Wait()
	close(got)

	seen := map[string]map[uint64]bool{}
	for pair := range got {
		key, seq := pair[0].(string), pair[1].(uint64)
		if seen[key] == nil {
			seen[key] = map[uint64]bool{}
		}
		require.False(t, seen[key][seq], "seq %d issued twice for %s", seq, key)
		seen[key][seq] = true
	}

	require.Len(t, seen, len(keys))
	for _, key := range keys {
		require.Len(t, seen[key], perStream)
		// Contiguous 1..perStream: a hole here is what a reader would report
		// as a Gap, i.e. as lost data.
		for i := uint64(1); i <= perStream; i++ {
			require.True(t, seen[key][i], "%s is missing seq %d of %d", key, i, perStream)
		}
	}
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

// streamEntries reads every entry currently in a stream key.
func streamEntries(t *testing.T, mr *miniredis.Miniredis, key string) []miniredis.StreamEntry {
	t.Helper()
	e, err := mr.Stream(key)
	if err != nil {
		return nil // key absent: no entries
	}
	return e
}

// entryValues flattens one miniredis entry's fields into a map.
//
// miniredis.StreamEntry.Values is a []string of alternating key/value — NOT a
// map — so it cannot be indexed by field name directly.
func entryValues(e miniredis.StreamEntry) map[string]string {
	m := make(map[string]string, len(e.Values)/2)
	for i := 0; i+1 < len(e.Values); i += 2 {
		m[e.Values[i]] = e.Values[i+1]
	}
	return m
}

func TestUnit_Streams_PublishSyncLandsInSyncStream(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("update"),
	}))

	entries := streamEntries(t, mr, "ygo:stream:s:room1")
	require.Len(t, entries, 1)
	require.Equal(t, "update", entryValues(entries[0])["d"])
	require.Equal(t, "1", entryValues(entries[0])["s"], "first publish must be seq 1")
	require.Empty(t, streamEntries(t, mr, "ygo:stream:a:room1"))
}

// Awareness must go to its own stream: sharing would let heartbeat traffic
// evict sync entries out of the retention window.
func TestUnit_Streams_PublishAwarenessLandsInAwarenessStream(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindAwareness, Data: []byte("presence"),
	}))

	require.Len(t, streamEntries(t, mr, "ygo:stream:a:room1"), 1)
	require.Empty(t, streamEntries(t, mr, "ygo:stream:s:room1"))
}

// Server.Shutdown cancels the relay ctx and then joins lane workers; a
// Publish that ignores cancellation stalls that join (#202).
func TestUnit_Streams_PublishHonoursCancelledContext(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = r.publishStream(ctx, cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("update"),
	})
	require.ErrorIs(t, err, context.Canceled)
}

// Both must reach BOTH tiers, since pub/sub and Streams nodes do not
// interoperate and migration rolls through Both.
//
// r.Start is required here even though the brief's version of this test
// omitted it: Publish's existing started-guard returns ErrRelayNotStarted
// before reaching the transport-routing branch this task adds, so without
// Start the require.NoError below would fail regardless of whether the new
// routing code is correct.
func TestUnit_Streams_BothPublishesToChannelAndStream(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Both})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	require.NoError(t, r.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("update"),
	}))

	// Stream side is synchronous and observable immediately.
	require.Eventually(t, func() bool {
		return len(streamEntries(t, mr, "ygo:stream:s:room1")) == 1
	}, 2*time.Second, 10*time.Millisecond, "Both must XADD to the stream")
}

// PubSub mode must never touch the keyspace.
//
// r.Start is required here for the same reason as the Both test above: the
// brief's version discarded Publish's error and never called Start, so
// Publish returned ErrRelayNotStarted before ever reaching the new
// transport-routing branch — meaning the stream-emptiness assertion below
// would have passed even if PubSub mode wrongly wrote to a stream. Calling
// Start first makes Publish actually exercise usesStreams()/usesPubSub().
func TestUnit_Streams_PubSubModeWritesNoStream(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	require.NoError(t, r.Publish(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("update"),
	}))
	require.Empty(t, streamEntries(t, mr, "ygo:stream:s:room1"))
}

// --- seq is per (node, stream), not per node -------------------------------

// A node publishing to several rooms must write a CONTIGUOUS series into each
// room's stream, because a reader of one stream sees only that stream's
// entries and reports a jump as lost data.
//
// This is the end-to-end shape of the defect the per-stream counter fixes: a
// single per-relay counter, incremented for every publish regardless of room,
// wrote [1 3 5] into room1's stream and [2 4 6] into room2's, so every reader
// of either room raised StreamStats.Gaps against a node that was doing
// nothing but working. Verified failing against that counter — see
// task-6-report.md.
func TestUnit_Streams_InterleavedRoomPublishesAreContiguousPerStream(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	const rounds = 3
	for i := 0; i < rounds; i++ {
		for _, room := range []string{"room1", "room2"} {
			require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
				Room: room, Kind: cluster.KindSync, Data: []byte(fmt.Sprintf("%s-%d", room, i)),
			}))
		}
	}

	for _, room := range []string{"room1", "room2"} {
		entries := streamEntries(t, mr, r.scfg.syncKey(room))
		require.Len(t, entries, rounds)
		for i, e := range entries {
			require.Equal(t, strconv.Itoa(i+1), entryValues(e)["s"],
				"%s entry %d: a stream's own series must be contiguous from 1", room, i)
		}
	}
}

// The two kinds of stream for one room have independent counters too: they
// are separate keys, and a reader reads them as separate series.
func TestUnit_Streams_SyncAndAwarenessCountSeparately(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	for i := 0; i < 2; i++ {
		for _, kind := range []cluster.Kind{cluster.KindSync, cluster.KindAwareness} {
			require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
				Room: "room1", Kind: kind, Data: []byte("x"),
			}))
		}
	}

	for _, key := range []string{r.scfg.syncKey("room1"), r.scfg.awKey("room1")} {
		entries := streamEntries(t, mr, key)
		require.Len(t, entries, 2)
		require.Equal(t, "1", entryValues(entries[0])["s"], key)
		require.Equal(t, "2", entryValues(entries[1])["s"], key)
	}
}

// The counter map is bounded, and the bound must not touch a stream whose
// room this node still holds: resetting a live stream's counter would make
// every reader of it record a Restart for nothing.
func TestUnit_Streams_SeqEvictionKeepsLiveStreams(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	r.streamMu.Lock()
	r.streamRooms["live"] = 1
	r.streamMu.Unlock()

	liveKey := r.scfg.syncKey("live")
	require.Equal(t, uint64(1), r.nextSeq(liveKey))
	for i := 0; i < seqLimit; i++ {
		r.nextSeq(fmt.Sprintf("ygo:stream:s:gone-%d", i))
	}
	r.nextSeq("ygo:stream:s:gone-trigger")

	require.Equal(t, uint64(2), r.nextSeq(liveKey),
		"a live stream's counter must survive eviction, or its readers see a phantom restart")

	r.streamMu.Lock()
	held := len(r.seqs)
	r.streamMu.Unlock()
	require.Less(t, held, seqLimit, "eviction must actually bound the map")
}

// --- Key namespaces ---------------------------------------------------------

// No two (room, kind) pairs may share a Redis key.
//
// Room names are permissive by design — internal/roomname.Valid accepts every
// printable character, ":" included, to match the y-websocket JS server — so
// the key layout has to be collision-proof for adversarial names, not merely
// for tidy ones. The "aw:foo" row is the one that failed under the earlier
// layout: prefix+"aw:foo" was simultaneously that room's sync key and room
// "foo"'s awareness key, so one Redis stream carried two rooms' traffic.
func TestUnit_Streams_KeyNamespacesCannotCollide(t *testing.T) {
	sc := streamCfg{prefix: defaultStreamPrefix}
	// "" is included even though roomname.Valid rejects it: the property is
	// about the key layout, and asserting it for names the validator would
	// never pass costs nothing and removes one dependency between the two.
	rooms := []string{"foo", "a:foo", "s:foo", "aw:foo", "a:", "s:", "", ":", "a:s:foo"}

	owner := map[string]string{}
	for _, room := range rooms {
		for kind, key := range map[string]string{
			"sync":      sc.syncKey(room),
			"awareness": sc.awKey(room),
		} {
			who := fmt.Sprintf("%s(%q)", kind, room)
			if prev, dup := owner[key]; dup {
				t.Fatalf("key %q is shared by %s and %s", key, prev, who)
			}
			owner[key] = who
		}
	}
	require.Len(t, owner, len(rooms)*streamsPerRoom)
}

// The end-to-end form of the same property: two rooms whose names differ only
// by a discriminator-shaped prefix must not see each other's traffic.
func TestUnit_Streams_ColonRoomsDoNotShareAStream(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Sync traffic for the rooms whose names could forge a discriminator...
	for _, room := range []string{"a:foo", "aw:foo"} {
		require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
			Room: room, Kind: cluster.KindSync, Data: []byte("sync-" + room),
		}))
	}
	// ...and presence for the room they might be mistaken for.
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "foo", Kind: cluster.KindAwareness, Data: []byte("presence-foo"),
	}))

	want := map[string]string{
		r.scfg.syncKey("a:foo"):  "sync-a:foo",
		r.scfg.syncKey("aw:foo"): "sync-aw:foo",
		r.scfg.awKey("foo"):      "presence-foo",
	}
	require.Len(t, want, 3, "the three keys must be distinct")

	for key, payload := range want {
		entries := streamEntries(t, mr, key)
		require.Len(t, entries, 1, "key %q must hold exactly its own entry", key)
		require.Equal(t, payload, entryValues(entries[0])["d"], "key %q", key)
	}
	// And nothing leaked into the room-"foo" sync stream, which was never
	// published to.
	require.Empty(t, streamEntries(t, mr, r.scfg.syncKey("foo")))
}

// parseStreamKey must be exact, and must refuse rather than guess.
func TestUnit_Streams_ParseStreamKeyIsExactOrRefuses(t *testing.T) {
	sc := streamCfg{prefix: defaultStreamPrefix}

	for _, room := range []string{"foo", "a:foo", "s:foo", "aw:foo", "", ":"} {
		got, isAw, ok := sc.parseStreamKey(sc.syncKey(room))
		require.True(t, ok)
		require.False(t, isAw)
		require.Equal(t, room, got, "sync key of %q must round-trip verbatim", room)

		got, isAw, ok = sc.parseStreamKey(sc.awKey(room))
		require.True(t, ok)
		require.True(t, isAw)
		require.Equal(t, room, got, "awareness key of %q must round-trip verbatim", room)
	}

	// Neither discriminator: ignored, not coerced into a room name.
	for _, key := range []string{
		"ygo:stream:",       // prefix only
		"ygo:stream:aw:foo", // the OLD awareness layout
		"ygo:stream:foo",    // the OLD sync layout
		"ygo:cluster:s:foo", // another namespace entirely
		"s:foo",             // unprefixed
	} {
		room, isAw, ok := sc.parseStreamKey(key)
		require.False(t, ok, "key %q must be refused", key)
		require.Empty(t, room)
		require.False(t, isAw)
	}
}
