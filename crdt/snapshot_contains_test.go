package crdt

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnit_SnapshotContainsUpdateV1(t *testing.T) {
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

	tests := []struct {
		name   string
		update []byte
		want   bool
	}{
		{"the whole state, deletions included", whole, true},
		{"an older state the deletion since covers", inserted, true},
		{"an empty update", EncodeStateAsUpdateV1(New(), nil), true},
		{"a deletion the snapshot lacks", newDeletion, false},
		{"an insertion the snapshot lacks", newInsertion, false},
	}
	snap := CaptureSnapshot(source)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SnapshotContainsUpdateV1(snap, tc.update)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("an update that does not decode", func(t *testing.T) {
		_, err := SnapshotContainsUpdateV1(snap, []byte{0x01, 0x05})
		require.ErrorIs(t, err, ErrInvalidUpdate)
	})
}
