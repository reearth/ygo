package crdt

import (
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/reearth/ygo/encoding"
)

func pendingCursorBomb(version, m int) []byte {
	doc := New()
	defer doc.Destroy()
	text := doc.GetText("text")
	groups := make(map[ClientID][]*Item, m+3)
	for i := 1; i <= 3; i++ { // a small reverse chain so both scans make progress
		client := ClientID(i)
		var origin *ID
		if version == 1 && i < 3 {
			origin = &ID{Client: client + 1}
		}
		if version == 2 && i > 1 {
			origin = &ID{Client: client - 1}
		}
		groups[client] = []*Item{{ID: ID{Client: client}, Parent: &text.abstractType, Origin: origin, Content: NewContentString("x")}}
	}
	for i := 0; i < m; i++ { // groups whose head waits on a client that never arrives
		c := ClientID(1000 + i)
		head := &Item{ID: ID{Client: c}, Parent: &text.abstractType, Origin: &ID{Client: 999_999_999}, Content: NewContentString("x")}
		tail := &Item{ID: ID{Client: c, Clock: 1}, Parent: &text.abstractType, Origin: &ID{Client: c}, Content: NewContentString("y")}
		groups[c] = []*Item{head, tail}
	}
	if version == 2 {
		return encodeStructStoreV2(groups, newDeleteSet(), nil, doc.store)
	}
	return encodeStructStoreV1(groups, newDeleteSet(), nil, doc.store)
}

// Fails if the worklist no longer wakes a head covered by another group.
func TestUnit_PendingBudget_OverlapWakeMatchesReference(t *testing.T) {
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	groups := [][]*Item{
		{{ID: ID{Client: 1}, Origin: &ID{Client: 50}, Parent: &text.abstractType, Content: NewContentString("h")},
			{ID: ID{Client: 1, Clock: 1}, Parent: &text.abstractType, Content: NewContentString("t")}},
		{{ID: ID{Client: 1}, Origin: &ID{Client: 10}, Parent: &text.abstractType, Content: NewContentString("c")}},
	}
	for c := 10; c <= 14; c++ {
		var o *ID
		if c < 14 {
			o = &ID{Client: ClientID(c + 1)}
		}
		groups = append(groups, []*Item{{ID: ID{Client: ClientID(c)}, Origin: o, Parent: &text.abstractType, Content: NewContentString("x")}})
	}
	for _, version := range []int{1, 2} {
		b := pendingBudget{initial: StateVector{}, update: pendingGuardUpdate(source, groups, nil, version), remaining: 0, v2: version == 2}
		if want := referencePendingBudgetCheck(b, 0); want != nil {
			t.Fatalf("V%d invalid complete fixture: %v", version, want)
		}
		if want, got := referencePendingBudgetCheck(b, 0), b.check(0); (want == nil) != (got == nil) {
			t.Errorf("V%d reference=%v worklist=%v", version, want, got)
		}
	}
}

// Fails if the worklist lets a skip satisfy a dependency.
func TestUnit_PendingBudget_SkipInWorklistMatchesReference(t *testing.T) {
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	groups := [][]*Item{
		{{ID: ID{Client: 1}, Origin: &ID{Client: 10}, Parent: &text.abstractType, Content: NewContentString("h")},
			{ID: ID{Client: 1, Clock: 2}, Parent: &text.abstractType, Content: NewContentString("r")}},
		{{ID: ID{Client: 2}, Origin: &ID{Client: 1, Clock: 1}, Parent: &text.abstractType, Content: NewContentString("d")}},
	}
	for c := 10; c <= 14; c++ {
		var o *ID
		if c < 14 {
			o = &ID{Client: ClientID(c + 1)}
		}
		groups = append(groups, []*Item{{ID: ID{Client: ClientID(c)}, Origin: o, Parent: &text.abstractType, Content: NewContentString("x")}})
	}
	for _, version := range []int{1, 2} {
		update := pendingGuardUpdate(source, groups, map[int]bool{0: true}, version)
		for _, rem := range []int{0, 1, 2} {
			b := pendingBudget{initial: StateVector{}, update: update, remaining: rem, v2: version == 2}
			if want, got := referencePendingBudgetCheck(b, rem), b.check(rem); (want == nil) != (got == nil) {
				t.Errorf("V%d remaining=%d: reference=%v worklist=%v", version, rem, want, got)
			}
		}
	}
}

func TestUnit_PendingBudget_CursorBombAllocations(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("V%d", version), func(t *testing.T) {
			update := pendingCursorBomb(version, 20000)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			doc := New(WithMaxPendingItems(16))
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			err := apply(doc, update, nil)
			runtime.ReadMemStats(&after)
			defer doc.Destroy()
			if !errors.Is(err, ErrInvalidUpdate) || doc.PendingStats().Items != 0 {
				t.Fatalf("err=%v pending=%d", err, doc.PendingStats().Items)
			}
			// Ordered unique groups with an external dependency need no index
			// or cursors; only V2's normal columns may scale with wire size.
			limit := uint64(64 * 1024)
			if version == 2 {
				limit += uint64(len(update))
			}
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > limit {
				t.Fatalf("wire=%d allocated=%d", len(update), allocated)
			}
		})
	}
}

func TestUnit_PendingBudget_WireBoundsMatchReference(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, shape := range []string{"missing-client", "missing-middle-client", "missing-clock", "clock-gap", "existing-store"} {
			t.Run(fmt.Sprintf("V%d/%s", version, shape), func(t *testing.T) {
				source := New()
				defer source.Destroy()
				text := source.GetText("text")
				groups := make(map[ClientID][]*Item)
				// Both free scans make progress; unresolved heads reach bound filtering.
				for c := ClientID(10); c <= 14; c++ {
					var origin *ID
					if version == 1 && c < 14 {
						origin = &ID{Client: c + 1}
					}
					if version == 2 && c > 10 {
						origin = &ID{Client: c - 1}
					}
					groups[c] = []*Item{{ID: ID{Client: c}, Parent: &text.abstractType, Origin: origin, Content: NewContentString("x")}}
				}
				dep := ID{Client: 12, Clock: 100} // Existing client, absent clock.
				initial := StateVector{}
				clock := uint64(0)
				if shape == "missing-client" || shape == "existing-store" {
					dep = ID{Client: 999}
				}
				if shape == "missing-middle-client" {
					dep = ID{Client: 7}
				}
				if shape == "clock-gap" {
					clock = 1
					dep = ID{Client: 12}
				}
				if shape == "existing-store" {
					initial[999] = 1
				}
				groups[1] = []*Item{
					{ID: ID{Client: 1, Clock: clock}, Parent: &text.abstractType, Origin: &dep, Content: NewContentString("h")},
					{ID: ID{Client: 1, Clock: clock + 1}, Parent: &text.abstractType, Origin: &ID{Client: 1, Clock: clock}, Content: NewContentString("t")},
				}
				encode := encodeStructStoreV1
				if version == 2 {
					encode = encodeStructStoreV2
				}
				update := encode(groups, newDeleteSet(), nil, source.store)
				for _, cap := range []int{0, 1, 2} {
					b := pendingBudget{initial: initial, update: update, v2: version == 2, remaining: cap}
					if want, got := referencePendingBudgetCheck(b, cap), b.check(cap); (want == nil) != (got == nil) {
						t.Fatalf("cap=%d reference=%v preflight=%v", cap, want, got)
					}
				}
			})
		}
	}
}

// Compare early range rejection with the independent fixed-point oracle,
// including directions, gaps, already-known external IDs, and duplicate groups.
func TestUnit_PendingBudget_ClientRangeMatchesReference(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, order := range []string{"ascending", "descending", "unordered", "duplicate"} {
			for _, shape := range []string{"below", "above", "middle", "known", "clock-gap"} {
				t.Run(fmt.Sprintf("V%d/%s/%s", version, order, shape), func(t *testing.T) {
					source := New()
					defer source.Destroy()
					text := source.GetText("text")
					clients := []ClientID{2, 4, 6, 8, 10}
					if order == "descending" {
						clients = []ClientID{10, 8, 6, 4, 2}
					}
					groups := make([][]*Item, 0, 8)
					for i, c := range clients {
						var origin *ID
						if i+1 < len(clients) {
							origin = &ID{Client: clients[i+1]}
						}
						groups = append(groups, []*Item{{ID: ID{Client: c}, Parent: &text.abstractType, Origin: origin, Content: NewContentString("x")}})
					}
					dep, clock := ID{Client: 30}, uint64(0)
					initial := StateVector{}
					switch shape {
					case "below":
						dep.Client = 0
					case "middle":
						dep.Client = 7
					case "known":
						initial[30] = 1
					case "clock-gap":
						clock, dep.Client = 1, clients[len(clients)-1]
					}
					head := []*Item{
						{ID: ID{Client: 20, Clock: clock}, Parent: &text.abstractType, Origin: &dep, Content: NewContentString("h")},
						{ID: ID{Client: 20, Clock: clock + 1}, Parent: &text.abstractType, Origin: &ID{Client: 20, Clock: clock}, Content: NewContentString("t")},
					}
					if order == "descending" {
						groups = append([][]*Item{head}, groups...)
					} else {
						groups = append(groups, head)
					}
					if order == "unordered" {
						groups[0], groups[1] = groups[1], groups[0]
					}
					if order == "duplicate" {
						// A late overlapping group may cover an otherwise impossible
						// head; the global range must not classify it as permanent.
						groups = append(groups, []*Item{{ID: ID{Client: 20}, Parent: &text.abstractType, Origin: &ID{Client: 2}, Content: NewContentString("cc")}})
					}
					update := pendingGuardUpdate(source, groups, nil, version)
					for _, cap := range []int{0, 1, 2, 3} {
						b := pendingBudget{initial: initial, update: update, v2: version == 2, remaining: cap}
						if want, got := referencePendingBudgetCheck(b, cap), b.check(cap); (want == nil) != (got == nil) {
							t.Fatalf("cap=%d reference=%v preflight=%v", cap, want, got)
						}
					}
				})
			}
		}
	}
}

// Preserve wire order and duplicate client groups in both formats.
func pendingGuardUpdate(source *Doc, groups [][]*Item, skips map[int]bool, version int) []byte {
	enc := encoding.NewEncoder()
	var v2 *v2Encoder
	if version == 2 {
		v2 = newV2Encoder()
		enc = v2.restEnc
	}
	enc.WriteVarUint(uint64(len(groups)))
	for i, items := range groups {
		n := len(items)
		if skips[i] {
			n++
		}
		enc.WriteVarUint(uint64(n))
		if v2 == nil {
			enc.WriteVarUint(uint64(items[0].ID.Client))
		} else {
			v2.writeClient(items[0].ID.Client)
		}
		enc.WriteVarUint(items[0].ID.Clock)
		for j, item := range items {
			if v2 == nil {
				encodeItem(enc, item, 0, source.store)
			} else {
				encodeItemV2(v2, item, 0, source.store)
			}
			if j == 0 && skips[i] {
				if v2 == nil {
					enc.WriteUint8(10)
				} else {
					v2.writeInfo(10)
				}
				enc.WriteVarUint(1)
			}
		}
	}
	if v2 != nil {
		encodeDeleteSetV2(v2, newDeleteSet())
		return v2.toBytes()
	}
	encodeDeleteSet(enc, newDeleteSet())
	return enc.Bytes()
}
