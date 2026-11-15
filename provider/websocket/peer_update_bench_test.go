package websocket_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// BenchmarkPeerUpdateRelay times one peer's keystroke update from its send
// until another peer in the room reads the server's relay of it, with and
// without persistence.
func BenchmarkPeerUpdateRelay(b *testing.B) {
	for _, bc := range []struct {
		name string
		srv  func() *ygws.Server
	}{
		{"persistence=none", ygws.NewServer},
		{"persistence=memory", func() *ygws.Server { return ygws.NewServerWithPersistence(ygws.NewMemoryPersistence()) }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			srv := bc.srv()
			ts := httptest.NewServer(srv)
			defer ts.Close()
			defer func() { _ = srv.Shutdown(context.Background()) }()
			sender, receiver := dial(b, ts, "room"), dial(b, ts, "room")
			drainHandshake(b, sender, crdt.New())
			drainHandshake(b, receiver, crdt.New())

			src := crdt.New(crdt.WithClientID(7))
			txt := src.GetText("t")
			updates := make([][]byte, 0, b.N)
			unsub := src.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
			for i := range b.N {
				src.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, i, "x", nil) })
			}
			unsub()

			b.ResetTimer()
			for _, u := range updates {
				sendUpdate(b, sender, u)
				if _, _, err := receiver.ReadMessage(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
