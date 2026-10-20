package updatecheck_test

import (
	"strconv"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/internal/updatecheck"
)

func TestUnit_ValidateV1(t *testing.T) {
	// Client 2 sets 100,001 keys, one more than crdt's default pending cap, on a
	// map client 1 created. Decoded without client 1's update, all of them park.
	author := crdt.New(crdt.WithClientID(1))
	root := author.GetMap("m")
	author.Transact(func(txn *crdt.Transaction) { root.Set(txn, "nested", crdt.NewMapPrelim()) })
	base := crdt.EncodeStateAsUpdateV1(author, nil)
	editor := crdt.New(crdt.WithClientID(2))
	if err := crdt.ApplyUpdateV1(editor, base, nil); err != nil {
		t.Fatalf("ApplyUpdateV1(base): %v", err)
	}
	v, _ := editor.GetMap("m").Get("nested")
	nested := v.(*crdt.YMap)
	editor.Transact(func(txn *crdt.Transaction) {
		for i := range 100_001 {
			nested.Set(txn, "k"+strconv.Itoa(i), i)
		}
	})
	dependent := crdt.EncodeStateAsUpdateV1(editor, author.StateVector())

	tests := []struct {
		name    string
		update  []byte
		wantErr bool
	}{
		{name: "full state", update: base},
		{name: "incremental update parking 100,001 items", update: dependent},
		{name: "bytes that do not decode", update: []byte{0xff, 0xff, 0xff}, wantErr: true},
		{name: "truncated update", update: dependent[:len(dependent)/2], wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := updatecheck.ValidateV1(tc.update)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateV1() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}
