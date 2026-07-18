package mobile

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countDrainGoroutines returns the number of live docDrain goroutines by
// scanning all goroutine stacks.
func countDrainGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "mobile.docDrain")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// recUpdate is one recorded OnChange delivery.
type recUpdate struct {
	update []byte
	local  bool
}

// recorder is a DocObserver that pushes each delivery onto a channel. An
// optional delay makes OnChange slow, forcing the bridge to coalesce.
type recorder struct {
	ch    chan recUpdate
	delay time.Duration
}

func newRecorder(buf int) *recorder { return &recorder{ch: make(chan recUpdate, buf)} }

func (r *recorder) OnChange(updateV1 []byte, local bool) {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.ch <- recUpdate{update: append([]byte(nil), updateV1...), local: local}
}

// recv waits up to timeout for one delivery.
func recv(t *testing.T, ch <-chan recUpdate, timeout time.Duration) (recUpdate, bool) {
	t.Helper()
	select {
	case r := <-ch:
		return r, true
	case <-time.After(timeout):
		return recUpdate{}, false
	}
}

// assertNoRecv fails if any delivery arrives within timeout.
func assertNoRecv(t *testing.T, ch <-chan recUpdate, timeout time.Duration) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected OnChange: local=%v len=%d", r.local, len(r.update))
	case <-time.After(timeout):
	}
}

// drainAll reads deliveries until none arrive within quiesce.
func drainAll(ch <-chan recUpdate, quiesce time.Duration) []recUpdate {
	var out []recUpdate
	for {
		select {
		case r := <-ch:
			out = append(out, r)
		case <-time.After(quiesce):
			return out
		}
	}
}

func TestEmptyUpdateV1_IsNonEmptyCanonicalBytes(t *testing.T) {
	if len(emptyUpdateV1) == 0 {
		t.Fatal("emptyUpdateV1 must be the canonical no-op update, not empty")
	}
	// A real edit's update must NOT be classified as empty.
	d := NewDoc()
	rec := newRecorder(4)
	sub := d.Observe(rec)
	defer sub.Close()
	if err := d.InsertText("t", 0, "hi"); err != nil {
		t.Fatal(err)
	}
	r, ok := recv(t, rec.ch, time.Second)
	if !ok {
		t.Fatal("expected a delivery for a real edit")
	}
	if isEmptyUpdateV1(r.update) {
		t.Fatal("a real edit's update was misclassified as the empty update")
	}
}

func TestObserve_LocalEditFires_ReproducesOnPeer(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(4)
	sub := d.Observe(rec)
	defer sub.Close()

	if err := d.InsertText("t", 0, "hello"); err != nil {
		t.Fatal(err)
	}
	r, ok := recv(t, rec.ch, time.Second)
	if !ok {
		t.Fatal("expected OnChange for local edit")
	}
	if !r.local {
		t.Fatal("local edit must report local=true")
	}
	if len(r.update) == 0 {
		t.Fatal("update must be non-empty")
	}

	peer := NewDoc()
	if err := peer.ApplyUpdate(r.update); err != nil {
		t.Fatal(err)
	}
	if got := peer.GetText("t"); got != "hello" {
		t.Fatalf("peer text = %q, want %q", got, "hello")
	}
}

func TestObserve_RemoteApplyFires_LocalFalse(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(4)
	sub := d.Observe(rec)
	defer sub.Close()

	other := NewDoc()
	if err := other.InsertText("t", 0, "world"); err != nil {
		t.Fatal(err)
	}
	upd := other.EncodeStateAsUpdate()
	if err := d.ApplyUpdate(upd); err != nil {
		t.Fatal(err)
	}

	r, ok := recv(t, rec.ch, time.Second)
	if !ok {
		t.Fatal("expected OnChange for remote apply")
	}
	if r.local {
		t.Fatal("remote apply must report local=false")
	}
	if got := d.GetText("t"); got != "world" {
		t.Fatalf("doc text = %q, want %q", got, "world")
	}
}

func TestObserve_NoOpTransactionDoesNotFire(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(4)
	sub := d.Observe(rec)
	defer sub.Close()

	// Inserting empty text is a crdt no-op: OnUpdate still fires with the
	// canonical empty update, but the bridge must drop it.
	if err := d.InsertText("t", 0, ""); err != nil {
		t.Fatal(err)
	}
	assertNoRecv(t, rec.ch, 250*time.Millisecond)
}

func TestObserve_SequentialEdits_ReproduceFinalState(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(64)
	sub := d.Observe(rec)
	defer sub.Close()

	const n = 6
	for i := 0; i < n; i++ {
		if err := d.InsertText("t", 0, "a"); err != nil {
			t.Fatal(err)
		}
		// Read promptly so, with a fast consumer, updates arrive individually
		// and in order.
		if _, ok := recv(t, rec.ch, time.Second); !ok {
			t.Fatalf("missing delivery for edit %d", i)
		}
	}

	// Apply the (possibly individual) stream to a peer and confirm it matches.
	// Re-drive by taking a full-state snapshot check: rebuild a peer from the
	// deltas we captured by re-observing is overkill; instead assert the source
	// itself is the expected length and a full-state peer matches.
	peer := NewDoc()
	if err := peer.ApplyUpdate(d.EncodeStateAsUpdate()); err != nil {
		t.Fatal(err)
	}
	if got := peer.GetText("t"); len(got) != n {
		t.Fatalf("peer text len = %d, want %d", len(got), n)
	}
}

func TestObserve_DeltaStreamReproducesPeer(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(256)
	sub := d.Observe(rec)
	defer sub.Close()

	const n = 20
	for i := 0; i < n; i++ {
		if err := d.InsertText("t", 0, "a"); err != nil {
			t.Fatal(err)
		}
	}

	peer := NewDoc()
	got := drainAll(rec.ch, 500*time.Millisecond)
	if len(got) == 0 {
		t.Fatal("no deliveries")
	}
	for _, r := range got {
		if err := peer.ApplyUpdate(r.update); err != nil {
			t.Fatal(err)
		}
	}
	if peer.GetText("t") != d.GetText("t") {
		t.Fatalf("peer text (len %d) != source (len %d)", len(peer.GetText("t")), len(d.GetText("t")))
	}
}

func TestSubscription_CloseStopsDelivery(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(4)
	sub := d.Observe(rec)

	if err := d.InsertText("t", 0, "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := recv(t, rec.ch, time.Second); !ok {
		t.Fatal("expected delivery before Close")
	}

	sub.Close()
	sub.Close() // idempotent

	if err := d.InsertText("t", 0, "y"); err != nil {
		t.Fatal(err)
	}
	assertNoRecv(t, rec.ch, 250*time.Millisecond)
}

func TestDocClose_StopsDeliveryNoPanic(t *testing.T) {
	d := NewDoc()
	rec := newRecorder(4)
	_ = d.Observe(rec)

	if err := d.InsertText("t", 0, "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := recv(t, rec.ch, time.Second); !ok {
		t.Fatal("expected delivery before Doc.Close")
	}

	d.Close()
	d.Close() // idempotent

	// Edits after Close return ErrClosed and fire nothing.
	if err := d.InsertText("t", 0, "y"); err != ErrClosed {
		t.Fatalf("InsertText after Close: err = %v, want ErrClosed", err)
	}
	assertNoRecv(t, rec.ch, 250*time.Millisecond)
}

func TestObserve_AfterCloseReturnsClosedStub(t *testing.T) {
	d := NewDoc()
	d.Close()

	sub := d.Observe(newRecorder(1))
	if sub == nil {
		t.Fatal("Observe after Close must return a non-nil Subscription")
	}
	sub.Close() // must be a safe no-op, no panic
}

// countingObserver never blocks — safe for the churn test where deliveries may
// go unread.
type countingObserver struct{ n int64 }

func (c *countingObserver) OnChange(updateV1 []byte, local bool) { atomic.AddInt64(&c.n, 1) }

func TestObserve_ConcurrentObserveCloseEdit_Race(t *testing.T) {
	d := NewDoc()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Editors.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = d.InsertText("t", 0, "x")
				}
			}
		}()
	}
	// Observer churn: Observe then immediately Close, repeatedly.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s := d.Observe(&countingObserver{})
					s.Close()
				}
			}
		}()
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	d.Close() // must not panic or deadlock
}

// TestDocClose_NoDrainGoroutineLeak reproduces the Observe||Close TOCTOU leak:
// Observe launches its drain goroutine before registering in m.subs, so a
// Close that snapshots the registry before Observe finishes registering would
// miss it and never signal its drain — leaking the goroutine (and pinning the
// observer). We race many rounds of Observe against Close on fresh docs, never
// calling sub.Close, then assert the docDrain goroutine count returns to
// baseline. This FAILS before the Close-ordering fix and PASSES after.
func TestDocClose_NoDrainGoroutineLeak(t *testing.T) {
	base := countDrainGoroutines()
	const rounds = 3000
	var wg sync.WaitGroup
	for i := 0; i < rounds; i++ {
		d := NewDoc()
		wg.Add(2)
		go func() { defer wg.Done(); _ = d.Observe(&countingObserver{}) }()
		go func() { defer wg.Done(); d.Close() }()
	}
	wg.Wait()

	// Every registered drain is signalled by Close and exits promptly; a leaked
	// drain parks forever on cond.Wait. Poll until the count settles to baseline
	// or the deadline proves a leak.
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		live := countDrainGoroutines() - base
		if live <= 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leaked %d docDrain goroutine(s) after %d Observe||Close rounds", live, rounds)
		}
	}
}

func TestObserve_SlowConsumerCoalesces(t *testing.T) {
	d := NewDoc()
	rec := &recorder{ch: make(chan recUpdate, 512), delay: 10 * time.Millisecond}
	sub := d.Observe(rec)
	defer sub.Close()

	const n = 200
	editsDone := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			_ = d.InsertText("t", 0, "a")
		}
		close(editsDone)
	}()

	// Drain concurrently. The slow OnChange forces the bridge to coalesce many
	// edits into the single pending slot while the drain goroutine is busy.
	<-editsDone
	got := drainAll(rec.ch, 300*time.Millisecond)

	if len(got) == 0 {
		t.Fatal("no deliveries")
	}
	if len(got) >= n {
		t.Fatalf("no coalescing occurred: %d deliveries for %d edits", len(got), n)
	}

	// Every op still arrives: applying the delivered (coalesced) deltas in order
	// to a fresh peer reproduces the full source state.
	peer := NewDoc()
	for _, r := range got {
		if err := peer.ApplyUpdate(r.update); err != nil {
			t.Fatal(err)
		}
	}
	if got := peer.GetText("t"); len(got) != n {
		t.Fatalf("peer text len = %d, want %d (source len %d)", len(got), n, len(d.GetText("t")))
	}
	if peer.GetText("t") != d.GetText("t") {
		t.Fatal("peer state diverged from source after coalesced delivery")
	}
}
