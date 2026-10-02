package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
)

// r.Start is REQUIRED in every test below that calls RoomActivated:
// RoomActivated returns immediately when !r.started.Load(), before it ever
// touches streamRooms, so without it trimOnce finds no rooms and these tests
// pass vacuously.
//
// These tests do NOT use miniredis's mr.FastForward to age entries.
// FastForward only decreases TTLs (db.fastForward walks expiring keys); it
// moves neither miniredis's now() behind XADD's "*" auto-ID nor its TIME reply
// — both stay real time.Now() unless a test calls mr.SetTime. Since trimOnce's
// cutoff comes from TIME and the ids come from that same clock, FastForward has
// no effect on whether an entry looks old to either side. Real time.Sleep ages
// it, so retention/AwarenessRetention below are small enough to stay fast.

// MAXLEN alone gives no age bound — a hot room's 4096 entries might be two
// seconds. The MINID sweeper is the time half of the guarantee.
func TestUnit_StreamTrim_RemovesEntriesOlderThanRetention(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:       Streams,
		StreamRetention: 100 * time.Millisecond,
		TrimInterval:    50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1")
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("old"),
	}))
	require.Len(t, streamEntries(t, mr, r.scfg.syncKey("room1")), 1)

	time.Sleep(150 * time.Millisecond) // past StreamRetention, by real wall-clock time
	require.NoError(t, r.trimOnce(context.Background()))

	require.Empty(t, streamEntries(t, mr, r.scfg.syncKey("room1")),
		"an entry older than StreamRetention must be swept")
	require.Positive(t, r.StreamStats().Trimmed)
}

// The guarantee direction: entries INSIDE the window must survive. This is
// asserted as a lower bound deliberately — miniredis ignores the ~/= trim
// modifiers and trims exactly, while real Redis with MAXLEN ~ may retain MORE
// than the cap. Approximate trimming never keeps FEWER entries than asked, so
// a lower-bound assertion holds on both, while a tight upper bound would pass
// here and fail against real Redis. (This test doesn't touch MAXLEN at all —
// it exercises the MINID sweeper only — but the same asymmetry argument
// applies: asserting "at least" is the only direction that is safe against
// approximation on either axis of this tier.)
func TestUnit_StreamTrim_KeepsEntriesInsideTheWindow(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:       Streams,
		StreamRetention: time.Hour,
		TrimInterval:    time.Minute,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1")
	for i := 0; i < 10; i++ {
		require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
			Room: "room1", Kind: cluster.KindSync, Data: []byte("fresh"),
		}))
	}
	require.NoError(t, r.trimOnce(context.Background()))

	require.GreaterOrEqual(t, len(streamEntries(t, mr, r.scfg.syncKey("room1"))), 10,
		"nothing inside the retention window may be swept")
}

// The awareness stream has its own, shorter retention.
func TestUnit_StreamTrim_AwarenessUsesItsOwnRetention(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:          Streams,
		StreamRetention:    time.Hour,
		TrimInterval:       time.Minute,
		AwarenessRetention: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1")
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindAwareness, Data: []byte("presence"),
	}))
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("edit"),
	}))

	time.Sleep(150 * time.Millisecond) // past AwarenessRetention, well inside StreamRetention
	require.NoError(t, r.trimOnce(context.Background()))

	require.Empty(t, streamEntries(t, mr, r.scfg.awKey("room1")),
		"awareness must be swept on AwarenessRetention, not StreamRetention")
	require.Len(t, streamEntries(t, mr, r.scfg.syncKey("room1")), 1,
		"the sync stream's 1-hour retention must not be affected by the awareness sweep")
}

// Close must not hang with the sweeper running. runTrimSweeper's select
// watches r.done directly, the same way runPublisher/runSubscriber do, NOT
// only ctx.Done(). ctx here is context.Background(), which is never cancelled
// (the ordinary case documented on streamReadCtx: ctx usually outlives the
// relay), so a sweeper gated on ctx.Done() alone would never observe Close and
// Close's r.wg.Wait() would block forever. Verified by mutation: dropping the
// "case <-r.done: return" arm reproduces exactly that hang
// (Close does not return within a bounded wait).
func TestUnit_StreamTrim_CloseDoesNotHang(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:    Streams,
		TrimInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(context.Background(), &countingSink{}))
	r.RoomActivated("room1")

	time.Sleep(50 * time.Millisecond) // let the sweeper tick at least once

	start := time.Now()
	require.NoError(t, r.Close())
	require.Less(t, time.Since(start), time.Second,
		"Close must not stall on the trim sweeper: its select must watch r.done, not only ctx.Done()")
}

func TestUnit_StreamTrim_NoRoomsIsANoOp(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	require.NoError(t, r.trimOnce(context.Background()))
	require.Zero(t, r.StreamStats().Trimmed)
}

// A room with a stream reference count of zero left lingering in the map (see
// RoomActivated/RoomDeactivated's use of streamRooms as a refcount, not a
// set) must not be swept: it is not active, and the zero-count branch is
// filtered by "if n > 0" in trimOnce, not by the map simply lacking the key.
// This distinguishes trimOnce's room selection from "every key in
// streamRooms" — a mutation that dropped the n > 0 guard would still pass
// every test above, because RoomDeactivated always DELETES a room whose
// count reaches zero (see redis.go) rather than leaving a zero entry behind.
// This test manufactures the zero-count case directly, bypassing
// RoomDeactivated's delete, to cover the guard on its own.
func TestUnit_StreamTrim_ZeroRefcountRoomIsSkipped(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:       Streams,
		StreamRetention: 100 * time.Millisecond,
		TrimInterval:    50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1")
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("old"),
	}))

	// Force the refcount to zero without going through RoomDeactivated's
	// delete, so the map entry survives with n == 0.
	r.streamMu.Lock()
	r.streamRooms["room1"] = 0
	r.streamMu.Unlock()

	time.Sleep(150 * time.Millisecond)
	require.NoError(t, r.trimOnce(context.Background()))

	require.Len(t, streamEntries(t, mr, r.scfg.syncKey("room1")), 1,
		"a room with a zero refcount must not be swept even though it is still a map key")
	require.Zero(t, r.StreamStats().Trimmed)
}

// The cutoff must be minted by the same clock as the ids it is compared
// against. XADD * takes its milliseconds from the REDIS server, so a cutoff
// read off the application host sweeps entries still inside the window
// whenever that host's clock runs ahead — and because the streams are shared,
// one skewed node shrinks the advertised window for every node.
//
// miniredis's own clock drives both XADD's auto-id and TIME (effectiveNow),
// so SetTime makes the two clocks disagree the way a real deployment's do.
// Verified by mutation: computing the cutoff from time.Now() again sweeps the
// whole stream here, because a local cutoff sits two hours past every id.
func TestUnit_StreamTrim_CutoffComesFromTheServerClock(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:       Streams,
		StreamRetention: time.Minute,
		TrimInterval:    30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))
	r.RoomActivated("room1")

	base := time.Now().Add(-2 * time.Hour)
	mr.SetTime(base)
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("aged"),
	}))

	// Two retention windows later ON THE SERVER CLOCK: "aged" is now outside
	// the window and "fresh" is inside it, with no sleeping.
	mr.SetTime(base.Add(2 * time.Minute))
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindSync, Data: []byte("fresh"),
	}))

	require.NoError(t, r.trimOnce(context.Background()))

	entries := streamEntries(t, mr, r.scfg.syncKey("room1"))
	require.Len(t, entries, 1,
		"an entry inside the server-clock window must survive: a cutoff from this host's clock would be two hours past every id and sweep the stream")
	require.Equal(t, "fresh", entryValues(entries[0])[fieldData],
		"and the entry that survived must be the recent one, not merely some entry")
	require.Equal(t, uint64(1), r.StreamStats().Trimmed)
}

// The sweep must not cost one round trip per key. At the 10k-room target a
// sequential sweep is 20k round trips, which cannot finish inside
// TrimInterval — so neither the age bound nor the memory model would hold.
//
// Round trips are counted from the connection pool, not from wall-clock time
// (a race on a loaded machine) and not from the command count (which a
// pipeline does not change): every command takes one pool connection for its
// round trip, while one pipeline takes one for the whole chunk.
func TestUnit_StreamTrim_SweepIsPipelined(t *testing.T) {
	mr := newMiniRedis(t)
	c := newClient(t, mr)
	r, err := New(c, Config{
		Transport:       Streams,
		StreamRetention: time.Minute,
		TrimInterval:    30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	// Deliberately NOT Started: the reader goroutines Start launches issue
	// their own XREADs on the same pool, which would make both counts below a
	// race. streamRooms is what trimOnce reads, so it is populated directly.
	const rooms = 300 // 600 keys: more than trimBatch, so chunking is exercised
	r.streamMu.Lock()
	for i := 0; i < rooms; i++ {
		r.streamRooms[fmt.Sprintf("room-%d", i)] = 1
	}
	r.streamMu.Unlock()

	// Warm the pool: go-redis runs a handshake (HELLO, CLIENT SETINFO) on
	// every new connection, and those commands would land inside the window
	// measured below.
	require.NoError(t, c.Ping(context.Background()).Err())

	beforeCmds := mr.CommandCount()
	before := c.PoolStats()
	require.NoError(t, r.trimOnce(context.Background()))
	after := c.PoolStats()

	// One TIME plus every key's XTRIM: pipelining saves round trips, not
	// commands, so this is also the sequential count.
	require.Equal(t, 1+rooms*streamsPerRoom, mr.CommandCount()-beforeCmds,
		"every active room's two streams must be swept, on one TIME per sweep")

	trips := (after.Hits + after.Misses) - (before.Hits + before.Misses)
	chunks := (rooms*streamsPerRoom + trimBatch - 1) / trimBatch
	require.LessOrEqual(t, int(trips), 1+chunks,
		"600 XTRIMs must go in %d pipelined round trips (plus TIME), not 600", chunks)
}

// A failing XTRIM is logged and the sweep goes on: aborting would leave every
// key after it untrimmed until the next tick, and Trimmed must still count
// what actually went.
//
// The BROKEN key is the room's sync key, which trimOnce queues first, so the
// surviving awareness sweep behind it is only reached by a chunk that carries
// on past a failure.
func TestUnit_StreamTrim_ChunkFailureDoesNotAbortTheSweep(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{
		Transport:       Streams,
		StreamRetention: time.Minute,
		TrimInterval:    30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))
	r.RoomActivated("room1")

	base := time.Now().Add(-2 * time.Hour)
	mr.SetTime(base)
	require.NoError(t, r.publishStream(context.Background(), cluster.Outbound{
		Room: "room1", Kind: cluster.KindAwareness, Data: []byte("aged"),
	}))
	mr.SetTime(base.Add(2 * time.Minute)) // past AwarenessRetention on the server clock

	// A key of the wrong TYPE fails its own XTRIM and nothing else.
	require.NoError(t, mr.Set(r.scfg.syncKey("room1"), "not-a-stream"))

	require.NoError(t, r.trimOnce(context.Background()),
		"one failed key must not fail the sweep")
	require.Empty(t, streamEntries(t, mr, r.scfg.awKey("room1")),
		"the healthy key queued behind the failing one must still be swept")
	require.Equal(t, uint64(1), r.StreamStats().Trimmed,
		"Trimmed must count what actually went, and only that")
}

// A failed TIME skips the sweep rather than falling back to this host's clock.
// Trimming nothing costs memory until the next tick; trimming on a cutoff from
// the wrong clock deletes live entries.
func TestUnit_StreamTrim_TimeFailureSkipsTheSweep(t *testing.T) {
	mr := newMiniRedis(t)
	// MaxRetries: -1 so the dial against the closed server fails at once
	// instead of spending the default backoff schedule on it.
	c := goredis.NewClient(&goredis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = c.Close() })
	r, err := New(c, Config{
		Transport:       Streams,
		StreamRetention: time.Minute,
		TrimInterval:    30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	r.streamMu.Lock()
	r.streamRooms["room1"] = 1
	r.streamMu.Unlock()

	mr.Close() // every command now fails, TIME first

	require.ErrorContains(t, r.trimOnce(context.Background()), "TIME",
		"the sweep must abandon on the clock rather than proceed on a local cutoff")
	require.Zero(t, r.StreamStats().Trimmed)
}
