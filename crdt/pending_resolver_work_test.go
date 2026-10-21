package crdt

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Count length queries as a deterministic bound on retry work. The payload is
// non-countable; these fixtures verify store clocks rather than rendered text.
type resolverWorkContent struct {
	Content
	work *resolverWork
}

type resolverWork struct {
	lengths, indexed int
	trackIndex       bool
}

func (c *resolverWorkContent) Len() int {
	c.work.lengths++
	if c.work.trackIndex {
		pc, _, _, _ := runtime.Caller(1)
		if f := runtime.FuncForPC(pc); f != nil && strings.HasSuffix(f.Name(), ".indexPendingProducers") {
			c.work.indexed++
		}
	}
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

func TestUnit_PendingResolver_SmallQueueAvoidsIndex(t *testing.T) {
	doc := New()
	defer doc.Destroy()
	work := resolverWork{trackIndex: true}
	pending := resolverWorkItems(doc, 16, "origin", &work)
	var err error
	doc.Transact(func(txn *Transaction) { err = resolveWithinUpdatePending(txn, pending) })
	require.NoError(t, err)
	require.Len(t, doc.StateVector(), 16)
	require.Zero(t, work.indexed, "small queues should not allocate a producer index")
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

func TestUnit_PendingResolver_CoveredItemWithMissingOrigin(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Destroy()
	text := doc.GetText("text")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "x", nil) })
	duplicate := &Item{ID: ID{Client: 1}, Parent: &text.abstractType, Origin: &ID{Client: 9999}, Content: NewContentString("x")}
	doc.Transact(func(txn *Transaction) { require.True(t, tryIntegrate(txn, duplicate)) })
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
