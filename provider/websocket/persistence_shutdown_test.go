package websocket_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
// counterpart of the deterministic gate above: many goroutines commit while
// Shutdown runs. It cannot prove the absence of loss on its own (which is why
// the deterministic test exists), but under -race it exercises the producer /
// worker handoff for interleavings a scripted test cannot reach, and it
// asserts Shutdown still terminates.
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
					txn.GetText("t").Insert(txn, 0, "x", nil)
				})
			}))
			doc := s.GetDoc("room")
			require.NotNil(t, doc)

			const writers = 8
			const perWriter = 25
			var wg sync.WaitGroup
			var mu sync.Mutex
			start := make(chan struct{})
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for i := 0; i < perWriter; i++ {
						mu.Lock()
						doc.Transact(func(txn *crdt.Transaction) {
							txt := txn.GetText("t")
							txt.Insert(txn, txt.Len(), "y", nil)
						})
						mu.Unlock()
					}
				}()
			}

			close(start)
			sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			require.NoError(t, s.Shutdown(sctx))
			wg.Wait()

			a.mu.Lock()
			stored := len(a.updates)
			a.mu.Unlock()
			require.NotZero(t, stored)
		})
	}
}
