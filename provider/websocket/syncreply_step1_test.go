package websocket_test

import (
	"net/http/httptest"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// A SyncReply (Hocuspocus tag 4) that carries a SyncStep1 is dropped, a
// writable peer's as a read-only peer's. Hocuspocus's own server sends its
// SyncStep1 this way, and answers one it receives with a SyncStep2 to the
// sender alone. The server used to build that SyncStep2 and throw it away,
// and relay the SyncStep1 to every other peer, each of which answered with a
// SyncStep2 against the state vector it carried; the sender got nothing.
//
// The room's document is held locked while the step arrives, so a server that
// reads it to encode an answer stalls the sender's next frame, a Ping, until
// the lock goes; and once it goes, the other peer must still receive nothing.
func TestInteg_Hocuspocus_SyncReply_AStep1IsDroppedUnreadAndUnrelayed(t *testing.T) {
	srv := ygws.NewServer()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	const room = "syncreplystep1"
	sender, other := dial(t, ts, room), dial(t, ts, room)
	senderDoc := crdt.New(crdt.WithClientID(1))
	drainHandshake(t, sender, senderDoc)
	drainHandshake(t, other, crdt.New(crdt.WithClientID(2)))
	doc := srv.GetDoc(room)
	require.NotNil(t, doc)

	locked, release, unlocked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(unlocked)
		doc.Transact(func(*crdt.Transaction) {
			close(locked)
			<-release
		})
	}()
	<-locked
	unlock := func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-unlocked
	}
	defer unlock()

	require.NoError(t, sender.WriteMessage(gws.BinaryMessage, append([]byte{4}, ygsync.EncodeSyncStep1(senderDoc)...)))
	sendPing(t, sender)
	_ = sender.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := sender.ReadMessage()
	if assert.NoError(t, err, "the sender's Ping went unanswered while the room's document was locked: the SyncStep1 waited to read it") {
		assert.Equal(t, []byte{10}, data, "the sender's next frame is the Pong, and no answer to the SyncStep1")
	}

	unlock()
	_ = other.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, data, err = other.ReadMessage()
	require.Error(t, err, "the other peer received %v: the SyncStep1 was relayed", data)
}
