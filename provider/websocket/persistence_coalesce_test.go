package websocket

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
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
	ack := make(chan bool, 1)
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

// --- flush-before-evict teardown (Task 4 / #175 follow-up) ------------------
//
// These tests exercise the real disconnect/reconnect path end-to-end through an
// httptest server so the teardown reorder in handleDisconnect + CloseRoom is
// covered against a live worker. They live in the internal package, so the
// external dial/handshake helpers in server_test.go are not reachable — the
// minimal local equivalents below dial a gws client, run the sync handshake,
// and apply received sync frames into a local *crdt.Doc.

// dialWS opens a WebSocket connection to the test server for the given room.
func dialWS(t *testing.T, ts *httptest.Server, room string) *gws.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room
	conn, _, err := gws.DefaultDialer.Dial(u, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// drainWS reads the three handshake frames the server always sends on connect
// (sync step-1, sync step-2, awareness) and applies any sync frames into doc.
// A fixed read count is used because gorilla's reader is permanently broken by a
// deadline expiry; the server contract is exactly three frames.
func drainWS(t *testing.T, conn *gws.Conn, doc *crdt.Doc) {
	t.Helper()
	for i := 0; i < 3; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, data, err := conn.ReadMessage()
		_ = conn.SetReadDeadline(time.Time{})
		require.NoError(t, err)
		dec := encoding.NewDecoder(data)
		outer, err := dec.ReadVarUint()
		require.NoError(t, err)
		if outer == msgSync {
			if _, err := ygsync.ApplySyncMessage(doc, dec.RemainingBytes(), nil); err != nil {
				t.Fatalf("apply sync frame: %v", err)
			}
		}
	}
}

// sendV1Update frames a full V1 update as an outer msgSync + inner MsgUpdate
// (VarBytes-wrapped) and sends it to the server.
func sendV1Update(t *testing.T, conn *gws.Conn, update []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(msgSync)
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))
}

// blockingAdapter blocks StoreUpdate until released, so a flush is in-flight
// during the reconnect window. close(blocked) signals the first store attempt.
type blockingAdapter struct {
	mu      sync.Mutex
	docs    map[string][]byte
	release chan struct{}
	blocked chan struct{}
	once    sync.Once
}

func newBlockingAdapter() *blockingAdapter {
	return &blockingAdapter{docs: map[string][]byte{}, release: make(chan struct{}), blocked: make(chan struct{})}
}

func (a *blockingAdapter) LoadDoc(room string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.docs[room], nil
}

func (a *blockingAdapter) StoreUpdate(room string, update []byte) error {
	a.once.Do(func() { close(a.blocked) })
	<-a.release // block until the test releases
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.docs[room]) == 0 {
		a.docs[room] = append([]byte(nil), update...)
	} else {
		merged, err := crdt.MergeUpdatesV1(a.docs[room], update)
		if err != nil {
			return err
		}
		a.docs[room] = merged
	}
	return nil
}

// A peer edits then disconnects; while the flush is blocked, a reconnect must
// NOT lose the edit (it finds the still-warm room, or reads a durable store).
func TestPersistTeardown_NoLossOnQuickRefresh(t *testing.T) {
	a := newBlockingAdapter()
	// Real server (real clock) so the disconnect/reconnect path runs end-to-end.
	s := NewServerWithPersistence(a)
	s.PersistCoalesceWindow = 5 * time.Second // long window: edit stays in batch
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Peer A edits.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	// Disconnect A -> teardown starts flush (blocks in StoreUpdate).
	_ = connA.Close()
	<-a.blocked // flush is now in-flight and blocked

	// Reconnect (fresh peer) for the same room while the flush is blocked.
	go func() { time.Sleep(50 * time.Millisecond); close(a.release) }()
	docB := crdt.New(crdt.WithClientID(2))
	connB := dialWS(t, ts, "room")
	drainWS(t, connB, docB)

	assert.Equal(t, "hello", docB.GetText("t").ToString(),
		"reconnect must see the edit — no loss on quick refresh")
}

// A peer rejoining during the flush keeps the room alive (no reload, no unload
// hook). Uses the blocking adapter to hold the flush open across the rejoin.
func TestPersistTeardown_RejoinDuringFlushKeepsRoom(t *testing.T) {
	a := newBlockingAdapter()
	s := NewServerWithPersistence(a)
	s.PersistCoalesceWindow = 5 * time.Second
	var unloaded int32
	s.OnUnloadDocument = func(_ context.Context, _ string) {
		atomic.AddInt32(&unloaded, 1)
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hi", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	_ = connA.Close()
	<-a.blocked
	// Rejoin while flush blocked, then release.
	docB := crdt.New(crdt.WithClientID(2))
	connB := dialWS(t, ts, "room")
	go func() { time.Sleep(50 * time.Millisecond); close(a.release) }()
	drainWS(t, connB, docB)

	assert.Equal(t, "hi", docB.GetText("t").ToString())
	assert.Equal(t, int32(0), atomic.LoadInt32(&unloaded), "room must not unload when a peer rejoined")
}

// alwaysFailAdapter fails every StoreUpdate, modelling a persistent backing-store
// outage. LoadDoc returns empty so that IF the room were (wrongly) evicted, a
// reconnect would reload an empty doc — surfacing the lost edit. Implements only
// StoreUpdate (not the Context variant) so both worker paths call it directly.
type alwaysFailAdapter struct {
	mu    sync.Mutex
	calls int
}

func (a *alwaysFailAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }
func (a *alwaysFailAdapter) StoreUpdate(_ string, _ []byte) error {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return errors.New("backing store down")
}
func (a *alwaysFailAdapter) storeCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// When the last peer disconnects and the pending flush FAILS, eviction MUST be
// aborted: the room stays in s.rooms with its worker alive so the retained batch
// is retried on the next teardown, and a reconnect meanwhile finds the warm
// in-memory doc rather than reloading a stale (empty) store. The flushReq ack
// must therefore be a real durability barrier, not a signal that fires
// regardless of flush success. Asserts OnUnloadDocument did NOT fire and the
// reconnect still sees the edit. (#175 follow-up, comment 1.)
func TestPersistTeardown_FlushFailureAbortsEviction(t *testing.T) {
	a := &alwaysFailAdapter{}
	s := NewServerWithPersistence(a)
	s.PersistCoalesceWindow = 5 * time.Second // long window: edit stays batched
	var unloaded int32
	s.OnUnloadDocument = func(_ context.Context, _ string) {
		atomic.AddInt32(&unloaded, 1)
	}
	// OnLastPeer fires in BOTH the evict and the flush-failed-abort paths, after
	// the eviction decision is made and (if evicted) persistDone drained. Use it
	// to synchronise: once signalled, the teardown has finished deciding, so a
	// reconnect deterministically observes the post-decision room state — no
	// sleeps. Non-blocking send so a later 1→0 (connB during Shutdown) can't stall
	// the hook goroutine.
	lastPeer := make(chan struct{}, 1)
	s.OnLastPeer = func(_ context.Context, _ string) {
		select {
		case lastPeer <- struct{}{}:
		default:
		}
	}
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Peer A edits, then disconnects. The pending batch flush on teardown fails.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dialWS(t, ts, "room")
	drainWS(t, connA, docA)
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	sendV1Update(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	_ = connA.Close()
	<-lastPeer // teardown reached its eviction decision (and drained if it evicted)

	// The flush must have been attempted (and failed) at least once.
	assert.GreaterOrEqual(t, a.storeCalls(), 1, "teardown must attempt the flush")

	// Reconnect for the same room. With the fix the warm room survived, so this
	// finds the live in-memory doc carrying the edit. Without the fix the room was
	// evicted, a fresh room reloads the empty store, and the edit is lost.
	docB := crdt.New(crdt.WithClientID(2))
	connB := dialWS(t, ts, "room")
	drainWS(t, connB, docB)

	assert.Equal(t, "hello", docB.GetText("t").ToString(),
		"reconnect must see the edit from the warm in-memory doc — flush failure must not evict")
	assert.Equal(t, int32(0), atomic.LoadInt32(&unloaded),
		"OnUnloadDocument must NOT fire when the flush failed (room not evicted)")

	require.NoError(t, s.Shutdown(context.Background()))
}

// discoverabilityProbeAdapter records, at the moment StoreUpdate runs, whether
// the room is still present in the server map (discoverable via GetDoc). It
// implements only StoreUpdate (not the Context variant) so the worker calls
// this method directly. GetDoc takes s.rmu.RLock; the flush send/ack in the
// teardown paths never holds rmu, so no deadlock.
type discoverabilityProbeAdapter struct {
	mu          sync.Mutex
	s           *Server
	room        string
	stores      [][]byte
	liveAtStore bool
}

func (a *discoverabilityProbeAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }
func (a *discoverabilityProbeAdapter) StoreUpdate(_ string, u []byte) error {
	live := a.s != nil && a.s.GetDoc(a.room) != nil
	a.mu.Lock()
	defer a.mu.Unlock()
	if live {
		a.liveAtStore = true
	}
	a.stores = append(a.stores, append([]byte(nil), u...))
	return nil
}

// CloseRoom must flush the pending coalesced batch WHILE the room is still
// discoverable in s.rooms (via flushReq), not after evicting it. If the flush
// happened after the delete, a racing reconnect would create a fresh room and
// read a stale store. We probe the ordering directly: the store must observe
// the room still present. Also asserts the flushed bytes carry the edit.
func TestPersistTeardown_CloseRoomFlushesBeforeEvict(t *testing.T) {
	a := &discoverabilityProbeAdapter{room: "room"}
	s := NewServerWithPersistence(a)
	a.s = s
	s.PersistCoalesceWindow = 5 * time.Second
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Server-side edit via Apply auto-creates a peerless room and batches the
	// update behind the 5s window (never flushed by a timer within the test).
	require.NoError(t, s.Apply(context.Background(), "room",
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "persisted", nil) })
		}))

	require.NoError(t, s.CloseRoom("room", false))

	a.mu.Lock()
	defer a.mu.Unlock()
	require.NotEmpty(t, a.stores, "CloseRoom must flush the pending batch before evicting")
	assert.True(t, a.liveAtStore,
		"the flush must happen while the room is still discoverable (before delete)")
	got := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(got, a.stores[len(a.stores)-1], nil))
	assert.Equal(t, "persisted", got.GetText("t").ToString())
}
