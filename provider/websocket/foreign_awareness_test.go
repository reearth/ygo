package websocket

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/encoding"
)

// newForeignAwarenessServer returns a server with a relay attached, so Inject
// stamps the relay origin as it does in a real cluster.
func newForeignAwarenessServer(t *testing.T, roomCap, foreignCap int) *Server {
	t.Helper()
	s := NewServer()
	s.MaxAwarenessClientsPerRoom = roomCap
	s.MaxForeignAwarenessClients = foreignCap
	require.NoError(t, s.AttachRelay(&captureRelay{}))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

// injectForeignPresence relays one awareness entry per ID in [1, n].
func injectForeignPresence(t *testing.T, s *Server, room string, n uint64) {
	t.Helper()
	for id := uint64(1); id <= n; id++ {
		require.NoError(t, s.Inject(context.Background(), cluster.Inbound{
			Room: room, Kind: cluster.KindAwareness, Data: awarenessUpdateFor(id),
		}))
	}
}

func roomAwarenessIDs(t *testing.T, s *Server, room string) map[uint64]bool {
	t.Helper()
	rm, _, err := s.getOrCreateRoom(context.Background(), room)
	require.NoError(t, err)
	defer s.releaseInflight(rm)
	ids := map[uint64]bool{}
	for id := range rm.awareness.GetStates() {
		ids[id] = true
	}
	return ids
}

// The local client keeps its slot while a relayed flood fills the foreign share.
func TestForeignAwareness_LocalClientKeepsItsSlot(t *testing.T) {
	s := newForeignAwarenessServer(t, 8, 4)
	ts := httptest.NewServer(s)
	defer ts.Close()

	injectForeignPresence(t, s, "room", 20)

	const localID = 1000
	conn, _, err := gws.DefaultDialer.Dial("ws"+ts.URL[len("http"):]+"/room", nil)
	require.NoError(t, err)
	defer conn.Close()

	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // msgAwareness
	enc.WriteVarBytes(awarenessUpdateFor(localID))
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))

	require.Eventually(t, func() bool {
		return roomAwarenessIDs(t, s, "room")[localID]
	}, 2*time.Second, 10*time.Millisecond, "local client's presence was refused")
	require.LessOrEqual(t, len(roomAwarenessIDs(t, s, "room"))-1, 4, "foreign presence exceeded its sub-cap")
}

// With no sub-cap configured, relayed presence is admitted exactly as before.
func TestForeignAwareness_DefaultAdmitsEverything(t *testing.T) {
	s := newForeignAwarenessServer(t, 0, 0)
	injectForeignPresence(t, s, "room", 20)
	require.Len(t, roomAwarenessIDs(t, s, "room"), 20)
}

// The sub-cap applies even when the room cap is unlimited.
func TestForeignAwareness_HonouredWithUnlimitedRoomCap(t *testing.T) {
	s := newForeignAwarenessServer(t, 0, 4)
	injectForeignPresence(t, s, "room", 20)
	require.Len(t, roomAwarenessIDs(t, s, "room"), 4)
}

// A sub-cap above the room cap has no effect: the room cap still binds.
func TestForeignAwareness_SubCapAboveRoomCapAdmitsNoMoreThanRoomCap(t *testing.T) {
	s := newForeignAwarenessServer(t, 4, 100)
	injectForeignPresence(t, s, "room", 20)
	require.Len(t, roomAwarenessIDs(t, s, "room"), 4)
}

func injectAwareness(t *testing.T, s *Server, payload []byte) {
	t.Helper()
	require.NoError(t, s.Inject(context.Background(), cluster.Inbound{
		Room: "room", Kind: cluster.KindAwareness, Data: payload,
	}))
}

// Relayed churn leaves tombstones behind, but they cannot fill the room cap and
// take the slots the sub-cap reserves for local clients.
func TestForeignAwareness_RelayedChurnKeepsLocalReservation(t *testing.T) {
	s := newForeignAwarenessServer(t, 8, 4)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	for id := uint64(1); id <= 20; id++ {
		injectAwareness(t, s, awFrame(liveEntry(id, 1)))
		injectAwareness(t, s, awFrame(nullEntry(id, 1)))
	}
	conn := dialRoomInternal(t, ts, "room")
	sendAwFrame(t, conn, awFrame(liveEntry(1000, 1)))
	waitForLiveID(t, s, 1000)
}

// The same with bare relayed nulls for clients the room never saw.
func TestForeignAwareness_RelayedNullsKeepLocalReservation(t *testing.T) {
	s := newForeignAwarenessServer(t, 8, 4)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	nulls := make([]awEntry, 0, 20)
	for id := uint64(1); id <= 20; id++ {
		nulls = append(nulls, nullEntry(id, 1))
	}
	injectAwareness(t, s, awFrame(nulls...))
	conn := dialRoomInternal(t, ts, "room")
	sendAwFrame(t, conn, awFrame(liveEntry(1000, 1)))
	waitForLiveID(t, s, 1000)
}

// Relayed presence the sub-cap refuses is not forwarded to local peers either.
func TestForeignAwareness_RefusedPresenceReachesNoPeer(t *testing.T) {
	s := newForeignAwarenessServer(t, 0, 2)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	conn := dialRoomInternal(t, ts, "room")
	drainHandshakeFrames(t, conn)

	flood := make([]awEntry, 0, 10)
	for id := uint64(1); id <= 10; id++ {
		flood = append(flood, liveEntry(id, 1))
	}
	injectAwareness(t, s, awFrame(flood...))
	require.Len(t, roomAwarenessIDs(t, s, "room"), 2)

	got := readAwarenessIDs(t, conn)
	require.Equal(t, map[uint64]bool{1: true, 2: true}, got)
}

// readAwarenessIDs reads frames until an awareness update arrives and returns
// the client IDs it names.
func readAwarenessIDs(t *testing.T, conn *gws.Conn) map[uint64]bool {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	for {
		_, data, err := conn.ReadMessage()
		require.NoError(t, err, "no awareness frame arrived")
		dec := encoding.NewDecoder(data)
		outerType, err := dec.ReadVarUint()
		require.NoError(t, err)
		if outerType != 1 { // msgAwareness
			continue
		}
		payload, err := dec.ReadVarBytes()
		require.NoError(t, err)
		dec = encoding.NewDecoder(payload)
		n, err := dec.ReadVarUint()
		require.NoError(t, err)
		ids := map[uint64]bool{}
		for i := uint64(0); i < n; i++ {
			id, err := dec.ReadVarUint()
			require.NoError(t, err)
			_, err = dec.ReadVarUint()
			require.NoError(t, err)
			_, err = dec.ReadVarString()
			require.NoError(t, err)
			ids[id] = true
		}
		return ids
	}
}
