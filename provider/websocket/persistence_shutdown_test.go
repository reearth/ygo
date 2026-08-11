package websocket_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// gateAdapter is a PersistenceAdapter + CompactableAdapter whose Compact call
// can be held open by the test. Compact is the seam that makes the #229 race
// deterministic: the persistence worker calls it on its exit path AFTER its
// final drain of r.persistCh and immediately BEFORE returning, so a test that
// commits a transaction while Compact is parked is guaranteed to hit the exact
// window the bug is about — an update handed to a channel whose reader has
// already swept it for the last time.
type gateAdapter struct {
	mu      sync.Mutex
	updates [][]byte

	compactEntered chan struct{}
	releaseCompact chan struct{}
	compactOnce    sync.Once
}

func newGateAdapter() *gateAdapter {
	return &gateAdapter{
		compactEntered: make(chan struct{}),
		releaseCompact: make(chan struct{}),
	}
}

func (a *gateAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }

func (a *gateAdapter) StoreUpdate(_ string, update []byte) error {
	cp := append([]byte(nil), update...)
	a.mu.Lock()
	a.updates = append(a.updates, cp)
	a.mu.Unlock()
	return nil
}

func (a *gateAdapter) Compact(context.Context, string) error {
	a.compactOnce.Do(func() {
		close(a.compactEntered)
		<-a.releaseCompact
	})
	return nil
}

// merged returns every stored update applied to a fresh doc, so assertions are
// on CONTENT rather than on the adapter's storage shape.
func (a *gateAdapter) text(t *testing.T, key string) string {
	t.Helper()
	a.mu.Lock()
	all := make([][]byte, len(a.updates))
	copy(all, a.updates)
	a.mu.Unlock()

	d := crdt.New()
	for _, u := range all {
		require.NoError(t, crdt.ApplyUpdateV1(d, u, nil))
	}
	return d.GetText(key).ToString()
}

// strandedGateAdapter parks in Compact (like gateAdapter) and then parks again
// in the FIRST StoreUpdate that arrives after Compact was released — which, by
// construction, is a stranded write performed by a committing goroutine rather
// than by the worker. That second park is what lets a test ask the question
// that matters to a caller: is Server.Shutdown still running while that write
// is in flight, or has it already returned?
type strandedGateAdapter struct {
	mu      sync.Mutex
	updates [][]byte

	compactEntered chan struct{}
	releaseCompact chan struct{}
	compactOnce    sync.Once

	armed        atomic.Bool
	storeEntered chan struct{}
	releaseStore chan struct{}
	storeOnce    sync.Once
}

func newStrandedGateAdapter() *strandedGateAdapter {
	return &strandedGateAdapter{
		compactEntered: make(chan struct{}),
		releaseCompact: make(chan struct{}),
		storeEntered:   make(chan struct{}),
		releaseStore:   make(chan struct{}),
	}
}

func (a *strandedGateAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }

func (a *strandedGateAdapter) StoreUpdate(_ string, update []byte) error {
	if a.armed.Load() {
		a.storeOnce.Do(func() {
			close(a.storeEntered)
			<-a.releaseStore
		})
	}
	cp := append([]byte(nil), update...)
	a.mu.Lock()
	a.updates = append(a.updates, cp)
	a.mu.Unlock()
	return nil
}

func (a *strandedGateAdapter) Compact(context.Context, string) error {
	a.compactOnce.Do(func() {
		close(a.compactEntered)
		<-a.releaseCompact
		a.armed.Store(true)
	})
	return nil
}

func (a *strandedGateAdapter) text(t *testing.T, key string) string {
	t.Helper()
	a.mu.Lock()
	all := make([][]byte, len(a.updates))
	copy(all, a.updates)
	a.mu.Unlock()

	d := crdt.New()
	for _, u := range all {
		require.NoError(t, crdt.ApplyUpdateV1(d, u, nil))
	}
	return d.GetText(key).ToString()
}

// awaitStrandedWriter blocks until at least one committing goroutine has
// registered in the stranded-write path. Tests use it to order a commit ahead
// of the worker's exit so they assert against Shutdown's join rather than
// against the residual window the join cannot cover.
func awaitStrandedWriter(t *testing.T, s *ygws.Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ygws.StrandedWritesInFlight(s) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no committing goroutine ever registered as a stranded writer")
}

// TestPersistenceShutdown_JoinsStrandedWrites asserts the property a caller can
// actually rely on, which is stronger than "the write eventually happens":
// Server.Shutdown must not return while a transaction committed during that
// same Shutdown is still being written to the adapter.
//
// This distinction is not academic. The real producers are peer read loops,
// which have no join point at all — nobody can wait for them on the caller's
// behalf. The standard deployment shape is `srv.Shutdown(ctx)` and then return
// from main; if Shutdown returns with a write in flight, the process exits and
// the transaction is lost exactly as before, just through a narrower window.
//
// Deterministic in the GREEN direction: Shutdown provably cannot return while
// the adapter is parked inside the stranded StoreUpdate. The RED direction uses
// a bounded observation window — a Shutdown that does not join has nothing left
// to do and returns immediately (with no relay attached it performs no further
// work at all), so the window only has to be long enough to schedule it.
func TestPersistenceShutdown_JoinsStrandedWrites(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coalesce time.Duration
	}{
		{"coalescing enabled (default)", 0},
		{"coalescing disabled", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newStrandedGateAdapter()
			s := ygws.NewServerWithPersistence(a)
			s.PersistCoalesceWindow = tc.coalesce

			ctx := context.Background()
			require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
				transact(func(txn *crdt.Transaction) {
					txn.GetText("t").Insert(txn, 0, "before", nil)
				})
			}))

			doc := s.GetDoc("room")
			require.NotNil(t, doc)

			shutdownDone := make(chan error, 1)
			go func() {
				sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				shutdownDone <- s.Shutdown(sctx)
			}()

			<-a.compactEntered // worker is past its final drain

			committed := make(chan struct{})
			go func() {
				defer close(committed)
				doc.Transact(func(txn *crdt.Transaction) {
					txt := txn.GetText("t")
					txt.Insert(txn, txt.Len(), "-during", nil)
				})
			}()

			// Sequence the commit ahead of the worker's exit. Without this the
			// test would be measuring the acknowledged residual (a commit that
			// starts after Shutdown reads the counter) instead of the join.
			// Deterministic once observed: the committer registers before it
			// can even read the retirement latch, and it then parks on a
			// persistDone that cannot close until Compact is released below.
			awaitStrandedWriter(t, s)

			close(a.releaseCompact) // worker exits; the commit strands

			select {
			case <-a.storeEntered:
			case <-time.After(10 * time.Second):
				t.Fatal("the stranded write never reached the adapter")
			}

			// THE assertion: Shutdown is still running. Deliberately does NOT
			// join the committing goroutine first — joining it would only prove
			// the write happens eventually, which no caller can observe.
			select {
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned (%v) while a transaction committed during "+
					"that Shutdown was still in flight to the adapter (#229)", err)
			case <-time.After(500 * time.Millisecond):
			}

			close(a.releaseStore)

			select {
			case err := <-shutdownDone:
				require.NoError(t, err)
			case <-time.After(30 * time.Second):
				t.Fatal("Shutdown did not return after the stranded write completed")
			}
			<-committed

			require.Equal(t, "before-during", a.text(t, "t"))
		})
	}
}

// TestPersistenceShutdown_StrandedWaitIsBoundedByCtx is the anti-hang half of
// the join above, and the case with a producer actually present: a stranded
// write that never completes must cost the caller its deadline and nothing
// more — Shutdown reports ctx.Err() rather than blocking forever.
func TestPersistenceShutdown_StrandedWaitIsBoundedByCtx(t *testing.T) {
	a := newStrandedGateAdapter()
	s := ygws.NewServerWithPersistence(a)

	ctx := context.Background()
	require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) {
			txn.GetText("t").Insert(txn, 0, "before", nil)
		})
	}))
	doc := s.GetDoc("room")
	require.NotNil(t, doc)

	shutdownDone := make(chan error, 1)
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownDone <- s.Shutdown(sctx)
	}()

	<-a.compactEntered
	go func() {
		doc.Transact(func(txn *crdt.Transaction) {
			txt := txn.GetText("t")
			txt.Insert(txn, txt.Len(), "-during", nil)
		})
	}()
	awaitStrandedWriter(t, s)
	close(a.releaseCompact)

	select {
	case <-a.storeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stranded write never reached the adapter")
	}

	// The adapter is wedged. Shutdown must still come back on its deadline.
	select {
	case err := <-shutdownDone:
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"a wedged stranded write must surface as the caller's deadline, not as a lie")
	case <-time.After(20 * time.Second):
		t.Fatal("Shutdown hung on a stranded write that never completes (#229)")
	}
	close(a.releaseStore)
}

// TestPersistenceShutdown_ConcurrentShutdownCallersAllReturn covers the
// stranded-write join under CONCURRENT Shutdown calls. shutdownOnce implies the
// API tolerates them, and it does — but the join must release all of them, not
// just whichever got there first. An earlier implementation woke waiters with a
// single cap-1 token; the winner consumed it and every other caller sat until
// its own context expired, turning a healthy shutdown into a spurious
// DeadlineExceeded for all but one caller.
//
// Sequenced: the stranded write parks inside the adapter, which holds the
// in-flight count above zero, so both callers are waiting on the same latch
// before it is released.
func TestPersistenceShutdown_ConcurrentShutdownCallersAllReturn(t *testing.T) {
	const callers = 4

	a := newStrandedGateAdapter()
	s := ygws.NewServerWithPersistence(a)

	ctx := context.Background()
	require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) {
			txn.GetText("t").Insert(txn, 0, "before", nil)
		})
	}))
	doc := s.GetDoc("room")
	require.NotNil(t, doc)

	results := make(chan error, callers)
	launch := make(chan struct{})
	for i := 0; i < callers; i++ {
		go func() {
			<-launch
			// Deliberately generous relative to the parked write below: a
			// caller that returns DeadlineExceeded here did so because it was
			// never woken, not because the work was slow.
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			results <- s.Shutdown(sctx)
		}()
	}
	close(launch)

	<-a.compactEntered

	committed := make(chan struct{})
	go func() {
		defer close(committed)
		doc.Transact(func(txn *crdt.Transaction) {
			txt := txn.GetText("t")
			txt.Insert(txn, txt.Len(), "-during", nil)
		})
	}()
	awaitStrandedWriter(t, s)

	close(a.releaseCompact)

	select {
	case <-a.storeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the stranded write never reached the adapter")
	}

	// Every caller is now past the persistence join and parked on the
	// stranded-write latch. Give the last of them time to get there, so the
	// release below really does have to wake more than one.
	time.Sleep(200 * time.Millisecond)
	close(a.releaseStore)

	for i := 0; i < callers; i++ {
		select {
		case err := <-results:
			require.NoError(t, err,
				"a concurrent Shutdown caller was never woken by the stranded-write join (#229)")
		case <-time.After(15 * time.Second):
			t.Fatal("a concurrent Shutdown caller never returned")
		}
	}
	<-committed
	require.Equal(t, "before-during", a.text(t, "t"))
}

// TestPersistenceShutdown_CommitDuringShutdownIsNotLost is the #229 regression
// gate. A transaction committed while Shutdown is in flight — after the
// persistence worker's final drain, before Shutdown returns — must reach the
// adapter rather than being parked forever in an unread 256-slot buffer.
//
// Deterministic by construction: the worker is parked inside Compact (its last
// act before returning) when the test commits, so the "after the final sweep"
// ordering is forced, not raced.
func TestPersistenceShutdown_CommitDuringShutdownIsNotLost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coalesce time.Duration
	}{
		{"coalescing enabled (default)", 0},
		{"coalescing disabled", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newGateAdapter()
			s := ygws.NewServerWithPersistence(a)
			s.PersistCoalesceWindow = tc.coalesce

			ctx := context.Background()
			require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
				transact(func(txn *crdt.Transaction) {
					txn.GetText("t").Insert(txn, 0, "before", nil)
				})
			}))

			doc := s.GetDoc("room")
			require.NotNil(t, doc)

			shutdownDone := make(chan error, 1)
			go func() {
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				shutdownDone <- s.Shutdown(sctx)
			}()

			// The worker has drained for the last time and is parked in Compact.
			// Everything after this point is strictly after that final drain.
			<-a.compactEntered

			// Commit while Shutdown is still running. This is the peer-read-loop
			// commit the issue describes, reduced to a deterministic sequence.
			// It runs on its own goroutine because the fixed code hands the
			// update over synchronously and waits for the worker to finish —
			// which cannot happen until Compact is released below.
			committed := make(chan struct{})
			go func() {
				defer close(committed)
				doc.Transact(func(txn *crdt.Transaction) {
					txt := txn.GetText("t")
					txt.Insert(txn, txt.Len(), "-during", nil)
				})
			}()

			close(a.releaseCompact)
			select {
			case <-committed:
			case <-time.After(10 * time.Second):
				t.Fatal("commit during Shutdown never returned")
			}

			select {
			case err := <-shutdownDone:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("Shutdown did not return")
			}

			require.Equal(t, "before-during", a.text(t, "t"),
				"a transaction committed during Shutdown was silently dropped (#229)")
		})
	}
}

// ctxAdapter is context-aware: it honours cancellation the way the
// PersistenceAdapterContext contract asks adapters to. #229's second defect is
// that the strict path handed the worker's CANCELLABLE ctx to the stores it
// issues from the moment shutdown fires onwards — including the final drain —
// so an adapter like this one discarded the whole tail.
//
// The first store call parks on gate (signalling entered), which lets a test
// pile updates up in the room's buffer before Shutdown fires.
type ctxAdapter struct {
	mu        sync.Mutex
	updates   [][]byte
	cancelled int

	gate    chan struct{} // nil to disable the park
	entered chan struct{}
	once    sync.Once
}

func (a *ctxAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }

func (a *ctxAdapter) StoreUpdate(_ string, update []byte) error {
	a.park()
	cp := append([]byte(nil), update...)
	a.mu.Lock()
	a.updates = append(a.updates, cp)
	a.mu.Unlock()
	return nil
}

func (a *ctxAdapter) park() {
	if a.gate == nil {
		return
	}
	a.once.Do(func() {
		close(a.entered)
		<-a.gate
	})
}

func (a *ctxAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	a.park()
	if err := ctx.Err(); err != nil {
		a.mu.Lock()
		a.cancelled++
		a.mu.Unlock()
		return err
	}
	return a.StoreUpdate(room, update)
}

func (a *ctxAdapter) text(t *testing.T, key string) string {
	t.Helper()
	a.mu.Lock()
	all := make([][]byte, len(a.updates))
	copy(all, a.updates)
	a.mu.Unlock()

	d := crdt.New()
	for _, u := range all {
		require.NoError(t, crdt.ApplyUpdateV1(d, u, nil))
	}
	return d.GetText(key).ToString()
}

// awaitShutdownRefusal blocks until s has begun shutting down, observed through
// the public API: Apply refuses with ErrServerShutdown from the instant
// shutdownCh closes, which is Shutdown's first act. Using a throwaway room
// keeps the probe from touching the room under test.
func awaitShutdownRefusal(t *testing.T, s *ygws.Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		err := s.Apply(context.Background(), "shutdown-probe", func(*crdt.Doc, func(func(*crdt.Transaction))) {})
		if errors.Is(err, ygws.ErrServerShutdown) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server never began shutting down")
}

// TestPersistenceShutdown_CtxAwareAdapterKeepsExitDrain covers #229's second
// defect. Updates already queued when Shutdown fires must still reach a
// ctx-aware adapter, on BOTH paths.
//
// Sequenced, not raced: the worker is parked inside its first store call while
// the remaining updates are queued and Shutdown is confirmed to have started,
// so the worker only ever resumes into a world where its ctx is already
// cancelled — the exact condition the defect needs. Which of the 60 queued
// updates the pre-fix worker consumed before taking its shutdown case was a
// coin flip per update, so pre-fix survival of all of them had probability
// 2**-60; the assertion is on content, which is what durability means.
func TestPersistenceShutdown_CtxAwareAdapterKeepsExitDrain(t *testing.T) {
	const queued = 60

	for _, tc := range []struct {
		name     string
		coalesce time.Duration
	}{
		{"coalescing enabled (default)", time.Millisecond},
		{"coalescing disabled", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &ctxAdapter{gate: make(chan struct{}), entered: make(chan struct{})}
			s := ygws.NewServerWithPersistence(a)
			s.PersistCoalesceWindow = tc.coalesce

			ctx := context.Background()
			want := ""
			apply := func(mark string) {
				require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
					transact(func(txn *crdt.Transaction) {
						txt := txn.GetText("t")
						txt.Insert(txn, txt.Len(), mark, nil)
					})
				}))
				want += mark
			}

			apply("a")
			<-a.entered // the worker is parked inside its first store

			for i := 0; i < queued; i++ {
				apply(fmt.Sprintf("%d", i%10))
			}

			shutdownDone := make(chan error, 1)
			go func() {
				sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				shutdownDone <- s.Shutdown(sctx)
			}()
			awaitShutdownRefusal(t, s)

			close(a.gate) // worker resumes with its ctx already cancelled

			select {
			case err := <-shutdownDone:
				require.NoError(t, err)
			case <-time.After(20 * time.Second):
				t.Fatal("Shutdown did not return")
			}

			require.Equal(t, want, a.text(t, "t"),
				"updates queued when Shutdown fired were dropped by ctx cancellation (#229)")
		})
	}
}

// TestPersistenceShutdown_ReturnsPromptlyWithoutProducers is the anti-hang
// gate for #229: the failure mode of a wrong fix is a Shutdown that waits
// forever for a producer that never appears. Rooms with no peers — idle
// resident and Apply-created — must still let their workers exit.
func TestPersistenceShutdown_ReturnsPromptlyWithoutProducers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coalesce time.Duration
		idle     time.Duration
	}{
		{"apply-created, coalescing", 0, 0},
		{"apply-created, strict", -1, 0},
		{"idle-resident, coalescing", 0, time.Hour},
		{"idle-resident, strict", -1, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &ctxAdapter{}
			s := ygws.NewServerWithPersistence(a)
			s.PersistCoalesceWindow = tc.coalesce
			s.RoomIdleTimeout = tc.idle

			ctx := context.Background()
			for _, room := range []string{"a", "b", "c"} {
				require.NoError(t, s.Apply(ctx, room, func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
					transact(func(txn *crdt.Transaction) {
						txn.GetText("t").Insert(txn, 0, room, nil)
					})
				}))
			}

			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			start := time.Now()
			require.NoError(t, s.Shutdown(sctx), "Shutdown hung waiting for a producer that never appears (#229)")
			require.Less(t, time.Since(start), 5*time.Second)
		})
	}
}

// TestPersistenceShutdown_ConcurrentCommitsRaceShutdown is the unsequenced
// counterpart of the deterministic gates above: eight goroutines commit,
// genuinely concurrently, while Shutdown runs. Under -race it exercises the
// producer/worker handoff — and the contention on the fallback's own lock —
// for interleavings a scripted test cannot reach.
//
// The producers are deliberately NOT serialised by a test-side mutex.
// crdt.Doc.Transact is internally locked and the OnUpdate observers fire
// outside that lock, so concurrent committers really are inside the observer
// at once, which is the only way the post-retirement fallback's own
// serialisation gets exercised at all.
//
// It asserts a property that IS sound under this race, rather than "something
// was stored": every commit whose Transact had RETURNED before Shutdown
// returned must be in the adapter. That follows from the design — by the time
// the observer returns, the update has either been buffered while the
// retirement latch was open (so the worker's final drain, which precedes
// persistDone, which precedes Shutdown's own waits, takes it) or been written
// synchronously by the committer. Commits still in flight when Shutdown
// returns are excluded on purpose: that is the acknowledged residual, and a
// test must not assert away a limit the code really has.
//
// Each commit sets a unique YMap key so persisted commits are individually
// identifiable, which a shared YText cannot give.
func TestPersistenceShutdown_ConcurrentCommitsRaceShutdown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coalesce time.Duration
	}{
		{"coalescing enabled (default)", 0},
		{"coalescing disabled", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &ctxAdapter{}
			s := ygws.NewServerWithPersistence(a)
			s.PersistCoalesceWindow = tc.coalesce

			ctx := context.Background()
			require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
				transact(func(txn *crdt.Transaction) {
					txn.GetMap("m").Set(txn, "seed", "1")
				})
			}))
			doc := s.GetDoc("room")
			require.NotNil(t, doc)

			// committed records keys whose Transact has already returned. It is
			// appended to AFTER Transact returns, so a key present in a snapshot
			// of it definitely finished committing before that snapshot.
			var recMu sync.Mutex
			committed := map[string]bool{"seed": true}

			const writers = 8
			const perWriter = 25
			var wg sync.WaitGroup
			start := make(chan struct{})
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for i := 0; i < perWriter; i++ {
						key := fmt.Sprintf("w%d-%d", w, i)
						doc.Transact(func(txn *crdt.Transaction) {
							txn.GetMap("m").Set(txn, key, "1")
						})
						recMu.Lock()
						committed[key] = true
						recMu.Unlock()
					}
				}()
			}

			close(start)
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, s.Shutdown(sctx))

			// Snapshot the instant Shutdown returns: everything in here had
			// already finished committing, so all of it must be durable.
			recMu.Lock()
			mustBeDurable := make([]string, 0, len(committed))
			for k := range committed {
				mustBeDurable = append(mustBeDurable, k)
			}
			recMu.Unlock()

			wg.Wait()

			a.mu.Lock()
			all := make([][]byte, len(a.updates))
			copy(all, a.updates)
			a.mu.Unlock()

			d := crdt.New()
			for _, u := range all {
				require.NoError(t, crdt.ApplyUpdateV1(d, u, nil))
			}
			persisted := make(map[string]bool, len(d.GetMap("m").Keys()))
			for _, k := range d.GetMap("m").Keys() {
				persisted[k] = true
			}
			for _, k := range mustBeDurable {
				require.True(t, persisted[k],
					"commit %q returned before Shutdown returned but never reached the adapter (#229)", k)
			}
			require.NotEmpty(t, mustBeDurable)
		})
	}
}

// TestPersistence_PostTeardownCommitStillPersists characterises a real
// behaviour change #229 makes, so it is not discovered by surprise.
//
// Before #229 the observer's escape hatch was `case <-r.persistStop:`, which
// DROPPED the update; a room that had been torn down therefore never wrote
// again. Nothing calls doc.Destroy on teardown, so a caller that retained the
// *crdt.Doc (from GetDoc, or from an OnLoadDocument hook) keeps the observer
// alive, and its later commits now reach the adapter instead of vanishing.
//
// That is the right behaviour — a committed transaction being silently
// discarded is the bug this issue is about — but it widens the window in which
// StoreUpdate can be called for a room name the server considers gone. See
// CompactableAdapter's godoc for what that means for adapters.
func TestPersistence_PostTeardownCommitStillPersists(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coalesce time.Duration
	}{
		{"coalescing enabled (default)", 0},
		{"coalescing disabled", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &ctxAdapter{}
			s := ygws.NewServerWithPersistence(a)
			s.PersistCoalesceWindow = tc.coalesce

			ctx := context.Background()
			require.NoError(t, s.Apply(ctx, "room", func(_ *crdt.Doc, transact func(func(*crdt.Transaction))) {
				transact(func(txn *crdt.Transaction) {
					txn.GetText("t").Insert(txn, 0, "before", nil)
				})
			}))

			doc := s.GetDoc("room")
			require.NotNil(t, doc)

			require.NoError(t, s.CloseRoom("room", false))
			require.Nil(t, s.GetDoc("room"), "the room is gone as far as the server is concerned")

			doc.Transact(func(txn *crdt.Transaction) {
				txt := txn.GetText("t")
				txt.Insert(txn, txt.Len(), "-after", nil)
			})

			require.Equal(t, "before-after", a.text(t, "t"),
				"a commit on a retained doc after teardown must be persisted, not dropped (#229)")

			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, s.Shutdown(sctx))
		})
	}
}
