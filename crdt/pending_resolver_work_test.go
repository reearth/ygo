package crdt

import (
	"fmt"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// Count length queries as a deterministic bound on retry work. The payload is
// non-countable; these fixtures verify store clocks rather than rendered text.
type resolverWorkContent struct {
	Content
	work *resolverWork
}

type resolverWork struct {
	lengths int
}

func (c *resolverWorkContent) Len() uint64 {
	c.work.lengths++
	return c.Content.Len()
}

func resolverWorkItems(doc *Doc, n int, kind string, work *resolverWork) []*Item {
	root := doc.GetMap("root")
	pending := make([]*Item, 0, 2*n)
	for i := 0; i < n; i++ {
		c := ClientID(i + 1)
		item := &Item{ID: ID{Client: c}, Parent: &root.abstractType, Content: &resolverWorkContent{NewContentDeleted(1), work}}
		switch kind {
		case "own":
			item.ID = ID{Client: 1, Clock: uint64(n - i - 1)}
		case "origin":
			if i+1 < n {
				item.Origin = &ID{Client: c + 1}
			}
		case "right":
			if i+1 < n {
				item.OriginRight = &ID{Client: c + 1}
			}
		case "parent":
			item.ID.Client = ClientID(n + i + 1)
			item.Parent = nil
			item.parentID = &ID{Client: c}
		}
		pending = append(pending, item)
	}
	if kind == "parent" {
		for i := 0; i < n; i++ {
			item := &Item{ID: ID{Client: ClientID(i + 1)}, Content: NewContentType(&NewMapPrelim().abstractType), ParentSub: strPtr("child")}
			if i+1 < n {
				item.parentID = &ID{Client: ClientID(i + 2)}
			} else {
				item.Parent = &root.abstractType
			}
			pending = append(pending, item)
		}
	}
	return pending
}

func TestUnit_PendingResolver_DependencyWork(t *testing.T) {
	for _, kind := range []string{"own", "origin", "right", "parent"} {
		t.Run(kind, func(t *testing.T) {
			const n = 512
			doc := New(WithClientID(100001))
			defer doc.Destroy()
			work := resolverWork{}
			pending := resolverWorkItems(doc, n, kind, &work)
			var err error
			doc.Transact(func(txn *Transaction) { err = resolveWithinUpdatePending(txn, pending) })
			require.NoError(t, err)
			require.Zero(t, doc.PendingStats().Items)
			if kind == "own" {
				require.Equal(t, uint64(n), doc.StateVector().Clock(1))
			} else {
				count := n
				if kind == "parent" {
					count *= 2
				}
				require.Len(t, doc.StateVector(), count)
			}
			require.LessOrEqual(t, work.lengths, 32*len(pending), "retry work must remain proportional to the queue")
		})
	}
}

// The structs an update deferred before a refused item resolve with the same
// work bound as the ordinary within-update retry.
func TestUnit_PendingResolver_IntegrateBeforeRefusalWork(t *testing.T) {
	const n = 512
	doc := New(WithClientID(100001))
	defer doc.Destroy()
	work := resolverWork{}
	pending := resolverWorkItems(doc, n, "origin", &work)
	doc.Transact(func(txn *Transaction) { integrateBeforeRefusal(txn, pending) })
	require.Zero(t, doc.PendingStats().Items)
	require.Len(t, doc.StateVector(), n)
	require.LessOrEqual(t, work.lengths, 32*n, "retry work must remain proportional to the queue")
}

// A refused item in a large deferred queue stops the pass, as Yjs stops when a
// struct throws: the chain it waited on integrates first, the rest stays out.
func TestUnit_PendingResolver_LargeQueueStopsAtARefusedItem(t *testing.T) {
	const n = 64
	doc := New(WithClientID(100001))
	defer doc.Destroy()
	root := doc.GetMap("root")
	gc := &Item{ID: ID{Client: 3 * n}, Content: NewContentDeleted(1), Deleted: true}
	doc.store.Append(gc)
	// The refused item waits on the first chain, then continues the GC struct.
	refused := &Item{ID: ID{Client: 3 * n}, Parent: &root.abstractType, Origin: &ID{Client: 1}, Content: NewContentString("ab")}
	pending := []*Item{refused}
	for _, first := range []ClientID{1, n + 1} {
		for i := ClientID(0); i < n; i++ {
			item := &Item{ID: ID{Client: first + i}, Parent: &root.abstractType, Content: NewContentDeleted(1)}
			if i+1 < n {
				item.Origin = &ID{Client: first + i + 1}
			}
			pending = append(pending, item)
		}
	}
	var remaining []*Item
	var err error
	doc.Transact(func(txn *Transaction) { remaining, err = resolveDeferred(txn, pending) })
	require.ErrorIs(t, err, ErrInvalidUpdate)
	require.Len(t, remaining, n, "the second chain, after the refusal")
	require.Len(t, doc.StateVector(), n+1, "the first chain and the GC struct")
	require.Equal(t, uint64(1), doc.StateVector().Clock(3*n))
}

func TestUnit_PendingResolver_ProgressPreservesBlockedItems(t *testing.T) {
	const n = 20000
	for _, reference := range []bool{false, true} {
		t.Run(fmt.Sprint(reference), func(t *testing.T) {
			doc := New(WithMaxPendingItems(n + 3))
			defer doc.Destroy()
			pending := resolverTestItems(doc, n, "cycle")
			// The cycle spans the actual client IDs, distinct from the ready items.
			root := doc.GetText("text")
			for i := 0; i < 3; i++ {
				pending = append(pending, &Item{ID: ID{Client: ClientID(n + i + 1)}, Parent: &root.abstractType, Content: NewContentString("r")})
			}
			var err error
			doc.Transact(func(txn *Transaction) {
				if reference {
					err = referenceWithinUpdatePending(txn, pending)
				} else {
					err = resolveWithinUpdatePending(txn, pending)
				}
			})
			require.NoError(t, err)
			require.Equal(t, n, doc.PendingStats().Items)
			require.Len(t, doc.StateVector(), 3)
			require.Equal(t, "rrr", root.ToString())
			for i, item := range doc.store.pending.items {
				require.Equal(t, ClientID(i+1), item.ID.Client)
			}
		})
	}
}

// A struct the document already holds still waits on an origin of another
// client it lacks, since Yjs getMissing looks for it before integrateStructs
// checks the struct's clocks.
func TestUnit_PendingResolver_CoveredItemWithMissingOrigin(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Destroy()
	text := doc.GetText("text")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "x", nil) })
	duplicate := &Item{ID: ID{Client: 1}, Parent: &text.abstractType, Origin: &ID{Client: 9999}, Content: NewContentString("x")}
	doc.Transact(func(txn *Transaction) {
		ok, waitsOn, err := tryIntegrate(txn, duplicate)
		require.False(t, ok)
		require.Equal(t, ClientID(9999), waitsOn)
		require.NoError(t, err)
	})
	require.Equal(t, "x", text.ToString())
	require.Equal(t, uint64(1), doc.StateVector().Clock(1))
	require.Zero(t, doc.PendingStats().Items)
}

// A shorter overlapping copy must not hide the range containing a dependency.
// Length queries bound retry work independently of wall-clock benchmark noise.
func TestUnit_PendingResolver_OverlappingRangesBoundWork(t *testing.T) {
	const n = 512
	doc := New(WithClientID(100001))
	defer doc.Destroy()
	root := doc.GetMap("root")
	work := resolverWork{}
	pending := make([]*Item, 0, 2*n)
	for i := 0; i < n; i++ {
		client := ClientID(i + 1)
		var origin *ID
		var parent *abstractType
		if i+1 < n {
			origin = &ID{Client: client + 1, Clock: 2}
		} else {
			parent = &root.abstractType
		}
		pending = append(pending,
			&Item{ID: ID{Client: client}, Parent: parent, Origin: origin, Content: &resolverWorkContent{NewContentDeleted(3), &work}},
			&Item{ID: ID{Client: client, Clock: 1}, Parent: parent, Origin: origin, Content: &resolverWorkContent{NewContentDeleted(1), &work}})
	}
	var err error
	doc.Transact(func(txn *Transaction) { err = resolveWithinUpdatePending(txn, pending) })
	require.NoError(t, err)
	require.Zero(t, doc.PendingStats().Items)
	require.Len(t, doc.StateVector(), n)
	for _, clock := range doc.StateVector() {
		require.Equal(t, uint64(3), clock)
	}
	require.LessOrEqual(t, work.lengths, 32*len(pending), "overlapping copies must not cause quadratic retries")
}

// The record of what a transaction parked is the queue's own run of the
// structs it parked, not a copy, and a resend that parks nothing records
// nothing. A second park in the transaction copies the record, so the queue
// keeps its structs.
func TestUnit_PendingResolver_ParkRecordHoldsOnlyWhatParks(t *testing.T) {
	const n = 4096
	doc := New(WithClientID(1), WithMaxPendingItems(n+3))
	defer doc.Destroy()
	doc.Transact(func(*Transaction) {})
	require.Nil(t, doc.store.parked, "an ordinary transaction does not allocate a park record")

	text := doc.GetText("t")
	structs := func(clock uint64, count int) []*Item {
		out := make([]*Item, count)
		for i := range out {
			out[i] = &Item{ID: ID{Client: 3, Clock: clock + uint64(i)}, Parent: &text.abstractType, Content: NewContentString("x")}
		}
		return out
	}
	park := func(runs ...[]*Item) (record []*Item) {
		doc.Transact(func(txn *Transaction) {
			for _, items := range runs {
				require.NoError(t, parkWithinUpdatePending(txn, items))
			}
			if txn.doc.store.parked != nil {
				record = txn.doc.store.parked.items
			}
		})
		return record
	}

	record := park(structs(1, n)) // past the gap at clock 0
	require.Equal(t, n, doc.PendingStats().Items)
	require.Len(t, record, n)
	require.Same(t, &doc.store.pending.items[0], &record[0], "the record is the queue's run, not a copy")

	require.Nil(t, park(structs(1, n)), "a resend that parks nothing records nothing")
	require.Equal(t, n, doc.PendingStats().Items)

	record = park(structs(n+1, 1))
	require.Equal(t, n+1, doc.PendingStats().Items)
	require.Len(t, record, 1)
	require.Same(t, &doc.store.pending.items[n], &record[0], "a lone parked struct is recorded in the queue's slot")

	first, second := structs(n+2, 1), structs(n+3, 1)
	record = park(first, second)
	require.Equal(t, []*Item{first[0], second[0]}, record)
	require.Equal(t, []*Item{first[0], second[0]}, doc.store.pending.items[n+1:])
}

// Parking state uses one lazy pointer, so ordinary transactions and stores
// stay compact on every supported pointer width. The store's three counts of
// waiting moves (pendingMoveCount, deletedPendingMoves, pendingMoveMapCap)
// are a word each beside it.
func TestUnit_Transaction_ParkRecordKeepsItsSize(t *testing.T) {
	word := unsafe.Sizeof(uintptr(0))
	require.Equal(t, 28*word, unsafe.Sizeof(Transaction{}))
	require.Equal(t, 13*word, unsafe.Sizeof(StructStore{}))
}
