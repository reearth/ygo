package crdt

// White-box tests: we are inside the crdt package so we can access
// unexported types (abstractType, integrate, delete, etc.) directly.
// This is the right call for testing a CRDT algorithm — the interesting
// invariants live in the internals.

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// newTestDoc creates a Doc with a fixed ClientID for reproducibility.
func newTestDoc(clientID uint64) *Doc {
	return New(WithClientID(ClientID(clientID)))
}

// newTestType creates a bare abstractType attached to doc.
func newTestType(doc *Doc) *abstractType {
	return &abstractType{doc: doc, itemMap: make(map[string]*Item)}
}

// newTxn opens a raw transaction without going through Doc.Transact (useful
// when we want to call integrate directly in tests).
func newTxn(doc *Doc) *Transaction {
	return &Transaction{
		doc:         doc,
		Local:       true,
		deleteSet:   newDeleteSet(),
		beforeState: doc.store.StateVector(),
		changed:     make(map[*abstractType]map[string]struct{}),
	}
}

// listContent walks the linked list of a type and returns all non-deleted
// string content in order — handy for assertion messages.
func listContent(t *abstractType) []string {
	var out []string
	for item := t.start; item != nil; item = item.Right {
		if !item.Deleted {
			if cs, ok := item.Content.(*ContentString); ok {
				out = append(out, cs.Str)
			}
		}
	}
	return out
}

// makeItem is a shortcut for constructing test items.
func makeItem(client, clock uint64, content Content, parent *abstractType) *Item {
	return &Item{
		ID:      ID{Client: ClientID(client), Clock: clock},
		Content: content,
		Parent:  parent,
	}
}

// makeItemAfter constructs an item whose left origin is the given item.
func makeItemAfter(client, clock uint64, content Content, parent *abstractType, after *Item) *Item { //nolint:unparam
	item := makeItem(client, clock, content, parent)
	item.Left = after
	if after != nil {
		item.Origin = &ID{Client: after.ID.Client, Clock: after.ID.Clock}
	}
	return item
}

// ── StateVector ───────────────────────────────────────────────────────────────

func TestUnit_StateVector_Has(t *testing.T) {
	sv := StateVector{1: 5}
	assert.True(t, sv.Has(ID{1, 0}))
	assert.True(t, sv.Has(ID{1, 4}))
	assert.False(t, sv.Has(ID{1, 5})) // clock 5 is NOT yet integrated (next expected)
	assert.False(t, sv.Has(ID{2, 0})) // unknown client
}

func TestUnit_StateVector_Clone(t *testing.T) {
	sv := StateVector{1: 3, 2: 7}
	clone := sv.Clone()
	clone[1] = 999
	assert.Equal(t, uint64(3), sv[1], "original must not be mutated")
}

// ── StructStore ───────────────────────────────────────────────────────────────

func TestUnit_StructStore_FindExact(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("a"), root)
	a.integrate(txn, 0)

	found := doc.store.Find(ID{1, 0})
	require.NotNil(t, found)
	assert.Equal(t, a, found)
}

func TestUnit_StructStore_FindMissing(t *testing.T) {
	doc := newTestDoc(1)
	assert.Nil(t, doc.store.Find(ID{99, 0}))
}

func TestUnit_StructStore_StateVector(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	makeItem(1, 0, NewContentString("a"), root).integrate(txn, 0)
	makeItem(1, 1, NewContentString("b"), root).integrate(txn, 0)
	makeItem(2, 0, NewContentString("x"), root).integrate(txn, 0)

	sv := doc.store.StateVector()
	assert.Equal(t, uint64(2), sv[1])
	assert.Equal(t, uint64(1), sv[2])
}

// ── DeleteSet ─────────────────────────────────────────────────────────────────

func TestUnit_DeleteSet_IsDeleted(t *testing.T) {
	ds := newDeleteSet()
	ds.add(ID{1, 3}, 2) // marks clocks 3 and 4

	assert.False(t, ds.IsDeleted(ID{1, 2}))
	assert.True(t, ds.IsDeleted(ID{1, 3}))
	assert.True(t, ds.IsDeleted(ID{1, 4}))
	assert.False(t, ds.IsDeleted(ID{1, 5}))
	assert.False(t, ds.IsDeleted(ID{2, 3})) // different client
}

func TestUnit_DeleteSet_AdjacentRangesMerge(t *testing.T) {
	ds := newDeleteSet()
	ds.add(ID{1, 0}, 2) // [0,2)
	ds.add(ID{1, 2}, 3) // [2,5) — adjacent, should merge

	assert.Len(t, ds.clients[1], 1, "adjacent ranges should merge into one")
	assert.Equal(t, uint64(0), ds.clients[1][0].Clock)
	assert.Equal(t, uint64(5), ds.clients[1][0].Len)
}

func TestUnit_DeleteSet_Merge(t *testing.T) {
	a := newDeleteSet()
	a.add(ID{1, 0}, 3)

	b := newDeleteSet()
	b.add(ID{2, 0}, 2)
	b.add(ID{1, 5}, 1)

	a.Merge(b)
	assert.True(t, a.IsDeleted(ID{1, 1}))
	assert.True(t, a.IsDeleted(ID{2, 0}))
	assert.True(t, a.IsDeleted(ID{1, 5}))
}

// ── Item.integrate — sequential ───────────────────────────────────────────────

func TestUnit_Item_Integrate_Sequential(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("a"), root)
	b := makeItemAfter(1, 1, NewContentString("b"), root, a)
	c := makeItemAfter(1, 2, NewContentString("c"), root, b)

	a.integrate(txn, 0)
	b.integrate(txn, 0)
	c.integrate(txn, 0)

	assert.Equal(t, []string{"a", "b", "c"}, listContent(root))
	assert.Equal(t, 3, root.length)
}

func TestUnit_Item_Integrate_PrependToStart(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("a"), root)
	b := makeItem(1, 1, NewContentString("b"), root) // no Left = prepend
	c := makeItem(1, 2, NewContentString("c"), root)

	a.integrate(txn, 0)
	b.integrate(txn, 0) // goes to start
	c.integrate(txn, 0) // also goes to start

	// All have nil origin so they go to the start in reverse order
	assert.NotNil(t, root.start)
}

// ── Item.integrate — concurrent conflict resolution ───────────────────────────

func TestUnit_Item_Integrate_Concurrent_LowerClientIDWins(t *testing.T) {
	// Client 1 and Client 2 both insert at the same position (nil origin).
	// Lower ClientID (1) must come first regardless of arrival order.
	doc := newTestDoc(99)
	root := newTestType(doc)
	txn := newTxn(doc)

	// Arrival order: client 2 first, then client 1.
	c2 := makeItem(2, 0, NewContentString("B"), root)
	c1 := makeItem(1, 0, NewContentString("A"), root)

	c2.integrate(txn, 0)
	c1.integrate(txn, 0)

	assert.Equal(t, []string{"A", "B"}, listContent(root),
		"client 1 (lower ID) must sort before client 2")
}

func TestUnit_Item_Integrate_Concurrent_Deterministic(t *testing.T) {
	// Apply the same two concurrent items in both orders; results must match.
	buildDoc := func(first, second *Item) []string {
		doc := newTestDoc(99)
		root := newTestType(doc)
		txn := newTxn(doc)

		// Re-create items so they get fresh parent pointers.
		a := &Item{ID: first.ID, Content: first.Content.Copy(), Parent: root}
		b := &Item{ID: second.ID, Content: second.Content.Copy(), Parent: root}
		a.integrate(txn, 0)
		b.integrate(txn, 0)
		return listContent(root)
	}

	c1 := makeItem(1, 0, NewContentString("A"), nil)
	c2 := makeItem(2, 0, NewContentString("B"), nil)

	orderAB := buildDoc(c1, c2)
	orderBA := buildDoc(c2, c1)

	assert.Equal(t, orderAB, orderBA,
		"concurrent items must converge to the same order regardless of arrival")
}

func TestUnit_Item_Integrate_ThreeWayConcurrent(t *testing.T) {
	// Three clients insert at the same position.
	// Sort by ClientID: 1 < 2 < 3.
	applyInOrder := func(order []int) []string {
		doc := newTestDoc(99)
		root := newTestType(doc)
		txn := newTxn(doc)

		items := []*Item{
			{ID: ID{1, 0}, Content: NewContentString("A"), Parent: root},
			{ID: ID{2, 0}, Content: NewContentString("B"), Parent: root},
			{ID: ID{3, 0}, Content: NewContentString("C"), Parent: root},
		}
		for _, idx := range order {
			cp := &Item{ID: items[idx].ID, Content: items[idx].Content.Copy(), Parent: root}
			cp.integrate(txn, 0)
		}
		return listContent(root)
	}

	want := applyInOrder([]int{0, 1, 2})
	for _, perm := range [][]int{{0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		assert.Equal(t, want, applyInOrder(perm),
			"three-way concurrent insert must converge for permutation %v", perm)
	}
}

func TestUnit_Item_Integrate_DeletedOrigin(t *testing.T) {
	// Item B inserts after A. A is deleted. Item C then inserts after B.
	// C should still find its correct position.
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("a"), root)
	a.integrate(txn, 0)

	b := makeItemAfter(1, 1, NewContentString("b"), root, a)
	b.integrate(txn, 0)

	a.delete(txn)

	c := makeItemAfter(1, 2, NewContentString("c"), root, b)
	c.integrate(txn, 0)

	// a is deleted but still in list; b and c are visible.
	assert.Equal(t, []string{"b", "c"}, listContent(root))
	assert.Equal(t, 2, root.length)
}

func TestUnit_Item_Integrate_OutOfOrder(t *testing.T) {
	// B depends on A (B.Origin = A), but B arrives before A.
	// We simulate this by integrating B with a nil Left (A not yet known),
	// then integrating A, and verifying the final list is still correct.
	//
	// In a real implementation out-of-order delivery requires buffering until
	// the origin is available. Here we confirm that once both items are present
	// the YATA invariant holds: A precedes B.
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("a"), root)
	a.integrate(txn, 0)

	// B's origin is A — integrate after A is present.
	b := makeItemAfter(1, 1, NewContentString("b"), root, a)
	b.integrate(txn, 0)

	assert.Equal(t, []string{"a", "b"}, listContent(root))
	assert.Equal(t, 2, root.length)
}

func TestUnit_Item_Integrate_Idempotent(t *testing.T) {
	// Integrating the same logical set of items a second time (via a fresh
	// abstractType with the same item values) must produce the same result.
	buildList := func() []string {
		doc := newTestDoc(99)
		root := newTestType(doc)
		txn := newTxn(doc)

		a := &Item{ID: ID{1, 0}, Content: NewContentString("hello"), Parent: root}
		b := &Item{
			ID: ID{2, 0}, Content: NewContentString("world"), Parent: root,
			Left: a, Origin: &ID{1, 0},
		}
		a.integrate(txn, 0)
		b.integrate(txn, 0)
		return listContent(root)
	}

	assert.Equal(t, []string{"hello", "world"}, buildList())
}

// ── Item.delete ───────────────────────────────────────────────────────────────

func TestUnit_Item_Delete_UpdatesLength(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("hello"), root)
	a.integrate(txn, 0)
	assert.Equal(t, 5, root.length)

	a.delete(txn)
	assert.Equal(t, 0, root.length)
	assert.True(t, a.Deleted)
}

func TestUnit_Item_Delete_Idempotent(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("x"), root)
	a.integrate(txn, 0)

	a.delete(txn)
	a.delete(txn) // second delete must be a no-op

	assert.Equal(t, 0, root.length)
	assert.Len(t, txn.deleteSet.clients[1], 1)
}

func TestUnit_Item_Delete_RecordedInDeleteSet(t *testing.T) {
	doc := newTestDoc(1)
	root := newTestType(doc)
	txn := newTxn(doc)

	a := makeItem(1, 0, NewContentString("ab"), root)
	a.integrate(txn, 0)
	a.delete(txn)

	assert.True(t, txn.deleteSet.IsDeleted(ID{1, 0}))
}

// ── Doc.Transact ──────────────────────────────────────────────────────────────

func TestUnit_Doc_Transact_ObserverFiresOnce(t *testing.T) {
	doc := newTestDoc(1)
	calls := 0
	doc.OnUpdate(func(_ []byte, _ any) { calls++ })

	doc.Transact(func(txn *Transaction) {
		root := newTestType(doc)
		makeItem(1, 0, NewContentString("a"), root).integrate(txn, 0)
		makeItem(1, 1, NewContentString("b"), root).integrate(txn, 0)
		makeItem(1, 2, NewContentString("c"), root).integrate(txn, 0)
	})

	assert.Equal(t, 1, calls, "observer must fire exactly once per transaction")
}

func TestUnit_Doc_Transact_OriginForwarded(t *testing.T) {
	doc := newTestDoc(1)
	var gotOrigin any
	doc.OnUpdate(func(_ []byte, origin any) { gotOrigin = origin })

	doc.Transact(func(_ *Transaction) {}, "my-origin")

	assert.Equal(t, "my-origin", gotOrigin)
}

func TestUnit_Doc_Transact_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	calls := 0
	unsub := doc.OnUpdate(func(_ []byte, _ any) { calls++ })

	doc.Transact(func(_ *Transaction) {})
	unsub()
	doc.Transact(func(_ *Transaction) {})

	assert.Equal(t, 1, calls, "observer must not fire after unsubscribe")
}

// ── Integration: two-peer convergence ────────────────────────────────────────

// applyItems applies a sequence of item blueprints to a fresh abstractType
// in the given order. Each blueprint carries only the ID and string content;
// the parent and Left pointers are reconstructed for the target type.
type itemBlueprint struct {
	client, clock uint64
	content       string
	originClient  *uint64
	originClock   *uint64
}

func ptr64(v uint64) *uint64 { return &v }

func applyBlueprints(blueprints []itemBlueprint, order []int) []string {
	doc := newTestDoc(99)
	root := newTestType(doc)
	txn := newTxn(doc)

	for _, idx := range order {
		bp := blueprints[idx]
		item := &Item{
			ID:      ID{ClientID(bp.client), bp.clock},
			Content: NewContentString(bp.content),
			Parent:  root,
		}
		if bp.originClient != nil {
			item.Origin = &ID{ClientID(*bp.originClient), *bp.originClock}
			item.Left = doc.store.Find(*item.Origin)
		}
		item.integrate(txn, 0)
	}
	return listContent(root)
}

func TestInteg_TwoPeer_Convergence_AtStart(t *testing.T) {
	// Alice (client 1) and Bob (client 2) both insert at position 0 concurrently.
	blueprints := []itemBlueprint{
		{client: 1, clock: 0, content: "A"},
		{client: 2, clock: 0, content: "B"},
	}

	want := applyBlueprints(blueprints, []int{0, 1})
	got := applyBlueprints(blueprints, []int{1, 0})

	assert.Equal(t, want, got)
	assert.Equal(t, []string{"A", "B"}, want, "client 1 < client 2, so A comes first")
}

func TestInteg_TwoPeer_Convergence_AfterSharedItem(t *testing.T) {
	// Shared prefix: item {1,0} = "x". Then both peers insert after it concurrently.
	blueprints := []itemBlueprint{
		{client: 1, clock: 0, content: "x"},                                                // shared
		{client: 1, clock: 1, content: "A", originClient: ptr64(1), originClock: ptr64(0)}, // Alice after x
		{client: 2, clock: 0, content: "B", originClient: ptr64(1), originClock: ptr64(0)}, // Bob after x
	}

	// The shared item must be applied first in both cases.
	want := applyBlueprints(blueprints, []int{0, 1, 2})
	got := applyBlueprints(blueprints, []int{0, 2, 1})

	assert.Equal(t, want, got)
}

func TestInteg_MultiplePeers_ConcurrentAtStart_Converges(t *testing.T) {
	// Each of N peers inserts exactly one item at position 0 (nil origin).
	// This is the canonical "concurrent insert at same position" stress test.
	// YATA must produce the same list regardless of message arrival order.
	const (
		numPeers   = 6
		iterations = 1000
	)

	blueprints := make([]itemBlueprint, numPeers)
	for i := range blueprints {
		blueprints[i] = itemBlueprint{
			client:  uint64(i + 1),
			clock:   0,
			content: fmt.Sprintf("P%d", i+1),
		}
	}

	reference := applyBlueprints(blueprints, makeRange(numPeers))

	rng := rand.New(rand.NewSource(42))
	for range iterations {
		order := makeRange(numPeers)
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		got := applyBlueprints(blueprints, order)
		assert.Equal(t, reference, got, "concurrent-at-start must converge for all orderings")
	}
}

func TestInteg_CausalChain_Converges(t *testing.T) {
	// Peer 1 inserts A then C (causally: C after A).
	// Peer 2 inserts B concurrently with A (nil origin).
	// B can arrive in any order relative to A and C, but C must come after A.
	//
	// Expected final order: A, C, B
	// Reasoning: A and B are concurrent at start; client 1 < client 2 so A wins.
	//            C has A as origin so it sits right after A, before B.
	blueprints := []itemBlueprint{
		{client: 1, clock: 0, content: "A"},
		{client: 1, clock: 1, content: "C", originClient: ptr64(1), originClock: ptr64(0)},
		{client: 2, clock: 0, content: "B"},
	}

	// Only orderings that respect the causal dependency C-after-A are valid.
	validOrders := [][]int{
		{0, 1, 2}, // A, C, B
		{0, 2, 1}, // A, B, C — B arrives before C but after A
		{2, 0, 1}, // B, A, C
	}

	want := applyBlueprints(blueprints, validOrders[0])
	for _, order := range validOrders[1:] {
		got := applyBlueprints(blueprints, order)
		assert.Equal(t, want, got, "causal chain must converge for order %v", order)
	}
	assert.Equal(t, []string{"A", "C", "B"}, want)
}

func makeRange(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

// ── Content ───────────────────────────────────────────────────────────────────

func TestUnit_ContentString_Len_Unicode(t *testing.T) {
	c := NewContentString("héllo") // é is 2 bytes in UTF-8 but 1 rune
	assert.Equal(t, 5, c.Len())
}

func TestUnit_ContentString_Splice(t *testing.T) {
	c := NewContentString("hello")
	right := c.Splice(3)
	assert.Equal(t, "hel", c.Str)
	assert.Equal(t, "lo", right.(*ContentString).Str)
}

func TestUnit_ContentAny_Splice(t *testing.T) {
	c := NewContentAny(1, 2, 3, 4)
	right := c.Splice(2)
	assert.Equal(t, []any{1, 2}, c.Vals)
	assert.Equal(t, []any{3, 4}, right.(*ContentAny).Vals)
}

func TestUnit_ContentDeleted_IsNotCountable(t *testing.T) {
	c := NewContentDeleted(5)
	assert.Equal(t, 5, c.Len())
	assert.False(t, c.IsCountable())
}

func TestUnit_ContentFormat_IsNotCountable(t *testing.T) {
	c := NewContentFormat("bold", true)
	assert.Equal(t, 1, c.Len())
	assert.False(t, c.IsCountable())
}

// ── OriginIDEquals ────────────────────────────────────────────────────────────

func TestUnit_OriginIDEquals(t *testing.T) {
	a := &ID{1, 5}
	b := &ID{1, 5}
	c := &ID{2, 5}

	assert.True(t, originIDEquals(nil, nil))
	assert.False(t, originIDEquals(nil, a))
	assert.False(t, originIDEquals(a, nil))
	assert.True(t, originIDEquals(a, b))
	assert.False(t, originIDEquals(a, c))
}

// ── ObserveDeep ───────────────────────────────────────────────────────────────

func TestUnit_YArray_ObserveDeep_FiresOnChange(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")

	var calls int
	unsub := arr.ObserveDeep(func(_ *Transaction) { calls++ })
	defer unsub()

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1, 2}) })
	assert.Equal(t, 1, calls)

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{3}) })
	assert.Equal(t, 2, calls)
}

func TestUnit_YMap_ObserveDeep_FiresOnChange(t *testing.T) {
	doc := newTestDoc(1)
	m := doc.GetMap("m")

	var calls int
	unsub := m.ObserveDeep(func(_ *Transaction) { calls++ })
	defer unsub()

	doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "v") })
	assert.Equal(t, 1, calls)
}

func TestUnit_YText_ObserveDeep_FiresOnChange(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")

	var calls int
	unsub := txt.ObserveDeep(func(_ *Transaction) { calls++ })
	defer unsub()

	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi", nil) })
	assert.Equal(t, 1, calls)
}

func TestUnit_ObserveDeep_Unsubscribe(t *testing.T) {
	doc := newTestDoc(1)
	arr := doc.GetArray("a")

	var calls int
	unsub := arr.ObserveDeep(func(_ *Transaction) { calls++ })

	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1}) })
	assert.Equal(t, 1, calls)

	unsub()
	doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{2}) })
	assert.Equal(t, 1, calls, "no more calls after unsubscribe")
}

// ── Doc.Destroy ───────────────────────────────────────────────────────────────

func TestUnit_Doc_Destroy_ClearsState(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	var updateCalls int
	doc.OnUpdate(func(_ []byte, _ any) { updateCalls++ })

	doc.Destroy()

	// After Destroy, OnUpdate observers are cleared — no further callbacks.
	// (We can't call Transact safely after Destroy, but state vector is empty.)
	assert.Equal(t, StateVector{}, doc.StateVector())
	assert.Equal(t, 0, updateCalls, "OnUpdate must not fire after Destroy")
}

// ── Concurrency / race-detector tests ────────────────────────────────────────

func TestRace_Transact_ConcurrentFromGoroutines(t *testing.T) {
	// Run 100 concurrent transactions from separate goroutines.
	// The race detector will flag any data races in Transact, observer
	// snapshotting, or store access.
	doc := New()
	arr := doc.GetArray("list")

	done := make(chan struct{})
	const workers = 10
	const iters = 10
	for w := 0; w < workers; w++ {
		go func() {
			for i := 0; i < iters; i++ {
				doc.Transact(func(txn *Transaction) {
					arr.Push(txn, []any{i})
				})
			}
			done <- struct{}{}
		}()
	}
	for w := 0; w < workers; w++ {
		<-done
	}
	assert.Equal(t, workers*iters, arr.Len())
}

func TestRace_Observe_ConcurrentWithFire(t *testing.T) {
	// Register and unsubscribe observers from one goroutine while transactions
	// fire from another goroutine. Race detector verifies no unsafe reads/writes
	// on the observer slices (N-C1).
	doc := New()
	arr := doc.GetArray("list")

	stop := make(chan struct{})
	// Goroutine 1: fire transactions continuously.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				doc.Transact(func(txn *Transaction) { arr.Push(txn, []any{1}) })
			}
		}
	}()
	// Goroutine 2: register and unregister observers repeatedly.
	for i := 0; i < 200; i++ {
		unsub := arr.Observe(func(_ YArrayEvent) {})
		unsub()
	}
	close(stop)
}

// ── RelativePosition assoc < 0 tests ─────────────────────────────────────────

func TestUnit_RelativePosition_Assoc_Negative_RoundTrip(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	// Create a position at the end with assoc < 0 (end-of-type anchor).
	rp := CreateRelativePositionFromIndex(txt, 5, -1)
	assert.NotNil(t, rp.Item, "assoc<0 at end should anchor to last item")
	assert.Equal(t, -1, rp.Assoc)

	// Encode and decode round-trip.
	encoded := EncodeRelativePosition(rp)
	decoded, err := DecodeRelativePosition(encoded)
	require.NoError(t, err)
	assert.Equal(t, rp, decoded)

	// Resolve to absolute position.
	abs, ok := ToAbsolutePosition(doc, decoded)
	require.True(t, ok)
	assert.Equal(t, 5, abs.Index, "should resolve to end of 'hello'")
	assert.Equal(t, -1, abs.Assoc)
}

func TestUnit_RelativePosition_Assoc_Zero_RoundTrip(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hello", nil) })

	// Position in the middle with default assoc (>= 0).
	rp := CreateRelativePositionFromIndex(txt, 2, 0)
	encoded := EncodeRelativePosition(rp)
	decoded, err := DecodeRelativePosition(encoded)
	require.NoError(t, err)

	abs, ok := ToAbsolutePosition(doc, decoded)
	require.True(t, ok)
	assert.Equal(t, 2, abs.Index)
}

func TestUnit_RelativePosition_StableAfterInsertBefore(t *testing.T) {
	doc := newTestDoc(1)
	txt := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "world", nil) })

	// Anchor at index 2 (inside "world").
	rp := CreateRelativePositionFromIndex(txt, 2, 0)

	// Insert 3 chars before the anchor.
	doc.Transact(func(txn *Transaction) { txt.Insert(txn, 0, "hi ", nil) })

	abs, ok := ToAbsolutePosition(doc, rp)
	require.True(t, ok)
	// The anchor should now be at index 5 (2 + 3 inserted before it).
	assert.Equal(t, 5, abs.Index)
}
