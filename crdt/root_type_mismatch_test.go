package crdt

import (
	"errors"
	"testing"
	"time"
)

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
					recovered := recover()
					if recovered == nil {
						t.Fatal("accessing a map root as a different type should panic")
					}
					if err, ok := recovered.(error); !ok || !errors.Is(err, ErrRootTypeMismatch) {
						t.Fatalf("panic = %v, want ErrRootTypeMismatch", recovered)
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

func TestRootTypeMismatchPanicsForEveryRootKind(t *testing.T) {
	tests := []struct {
		name       string
		createRoot func(*Doc)
		access     func(*Doc)
	}{
		{
			name:       "text as map",
			createRoot: func(doc *Doc) { doc.GetText("root") },
			access:     func(doc *Doc) { doc.GetMap("root") },
		},
		{
			name:       "xml fragment as text",
			createRoot: func(doc *Doc) { doc.GetXmlFragment("root") },
			access:     func(doc *Doc) { doc.GetText("root") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := New()
			tt.createRoot(doc)
			defer func() {
				recovered := recover()
				if err, ok := recovered.(error); !ok || !errors.Is(err, ErrRootTypeMismatch) {
					t.Fatalf("panic = %v, want ErrRootTypeMismatch", recovered)
				}
			}()
			tt.access(doc)
		})
	}
}

func TestTransactionRootTypeMismatchPanicsWithoutReplacingExistingRoot(t *testing.T) {
	mismatchedAccessors := []struct {
		name   string
		access func(*Transaction)
	}{
		{name: "array", access: func(txn *Transaction) { txn.GetArray("root") }},
		{name: "text", access: func(txn *Transaction) { txn.GetText("root") }},
		{name: "xml fragment", access: func(txn *Transaction) { txn.GetXmlFragment("root") }},
	}

	for _, test := range mismatchedAccessors {
		t.Run(test.name, func(t *testing.T) {
			doc := New()
			doc.Transact(func(txn *Transaction) {
				txn.GetMap("root").Set(txn, "key", "value")
			})

			panicked := func() (panicked bool) {
				defer func() { panicked = recover() != nil }()
				doc.Transact(func(txn *Transaction) { test.access(txn) })
				return false
			}()
			if !panicked {
				t.Fatal("accessing a map root as a different type should panic")
			}

			if got, ok := doc.GetMap("root").Get("key"); !ok || got != "value" {
				t.Fatalf("existing map root was changed: got (%v, %v), want (value, true)", got, ok)
			}
		})
	}
}

func TestRootTypeMismatchPanicsAfterRawRootUpgrade(t *testing.T) {
	source := New()
	source.Transact(func(txn *Transaction) {
		txn.GetMap("root").Set(txn, "key", "value")
	})

	doc := New()
	if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(source, nil), nil); err != nil {
		t.Fatalf("apply remote update: %v", err)
	}
	// The remote update leaves a raw placeholder until this access upgrades it.
	root := doc.GetMap("root")
	if got, ok := root.Get("key"); !ok || got != "value" {
		t.Fatalf("upgraded map root lost its contents: got (%v, %v), want (value, true)", got, ok)
	}

	panicked := func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		doc.GetText("root")
		return false
	}()
	if !panicked {
		t.Fatal("accessing an upgraded map root as text should panic")
	}
	if got, ok := root.Get("key"); !ok || got != "value" {
		t.Fatalf("mismatched access changed the upgraded root: got (%v, %v), want (value, true)", got, ok)
	}
}

func TestDocUsableAfterRecoveredRootTypeMismatchPanic(t *testing.T) {
	doc := New()
	doc.GetMap("root")
	done := make(chan bool, 1)

	go func() {
		panicked := func() (panicked bool) {
			defer func() { panicked = recover() != nil }()
			doc.Transact(func(txn *Transaction) { txn.GetText("root") })
			return false
		}()
		if !panicked {
			done <- false
			return
		}

		doc.Transact(func(txn *Transaction) {
			txn.GetMap("root").Set(txn, "after", "panic")
		})
		done <- len(doc.EncodeStateAsUpdate()) > 0
	}()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("document was not usable after recovering from the panic")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("document operations hung after recovering from the panic")
	}
}
