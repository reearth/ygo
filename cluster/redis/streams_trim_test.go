package redis

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
)

// r.Start is required in every test below that calls RoomActivated, even
// though the task-9 brief's version omitted it: RoomActivated returns
// immediately when !r.started.Load(), before it ever touches streamRooms —
// so without Start, streamRooms stays empty, trimOnce finds no rooms, and
// these tests would pass vacuously (or fail outright) regardless of whether
// trimOnce is correct. Same class of gap already documented in
// streams_reader_test.go's TestUnit_StreamReader_ActivationRefcounts.
//
// These tests also do NOT use miniredis's mr.FastForward to simulate aged
// entries, unlike the brief's version. FastForward only decreases TTLs
// (miniredis's db.fastForward walks expiring keys) — it does not move
// miniredis's own now() used by XADD's "*" auto-ID, which stays real
// time.Now() unless a test calls the separate mr.SetTime. trimOnce's cutoff
// is computed from real time.Now() too (see streams_trim.go), entirely on
// the Go client side — XTRIM MINID takes an explicit ID and does not consult
// any server-side clock at all. So mr.FastForward has NO effect on whether
// an entry looks old to either side of this mechanism: an entry published a
// few microseconds before trimOnce runs is still only a few microseconds
// old by real time, FastForward or not, and the brief's tests would have
// failed to observe any trimming at all. Real time.Sleep — already this
// package's established idiom (see e.g. streams_reader_test.go,
// internal_test.go, redis_test.go; grep finds no existing use of
// FastForward anywhere in this package) — is what actually ages an entry,
// so retention/AwarenessRetention below are set small enough to keep that
// fast.

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
// watches r.done directly, the same way runPublisher/runSubscriber do — NOT
// only ctx.Done(), which is what the task-9 brief's version of this file
// did. ctx here is context.Background(), which is never cancelled (matching
// the ordinary case documented on streamReadCtx: ctx usually outlives the
// relay), so a sweeper gated on ctx.Done() alone would never observe Close
// at all, and Close's r.wg.Wait() would block forever. Verified by mutation:
// dropping the "case <-r.done: return" arm reproduces exactly that hang
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
