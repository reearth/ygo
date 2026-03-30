package sync_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/sync"
)

// helpers

func newDoc(clientID crdt.ClientID, text string) *crdt.Doc {
	doc := crdt.New(crdt.WithClientID(clientID))
	if text != "" {
		txt := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) {
			txt.Insert(txn, 0, text, nil)
		})
	}
	return doc
}

// ── Unit tests ────────────────────────────────────────────────────────────────

func TestUnit_EncodeSyncStep1_DecodesStateVector(t *testing.T) {
	doc := newDoc(1, "hello")

	msg := sync.EncodeSyncStep1(doc)

	require.NotEmpty(t, msg)
	assert.Equal(t, byte(sync.MsgSyncStep1), msg[0])
}

func TestUnit_EncodeSyncStep2_ContainsMissingUpdate(t *testing.T) {
	docA := newDoc(1, "hello")
	docB := newDoc(2, "") // empty — missing everything from A

	step1 := sync.EncodeSyncStep1(docB)
	step2, err := sync.EncodeSyncStep2(docA, step1)
	require.NoError(t, err)
	require.NotEmpty(t, step2)
	assert.Equal(t, byte(sync.MsgSyncStep2), step2[0])
}

func TestUnit_EncodeUpdate_HasCorrectType(t *testing.T) {
	doc := newDoc(1, "hi")
	raw := crdt.EncodeStateAsUpdateV1(doc, nil)

	msg := sync.EncodeUpdate(raw)
	assert.Equal(t, byte(sync.MsgUpdate), msg[0])
}

func TestUnit_ApplySyncMessage_UnknownType_ReturnsError(t *testing.T) {
	doc := newDoc(1, "")
	_, err := sync.ApplySyncMessage(doc, []byte{99}, nil)
	require.ErrorIs(t, err, sync.ErrUnknownMessage)
}

func TestUnit_ApplySyncMessage_EmptyMsg_ReturnsError(t *testing.T) {
	doc := newDoc(1, "")
	_, err := sync.ApplySyncMessage(doc, []byte{}, nil)
	require.ErrorIs(t, err, sync.ErrUnexpectedEOF)
}

// ── Integration tests ─────────────────────────────────────────────────────────

func TestInteg_TwoPeer_FullHandshake(t *testing.T) {
	docA := newDoc(1, "Alice")
	docB := newDoc(2, "Bob")

	// Step 1: B sends its state vector to A.
	step1B := sync.EncodeSyncStep1(docB)

	// Step 2: A replies with what B is missing.
	step2A, err := sync.EncodeSyncStep2(docA, step1B)
	require.NoError(t, err)

	// B applies A's updates.
	reply, err := sync.ApplySyncMessage(docB, step2A, nil)
	require.NoError(t, err)
	assert.Nil(t, reply)

	// Step 1: A sends its state vector to B.
	step1A := sync.EncodeSyncStep1(docA)

	// Step 2: B replies with what A is missing.
	step2B, err := sync.EncodeSyncStep2(docB, step1A)
	require.NoError(t, err)

	// A applies B's updates.
	reply, err = sync.ApplySyncMessage(docA, step2B, nil)
	require.NoError(t, err)
	assert.Nil(t, reply)

	// Both peers converge.
	assert.Equal(t, docA.GetText("t").ToString(), docB.GetText("t").ToString())
}

func TestInteg_ApplySyncMessage_Step1_ReturnsStep2Reply(t *testing.T) {
	docA := newDoc(1, "hello")
	docB := newDoc(2, "")

	step1 := sync.EncodeSyncStep1(docB)

	// Passing step-1 to ApplySyncMessage should return a step-2 reply.
	reply, err := sync.ApplySyncMessage(docA, step1, nil)
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, byte(sync.MsgSyncStep2), reply[0])
}

func TestInteg_IncrementalUpdate_Broadcast(t *testing.T) {
	docA := newDoc(1, "hello")
	docB := newDoc(2, "")

	// Initial sync: B catches up to A.
	step1B := sync.EncodeSyncStep1(docB)
	step2A, err := sync.EncodeSyncStep2(docA, step1B)
	require.NoError(t, err)
	_, err = sync.ApplySyncMessage(docB, step2A, nil)
	require.NoError(t, err)
	assert.Equal(t, "hello", docB.GetText("t").ToString())

	// A makes a new change and broadcasts it as an update message.
	svBefore := docA.StateVector()
	txt := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) {
		txt.Insert(txn, 5, " world", nil)
	})
	diff := crdt.EncodeStateAsUpdateV1(docA, svBefore)
	updateMsg := sync.EncodeUpdate(diff)

	// B applies the incremental update.
	_, err = sync.ApplySyncMessage(docB, updateMsg, nil)
	require.NoError(t, err)
	assert.Equal(t, "hello world", docB.GetText("t").ToString())
}

func TestInteg_AlreadySynced_Step2IsEmpty(t *testing.T) {
	docA := newDoc(1, "hello")
	docB := newDoc(2, "")

	// Fully sync B to A.
	step1B := sync.EncodeSyncStep1(docB)
	step2A, err := sync.EncodeSyncStep2(docA, step1B)
	require.NoError(t, err)
	_, err = sync.ApplySyncMessage(docB, step2A, nil)
	require.NoError(t, err)

	// Now B sends step-1 again — A has nothing new to offer.
	step1B2 := sync.EncodeSyncStep1(docB)
	step2A2, err := sync.EncodeSyncStep2(docA, step1B2)
	require.NoError(t, err)

	// Apply the empty step-2 — must be a no-op.
	_, err = sync.ApplySyncMessage(docA, step2A2, nil)
	require.NoError(t, err)
	assert.Equal(t, "hello", docA.GetText("t").ToString())
}

func TestInteg_Origin_PassedThrough(t *testing.T) {
	docA := newDoc(1, "hello")
	docB := newDoc(2, "")

	step1 := sync.EncodeSyncStep1(docB)
	step2, err := sync.EncodeSyncStep2(docA, step1)
	require.NoError(t, err)

	// Pass a custom origin value — should not cause errors.
	_, err = sync.ApplySyncMessage(docB, step2, "connection-42")
	require.NoError(t, err)
	assert.Equal(t, "hello", docB.GetText("t").ToString())
}

func TestInteg_Sync_ThreePeers_Convergence(t *testing.T) {
	// Three peers each start with independent content, then fully sync via
	// pairwise bidirectional step-1/step-2 exchanges. All three must converge.
	docA := newDoc(1, "Alice")
	docB := newDoc(2, "Bob")
	docC := newDoc(3, "Carol")

	// bidirectionalSync exchanges step-1/step-2 in both directions between
	// two documents, leaving both peers with each other's content.
	bidirectionalSync := func(d1, d2 *crdt.Doc) {
		t.Helper()
		s1 := sync.EncodeSyncStep1(d1)
		s2, err := sync.EncodeSyncStep2(d2, s1)
		require.NoError(t, err)
		_, err = sync.ApplySyncMessage(d1, s2, nil)
		require.NoError(t, err)

		s1b := sync.EncodeSyncStep1(d2)
		s2b, err := sync.EncodeSyncStep2(d1, s1b)
		require.NoError(t, err)
		_, err = sync.ApplySyncMessage(d2, s2b, nil)
		require.NoError(t, err)
	}

	// Sync A↔B, then A↔C, then B↔C (one round is enough for pairwise exchange).
	bidirectionalSync(docA, docB)
	bidirectionalSync(docA, docC)
	bidirectionalSync(docB, docC)

	textA := docA.GetText("t").ToString()
	textB := docB.GetText("t").ToString()
	textC := docC.GetText("t").ToString()

	assert.Equal(t, textA, textB, "peers A and B must converge")
	assert.Equal(t, textB, textC, "peers B and C must converge")
	assert.NotEmpty(t, textA, "converged content must be non-empty")
}

// ── Fuzz ──────────────────────────────────────────────────────────────────────

// FuzzApplySyncMessage verifies that no arbitrary byte sequence causes a panic
// in the sync message handler.
func FuzzApplySyncMessage(f *testing.F) {
	// Seed with well-formed messages of each type.
	doc := crdt.New()
	f.Add(sync.EncodeSyncStep1(doc))
	step1 := sync.EncodeSyncStep1(crdt.New())
	if step2, err := sync.EncodeSyncStep2(doc, step1); err == nil {
		f.Add(step2)
	}
	raw := crdt.EncodeStateAsUpdateV1(doc, nil)
	f.Add(sync.EncodeUpdate(raw))
	// Seed with edge cases.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		target := crdt.New()
		// Must never panic regardless of input.
		_, _ = sync.ApplySyncMessage(target, data, nil)
	})
}
