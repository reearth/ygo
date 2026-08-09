package websocket_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// shutdownProbeRelay is the #202 acceptance harness: a relay whose Publish
// wedges (until released or its ctx is cancelled) while exposing exactly the
// two facts those tests must assert on — every successfully published
// payload, and how many Publish calls are in flight right now.
//
// Distinct from relay_lane_test.go's stallingRelay, which wedges by room name
// and records nothing about in-flight calls: the #202 tests need to observe
// "a Publish call outlived Shutdown" directly, not infer it.
type shutdownProbeRelay struct {
	release  chan struct{}
	inFlight atomic.Int64

	// ignoreCtx makes Publish sleep out its wedge even when ctx is cancelled,
	// standing in for a relay mid-network-write that cannot unwind instantly.
	// Used by the join test; zero means "honor ctx like a conforming relay".
	ignoreCtxFor time.Duration

	mu        sync.Mutex
	published []cluster.Outbound
}

func newShutdownProbeRelay() *shutdownProbeRelay {
	return &shutdownProbeRelay{release: make(chan struct{})}
}

func (r *shutdownProbeRelay) Publish(ctx context.Context, out cluster.Outbound) error {
	r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	if r.ignoreCtxFor > 0 {
		time.Sleep(r.ignoreCtxFor)
	} else {
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

func (r *shutdownProbeRelay) Start(context.Context, cluster.Sink) error { return nil }
func (r *shutdownProbeRelay) RoomActivated(string)                      {}
func (r *shutdownProbeRelay) RoomDeactivated(string)                    {}
func (r *shutdownProbeRelay) Close() error                              { return nil }

// syncPayloads returns the successfully published KindSync blobs for room.
func (r *shutdownProbeRelay) syncPayloads(room string) [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]byte
	for _, p := range r.published {
		if p.Room == room && p.Kind == cluster.KindSync {
			out = append(out, p.Data)
		}
	}
	return out
}

// replayedTextLen applies blobs to a fresh doc and reports the length of its
// "t" text — the ground truth for "how much of the backlog actually arrived".
func replayedTextLen(t *testing.T, blobs [][]byte) int {
	t.Helper()
	d := crdt.New()
	for _, b := range blobs {
		require.NoError(t, crdt.ApplyUpdateV1(d, b, nil))
	}
	return len(d.GetText("t").ToString())
}

// THE #202 DELIVERY GATE: updates queued in a room's outbound lane when
// Shutdown is called must still be PUBLISHED when the relay recovers within
// Shutdown's ctx budget. Before the fix, Shutdown cancelled the relay context
// as its second act, so the wedged Publish aborted, the lane's backlog was
// discarded by the worker's bare ctx.Done() return, and releasing the wedge
// mid-Shutdown delivered nothing — with Dropped still reading zero.
func TestRelayShutdown_DeliversBacklogWhenRelayRecovers(t *testing.T) {
	relay := newShutdownProbeRelay()
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	conn := dial(t, ts, "r")
	drainHandshake(t, conn, crdt.New())

	// Wedge the worker inside its first Publish, then pile a backlog behind it.
	const edits = 50
	for i := 0; i < edits; i++ {
		require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "a"), nil))
	}
	require.Eventually(t, func() bool { return relay.inFlight.Load() == 1 },
		2*time.Second, 5*time.Millisecond,
		"the lane worker must be wedged inside Publish before Shutdown for this test to prove anything")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()

	// Let Shutdown get past its connection-close phase while the relay is
	// still wedged, then let the relay recover. The 200ms is not load-bearing
	// for the pre-fix failure: before the fix Shutdown has already cancelled
	// the relay context (aborting the wedged Publish and killing the worker)
	// regardless of when release is closed.
	time.Sleep(200 * time.Millisecond)
	close(relay.release)

	select {
	case err := <-done:
		require.NoError(t, err, "Shutdown must complete within its generous ctx once the relay recovers")
	case <-time.After(8 * time.Second):
		t.Fatal("Shutdown did not return after the relay recovered")
	}

	// Every queued edit must have arrived (possibly coalesced into fewer,
	// larger blobs) — and nothing may have been counted as lost.
	require.Equal(t, edits, replayedTextLen(t, relay.syncPayloads("r")),
		"the outbound backlog queued at Shutdown must be published, not discarded")
	require.Zero(t, srv.RelayStats().Dropped,
		"nothing was lost, so Dropped must stay zero")
}

// THE #202 ACCOUNTING GATE: when the relay never recovers and Shutdown's ctx
// expires, the queued backlog is undeliverable — and then it MUST be counted.
// The invariant from the issue: no path may exist where updates vanish while
// RelayStats().Dropped and HardDrops both read zero. Before the fix, the
// worker's ctx.Done() case returned without draining and publishRelay's
// failure path only logged, so exactly that silent-zero path existed.
func TestRelayShutdown_CountsBacklogWhenRelayNeverRecovers(t *testing.T) {
	relay := newShutdownProbeRelay() // release is never closed
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	conn := dial(t, ts, "r")
	drainHandshake(t, conn, crdt.New())

	for i := 0; i < 50; i++ {
		require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "a"), nil))
	}
	require.Eventually(t, func() bool { return relay.inFlight.Load() == 1 },
		2*time.Second, 5*time.Millisecond,
		"the lane worker must be wedged inside Publish before Shutdown")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()

	// While Shutdown is in flight, inbound delivery must already be refused:
	// shutdownCh closes as Shutdown's first act, so delaying the relay-context
	// cancellation (part of the #202 fix) cannot let remote changes mutate
	// rooms during the shutdown window.
	require.Eventually(t, func() bool {
		err := srv.Inject(context.Background(), cluster.Inbound{
			Room: "r", Kind: cluster.KindSync, Data: syncUpdate(t, "x"),
		})
		return err == ygws.ErrServerShutdown
	}, 2*time.Second, 5*time.Millisecond,
		"Inject during an in-flight Shutdown must refuse with ErrServerShutdown")

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"a Shutdown that could not deliver within its ctx must say so")
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return by its own deadline")
	}

	// The backlog could not be delivered — so its loss must become visible.
	// (The worker counts asynchronously as the cancelled Publish unwinds,
	// hence Eventually rather than an immediate read.)
	require.Eventually(t, func() bool {
		st := srv.RelayStats()
		return st.Dropped > 0
	}, 2*time.Second, 10*time.Millisecond,
		"an undeliverable shutdown backlog must be counted in RelayStats().Dropped — silent loss is the #202 bug")
	require.Empty(t, relay.syncPayloads("r"),
		"sanity: nothing can have been published, the relay never recovered")
}

// THE #202 OWNERSHIP GATE: no relay.Publish call may still be in flight after
// Shutdown returns within its ctx budget — otherwise the documented rule "the
// caller must Close() the relay once every attached server is done with it"
// is unsafe for relays that free resources in Close. Before the fix nothing
// joined the lane workers, so a Publish that does not unwind instantly (here:
// one mid-"network write" for 150ms) was still running when Shutdown returned.
func TestRelayShutdown_JoinsInFlightPublish(t *testing.T) {
	relay := newShutdownProbeRelay()
	relay.ignoreCtxFor = 150 * time.Millisecond
	srv := ygws.NewServer()
	require.NoError(t, srv.AttachRelay(relay))
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		ts.Close()
	})

	conn := dial(t, ts, "r")
	drainHandshake(t, conn, crdt.New())
	require.NoError(t, crdt.ApplyUpdateV1(srv.GetDoc("r"), syncUpdate(t, "a"), nil))
	require.Eventually(t, func() bool { return relay.inFlight.Load() == 1 },
		2*time.Second, 5*time.Millisecond,
		"a Publish call must be in flight when Shutdown starts")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, srv.Shutdown(ctx))

	require.Zero(t, relay.inFlight.Load(),
		"no Publish may outlive a Shutdown that completed within its ctx — Shutdown must join the lane workers")
}
