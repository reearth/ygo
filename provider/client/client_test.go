package client

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// countingStore wraps a LocalStore and counts StoreUpdate calls, so tests can
// assert on persistence traffic (specifically: that hydrating a doc from the
// store does not turn around and write the same bytes straight back).
type countingStore struct {
	LocalStore
	storeUpdates atomic.Uint64
}

func (s *countingStore) StoreUpdate(room string, update []byte) error {
	s.storeUpdates.Add(1)
	return s.LocalStore.StoreUpdate(room, update)
}

func (s *countingStore) count() uint64 { return s.storeUpdates.Load() }

// awaitStatus blocks until this Client reports want, or fails the test. Used
// by lifecycle tests that need to know a Connect goroutine has actually
// reached a given point rather than guessing with a sleep.
func awaitStatus(t *testing.T, c *Client, want State) {
	t.Helper()
	hit := make(chan struct{})
	var once sync.Once
	unsub := c.OnStatus(func(s Status) {
		if s.State == want {
			once.Do(func() { close(hit) })
		}
	})
	defer unsub()
	select {
	case <-hit:
	case <-time.After(5 * time.Second):
		t.Fatalf("client never reported state %v", want)
	}
}

// TestClient_PersistsLocalEditMadeBeforeConnect pins down the rule that a
// local edit is durable from the moment it is made, not from the moment
// Connect happens to get around to wiring things up.
//
// This is the failure mode a registration-order-based design has and an
// origin-based one does not: if the Doc observer were only registered part-way
// through Connect, an app that constructs a Client and immediately starts
// editing (entirely reasonable — the whole point of an offline-first client is
// that the Doc is usable straight away) would have every edit made before that
// moment silently absent from the store, and would lose them all on a restart
// that happened before the next successful sync.
//
// The edit is also checked to survive Connect's own hydration, which reads
// back exactly these bytes: hydration must not be re-persisted (the count
// assertion), and must not disturb what is already stored.
func TestClient_PersistsLocalEditMadeBeforeConnect(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "preconnect.db")
	sqlite, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })
	store := &countingStore{LocalStore: sqlite}

	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/preconnect", Doc: doc, Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The edit lands here: after New, before Connect.
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "early", nil) })

	if got := store.count(); got != 1 {
		t.Fatalf("store.count() after a pre-Connect edit = %d, want 1", got)
	}

	// Now run Connect against a server that refuses every connection, and let
	// it get all the way to reporting the failure, so hydration has definitely
	// run by the time the assertions below happen.
	connect(t, c)
	awaitStatus(t, c, StateDisconnected)

	if got := store.count(); got != 1 {
		t.Fatalf("store.count() after hydration = %d, want 1 (hydrated bytes must not be re-persisted)", got)
	}

	// The edit must be reconstructible from the store alone — that is what
	// "durable" means here, and counting writes alone would not prove it.
	blob, err := store.LoadDoc("preconnect")
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if len(blob) == 0 {
		t.Fatal("LoadDoc returned nothing; the pre-Connect edit was never persisted")
	}
	fresh := crdt.New()
	if err := crdt.ApplyUpdateV1(fresh, blob, nil); err != nil {
		t.Fatalf("ApplyUpdateV1: %v", err)
	}
	if got := fresh.GetText("t").ToString(); got != "early" {
		t.Fatalf("rehydrated text = %q, want %q", got, "early")
	}
}

// TestClient_Connect_SecondCallRejected checks the single-use guard. Two
// concurrent Connects on one Client would mean two dial loops, two sockets,
// and two observers on the same Doc — so every local edit stored twice and
// sent twice, and two goroutines racing to be "the" single writer of their
// respective sockets. None of that is recoverable at a lower layer, so it is
// refused outright.
func TestClient_Connect_SecondCallRejected(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	connect(t, c)
	// Wait until the first Connect has actually started its loop, so this
	// test cannot pass by racing ahead of it.
	awaitStatus(t, c, StateDisconnected)

	if err := c.Connect(context.Background()); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("second Connect returned %v, want ErrAlreadyConnected", err)
	}

	// Still refused after the first Connect has been stopped: a Client is
	// single-use, and a caller wanting a fresh connection constructs a fresh
	// Client rather than reusing one whose Close already fired.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Connect(context.Background()); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("Connect after Close returned %v, want ErrAlreadyConnected", err)
	}
}

// TestClient_Connect_HydrationFailureDoesNotLatch checks the other half of the
// single-use guard: a Connect that fails before it starts anything leaves the
// Client reusable, mirroring AttachRelay's "the call may be retried — it does
// not latch a partial attach" contract in provider/websocket.
func TestClient_Connect_HydrationFailureDoesNotLatch(t *testing.T) {
	store := &failingLoadStore{err: errors.New("disk gone")}
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New(), Store: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Connect(context.Background()); !errors.Is(err, store.err) {
		t.Fatalf("Connect returned %v, want the store's load error", err)
	}
	// The guard must have been released: a retry gets past it and fails on
	// the store again, rather than being refused as already-connected.
	if err := c.Connect(context.Background()); errors.Is(err, ErrAlreadyConnected) {
		t.Fatal("Connect latched after a hydration failure; the failed call started nothing")
	}
}

// failingLoadStore is a LocalStore whose LoadDoc always fails, so a test can
// drive Connect's hydration-error path.
type failingLoadStore struct{ err error }

func (s *failingLoadStore) LoadDoc(string) ([]byte, error)   { return nil, s.err }
func (s *failingLoadStore) StoreUpdate(string, []byte) error { return nil }

// TestNew_Validation exercises the guard clauses New must apply before it
// will hand back a usable *Client: a nil Doc would panic the first time
// anything touched it, and a URL with no room segment gives the (later)
// dial loop nothing to ask the server for. Both are caller mistakes that
// should fail loudly at construction, not once the app is mid-flight.
func TestNew_Validation(t *testing.T) {
	validDoc := crdt.New()

	cases := []struct {
		name string
		opts Options
	}{
		{"nil Doc", Options{URL: "ws://127.0.0.1:1/room"}},
		{"empty URL", Options{URL: "", Doc: validDoc}},
		{"URL without room segment", Options{URL: "ws://127.0.0.1:1", Doc: validDoc}},
		{"URL without room segment, trailing slash", Options{URL: "ws://127.0.0.1:1/", Doc: validDoc}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatalf("New(%+v) = nil error, want an error", tc.opts)
			}
		})
	}
}

// TestNew_AppliesDefaults locks in the brief's exact default values so a
// later refactor can't silently change client behaviour for embedders who
// leave these fields zero.
func TestNew_AppliesDefaults(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.opts.MaxBackoff != 30*time.Second {
		t.Errorf("MaxBackoff = %v, want 30s", c.opts.MaxBackoff)
	}
	if c.opts.PingInterval != 30*time.Second {
		t.Errorf("PingInterval = %v, want 30s", c.opts.PingInterval)
	}
	if c.opts.ReadLimit != 64<<20 {
		t.Errorf("ReadLimit = %v, want %v", c.opts.ReadLimit, int64(64<<20))
	}
	if c.opts.CompactEvery != 500 {
		t.Errorf("CompactEvery = %v, want 500", c.opts.CompactEvery)
	}
}

// TestNew_PreservesExplicitOptions checks the defaulting logic only fills in
// zero values, so a caller who deliberately sets these fields (including to
// values a naive `if x == 0` might mistake for "unset") keeps them.
func TestNew_PreservesExplicitOptions(t *testing.T) {
	c, err := New(Options{
		URL:          "ws://127.0.0.1:1/room",
		Doc:          crdt.New(),
		Token:        "tok",
		Header:       http.Header{"X-Test": []string{"1"}},
		MaxBackoff:   time.Minute,
		PingInterval: time.Second,
		ReadLimit:    1024,
		CompactEvery: 7,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.opts.MaxBackoff != time.Minute || c.opts.PingInterval != time.Second ||
		c.opts.ReadLimit != 1024 || c.opts.CompactEvery != 7 {
		t.Errorf("explicit options were overwritten: %+v", c.opts)
	}
	if c.opts.Token != "tok" || c.opts.Header.Get("X-Test") != "1" {
		t.Errorf("Token/Header not preserved: %+v", c.opts)
	}
}

// TestRoomFromURL pins down the room-extraction rule: the last path segment,
// percent-decoded, matching what a y-websocket server's route table expects
// as the room/doc name.
func TestRoomFromURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"simple", "ws://host/yjs/myroom", "myroom", false},
		{"percent-encoded space", "wss://h/yjs/my%20room", "my room", false},
		{"trailing slash", "ws://host/yjs/myroom/", "myroom", false},
		{"root only", "ws://host/", "", true},
		{"no path", "ws://host", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roomFromURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("roomFromURL(%q) = %q, nil, want an error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("roomFromURL(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("roomFromURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestClient_HydratesBeforeDial is the heart of this task: an app must be
// able to read (and, transitively, edit) a doc that was durable on a
// previous run even when the server is completely unreachable — the
// hydrate-before-dial ordering is what makes the client "offline-first"
// rather than "offline-eventually". ws://127.0.0.1:1 refuses every
// connection attempt (nothing listens on port 1), which is deliberately kept
// as the dial target even though this task's Connect never actually dials:
// once Task 4 adds the dial loop, this test still proves hydration survives
// total server unavailability without editing it.
//
// It also proves the hydrate-origin sentinel actually does its job: without
// it, applying the hydrated blob through the same code path that persists
// local edits would immediately write it straight back to the store,
// doubling storeUpdates on every restart for no reason. Note that the
// remote-origin sentinel would NOT be enough here — server-received updates
// are deliberately persisted, so hydration needs an origin of its own.
func TestClient_HydratesBeforeDial(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "client.db")
	sqlite, err := OpenSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })

	store := &countingStore{LocalStore: sqlite}

	// Seed the store as if a previous run persisted an offline edit.
	seedDoc := crdt.New()
	seedText := seedDoc.GetText("t")
	seedDoc.Transact(func(txn *crdt.Transaction) {
		seedText.Insert(txn, 0, "hello", nil)
	})
	seed := crdt.EncodeStateAsUpdateV1(seedDoc, nil)
	if err := store.StoreUpdate("testroom", seed); err != nil {
		t.Fatalf("seed StoreUpdate: %v", err)
	}
	if got := store.count(); got != 1 {
		t.Fatalf("store.count() after seed = %d, want 1", got)
	}

	doc := crdt.New()
	c, err := New(Options{
		URL:   "ws://127.0.0.1:1/testroom",
		Doc:   doc,
		Store: store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	connectErr := make(chan error, 1)
	go func() { connectErr <- c.Connect(ctx) }()

	// Poll for hydration: Connect hydrates synchronously before it blocks,
	// but we're racing it from another goroutine, so poll rather than assume
	// timing.
	deadline := time.Now().Add(5 * time.Second)
	for doc.GetText("t").ToString() != "hello" {
		if time.Now().After(deadline) {
			t.Fatalf("doc not hydrated within deadline; text = %q", doc.GetText("t").ToString())
		}
		time.Sleep(time.Millisecond)
	}

	// The hydrated update must not have been re-persisted: it carried the
	// remote-origin sentinel, so the local-persist hook must have skipped it.
	if got := store.count(); got != 1 {
		t.Fatalf("store.count() after hydration = %d, want 1 (hydrated update must not be re-persisted)", got)
	}

	cancel()
	select {
	case err := <-connectErr:
		if err != context.Canceled {
			t.Fatalf("Connect returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return after ctx cancellation")
	}
}

// TestClient_Close_UnblocksConnect checks the other way Connect is meant to
// return: an explicit Close, independent of the caller's context.
func TestClient_Close_UnblocksConnect(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	connectErr := make(chan error, 1)
	go func() { connectErr <- c.Connect(context.Background()) }()

	// Give Connect a moment to reach its blocking select before closing, so
	// this test can't pass by coincidence (Close racing Connect's startup).
	time.Sleep(10 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-connectErr:
		if err != nil {
			t.Fatalf("Connect returned %v, want nil after Close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return after Close")
	}
}

// TestClient_OnStatus_FanOut checks the observer contract the rest of #165
// will build on: subscribers only hear about statuses after they subscribe,
// unsubscribe is safe to call out of order, and — critically — the callback
// must not run with any Client lock held, or a subscriber that calls back
// into the Client (e.g. to unsubscribe itself, or subscribe another
// observer) would deadlock.
func TestClient_OnStatus_FanOut(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var gotA, gotB []Status
	unsubA := c.OnStatus(func(s Status) { gotA = append(gotA, s) })
	unsubB := c.OnStatus(func(s Status) {
		gotB = append(gotB, s)
		// Reentrant subscribe/unsubscribe from inside a callback must not
		// deadlock: proves emitStatus released its lock before calling out.
		unsubA()
	})

	c.emitStatus(Status{State: StateConnecting})
	if len(gotA) != 1 || len(gotB) != 1 {
		t.Fatalf("first emit: gotA=%d gotB=%d, want 1 and 1", len(gotA), len(gotB))
	}

	// unsubA was called from inside gotB's callback above; a second emit
	// must reach only B.
	c.emitStatus(Status{State: StateConnected})
	if len(gotA) != 1 {
		t.Fatalf("gotA after unsub = %d entries, want 1 (unsubscribed)", len(gotA))
	}
	if len(gotB) != 2 || gotB[1].State != StateConnected {
		t.Fatalf("gotB after second emit = %+v, want 2 entries ending in StateConnected", gotB)
	}

	unsubB()
	c.emitStatus(Status{State: StateSynced})
	if len(gotB) != 2 {
		t.Fatalf("gotB after unsubB = %d entries, want still 2", len(gotB))
	}

	// Unsubscribing twice must be a safe no-op (out-of-order/double
	// unsubscribe), matching crdt.Doc.OnUpdate's contract.
	unsubB()
}

// TestClient_Awareness checks the client wires its Awareness instance to the
// doc's own ClientID, so awareness state the caller sets locally is
// attributed to the same peer identity the sync protocol will use.
func TestClient_Awareness(t *testing.T) {
	doc := crdt.New()
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: doc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := c.Awareness()
	if a == nil {
		t.Fatal("Awareness() = nil")
	}
	if a.ClientID() != uint64(doc.ClientID()) {
		t.Fatalf("Awareness().ClientID() = %d, want %d", a.ClientID(), uint64(doc.ClientID()))
	}
}

// TestClient_Stats_ZeroValue checks Stats is available and zeroed before any
// sync loop has run (Task 3 builds no loop, so nothing should be non-zero).
func TestClient_Stats_ZeroValue(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Stats(); got != (Stats{}) {
		t.Fatalf("Stats() = %+v, want zero value", got)
	}
}

// TestClient_Synced_NotClosedWithoutServer checks Synced never fires when no
// sync has actually happened — a client that never dialed must not report
// itself synced.
func TestClient_Synced_NotClosedWithoutServer(t *testing.T) {
	c, err := New(Options{URL: "ws://127.0.0.1:1/room", Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	select {
	case <-c.Synced():
		t.Fatal("Synced() fired with no server and no sync loop")
	case <-time.After(20 * time.Millisecond):
	}
}
