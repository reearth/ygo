package mobile

import (
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func docWithMapRoot() *Doc {
	doc := NewDoc()
	doc.d.Transact(func(txn *crdt.Transaction) {
		txn.GetMap("root").Set(txn, "existing", "value")
	})
	return doc
}

func docWithTextRoot() *Doc {
	doc := NewDoc()
	doc.d.Transact(func(txn *crdt.Transaction) {
		txn.GetText("root").Insert(txn, 0, "seed", nil)
	})
	return doc
}

func assertMapRootPreserved(t *testing.T, doc *Doc) {
	t.Helper()
	got, err := doc.GetMapJSON("root")
	if err != nil {
		t.Fatalf("GetMapJSON after recovered mismatch: %v", err)
	}
	if !jsonEqual(t, got, []byte(`{"existing":"value"}`)) {
		t.Fatalf("map root after recovered mismatch = %s", got)
	}
}

func assertTextRootPreserved(t *testing.T, doc *Doc) {
	t.Helper()
	if got := doc.GetText("root"); got != "seed" {
		t.Fatalf("text root after recovered mismatch = %q, want seed", got)
	}
}

func TestUnit_Mobile_RootTypeMismatchReturnsErrors(t *testing.T) {
	tests := []struct {
		name   string
		newDoc func() *Doc
		call   func(*Doc) error
		verify func(*testing.T, *Doc)
	}{
		{
			name:   "GetTextJSON",
			newDoc: docWithMapRoot,
			call: func(doc *Doc) error {
				_, err := doc.GetTextJSON("root")
				return err
			},
			verify: assertMapRootPreserved,
		},
		{
			name:   "GetMapJSON",
			newDoc: docWithTextRoot,
			call: func(doc *Doc) error {
				_, err := doc.GetMapJSON("root")
				return err
			},
			verify: assertTextRootPreserved,
		},
		{
			name:   "GetArrayJSON",
			newDoc: docWithMapRoot,
			call: func(doc *Doc) error {
				_, err := doc.GetArrayJSON("root")
				return err
			},
			verify: assertMapRootPreserved,
		},
		{name: "InsertText", newDoc: docWithMapRoot, call: func(doc *Doc) error { return doc.InsertText("root", 0, "x") }, verify: assertMapRootPreserved},
		{name: "InsertTextWithAttributes", newDoc: docWithMapRoot, call: func(doc *Doc) error { return doc.InsertTextWithAttributes("root", 0, "x", nil) }, verify: assertMapRootPreserved},
		{name: "DeleteText", newDoc: docWithMapRoot, call: func(doc *Doc) error { return doc.DeleteText("root", 0, 0) }, verify: assertMapRootPreserved},
		{name: "FormatText", newDoc: docWithMapRoot, call: func(doc *Doc) error { return doc.FormatText("root", 0, 0, nil) }, verify: assertMapRootPreserved},
		{name: "InsertArray", newDoc: docWithMapRoot, call: func(doc *Doc) error { return doc.InsertArray("root", 0, []byte(`[]`)) }, verify: assertMapRootPreserved},
		{name: "DeleteArray", newDoc: docWithMapRoot, call: func(doc *Doc) error { return doc.DeleteArray("root", 0, 0) }, verify: assertMapRootPreserved},
		{name: "SetMap", newDoc: docWithTextRoot, call: func(doc *Doc) error { return doc.SetMap("root", "key", []byte(`null`)) }, verify: assertTextRootPreserved},
		{name: "DeleteMapKey", newDoc: docWithTextRoot, call: func(doc *Doc) error { return doc.DeleteMapKey("root", "key") }, verify: assertTextRootPreserved},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := tt.newDoc()
			err := tt.call(doc)
			if err == nil {
				t.Fatal("expected root type mismatch error")
			}
			if !strings.Contains(err.Error(), `crdt: root "root" has type`) || !strings.Contains(err.Error(), "cannot access it as") {
				t.Fatalf("unexpected mismatch error: %v", err)
			}
			tt.verify(t, doc)
		})
	}
}

func TestUnit_Mobile_GetTextReturnsEmptyForRootTypeMismatch(t *testing.T) {
	doc := docWithMapRoot()
	if got := doc.GetText("root"); got != "" {
		t.Fatalf("GetText with mismatched root = %q, want empty string", got)
	}
	assertMapRootPreserved(t, doc)
}
