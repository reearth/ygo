package crdt

import "testing"

// StructCount is the structs this replica's store holds: one per run of
// content, each piece of a run that an edit split, and each deleted item,
// whether garbage collection has reduced it to a placeholder or not, and none
// of a subdocument's.
func TestUnit_StructCount_CountsTheStructsTheDocumentHolds(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  func(t *testing.T) *Doc
		want int
	}{
		{"a new document", func(*testing.T) *Doc { return New() }, 0},
		{"a run typed in one transaction", func(*testing.T) *Doc { return typed("hello world") }, 1},
		{"a run typed a character per transaction", func(t *testing.T) *Doc { return typedByCharacter(t, "hello world") }, 11},
		{"that run applied an update per transaction", func(t *testing.T) *Doc {
			doc := New(WithClientID(7))
			typedByCharacter(t, "hello world", doc)
			return doc
		}, 11},
		{"that run loaded in one update", func(t *testing.T) *Doc {
			doc := New(WithClientID(7))
			if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(typedByCharacter(t, "hello world"), nil), nil); err != nil {
				t.Fatal(err)
			}
			return doc
		}, 1},
		{"1 item from two clients in turn", func(t *testing.T) *Doc { return alternatingWriters(t, 1) }, 1},
		{"4 items from two clients in turn", func(t *testing.T) *Doc { return alternatingWriters(t, 4) }, 4},
		{"25 items from two clients in turn", func(t *testing.T) *Doc { return alternatingWriters(t, 25) }, 25},
		{"a run a peer's update split", func(t *testing.T) *Doc {
			state := EncodeStateAsUpdateV1(typed("hello world"), nil)
			doc := New(WithClientID(7))
			if err := ApplyUpdateV1(doc, state, nil); err != nil {
				t.Fatal(err)
			}
			peer := New(WithClientID(2))
			if err := ApplyUpdateV1(peer, state, nil); err != nil {
				t.Fatal(err)
			}
			sv := peer.StateVector()
			text := peer.GetText("t")
			peer.Transact(func(txn *Transaction) {
				text.Insert(txn, 0, "a", nil) // "ahello world"
				text.Insert(txn, 4, "b", nil) // "ahelblo world"
				text.Insert(txn, 8, "c", nil) // "ahelblo cworld"
			})
			if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(peer, sv), nil); err != nil {
				t.Fatal(err)
			}
			return doc // a, b, c, and "hel", "lo " and "world"
		}, 6},
		{"a run with its middle deleted", func(*testing.T) *Doc { return deletedMiddle(WithGC(false)) }, 3},
		{"a run with its middle deleted and collected", func(*testing.T) *Doc { return deletedMiddle() }, 3},
		{"a map holding a subdocument with text", func(*testing.T) *Doc {
			doc, sub := New(WithClientID(1)), typed("hello", WithGUID("sub"))
			m := doc.GetMap("m")
			doc.Transact(func(txn *Transaction) { m.Set(txn, "sub", sub) })
			return doc // the map entry; the subdocument's run is its own Doc's
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.doc(t).StructCount(); got != tc.want {
				t.Fatalf("StructCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A struct parked until its dependency arrives is in no client's store yet, so
// StructCount leaves it out until it integrates; PendingStats counts it.
func TestUnit_StructCount_LeavesOutStructsParkedForADependency(t *testing.T) {
	first := EncodeStateAsUpdateV1(typed("hello"), nil)
	peer := New(WithClientID(2))
	if err := ApplyUpdateV1(peer, first, nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	text := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { text.Insert(txn, 5, "!", nil) })
	second := EncodeStateAsUpdateV1(peer, sv)

	doc := New(WithClientID(7))
	if err := ApplyUpdateV1(doc, second, nil); err != nil {
		t.Fatal(err)
	}
	if n, parked := doc.StructCount(), doc.PendingStats().Items; n != 0 || parked != 1 {
		t.Fatalf("with its dependency missing, the document holds %d structs and parks %d, want 0 and 1", n, parked)
	}
	if err := ApplyUpdateV1(doc, first, nil); err != nil {
		t.Fatal(err)
	}
	if n, parked := doc.StructCount(), doc.PendingStats().Items; n != 2 || parked != 0 {
		t.Fatalf("with its dependency applied, the document holds %d structs and parks %d, want 2 and 0", n, parked)
	}
}

// typed is a document in which client 1 typed s into text "t" in one
// transaction.
func typed(s string, opts ...DocOption) *Doc {
	doc := New(append([]DocOption{WithClientID(1)}, opts...)...)
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, s, nil) })
	return doc
}

// typedByCharacter is a document in which client 1 typed s into text "t" one
// character per transaction, in order, applying each transaction's update to
// each of replicas as it went.
func typedByCharacter(t *testing.T, s string, replicas ...*Doc) *Doc {
	t.Helper()
	doc := New(WithClientID(1))
	text := doc.GetText("t")
	for i, c := range s {
		sv := doc.StateVector()
		doc.Transact(func(txn *Transaction) { text.Insert(txn, i, string(c), nil) })
		for _, r := range replicas {
			if err := ApplyUpdateV1(r, EncodeStateAsUpdateV1(doc, sv), nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	return doc
}

// deletedMiddle is a document whose text "hello world" had "lo " deleted, which
// splits the run into "hel", the deleted "lo " and "world".
func deletedMiddle(opts ...DocOption) *Doc {
	doc := typed("hello world", opts...)
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Delete(txn, 3, 3) })
	return doc
}

// alternatingWriters is a document whose text is l one-character items from
// clients 5 and 6 in turn, each typed at the end after the other's, so no two
// neighbours merge.
func alternatingWriters(t *testing.T, l int) *Doc {
	t.Helper()
	a, b := New(WithClientID(5)), New(WithClientID(6))
	at, bt := a.GetText("t"), b.GetText("t")
	for i := range l {
		from, to, text := a, b, at
		if i%2 == 1 {
			from, to, text = b, a, bt
		}
		sv := to.StateVector()
		from.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "c", nil) })
		if err := ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, sv), nil); err != nil {
			t.Fatal(err)
		}
	}
	return a
}
