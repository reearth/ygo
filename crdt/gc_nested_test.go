package crdt

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// Yjs 13.6.32 updates. nestedFirst: client 5 puts in root map "m" keep="v",
// q=Y.Array [a, b], p=Y.Map {k: 1 then 2, t: Y.Text "xy"} and live=Y.Text "hi".
// nestedFromB: client 7 loads nestedFirst and inserts "z" in p.t.
// nestedFromA: client 5 deletes q and p.
const (
	nestedFirst = "010a05002801016d046b656570017701762701016d01710008000501027701617701622701016d01700121000504016b01a80505017d0227000504017402040005070278792701016d046c697665020400050a0268690105010501"
	nestedFromA = "000105010109"
	nestedFromB = "01010700c405080509017a0105010501"
)

// What a Yjs 13.6.32 server stores for those updates: the children of q and p
// and the item inserted in p.t are GC structs. nestedItemKinds is the same
// clocks with every struct an item.
const (
	nestedYjsKinds      = "5:0+2 Item 5:2+2 GC 5:4+1 Item 5:5+5 GC 5:10+3 Item"
	nestedYjsKindsWithB = nestedYjsKinds + " 7:0+1 GC"
	nestedItemKinds     = "5:0+13 Item 7:0+1 Item"
	nestedContent       = `{"keep":"v","live":"hi"}`
)

// nestedOldState is the V1 state v1.54.0 encoded after nestedFirst,
// nestedFromA and nestedFromB: every struct is an item.
const nestedOldState = "020b05002801016d046b656570017701762101016d01710101000501022101016d01700121000504016b01a1050501210005040174010100050701810508012701016d046c697665020400050a026869010700c10508050901020701000105010109"

type nestedGCFormat struct {
	name   string
	update func(*testing.T, string) []byte
	apply  func(*Doc, []byte, any) error
	encode func(*Doc, StateVector) []byte
}

var nestedGCFormats = []nestedGCFormat{
	{"V1", mustHex, ApplyUpdateV1, EncodeStateAsUpdateV1},
	{"V2", func(t *testing.T, h string) []byte {
		u, err := UpdateV1ToV2(mustHex(t, h))
		require.NoError(t, err)
		return u
	}, ApplyUpdateV2, EncodeStateAsUpdateV2},
}

// clockKinds lists the clock ranges of doc's store per client, each as a GC
// struct or an item, merging neighbours of one kind as Yjs merges GC structs.
func clockKinds(doc *Doc) string {
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	clients := make([]ClientID, 0, len(doc.store.clients))
	for c := range doc.store.clients {
		clients = append(clients, c)
	}
	slices.Sort(clients)
	var out []string
	for _, c := range clients {
		kind, start, end := "", uint64(0), uint64(0)
		flush := func() {
			if kind != "" {
				out = append(out, fmt.Sprintf("%d:%d+%d %s", c, start, end-start, kind))
			}
		}
		for _, item := range doc.store.clients[c] {
			k := "Item"
			if item.isGCOrphan() {
				k = "GC"
			}
			if k == kind && item.ID.Clock == end {
				end += uint64(item.Content.Len())
				continue
			}
			flush()
			kind, start, end = k, item.ID.Clock, item.ID.Clock+uint64(item.Content.Len())
		}
		flush()
	}
	return strings.Join(out, " ")
}

func nestedContentOf(t *testing.T, doc *Doc) string {
	t.Helper()
	j, err := doc.GetMap("m").ToJSON()
	require.NoError(t, err)
	return string(j)
}

// applyNested applies the updates, given in V1 hex, to doc in format f.
func applyNested(t *testing.T, f nestedGCFormat, doc *Doc, updates ...string) {
	t.Helper()
	for _, u := range updates {
		require.NoError(t, f.apply(doc, f.update(t, u), nil))
	}
}

// requireNestedKinds asserts that doc and a replica of doc's encoded state
// both show nestedContent and store want.
func requireNestedKinds(t *testing.T, f nestedGCFormat, doc *Doc, want string) {
	t.Helper()
	replica := New(WithClientID(2))
	require.NoError(t, f.apply(replica, f.encode(doc, nil), nil))
	for name, d := range map[string]*Doc{"doc": doc, "replica": replica} {
		require.JSONEq(t, nestedContent, nestedContentOf(t, d), name)
		require.Equal(t, want, clockKinds(d), name)
	}
}

// A server's GC turns the children of a deleted nested type into GC structs,
// nested types' children and map entries included, as Yjs ContentType.gc does.
// An item that arrives in the deleted type after it was collected is a GC
// struct too.
func TestUnit_GC_ChildrenOfADeletedNestedTypeBecomeGCStructs(t *testing.T) {
	for _, f := range nestedGCFormats {
		for _, tc := range []struct {
			name    string
			updates []string
			want    string
		}{
			{"delete", []string{nestedFirst, nestedFromA}, nestedYjsKinds},
			{"delete then insert", []string{nestedFirst, nestedFromA, nestedFromB}, nestedYjsKindsWithB},
			{"insert then delete", []string{nestedFirst, nestedFromB, nestedFromA}, nestedYjsKindsWithB},
		} {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				room := New(WithClientID(1))
				applyNested(t, f, room, tc.updates...)
				requireNestedKinds(t, f, room, tc.want)
			})
		}
	}
}

// The document that deletes the nested types collects their children the same
// way.
func TestUnit_GC_LocalDeleteOfANestedTypeMatchesYjs(t *testing.T) {
	doc := New(WithClientID(5))
	m := doc.GetMap("m")
	var q *YArray
	var p *YMap
	var tx *YText
	doc.Transact(func(txn *Transaction) { m.Set(txn, "keep", "v") })
	doc.Transact(func(txn *Transaction) { q = NewArrayPrelim(); m.Set(txn, "q", q) })
	doc.Transact(func(txn *Transaction) { q.Insert(txn, 0, []any{"a"}) })
	doc.Transact(func(txn *Transaction) { q.Insert(txn, 1, []any{"b"}) })
	doc.Transact(func(txn *Transaction) { p = NewMapPrelim(); m.Set(txn, "p", p) })
	doc.Transact(func(txn *Transaction) { p.Set(txn, "k", 1) })
	doc.Transact(func(txn *Transaction) { p.Set(txn, "k", 2) })
	doc.Transact(func(txn *Transaction) { tx = NewTextPrelim(); p.Set(txn, "t", tx) })
	doc.Transact(func(txn *Transaction) { tx.Insert(txn, 0, "xy", nil) })
	doc.Transact(func(txn *Transaction) {
		live := NewTextPrelim()
		m.Set(txn, "live", live)
		live.Insert(txn, 0, "hi", nil)
	})
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "q") })
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "p") })
	for _, f := range nestedGCFormats {
		t.Run(f.name, func(t *testing.T) {
			requireNestedKinds(t, f, doc, nestedYjsKinds)
		})
	}
}

// RunGC collects the children of a deleted nested type that the transaction
// GC skipped because an UndoManager was attached.
func TestUnit_RunGC_ChildrenOfADeletedNestedTypeBecomeGCStructs(t *testing.T) {
	for _, f := range nestedGCFormats {
		t.Run(f.name, func(t *testing.T) {
			room := New(WithClientID(1))
			NewUndoManager(room, []SharedType{room.GetMap("m")})
			applyNested(t, f, room, nestedFirst, nestedFromB, nestedFromA)
			require.Equal(t, nestedItemKinds, clockKinds(room))
			RunGC(room)
			requireNestedKinds(t, f, room, nestedYjsKindsWithB)
		})
	}
}

// With GC off the children stay items: nested collection runs only behind the
// doc.gc gates, here and in RunGC.
func TestUnit_GC_OffKeepsChildrenOfADeletedNestedType(t *testing.T) {
	for _, f := range nestedGCFormats {
		t.Run(f.name, func(t *testing.T) {
			room := New(WithClientID(1), WithGC(false))
			applyNested(t, f, room, nestedFirst, nestedFromB, nestedFromA)
			require.Equal(t, nestedItemKinds, clockKinds(room))
			RunGC(room)
			require.Equal(t, nestedItemKinds, clockKinds(room))
		})
	}
}

// A state that v1.54.0 stored, with deleted items under the deleted nested
// types, loads with the same content and converges with a peer that holds the
// GC structs.
func TestUnit_GC_LoadsAnOldStateOfADeletedNestedType(t *testing.T) {
	for _, f := range nestedGCFormats {
		t.Run(f.name, func(t *testing.T) {
			old := New(WithClientID(1))
			applyNested(t, f, old, nestedOldState)
			require.JSONEq(t, nestedContent, nestedContentOf(t, old))

			peer := New(WithClientID(3))
			applyNested(t, f, peer, nestedFirst, nestedFromA, nestedFromB)
			require.NoError(t, f.apply(peer, f.encode(old, peer.StateVector()), nil))
			require.NoError(t, f.apply(old, f.encode(peer, old.StateVector()), nil))
			require.Equal(t, peer.StateVector(), old.StateVector())
			requireNestedKinds(t, f, old, nestedYjsKindsWithB)
			requireNestedKinds(t, f, peer, nestedYjsKindsWithB)
		})
	}
}

// A write through a handle to a collected type finds the type empty, as in
// Yjs: the new items carry no origin, and are deleted and collected at once.
// A write into a type under the collected type fires no deep observer above
// it, as in Yjs.
func TestUnit_GC_WriteIntoACollectedTypeMatchesYjs(t *testing.T) {
	// Yjs 13.6.32's state diffs for the two writes below.
	want := map[string]string{
		"V1": "0102050521000500016b0101000502010105010007",
		"V2": "0000024501020004000321000103016b010100000241000102050105010006",
	}
	for _, f := range nestedGCFormats {
		t.Run(f.name, func(t *testing.T) {
			doc := New(WithClientID(5))
			m := doc.GetMap("m")
			var p *YMap
			var tx *YText
			doc.Transact(func(txn *Transaction) { p = NewMapPrelim(); m.Set(txn, "p", p) })
			doc.Transact(func(txn *Transaction) { p.Set(txn, "k", 1) })
			doc.Transact(func(txn *Transaction) { tx = NewTextPrelim(); p.Set(txn, "t", tx) })
			doc.Transact(func(txn *Transaction) { tx.Insert(txn, 0, "xy", nil) })
			doc.Transact(func(txn *Transaction) { m.Delete(txn, "p") })
			sv := doc.StateVector()
			doc.Transact(func(txn *Transaction) { p.Set(txn, "k", 3) })
			deep := 0
			m.ObserveDeep(func(*Transaction) { deep++ })
			doc.Transact(func(txn *Transaction) { tx.Insert(txn, 0, "q", nil) })
			require.Zero(t, deep)
			require.Equal(t, want[f.name], hex.EncodeToString(f.encode(doc, sv)))
			for _, id := range []ID{{Client: 5, Clock: 5}, {Client: 5, Clock: 6}} {
				item := doc.store.Find(id)
				require.Nil(t, item.Origin, "%v", id)
				require.Nil(t, item.OriginRight, "%v", id)
			}
		})
	}
}

// RunGC collects the items of a nested type that another client wrote the same
// way whichever client's items it visits first: two GC structs, which its
// merge pass leaves apart.
func TestUnit_RunGC_NestedItemsOfAnotherClientDoNotDependOnClientOrder(t *testing.T) {
	owner, writer := New(WithClientID(1), WithGC(false)), New(WithClientID(2))
	om := owner.GetMap("m")
	owner.Transact(func(txn *Transaction) { om.Set(txn, "p", NewArrayPrelim()) })
	require.NoError(t, ApplyUpdateV1(writer, EncodeStateAsUpdateV1(owner, nil), nil))
	v, _ := writer.GetMap("m").Get("p")
	arr := v.(*YArray)
	writer.Transact(func(txn *Transaction) { arr.Push(txn, []any{"a"}) })
	writer.Transact(func(txn *Transaction) { arr.Push(txn, []any{"b"}) })
	require.NoError(t, ApplyUpdateV1(owner, EncodeStateAsUpdateV1(writer, nil), nil))
	owner.Transact(func(txn *Transaction) { om.Delete(txn, "p") })
	state := EncodeStateAsUpdateV1(owner, nil)
	for range 32 {
		room := New(WithClientID(9))
		NewUndoManager(room, []SharedType{room.GetMap("m")})
		require.NoError(t, ApplyUpdateV1(room, state, nil))
		RunGC(room)
		require.Equal(t, "1:0+1 Item 2:0+2 GC", clockKinds(room))
		require.Len(t, room.store.clients[2], 2)
	}
}

// Load gets the parent document from the subdocument, not from its item,
// which the GC makes a GC struct, with no parent, under the parent's lock
// while Load runs.
func TestUnit_Doc_LoadWhileTheGCCollectsTheSubdocsType(t *testing.T) {
	parent := New(WithClientID(1))
	root := parent.GetMap("root")
	children := make([]*Doc, 200)
	for i := range children {
		inner := NewMapPrelim()
		children[i] = New(WithShouldLoad(false))
		parent.Transact(func(txn *Transaction) { root.Set(txn, fmt.Sprint(i), inner) })
		parent.Transact(func(txn *Transaction) { inner.Set(txn, "doc", children[i]) })
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range children {
			parent.Transact(func(txn *Transaction) { root.Delete(txn, fmt.Sprint(i)) })
		}
	}()
	go func() {
		defer wg.Done()
		for _, c := range children {
			c.Load()
		}
	}()
	wg.Wait()
	for _, c := range children {
		require.True(t, c.ShouldLoad())
	}
}
