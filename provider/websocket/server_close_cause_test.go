package websocket_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// closeCodeCause is a context cancellation cause that names a WebSocket close
// code, as an embedder revoking a session would use.
type closeCodeCause struct {
	code   int
	reason string
}

func (c closeCodeCause) Error() string            { return c.reason }
func (c closeCodeCause) CloseCode() (int, string) { return c.code, c.reason }

func TestUnit_Server_ContextCauseCloseCode(t *testing.T) {
	tests := []struct {
		name     string
		cause    error
		wantCode int
		wantText string
	}{
		{
			name:     "cause with close code",
			cause:    closeCodeCause{code: 4001, reason: "session_revoked"},
			wantCode: 4001,
			wantText: "session_revoked",
		},
		{
			// Negative control: a cause without CloseCode keeps the stock
			// behaviour, a close with no close frame (abnormal closure, 1006).
			name:     "plain cause",
			cause:    errors.New("x"),
			wantCode: gws.CloseAbnormalClosure,
		},
		{
			name:     "wrapped cause",
			cause:    fmt.Errorf("revoking alice: %w", closeCodeCause{code: 4001, reason: "session_revoked"}),
			wantCode: 4001,
			wantText: "session_revoked",
		},
		{
			// A frame over the 125-byte control limit would be rejected and
			// the peer would see 1006.
			name:     "long reason truncated",
			cause:    closeCodeCause{code: 4001, reason: strings.Repeat("r", 200)},
			wantCode: 4001,
			wantText: strings.Repeat("r", 123),
		},
		{
			// The cut at byte 123 must not leave half a character behind.
			name:     "long reason truncated on a rune boundary",
			cause:    closeCodeCause{code: 4001, reason: strings.Repeat("r", 122) + "é" + "tail"},
			wantCode: 4001,
			wantText: strings.Repeat("r", 122),
		},
		{
			name:     "invalid utf-8 dropped from reason",
			cause:    closeCodeCause{code: 4001, reason: "bad\xffreason"},
			wantCode: 4001,
			wantText: "badreason",
		},
		{
			name:     "normal closure",
			cause:    closeCodeCause{code: gws.CloseNormalClosure, reason: "bye"},
			wantCode: gws.CloseNormalClosure,
			wantText: "bye",
		},
		{
			// Codes a peer may not receive fall back to the plain close.
			name:     "reserved code",
			cause:    closeCodeCause{code: gws.CloseAbnormalClosure, reason: "x"},
			wantCode: gws.CloseAbnormalClosure,
		},
		{
			name:     "out-of-range code",
			cause:    closeCodeCause{code: 5000, reason: "x"},
			wantCode: gws.CloseAbnormalClosure,
		},
		{
			name:     "zero code",
			cause:    closeCodeCause{code: 0, reason: "x"},
			wantCode: gws.CloseAbnormalClosure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := ygws.NewServer()
			cancels := make(chan context.CancelCauseFunc, 1)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancelCause(r.Context())
				defer cancel(nil)
				cancels <- cancel
				srv.ServeHTTP(w, r.WithContext(ctx))
			}))
			defer ts.Close()

			conn := dial(t, ts, "close-cause")
			drainHandshake(t, conn, crdt.New())
			cancel := <-cancels
			cancel(tt.cause)

			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			var err error
			for err == nil {
				_, _, err = conn.ReadMessage()
			}
			var closeErr *gws.CloseError
			require.ErrorAs(t, err, &closeErr)
			assert.Equal(t, tt.wantCode, closeErr.Code)
			if tt.wantText != "" {
				assert.Equal(t, tt.wantText, closeErr.Text)
			}
		})
	}
}

// A revoked peer that has stopped reading must not keep writing into the room
// while the server waits to send it the close frame.
func TestUnit_Server_ContextCauseCloseCode_StalledReader(t *testing.T) {
	const room = "close-cause-stalled"
	srv := ygws.NewServer()
	cancels := make(chan context.CancelCauseFunc, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		defer cancel(nil)
		cancels <- cancel
		srv.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer ts.Close()

	conn := dial(t, ts, room)
	drainHandshake(t, conn, crdt.New())
	cancel := <-cancels

	// A second, reading peer keeps the room open after the first one closes.
	observer := dial(t, ts, room)
	<-cancels
	go func() {
		for {
			if _, _, err := observer.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Stop reading and fill the server's outbound pipe so its writer blocks
	// holding the connection's write lock.
	for i := 0; i < 8; i++ {
		big := crdt.New(crdt.WithClientID(crdt.ClientID(1000 + i)))
		txt := big.GetText("fill")
		big.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, strings.Repeat("x", 4<<20), nil) })
		require.NoError(t, srv.BroadcastUpdate(context.Background(), room, crdt.EncodeStateAsUpdateV1(big, nil)))
	}
	time.Sleep(200 * time.Millisecond)

	cancel(closeCodeCause{code: 4001, reason: "session_revoked"})
	cancelled := time.Now()
	time.Sleep(50 * time.Millisecond) // let the context watcher run

	sent := 0
	for time.Since(cancelled) < 1500*time.Millisecond {
		d := crdt.New(crdt.WithClientID(crdt.ClientID(2000 + sent)))
		m := d.GetMap("writes")
		d.Transact(func(txn *crdt.Transaction) { m.Set(txn, fmt.Sprintf("k%d", sent), true) })
		if err := writeUpdate(conn, crdt.EncodeStateAsUpdateV1(d, nil)); err != nil {
			break
		}
		sent++
		time.Sleep(20 * time.Millisecond)
	}
	require.Positive(t, sent)

	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, srv.GetDoc(room).GetMap("writes").Keys(),
		"updates sent after the cancel were applied to the room")
}

// writeUpdate frames a V1 update as msgSync + MsgUpdate and returns the write error.
func writeUpdate(conn *gws.Conn, update []byte) error {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0) // msgSync
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	return conn.WriteMessage(gws.BinaryMessage, enc.Bytes())
}
