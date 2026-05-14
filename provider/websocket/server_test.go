package websocket_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// pipeDialer returns a gorilla Dialer whose NetDial hook uses net.Pipe() to
// create an in-process connection pair and delivers the server-side half to
// the supplied httptest.Server via a custom net.Listener. Writes on net.Pipe
// block immediately when the reader is not consuming (no OS-level buffering),
// making writeCh overflow deterministic in tests.
//
// Usage:
//
//	ln := newPipeListener()
//	ts := httptest.NewUnstartedServer(handler)
//	ts.Listener = ln
//	ts.Start()
//	conn := pipeDialer(ln).Dial("ws://unused/room", nil)
func newPipeListener() *pipeListener {
	return &pipeListener{
		ch:     make(chan net.Conn, 8),
		addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
		closed: make(chan struct{}),
	}
}

type pipeListener struct {
	ch     chan net.Conn
	addr   net.Addr
	closed chan struct{}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.addr }

// pipeDialer returns a gorilla WebSocket Dialer that connects via net.Pipe()
// rather than TCP. Every Dial() call creates a new net.Pipe() pair and sends
// the server-side to l so the httptest.Server's Accept loop picks it up.
func pipeDialer(l *pipeListener) *gws.Dialer {
	return &gws.Dialer{
		NetDial: func(_, _ string) (net.Conn, error) {
			serverSide, clientSide := net.Pipe()
			l.ch <- serverSide
			return clientSide, nil
		},
	}
}

// wsURL converts an httptest server URL to a ws:// URL.
func wsURL(srv *httptest.Server, room string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/" + room
}

// dial opens a WebSocket connection to the test server.
func dial(t *testing.T, srv *httptest.Server, room string) *gws.Conn {
	t.Helper()
	conn, _, err := gws.DefaultDialer.Dial(wsURL(srv, room), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readOne reads a single WebSocket message with a deadline, then clears it.
// Returns the outer type and decoded payload.
// For sync (type 0), payload is the raw sync bytes (no length prefix).
// For awareness (type 1), payload is the VarBytes-unwrapped awareness bytes.
func readOne(t *testing.T, conn *gws.Conn, deadline time.Duration) (outerType uint64, payload []byte) { //nolint:unparam
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	_, data, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{}) // reset immediately so the conn stays usable
	require.NoError(t, err)

	dec := encoding.NewDecoder(data)
	outerType, err = dec.ReadVarUint()
	require.NoError(t, err)

	if outerType == 1 { // msgAwareness — VarBytes-wrapped
		payload, err = dec.ReadVarBytes()
		require.NoError(t, err)
	} else {
		payload = dec.RemainingBytes()
	}
	return
}

// drainHandshake reads the three messages the server always sends on connect
// (step-1, step-2, awareness) and applies any sync messages to doc.
// Gorilla's bufio.Reader is permanently broken if a deadline expires, so we
// read a known count instead of draining by timeout.
func drainHandshake(t *testing.T, conn *gws.Conn, doc *crdt.Doc) {
	t.Helper()
	for i := 0; i < 3; i++ {
		outerType, payload := readOne(t, conn, 2*time.Second)
		if outerType == 0 { // msgSync
			_, _ = ygsync.ApplySyncMessage(doc, payload, nil)
		}
	}
}

// sendSync wraps payload in an outer msgSync message and sends it.
// Sync payload is NOT VarBytes-wrapped (raw append after type byte).
func sendSync(t *testing.T, conn *gws.Conn, syncMsg []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteRaw(syncMsg)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))
}

// sendAwareness wraps payload in an outer msgAwareness message.
// Awareness payload IS VarBytes-wrapped.
func sendAwareness(t *testing.T, conn *gws.Conn, awMsg []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // msgAwareness
	enc.WriteVarBytes(awMsg)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))
}

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestUnit_NewServer_GetDoc_ReturnsNilForUnknownRoom(t *testing.T) {
	srv := ygws.NewServer()
	assert.Nil(t, srv.GetDoc("no-such-room"))
}

func TestUnit_ServerHandshake_SendsStep1AndStep2ThenAwareness(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "room1")

	// Message 1: msgSync + step-1
	outerType, payload := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType, "first msg should be msgSync")
	dec := encoding.NewDecoder(payload)
	syncType, err := dec.ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(ygsync.MsgSyncStep1), syncType)

	// Message 2: msgSync + step-2
	outerType, payload = readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(0), outerType, "second msg should be msgSync")
	dec = encoding.NewDecoder(payload)
	syncType, err = dec.ReadVarUint()
	require.NoError(t, err)
	assert.Equal(t, uint64(ygsync.MsgSyncStep2), syncType)

	// Message 3: msgAwareness
	outerType, _ = readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(1), outerType, "third msg should be msgAwareness")
}

func TestUnit_GetDoc_PopulatedAfterFirstConnection(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "myroom")
	drainHandshake(t, conn, crdt.New())

	assert.NotNil(t, srv.GetDoc("myroom"))
}

// ── Integration tests ─────────────────────────────────────────────────────────

func TestInteg_TwoPeer_DocumentConvergence(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Peer A connects, drains handshake, then sends "hello" to the server.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dial(t, ts, "room")
	drainHandshake(t, connA, docA)

	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connA, enc.Bytes())

	time.Sleep(50 * time.Millisecond)

	// Peer B connects and drains the handshake; step-2 should contain "hello".
	docB := crdt.New(crdt.WithClientID(2))
	connB := dial(t, ts, "room")
	drainHandshake(t, connB, docB)

	assert.Equal(t, "hello", docB.GetText("t").ToString())
}

func TestInteg_StepOneReply_ServerRespondsWithStep2(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// First peer seeds the room with "world".
	seedDoc := crdt.New(crdt.WithClientID(10))
	connSeed := dial(t, ts, "seeded")
	drainHandshake(t, connSeed, seedDoc)

	seedTxt := seedDoc.GetText("t")
	seedDoc.Transact(func(txn *crdt.Transaction) { seedTxt.Insert(txn, 0, "world", nil) })
	update := crdt.EncodeStateAsUpdateV1(seedDoc, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connSeed, enc.Bytes())
	time.Sleep(50 * time.Millisecond)

	// Second peer connects; step-2 in handshake should contain "world".
	docNew := crdt.New(crdt.WithClientID(20))
	connNew := dial(t, ts, "seeded")
	drainHandshake(t, connNew, docNew)

	assert.Equal(t, "world", docNew.GetText("t").ToString())
}

func TestInteg_IncrementalUpdate_BroadcastToPeer(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Both peers connect.
	docA := crdt.New(crdt.WithClientID(1))
	docB := crdt.New(crdt.WithClientID(2))
	connA := dial(t, ts, "room")
	connB := dial(t, ts, "room")
	drainHandshake(t, connA, docA)
	drainHandshake(t, connB, docB)

	// A sends an update.
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "incremental", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connA, enc.Bytes())

	// B should receive the broadcast.
	outerType, payload := readOne(t, connB, 2*time.Second)
	assert.Equal(t, uint64(0), outerType)

	// Apply to B's doc.
	_, _ = ygsync.ApplySyncMessage(docB, payload, nil)
	assert.Equal(t, "incremental", docB.GetText("t").ToString())
}

func TestInteg_AwarenessBroadcast_PeerReceivesState(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	// Both peers connect and drain their handshakes.
	connA := dial(t, ts, "awroom")
	connB := dial(t, ts, "awroom")
	drainHandshake(t, connA, crdt.New())
	drainHandshake(t, connB, crdt.New())

	// A sends an awareness update with clientID=42.
	aw := awareness.New(42)
	aw.SetLocalState(map[string]any{"cursor": float64(5)})
	sendAwareness(t, connA, aw.EncodeUpdate(nil))

	// B should receive the awareness broadcast.
	outerType, awPayload := readOne(t, connB, 2*time.Second)
	assert.Equal(t, uint64(1), outerType)

	// Parse and verify.
	awDec := awareness.New(99)
	require.NoError(t, awDec.ApplyUpdate(awPayload, nil))
	states := awDec.GetStates()
	require.Contains(t, states, uint64(42))
	assert.InEpsilon(t, float64(5), states[42].State["cursor"], 1e-9)
}

func TestInteg_QueryAwareness_ReturnsCurrentState(t *testing.T) {
	ts := httptest.NewServer(ygws.NewServer())
	defer ts.Close()

	conn := dial(t, ts, "qroom")
	drainHandshake(t, conn, crdt.New())

	// Send msgQueryAwareness (type 3).
	enc := encoding.NewEncoder()
	enc.WriteVarUint(3)
	require.NoError(t, conn.WriteMessage(gws.BinaryMessage, enc.Bytes()))

	outerType, _ := readOne(t, conn, 2*time.Second)
	assert.Equal(t, uint64(1), outerType)
}

func TestUnit_MaxPeersPerRoom_NotExceeded_UnderConcurrency(t *testing.T) {
	srv := ygws.NewServer()
	srv.MaxPeersPerRoom = 2

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("room", "test")
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()

	connect := func() (*gws.Conn, error) {
		u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/test"
		c, _, err := gws.DefaultDialer.Dial(u, nil)
		return c, err
	}

	// First two connections must succeed
	c1, err := connect()
	require.NoError(t, err)
	defer c1.Close()
	c2, err := connect()
	require.NoError(t, err)
	defer c2.Close()

	// Third must be rejected (503)
	resp, err := http.Get(strings.Replace(ts.URL+"/test", "http", "http", 1))
	if err == nil {
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		resp.Body.Close()
	}
}

func TestInteg_MultiRoom_Isolated(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// Peer in room-A sends "alpha".
	docA := crdt.New(crdt.WithClientID(1))
	connA := dial(t, ts, "room-a")
	drainHandshake(t, connA, docA)

	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "alpha", nil) })
	update := crdt.EncodeStateAsUpdateV1(docA, nil)
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, connA, enc.Bytes())
	time.Sleep(50 * time.Millisecond)

	// Peer connecting to room-B must NOT see "alpha".
	docB := crdt.New(crdt.WithClientID(2))
	connB := dial(t, ts, "room-b")
	drainHandshake(t, connB, docB)

	assert.Empty(t, docB.GetText("t").ToString())
	assert.NotNil(t, srv.GetDoc("room-a"))
	assert.NotNil(t, srv.GetDoc("room-b"))
}

func TestServer_SlowPeer_GetsDisconnectedOnQueueOverflow(t *testing.T) {
	// Stand up a server with a tiny peer-write queue (cap 4). Use net.Pipe()
	// connections (via pipeListener) so that server-side writes block as soon
	// as the reader stops consuming — there is no OS-level kernel buffering.
	// This makes the writeCh overflow deterministic:
	//
	//   1. Both peers connect via net.Pipe().
	//   2. Both peers drain their 3-message handshake.
	//   3. Slow peer stops reading; server-side writes to it block immediately.
	//   4. runWriter for the slow peer blocks on the first broadcast write.
	//   5. Fast peer sends more messages; broadcasts fill writeCh (cap 4).
	//   6. The 5th broadcast finds writeCh full → overflow → conn.Close().
	//   7. slowConn.ReadMessage() returns an error.

	srv := ygws.NewServer()
	srv.PeerWriteQueueSize = 4

	ln := newPipeListener()
	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = ln
	ts.Start()
	defer ts.Close()

	dialer := pipeDialer(ln)
	// Use a placeholder URL — the dialer ignores the host and injects a pipe.
	const wsBase = "ws://pipe"

	// Fast peer: connects via pipe, drains handshake, keeps reading.
	fastConn, _, err := dialer.Dial(wsBase+"/overflow-room", nil)
	require.NoError(t, err)
	defer fastConn.Close()
	fastDoc := crdt.New(crdt.WithClientID(1))
	_ = fastConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 3; i++ {
		if _, _, e := fastConn.ReadMessage(); e != nil {
			break
		}
	}
	_ = fastConn.SetReadDeadline(time.Time{})
	// Start a goroutine that keeps draining from fastConn so the server can
	// write broadcast messages back without blocking.
	go func() {
		for {
			if _, _, e := fastConn.ReadMessage(); e != nil {
				return
			}
		}
	}()

	// Slow peer: connects via pipe.
	slowConn, _, err := dialer.Dial(wsBase+"/overflow-room", nil)
	require.NoError(t, err)
	defer slowConn.Close()

	// Drain slow peer's 3-message handshake to free up writeCh capacity.
	_ = slowConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 3; i++ {
		if _, _, e := slowConn.ReadMessage(); e != nil {
			break
		}
	}
	_ = slowConn.SetReadDeadline(time.Time{}) // stop reading — writes to slow peer now block

	// Fast peer sends messages; each is broadcast to the slow peer.
	// runWriter for slow peer blocks on the first write (net.Pipe has no OS
	// buffer). After 4 more queued broadcasts, the 5th fires the overflow.
	fastTxt := fastDoc.GetText("t") // fetch outside Transact to avoid deadlock
	for i := 0; i < 20; i++ {
		fastDoc.Transact(func(txn *crdt.Transaction) {
			fastTxt.Insert(txn, 0, "x", nil)
		})
		update := crdt.EncodeStateAsUpdateV1(fastDoc, nil)
		enc := encoding.NewEncoder()
		enc.WriteVarUint(ygsync.MsgUpdate)
		enc.WriteVarBytes(update)
		if err := fastConn.WriteMessage(gws.BinaryMessage, enc.Bytes()); err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond) // let the server process + broadcast each message
	}

	// slowConn should get an error because the server closed its connection.
	_ = slowConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, readErr := slowConn.ReadMessage()
	assert.Error(t, readErr, "slow peer should be disconnected by the server after queue overflow")
}

func TestServer_RunWriter_NoLeakOnConnectChurn(t *testing.T) {
	// Regression for #33: runWriter must not leak when the room
	// membership TOCTOU check fails after peer setup.
	//
	// Strategy: open and close many connections rapidly to a server
	// configured with no persistence and rapid room creation/deletion
	// cycles. Without the fix, each TOCTOU loss leaks one runWriter
	// goroutine.
	gotBefore := runtime.NumGoroutine()

	s := ygws.NewServer()
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/race-room"

	for i := 0; i < 50; i++ {
		c, _, err := gws.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			continue
		}
		_ = c.Close()
	}

	// Allow time for goroutines to fully tear down.
	time.Sleep(500 * time.Millisecond)

	gotAfter := runtime.NumGoroutine()
	const slack = 5
	assert.LessOrEqual(t, gotAfter-gotBefore, slack,
		"runWriter goroutine leak suspected: %d before, %d after %d connect/disconnect cycles",
		gotBefore, gotAfter, 50)
}

func TestServer_MaxConnections_HardCapUnderConcurrentBurst(t *testing.T) {
	// Open many concurrent connections to a server with MaxConnections=10.
	// All should either succeed (≤ 10 active at once) or be rejected with
	// HTTP 503; never more than 10 simultaneously connected.

	const cap = 10
	const burst = 50

	s := ygws.NewServer()
	s.MaxConnections = cap
	httpSrv := httptest.NewServer(s)
	defer httpSrv.Close()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/cap-room"

	var wg sync.WaitGroup
	var holding atomic.Int32
	var maxObserved atomic.Int32
	var rejected atomic.Int32

	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := gws.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				rejected.Add(1)
				return
			}
			n := holding.Add(1)
			for {
				if m := maxObserved.Load(); n > m {
					if maxObserved.CompareAndSwap(m, n) {
						break
					}
					continue
				}
				break
			}
			time.Sleep(100 * time.Millisecond) // hold the conn long enough to observe max
			holding.Add(-1)
			_ = c.Close()
		}()
	}
	wg.Wait()

	assert.LessOrEqual(t, int(maxObserved.Load()), cap,
		"max concurrent connections (%d) exceeded the cap (%d) — atomic-counter race window",
		maxObserved.Load(), cap)
	assert.Positive(t, int(rejected.Load()), "some connections should have been rejected")
}

// --- PersistenceAdapterContext (#35) ---

// captureCtxAdapter implements both PersistenceAdapter and
// PersistenceAdapterContext. Records whether the ctx-aware path was used.
type captureCtxAdapter struct {
	mu             sync.Mutex
	contextUsed    bool
	legacyUsed     bool
	receivedCtxErr func() error // closure to read ctx.Err() at call time
}

func (a *captureCtxAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }

func (a *captureCtxAdapter) StoreUpdate(room string, update []byte) error {
	a.mu.Lock()
	a.legacyUsed = true
	a.mu.Unlock()
	return nil
}

func (a *captureCtxAdapter) StoreUpdateContext(ctx context.Context, room string, update []byte) error {
	a.mu.Lock()
	a.contextUsed = true
	a.receivedCtxErr = ctx.Err
	a.mu.Unlock()
	return nil
}

func TestServer_PersistenceAdapterContext_PreferredOverLegacy(t *testing.T) {
	a := &captureCtxAdapter{}
	s := ygws.NewServerWithPersistence(a)
	defer s.Shutdown(context.Background())

	// Trigger a persistence write by calling Apply.
	err := s.Apply(context.Background(), "room1", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		text := doc.GetText("body")
		transact(func(txn *crdt.Transaction) {
			text.Insert(txn, 0, "hello", nil)
		})
	})
	require.NoError(t, err)

	// Allow the persistence goroutine to run.
	time.Sleep(100 * time.Millisecond)

	a.mu.Lock()
	contextUsed := a.contextUsed
	legacyUsed := a.legacyUsed
	a.mu.Unlock()

	assert.True(t, contextUsed, "ctx-aware StoreUpdateContext should be called when adapter implements it")
	assert.False(t, legacyUsed, "legacy StoreUpdate should be skipped when ctx variant is available")
}

// legacyAdapter implements only PersistenceAdapter (no ctx variant).
type legacyAdapter struct {
	mu     sync.Mutex
	called bool
}

func (a *legacyAdapter) LoadDoc(string) ([]byte, error) { return nil, nil }
func (a *legacyAdapter) StoreUpdate(room string, update []byte) error {
	a.mu.Lock()
	a.called = true
	a.mu.Unlock()
	return nil
}

func TestServer_LegacyPersistenceAdapter_StillWorks(t *testing.T) {
	a := &legacyAdapter{}
	s := ygws.NewServerWithPersistence(a)
	defer s.Shutdown(context.Background())

	err := s.Apply(context.Background(), "room2", func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		arr := doc.GetArray("items")
		transact(func(txn *crdt.Transaction) {
			arr.Insert(txn, 0, []any{"x"})
		})
	})
	require.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	a.mu.Lock()
	called := a.called
	a.mu.Unlock()
	assert.True(t, called, "legacy StoreUpdate must still be called for adapters without ctx variant")
}
