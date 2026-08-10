package mobile

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// startTestServer stands up a real provider/websocket.Server behind an
// httptest server, exactly the harness provider/client's own tests use (see
// provider/client/harness_test.go's startServer). That harness lives in
// provider/client's _test.go files and is package-private, so it cannot be
// imported from mobile — this is a deliberate, minimal re-implementation
// rather than exporting a test-only helper from a production package (see
// task-11-brief.md's testing note). It exercises ygo's own server, not a
// hand-rolled fake, for the same reason provider/client's harness does: a
// wire/handshake mistake in SyncClient should fail here, not only in
// provider/client's own suite.
func startTestServer(t *testing.T) (*ygws.Server, *httptest.Server) {
	t.Helper()
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		ts.Close()
	})
	return srv, ts
}

// wsTestURL builds the ws:// URL a SyncClient should dial to reach room on
// ts, matching roomFromURL's room-is-the-final-path-segment convention (see
// provider/client's wsURL helper).
func wsTestURL(ts *httptest.Server, room string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room
}

// waitFor polls cond every 10ms for up to timeout, failing the test if it
// never becomes true. Used for assertions that depend on background sync
// activity (SyncClient's Connect is non-blocking by design, so tests cannot
// simply call it and assert synchronously).
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// TestSyncClient_ConvergesWithServer is the acceptance test for the mobile
// binding (#165 Task 11): a device-shaped SyncClient (memory-only store,
// non-blocking Connect) must converge with a real provider/websocket server
// in both directions, exactly like provider/client's own
// TestClient_SyncRoundTrip proves for the underlying Client.
func TestSyncClient_ConvergesWithServer(t *testing.T) {
	srv, ts := startTestServer(t)
	const room = "roundtrip"

	require := func(err error) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	require(srv.Apply(context.Background(), room,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "server", nil) })
		}))

	sc, err := NewSyncClient(wsTestURL(ts, room), "", "")
	if err != nil {
		t.Fatalf("NewSyncClient: %v", err)
	}
	t.Cleanup(sc.Close)

	// Doc() must be usable before Connect().
	if sc.Doc() == nil {
		t.Fatal("Doc() returned nil before Connect")
	}
	if sc.SyncedOnce() {
		t.Fatal("SyncedOnce() true before any Connect")
	}

	sc.Connect() // must return immediately (non-blocking)

	waitFor(t, 5*time.Second, sc.SyncedOnce)
	if got := sc.Doc().GetText("t"); got != "server" {
		t.Fatalf("Doc().GetText(\"t\") = %q, want %q", got, "server")
	}

	// Direction 2: a local edit through the mobile Doc mutator reaches the
	// server.
	if err := sc.Doc().InsertText("t", 0, "client-"); err != nil {
		t.Fatalf("InsertText: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		d := srv.GetDoc(room)
		return d != nil && d.GetText("t").ToString() == "client-server"
	})
}

// TestSyncClient_ConnectIsNonBlocking guards the headline constraint: Connect
// must return immediately even though it starts a network dial + handshake
// internally, because gomobile calls run on the platform UI thread and a
// blocking Connect would freeze the app.
func TestSyncClient_ConnectIsNonBlocking(t *testing.T) {
	_, ts := startTestServer(t)
	sc, err := NewSyncClient(wsTestURL(ts, "nonblocking"), "", "")
	if err != nil {
		t.Fatalf("NewSyncClient: %v", err)
	}
	t.Cleanup(sc.Close)

	done := make(chan struct{})
	go func() {
		sc.Connect()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Connect() did not return promptly; it must be non-blocking")
	}
}

// statusRecorder is a SyncStatusObserver that records every delivery.
type statusRecorder struct {
	ch chan statusCall
}

type statusCall struct {
	state  int64
	errMsg string
}

func newStatusRecorder() *statusRecorder {
	return &statusRecorder{ch: make(chan statusCall, 64)}
}

func (r *statusRecorder) OnStatus(state int64, errMsg string) {
	r.ch <- statusCall{state: state, errMsg: errMsg}
}

// TestSyncClient_StatusObserverReceivesInt64States proves the observer sees
// the documented int64 State mapping, ending in SyncStateSynced once the
// handshake completes.
func TestSyncClient_StatusObserverReceivesInt64States(t *testing.T) {
	_, ts := startTestServer(t)
	sc, err := NewSyncClient(wsTestURL(ts, "statuses"), "", "")
	if err != nil {
		t.Fatalf("NewSyncClient: %v", err)
	}
	t.Cleanup(sc.Close)

	rec := newStatusRecorder()
	sc.SetOnStatus(rec)
	sc.Connect()

	sawSynced := false
	deadline := time.After(5 * time.Second)
	for !sawSynced {
		select {
		case call := <-rec.ch:
			if call.state < SyncStateConnecting || call.state > SyncStateDisconnected {
				t.Fatalf("OnStatus delivered out-of-range state %d", call.state)
			}
			if call.state == SyncStateSynced {
				sawSynced = true
			}
		case <-deadline:
			t.Fatal("never observed SyncStateSynced via the status observer")
		}
	}
}

// TestSyncClient_SyncedOnceFlips proves SyncedOnce transitions false -> true
// exactly once real convergence has happened, not merely once Connect was
// called.
func TestSyncClient_SyncedOnceFlips(t *testing.T) {
	_, ts := startTestServer(t)
	sc, err := NewSyncClient(wsTestURL(ts, "syncedonce"), "", "")
	if err != nil {
		t.Fatalf("NewSyncClient: %v", err)
	}
	t.Cleanup(sc.Close)

	if sc.SyncedOnce() {
		t.Fatal("SyncedOnce() true before Connect was even called")
	}
	sc.Connect()
	waitFor(t, 5*time.Second, sc.SyncedOnce)
}

// TestSyncClient_DocUsableBeforeConnect proves the offline-first promise:
// the UI can bind to and mutate Doc() before Connect is ever called.
func TestSyncClient_DocUsableBeforeConnect(t *testing.T) {
	sc, err := NewSyncClient("ws://127.0.0.1:1/unreachable-room", "", "")
	if err != nil {
		t.Fatalf("NewSyncClient: %v", err)
	}
	t.Cleanup(sc.Close)

	if err := sc.Doc().InsertText("t", 0, "offline"); err != nil {
		t.Fatalf("InsertText before Connect: %v", err)
	}
	if got := sc.Doc().GetText("t"); got != "offline" {
		t.Fatalf("GetText = %q, want %q", got, "offline")
	}
}

// TestSyncClient_Close proves Close is safe to call without ever connecting,
// and safe to call twice.
func TestSyncClient_Close(t *testing.T) {
	sc, err := NewSyncClient("ws://127.0.0.1:1/unreachable-room", "", "")
	if err != nil {
		t.Fatalf("NewSyncClient: %v", err)
	}
	sc.Close()
	sc.Close() // idempotent
}

// TestSyncClient_StorePathPersistsAcrossRestart is the mobile-binding
// review's Important 2 (#165 Task 11 review): every prior test used
// dbPath == "" (memory-only), so none of them ever reached the branch of
// NewSyncClient that can fail, and nothing proved the binding actually
// persists across a restart — the headline premise of SyncClient's own
// godoc ("the device's content survives a process restart").
//
// This proves it end to end: sc1 syncs against a real server and picks up
// its content; sc1.Close(); a SECOND SyncClient opens the SAME dbPath but
// points at a server that is not listening at all (ws://127.0.0.1:1 — a
// reserved port nothing binds to), so any content it ends up with can only
// have come from the local store, never from a sync. SyncedOnce() staying
// false on sc2 is the other half of that proof: it rules out a false
// positive where the assertion happened to pass because sc2 quietly
// connected to something.
func TestSyncClient_StorePathPersistsAcrossRestart(t *testing.T) {
	srv, ts := startTestServer(t)
	const room = "restart"
	dbPath := filepath.Join(t.TempDir(), "sync.db")

	if err := srv.Apply(context.Background(), room,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "server", nil) })
		}); err != nil {
		t.Fatalf("seed server doc: %v", err)
	}

	sc1, err := NewSyncClient(wsTestURL(ts, room), dbPath, "")
	if err != nil {
		t.Fatalf("NewSyncClient (first): %v", err)
	}
	sc1.Connect()
	waitFor(t, 5*time.Second, sc1.SyncedOnce)
	if got := sc1.Doc().GetText("t"); got != "server" {
		t.Fatalf("first SyncClient Doc().GetText(\"t\") = %q, want %q", got, "server")
	}
	sc1.Close() // must release the owned SQLite file so sc2 below can reopen it

	sc2, err := NewSyncClient("ws://127.0.0.1:1/"+room, dbPath, "")
	if err != nil {
		t.Fatalf("NewSyncClient (restart): %v", err)
	}
	t.Cleanup(sc2.Close)
	sc2.Connect() // hydrates from dbPath immediately; dialing the unreachable URL fails and retries forever in the background

	waitFor(t, 5*time.Second, func() bool { return sc2.Doc().GetText("t") == "server" })
	if sc2.SyncedOnce() {
		t.Fatal("SyncedOnce() reported true against an unreachable server; " +
			"the content must have come from the local store, not a sync that should be impossible here")
	}
}

// TestSyncClient_BadDBPath_ReturnsError proves the other half of Important
// 2's coverage gap: a dbPath NewSyncClient cannot actually open as a local
// SQLite database must fail construction outright — via NewSyncClient's own
// error return — rather than handing back a SyncClient that only discovers
// the problem later, silently, on some background goroutine.
//
// A directory is used as the "bad path": persistence/sqlite.Open runs its
// schema migration (CREATE TABLE IF NOT EXISTS ...) eagerly, synchronously,
// inside Open itself, so opening a directory as if it were a database file
// fails immediately and unconditionally, with no test-environment-specific
// setup (permissions, disk state) required.
func TestSyncClient_BadDBPath_ReturnsError(t *testing.T) {
	dir := t.TempDir() // a directory, not a file: cannot be opened as a SQLite database
	sc, err := NewSyncClient("ws://127.0.0.1:1/room", dir, "")
	if err == nil {
		t.Fatal("NewSyncClient with a directory as dbPath should have failed to open the local store")
	}
	if sc != nil {
		t.Fatal("NewSyncClient returned a non-nil *SyncClient alongside an error")
	}
}
