package cluster_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// relaySentinel is a package-local origin token a fakeSink stamps on every
// inbound application, exactly as the provider wiring does. The fakeSink's
// doc-observer uses it to drop echoes (never re-publish what came in via the
// relay).
var relaySentinel = new(struct{})

// fakeSink is an in-memory Sink that hosts one room's doc + awareness and
// republishes local (non-sentinel) changes to a relay. It models what the
// provider wiring does, without the WebSocket machinery, so the relay's
// round-trip and echo-guard behaviour can be tested in isolation.
type fakeSink struct {
	room string
	doc  *crdt.Doc
	aw   *awareness.Awareness

	relay cluster.Relay

	injectedSync      atomic.Int64
	injectedAwareness atomic.Int64
	publishedSync     atomic.Int64
	publishedAware    atomic.Int64
}

func newFakeSink(room string, relay cluster.Relay) *fakeSink {
	return newFakeSinkClient(room, relay, 0)
}

func newFakeSinkClient(room string, relay cluster.Relay, clientID uint64) *fakeSink {
	s := &fakeSink{
		room:  room,
		doc:   crdt.New(),
		aw:    awareness.New(clientID),
		relay: relay,
	}
	// Local doc changes (origin != sentinel) get republished; inbound
	// (sentinel) changes are dropped — the echo guard.
	s.doc.OnUpdate(func(update []byte, origin any) {
		if origin == relaySentinel {
			return
		}
		s.publishedSync.Add(1)
		_ = relay.Publish(context.Background(), cluster.Outbound{
			Room: room, Kind: cluster.KindSync, Data: update, Origin: origin,
		})
	})
	s.aw.OnChange(func(ev awareness.ChangeEvent) {
		if ev.Origin == relaySentinel {
			return
		}
		// Re-encode the changed clients and publish.
		s.publishedAware.Add(1)
		_ = relay.Publish(context.Background(), cluster.Outbound{
			Room: room, Kind: cluster.KindAwareness, Data: s.aw.EncodeUpdate(nil), Origin: ev.Origin,
		})
	})
	return s
}

func (s *fakeSink) Inject(_ context.Context, in cluster.Inbound) error {
	switch in.Kind {
	case cluster.KindSync:
		s.injectedSync.Add(1)
		return crdt.ApplyUpdateV1(s.doc, in.Data, relaySentinel)
	case cluster.KindAwareness:
		s.injectedAwareness.Add(1)
		return s.aw.ApplyUpdate(in.Data, relaySentinel)
	}
	return nil
}

func (s *fakeSink) Rooms() []string { return []string{s.room} }
func (s *fakeSink) GetAwareness(room string) (*awareness.Awareness, bool) {
	if room == s.room {
		return s.aw, true
	}
	return nil, false
}
func (s *fakeSink) GetDoc(room string) *crdt.Doc {
	if room == s.room {
		return s.doc
	}
	return nil
}

// eventually polls cond until true or timeout.
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within timeout: %s", msg)
}

func TestMemRelay_SyncRoundTrip_NoEcho(t *testing.T) {
	relay := cluster.NewMemRelay()
	defer func() { require.NoError(t, relay.Close()) }()

	a := newFakeSink("room", relay)
	b := newFakeSink("room", relay)
	require.NoError(t, relay.Start(context.Background(), a))
	require.NoError(t, relay.Start(context.Background(), b))

	// A makes a local edit → publishes once → B injects it.
	txtA := a.doc.GetText("t")
	a.doc.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })

	eventually(t, func() bool { return b.injectedSync.Load() >= 1 }, "B should inject A's sync update")

	// B's doc now reflects the edit.
	txtB := b.doc.GetText("t")
	assert.Equal(t, "hello", txtB.ToString())

	// Echo guard: B injected with the sentinel origin, so B must NOT have
	// republished it. Give any spurious echo a moment to (not) happen.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), a.publishedSync.Load(), "A publishes exactly once")
	assert.Equal(t, int64(0), b.publishedSync.Load(), "B must not echo")
}

func TestMemRelay_AwarenessRoundTrip_NoEcho(t *testing.T) {
	relay := cluster.NewMemRelay()
	defer func() { require.NoError(t, relay.Close()) }()

	// Distinct client IDs so awareness states don't collide on self-protection.
	a := newFakeSinkClient("room", relay, 1)
	b := newFakeSinkClient("room", relay, 2)
	require.NoError(t, relay.Start(context.Background(), a))
	require.NoError(t, relay.Start(context.Background(), b))

	// A sets local presence → publishes → B injects.
	a.aw.SetLocalState(map[string]any{"cursor": 5})

	eventually(t, func() bool { return b.injectedAwareness.Load() >= 1 }, "B should inject A's awareness")

	states := b.aw.GetStates()
	cs, ok := states[1]
	require.True(t, ok, "B should know client 1's state")
	assert.EqualValues(t, 5, cs.State["cursor"])

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(1), a.publishedAware.Load(), "A publishes awareness once")
	assert.Equal(t, int64(0), b.publishedAware.Load(), "B must not echo awareness")
}

func TestMemRelay_PublishBeforeStart(t *testing.T) {
	relay := cluster.NewMemRelay()
	defer func() { _ = relay.Close() }()
	err := relay.Publish(context.Background(), cluster.Outbound{Room: "r", Kind: cluster.KindSync, Data: []byte{0}})
	assert.ErrorIs(t, err, cluster.ErrRelayNotStarted)
}

func TestMemRelay_PublishAfterClose(t *testing.T) {
	relay := cluster.NewMemRelay()
	a := newFakeSink("room", relay)
	require.NoError(t, relay.Start(context.Background(), a))
	require.NoError(t, relay.Close())
	err := relay.Publish(context.Background(), cluster.Outbound{Room: "room", Kind: cluster.KindSync, Data: []byte{0}})
	assert.ErrorIs(t, err, cluster.ErrRelayClosed)
}

func TestMemRelay_ConcurrentPublishInject_Race(t *testing.T) {
	relay := cluster.NewMemRelay(cluster.WithBufferSize(1024))
	defer func() { require.NoError(t, relay.Close()) }()

	const nodes = 4
	sinks := make([]*fakeSink, nodes)
	for i := range sinks {
		sinks[i] = newFakeSink("room", relay)
		require.NoError(t, relay.Start(context.Background(), sinks[i]))
	}

	var wg sync.WaitGroup
	for i := 0; i < nodes; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s := sinks[idx]
			txt := s.doc.GetText("t")
			for j := 0; j < 50; j++ {
				s.doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
			}
		}(i)
	}
	wg.Wait()

	// Let deliveries drain.
	eventually(t, func() bool {
		total := int64(0)
		for _, s := range sinks {
			total += s.injectedSync.Load()
		}
		// Each node's 50 edits delivered to the other 3 nodes = 4*50*3 = 600.
		return total >= int64(nodes*50*(nodes-1))
	}, "all sync updates should be delivered")
}
