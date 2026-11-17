package http_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// A POST whose update stands while the room's apply check refuses only the
// drain of what another client parked is answered 204, as an applied update
// is, and the room holds the update without the parked content; a POST whose
// update the check refuses is answered 400 and changes nothing.
func TestUnit_POST_ApplyCheck_AnUpdateWhoseDrainIsRefusedIsAnsweredApplied(t *testing.T) {
	honest := crdt.New(crdt.WithClientID(5))
	text := honest.GetText("t")
	typed := func(s string) []byte {
		sv := honest.StateVector()
		honest.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), s, nil) })
		return crdt.EncodeStateAsUpdateV1(honest, sv)
	}
	first := typed("h")
	attacker := crdt.New(crdt.WithClientID(4))
	require.NoError(t, crdt.ApplyUpdateV1(attacker, first, nil))
	sv := attacker.StateVector()
	evilText := attacker.GetText("t")
	attacker.Transact(func(txn *crdt.Transaction) { evilText.Insert(txn, evilText.Len(), "EVIL", nil) })
	evil := crdt.EncodeStateAsUpdateV1(attacker, sv)
	bad := typed("BAD")

	srv := newTestServer()
	require.Equal(t, http.StatusNoContent, doPOST(t, srv, "room", evil).Code, "EVIL parks")
	doc := srv.GetDoc("room")
	require.NotNil(t, doc)
	room := doc.GetText("t")
	doc.SetApplyCheck(func(_ any, v *crdt.ApplyView) error {
		if s := v.TextString(room); strings.Contains(s, "EVIL") || strings.Contains(s, "BAD") {
			return errors.New("the room's check refuses it")
		}
		return nil
	})

	assert.Equal(t, http.StatusNoContent, doPOST(t, srv, "room", first).Code, "the stood update is answered as applied")
	assert.Equal(t, "h", room.ToString(), "the room holds the update and nothing the drain landed")
	assert.Equal(t, 1, doc.PendingStats().Items, "EVIL is parked again")

	before := crdt.EncodeStateAsUpdateV1(doc, nil)
	assert.Equal(t, http.StatusBadRequest, doPOST(t, srv, "room", bad).Code, "the refused update is answered 400")
	assert.Equal(t, before, crdt.EncodeStateAsUpdateV1(doc, nil), "the refused update changed the room")
}
