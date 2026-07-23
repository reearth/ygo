package websocket

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// --- fake clock -----------------------------------------------------------

type fakeTimer struct {
	c   chan time.Time
	dur time.Duration
}

func (t *fakeTimer) ch() <-chan time.Time { return t.c }
func (t *fakeTimer) stop()                {}
func (t *fakeTimer) fire()                { t.c <- time.Time{} }

type fakeClock struct {
	created chan *fakeTimer // buffered; one send per newTimer call

	mu      sync.Mutex
	pending []*fakeTimer // non-matching timers set aside by nextTimerOfDur
}

func newFakeClock() *fakeClock { return &fakeClock{created: make(chan *fakeTimer, 256)} }

func (f *fakeClock) newTimer(d time.Duration) wsTimer {
	t := &fakeTimer{c: make(chan time.Time, 1), dur: d}
	f.created <- t
	return t
}

// nextTimerOfDur returns the next-created timer of duration d. Receiving a timer
// from created proves the worker processed the update that armed it
// (happens-before), so the batch state is known. Timers of other durations seen
// along the way are set aside (not discarded) so an interleaved, singly-armed
// timer — e.g. the maxWait timer among a run of window timers — can still be
// retrieved by a later call.
func (f *fakeClock) nextTimerOfDur(t *testing.T, d time.Duration) *fakeTimer {
	t.Helper()
	// Reuse a matching timer set aside by an earlier call first.
	f.mu.Lock()
	for i, ft := range f.pending {
		if ft.dur == d {
			f.pending = append(f.pending[:i], f.pending[i+1:]...)
			f.mu.Unlock()
			return ft
		}
	}
	f.mu.Unlock()
	for {
		select {
		case ft := <-f.created:
			if ft.dur == d {
				return ft
			}
			f.mu.Lock()
			f.pending = append(f.pending, ft)
			f.mu.Unlock()
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for timer of duration %v", d)
			return nil
		}
	}
}

// --- recording adapter ----------------------------------------------------

type recordAdapter struct {
	mu      sync.Mutex
	stores  [][]byte
	ctxErrs []error // ctx.Err() observed at each StoreUpdateContext call
}

func (a *recordAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }
func (a *recordAdapter) StoreUpdate(_ string, u []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stores = append(a.stores, append([]byte(nil), u...))
	a.ctxErrs = append(a.ctxErrs, nil)
	return nil
}
func (a *recordAdapter) StoreUpdateContext(ctx context.Context, _ string, u []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stores = append(a.stores, append([]byte(nil), u...))
	a.ctxErrs = append(a.ctxErrs, ctx.Err())
	return nil
}
func (a *recordAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.stores)
}

// applyEdit commits one transaction to the room doc, producing exactly one
// OnUpdate → one entry on persistCh. Text handle resolved OUTSIDE Transact
// (GetText inside Transact deadlocks).
func applyEdit(r *room, s string, at int) {
	txt := r.doc.GetText("t")
	r.doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, at, s, nil) })
}

func newCoalesceServer(a PersistenceAdapter, fc *fakeClock, window, maxWait time.Duration) *Server {
	s := NewServerWithPersistence(a)
	s.clock = fc
	s.PersistCoalesceWindow = window
	s.PersistCoalesceMaxWait = maxWait
	return s
}

// Burst of N edits within one window collapses to a single merged store whose
// state equals the sequential apply.
func TestPersistCoalesce_BurstMergesToOneStore(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	const n = 5
	for i := 0; i < n; i++ {
		applyEdit(r, "x", 0)
	}
	// Each edit arms one window timer (create or recreate). Receiving the n-th
	// proves all n updates were batched. Fire it to flush.
	var last *fakeTimer
	for i := 0; i < n; i++ {
		last = fc.nextTimerOfDur(t, 50*time.Millisecond)
	}
	last.fire()

	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond,
		"burst should produce exactly one store")

	// Merged store must decode to the same 5 inserted chars.
	got := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(got, a.stores[0], nil))
	assert.Equal(t, n, got.GetText("t").Len())
}

// Disabled (window < 0) preserves strict per-update behaviour: no timers, one
// store per edit.
func TestPersistCoalesce_DisabledIsPerUpdate(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, -1, 0)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		applyEdit(r, "y", 0)
	}
	assert.Eventually(t, func() bool { return a.count() == 3 }, time.Second, 5*time.Millisecond,
		"disabled coalescing should store once per update")
	assert.Empty(t, fc.created, "disabled path must not create timers")
}

// maxWait flushes a batch even while the window keeps being reset.
func TestPersistCoalesce_MaxWaitFlushes(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	const n = 4
	for i := 0; i < n; i++ {
		applyEdit(r, "z", 0)
	}
	for i := 0; i < n; i++ {
		fc.nextTimerOfDur(t, 50*time.Millisecond) // drain window arms (do not fire)
	}
	maxT := fc.nextTimerOfDur(t, 500*time.Millisecond)
	maxT.fire()

	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond,
		"maxWait should flush the whole batch once")
}

// Shutdown mid-batch flushes with a NON-cancelled ctx (guards the critical fix).
func TestPersistCoalesce_ShutdownFlushesWithLiveCtx(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	const n = 3
	for i := 0; i < n; i++ {
		applyEdit(r, "w", 0)
	}
	for i := 0; i < n; i++ {
		fc.nextTimerOfDur(t, 50*time.Millisecond) // ensure all batched, don't fire
	}
	require.NoError(t, s.Shutdown(context.Background()))

	require.Equal(t, 1, a.count(), "shutdown should flush the pending batch exactly once")
	a.mu.Lock()
	defer a.mu.Unlock()
	assert.NoError(t, a.ctxErrs[0], "shutdown flush must use a non-cancelled context")
}

// failFirstAdapter fails the FIRST StoreUpdateContext call and succeeds on every
// call after, recording each attempted update. Models a transient store failure
// (or the ctx-cancelled-at-shutdown case) so the worker's retain-and-reflush path
// can be exercised deterministically.
type failFirstAdapter struct {
	mu     sync.Mutex
	calls  [][]byte
	failed bool
}

func (a *failFirstAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }
func (a *failFirstAdapter) StoreUpdate(_ string, u []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, append([]byte(nil), u...))
	return nil
}
func (a *failFirstAdapter) StoreUpdateContext(_ context.Context, _ string, u []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, append([]byte(nil), u...))
	if !a.failed {
		a.failed = true
		return errors.New("transient store failure")
	}
	return nil
}
func (a *failFirstAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// A flush that does not fully persist (transient error, or ctx cancelled by a
// shutdown that races the timer) must RETAIN the batch so it is re-flushed — not
// silently dropped. Guards the shutdown-handoff durability fix.
func TestPersistCoalesce_RetainsBatchOnFailedFlush(t *testing.T) {
	a := &failFirstAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	const n = 3
	for i := 0; i < n; i++ {
		applyEdit(r, "r", 0)
	}
	var last *fakeTimer
	for i := 0; i < n; i++ {
		last = fc.nextTimerOfDur(t, 50*time.Millisecond)
	}
	last.fire() // window flush attempts one merged store — which fails.

	// Observing the first (failing) store call proves the timer flush ran; the
	// batch must have been retained (flush returned false), not niled.
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond,
		"first flush should attempt exactly one merged store (which fails)")

	require.NoError(t, s.Shutdown(context.Background()))

	// Shutdown re-flushes the retained batch with a live (background) ctx: a
	// SECOND store call. If the batch had been dropped, count would stay at 1.
	require.Equal(t, 2, a.count(), "retained batch must be re-flushed on shutdown (no data loss)")

	// The re-flushed update decodes to the full n-char state — nothing lost.
	a.mu.Lock()
	reflushed := a.calls[len(a.calls)-1]
	a.mu.Unlock()
	got := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(got, reflushed, nil))
	assert.Equal(t, n, got.GetText("t").Len())
}

// compactAdapter is a recordAdapter that also implements CompactableAdapter.
type compactAdapter struct {
	recordAdapter
	mu       sync.Mutex
	compacts []string // room per Compact call
}

func (a *compactAdapter) Compact(_ context.Context, room string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.compacts = append(a.compacts, room)
	return nil
}
func (a *compactAdapter) compactCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.compacts)
}

// CompactEvery=N → Compact after every N non-empty window flushes.
func TestPersistCompact_CountTrigger(t *testing.T) {
	a := &compactAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	s.CompactEvery = 2
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	// 2 separate flushes: edit → window fire → edit → window fire.
	for i := 0; i < 2; i++ {
		applyEdit(r, "x", 0)
		fc.nextTimerOfDur(t, 50*time.Millisecond).fire()
		// wait for the store from this flush to land
		want := i + 1
		assert.Eventually(t, func() bool { return a.count() == want }, time.Second, 5*time.Millisecond)
	}
	assert.Eventually(t, func() bool { return a.compactCount() == 1 }, time.Second, 5*time.Millisecond,
		"Compact should fire once after 2 flushes")
}

// CompactEvery=0 → no count-based Compact (only on unload).
func TestPersistCompact_ZeroNoCountTrigger(t *testing.T) {
	a := &compactAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond) // CompactEvery=0
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)
	applyEdit(r, "x", 0)
	fc.nextTimerOfDur(t, 50*time.Millisecond).fire()
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond)
	// Give any erroneous compaction a chance, then assert none.
	assert.Never(t, func() bool { return a.compactCount() > 0 }, 100*time.Millisecond, 10*time.Millisecond)
}

// On room unload (Shutdown), Compact fires once after the final flush.
func TestPersistCompact_OnUnload(t *testing.T) {
	a := &compactAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)
	applyEdit(r, "x", 0)
	fc.nextTimerOfDur(t, 50*time.Millisecond) // batched, do not fire
	require.NoError(t, s.Shutdown(context.Background()))
	assert.Equal(t, 1, a.compactCount(), "Compact fires once on unload")
	assert.GreaterOrEqual(t, a.count(), 1, "batch flushed before compact")
}

// Adapter without CompactableAdapter → no panic, no calls.
func TestPersistCompact_NonCompactableAdapterSafe(t *testing.T) {
	a := &recordAdapter{} // no Compact method
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	s.CompactEvery = 1
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)
	applyEdit(r, "x", 0)
	fc.nextTimerOfDur(t, 50*time.Millisecond).fire()
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond)
	require.NoError(t, s.Shutdown(context.Background())) // must not panic
}

// flushRoom sends a flush request and waits for the ack (test helper mirroring
// what teardown will do).
func flushRoom(t *testing.T, r *room) {
	t.Helper()
	ack := make(chan struct{})
	select {
	case r.flushReq <- ack:
		select {
		case <-ack:
		case <-time.After(2 * time.Second):
			t.Fatal("flush ack timed out")
		}
	case <-r.persistDone:
		// worker already gone
	case <-time.After(2 * time.Second):
		t.Fatal("flushReq send timed out")
	}
}

// A pending batch (window timer NOT fired) is made durable by flushReq.
func TestPersistFlushReq_MakesPendingBatchDurable(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	const n = 3
	for i := 0; i < n; i++ {
		applyEdit(r, "x", 0)
	}
	for i := 0; i < n; i++ {
		fc.nextTimerOfDur(t, 50*time.Millisecond) // ensure all batched, do NOT fire
	}
	flushRoom(t, r) // force durable flush without firing the timer

	require.Equal(t, 1, a.count(), "flushReq should flush the pending batch exactly once")
	got := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(got, a.stores[0], nil))
	assert.Equal(t, n, got.GetText("t").Len())
}

// flushReq works on the disabled path too.
func TestPersistFlushReq_DisabledPath(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, -1, 0) // disabled
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)
	applyEdit(r, "y", 0)
	assert.Eventually(t, func() bool { return a.count() == 1 }, time.Second, 5*time.Millisecond)
	flushRoom(t, r) // no pending data; must still ack, no deadlock, no extra store
	assert.Equal(t, 1, a.count())
}

// Merge failure falls back to storing each update individually (no loss).
func TestPersistCoalesce_MergeFailureFallsBack(t *testing.T) {
	a := &recordAdapter{}
	fc := newFakeClock()
	s := newCoalesceServer(a, fc, 50*time.Millisecond, 500*time.Millisecond)
	s.mergeFn = func(...[]byte) ([]byte, error) { return nil, errors.New("boom") }
	r, err := s.getOrCreateRoom(context.Background(), "doc1")
	require.NoError(t, err)

	const n = 3
	for i := 0; i < n; i++ {
		applyEdit(r, "q", 0)
	}
	var last *fakeTimer
	for i := 0; i < n; i++ {
		last = fc.nextTimerOfDur(t, 50*time.Millisecond)
	}
	last.fire()

	assert.Eventually(t, func() bool { return a.count() == n }, time.Second, 5*time.Millisecond,
		"merge failure should fall back to individual stores")
}
