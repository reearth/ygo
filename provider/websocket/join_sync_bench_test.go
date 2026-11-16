package websocket_test

import (
	"context"
	"net/http/httptest"
	"testing"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

// BenchmarkJoin_FragmentedRoom measures one joiner connecting to a room of
// 20,000 typed-in-the-middle characters until it holds the room's state, and
// reports the bytes of SyncStep2 it received.
func BenchmarkJoin_FragmentedRoom(b *testing.B) {
	for _, hocuspocus := range []bool{false, true} {
		b.Run(framingName(hocuspocus), func(b *testing.B) {
			srv := ygws.NewServer()
			srv.HocuspocusFraming = hocuspocus
			srv.OnLoadDocument = func(_ context.Context, _ string, doc *crdt.Doc) error {
				txt := doc.GetText("t")
				for i := range 20000 {
					doc.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, i/2, "a", nil) })
				}
				return nil
			}
			ts := httptest.NewServer(srv)
			b.Cleanup(ts.Close)
			// A resident peer keeps the room loaded between joins.
			holder, _, err := gws.DefaultDialer.Dial(wsURL(ts, "room"), nil)
			require.NoError(b, err)
			b.Cleanup(func() { _ = holder.Close() })
			joinedSteps(b, holder, hocuspocus, "room", crdt.New(), true)

			var step2Bytes int
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				conn, _, err := gws.DefaultDialer.Dial(wsURL(ts, "room"), nil)
				require.NoError(b, err)
				for _, f := range joinedSteps(b, conn, hocuspocus, "room", crdt.New(), true) {
					step2Bytes += len(f)
				}
				_ = conn.Close()
			}
			b.ReportMetric(float64(step2Bytes)/float64(b.N), "step2-B/join")
		})
	}
}
