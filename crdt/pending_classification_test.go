package crdt

import (
	"fmt"
	"testing"
)

// A head wrongly classified as permanently blocked trips the early
// permanent > remaining rejection, so a resolvable update is lost. Each case
// sits on a classification edge; the preflight must agree with the reference
// fixed point at every remaining budget.
func TestUnit_PendingBudget_ClassificationMatchesReference(t *testing.T) {
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	item := func(c ClientID, clock uint64, s string, origin, right *ID) *Item {
		i := &Item{ID: ID{Client: c, Clock: clock}, Origin: origin, OriginRight: right, Content: NewContentString(s)}
		if origin == nil && right == nil {
			i.Parent = &text.abstractType
		}
		return i
	}
	id := func(c ClientID, clock uint64) *ID { return &ID{Client: c, Clock: clock} }
	// chain gives each client one struct whose origin is the next client's
	// first struct, so the groups resolve only in reverse wire order.
	chain := func(clients ...ClientID) map[ClientID][]*Item {
		m := map[ClientID][]*Item{}
		for i, c := range clients {
			var o *ID
			if i+1 < len(clients) {
				o = id(clients[i+1], 0)
			}
			m[c] = []*Item{item(c, 0, "x", o, nil)}
		}
		return m
	}
	type tc struct {
		name    string
		groups  [][]*Item
		initial StateVector
	}
	var cases []tc
	{ // Ascending wire; the first client gap starts right after a blocked client.
		m := chain(3, 10, 11, 12)
		head := []*Item{item(20, 0, "h", id(3, 0), nil), item(20, 1, "t", id(20, 0), nil)}
		cases = append(cases, tc{"ascending-gap-edge", [][]*Item{m[3], m[10], m[11], m[12], head}, StateVector{}})
	}
	{ // Descending wire; the gap ends right before a client whose second struct is blocked.
		m := chain(12, 11, 10, 3)
		three := []*Item{item(3, 0, "x", nil, nil), item(3, 1, "y", id(3, 0), id(11, 0))}
		head := []*Item{item(13, 0, "h", id(3, 1), nil), item(13, 1, "t", id(13, 0), nil)}
		cases = append(cases, tc{"descending-gap-edge", [][]*Item{head, m[12], m[11], m[10], three}, StateVector{}})
	}
	{ // Descending wire with a repeated client: only its later, still-blocked group is integrated.
		m := chain(12, 11, 10)
		head := []*Item{item(13, 0, "h", id(3, 1), nil), item(13, 1, "t", id(13, 0), nil)}
		later := []*Item{item(3, 1, "y", id(3, 0), id(10, 0))}
		first := []*Item{item(3, 0, "x", id(12, 0), nil)}
		cases = append(cases, tc{"descending-repeated-client", [][]*Item{head, m[12], m[11], m[10], later, first}, StateVector{}})
	}
	{ // Ascending wire; a dependency on a client outside the wire is already in the store.
		m := chain(3, 10, 11, 12)
		head := []*Item{item(20, 0, "h", id(30, 0), id(3, 0)), item(20, 1, "t", id(20, 0), nil)}
		cases = append(cases, tc{"ordered-known-out-of-range", [][]*Item{m[3], m[10], m[11], m[12], head}, StateVector{30: 1}})
	}
	{ // Unordered wire; a dependency on a client absent from the update is already in the store.
		m := chain(3, 10, 11, 12, 13, 14)
		head := []*Item{item(20, 0, "h", id(30, 0), id(3, 0)), item(20, 1, "t", id(20, 0), nil)}
		cases = append(cases, tc{"known-absent-client", [][]*Item{m[3], m[10], m[11], m[12], m[13], head, m[14]}, StateVector{30: 1}})
	}
	{ // The dependency's client has two groups; only the later one is integrated.
		m := chain(3, 10, 11, 12, 13, 14)
		d1 := []*Item{item(40, 0, "x", nil, nil)}
		d2 := []*Item{item(40, 1, "abcd", id(40, 0), id(3, 0))}
		head := []*Item{item(20, 0, "h", id(40, 3), nil), item(20, 1, "t", id(20, 0), nil)}
		cases = append(cases, tc{"split-dependency-client", [][]*Item{m[3], m[10], m[11], m[12], m[13], head, d1, d2, m[14]}, StateVector{}})
	}
	{ // A cycle the classification cannot prove permanent, next to a head it can.
		m := chain(3, 10)
		cycle := [][]*Item{{item(20, 0, "a", id(21, 0), nil)}, {item(21, 0, "b", id(20, 0), nil)}}
		lost := []*Item{item(22, 0, "c", id(50, 0), nil)}
		cases = append(cases, tc{"cycle-and-permanent-head", [][]*Item{m[3], m[10], cycle[0], cycle[1], lost}, StateVector{}})
	}
	{ // A head whose dependency's client has a superseded group, left over after
		// the worklist next to provable ones.
		m := chain(3, 10, 11)
		dup := []*Item{item(40, 0, "d", nil, nil)}
		open := []*Item{item(30, 0, "o", id(40, 5), nil)}
		lost := []*Item{item(22, 0, "c", id(50, 0), nil)}
		cases = append(cases, tc{"unprovable-and-permanent-heads", [][]*Item{m[3], m[10], m[11], lost, open, dup, dup}, StateVector{}})
	}
	{ // Permanently blocked heads alongside a resolvable chain: they must still count.
		m := chain(3, 10, 11, 12)
		lost := [][]*Item{
			{item(20, 0, "a", id(50, 0), nil), item(20, 1, "b", id(20, 0), nil)},
			{item(21, 0, "a", id(50, 1), nil), item(21, 1, "b", id(21, 0), nil)},
		}
		cases = append(cases, tc{"ordered-permanent-heads", [][]*Item{m[3], m[10], m[11], m[12], lost[0], lost[1]}, StateVector{}})
		cases = append(cases, tc{"unordered-permanent-heads", [][]*Item{m[12], lost[0], m[3], lost[1], m[11], m[10]}, StateVector{}})
	}
	{ // A superseded group's blocked tail must not count; only the last group is integrated.
		m := chain(3, 10, 11, 12)
		dropped := []*Item{item(20, 0, "a", id(50, 0), nil), item(20, 1, "b", id(50, 1), nil), item(20, 2, "c", id(20, 1), nil)}
		last := []*Item{item(20, 0, "z", id(3, 0), nil)}
		cases = append(cases, tc{"superseded-tail-not-counted", [][]*Item{m[3], dropped, m[10], m[11], m[12], last}, StateVector{}})
	}
	{ // A head needs a clock only the superseded group carried.
		m := chain(3, 10, 11, 12)
		dropped := []*Item{item(20, 0, "abc", id(3, 0), nil)}
		head := []*Item{item(30, 0, "h", id(20, 2), nil), item(30, 1, "t", id(30, 0), nil)}
		last := []*Item{item(20, 0, "a", id(3, 0), nil)}
		cases = append(cases, tc{"superseded-dependency", [][]*Item{dropped, m[3], m[10], m[11], m[12], head, last}, StateVector{}})
	}
	{ // Ascending survivors; the dropped group alone breaks the order.
		m := chain(3, 10, 11, 12)
		head := []*Item{item(20, 0, "h", id(40, 0), nil), item(20, 1, "t", id(20, 0), nil)}
		dropped := []*Item{item(12, 0, "q", id(50, 0), nil), item(12, 1, "r", id(50, 1), nil)}
		cases = append(cases, tc{"superseded-ordered-survivors", [][]*Item{dropped, m[3], m[10], m[11], m[12], head}, StateVector{}})
	}
	{ // Many groups wait on one target; it resolves only up to its third struct.
		// The chain outlasts the plain scans, so the target resolves in the worklist.
		m := chain(3, 10, 11, 12, 13, 14, 15, 16)
		target := []*Item{item(40, 0, "a", id(3, 0), nil), item(40, 1, "b", id(40, 0), nil), item(40, 2, "c", id(40, 1), nil),
			item(40, 3, "d", id(50, 0), nil), item(40, 4, "e", id(40, 3), nil)}
		groups := make([][]*Item, 0, 19)
		groups = append(groups, m[3])
		for i, clock := range []uint64{4, 0, 2, 3, 1, 2, 0, 4, 1, 3} {
			c := ClientID(60 + i)
			groups = append(groups, []*Item{item(c, 0, "w", id(40, clock), nil), item(c, 1, "t", id(c, 0), nil)})
		}
		groups = append(groups, target, m[10], m[11], m[12], m[13], m[14], m[15], m[16])
		cases = append(cases, tc{"many-watchers-one-target", groups, StateVector{}})
	}
	{ // A superseded short group for the dependency's client must not bound it.
		m := chain(3, 10, 11, 12, 13, 14)
		dropped := []*Item{item(40, 0, "x", id(50, 0), nil)}
		last := []*Item{item(40, 0, "abcd", id(3, 0), nil)}
		head := []*Item{item(20, 0, "h", id(40, 3), nil), item(20, 1, "t", id(20, 0), nil)}
		cases = append(cases, tc{"superseded-short-bound", [][]*Item{m[3], m[10], dropped, m[11], m[12], m[13], head, last, m[14]}, StateVector{}})
	}
	for _, c := range cases {
		structs := 0
		for _, g := range c.groups {
			structs += len(g)
		}
		for _, version := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/V%d", c.name, version), func(t *testing.T) {
				update := pendingGuardUpdate(source, c.groups, nil, version)
				for rem := 0; rem <= structs+1; rem++ {
					b := pendingBudget{initial: c.initial, update: update, v2: version == 2, remaining: rem}
					want := referencePendingBudgetCheck(b, rem)
					got := b.check(rem)
					if (want == nil) != (got == nil) {
						t.Fatalf("rem=%d: preflight=%v, reference=%v", rem, got, want)
					}
				}
			})
		}
	}
}

// A skip that ends exactly at the known clock leaves the client's later
// structs reachable, so a dependency on them is not impossible.
func TestUnit_PendingBudget_SkipEndingAtKnownClock(t *testing.T) {
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	item := func(c ClientID, clock uint64, origin *ID) *Item {
		i := &Item{ID: ID{Client: c, Clock: clock}, Origin: origin, Content: NewContentString("x")}
		if origin == nil {
			i.Parent = &text.abstractType
		}
		return i
	}
	id := func(c ClientID, clock uint64) *ID { return &ID{Client: c, Clock: clock} }
	groups := [][]*Item{
		{item(20, 0, id(10, 2))},
		{item(10, 0, nil), item(10, 2, id(31, 0))},
		{item(31, 0, id(32, 0))},
		{item(32, 0, id(33, 0))},
		{item(33, 0, nil)},
	}
	initial := StateVector{10: 2}
	for _, version := range []int{1, 2} {
		update := pendingGuardUpdate(source, groups, map[int]uint64{1: 1}, version)
		for rem := 0; rem <= 7; rem++ {
			b := pendingBudget{initial: initial, update: update, v2: version == 2, remaining: rem}
			want := referencePendingBudgetCheck(b, rem)
			if got := b.check(rem); (want == nil) != (got == nil) {
				t.Fatalf("V%d rem=%d: preflight=%v, reference=%v", version, rem, got, want)
			}
		}
	}
}
