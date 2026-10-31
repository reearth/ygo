package crdt

import (
	"errors"
	"strings"
	"testing"
)

// panicContent panics when the integration reads its length.
type panicContent struct{ Content }

func (panicContent) Len() int { panic("parked content panicked") }

// drainPanicUpdates returns V1 updates "a", "b" and "c" typed by client 13 in
// turn, and "x" typed by client 20 after "a".
func drainPanicUpdates(t *testing.T) (a, b, c, x []byte) {
	t.Helper()
	w := New(WithClientID(13))
	wt := w.GetText("t")
	a = lazyRecord(t, w, func(txn *Transaction) { wt.Insert(txn, 0, "a", nil) })
	b = lazyRecord(t, w, func(txn *Transaction) { wt.Insert(txn, 1, "b", nil) })
	c = lazyRecord(t, w, func(txn *Transaction) { wt.Insert(txn, 2, "c", nil) })
	p := New(WithClientID(20))
	pt := p.GetText("t")
	lazyApply(t, p, a, false)
	x = lazyRecord(t, p, func(txn *Transaction) { pt.Insert(txn, 1, "x", nil) })
	return a, b, c, x
}

// A parked item that panics in the drain is dropped, the other parked items
// still integrate, and the apply that ran the drain returns the panic.
func TestUnit_Doc_DrainPanicDropsOnlyThePanickingItem(t *testing.T) {
	a, b, c, x := drainPanicUpdates(t)
	for _, v2 := range []bool{false, true} {
		for _, order := range []struct {
			name   string
			parked [][]byte
		}{
			{"panicking item parked first", [][]byte{b, c, x}},
			{"panicking item parked last", [][]byte{x, c, b}},
		} {
			name := "V1/" + order.name
			if v2 {
				name = "V2/" + order.name
			}
			t.Run(name, func(t *testing.T) {
				room := New(WithClientID(1))
				for _, u := range order.parked {
					lazyApply(t, room, u, v2)
				}
				for _, item := range room.store.pending.items {
					if item.ID == (ID{Client: 13, Clock: 1}) {
						item.Content = panicContent{} // "b" panics when the drain tries it
					}
				}
				var emitted [][]byte
				room.OnUpdate(func(u []byte, _ any) { emitted = append(emitted, u) })

				var err error
				if v2 {
					u2, convErr := UpdateV1ToV2(a)
					if convErr != nil {
						t.Fatal(convErr)
					}
					err = ApplyUpdateV2(room, u2, nil)
				} else {
					err = ApplyUpdateV1(room, a, nil)
				}
				if !errors.Is(err, ErrInvalidUpdate) || !strings.Contains(err.Error(), "parked content panicked") {
					t.Fatalf("apply that ran the drain: err = %v, want ErrInvalidUpdate with the panic", err)
				}
				want := New(WithClientID(2))
				lazyApply(t, want, a, false)
				lazyApply(t, want, x, false)
				if got, w := room.GetText("t").ToString(), want.GetText("t").ToString(); got != w {
					t.Fatalf("room after the drain = %q, want %q", got, w)
				}
				if got := room.PendingStats().Items; got != 1 {
					t.Fatalf("pending items after the drain = %d, want 1 (\"c\"; \"b\" dropped)", got)
				}
				if len(emitted) != 1 {
					t.Fatalf("OnUpdate calls = %d, want 1", len(emitted))
				}
				replica := New(WithClientID(3))
				lazyApply(t, replica, emitted[0], false)
				if got, w := replica.GetText("t").ToString(), want.GetText("t").ToString(); got != w {
					t.Fatalf("OnUpdate output applied to a new doc = %q, want %q", got, w)
				}

				// The sender re-sends "b"; the room converges with a peer that saw everything.
				lazyApply(t, room, b, v2)
				peer := New(WithClientID(4))
				for _, u := range [][]byte{a, b, c, x} {
					lazyApply(t, peer, u, false)
				}
				if got, w := room.GetText("t").ToString(), peer.GetText("t").ToString(); got != w {
					t.Fatalf("room after b is re-sent = %q, peer = %q", got, w)
				}
				if got := room.PendingStats().Items; got != 0 {
					t.Fatalf("pending items after b is re-sent = %d, want 0", got)
				}
			})
		}
	}
}

// Within one update, a panicking struct still stops the scheduler, so the
// apply returns the panic as an error.
func TestUnit_ResolvePendingWaiters_PanicPropagates(t *testing.T) {
	doc := New(WithClientID(1))
	doc.Transact(func(txn *Transaction) {
		mustPanicContaining(t, "parked content panicked", func() {
			resolvePendingWaiters(txn, []*Item{{ID: ID{Client: 5}, Content: panicContent{}}})
		})
	})
}
