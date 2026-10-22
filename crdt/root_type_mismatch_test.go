package crdt

import "testing"

func TestRootTypeMismatchPanicsWithoutReplacingExistingRoot(t *testing.T) {
	mismatchedAccessors := []struct {
		name   string
		access func(*Doc)
	}{
		{name: "array", access: func(doc *Doc) { doc.GetArray("root") }},
		{name: "text", access: func(doc *Doc) { doc.GetText("root") }},
		{name: "xml fragment", access: func(doc *Doc) { doc.GetXmlFragment("root") }},
	}

	for _, test := range mismatchedAccessors {
		t.Run(test.name, func(t *testing.T) {
			doc := New()
			root := doc.GetMap("root")
			doc.Transact(func(txn *Transaction) {
				root.Set(txn, "key", "value")
			})

			func() {
				defer func() {
					if recovered := recover(); recovered == nil {
						t.Fatal("accessing a map root as a different type should panic")
					}
				}()
				test.access(doc)
			}()

			if got, ok := doc.GetMap("root").Get("key"); !ok || got != "value" {
				t.Fatalf("existing map root was changed: got (%v, %v), want (value, true)", got, ok)
			}
		})
	}
}
