package websocket_test

import (
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
)

func TestUnit_GetAwareness_UnknownRoom(t *testing.T) {
	srv := ygws.NewServer()
	aw, ok := srv.GetAwareness("no-such-room")
	assert.False(t, ok)
	assert.Nil(t, aw)
}

func TestUnit_GetAwareness_PopulatedAfterConnection(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	conn := dial(t, ts, "myroom")
	drainHandshake(t, conn, crdt.New())

	aw, ok := srv.GetAwareness("myroom")
	assert.True(t, ok)
	assert.NotNil(t, aw)
}

func TestUnit_Rooms_Empty(t *testing.T) {
	srv := ygws.NewServer()
	assert.Empty(t, srv.Rooms())
}

func TestUnit_Rooms_ListsActiveRooms(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	connA := dial(t, ts, "alpha")
	drainHandshake(t, connA, crdt.New())
	connB := dial(t, ts, "beta")
	drainHandshake(t, connB, crdt.New())

	got := srv.Rooms()
	sort.Strings(got)
	assert.Equal(t, []string{"alpha", "beta"}, got)
}
