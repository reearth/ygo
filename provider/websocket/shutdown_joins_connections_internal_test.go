package websocket

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/encoding"
)

// blockUntilReleased blocks until release is closed, bounded so a failed test
// cannot hang the hook.
func blockUntilReleased(release <-chan struct{}) error {
	select {
	case <-release:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("the test never released the hook")
	}
}

// serveJoinedPeer serves srv, connects one peer to room "r1", and waits until
// the server has sent it the first frame of its handshake, so it has joined.
func serveJoinedPeer(t *testing.T, srv *Server) *gws.Conn {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	c := dialRoomInternal(t, ts, "r1")
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err := c.ReadMessage()
	require.NoError(t, err)
	return c
}

// within returns what ch delivers within d, or fails t.
func within[T any](t *testing.T, ch <-chan T, d time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatal(what)
		panic("unreachable")
	}
}

// shutdownHeldByHook calls Shutdown(context.Background()), waits for the hook
// to start, checks that Shutdown does not return while the hook runs, then
// releases the hook and returns Shutdown's result.
func shutdownHeldByHook(t *testing.T, srv *Server, started <-chan struct{}, release chan struct{}) error {
	t.Helper()
	returned := make(chan error, 1)
	go func() { returned <- srv.Shutdown(context.Background()) }()
	within(t, started, 2*time.Second, "the hook was not called")
	select {
	case err := <-returned:
		close(release)
		t.Fatalf("Shutdown returned %v while the hook still ran", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	return within(t, returned, 5*time.Second, "Shutdown did not return after the hook returned")
}

// tokenHookRunning serves a server whose OnTokenAuth blocks until release is
// closed, and returns once a peer's token has started that hook.
func tokenHookRunning(t *testing.T) (srv *Server, started, release chan struct{}) {
	t.Helper()
	started, release = make(chan struct{}), make(chan struct{})
	srv = NewServer()
	srv.HocuspocusFraming = true
	srv.OnTokenAuth = func(string, string) (ConnectionConfig, error) {
		close(started)
		return ConnectionConfig{}, blockUntilReleased(release)
	}
	c := serveJoinedPeer(t, srv)
	require.NoError(t, c.WriteMessage(gws.BinaryMessage, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarString("r1")
		enc.WriteVarUint(msgAuth)
		enc.WriteVarUint(authTypeToken)
		enc.WriteVarString("token")
	})))
	within(t, started, 2*time.Second, "OnTokenAuth was not called")
	return srv, started, release
}

// Shutdown does not return nil while a hook still runs on a connection's read
// loop.
func TestUnit_Shutdown_WaitsForAHookOnTheReadLoop(t *testing.T) {
	srv, started, release := tokenHookRunning(t)
	require.NoError(t, shutdownHeldByHook(t, srv, started, release))
}

// Shutdown does not return nil while a connection's teardown still runs a hook.
func TestUnit_Shutdown_WaitsForATeardownHook(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := NewServer()
	srv.OnLastPeer = func(context.Context, string) {
		close(started)
		_ = blockUntilReleased(release)
	}
	serveJoinedPeer(t, srv)

	require.NoError(t, shutdownHeldByHook(t, srv, started, release))
}

// Shutdown returns ctx's error once ctx expires, while a hook on a connection's
// read loop still runs.
func TestUnit_Shutdown_ReturnsTheContextErrorWhenAHookOutlivesIt(t *testing.T) {
	srv, _, release := tokenHookRunning(t)
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- srv.Shutdown(ctx) }()
	err := within(t, returned, 2*time.Second, "Shutdown did not return when its ctx expired")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// authorizeHeld makes srv's Authorize block until release is closed, serves
// srv, and returns once a dialed connection is in Authorize.
func authorizeHeld(t *testing.T, srv *Server) (started, release chan struct{}) {
	t.Helper()
	started, release = make(chan struct{}), make(chan struct{})
	srv.Authorize = func(*http.Request) (ConnectionConfig, bool) {
		close(started)
		return ConnectionConfig{}, blockUntilReleased(release) == nil
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	go func() {
		c, _, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/r1", nil)
		if err == nil {
			_ = c.Close()
		}
	}()
	within(t, started, 2*time.Second, "Authorize was not called")
	return started, release
}

// Shutdown waits for a connection that is still in Authorize, before the
// upgrade.
func TestUnit_Shutdown_WaitsForAConnectionInAuthorize(t *testing.T) {
	srv := NewServer()
	started, release := authorizeHeld(t, srv)
	require.NoError(t, shutdownHeldByHook(t, srv, started, release))
}

// stalledRelay's RoomActivated or RoomDeactivated waits for the context Start
// received, as a relay call that waits on the relay context does.
type stalledRelay struct {
	ctx                            context.Context
	stallActivate, stallDeactivate bool
}

func (r *stalledRelay) Start(ctx context.Context, _ cluster.Sink) error { r.ctx = ctx; return nil }
func (r *stalledRelay) Publish(context.Context, cluster.Outbound) error { return nil }
func (r *stalledRelay) Close() error                                    { return nil }

func (r *stalledRelay) RoomActivated(string) {
	if r.stallActivate {
		<-r.ctx.Done()
	}
}

func (r *stalledRelay) RoomDeactivated(string) {
	if r.stallDeactivate {
		<-r.ctx.Done()
	}
}

// shutdownReturnsNil calls Shutdown with a 2 s ctx and requires nil within 1 s.
func shutdownReturnsNil(t *testing.T, srv *Server, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	returned := make(chan error, 1)
	go func() { returned <- srv.Shutdown(ctx) }()
	require.NoError(t, within(t, returned, time.Second, what))
}

// A connection's teardown during Shutdown does not call the relay, whose
// context Shutdown cancels only after it has joined the connections.
func TestUnit_Shutdown_DoesNotWaitForTheRelayInATeardown(t *testing.T) {
	srv := NewServer()
	require.NoError(t, srv.AttachRelay(&stalledRelay{stallDeactivate: true}))
	serveJoinedPeer(t, srv)

	shutdownReturnsNil(t, srv, "Shutdown waited for the relay's RoomDeactivated")
}

// A connection that creates its room during Shutdown does not call the relay.
func TestUnit_Shutdown_DoesNotWaitForTheRelayInARoomCreation(t *testing.T) {
	srv := NewServer()
	require.NoError(t, srv.AttachRelay(&stalledRelay{stallActivate: true}))
	_, release := authorizeHeld(t, srv)
	go func() {
		<-srv.shutdownCh // Shutdown has begun
		close(release)   // the connection goes on to create room r1
	}()

	shutdownReturnsNil(t, srv, "Shutdown waited for the relay's RoomActivated")
}
