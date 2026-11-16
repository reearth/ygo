package crdt

import "testing"

// ClientCount is how many clients this replica holds structs from: a client
// counts once one of its writes integrates here, and stays counted when its
// content is deleted and collected. A client that has only deleted wrote no
// struct, so it is not counted, on its own document or on a replica that
// applies its deletion.
func TestUnit_ClientCount_CountsTheClientsThatWrote(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  func(t *testing.T) *Doc
		want int
	}{
		{"a new document", func(*testing.T) *Doc { return New(WithClientID(1)) }, 0},
		{"a document that opened a text and wrote nothing", func(*testing.T) *Doc {
			doc := New(WithClientID(1))
			doc.GetText("t")
			doc.Transact(func(*Transaction) {})
			return doc
		}, 0},
		{"one client", func(*testing.T) *Doc { return clientCountWriter(1, "hello") }, 1},
		{"one client typing a character per transaction", func(*testing.T) *Doc {
			doc := New(WithClientID(1))
			text := doc.GetText("t")
			for i, c := range "hello" {
				doc.Transact(func(txn *Transaction) { text.Insert(txn, i, string(c), nil) })
			}
			return doc
		}, 1},
		{"three clients", func(t *testing.T) *Doc { return clientCountRoom(t, false) }, 3},
		{"three clients, their text deleted and collected", func(t *testing.T) *Doc { return clientCountRoom(t, true) }, 3},
		{"a client that only deleted", func(t *testing.T) *Doc {
			deleter, _, _ := clientCountDeletion(t)
			return deleter
		}, 1},
		{"a replica that applied that client's deletion", func(t *testing.T) *Doc {
			_, state, deletion := clientCountDeletion(t)
			doc := New(WithClientID(7))
			clientCountApply(t, doc, state)
			clientCountApply(t, doc, deletion)
			if got := doc.GetText("t").ToString(); got != "world" {
				t.Fatalf("the replica holds %q, want %q", got, "world")
			}
			return doc
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.doc(t)
			if got := doc.ClientCount(); got != tc.want {
				t.Fatalf("ClientCount() = %d, want %d", got, tc.want)
			}
			if got := len(doc.StateVector()); got != tc.want {
				t.Fatalf("StateVector() holds %d clients, want %d", got, tc.want)
			}
			if allocs := testing.AllocsPerRun(10, func() { doc.ClientCount() }); allocs != 0 {
				t.Fatalf("ClientCount() allocates %v times, want 0", allocs)
			}
		})
	}
}

// A client whose every struct is parked until its dependency arrives is in no
// client's store yet, so ClientCount leaves it out until they integrate.
func TestUnit_ClientCount_LeavesOutAClientWhoseStructsAreParked(t *testing.T) {
	writer := clientCountWriter(1, "hello")
	first := EncodeStateAsUpdateV1(writer, nil)
	peer := New(WithClientID(2))
	clientCountApply(t, peer, first)
	sv := peer.StateVector()
	text := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { text.Insert(txn, 5, "!", nil) })
	second := EncodeStateAsUpdateV1(peer, sv)

	doc := New(WithClientID(7))
	clientCountApply(t, doc, second)
	if n, parked := doc.ClientCount(), doc.PendingStats().Items; n != 0 || parked != 1 {
		t.Fatalf("with its dependency missing, the document counts %d clients and parks %d structs, want 0 and 1", n, parked)
	}
	clientCountApply(t, doc, first)
	if n, parked := doc.ClientCount(), doc.PendingStats().Items; n != 2 || parked != 0 {
		t.Fatalf("with its dependency applied, the document counts %d clients and parks %d structs, want 2 and 0", n, parked)
	}
}

// clientCountWriter is a document in which client typed s into text "t" in one
// transaction.
func clientCountWriter(client ClientID, s string) *Doc {
	doc := New(WithClientID(client))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, s, nil) })
	return doc
}

// clientCountRoom is a document of client 9 that applied the text of clients
// 1, 2 and 3, each typed on its own, and, when deleteAll is set, then deleted
// all of it in a transaction that collected what it deleted.
func clientCountRoom(t *testing.T, deleteAll bool) *Doc {
	t.Helper()
	room := New(WithClientID(9))
	for client := ClientID(1); client <= 3; client++ {
		clientCountApply(t, room, EncodeStateAsUpdateV1(clientCountWriter(client, "abc"), nil))
	}
	if deleteAll {
		text := room.GetText("t")
		n := text.Len()
		room.Transact(func(txn *Transaction) { text.Delete(txn, 0, n) })
		for client, items := range room.store.clients {
			for _, item := range items {
				if _, collected := item.Content.(*ContentDeleted); !collected {
					t.Fatalf("client %d holds %T, want the deleted text collected", client, item.Content)
				}
			}
		}
	}
	return room
}

// clientCountDeletion returns the document of client 2, which applied client
// 1's "hello world" and deleted "hello ", with client 1's state and the update
// that carries the deletion.
func clientCountDeletion(t *testing.T) (deleter *Doc, state, deletion []byte) {
	t.Helper()
	state = EncodeStateAsUpdateV1(clientCountWriter(1, "hello world"), nil)
	deleter = New(WithClientID(2))
	clientCountApply(t, deleter, state)
	sv := deleter.StateVector()
	text := deleter.GetText("t")
	deleter.Transact(func(txn *Transaction) { text.Delete(txn, 0, 6) })
	return deleter, state, EncodeStateAsUpdateV1(deleter, sv)
}

func clientCountApply(t *testing.T, doc *Doc, update []byte) {
	t.Helper()
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatal(err)
	}
}
