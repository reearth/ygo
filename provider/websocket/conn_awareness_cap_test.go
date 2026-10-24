package websocket

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/encoding"
)

// awEntry is one entry of an awareness frame built by awFrame.
type awEntry struct {
	id    uint64
	clock uint64
	state string
}

func liveEntry(id, clock uint64) awEntry { return awEntry{id, clock, `{"c":1}`} }
func nullEntry(id, clock uint64) awEntry { return awEntry{id, clock, "null"} }

// awFrame encodes an awareness update carrying entries in wire order.
func awFrame(entries ...awEntry) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(entries)))
		for _, e := range entries {
			enc.WriteVarUint(e.id)
			enc.WriteVarUint(e.clock)
			enc.WriteVarString(e.state)
		}
	})
}

// newConnCapServer returns a running server with the per-connection
// introduction cap set, and no room cap to interfere.
func newConnCapServer(t *testing.T, connCap int) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer()
	s.MaxAwarenessClientsPerConn = connCap
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s, ts
}

// sendAwFrame writes an awareness frame (outer tag 1, VarBytes payload).
func sendAwFrame(t *testing.T, conn *gws.Conn, payload []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // msgAwareness
	enc.WriteVarBytes(payload)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))
}

// seedRoomPresence applies live presence for IDs [1, n] directly to the room,
// as if other connections had introduced them.
func seedRoomPresence(t *testing.T, s *Server, n uint64) []awEntry {
	t.Helper()
	rm, _, err := s.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	defer s.releaseInflight(rm)
	seeded := make([]awEntry, 0, n)
	for id := uint64(1); id <= n; id++ {
		seeded = append(seeded, liveEntry(id, 1))
	}
	require.NoError(t, rm.awareness.ApplyUpdate(awFrame(seeded...), nil))
	require.Len(t, roomAwarenessIDs(t, s, "room"), int(n))
	return seeded
}

// tombstoneRoomPresence removes the given clients from the room, as a peer
// owning them disconnecting would.
func tombstoneRoomPresence(t *testing.T, s *Server, entries []awEntry) {
	t.Helper()
	rm, _, err := s.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	defer s.releaseInflight(rm)
	nulls := make([]awEntry, 0, len(entries))
	for _, e := range entries {
		nulls = append(nulls, nullEntry(e.id, e.clock+1))
	}
	require.NoError(t, rm.awareness.ApplyUpdate(awFrame(nulls...), nil))
	for _, e := range entries {
		require.False(t, roomAwarenessIDs(t, s, "room")[e.id])
	}
}

// waitForLiveID waits until the room holds live presence for id.
func waitForLiveID(t *testing.T, s *Server, id uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return roomAwarenessIDs(t, s, "room")[id]
	}, 2*time.Second, 5*time.Millisecond, "room never admitted client %d", id)
}

// waitProcessed sends a frame the gate always admits — a re-assert of an ID the
// connection already owns and the room already holds live — and waits for the
// room to show its new clock. Frames are handled in order on one read loop, so
// once this lands the verdict on every earlier frame is already applied.
func waitProcessed(t *testing.T, s *Server, conn *gws.Conn, id, clock uint64) {
	t.Helper()
	sendAwFrame(t, conn, awFrame(liveEntry(id, clock)))
	rm, _, err := s.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	defer s.releaseInflight(rm)
	require.Eventually(t, func() bool {
		m, ok := rm.awareness.Meta(id)
		return ok && m.Clock == clock
	}, 2*time.Second, 5*time.Millisecond, "connection never processed the ordering frame")
}

// A frame introducing more client IDs than the connection may is rejected whole.
func TestConnAwarenessCap_RejectsFloodFrame(t *testing.T) {
	s, ts := newConnCapServer(t, 4)
	conn := dialRoomInternal(t, ts, "room")

	flood := make([]awEntry, 0, 10)
	for id := uint64(1); id <= 10; id++ {
		flood = append(flood, liveEntry(id, 1))
	}
	sendAwFrame(t, conn, awFrame(flood...))

	// A later frame the gate admits orders the assertion below.
	const sentinel = 9000
	sendAwFrame(t, conn, awFrame(liveEntry(sentinel, 1)))
	waitForLiveID(t, s, sentinel)

	ids := roomAwarenessIDs(t, s, "room")
	for id := uint64(1); id <= 10; id++ {
		require.False(t, ids[id], "flooded client %d reached the room", id)
	}
}

// One connection's rejected flood does not spend another connection's budget.
func TestConnAwarenessCap_SecondConnectionUnaffected(t *testing.T) {
	s, ts := newConnCapServer(t, 4)
	flooder := dialRoomInternal(t, ts, "room")

	flood := make([]awEntry, 0, 10)
	for id := uint64(1); id <= 10; id++ {
		flood = append(flood, liveEntry(id, 1))
	}
	sendAwFrame(t, flooder, awFrame(flood...))

	const sentinel = 9000
	sendAwFrame(t, flooder, awFrame(liveEntry(sentinel, 1)))
	waitForLiveID(t, s, sentinel)

	// The second connection gets its own full budget, not what the flooder left.
	second := dialRoomInternal(t, ts, "room")
	own := make([]awEntry, 0, 4)
	for id := uint64(9001); id <= 9004; id++ {
		own = append(own, liveEntry(id, 1))
	}
	sendAwFrame(t, second, awFrame(own...))
	for _, e := range own {
		waitForLiveID(t, s, e.id)
	}
}

// A y-protocols timeout-removal frame names every client the sender has given
// up on as a null. Half of them the room already dropped — the very departures
// that triggered the sender's sweep — so those nulls name IDs that are not live
// either. No null is an introduction, so the sender's own cursor still lands.
func TestConnAwarenessCap_TimeoutRemovalFrameNotCounted(t *testing.T) {
	s, ts := newConnCapServer(t, 4)
	seeded := seedRoomPresence(t, s, 20)
	tombstoneRoomPresence(t, s, seeded[:10])
	conn := dialRoomInternal(t, ts, "room")

	frame := make([]awEntry, 0, len(seeded)+1)
	for _, e := range seeded {
		frame = append(frame, nullEntry(e.id, e.clock+2))
	}
	const cursor = 9000
	frame = append(frame, liveEntry(cursor, 1))
	sendAwFrame(t, conn, awFrame(frame...))

	waitForLiveID(t, s, cursor)
}

// A full-state echo re-asserts IDs the room already holds live; those are not
// introductions either.
func TestConnAwarenessCap_FullStateEchoNotCounted(t *testing.T) {
	s, ts := newConnCapServer(t, 4)
	seeded := seedRoomPresence(t, s, 20)
	conn := dialRoomInternal(t, ts, "room")

	frame := make([]awEntry, 0, len(seeded)+1)
	for _, e := range seeded {
		frame = append(frame, liveEntry(e.id, e.clock+1))
	}
	const cursor = 9000
	frame = append(frame, liveEntry(cursor, 1))
	sendAwFrame(t, conn, awFrame(frame...))

	waitForLiveID(t, s, cursor)
}

// Removing an introduced ID returns its slot to the connection.
func TestConnAwarenessCap_RemovalFreesASlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 1)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 2)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitForLiveID(t, s, idB)
	require.False(t, roomAwarenessIDs(t, s, "room")[idA], "removed client is still live")
}

// Reviving an ID the connection removed spends a slot again: a tombstone is not
// a licence to re-introduce for free.
func TestConnAwarenessCap_ReintroductionCountsAgain(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 1)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 2)))
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 3)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitProcessed(t, s, conn, idA, 5)
	require.False(t, roomAwarenessIDs(t, s, "room")[idB], "cap was not spent by the re-introduction")
}

// A removal earlier in a frame frees the slot an introduction later in the same
// frame needs.
func TestConnAwarenessCap_RemovalAndIntroductionInOneFrame(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 1)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 2), liveEntry(idB, 1)))

	waitForLiveID(t, s, idB)
	require.False(t, roomAwarenessIDs(t, s, "room")[idA], "removed client is still live")
}

// Retiring an ID and re-asserting it inside one frame is a re-introduction, so
// it keeps costing a slot: the room's liveness at the start of the frame does
// not excuse an assertion the frame's own removal came before.
func TestConnAwarenessCap_ReintroductionInOneFrameCountsAgain(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 1)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 2), liveEntry(idA, 3)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitProcessed(t, s, conn, idA, 5)
	require.False(t, roomAwarenessIDs(t, s, "room")[idB], "cap was not spent by the re-introduction")
}

// A null at a stale clock is discarded by the applier, so it frees no slot: the
// client it names is still live and still this connection's to answer for.
func TestConnAwarenessCap_StaleRemovalDoesNotFreeASlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 5)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 1)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitProcessed(t, s, conn, idA, 9)
	ids := roomAwarenessIDs(t, s, "room")
	require.True(t, ids[idA], "the stale removal took effect after all")
	require.False(t, ids[idB], "a discarded removal freed a slot")
}

// The same shape within one frame: the null is stale against the clock the
// frame itself just asserted, so the introduction after it has no slot and the
// whole frame is refused.
func TestConnAwarenessCap_StaleRemovalInOneFrameDoesNotFreeASlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB, idC = 101, 102, 103
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 10), nullEntry(idA, 7), liveEntry(idB, 1)))

	sendAwFrame(t, conn, awFrame(liveEntry(idC, 1)))
	waitForLiveID(t, s, idC)

	ids := roomAwarenessIDs(t, s, "room")
	require.False(t, ids[idA], "rejected frame reached the room")
	require.False(t, ids[idB], "rejected frame reached the room")
}

// A removal the applier honours still frees the slot, even after a stale one
// did not.
func TestConnAwarenessCap_HonouredRemovalStillFreesASlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 5)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 1)))
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 6)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitForLiveID(t, s, idB)
	require.False(t, roomAwarenessIDs(t, s, "room")[idA], "removed client is still live")
}

// ygo's own disconnect removals leave the clock unbumped, and the applier
// honours a null at the current clock when the client is live. The gate must
// agree, or a y-protocols client's removals would never free its slots.
func TestConnAwarenessCap_EqualClockRemovalFreesASlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 5)))
	waitForLiveID(t, s, idA)
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 5)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitForLiveID(t, s, idB)
	require.False(t, roomAwarenessIDs(t, s, "room")[idA], "removed client is still live")
}

// A client the room expired is already a tombstone at its own clock, so the
// null its connection sends is one the applier discards. The slot is released
// all the same: a connection whose awareness identity churns must not wedge.
func TestConnAwarenessCap_ExpiredClientFreesItsSlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 3)))
	waitForLiveID(t, s, idA)

	// Expiry tombstones at the unchanged clock, exactly as the sweeper does.
	rm, _, err := s.getOrCreateRoom(context.Background(), "room")
	require.NoError(t, err)
	defer s.releaseInflight(rm)
	rm.awareness.RemoveExpired(0)
	require.False(t, roomAwarenessIDs(t, s, "room")[idA])

	sendAwFrame(t, conn, awFrame(nullEntry(idA, 3)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitForLiveID(t, s, idB)
}

// The room's own awareness client (ID 0) cannot be retired from the wire: the
// applier protects its local state instead of removing it, so the gate frees no
// slot for that null either.
func TestConnAwarenessCap_RemovalOfTheRoomsOwnClientFreesNoSlot(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	const localID, idB = 0, 102
	sendAwFrame(t, conn, awFrame(liveEntry(localID, 1)))
	waitForLiveID(t, s, localID)
	sendAwFrame(t, conn, awFrame(nullEntry(localID, 2)))
	sendAwFrame(t, conn, awFrame(liveEntry(idB, 1)))

	waitProcessed(t, s, conn, localID, 9)
	ids := roomAwarenessIDs(t, s, "room")
	require.True(t, ids[localID], "the room's own client was removed from the wire")
	require.False(t, ids[idB], "a null the applier ignores freed a slot")
}

// A rejected frame leaves the budget exactly as it found it, however it shuffled
// ownership before hitting the cap.
func TestConnAwarenessCap_RejectedFrameLeavesNoResidue(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	// Introduce X, retire it, then introduce Y and Z: the second introduction
	// hits the cap and the whole frame is rejected.
	const idX, idY, idZ, idW = 101, 102, 103, 104
	sendAwFrame(t, conn, awFrame(
		liveEntry(idX, 1), nullEntry(idX, 2), liveEntry(idY, 1), liveEntry(idZ, 1)))

	sendAwFrame(t, conn, awFrame(liveEntry(idW, 1)))
	waitForLiveID(t, s, idW)

	ids := roomAwarenessIDs(t, s, "room")
	for _, id := range []uint64{idX, idY, idZ} {
		require.False(t, ids[id], "rejected frame put client %d in the room", id)
	}
}

// A frame carrying an oversized state fails at the applier wholesale, so the
// gate must refuse it too: admitting it would release the slots of the clients
// it retires while leaving them live in the room, which is the cap undone one
// megabyte at a time.
func TestConnAwarenessCap_OversizedStateFrameFreesNoSlot(t *testing.T) {
	s, ts := newConnCapServer(t, 3)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB, idX, idC, idD = 101, 102, 103, 104, 105
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 1), liveEntry(idB, 1), liveEntry(idX, 1)))
	waitForLiveID(t, s, idX)

	// Retire two of the three, with an oversized state for the third. The
	// applier refuses the frame outright, so A and B stay live and the
	// connection still speaks for all three.
	oversized := awEntry{id: idX, clock: 2, state: `"` + strings.Repeat("x", 1<<20) + `"`}
	sendAwFrame(t, conn, awFrame(nullEntry(idA, 2), nullEntry(idB, 2), oversized))

	sendAwFrame(t, conn, awFrame(liveEntry(idC, 1), liveEntry(idD, 1)))

	waitProcessed(t, s, conn, idX, 9)
	ids := roomAwarenessIDs(t, s, "room")
	require.True(t, ids[idA], "the refused frame was applied after all")
	require.True(t, ids[idB], "the refused frame was applied after all")
	require.False(t, ids[idC], "a frame the applier refuses freed a slot")
	require.False(t, ids[idD], "a frame the applier refuses freed a slot")
}

// The gate's frame limits must stay exactly the applier's, so no frame can pass
// the gate and then fail the apply wholesale. The applier itself is the oracle:
// if the awareness package moves a ceiling, this fails.
func TestConnAwarenessCap_FrameLimitsMatchTheApplier(t *testing.T) {
	bigState := func(n int) string { return `"` + strings.Repeat("x", n-2) + `"` }
	manyEntries := func(n int) []awEntry {
		out := make([]awEntry, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, liveEntry(uint64(i+1), 1))
		}
		return out
	}

	cases := []struct {
		name    string
		payload []byte
		refused bool
	}{
		{"state at the limit", awFrame(awEntry{1, 1, bigState(maxAwarenessStateBytesPerEntry)}), false},
		{"state past the limit", awFrame(awEntry{1, 1, bigState(maxAwarenessStateBytesPerEntry + 1)}), true},
		{"entries at the limit", awFrame(manyEntries(maxAwarenessEntriesPerFrame)...), false},
		{"entries past the limit", awFrame(manyEntries(maxAwarenessEntriesPerFrame + 1)...), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applyErr := awareness.New(0).ApplyUpdate(c.payload, nil)
			_, gateErr := decodeAwarenessEntries(c.payload)
			require.Equal(t, c.refused, applyErr != nil, "applier disagrees with the case")
			require.Equal(t, applyErr != nil, gateErr != nil,
				"gate and applier disagree: apply=%v gate=%v", applyErr, gateErr)
		})
	}
}

// The gate is on by default: an unset cap is 64, not unlimited and not zero.
func TestConnAwarenessCap_DefaultIsSixtyFour(t *testing.T) {
	require.Equal(t, 64, NewServer().maxAwarenessClientsPerConn())
}

// End to end on a default-configured server: 64 introductions are admitted and
// the 65th is refused.
func TestConnAwarenessCap_DefaultAdmitsSixtyFourIntroductions(t *testing.T) {
	s := NewServer()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	conn := dialRoomInternal(t, ts, "room")

	within := make([]awEntry, 0, 64)
	for id := uint64(1); id <= 64; id++ {
		within = append(within, liveEntry(id, 1))
	}
	sendAwFrame(t, conn, awFrame(within...))
	waitForLiveID(t, s, 64)

	const oneTooMany = 65
	sendAwFrame(t, conn, awFrame(liveEntry(oneTooMany, 1)))
	waitProcessed(t, s, conn, 1, 9)
	require.False(t, roomAwarenessIDs(t, s, "room")[oneTooMany], "the 65th introduction was admitted")
}

// A truncated frame is dropped without panicking, spends no budget, and leaves
// the connection usable. Cap 1: the entry decoded before the truncation must
// not take the slot the following valid frame needs.
func TestConnAwarenessCap_MalformedFrameDropped(t *testing.T) {
	s, ts := newConnCapServer(t, 1)
	conn := dialRoomInternal(t, ts, "room")

	truncated := encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(2) // claims two entries
		enc.WriteVarUint(101)
		enc.WriteVarUint(1)
		enc.WriteVarString(`{"c":1}`)
	})
	sendAwFrame(t, conn, truncated)

	const sentinel = 9000
	sendAwFrame(t, conn, awFrame(liveEntry(sentinel, 1)))
	waitForLiveID(t, s, sentinel)
	require.False(t, roomAwarenessIDs(t, s, "room")[101], "truncated frame was partly applied")
}

// A negative cap disables the gate entirely.
func TestConnAwarenessCap_NegativeDisablesGate(t *testing.T) {
	s, ts := newConnCapServer(t, -1)
	conn := dialRoomInternal(t, ts, "room")

	flood := make([]awEntry, 0, 50)
	for id := uint64(1); id <= 50; id++ {
		flood = append(flood, liveEntry(id, 1))
	}
	sendAwFrame(t, conn, awFrame(flood...))

	require.Eventually(t, func() bool {
		return len(roomAwarenessIDs(t, s, "room")) == 50
	}, 2*time.Second, 5*time.Millisecond, "gate was not disabled")
}

// Disconnecting broadcasts a removal for the IDs the connection still holds,
// not for ones it already removed.
func TestConnAwarenessCap_DisconnectRemovesOnlyLiveIDs(t *testing.T) {
	s, ts := newConnCapServer(t, 4)
	conn := dialRoomInternal(t, ts, "room")

	const idA, idB = 101, 102
	sendAwFrame(t, conn, awFrame(liveEntry(idA, 1), liveEntry(idB, 1)))
	waitForLiveID(t, s, idA)
	waitForLiveID(t, s, idB)
	sendAwFrame(t, conn, awFrame(nullEntry(idB, 2)))
	require.Eventually(t, func() bool {
		return !roomAwarenessIDs(t, s, "room")[idB]
	}, 2*time.Second, 5*time.Millisecond, "room never applied the removal")

	observer := dialRoomInternal(t, ts, "room")
	drainHandshakeFrames(t, observer)
	require.NoError(t, conn.Close())

	removed := readRemovalBroadcast(t, observer)
	require.True(t, removed[idA], "disconnect did not remove the client still held")
	require.False(t, removed[idB], "disconnect re-removed an already-removed client")
}

// drainHandshakeFrames reads the three frames the server sends on connect
// (step-1, step-2, awareness state), so the next frame read is a broadcast.
// The awareness state carries existing tombstones, which would otherwise be
// mistaken for a removal broadcast.
func drainHandshakeFrames(t *testing.T, conn *gws.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for i := 0; i < 3; i++ {
		_, _, err := conn.ReadMessage()
		require.NoError(t, err)
	}
}

// readRemovalBroadcast reads frames until an awareness update carrying null
// entries arrives, and returns the IDs it removes.
func readRemovalBroadcast(t *testing.T, conn *gws.Conn) map[uint64]bool {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		_, data, err := conn.ReadMessage()
		require.NoError(t, err, "no removal broadcast arrived")
		dec := encoding.NewDecoder(data)
		outerType, err := dec.ReadVarUint()
		require.NoError(t, err)
		if outerType != 1 { // msgAwareness
			continue
		}
		payload, err := dec.ReadVarBytes()
		require.NoError(t, err)
		if removed := nullIDsIn(t, payload); len(removed) > 0 {
			return removed
		}
	}
}

// nullIDsIn returns the client IDs an awareness update marks as removed.
func nullIDsIn(t *testing.T, payload []byte) map[uint64]bool {
	t.Helper()
	dec := encoding.NewDecoder(payload)
	n, err := dec.ReadVarUint()
	require.NoError(t, err)
	out := map[uint64]bool{}
	for i := uint64(0); i < n; i++ {
		id, err := dec.ReadVarUint()
		require.NoError(t, err)
		_, err = dec.ReadVarUint()
		require.NoError(t, err)
		state, err := dec.ReadVarString()
		require.NoError(t, err)
		if state == "null" || state == "" {
			out[id] = true
		}
	}
	return out
}
