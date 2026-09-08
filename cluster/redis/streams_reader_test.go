package redis

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Assignment must be stable: a room that moved readers between cycles would
// have two readers holding cursors for it.
func TestUnit_StreamReader_AssignmentIsStable(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	first := r.readerFor("room-alpha")
	for i := 0; i < 100; i++ {
		require.Equal(t, first, r.readerFor("room-alpha"))
	}
	require.GreaterOrEqual(t, first, 0)
	require.Less(t, first, 4)
}

// With many rooms every reader should get work; a hash that piles everything
// onto one reader would defeat the sharding.
func TestUnit_StreamReader_AssignmentSpreadsAcrossReaders(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams, Readers: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	used := map[int]int{}
	for i := 0; i < 400; i++ {
		used[r.readerFor(fmt.Sprintf("room-%d", i))]++
	}
	require.Len(t, used, 4, "every reader should own some rooms")
	for idx, n := range used {
		require.Greater(t, n, 20, "reader %d got only %d of 400 rooms", idx, n)
	}
}

// XREAD command size must be bounded independently of Readers: 10k rooms over
// 4 readers would otherwise build a ~5000-argument command.
func TestUnit_StreamReader_KeyBatching(t *testing.T) {
	keys := make([]string, 0, 1300)
	for i := 0; i < 1300; i++ {
		keys = append(keys, fmt.Sprintf("k%d", i))
	}

	batches := keyBatches(keys, maxKeysPerRead)
	require.Len(t, batches, 3)
	require.Len(t, batches[0], 512)
	require.Len(t, batches[1], 512)
	require.Len(t, batches[2], 276)

	seen := 0
	for _, b := range batches {
		seen += len(b)
	}
	require.Equal(t, len(keys), seen, "batching must not drop keys")
}

func TestUnit_StreamReader_KeyBatchingEmpty(t *testing.T) {
	require.Empty(t, keyBatches(nil, maxKeysPerRead))
}

// The Relay contract requires tolerating a successor RoomActivated before the
// predecessor's RoomDeactivated, in either order. Refcounting rides that out;
// treating deactivation as an unconditional removal would drop a live room.
//
// r.Start is required here even though the brief's version of this test
// omitted it: RoomActivated/RoomDeactivated both return immediately when
// !r.started.Load(), before ever reaching the pub/sub work this task's
// stream block is appended after — so without Start, streamRooms would
// never be touched and this test would fail regardless of whether the new
// refcounting code is correct (the same class of gap already caught and
// documented in streams_test.go's Both/PubSub Publish tests).
//
// The brief's version of this test asserted only presence/absence via
// roomsForReader, which does not actually distinguish a true integer refcount
// from a naive boolean flag that merely mirrors whether activeRooms[room] is
// zero: since every RoomActivated/RoomDeactivated call drives BOTH activeRooms
// and streamRooms off the exact same events, a flag that flips on the 0->1
// and >0->0 crossings would show identically "present" after the successor
// activation and "absent" after the final deactivation, with no assertion
// ever distinguishing it from streamRooms holding the real count (2, then 1).
// Added a direct read of r.streamRooms (this is package redis, so the
// unexported field is reachable) to require the count itself is 2 after both
// activations, and 1 after the predecessor's deactivation — the "reference
// count" the interface's own doc comment calls for, not just a derived flag.
func TestUnit_StreamReader_ActivationRefcounts(t *testing.T) {
	mr := newMiniRedis(t)
	r, err := New(newClient(t, mr), Config{Transport: Streams})
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Start(context.Background(), &countingSink{}))

	r.RoomActivated("room1") // predecessor
	r.RoomActivated("room1") // successor, before the predecessor tears down

	r.streamMu.Lock()
	require.Equal(t, 2, r.streamRooms["room1"], "two activations must produce a count of 2, not a boolean flag")
	r.streamMu.Unlock()

	r.RoomDeactivated("room1")

	r.streamMu.Lock()
	require.Equal(t, 1, r.streamRooms["room1"], "one of two activations released must leave a count of 1")
	r.streamMu.Unlock()

	idx := r.readerFor("room1")
	require.Contains(t, r.roomsForReader(idx), "room1",
		"a successor activation must keep the room assigned")

	r.RoomDeactivated("room1")
	require.NotContains(t, r.roomsForReader(idx), "room1")
}
