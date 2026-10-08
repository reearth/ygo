package websocket

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// newIdleSweepServer returns a server in idle-residency mode whose sweeper
// ticks every 5ms, plus a per-room OnUnloadDocument signal.
func newIdleSweepServer(timeout time.Duration) (*Server, func(t *testing.T, name string, d time.Duration) bool) {
	s := NewServerWithPersistence(&idleRecordAdapter{})
	s.RoomIdleTimeout = timeout
	s.idleSweepInterval = 5 * time.Millisecond
	return s, newUnloadSignal(s)
}

func insertX(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
	txt := doc.GetText("t")
	transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
}

// A room that only Apply ever touched has no peer to leave it, so nothing but
// Apply itself can stamp it idle. The sweeper must reclaim it once it has been
// idle for RoomIdleTimeout, whichever way Apply returned.
func TestIdleSweep_EvictsRoomOnlyApplyTouched(t *testing.T) {
	cases := []struct {
		name      string
		fn        func(*crdt.Doc, func(func(*crdt.Transaction)))
		wantErr   error
		wantPanic bool
	}{
		{name: "fn writes", fn: insertX},
		{
			name:    "fn writes nothing",
			fn:      func(*crdt.Doc, func(func(*crdt.Transaction))) {},
			wantErr: ErrNoChanges,
		},
		{
			name: "fn panics",
			fn: func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
				insertX(doc, transact)
				panic("fn failed")
			},
			wantPanic: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, waitUnloaded := newIdleSweepServer(30 * time.Millisecond)
			apply := func() error { return s.Apply(context.Background(), "room", tc.fn) }
			switch {
			case tc.wantPanic:
				assert.Panics(t, func() { _ = apply() })
			case tc.wantErr != nil:
				require.ErrorIs(t, apply(), tc.wantErr)
			default:
				require.NoError(t, apply())
			}

			require.True(t, waitUnloaded(t, "room", 2*time.Second),
				"the sweeper must evict a room only Apply touched once it is idle past RoomIdleTimeout")
			_, present, _ := roomState(s, "room")
			assert.False(t, present, "swept room must be gone from s.rooms")
			require.NoError(t, s.Shutdown(context.Background()))
		})
	}
}

// Apply clears the idle stamp of a room whose last peer has already left. The
// room must still be swept after Apply returns, not kept until process exit.
func TestIdleSweep_EvictsIdleRoomAfterApply(t *testing.T) {
	s, waitUnloaded := newIdleSweepServer(200 * time.Millisecond)
	lastPeer := newLastPeerSignal(s)
	ts := httptest.NewServer(s)
	defer ts.Close()

	doc := crdt.New(crdt.WithClientID(1))
	conn := dialWS(t, ts, "room")
	drainWS(t, conn, doc)
	_ = conn.Close()
	<-lastPeer

	_, present, idleSince := roomState(s, "room")
	require.True(t, present, "the room must be idle-resident before Apply")
	require.False(t, idleSince.IsZero(), "the last peer leaving must stamp the room idle")

	err := s.Apply(context.Background(), "room", func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	require.ErrorIs(t, err, ErrNoChanges)

	require.True(t, waitUnloaded(t, "room", 3*time.Second),
		"an idle room an Apply touched must be evicted once idle past RoomIdleTimeout again")
	require.NoError(t, s.Shutdown(context.Background()))
}

// A room must stay resident while an Apply on it is still running, however long
// that is, and its idle time is counted from when Apply returns.
func TestIdleSweep_KeepsRoomWhileApplyRuns(t *testing.T) {
	const timeout = 300 * time.Millisecond
	s, waitUnloaded := newIdleSweepServer(timeout)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Apply(context.Background(), "room", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			insertX(doc, transact)
			close(started)
			<-release
		})
	}()
	<-started

	assert.False(t, waitUnloaded(t, "room", 2*timeout),
		"a room must not be evicted while an Apply on it is still running")
	close(release)
	require.NoError(t, <-done)

	assert.False(t, waitUnloaded(t, "room", timeout/2),
		"the room's idle time must start when Apply returns, not when it started")
	require.True(t, waitUnloaded(t, "room", 3*time.Second),
		"the room must be evicted once idle past RoomIdleTimeout after Apply returned")
	require.NoError(t, s.Shutdown(context.Background()))
}

// MaxResidentRooms bounds every idle room, including those only Apply touched.
func TestIdleSweep_MaxResidentRoomsCountsRoomsOnlyApplyTouched(t *testing.T) {
	s, waitUnloaded := newIdleSweepServer(10 * time.Second) // isolate the LRU bound
	s.MaxResidentRooms = 1

	require.NoError(t, s.Apply(context.Background(), "room1", insertX))
	time.Sleep(3 * time.Millisecond)
	require.NoError(t, s.Apply(context.Background(), "room2", insertX))

	require.True(t, waitUnloaded(t, "room1", 2*time.Second),
		"LRU: the least-recently-idle room must be evicted once idle rooms exceed MaxResidentRooms")
	_, present, _ := roomState(s, "room2")
	assert.True(t, present, "the most-recently-idle room must stay resident")
	require.NoError(t, s.Shutdown(context.Background()))
}

// A relay delivery auto-creates the room on a node with no local peers for it.
// That room must be swept like any other idle room.
func TestIdleSweep_EvictsRoomOnlyRelayTouched(t *testing.T) {
	remote := crdt.New(crdt.WithClientID(99))
	txt := remote.GetText("t")
	remote.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "remote", nil) })

	cases := []struct {
		name string
		in   cluster.Inbound
	}{
		{name: "sync", in: cluster.Inbound{Room: "room", Kind: cluster.KindSync, Data: crdt.EncodeStateAsUpdateV1(remote, nil)}},
		{name: "awareness", in: cluster.Inbound{Room: "room", Kind: cluster.KindAwareness, Data: awarenessUpdateFor(77)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, waitUnloaded := newIdleSweepServer(30 * time.Millisecond)
			require.NoError(t, s.AttachRelay(&captureRelay{}))

			require.NoError(t, s.Inject(context.Background(), tc.in))

			require.True(t, waitUnloaded(t, "room", 2*time.Second),
				"the sweeper must evict a room only the relay touched once it is idle past RoomIdleTimeout")
			require.NoError(t, s.Shutdown(context.Background()))
		})
	}
}
