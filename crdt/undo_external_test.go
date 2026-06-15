package crdt_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// F-8a (#124): NewUndoManager must be constructible from outside the crdt
// package. Before SharedType was exported, the scope parameter took an
// unexported interface, so []crdt.SharedType{...} could not be named by an
// external caller and this file would not compile at all.
func TestUnit_NewUndoManager_UsableFromExternalPackage(t *testing.T) {
	d := crdt.New(crdt.WithClientID(1))
	txt := d.GetText("t")
	arr := d.GetArray("a")
	m := d.GetMap("m")

	um := crdt.NewUndoManager(d, []crdt.SharedType{txt, arr, m})
	require.NotNil(t, um)

	d.Transact(func(txn *crdt.Transaction) { txt.Insert(txn, 0, "x", nil) })
	d.Transact(func(txn *crdt.Transaction) { txt.Delete(txn, 0, 1) })
	require.True(t, um.Undo())
	require.Equal(t, "x", txt.ToString(), "undo restores via the external UndoManager")
}
