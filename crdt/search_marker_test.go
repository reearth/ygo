package crdt

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// coldLeftNeighbour is the reference (marker-free) linear walk used as the oracle.
func coldLeftNeighbour(t *abstractType, index int) (*Item, int) {
	if index <= 0 {
		return nil, 0
	}
	counted := 0
	for item := t.start; item != nil; item = item.Right {
		if item.Deleted || !item.Content.IsCountable() {
			continue
		}
		n := item.Content.Len()
		if counted+n >= index {
			return item, counted
		}
		counted += n
	}
	return nil, counted
}

func TestSearchMarker_ROMatchesCold_Text(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) { txt.Insert(tr, 0, "hello world foo bar baz", nil) })
	at := txt.baseType()
	for i := 0; i <= txt.Len(); i++ {
		gi, gidx := at.findMarkerRO(i)
		ci, cidx := coldLeftNeighbour(at, i)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", i, gi, gidx, ci, cidx)
		}
	}
}

// TestSearchMarker_ROMatchesCold_LargeDocRandom builds a ~5k-char document
// via many separate inserts (so the linked list has many distinct items,
// some possibly deleted), hand-seeds a handful of search markers with
// correct (index, item) pairs taken from the cold oracle, and asserts that
// findMarkerRO — using those markers as its nearest-marker starting points —
// still agrees with the marker-free oracle for 500 random indices. This
// exercises the nearest-marker-then-walk-right/left code paths in
// findMarkerRO, not just the empty-markers fallback covered by the test
// above.
func TestSearchMarker_ROMatchesCold_LargeDocRandom(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")

	rng := rand.New(rand.NewSource(42))

	// Build a large, fragmented document: many small inserts at random
	// positions, plus occasional deletes, so the linked list contains a
	// realistic mix of live and tombstoned items.
	d.Transact(func(tr *Transaction) {
		for txt.Len() < 5000 {
			chunk := fmt.Sprintf("chunk%d ", rng.Intn(100000))
			pos := rng.Intn(txt.Len() + 1)
			txt.Insert(tr, pos, chunk, nil)
			if txt.Len() > 20 && rng.Intn(5) == 0 {
				delPos := rng.Intn(txt.Len() - 10)
				txt.Delete(tr, delPos, 3+rng.Intn(5))
			}
		}
	})

	at := txt.baseType()

	// Hand-seed markers at a handful of positions using the cold oracle as
	// the source of truth for (item, index) pairs, mimicking what a later
	// write-path would install — but findMarkerRO itself must never write
	// t.markers.
	seedPositions := []int{1, 100, 777, 1234, 2500, 3333, 4321, 4999, txt.Len()}
	markers := make([]searchMarker, 0, len(seedPositions))
	for _, p := range seedPositions {
		item, idx := coldLeftNeighbour(at, p)
		if item == nil {
			continue
		}
		markers = append(markers, searchMarker{item: item, index: idx})
	}
	at.markers = markers

	for i := 0; i < 500; i++ {
		idx := rng.Intn(txt.Len() + 1)
		gi, gidx := at.findMarkerRO(idx)
		ci, cidx := coldLeftNeighbour(at, idx)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", idx, gi, gidx, ci, cidx)
		}
	}

	// Sanity: the marker slice itself must be untouched by findMarkerRO —
	// it must still have exactly the entries we seeded (read-only lookup
	// never appends/truncates/reassigns t.markers).
	if len(at.markers) != len(markers) {
		t.Fatalf("t.markers length changed by findMarkerRO: got %d, want %d", len(at.markers), len(markers))
	}
}

// TestSearchMarker_InsertMatchesCold drives the mutating write-path lookup
// (leftNeighbourAt → findMarkerMut) with several insert orderings and asserts
// the resulting document is identical to a force-cold run (disableMarkers),
// i.e. the search-marker maintenance never changes the position a local
// insert resolves to.
func TestSearchMarker_InsertMatchesCold(t *testing.T) {
	build := func(cold bool, order []int) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			for _, pos := range order {
				txt.Insert(tr, pos, "x", nil)
			}
		})
		return txt.ToString()
	}
	seq := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	rev := []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	rnd := []int{0, 1, 0, 2, 1, 3, 0, 4, 2, 1}
	for _, order := range [][]int{seq, rev, rnd} {
		if got, want := build(false, order), build(true, order); got != want {
			t.Fatalf("markers=%q cold=%q order=%v", got, want, order)
		}
	}
}

// TestSearchMarker_InsertMatchesCold_LargeRandom is a heavier version of the
// above: many random inserts and deletes through the marker write path must
// still yield exactly the force-cold document. This exercises marker refresh,
// LRU eviction, and the shim-driven invalidation across a long transaction.
func TestSearchMarker_InsertMatchesCold_LargeRandom(t *testing.T) {
	build := func(cold bool) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		rng := rand.New(rand.NewSource(7))
		d.Transact(func(tr *Transaction) {
			for i := 0; i < 3000; i++ {
				pos := rng.Intn(txt.Len() + 1)
				txt.Insert(tr, pos, fmt.Sprintf("%d", rng.Intn(10)), nil)
				if txt.Len() > 30 && rng.Intn(6) == 0 {
					txt.Delete(tr, rng.Intn(txt.Len()-5), 1+rng.Intn(4))
				}
			}
		})
		return txt.ToString()
	}
	if got, want := build(false), build(true); got != want {
		t.Fatalf("marker/cold divergence:\n marker len=%d\n cold   len=%d", len(got), len(want))
	}
}

// TestSearchMarker_RemoteDeleteOnly_NoStale is the v1.31.6 stale-cache class
// guard (Task 3, #181). A remote update that ONLY tombstones an item is applied
// via ApplyUpdateV1 (which runs with txn.Local hardcoded true, so item.delete's
// own !txn.Local invalidation is dead), then a positioned LOCAL insert follows.
// If the remote delete-apply path fails to invalidate the search markers, the
// still-live markers after the tombstone carry indices that are now one-too-high
// and the insert resolves the wrong neighbour. The marker-run result must equal
// the force-cold run.
func TestSearchMarker_RemoteDeleteOnly_NoStale(t *testing.T) {
	// The v1.31.6 stale-cache class: a remote update that ONLY tombstones an
	// item applies with txn.Local hardcoded true (so item.delete's own
	// !txn.Local invalidation is dead) and must still invalidate the search
	// markers. A warmed cache holding markers for live items AFTER the deleted
	// one would otherwise carry indices that are now too high, and the next
	// positioned insert resolves the wrong neighbour.
	//
	// The cache is hand-seeded (as the read-only oracle tests do) because the
	// write path snaps markers to run-starts, which would hide the exact tail
	// marker this class needs; seeding models precisely the warmed state that
	// v1.31.6 corrupted.
	a := New(WithClientID(1))
	arrA := a.GetArray("arr")
	a.Transact(func(tr *Transaction) {
		for _, v := range []any{"A", "B", "C", "D", "E"} { // 5 distinct (unmerged) items
			arrA.Push(tr, []any{v})
		}
	})
	at := arrA.baseType()

	// Seed a correct, pre-delete warmed cache: markers for the live tail items.
	seed := func() {
		at.markers = at.markers[:0]
		for _, p := range []int{4, 5} { // -> (D,3), (E,4)
			it, idx := coldLeftNeighbour(at, p)
			if it != nil {
				at.markers = append(at.markers, searchMarker{item: it, index: idx})
			}
		}
	}
	seed()
	// Sanity: with the correct warmed cache, findMarkerRO agrees with cold.
	for i := 0; i <= arrA.Len(); i++ {
		if gi, gx := at.findMarkerRO(i); func() bool { ci, cx := coldLeftNeighbour(at, i); return gi != ci || gx != cx }() {
			t.Fatalf("pre-delete seed already stale at %d: got (%p,%d)", i, gi, gx)
		}
	}

	// A remote peer tombstones "B" and ships ONLY that delete back.
	b := New(WithClientID(2))
	if err := ApplyUpdateV1(b, EncodeStateAsUpdateV1(a, nil), nil); err != nil {
		t.Fatal(err)
	}
	arrB := b.GetArray("arr")
	seed() // re-warm right before the delete apply (encoding above may have touched markers)
	b.Transact(func(tr *Transaction) { arrB.Delete(tr, 1, 1) })
	upd := EncodeStateAsUpdateV1(b, a.StateVector())
	if err := ApplyUpdateV1(a, upd, nil); err != nil { // a live: [A C D E]; D,E shift left by 1
		t.Fatal(err)
	}

	// The invariant the remote-delete apply must uphold: every surviving marker
	// still resolves like the cold oracle. A stale (D,3)/(E,4) that survived the
	// tombstone would make findMarkerRO return the wrong item near the tail.
	for i := 0; i <= arrA.Len(); i++ {
		gi, gx := at.findMarkerRO(i)
		ci, cx := coldLeftNeighbour(at, i)
		if gi != ci || gx != cx {
			t.Fatalf("stale marker after remote delete-only apply at index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", i, gi, gx, ci, cx)
		}
	}

	// And the end-to-end shape: a positioned insert after the remote delete must
	// land where the cold walk would put it.
	a.Transact(func(tr *Transaction) { arrA.Insert(tr, 4, []any{"Y"}) }) // append at end -> [A C D E Y]
	j, err := arrA.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(j), `["A","C","D","E","Y"]`; got != want {
		t.Fatalf("positioned insert after remote delete-only: got %s want %s", got, want)
	}
}

// TestUpdateMarkerChanges_ShiftSemantics directly exercises the index-shift
// primitive that Task 3 wires into the structural mutation sites. The subtle
// invariant it must uphold: after the call, every surviving marker's recorded
// index still equals its item's true rendered start. The exact-boundary case
// (a marker whose index equals the edit position) is the one that made the
// naive strict-less-than condition wrong for our re-walk-trusting findMarkerRO
// (see the "index <= m.index" reasoning in search_marker.go).
func TestUpdateMarkerChanges_ShiftSemantics(t *testing.T) {
	live := &Item{Content: NewContentString("x")} // countable, not deleted
	dead := &Item{Content: NewContentString("y"), Deleted: true}

	t.Run("insert shifts markers at or after the edit index", func(t *testing.T) {
		at := &abstractType{markers: []searchMarker{
			{item: live, index: 2}, // strictly before edit: must NOT move
			{item: live, index: 5}, // exactly at edit: MUST move (boundary case)
			{item: live, index: 9}, // after edit: must move
		}}
		at.updateMarkerChanges(5, +3) // insert 3 units at index 5
		want := []int{2, 8, 12}
		for i, w := range want {
			if at.markers[i].index != w {
				t.Fatalf("insert marker[%d].index=%d want %d", i, at.markers[i].index, w)
			}
		}
	})

	t.Run("delete shifts and clamps markers at or after the edit index", func(t *testing.T) {
		at := &abstractType{markers: []searchMarker{
			{item: live, index: 2},  // before delete: unchanged
			{item: live, index: 10}, // after delete: shift left by 3
		}}
		at.updateMarkerChanges(4, -3) // delete 3 units at index 4
		want := []int{2, 7}
		for i, w := range want {
			if at.markers[i].index != w {
				t.Fatalf("delete marker[%d].index=%d want %d", i, at.markers[i].index, w)
			}
		}
	})

	t.Run("markers on deleted items are dropped", func(t *testing.T) {
		at := &abstractType{markers: []searchMarker{
			{item: live, index: 1},
			{item: dead, index: 4}, // item became a tombstone: drop
			{item: live, index: 8},
		}}
		at.updateMarkerChanges(4, -2)
		if len(at.markers) != 2 {
			t.Fatalf("expected deleted-item marker dropped, got %d markers", len(at.markers))
		}
		for _, m := range at.markers {
			if m.item == dead {
				t.Fatalf("deleted-item marker survived")
			}
		}
	})
}

// TestSearchMarker_RO_IndexBeyondLen (carried over from Task 1 review): a
// read-only lookup for index > Len() must match the cold oracle (both walk
// off the end and return (nil, totalCounted)).
func TestSearchMarker_RO_IndexBeyondLen(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) { txt.Insert(tr, 0, "abcde", nil) })
	at := txt.baseType()
	for _, idx := range []int{txt.Len() + 1, txt.Len() + 5, txt.Len() + 100} {
		gi, gidx := at.findMarkerRO(idx)
		ci, cidx := coldLeftNeighbour(at, idx)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", idx, gi, gidx, ci, cidx)
		}
	}
}

// TestSearchMarker_RO_DisableMarkersIgnoresBadMarkers (carried over from Task
// 1 review): with disableMarkers set, findMarkerRO must ignore t.markers
// entirely and fall back to the cold walk — even when t.markers is populated
// with deliberately wrong (item, index) pairs.
func TestSearchMarker_RO_DisableMarkersIgnoresBadMarkers(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) { txt.Insert(tr, 0, "hello world foo bar", nil) })
	at := txt.baseType()

	// Populate markers with garbage: point every marker at the first item but
	// claim wildly wrong indices. If findMarkerRO consulted these it would
	// return wrong answers.
	at.markers = []searchMarker{
		{item: at.start, index: 9999},
		{item: at.start, index: -50},
		{item: at.start, index: 3},
	}
	at.disableMarkers = true

	for i := 0; i <= txt.Len()+2; i++ {
		gi, gidx := at.findMarkerRO(i)
		ci, cidx := coldLeftNeighbour(at, i)
		if gi != ci || gidx != cidx {
			t.Fatalf("index %d: findMarkerRO=(%p,%d) cold=(%p,%d)", i, gi, gidx, ci, cidx)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 4: route positional ops through markers + single-cursor ApplyDelta (#181)
//
// Every test below builds the SAME document twice: once with markers live
// (disableMarkers=false, the fast path this task adds) and once with
// disableMarkers=true (forces every op back to its pre-Task-4 full linear
// walk — the oracle). The two runs must be byte/value-identical. Where an
// independent expectation is easy to compute (values are their own index,
// or plain-ASCII string splicing), the test also checks the result against
// that — so a bug shared by both the marker and cold-walk implementations
// can't hide behind marker==cold agreement alone.

// TestSearchMarker_ArrayGet_MatchesCold exercises YArray.Get's findMarkerRO
// fast path (yarray.go). Values are inserted equal to their own index, so
// the expected answer is independently known without re-deriving it from
// the array's own Get/Slice logic.
func TestSearchMarker_ArrayGet_MatchesCold(t *testing.T) {
	idxs := []int{0, 1999, 1000, 3, 1500, 7, 1234, 1998}
	build := func(cold bool) []any {
		d := New(WithClientID(1))
		arr := d.GetArray("a")
		arr.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			for i := 0; i < 2000; i++ {
				arr.Insert(tr, arr.Len(), []any{i})
			}
		})
		out := make([]any, 0, len(idxs))
		for _, idx := range idxs {
			out = append(out, arr.Get(idx))
		}
		return out
	}
	got, cold := build(false), build(true)
	if !reflect.DeepEqual(got, cold) {
		t.Fatalf("marker/cold mismatch:\n got  %v\n cold %v", got, cold)
	}
	want := make([]any, len(idxs))
	for i, idx := range idxs {
		// arr[i] == i by construction, but NewContentAny normalises Go `int`
		// values to int64 (matching the JSON-number convention used
		// elsewhere), so the independent oracle must match that type.
		want[i] = int64(idx)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v (independent oracle)", got, want)
	}
	// Out-of-bounds and negative indices must still behave like before.
	d := New(WithClientID(1))
	arr := d.GetArray("a")
	d.Transact(func(tr *Transaction) { arr.Insert(tr, 0, []any{1, 2, 3}) })
	if v := arr.Get(-1); v != nil {
		t.Fatalf("Get(-1) = %v, want nil", v)
	}
	if v := arr.Get(3); v != nil {
		t.Fatalf("Get(len) = %v, want nil", v)
	}
}

// TestSearchMarker_ArraySlice_MatchesCold exercises YArray.Slice's
// findMarkerRO-accelerated start lookup (yarray.go).
func TestSearchMarker_ArraySlice_MatchesCold(t *testing.T) {
	build := func(cold bool) [][]any {
		d := New(WithClientID(1))
		arr := d.GetArray("a")
		arr.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			for i := 0; i < 2000; i++ {
				arr.Insert(tr, arr.Len(), []any{i})
			}
		})
		return [][]any{
			arr.Slice(0, 10),
			arr.Slice(500, 510),
			arr.Slice(1990, 2000),
			arr.Slice(1000, 1000), // empty range
			arr.Slice(1995, 5000), // end clamps to Len()
		}
	}
	got, cold := build(false), build(true)
	if !reflect.DeepEqual(got, cold) {
		t.Fatalf("marker/cold mismatch:\n got  %v\n cold %v", got, cold)
	}
	expectRange := func(s, e int) []any {
		if e > 2000 {
			e = 2000
		}
		out := make([]any, 0, e-s)
		for i := s; i < e; i++ {
			out = append(out, int64(i)) // NewContentAny normalises int -> int64
		}
		return out
	}
	want := [][]any{
		expectRange(0, 10),
		expectRange(500, 510),
		expectRange(1990, 2000),
		expectRange(1000, 1000),
		expectRange(1995, 5000),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v (independent oracle)", got, want)
	}
}

// TestSearchMarker_ArrayGetSlice_Move_MatchesCold checks that the move-aware
// search-marker walk (G5) returns the same values via markers as via a full
// cold walk for an array that uses Move. renderedStep now renders a winning
// ContentMove's target at its destination and skips a moved-away item at its
// origin, so YArray.Get/Slice route move-containing arrays through the marker
// fast path (the earlier hasMoves bypass is gone). Independent oracle:
// [b c d a e].
func TestSearchMarker_ArrayGetSlice_Move_MatchesCold(t *testing.T) {
	build := func(cold bool) ([]any, []any) {
		d := New(WithClientID(1))
		arr := d.GetArray("a")
		arr.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			for _, v := range []any{"a", "b", "c", "d", "e"} {
				arr.Push(tr, []any{v})
			}
			arr.Move(tr, 0, 3) // [b c d a e]
		})
		return arr.ToSlice(), []any{arr.Get(0), arr.Get(1), arr.Get(2), arr.Get(3), arr.Get(4)}
	}
	gotSlice, gotGet := build(false)
	coldSlice, coldGet := build(true)
	if !reflect.DeepEqual(gotSlice, coldSlice) || !reflect.DeepEqual(gotGet, coldGet) {
		t.Fatalf("marker/cold mismatch: slice got=%v cold=%v; get got=%v cold=%v", gotSlice, coldSlice, gotGet, coldGet)
	}
	want := []any{"b", "c", "d", "a", "e"}
	if !reflect.DeepEqual(gotSlice, want) {
		t.Fatalf("ToSlice got %v want %v", gotSlice, want)
	}
	if !reflect.DeepEqual(gotGet, want) {
		t.Fatalf("Get(0..4) got %v want %v", gotGet, want)
	}
}

// TestSearchMarker_WithMove_MatchesCold is the move-awareness oracle (G5): a
// document containing an active ContentMove must return the same rendered order
// via the marker fast path as via a full cold walk (disableMarkers), across
// every index. Covers three ContentMove shapes — a move that WINS (renders its
// target at the destination), a move that LOSES concurrent arbitration (present
// in the linked list but renders nothing), and a move whose element is later
// DELETED (renders nothing). renderedStep must count a moved-away item at its
// rendered destination, not its physical list position, or markers diverge from
// cold.
func TestSearchMarker_WithMove_MatchesCold(t *testing.T) {
	// Scenario 1: single winning move. [0 1 2 3 4], Move(4→1) => [0 4 1 2 3].
	t.Run("winning", func(t *testing.T) {
		build := func(cold bool) []any {
			d := New(WithClientID(1))
			arr := d.GetArray("a")
			arr.baseType().disableMarkers = cold
			d.Transact(func(tr *Transaction) {
				for _, v := range []int{0, 1, 2, 3, 4} {
					arr.Insert(tr, arr.Len(), []any{v})
				}
			})
			d.Transact(func(tr *Transaction) { arr.Move(tr, 4, 1) })
			out := make([]any, 0, arr.Len())
			for i := 0; i < arr.Len(); i++ {
				out = append(out, arr.Get(i))
			}
			return out
		}
		got, cold := build(false), build(true)
		if !reflect.DeepEqual(got, cold) {
			t.Fatalf("winning move: markers=%v cold=%v", got, cold)
		}
		want := []any{int64(0), int64(4), int64(1), int64(2), int64(3)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("winning move: got %v want %v (independent oracle)", got, want)
		}
	})

	// Scenario 2: a losing ContentMove. Two peers move the same element to
	// different destinations; after merge both docs hold two ContentMove items
	// targeting the same item — one wins, one loses (renders nothing). The
	// lower ClientID wins, so doc1's destination is authoritative.
	t.Run("losing", func(t *testing.T) {
		build := func(cold bool) []any {
			doc1 := newTestDoc(1)
			doc2 := newTestDoc(2)
			arr1 := doc1.GetArray("list")
			arr2 := doc2.GetArray("list")
			arr1.baseType().disableMarkers = cold
			doc1.Transact(func(txn *Transaction) { arr1.Push(txn, []any{"a", "b", "c", "d"}) })
			if err := ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, nil), nil); err != nil {
				t.Fatal(err)
			}
			doc1.Transact(func(txn *Transaction) { arr1.Move(txn, 0, 1) }) // [b a c d]
			doc2.Transact(func(txn *Transaction) { arr2.Move(txn, 0, 3) }) // [b c d a]
			sv1, sv2 := doc1.store.StateVector(), doc2.store.StateVector()
			if err := ApplyUpdateV1(doc1, EncodeStateAsUpdateV1(doc2, sv1), nil); err != nil {
				t.Fatal(err)
			}
			if err := ApplyUpdateV1(doc2, EncodeStateAsUpdateV1(doc1, sv2), nil); err != nil {
				t.Fatal(err)
			}
			out := make([]any, 0, arr1.Len())
			for i := 0; i < arr1.Len(); i++ {
				out = append(out, arr1.Get(i))
			}
			return out
		}
		got, cold := build(false), build(true)
		if !reflect.DeepEqual(got, cold) {
			t.Fatalf("losing move: markers=%v cold=%v", got, cold)
		}
	})

	// Scenario 3: warm markers across a move — after the move, further
	// positioned inserts build/refresh markers under move presence (exercising
	// markPositionAt's move-aware counting and findMarkerRO reading from a
	// move-aware marker), then Get across all indices must equal cold and the
	// independent oracle. Inserting into a move-containing array also exercises
	// leftNeighbourAt on a move-aware bracket.
	t.Run("warm-inserts-after-move", func(t *testing.T) {
		build := func(cold bool) []any {
			d := New(WithClientID(1))
			arr := d.GetArray("a")
			arr.baseType().disableMarkers = cold
			d.Transact(func(tr *Transaction) {
				for i := 0; i < 30; i++ {
					arr.Insert(tr, arr.Len(), []any{i})
				}
				arr.Move(tr, 29, 5) // move last elem to index 5 (a ContentMove there)
				// Positioned inserts after the move rebuild markers move-aware.
				// Index 6 brackets the moved element's ContentMove in
				// leftNeighbourAt, exercising the offset path on a move bracket.
				arr.Insert(tr, 6, []any{99})
				arr.Insert(tr, 3, []any{100})
				arr.Insert(tr, 10, []any{101})
				arr.Insert(tr, 20, []any{102})
			})
			out := make([]any, 0, arr.Len())
			for i := 0; i < arr.Len(); i++ {
				out = append(out, arr.Get(i))
			}
			return out
		}
		got, cold := build(false), build(true)
		if !reflect.DeepEqual(got, cold) {
			t.Fatalf("warm inserts after move: markers=%v cold=%v", got, cold)
		}
	})

	// Scenario 4: a winning move whose element is then deleted — the
	// ContentMove and its target both render nothing.
	t.Run("deleted", func(t *testing.T) {
		build := func(cold bool) []any {
			d := New(WithClientID(1))
			arr := d.GetArray("a")
			arr.baseType().disableMarkers = cold
			d.Transact(func(tr *Transaction) {
				for _, v := range []int{0, 1, 2, 3, 4} {
					arr.Insert(tr, arr.Len(), []any{v})
				}
			})
			d.Transact(func(tr *Transaction) { arr.Move(tr, 4, 1) }) // [0 4 1 2 3]
			d.Transact(func(tr *Transaction) { arr.Delete(tr, 1, 1) })
			out := make([]any, 0, arr.Len())
			for i := 0; i < arr.Len(); i++ {
				out = append(out, arr.Get(i))
			}
			return out
		}
		got, cold := build(false), build(true)
		if !reflect.DeepEqual(got, cold) {
			t.Fatalf("deleted move: markers=%v cold=%v", got, cold)
		}
	})

	// Scenario 5: many deletes at varied positions on a move-containing array.
	// deleteRange positions its start via findMarkerMut (now move-aware) and
	// then walks; markers ON vs force-cold must yield the identical document,
	// guaranteeing the marker cache never changes which element a delete
	// resolves to even when moves are present.
	t.Run("delete-positions-after-move", func(t *testing.T) {
		build := func(cold bool) []any {
			d := New(WithClientID(1))
			arr := d.GetArray("a")
			arr.baseType().disableMarkers = cold
			d.Transact(func(tr *Transaction) {
				for i := 0; i < 40; i++ {
					arr.Insert(tr, arr.Len(), []any{i})
				}
				arr.Move(tr, 39, 4)
				arr.Move(tr, 10, 30)
				arr.Delete(tr, 5, 3)
				arr.Delete(tr, 0, 2)
				arr.Delete(tr, 20, 4)
				arr.Delete(tr, 15, 1)
			})
			out := make([]any, 0, arr.Len())
			for i := 0; i < arr.Len(); i++ {
				out = append(out, arr.Get(i))
			}
			return out
		}
		got, cold := build(false), build(true)
		if !reflect.DeepEqual(got, cold) {
			t.Fatalf("delete positions after move: markers=%v cold=%v", got, cold)
		}
	})
}

// cyclicText returns a length-n string where byte i is determined by i mod 7
// (a period unlikely to accidentally line up with the small shift amounts a
// boundary/off-by-a-few bug would introduce). Unlike a uniform "aaaa...", a
// range shifted by a few positions but kept the same length still changes
// the surviving/inserted substring content — so tests built on cyclicText
// catch position bugs that a uniform-content oracle would render invisible
// (e.g. deleting [2925,2965) instead of [2930,2970) from an all-'x' string
// yields the same result either way; from cyclicText it does not).
func cyclicText(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%7)
	}
	return string(b)
}

// TestSearchMarker_DeleteRange_MatchesCold_Random exercises deleteRange's
// findMarkerMut-accelerated start lookup (yarray.go, shared by YArray.Delete
// and YText.Delete) with many random-position deletes on a large document.
func TestSearchMarker_DeleteRange_MatchesCold_Random(t *testing.T) {
	build := func(cold bool) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		rng := rand.New(rand.NewSource(99))
		d.Transact(func(tr *Transaction) {
			for txt.Len() < 4000 {
				txt.Insert(tr, txt.Len(), fmt.Sprintf("%d-", rng.Intn(1000)), nil)
			}
			for i := 0; i < 300 && txt.Len() > 20; i++ {
				pos := rng.Intn(txt.Len() - 10)
				n := 1 + rng.Intn(9)
				txt.Delete(tr, pos, n)
			}
		})
		return txt.ToString()
	}
	got, cold := build(false), build(true)
	if got != cold {
		t.Fatalf("marker/cold mismatch: len(got)=%d len(cold)=%d", len(got), len(cold))
	}
}

// TestSearchMarker_DeleteRange_MatchesCold_Tail exercises the deleteRange
// fast path for a delete anchored near the end of a large document — the
// case an O(index) walk from the head would make expensive, and the case
// most likely to trip up findMarkerMut's boundary tie-break if it were wired
// in wrong (see the "counted+n <= index: skip forward" comment in
// yarray.go's deleteRange). Content is cyclicText (not a uniform character)
// so a start-position-shifted-but-same-length delete is actually detectable
// (see cyclicText's doc comment) — this is what makes the independent
// oracle discriminating rather than tautological.
func TestSearchMarker_DeleteRange_MatchesCold_Tail(t *testing.T) {
	const n = 3000
	// Two variants: one where the tail delete lands exactly on an item
	// boundary (single-char items), and one where it must split a large
	// multi-char item mid-run — the latter is what actually exercises
	// deleteRange's "counted < index: split at the start of the deletion"
	// branch (a corrupted marker-derived `counted` that still finds the
	// right bracket item can hide behind a boundary-aligned delete, since
	// the split decision never triggers there).
	base := cyclicText(n)
	build := func(cold bool, bulk bool) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			if bulk {
				txt.Insert(tr, 0, base, nil) // one big item
				txt.Delete(tr, n-70, 40)     // mid-item, both boundaries
			} else {
				for i := 0; i < n; i++ {
					txt.Insert(tr, txt.Len(), base[i:i+1], nil)
				}
				txt.Delete(tr, txt.Len()-50, 50) // exactly on an item boundary
			}
		})
		return txt.ToString()
	}
	for _, bulk := range []bool{false, true} {
		got, cold := build(false, bulk), build(true, bulk)
		var want string
		if bulk {
			want = base[:n-70] + base[n-30:]
		} else {
			want = base[:n-50]
		}
		if got != cold {
			t.Fatalf("bulk=%v marker/cold mismatch:\n got  %q\n cold %q", bulk, got, cold)
		}
		if got != want {
			t.Fatalf("bulk=%v got %q want %q (independent oracle)", bulk, got, want)
		}
	}
}

// TestSearchMarker_Format_MatchesCold exercises YText.Format's
// findTextPos/findMarkerMut-accelerated cursor resolution on a large
// document (ytext.go). Content is cyclicText so a shifted-but-same-length
// format range still produces a detectably different Delta (different
// substrings in the surrounding plain runs), not just different lengths.
func TestSearchMarker_Format_MatchesCold(t *testing.T) {
	const n = 3000
	base := cyclicText(n)
	build := func(cold bool) []Delta {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			txt.Insert(tr, 0, base, nil)
			txt.Format(tr, 1500, 100, Attributes{"bold": true})
		})
		return txt.ToDelta()
	}
	got, cold := build(false), build(true)
	if !reflect.DeepEqual(got, cold) {
		t.Fatalf("marker/cold mismatch:\n got  %v\n cold %v", got, cold)
	}
	want := []Delta{
		{Op: DeltaOpInsert, Insert: base[:1500]},
		{Op: DeltaOpInsert, Insert: base[1500:1600], Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: base[1600:]},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v (independent oracle)", got, want)
	}
}

// TestSearchMarker_CurrentAttributesAt_MatchesCold exercises
// YText.currentAttributesAt's !hasFormatting short-circuit (returns empty
// without walking, ytext.go) together with its ordinary full-walk fallback
// once formatting exists — both are consulted by Insert whenever attrs is
// non-empty. Attrs-carrying inserts happen before, at the hasFormatting
// transition, and after, on a large document.
func TestSearchMarker_CurrentAttributesAt_MatchesCold(t *testing.T) {
	build := func(cold bool) []Delta {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			for i := 0; i < 2000; i++ {
				txt.Insert(tr, txt.Len(), "a", nil)
			}
			// First attrs-carrying insert: hasFormatting is still false when
			// currentAttributesAt is consulted here (it flips true only once
			// this call's own opening marker integrates).
			txt.Insert(tr, txt.Len(), "BOLD", Attributes{"bold": true})
			for i := 0; i < 2000; i++ {
				txt.Insert(tr, txt.Len(), "b", nil)
			}
			// hasFormatting is now true: exercises the full walk from
			// txt.start, at a position before the only existing marker.
			txt.Insert(tr, 500, "MID", Attributes{"bold": true})
			txt.Insert(tr, txt.Len(), "END", Attributes{"italic": true})
		})
		return txt.ToDelta()
	}
	got, cold := build(false), build(true)
	if !reflect.DeepEqual(got, cold) {
		t.Fatalf("marker/cold mismatch:\n got  %v\n cold %v", got, cold)
	}
}

// TestSearchMarker_ApplyDelta_MatchesCold exercises ApplyDelta's single
// threaded itemTextPos cursor (ytext.go) across a full mix of insert/
// delete/retain/retain+format ops on a large document. The independent
// oracle applies the identical delta to a plain Go string via ordinary
// slicing (no crdt code at all), so it also catches a bug shared by both the
// marker and cold-cursor ygo code paths.
func TestSearchMarker_ApplyDelta_MatchesCold(t *testing.T) {
	const n = 3000
	delta := []Delta{
		{Op: DeltaOpRetain, Retain: 500},
		{Op: DeltaOpInsert, Insert: "HELLO"},
		{Op: DeltaOpDelete, Delete: 20},
		{Op: DeltaOpRetain, Retain: 1000, Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: "WORLD", Attributes: Attributes{"italic": true}},
		{Op: DeltaOpRetain, Retain: 300},
		{Op: DeltaOpDelete, Delete: 500},
		{Op: DeltaOpInsert, Insert: "TAIL"},
	}
	base := cyclicText(n)
	build := func(cold bool) (string, []Delta) {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			txt.Insert(tr, 0, base, nil)
			txt.ApplyDelta(tr, delta)
		})
		return txt.ToString(), txt.ToDelta()
	}
	gotStr, gotDelta := build(false)
	coldStr, coldDelta := build(true)
	if gotStr != coldStr {
		t.Fatalf("ToString marker/cold mismatch:\n got  %q\n cold %q", gotStr, coldStr)
	}
	if !reflect.DeepEqual(gotDelta, coldDelta) {
		t.Fatalf("ToDelta marker/cold mismatch:\n got  %v\n cold %v", gotDelta, coldDelta)
	}

	// Independent oracle: replay the same delta over plain ASCII text with
	// ordinary string slicing (quill-delta semantics), entirely outside the
	// crdt package.
	independentApply := func(base string, delta []Delta) string {
		var out strings.Builder
		pos := 0
		for _, d := range delta {
			switch d.Op {
			case DeltaOpInsert:
				if s, ok := d.Insert.(string); ok {
					out.WriteString(s)
				}
			case DeltaOpRetain:
				out.WriteString(base[pos : pos+d.Retain])
				pos += d.Retain
			case DeltaOpDelete:
				pos += d.Delete
			}
		}
		out.WriteString(base[pos:])
		return out.String()
	}
	want := independentApply(base, delta)
	if gotStr != want {
		t.Fatalf("got %q want %q (independent oracle)", gotStr, want)
	}
}

// TestApplyDelta_InsertDoesNotInheritPrecedingRetainFormat pins the corrected
// (Yjs/Quill-aligned) YText.ApplyDelta insert-attribute semantics introduced
// by the single-cursor rewrite (#181, commit 481c949): an Insert op's
// formatting comes ONLY from that op's own Attributes field, never from a
// preceding {retain, attributes} op's format bleeding through the shared
// cursor. This is the documented Yjs/Quill delta rule (Yjs Text.js
// applyDelta / the Quill delta spec): "insert" ops are formatted exclusively
// by their own attributes map, and retain+attributes formats only the
// retained range, closing itself with a matching negated marker immediately
// after the range (Yjs insertNegatedAttributes) so nothing downstream
// inherits it.
//
// TestSearchMarker_ApplyDelta_MatchesCold (above) is NOT discriminating for
// this: both its marker and cold arms run the exact same ApplyDelta cursor
// code (disableMarkers only gates search-marker usage in unrelated
// positional lookups, not ApplyDelta's threaded cursor), and its only
// attributed insert ("WORLD") already carries its own explicit Attributes,
// so it never exercises the "insert with NO attributes right after a
// formatted retain" case that changed behavior in 481c949. This test
// asserts an explicit expected Delta/ToString, not marker==cold agreement,
// so a silent regression back to the old bleed-through behavior is caught.
// (Confirmed discriminating by hand: temporarily making applyDeltaInsert
// re-derive its anchor from the index via leftNeighbourAt(pos.index) instead
// of trusting the threaded pos.left pointer — i.e. reproducing the old
// pre-#181 per-op index-based anchoring that let a same-index plain insert
// land on the wrong side of a just-emitted format-closing marker — turns
// this test red (got Insert:"abcX" with Attributes:{bold:true} instead of
// separate "abc"/bold and "Xd"/plain runs). Reverted after confirming, not
// committed.)
//
// NOTE: a Retain is deliberately placed between the two Insert ops below
// (rather than putting them back-to-back) to sidestep a separate, unrelated
// cursor bug also present in this rewrite: applyDeltaInsert only advances
// pos.left past the newly-inserted item when the insert carries its own
// attributes (diff non-empty); for a plain attribute-less insert, pos.left
// is left stale, so a second Insert op immediately following it re-anchors
// at the PRE-insert position and can integrate out of order (observed:
// ApplyDelta([{Retain:3},{Insert:"X"},{Insert:"Y"}]) on "abcdefghij" yields
// "abcYXdefghij" — X and Y swapped). That bug is out of scope for this test
// (which only pins the insert-attribute/format-bleed semantic) and has been
// flagged separately for a dedicated fix.
func TestApplyDelta_InsertDoesNotInheritPrecedingRetainFormat(t *testing.T) {
	d := New(WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tr *Transaction) {
		txt.Insert(tr, 0, "abcdefghij", nil) // 10 plain chars, no formatting
		txt.ApplyDelta(tr, []Delta{
			// Formats "abc" bold; the format must close itself and NOT leak
			// past its own range into whatever comes next.
			{Op: DeltaOpRetain, Retain: 3, Attributes: Attributes{"bold": true}},
			// No attributes of its own -> must land PLAIN (the behavior this
			// task is pinning), not bold-formatted from the preceding retain.
			{Op: DeltaOpInsert, Insert: "X"},
			// Plain retain over "d": no attributes, no format change. (Also
			// sidesteps the unrelated back-to-back-insert cursor bug noted above.)
			{Op: DeltaOpRetain, Retain: 1},
			// Explicit attributes of its own -> must land bold.
			{Op: DeltaOpInsert, Insert: "Y", Attributes: Attributes{"bold": true}},
			// Plain retain over "e": no attributes, no format change.
			{Op: DeltaOpRetain, Retain: 1},
			// Deletes "fg".
			{Op: DeltaOpDelete, Delete: 2},
			// Trailing "hij" is left untouched (implicit retain-to-end).
		})
	})

	wantStr := "abcXdYehij"
	if got := txt.ToString(); got != wantStr {
		t.Fatalf("ToString() = %q, want %q", got, wantStr)
	}

	want := []Delta{
		{Op: DeltaOpInsert, Insert: "abc", Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: "Xd"},
		{Op: DeltaOpInsert, Insert: "Y", Attributes: Attributes{"bold": true}},
		{Op: DeltaOpInsert, Insert: "ehij"},
	}
	got := txt.ToDelta()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToDelta() mismatch (Yjs-aligned insert-attribute semantics):\n got  %#v\n want %#v", got, want)
	}
}

// TestSearchMarker_Format_MatchesCold_OutOfRange exercises findTextPos's
// !hasFormatting fast path (ytext.go) at and beyond the document's length —
// the case findTextPos must clamp pos.index to t.length exactly like the
// walk-from-start loop does. Format itself clamps length so an out-of-range
// index is a no-op either way; what this actually exercises is that
// findTextPos's fast path doesn't panic or corrupt state when index >
// t.length, agreeing with the cold walk on the (harmless) result.
func TestSearchMarker_Format_MatchesCold_OutOfRange(t *testing.T) {
	const n = 500
	build := func(cold bool, idx int) string {
		d := New(WithClientID(1))
		txt := d.GetText("t")
		txt.baseType().disableMarkers = cold
		d.Transact(func(tr *Transaction) {
			txt.Insert(tr, 0, cyclicText(n), nil)
			txt.Format(tr, idx, 10, Attributes{"bold": true})
		})
		return txt.ToString()
	}
	for _, idx := range []int{n, n + 1, n + 50} {
		got, cold := build(false, idx), build(true, idx)
		if got != cold {
			t.Fatalf("idx=%d marker/cold mismatch:\n got  %q\n cold %q", idx, got, cold)
		}
		if got != cyclicText(n) {
			t.Fatalf("idx=%d out-of-range Format changed content: got %q", idx, got)
		}
	}
}
