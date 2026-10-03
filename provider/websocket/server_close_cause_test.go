package websocket_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
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
