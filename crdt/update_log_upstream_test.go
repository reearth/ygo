package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The base is what EncodeStateAsUpdateV1(d, nil) returns just before the
// first commit, parked structs included: a replica built from the base takes
// the missing dependency and then holds what the document holds.
func TestUnit_UpdateLog_BaseIncludesParkedStructs(t *testing.T) {
	remote := newTestDoc(2)
	rt := remote.GetText("t")
	remote.Transact(func(txn *Transaction) { rt.Insert(txn, 0, "ab", nil) })
	first := EncodeStateAsUpdateV1(remote, nil)
	sv := remote.StateVector()
	remote.Transact(func(txn *Transaction) { rt.Insert(txn, 2, "cd", nil) })
	second := EncodeStateAsUpdateV1(remote, sv)

	doc := newTestDoc(1)
	require.NoError(t, ApplyUpdateV1(doc, second, nil))
	require.Positive(t, doc.PendingStats().Items, "the second update parks without the first")

	log := doc.OpenUpdateLog()
	defer log.Close()
	before := EncodeStateAsUpdateV1(doc, nil)
	doc.Transact(func(txn *Transaction) { txn.GetMap("m").Set(txn, "k", "v") }, "local")
	require.NoError(t, ApplyUpdateV1(doc, first, "first"))
	require.Equal(t, "abcd", doc.GetText("t").ToString())

	base, updates := log.Drain()
	assert.Equal(t, before, base)
	assert.Equal(t, []any{"local", "first"}, loggedOrigins(updates))
	assert.Positive(t, replay(t, base, nil).PendingStats().Items, "the base carries the parked structs")
	replica := replay(t, base, updates)
	assert.Equal(t, "abcd", replica.GetText("t").ToString())
	assert.Equal(t, doc.StateVector(), replica.StateVector())
}

// A remote update's format cleanup commits as a transaction of its own after
// the remote one's observers, and is logged like one. A log opened from the
// remote transaction's observer takes its base from the cleanup, the first
// commit after it opened.
func TestUnit_UpdateLog_FormatCleanupIsLogged(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })

	whole := b.OpenUpdateLog()
	defer whole.Close()
	b.Transact(func(*Transaction) {}) // records whole's base
	_, _ = whole.Drain()
	var opened *UpdateLog
	var afterRemote []byte
	b.OnUpdate(func(_ []byte, origin any) {
		if origin == "remote" && opened == nil {
			opened = b.OpenUpdateLog()
			afterRemote = EncodeStateAsUpdateV1(b, nil)
		}
	})
	syncText(t, a, b, "remote")
	require.NotNil(t, opened)
	defer opened.Close()
	require.Equal(t, 1, countLiveContentFormat(b), "the cleanup ran")

	base, updates := whole.Drain()
	assert.Nil(t, base, "the base was handed over before")
	require.Equal(t, []any{"remote", nil}, loggedOrigins(updates), "the remote commit, then its cleanup")

	base, updates = opened.Drain()
	assert.Equal(t, afterRemote, base, "the base is the state just before the cleanup")
	require.Equal(t, []any{nil}, loggedOrigins(updates))
	replica := replay(t, base, updates)
	assert.Equal(t, bt.ToDelta(), replica.GetText("t").ToDelta())
	assert.Equal(t, 1, countLiveContentFormat(replica))
}
