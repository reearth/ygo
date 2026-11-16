package websocket_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// Tests for Server.OnStateAnswer, which runs before the server answers a
// peer's SyncStep1 with the room's state, and whose release runs once the
// server is done with that answer.

const stateAnswerRoomName = "room"

// stateAnswerRoom starts a Hocuspocus-framed server whose room holds text, so
// an answer to a SyncStep1 carries something. configure, when non-nil, runs
// before the server starts.
func stateAnswerRoom(t *testing.T, text string, configure func(*ygws.Server)) *httptest.Server {
	t.Helper()
	srv, ts := hocuspocusServer(t, configure)
	require.NoError(t, srv.Apply(context.Background(), stateAnswerRoomName,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t") // outside transact, which holds the write lock
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, text, nil) })
		}))
	return ts
}

// joinAndReadStep1 connects to the room and reads the SyncStep1 the server
// greets every peer with, so what follows answers the peer's own SyncStep1.
func joinAndReadStep1(t *testing.T, ts *httptest.Server) *gws.Conn {
	t.Helper()
	conn := dial(t, ts, stateAnswerRoomName)
	tag, body, err := hpRead(conn, stateAnswerRoomName, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, tagSync, tag)
	sub, _, err := ygsync.ReadSyncMessage(body)
	require.NoError(t, err)
	require.Equal(t, ygsync.MsgSyncStep1, sub)
	return conn
}

// readStateAnswer reads the sync messages of one answer, up to and including
// the SyncStep2 that ends it.
func readStateAnswer(t *testing.T, conn *gws.Conn) [][]byte {
	t.Helper()
	var msgs [][]byte
	for {
		tag, body, err := hpRead(conn, stateAnswerRoomName, 2*time.Second)
		require.NoError(t, err)
		if tag != tagSync {
			continue
		}
		msgs = append(msgs, body)
		sub, _, err := ygsync.ReadSyncMessage(body)
		require.NoError(t, err)
		if sub == ygsync.MsgSyncStep2 {
			return msgs
		}
	}
}

// The hook runs before the answer and its release after it: while the hook
// has not returned the peer is sent nothing, and the answer reaches the peer
// while the release waits for it, so its frames were queued before the
// release ran.
func TestInteg_OnStateAnswer_RunsBeforeTheAnswerAndReleasesAfterIt(t *testing.T) {
	admit := make(chan struct{})
	answered := make(chan struct{})
	called := make(chan ygws.StateAnswerInfo, 4)
	released := make(chan bool, 4) // whether the answer arrived while the release waited
	ts := stateAnswerRoom(t, "the room's state", func(srv *ygws.Server) {
		srv.OnStateAnswer = func(info ygws.StateAnswerInfo) (func(), error) {
			called <- info
			<-admit
			return func() {
				select {
				case <-answered:
					released <- true
				case <-time.After(2 * time.Second):
					released <- false
				}
			}, nil
		}
	})

	conn := joinAndReadStep1(t, ts)
	// A read that times out leaves a gorilla connection unusable, so the
	// frames are read on a goroutine and the test waits on the channel.
	sent := make(chan []byte, 32)
	go func() {
		for {
			tag, body, err := hpRead(conn, stateAnswerRoomName, 30*time.Second)
			if err != nil {
				close(sent)
				return
			}
			if tag == tagSync {
				sent <- body
			}
		}
	}()
	step1 := ygsync.EncodeSyncStep1(crdt.New())
	hpSend(t, conn, stateAnswerRoomName, tagSync, step1)

	var info ygws.StateAnswerInfo
	select {
	case info = <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("the hook never ran")
	}
	assert.Equal(t, stateAnswerRoomName, info.Room)
	assert.NotNil(t, info.Doc)
	assert.Positive(t, info.Structs)
	assert.Equal(t, step1, info.Step1)

	time.Sleep(250 * time.Millisecond)
	require.Empty(t, sent, "the peer was answered before the hook returned")
	require.Empty(t, released, "the release ran before the hook returned")

	close(admit)
	for {
		var body []byte
		select {
		case body = <-sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the admitted answer never reached the peer")
		}
		sub, _, err := ygsync.ReadSyncMessage(body)
		require.NoError(t, err)
		if sub == ygsync.MsgSyncStep2 {
			break
		}
	}
	close(answered)
	select {
	case arrived := <-released:
		assert.True(t, arrived, "the release ran before the answer was queued")
	case <-time.After(2 * time.Second):
		t.Fatal("the release never ran")
	}
	assert.Empty(t, released, "the release ran more than once")
}

// A SyncStep1 whose state vector does not decode is dropped, as it is without
// a hook, and the release still runs.
func TestInteg_OnStateAnswer_ReleaseRunsWhenTheAnswerCannotBeEncoded(t *testing.T) {
	var calls atomic.Int32
	released := make(chan struct{}, 4)
	ts := stateAnswerRoom(t, "the room's state", func(srv *ygws.Server) {
		srv.OnStateAnswer = func(ygws.StateAnswerInfo) (func(), error) {
			calls.Add(1)
			return func() { released <- struct{}{} }, nil
		}
	})

	conn := joinAndReadStep1(t, ts)
	// The state vector is a truncated varuint: ReadSyncMessage reads the
	// message, and EncodeSyncStep2Split cannot read the vector inside it.
	hpSend(t, conn, stateAnswerRoomName, tagSync, encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(ygsync.MsgSyncStep1))
		enc.WriteVarBytes([]byte{0xFF})
	}))

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("the release never ran for an answer that could not be encoded")
	}
	assert.Equal(t, int32(1), calls.Load())

	// The peer stays connected and is answered its next SyncStep1.
	hpSend(t, conn, stateAnswerRoomName, tagSync, ygsync.EncodeSyncStep1(crdt.New()))
	require.NotEmpty(t, readStateAnswer(t, conn))
}

// A refusal sends no state and closes the peer: with the code and reason of a
// *PeerClose, and with 4205 "Reset Connection" for a plain error or a panic.
// The release returned beside the error does not run.
func TestInteg_OnStateAnswer_ARefusalSendsNoAnswerAndClosesThePeer(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		code   int
		reason string
	}{
		"a PeerClose": {
			err:    &ygws.PeerClose{Code: 4403, Reason: "the room is too large to answer now"},
			code:   4403,
			reason: "the room is too large to answer now",
		},
		"a plain error": {err: errors.New("no room for this answer"), code: 4205, reason: "Reset Connection"},
		"a panic":       {code: 4205, reason: "Reset Connection"},
	} {
		t.Run(name, func(t *testing.T) {
			released := make(chan struct{}, 4)
			ts := stateAnswerRoom(t, "the room's state", func(srv *ygws.Server) {
				srv.OnStateAnswer = func(ygws.StateAnswerInfo) (func(), error) {
					if c.err == nil {
						panic("the hook panicked")
					}
					return func() { released <- struct{}{} }, c.err
				}
			})

			conn := joinAndReadStep1(t, ts)
			hpSend(t, conn, stateAnswerRoomName, tagSync, ygsync.EncodeSyncStep1(crdt.New()))

			for {
				tag, _, err := hpRead(conn, stateAnswerRoomName, 2*time.Second)
				if err != nil {
					var closed *gws.CloseError
					require.ErrorAs(t, err, &closed, "the refusal must close the connection")
					assert.Equal(t, c.code, closed.Code)
					assert.Equal(t, c.reason, closed.Text)
					break
				}
				require.NotEqual(t, tagSync, tag, "a refused answer must send no sync message")
			}
			assert.Empty(t, released, "a refused answer runs no release")
		})
	}
}

// A hook that admits the answer leaves it as a server with no hook sends it:
// the same series of message types, leaving the receiver holding the same
// text.
func TestInteg_OnStateAnswer_AnAdmittedAnswerIsTheAnswerWithoutAHook(t *testing.T) {
	text := strings.Repeat("the room's state, long enough to be cut into frames. ", 2000)
	answer := func(configure func(*ygws.Server)) (subs []int, held string) {
		conn := joinAndReadStep1(t, stateAnswerRoom(t, text, configure))
		hpSend(t, conn, stateAnswerRoomName, tagSync, ygsync.EncodeSyncStep1(crdt.New()))
		receiver := crdt.New(crdt.WithClientID(99))
		for _, msg := range readStateAnswer(t, conn) {
			sub, _, err := ygsync.ReadSyncMessage(msg)
			require.NoError(t, err)
			subs = append(subs, sub)
			_, err = ygsync.ApplySyncMessage(receiver, msg, nil)
			require.NoError(t, err)
		}
		return subs, receiver.GetText("t").ToString()
	}

	noneSubs, noneHeld := answer(nil)
	require.Equal(t, text, noneHeld)
	require.Greater(t, len(noneSubs), 1, "the answer must be cut into frames for the comparison to mean anything")

	var calls atomic.Int32
	hookSubs, hookHeld := answer(func(srv *ygws.Server) {
		srv.OnStateAnswer = func(ygws.StateAnswerInfo) (func(), error) {
			calls.Add(1)
			return nil, nil
		}
	})
	assert.Equal(t, noneSubs, hookSubs)
	assert.Equal(t, noneHeld, hookHeld)
	assert.Equal(t, int32(1), calls.Load())
}

// A read-only peer may ask for the room's state, and its answer goes through
// the hook too.
func TestInteg_OnStateAnswer_RunsForReadOnlyPeers(t *testing.T) {
	var calls atomic.Int32
	_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
		srv.Authorize = readOnlyByHeader
		srv.OnStateAnswer = func(ygws.StateAnswerInfo) (func(), error) {
			calls.Add(1)
			return nil, nil
		}
	})
	hpDial(t, ts, stateAnswerRoomName, true, crdt.New())
	assert.Equal(t, int32(1), calls.Load())
}

// parkedStructsUpdate is a V1 update of n one-character structs of client 2 at
// clocks 1 to n, each with the one before it as its left origin. Clock 0 is
// never sent, so a document parks all of them.
func parkedStructsUpdate(n int) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1) // clients
		enc.WriteVarUint(uint64(n))
		enc.WriteVarUint(2) // client
		enc.WriteVarUint(1) // first clock
		for i := range n {
			enc.WriteUint8(4 | 0x80)    // a string, with a left origin
			enc.WriteVarUint(2)         // the origin's client
			enc.WriteVarUint(uint64(i)) // the origin's clock
			enc.WriteVarString("x")
		}
		enc.WriteVarUint(0) // delete set: no clients
	})
}

// Structs counts the parked structs the answer carries as well as the stored
// ones, and StructsNow counts what the room holds when the hook reads it, which
// a hook that waits needs because the answer is built after it returns.
func TestInteg_OnStateAnswer_StructsCountsWhatTheAnswerEncodes(t *testing.T) {
	const parked, arrivedWhileWaiting = 100, 40
	admit := make(chan struct{})
	called := make(chan ygws.StateAnswerInfo, 4)
	now := make(chan int, 4)
	srv, ts := hocuspocusServer(t, func(s *ygws.Server) {
		s.OnStateAnswer = func(info ygws.StateAnswerInfo) (func(), error) {
			called <- info
			<-admit
			now <- info.StructsNow()
			return nil, nil
		}
	})
	require.NoError(t, srv.Apply(context.Background(), stateAnswerRoomName,
		func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
			txt := doc.GetText("t")
			transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "seed", nil) })
		}))
	room := srv.GetDoc(stateAnswerRoomName)
	require.NotNil(t, room)
	require.NoError(t, crdt.ApplyUpdateV1(room, parkedStructsUpdate(parked), nil))
	require.Equal(t, parked, room.PendingStats().Items)
	stored := room.StructCount()

	conn := joinAndReadStep1(t, ts)
	hpSend(t, conn, stateAnswerRoomName, tagSync, ygsync.EncodeSyncStep1(crdt.New()))

	var info ygws.StateAnswerInfo
	select {
	case info = <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("the hook never ran")
	}
	assert.Equal(t, stored+parked, info.Structs)

	require.NoError(t, crdt.ApplyUpdateV1(room, parkedStructsUpdate(parked+arrivedWhileWaiting), nil))
	close(admit)
	select {
	case got := <-now:
		assert.Equal(t, stored+parked+arrivedWhileWaiting, got)
	case <-time.After(2 * time.Second):
		t.Fatal("the hook never read the count again")
	}
	require.NotEmpty(t, readStateAnswer(t, conn))
}

// A release that panics is recovered: the answer it released has been sent,
// and the peer is answered its next SyncStep1.
func TestInteg_OnStateAnswer_APanickingReleaseIsRecovered(t *testing.T) {
	ts := stateAnswerRoom(t, "the room's state", func(srv *ygws.Server) {
		srv.OnStateAnswer = func(ygws.StateAnswerInfo) (func(), error) {
			return func() { panic("the release panicked") }, nil
		}
	})

	conn := joinAndReadStep1(t, ts)
	hpSend(t, conn, stateAnswerRoomName, tagSync, ygsync.EncodeSyncStep1(crdt.New()))
	require.NotEmpty(t, readStateAnswer(t, conn))

	hpSend(t, conn, stateAnswerRoomName, tagSync, ygsync.EncodeSyncStep1(crdt.New()))
	require.NotEmpty(t, readStateAnswer(t, conn))
}
