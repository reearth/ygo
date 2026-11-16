package crdt

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// gcNeighbourFormat applies the V1 test inputs in one wire format.
type gcNeighbourFormat struct {
	name    string
	convert func([]byte) ([]byte, error)
	apply   func(*Doc, []byte, any) error
	encode  func(*Doc, StateVector) []byte
	merge   func(...[]byte) ([]byte, error)
	diff    func([]byte, StateVector) ([]byte, error)
}

var gcNeighbourFormats = []gcNeighbourFormat{
	{"V1", func(u []byte) ([]byte, error) { return u, nil }, ApplyUpdateV1, EncodeStateAsUpdateV1, MergeUpdatesV1, DiffUpdateV1},
	{"V2", UpdateV1ToV2, ApplyUpdateV2, EncodeStateAsUpdateV2, MergeUpdatesV2, DiffUpdateV2},
}

func (f gcNeighbourFormat) applyV1(t *testing.T, doc *Doc, v1 []byte) {
	t.Helper()
	u, err := f.convert(v1)
	require.NoError(t, err)
	require.NoError(t, f.apply(doc, u, nil))
}

// gcOverZZ is one GC struct over client 5's clocks 5-6, the "ZZ" of gcNeighbourText.
var gcOverZZ = []byte{1, 1, 5, 5, 0, 2, 0}

// gcNeighbourPositions are the indexes of client 7's "b" in "heZZllo" whose
// origin, right origin or both are in "ZZ".
var gcNeighbourPositions = []struct {
	name string
	pos  int
}{
	{"right origin in ZZ", 2},
	{"both origins in ZZ", 3},
	{"origin in ZZ", 4},
}

// gcNeighbourText returns client 5's "hello", and the full state of client 5
// after it inserts "ZZ" at index 2 ("heZZllo").
func gcNeighbourText(t *testing.T) (hello, heZZllo []byte) {
	t.Helper()
	a := New(WithClientID(5))
	text := a.GetText("t")
	a.Transact(func(txn *Transaction) { text.Insert(txn, 0, "hello", nil) })
	hello = EncodeStateAsUpdateV1(a, nil)
	a.Transact(func(txn *Transaction) { text.Insert(txn, 2, "ZZ", nil) })
	return hello, EncodeStateAsUpdateV1(a, nil)
}

// peerInsert returns the update of a client that loads state and inserts s at index pos of "t".
func peerInsert(t *testing.T, client ClientID, state []byte, pos int, s string) []byte {
	t.Helper()
	doc := New(WithClientID(client))
	require.NoError(t, ApplyUpdateV1(doc, state, nil))
	sv := doc.StateVector()
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, pos, s, nil) })
	return EncodeStateAsUpdateV1(doc, sv)
}

func textOf(doc *Doc) string { return doc.GetText("t").ToString() }

// requireGCStructs asserts that room and a replica of room's encoded state both
// show want, hold each of ids as a GC struct and agree on the state vector.
func requireGCStructs(t *testing.T, f gcNeighbourFormat, room *Doc, show func(*Doc) string, want string, ids ...ID) {
	t.Helper()
	replica := New(WithClientID(2))
	require.NoError(t, f.apply(replica, f.encode(room, nil), nil))
	require.Zero(t, room.PendingStats().Items)
	for name, doc := range map[string]*Doc{"room": room, "replica of the room's state": replica} {
		require.Equal(t, want, show(doc), name)
		for _, id := range ids {
			s := doc.store.Find(id)
			require.NotNil(t, s, "%s: %v missing", name, id)
			require.True(t, s.isGCOrphan(), "%s: %v must be a GC struct", name, id)
		}
	}
	require.Equal(t, room.StateVector(), replica.StateVector())
}

// findStruct returns the struct of doc that holds id, and fails the test when there is none.
func findStruct(t *testing.T, doc *Doc, id ID) *Item {
	t.Helper()
	s := doc.store.Find(id)
	require.NotNil(t, s, "%v missing", id)
	return s
}

// requireGCStructsEitherOrder applies base, then gc and item in both orders, in
// each format, and checks each room with requireGCStructs.
func requireGCStructsEitherOrder(t *testing.T, base, gc, item []byte, show func(*Doc) string, want string, ids ...ID) {
	t.Helper()
	for _, f := range gcNeighbourFormats {
		for _, order := range []struct {
			name    string
			updates [][]byte
		}{
			{"GC first", [][]byte{gc, item}},
			{"item first", [][]byte{item, gc}},
		} {
			t.Run(f.name+"/"+order.name, func(t *testing.T) {
				room := New(WithClientID(1))
				for _, u := range append([][]byte{base}, order.updates...) {
					f.applyV1(t, room, u)
				}
				requireGCStructs(t, f, room, show, want, ids...)
			})
		}
	}
}

// An item next to a GC struct becomes a GC struct, as in Yjs, whichever side the
// GC struct is on and whether it arrives before or after the item.
func TestUnit_ApplyUpdate_ItemNextToGCStructBecomesGCStruct(t *testing.T) {
	hello, heZZllo := gcNeighbourText(t)
	for _, tc := range gcNeighbourPositions {
		t.Run(tc.name, func(t *testing.T) {
			b := peerInsert(t, 7, heZZllo, tc.pos, "b")
			requireGCStructsEitherOrder(t, hello, gcOverZZ, b, textOf, "hello", ID{Client: 7, Clock: 0})
		})
	}
}

// A deleted item that is not a GC struct keeps its neighbour live, as in Yjs.
func TestUnit_ApplyUpdate_ItemNextToDeletedItemStaysLive(t *testing.T) {
	hello, heZZllo := gcNeighbourText(t)
	a := New(WithClientID(5))
	require.NoError(t, ApplyUpdateV1(a, heZZllo, nil))
	sv := a.StateVector()
	text := a.GetText("t")
	a.Transact(func(txn *Transaction) { text.Delete(txn, 2, 2) })
	deleteZZ := EncodeStateAsUpdateV1(a, sv)
	zzDeleted := EncodeStateAsUpdateV1(a, StateVector{5: 5})
	for _, f := range gcNeighbourFormats {
		for _, tc := range gcNeighbourPositions {
			b := peerInsert(t, 7, heZZllo, tc.pos, "b")
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				room := New(WithClientID(1))
				for _, u := range [][]byte{heZZllo, deleteZZ, b} {
					f.applyV1(t, room, u)
				}
				require.Equal(t, "hebllo", textOf(room))
				require.False(t, findStruct(t, room, ID{Client: 7, Clock: 0}).Deleted)
			})
		}
		// b arrives first and waits for "ZZ", which arrives in one update with its deletion.
		b := peerInsert(t, 7, heZZllo, 4, "b")
		t.Run(f.name+"/origin in ZZ, parked", func(t *testing.T) {
			room := New(WithClientID(1))
			for _, u := range [][]byte{hello, b, zzDeleted} {
				f.applyV1(t, room, u)
			}
			require.Equal(t, "hebllo", textOf(room))
			require.False(t, findStruct(t, room, ID{Client: 7, Clock: 0}).Deleted)
		})
	}
}

// A parked delete that ends inside a GC struct leaves an item whose origin is
// in that GC struct a GC struct.
func TestUnit_ApplyUpdate_ParkedDeleteInsideAGCStructKeepsItsNeighbourGC(t *testing.T) {
	hello, heZZllo := gcNeighbourText(t)
	a := New(WithClientID(5))
	require.NoError(t, ApplyUpdateV1(a, heZZllo, nil))
	sv := a.StateVector()
	text := a.GetText("t")
	a.Transact(func(txn *Transaction) { text.Delete(txn, 3, 1) })
	deleteSecondZ := EncodeStateAsUpdateV1(a, sv)
	b := peerInsert(t, 7, heZZllo, 4, "b")
	for _, f := range gcNeighbourFormats {
		t.Run(f.name, func(t *testing.T) {
			room := New(WithClientID(1))
			for _, u := range [][]byte{hello, b, deleteSecondZ, gcOverZZ} {
				f.applyV1(t, room, u)
			}
			require.Equal(t, 5, room.GetText("t").Len())
			requireGCStructs(t, f, room, textOf, "hello", ID{Client: 7, Clock: 0}, ID{Client: 5, Clock: 6})
		})
	}
}

// An item whose right origin is inside a GC struct, not at its start, becomes a
// GC struct: the GC struct is not split, as in Yjs.
func TestUnit_ApplyUpdate_RightOriginInsideAGCStructBecomesGCStruct(t *testing.T) {
	a := New(WithClientID(5))
	text := a.GetText("t")
	a.Transact(func(txn *Transaction) { text.Insert(txn, 0, "hello", nil) })
	hello := EncodeStateAsUpdateV1(a, nil)
	a.Transact(func(txn *Transaction) { text.Insert(txn, 2, "Z", nil) }) // clock 5: "heZllo"
	a.Transact(func(txn *Transaction) { text.Insert(txn, 4, "W", nil) }) // clock 6: "heZlWlo"
	y := peerInsert(t, 7, EncodeStateAsUpdateV1(a, nil), 4, "y")         // origin "l" 5:2, right origin "W" 5:6
	requireGCStructsEitherOrder(t, hello, gcOverZZ, y, textOf, "hello", ID{Client: 7, Clock: 0}, ID{Client: 5, Clock: 6})
}

// A move whose target is inside a GC struct does not split it, so an item whose
// right origin is in the GC struct still becomes a GC struct.
func TestUnit_ApplyUpdate_MoveTargetInAGCStructDoesNotSplitIt(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		x := &Item{ID: ID{Client: 9}, Parent: &arr.abstractType, Content: NewContentAny([]any{"x"})}
		require.True(t, integrated(tryIntegrate(txn, x)))
	})
	require.NoError(t, ApplyUpdateV1(doc, []byte{1, 1, 5, 0, 0, 3, 0}, nil)) // GC struct over client 5's clocks 0-2
	doc.Transact(func(txn *Transaction) {
		mv := &Item{ID: ID{Client: 7}, Parent: &arr.abstractType, Content: NewContentMove(&ID{Client: 5, Clock: 1}, 1)}
		require.True(t, integrated(tryIntegrate(txn, mv)))
		b := &Item{ID: ID{Client: 8}, Origin: &ID{Client: 9}, OriginRight: &ID{Client: 5, Clock: 1}, Content: NewContentAny([]any{"b"})}
		require.True(t, integrated(tryIntegrate(txn, b)))
	})
	require.Len(t, doc.store.clients[5], 1)
	require.True(t, findStruct(t, doc, ID{Client: 8}).isGCOrphan())
	require.Nil(t, findStruct(t, doc, ID{Client: 5, Clock: 1}).MovedBy)
	require.Equal(t, 1, arr.Len())
}

// A deferred move retries when its target arrives as a GC struct, whichever
// way the target becomes one, so the store keeps no move that waits for a
// target already present.
func TestUnit_ApplyUpdate_TargetArrivingAsGCStructReleasesItsPendingMove(t *testing.T) {
	hello, heZZllo := gcNeighbourText(t)
	for _, f := range gcNeighbourFormats {
		for _, tc := range []struct {
			name    string
			target  ID
			updates [][]byte
		}{
			{"GC struct from the update", ID{Client: 5, Clock: 5}, [][]byte{gcOverZZ}},
			{"right origin in a GC struct", ID{Client: 7}, [][]byte{gcOverZZ, peerInsert(t, 7, heZZllo, 2, "b")}},
			{"origin in a GC struct", ID{Client: 7}, [][]byte{gcOverZZ, peerInsert(t, 7, heZZllo, 4, "b")}},
		} {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				room := New(WithClientID(1))
				f.applyV1(t, room, hello)
				target := tc.target
				room.store.addPendingMove(target, &Item{ID: ID{Client: 9}, Content: NewContentMove(&target, 1)})
				for _, u := range tc.updates {
					f.applyV1(t, room, u)
				}
				require.True(t, findStruct(t, room, target).isGCOrphan())
				require.Empty(t, room.store.pendingMoves)
			})
		}
	}
}

// A move that waits for a target and is then deleted no longer holds the move
// or its target ID in the deferred-move table after GC.
func TestUnit_Delete_UnreachableDeferredMoveIsReleased(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		target := ID{Client: 8, Clock: 7}
		move := &Item{ID: ID{Client: 1}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
		require.True(t, integrated(tryIntegrate(txn, move)))
		require.Len(t, doc.store.pendingMoves[target.Client], 1)
		move.delete(txn)
	})
	RunGC(doc)
	require.Empty(t, doc.store.pendingMoves)
}

// Deleted waiting moves are dropped once they are more than half of the
// waiting moves, so they never outnumber the live ones and deleting n of them
// costs O(n) in total.
func TestUnit_Delete_DeferredMovesCompactWhenHalfAreDeleted(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	moves := make([]*Item, 4)
	doc.Transact(func(txn *Transaction) {
		for i := range moves {
			target := ID{Client: 8, Clock: uint64(i)}
			moves[i] = &Item{ID: ID{Client: 1, Clock: uint64(i)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, moves[i])))
		}
	})
	for i, want := range []int{4, 4, 1, 0} {
		doc.Transact(func(txn *Transaction) { moves[i].delete(txn) })
		require.Len(t, doc.store.pendingMoves[8], want, "after %d deletes", i+1)
	}
	require.Empty(t, doc.store.pendingMoves)
	require.Zero(t, doc.store.pendingMoveCount)
}

// Deleted waiting moves are also dropped when live moves for another client
// resolve and leave the deleted ones as the majority.
func TestUnit_Delete_DeferredMovesCompactWhenOthersResolve(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	var deleted []*Item
	doc.Transact(func(txn *Transaction) {
		for i, target := range []ID{{Client: 8, Clock: 3}, {Client: 8, Clock: 4}, {Client: 9}, {Client: 9, Clock: 1}} {
			mv := &Item{ID: ID{Client: 1, Clock: uint64(i)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, mv)))
			if target.Client == 8 {
				deleted = append(deleted, mv)
			}
		}
	})
	doc.Transact(func(txn *Transaction) {
		for _, mv := range deleted {
			mv.delete(txn)
		}
	})
	require.Len(t, doc.store.pendingMoves[8], 2, "two of four deleted is not more than half")
	doc.Transact(func(txn *Transaction) {
		live := &Item{ID: ID{Client: 9}, Parent: &arr.abstractType, Content: NewContentAny("x", "y")}
		require.True(t, integrated(tryIntegrate(txn, live)))
	})
	require.Empty(t, doc.store.pendingMoves)
	require.Zero(t, doc.store.pendingMoveCount)
}

// Compaction copies a retained heap, so deleting most waiting moves for one
// client releases the heap allocation they used.
func TestUnit_Delete_DeferredMoveCompactionReleasesHeapCapacity(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	moves := make([]*Item, 1024)
	doc.Transact(func(txn *Transaction) {
		for i := range moves {
			target := ID{Client: 8, Clock: uint64(i + 1)}
			moves[i] = &Item{ID: ID{Client: 1, Clock: uint64(i)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, moves[i])))
		}
	})
	doc.Transact(func(txn *Transaction) {
		for _, mv := range moves[:len(moves)-1] {
			mv.delete(txn)
		}
	})
	h := doc.store.pendingMoves[8]
	require.Len(t, h, 1)
	require.Equal(t, 1, cap(h))
}

// A retry that resolves most of a client's waiting moves shrinks that heap, so
// one move that still waits does not keep the peak allocation.
func TestUnit_ApplyUpdate_DeferredMoveResolutionReleasesHeapCapacity(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		for i := range 1024 {
			target := ID{Client: 8, Clock: uint64(i)}
			mv := &Item{ID: ID{Client: 1, Clock: uint64(i)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, mv)))
		}
	})
	vals := make([]any, 1023)
	for i := range vals {
		vals[i] = i
	}
	doc.Transact(func(txn *Transaction) {
		require.True(t, integrated(tryIntegrate(txn, &Item{ID: ID{Client: 8}, Parent: &arr.abstractType, Content: NewContentAny(vals...)})))
	})
	h := doc.store.pendingMoves[8]
	require.Len(t, h, 1)
	require.LessOrEqual(t, cap(h), 4)
}

// Resolution rebuilds the client map after enough buckets disappear, so a
// transient wide table does not leave its bucket array behind.
func TestUnit_ApplyUpdate_DeferredMoveMapShrinksAfterResolution(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		for client := ClientID(10); client < 74; client++ {
			target := ID{Client: client}
			mv := &Item{ID: ID{Client: 1, Clock: uint64(client - 10)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, mv)))
		}
	})
	require.Equal(t, 64, doc.store.pendingMoveMapCap)
	doc.Transact(func(txn *Transaction) {
		for client := ClientID(10); client < 42; client++ {
			require.True(t, integrated(tryIntegrate(txn, &Item{ID: ID{Client: client}, Parent: &arr.abstractType, Content: NewContentAny(client)})))
		}
	})
	require.Len(t, doc.store.pendingMoves, 32)
	require.Equal(t, 32, doc.store.pendingMoveMapCap)
}

// A retry resolves only the moves whose target clock its client has reached,
// in target-clock order, and leaves the others waiting untouched.
func TestUnit_ApplyUpdate_DeferredMovesResolveByTargetClock(t *testing.T) {
	doc := New(WithClientID(1))
	arr := doc.GetArray("a")
	doc.Transact(func(txn *Transaction) {
		for i, clock := range []uint64{9, 0, 5} {
			target := ID{Client: 8, Clock: clock}
			mv := &Item{ID: ID{Client: 1, Clock: uint64(i)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, mv)))
		}
	})
	for _, step := range []struct {
		item  *Item
		want  []uint64
		moved ID
	}{
		{&Item{ID: ID{Client: 8}, Parent: &arr.abstractType, Content: NewContentAny(0, 1, 2)}, []uint64{5, 9}, ID{Client: 8}},
		{&Item{ID: ID{Client: 8, Clock: 3}, Origin: &ID{Client: 8, Clock: 2}, Parent: &arr.abstractType, Content: NewContentAny(3, 4, 5, 6)}, []uint64{9}, ID{Client: 8, Clock: 5}},
	} {
		doc.Transact(func(txn *Transaction) { require.True(t, integrated(tryIntegrate(txn, step.item))) })
		waiting := make([]uint64, 0, len(doc.store.pendingMoves[8]))
		for _, e := range doc.store.pendingMoves[8] {
			waiting = append(waiting, e.clock)
		}
		require.ElementsMatch(t, step.want, waiting)
		require.NotNil(t, findStruct(t, doc, step.moved).MovedBy)
	}
}

// An item with one GC neighbour waits for its other neighbour, as in Yjs, and
// only then becomes a GC struct.
func TestUnit_ApplyUpdate_ItemNextToGCStructWaitsForItsOtherNeighbour(t *testing.T) {
	hello, heZZllo := gcNeighbourText(t)
	// Left GC, right missing: client 9's "Q" follows "ZZ", so it is a GC struct too.
	q9 := peerInsert(t, 9, heZZllo, 4, "Q")
	// Left missing, right GC: client 3's "Q", typed on "hello", sorts before "ZZ" and stays live.
	q3 := peerInsert(t, 3, hello, 2, "Q")
	for _, f := range gcNeighbourFormats {
		for _, tc := range []struct {
			name string
			q    []byte
			bPos int // index of client 7's "b" in the text with "ZZ" and q
			want string
			gc   []ID
		}{
			{"left GC right missing", q9, 4, "hello", []ID{{Client: 9}, {Client: 7}}},
			{"left missing right GC", q3, 3, "heQllo", []ID{{Client: 7}}},
		} {
			state, err := MergeUpdatesV1(heZZllo, tc.q)
			require.NoError(t, err)
			b := peerInsert(t, 7, state, tc.bPos, "b")
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				room := New(WithClientID(1))
				f.applyV1(t, room, hello)
				f.applyV1(t, room, gcOverZZ)
				f.applyV1(t, room, b)
				require.Equal(t, 1, room.PendingStats().Items)
				require.Equal(t, StateVector{5: 7}, room.StateVector())

				f.applyV1(t, room, tc.q)
				requireGCStructs(t, f, room, textOf, tc.want, tc.gc...)
			})
		}
	}
}

// Updates from Yjs 13.6.32 clients 5 and 7. First: client 5 sets m.keep to "v"
// and m.inner to a Y.Text "x1". Then, unseen by the room, client 5 appends
// "x2"; client 7 syncs with it and inserts "y" between "x1" and "x2" (fromB);
// client 5 deletes m.inner, so its diff for the room carries "x2" as a GC struct (fromA).
const (
	deleteNestedFirst = "010305002701016d05696e6e6572022801016d046b656570017701760400050002783100"
	deleteNestedFromA = "01010504000201050200010204"
	deleteNestedFromB = "01010700c405030504017900"
)

// Honest Yjs clients reach the case when one inserts into a nested type that another deletes.
func TestUnit_ApplyUpdate_InsertIntoDeletedNestedTypeBecomesGCStruct(t *testing.T) {
	first, fromA, fromB := mustHex(t, deleteNestedFirst), mustHex(t, deleteNestedFromA), mustHex(t, deleteNestedFromB)
	mapJSON := func(doc *Doc) string {
		m, err := doc.GetMap("m").ToJSON()
		if err != nil {
			return err.Error()
		}
		return string(m)
	}
	requireGCStructsEitherOrder(t, first, fromA, fromB, mapJSON, `{"keep":"v"}`, ID{Client: 7, Clock: 0})
}

// integrated reports whether tryIntegrate took the item without an error.
func integrated(ok bool, _ ClientID, err error) bool { return ok && err == nil }
