//go:build benchheavy

// Package websocket benchmark for issue #180 (Task 3): server broadcast
// fan-out. Measures the cost of Server.BroadcastUpdate as the number of
// connected peers in the target room grows (N=10/100/500). Every peer runs
// a reader goroutine that continuously drains its connection so the
// server's per-peer writeCh never fills — a full writeCh would make this
// benchmark measure back-pressure/blocking instead of pure fan-out cost.
//
// Run:
//
//	go test -tags benchheavy ./provider/websocket/ -run '^$' -bench BroadcastFanout -benchtime=1x -benchmem
package websocket

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"

	"github.com/reearth/ygo/crdt"
)

// dialWSBench opens a WebSocket connection to the test server for the given
// room. Adapted from the dialWS helper in persistence_coalesce_test.go (and
// mirrored in persistence/adapter_test.go) for benchmark use: takes *testing.B
// and fails via b.Fatalf instead of require.NoError, since testify's require
// is unavailable/unnecessary in the benchmark path.
func dialWSBench(b *testing.B, ts *httptest.Server, room string) *gws.Conn {
	b.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/" + room
	conn, _, err := gws.DefaultDialer.Dial(u, nil)
	if err != nil {
		b.Fatalf("dial %s: %v", room, err)
	}
	return conn
}

// drainHandshakeBench reads and discards the three handshake frames the
// server always sends on connect (sync step-1, sync step-2, awareness).
// Adapted from drainWS: this benchmark only cares that clients are fully
// joined before timing starts, not the frame contents, so unlike drainWS it
// does not decode/apply sync frames into a local doc.
//
// It returns an error rather than calling b.Fatalf directly so the caller
// can close the partially-joined connection first — failing straight into
// b.Fatalf here would leak the socket (never recorded anywhere for cleanup)
// on a mid-setup failure, e.g. a tight fd ulimit at N=500.
func drainHandshakeBench(conn *gws.Conn) error {
	for i := 0; i < 3; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		_ = conn.SetReadDeadline(time.Time{})
		if err != nil {
			return fmt.Errorf("drain handshake frame %d: %w", i, err)
		}
	}
	return nil
}

// drainLoop continuously reads (and discards) messages from conn until the
// connection errors (closed locally via Close, or by the server on
// shutdown). It must run for the lifetime of every dialed client so the
// server's per-peer writeCh never fills during the timed broadcast loop —
// a full writeCh would make BroadcastFanout measure slow-peer back-pressure
// instead of fan-out cost. Closes done on exit so callers can wait for every
// reader goroutine to unwind before tearing down the server (no goroutine
// leaks across sub-benchmarks).
func drainLoop(conn *gws.Conn, done chan<- struct{}) {
	defer close(done)
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// seedBroadcastUpdate builds one small, deterministic V1 update — a short
// text insert into a freshly seeded doc — used as the fixed payload every
// BroadcastUpdate call fans out in BenchmarkBroadcastFanout.
func seedBroadcastUpdate() []byte {
	doc := crdt.New(crdt.WithClientID(1))
	txt := doc.GetText("t")
	doc.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 0, "hello benchmark", nil)
	})
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// BenchmarkBroadcastFanout measures Server.BroadcastUpdate's fan-out cost to
// a single room as the number of connected peers (N) grows. Each sub-bench
// dials N loopback WebSocket clients into "room", drains their handshake
// frames so they are fully joined, then times b.N BroadcastUpdate calls with
// every client's reader goroutine draining continuously in the background.
func BenchmarkBroadcastFanout(b *testing.B) {
	update := seedBroadcastUpdate()

	for _, n := range []int{10, 100, 500} {
		n := n
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			s := NewServer()
			ts := httptest.NewServer(s)
			defer ts.Close()
			defer func() { _ = s.Shutdown(context.Background()) }()

			// conns/doneCh are preallocated to size n, but a mid-setup failure
			// (plausible at N=500 under a tight fd ulimit: 500 client + 500
			// server sockets) can leave a suffix of both slices unset. joined
			// tracks how many entries actually got a live conn + a started
			// drainLoop goroutine; the cleanup defer below bounds itself to
			// conns[:joined]/doneCh[:joined] so it never touches a nil *gws.Conn
			// (which would panic in Close, crashing the whole bench binary
			// during the b.Fatalf-triggered Goexit unwind) or blocks forever on
			// a nil doneCh receive.
			conns := make([]*gws.Conn, n)
			doneCh := make([]chan struct{}, n)
			joined := 0
			defer func() {
				for i := 0; i < joined; i++ {
					_ = conns[i].Close()
				}
				for i := 0; i < joined; i++ {
					<-doneCh[i]
				}
			}()

			for i := 0; i < n; i++ {
				conn := dialWSBench(b, ts, "room")
				if err := drainHandshakeBench(conn); err != nil {
					// Not yet recorded in conns/joined, so close it here —
					// otherwise this socket leaks past the b.Fatalf below.
					_ = conn.Close()
					b.Fatalf("client %d: %v", i, err)
				}
				conns[i] = conn
				doneCh[i] = make(chan struct{})
				go drainLoop(conn, doneCh[i])
				joined++
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.BroadcastUpdate(context.Background(), "room", update); err != nil {
					b.Fatalf("BroadcastUpdate: %v", err)
				}
			}
			b.StopTimer()
		})
	}
}
