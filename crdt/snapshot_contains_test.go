package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_SnapshotContainsUpdateV1(t *testing.T) {
	testContainsUpdateV1(t, func(doc *Doc, update []byte) (bool, error) {
		return SnapshotContainsUpdateV1(CaptureSnapshot(doc), update)
	})
}

func TestUnit_DocContainsUpdateV1(t *testing.T) {
	testContainsUpdateV1(t, DocContainsUpdateV1)
}

// testContainsUpdateV1 checks contains, one of SnapshotContainsUpdateV1 over a
// fresh snapshot or DocContainsUpdateV1, which must answer alike.
func testContainsUpdateV1(t *testing.T, contains func(*Doc, []byte) (bool, error)) {
	t.Helper()
	// source holds "hello" with its first character deleted; each case's
	// update is checked against a snapshot of source.
	source := newTestDoc(1)
	txt := source.GetText("t")
	source.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })
	inserted := EncodeStateAsUpdateV1(source, nil)
	source.Transact(func(txn *Transaction) { txt.Delete(txn, 0, 1) })
	whole := EncodeStateAsUpdateV1(source, nil)

	// The deletion of a character source still shows, made by another client.
	deleter := newTestDoc(2)
	require.NoError(t, ApplyUpdateV1(deleter, whole, nil))
	before := deleter.StateVector()
	deleterText := deleter.GetText("t")
	deleter.Transact(func(txn *Transaction) { deleterText.Delete(txn, 0, 1) })
	newDeletion := EncodeStateAsUpdateV1(deleter, before)

	// An insertion by another client.
	writer := newTestDoc(3)
	require.NoError(t, ApplyUpdateV1(writer, whole, nil))
	before = writer.StateVector()
	writerText := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { writerText.Insert(txn, 0, "x", nil) })
	newInsertion := EncodeStateAsUpdateV1(writer, before)

	// A deletion range that spans a deleted struct and a live one.
	spanner := newTestDoc(4)
	require.NoError(t, ApplyUpdateV1(spanner, whole, nil))
	before = spanner.StateVector()
	spannerText := spanner.GetText("t")
	spanner.Transact(func(txn *Transaction) { spannerText.Delete(txn, 0, 2) })
	spanningDeletion := EncodeStateAsUpdateV1(spanner, before)

	tests := []struct {
		name   string
		update []byte
		want   bool
	}{
		{"the whole state, deletions included", whole, true},
		{"an older state the deletion since covers", inserted, true},
		{"an empty update", EncodeStateAsUpdateV1(New(), nil), true},
		{"a deletion the snapshot lacks", newDeletion, false},
		{"a deletion partly held", spanningDeletion, false},
		{"an insertion the snapshot lacks", newInsertion, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := contains(source, tc.update)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("an update that does not decode", func(t *testing.T) {
		_, err := contains(source, []byte{0x01, 0x05})
		require.ErrorIs(t, err, ErrInvalidUpdate)
	})
}

// BenchmarkContainsUpdateV1_LargeDeleteSet measures the read-only SyncStep2
// check against a document with 20k deletions: for an empty update, for the
// delete set alone (what a reconnecting viewer that holds the document sends:
// Yjs always writes its whole delete set), and for the whole state.
func BenchmarkContainsUpdateV1_LargeDeleteSet(b *testing.B) {
	doc := New(WithClientID(1))
	txt := doc.GetText("t")
	for range 20000 {
		doc.Transact(func(txn *Transaction) { txt.Insert(txn, txt.Len(), "ab", nil) })
	}
	for i := range 20000 {
		doc.Transact(func(txn *Transaction) { txt.Delete(txn, i, 1) })
	}
	empty := EncodeStateAsUpdateV1(New(), nil)
	deletes := EncodeStateAsUpdateV1(doc, doc.StateVector())
	whole := EncodeStateAsUpdateV1(doc, nil)
	for _, bc := range []struct {
		name   string
		update []byte
	}{{"empty", empty}, {"deletes", deletes}, {"whole", whole}} {
		b.Run("snapshot/"+bc.name, func(b *testing.B) {
			for range b.N {
				_, _ = SnapshotContainsUpdateV1(CaptureSnapshot(doc), bc.update)
			}
		})
		b.Run("doc/"+bc.name, func(b *testing.B) {
			for range b.N {
				_, _ = DocContainsUpdateV1(doc, bc.update)
			}
		})
	}
}
