package websocket_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// stallingRelay blocks Publish for one room until released, standing in for a
// relay that is slow for a single room. Every other room's Publish must still
// get through — the outbound half of #187.
type stallingRelay struct {
	stallRoom string
	release   chan struct{}

	mu        sync.Mutex
	published []cluster.Outbound
}

func newStallingRelay(stallRoom string) *stallingRelay {
	return &stallingRelay{stallRoom: stallRoom, release: make(chan struct{})}
}

func (r *stallingRelay) Publish(ctx context.Context, out cluster.Outbound) error {
	if out.Room == r.stallRoom {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.published = append(r.published, out)
	r.mu.Unlock()
	return nil
}

func (r *stallingRelay) Start(context.Context, cluster.Sink) error { return nil }
func (r *stallingRelay) RoomActivated(string)                      {}
func (r *stallingRelay) RoomDeactivated(string)                    {}
func (r *stallingRelay) Close() error                              { return nil }

func (r *stallingRelay) roomsPublished() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.published))
	for _, p := range r.published {
		out = append(out, p.Room)
	}
	return out
}

// syncUpdate returns one V1 update blob that inserts text into a fresh doc.
func syncUpdate(t *testing.T, s string) []byte {
	t.Helper()
	d := crdt.New()
	txt := d.GetText("t")
	d.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, s, nil) })
	return crdt.EncodeStateAsUpdateV1(d, nil)
}

// applyLocal creates room (dialing a peer connection, exactly like a real
// client joining, so getOrCreateRoom wires the relay observers) and then
// applies update to the room's own doc with a non-sentinel (nil) origin —
// the "documented pattern" also used by inject_test.go's
// TestUnit_BroadcastUpdate_Relays: apply directly to serverDoc, then
// (optionally) fan out.
//
// This is deliberately NOT srv.BroadcastUpdate: BroadcastUpdate (1) requires
// the room to already exist (it looks the room up and returns
// ErrRoomNotFound otherwise) and, more importantly, (2) never touches the
// room's own doc — it only fans already-applied bytes out to already
// -connected peers (see inject.go's broadcastUpdate). It therefore never
// fires doc.OnUpdate and cannot reach registerRelayObservers' relay
// subscription at all, no matter how the outbound path is implemented.
//
// It is also deliberately NOT srv.Apply: Apply tags its own transaction with
// a private `origin := new(struct{})` sentinel, and Go's zero-size-value
// guarantee (https://go.dev/ref/spec#Size_and_alignment_guarantees) means
// EVERY `new(struct{})` in the process may share one address — in practice
// Apply's origin pointer compares equal (==) to AttachRelay's own
// `relaySentinel := new(struct{})`. That makes registerRelayObservers' echo
// guard (`if origin == sentinel`) always fire for an Apply-driven change,
// silently swallowing it before it ever reaches enqueueRelayOutbound. This
// is a pre-existing latent bug in Server.Apply + relay interaction, entirely
// outside Task 5's file scope (inject.go, not cluster.go/server.go) — flagged
// separately rather than fixed here.
func applyLocal(t *testing.T, ts *httptest.Server, srv *ygws.Server, room, text string) {
	t.Helper()
	conn := dial(t, ts, room)
	drainHandshake(t, conn, crdt.New())
	require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc(room), syncUpdate(t, text), nil))
}

// THE OUTBOUND #187 GATE: a room whose Publish is wedged must not stop any
// other room from publishing. Fails before the fix, where one worker drains a
// single shared queue.
func TestRelayOutbound_CrossRoomIsolation(t *testing.T) {
	relay := newStallingRelay("slow")
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		close(relay.release)
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	// Wedge "slow", then write to "fast".
	applyLocal(t, ts, srv, "slow", "a")
	applyLocal(t, ts, srv, "fast", "b")

	require.Eventually(t, func() bool {
		for _, room := range relay.roomsPublished() {
			if room == "fast" {
				return true
			}
		}
		return false
	}, 2*time.Second, 10*time.Millisecond,
		`room "fast" must publish while room "slow" is wedged`)
}
