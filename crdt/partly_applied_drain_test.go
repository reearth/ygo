package crdt

import (
	"maps"
	"math"
	"strings"
	"testing"
)

// partlyAppliedCase is an update that integrates "a", which "b" parked
// behind, and then fails.
type partlyAppliedCase struct {
	name  string
	v2    bool
	gap   []byte // "b", which parks behind 13:0
	bad   []byte // "a", then a cut: integrates "a" and fails
	whole []byte // "a" uncut
}

// partlyAppliedCases cuts "a"'s delete set, and a merge of "a" and "c" inside
// "c"'s struct, in V1 and V2.
func partlyAppliedCases(t *testing.T) []partlyAppliedCase {
	t.Helper()
	w := New(WithClientID(13))
	wt := w.GetText("t")
	var ups [][]byte
	w.OnUpdate(func(u []byte, _ any) { ups = append(ups, append([]byte(nil), u...)) })
	for i, s := range []string{"a", "b", "c"} {
		w.Transact(func(txn *Transaction) { wt.Insert(txn, i, s, nil) })
	}
	both, err := MergeUpdatesV1(ups[0], ups[2])
	if err != nil {
		t.Fatal(err)
	}
	v2 := func(u []byte) []byte {
		c, err := UpdateV1ToV2(u)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a2, b2, both2 := v2(ups[0]), v2(ups[1]), v2(both)
	return []partlyAppliedCase{
		{name: "V1 delete set cut", gap: ups[1], bad: ups[0][:len(ups[0])-1], whole: ups[0]},
		{name: "V1 struct cut", gap: ups[1], bad: both[:len(both)-3], whole: ups[0]},
		{name: "V2 delete set cut", v2: true, gap: b2, bad: a2[:len(a2)-1], whole: a2},
		{name: "V2 struct cut", v2: true, gap: b2, bad: both2[:len(both2)-2], whole: a2},
	}
}

func (c partlyAppliedCase) apply(doc *Doc, update []byte) error {
	if c.v2 {
		return ApplyUpdateV2(doc, update, nil)
	}
	return ApplyUpdateV1(doc, update, nil)
}

// A replica that applies what the room's observers were handed for a partly
// applied update holds what the room holds.
func TestUnit_PartlyAppliedUpdate_DrainsWhatItIntegrated(t *testing.T) {
	for _, c := range partlyAppliedCases(t) {
		t.Run(c.name, func(t *testing.T) {
			room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
			if err := c.apply(room, c.gap); err != nil {
				t.Fatal(err)
			}
			if room.PendingStats().Items != 1 {
				t.Fatalf("pending %d before the cut update, want \"b\" parked", room.PendingStats().Items)
			}
			// The replica holds what the room held before the cut update.
			replica := New(WithClientID(2), WithMaxPendingItems(math.MaxInt))
			if err := c.apply(replica, c.gap); err != nil {
				t.Fatal(err)
			}
			var handed [][]byte
			room.OnUpdate(func(u []byte, _ any) { handed = append(handed, u) })

			if err := c.apply(room, c.bad); err == nil {
				t.Fatal("the cut update applied without an error")
			}
			if got := room.StateVector().Clock(13); got == 0 {
				t.Fatal("the cut update integrated nothing; the case does not test a partial apply")
			}

			for _, u := range handed {
				if err := ApplyUpdateV1(replica, u, nil); err != nil {
					t.Fatal(err)
				}
			}
			requireSameDoc(t, room, replica)
			if got := room.GetText("t").ToString(); got != "ab" {
				t.Fatalf("room text %q, want \"ab\": the drain did not integrate \"b\"", got)
			}
		})
	}
}

// A peer that applies ApplyUpdateV1Landed's landed update holds what the room
// holds after its drain.
func TestUnit_PartlyAppliedUpdate_LandedHoldsTheDrain(t *testing.T) {
	for _, c := range partlyAppliedCases(t) {
		if c.v2 {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
			peer := New(WithClientID(2), WithMaxPendingItems(math.MaxInt))
			for _, d := range []*Doc{room, peer} {
				if err := ApplyUpdateV1(d, c.gap, nil); err != nil {
					t.Fatal(err)
				}
			}
			landed, err := ApplyUpdateV1Landed(room, c.bad, nil)
			if err == nil || landed == nil {
				t.Fatalf("landed %x, err %v: want a partial apply", landed, err)
			}
			if err := ApplyUpdateV1(peer, landed, nil); err != nil {
				t.Fatal(err)
			}
			requireSameDoc(t, room, peer)
		})
	}
}

// A drain that panics keeps a partial apply's first error, becomes a whole
// apply's error, and parks the queue again in both.
func TestUnit_PartlyAppliedUpdate_DrainPanicKeepsTheQueue(t *testing.T) {
	for _, c := range partlyAppliedCases(t) {
		t.Run(c.name, func(t *testing.T) {
			for _, partial := range []bool{true, false} {
				room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
				if err := c.apply(room, c.gap); err != nil {
					t.Fatal(err)
				}
				room.store.pending.items[0].Content = nil // the drain panics on "b"
				update, want := c.whole, "panic during item integration"
				if partial {
					update, want = c.bad, "unexpected end of input"
				}
				if err := c.apply(room, update); err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("partial %v: apply returned %v, want %q", partial, err, want)
				}
				if got := room.PendingStats().Items; got != 1 {
					t.Fatalf("partial %v: pending %d after the drain panicked, want \"b\" parked again", partial, got)
				}
			}
		})
	}
}

// A drain that panics parks its queue again with the clocks each item waits
// on, so the items drain once those clocks arrive, in V1 and V2.
func TestUnit_DrainPanic_ParksTheQueueWithItsClocks(t *testing.T) {
	writer := func(client ClientID, name string) [][]byte {
		w := New(WithClientID(client))
		text := w.GetText(name)
		var ups [][]byte
		w.OnUpdate(func(u []byte, _ any) { ups = append(ups, append([]byte(nil), u...)) })
		w.Transact(func(txn *Transaction) { text.Insert(txn, 0, "a", nil) })
		w.Transact(func(txn *Transaction) { text.Insert(txn, 1, "b", nil) })
		return ups
	}
	t13, t20 := writer(13, "t"), writer(20, "u")
	for _, v2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "V1", true: "V2"}[v2], func(t *testing.T) {
			c := partlyAppliedCase{v2: v2}
			apply := func(d *Doc, u []byte) error {
				if v2 {
					var err error
					if u, err = UpdateV1ToV2(u); err != nil {
						t.Fatal(err)
					}
				}
				return c.apply(d, u)
			}
			room := New(WithClientID(1), WithMaxPendingItems(math.MaxInt))
			for _, u := range [][]byte{t13[1], t20[1]} {
				if err := apply(room, u); err != nil {
					t.Fatal(err)
				}
			}
			first := room.store.pending.items[0]
			content := first.Content
			first.Content = nil // the drain panics on 13:1, before it tries 20:1
			if err := apply(room, t13[0]); err == nil {
				t.Fatal("the drain did not panic")
			}
			first.Content = content
			if err := apply(room, t20[0]); err != nil {
				t.Fatal(err)
			}
			if got, want := room.GetText("t").ToString()+room.GetText("u").ToString(), "abab"; got != want || room.PendingStats().Items != 0 {
				t.Fatalf("text %q pending %d, want %q with nothing parked", got, room.PendingStats().Items, want)
			}
		})
	}
}

// requireSameDoc requires room and other to hold the same text, state vector
// and count of parked items.
func requireSameDoc(t *testing.T, room, other *Doc) {
	t.Helper()
	roomText, otherText := room.GetText("t").ToString(), other.GetText("t").ToString()
	roomSV, otherSV := room.StateVector(), other.StateVector()
	roomPending, otherPending := room.PendingStats().Items, other.PendingStats().Items
	if roomText != otherText || !maps.Equal(roomSV, otherSV) || roomPending != otherPending {
		t.Fatalf("room %q pending %d sv %v; other %q pending %d sv %v",
			roomText, roomPending, roomSV, otherText, otherPending, otherSV)
	}
}
