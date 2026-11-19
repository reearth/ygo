package websocket_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"
)

// Under AnswerOnceStored, a frame whose apply changed nothing is answered 1
// as soon as the answers before it are out, even when what it carries is in
// the room only because an earlier frame's commit was NOT stored: the edit is
// then answered applied without ever being stored.
func TestAudit_AnswerOnceStored_AHeldFrameIsAnsweredAppliedThoughItsContentWasNotStored(t *testing.T) {
	for name, second := range map[string]func(editorDoc *crdt.Doc, first []byte) []byte{
		// The same Update again, in flight behind the first.
		"the update resent": func(_ *crdt.Doc, first []byte) []byte { return ygsync.EncodeUpdate(first) },
		// The editor's SyncStep2 answering the room's handshake SyncStep1
		// (the room's state vector before the first frame landed), which
		// carries the same edit.
		"a SyncStep2 written before the first frame landed": func(editorDoc *crdt.Doc, _ []byte) []byte {
			step2, err := ygsync.EncodeSyncStep2(editorDoc, ygsync.EncodeSyncStep1(crdt.New()))
			require.NoError(t, err)
			return step2
		},
	} {
		t.Run(name, func(t *testing.T) {
			gate := newGatedStore()
			srv, ts := storeServer(t, storedGate{gate}, gate, answerOnceStored)
			editorDoc := crdt.New(crdt.WithClientID(7))
			editor := hpDial(t, ts, "room", false, editorDoc)
			first := textUpdate(editorDoc, "a")

			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(first))
			gate.waitStore(t)
			hpSend(t, editor, "room", tagSync, second(editorDoc, first))
			assert.Empty(t, syncStatuses(t, editor, "room"), "both answers wait on the first frame's store")

			gate.release <- storeVerdict{stored: false} // the adapter did not store the edit
			flags := statusesFor(t, editor, 2)
			assert.Equal(t, "a", srv.GetDoc("room").GetText("t").ToString(), "the edit stands in the room, unstored")

			// And the edit sent again after the 0 is answered at once, with no store call.
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(first))
			again := statusesFor(t, editor, 1)
			select {
			case u := <-gate.started:
				t.Logf("a store call began for %d bytes", len(u))
			case <-time.After(300 * time.Millisecond):
				t.Logf("no store call began for the resend")
			}
			t.Logf("answers: %v then %v", flags, again)
			assert.Equal(t, []uint64{0, 0}, flags, "a frame carrying only an unstored edit must not be answered applied")
			assert.Equal(t, []uint64{0}, again, "the resend of an unstored edit must not be answered applied")
		})
	}
}
