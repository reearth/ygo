package crdt

import (
	"bytes"
	"reflect"
	"testing"
)

// failingAfterParking is a V1 update in which client 6 inserts "ok", which
// integrates, and client 5 inserts "xy" after client 9's "Z", which the
// receiver lacks, so it parks; its delete set is cut short, so the apply fails
// after both. filler is client 9's "Z".
func failingAfterParking(t *testing.T) (update, filler []byte) {
	t.Helper()
	z := New(WithClientID(9))
	zt := z.GetText("t")
	z.Transact(func(txn *Transaction) { zt.Insert(txn, 0, "Z", nil) })
	filler = EncodeStateAsUpdateV1(z, nil)

	parker := New(WithClientID(5))
	if err := ApplyUpdateV1(parker, filler, nil); err != nil {
		t.Fatal(err)
	}
	pt := parker.GetText("t")
	parker.Transact(func(txn *Transaction) { pt.Insert(txn, 1, "xy", nil) })
	ok := New(WithClientID(6))
	ot := ok.GetText("t")
	ok.Transact(func(txn *Transaction) { ot.Insert(txn, 0, "ok", nil) })

	valid, err := MergeUpdatesV1(EncodeStateAsUpdateV1(parker, z.StateVector()), EncodeStateAsUpdateV1(ok, nil))
	if err != nil {
		t.Fatal(err)
	}
	if valid[len(valid)-1] != 0 {
		t.Fatalf("the update does not end in an empty delete set: %x", valid)
	}
	// One client (5) with one range, and no clock: the delete set ends early.
	return append(valid[:len(valid)-1:len(valid)-1], 1, 5, 1), filler
}

// requireSame fails t unless room and peer hold the same state and the same
// parked structs and deletions.
func requireSame(t *testing.T, room, peer *Doc, when string) {
	t.Helper()
	if !bytes.Equal(EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(peer, nil)) {
		t.Fatalf("%s: the peer's state differs from the room's (room %q, peer %q)", when,
			room.GetText("t").ToString(), peer.GetText("t").ToString())
	}
	if r, p := room.PendingStats(), peer.PendingStats(); !reflect.DeepEqual(r, p) {
		t.Fatalf("%s: pending differs: room %+v, peer %+v", when, r, p)
	}
}

// A failed apply's landed update carries what it parked, so a peer that
// applies it parks the same structs and, once the missing predecessor arrives
// and both apply it, shows what the room shows. V1 through ApplyUpdateV1Landed,
// V2 through the same encoding of the transaction.
func TestUnit_ApplyUpdateLanded_CarriesWhatTheFailedApplyParked(t *testing.T) {
	v1, filler := failingAfterParking(t)
	valid := append(v1[:len(v1)-3:len(v1)-3], 0)
	v2Valid, err := UpdateV1ToV2(valid)
	if err != nil {
		t.Fatal(err)
	}
	if v2Valid[len(v2Valid)-1] != 0 {
		t.Fatalf("the V2 update does not end in an empty delete set: %x", v2Valid)
	}
	v2 := append(v2Valid[:len(v2Valid)-1:len(v2Valid)-1], 1, 5, 1)

	for _, c := range []struct {
		name  string
		apply func(*Doc) ([]byte, error)
	}{
		{"V1", func(room *Doc) ([]byte, error) { return ApplyUpdateV1Landed(room, v1, nil) }},
		{"V2", func(room *Doc) (landed []byte, err error) {
			room.Transact(func(txn *Transaction) {
				txn.Local = false
				if err = applyV2Txn(txn, v2); err != nil {
					landed = encodeTxnV1Locked(txn)
				}
			}, nil)
			return landed, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			room, peer := New(WithClientID(1)), New(WithClientID(2))
			landed, err := c.apply(room)
			if err == nil {
				t.Fatal("the update with a short delete set applied")
			}
			if got := room.PendingStats().Items; got != 1 {
				t.Fatalf("the room parks %d items, want client 5's \"xy\"", got)
			}
			if landed == nil {
				t.Fatal("nothing landed")
			}
			if err := ApplyUpdateV1(peer, landed, nil); err != nil {
				t.Fatalf("the peer refused the landed update: %v", err)
			}
			requireSame(t, room, peer, "after the failed apply")

			// The filler arrives; the room relays its bytes, which apply.
			for _, d := range []*Doc{room, peer} {
				if err := ApplyUpdateV1(d, filler, nil); err != nil {
					t.Fatal(err)
				}
			}
			requireSame(t, room, peer, "after the filler")
			if got := room.GetText("t").ToString(); len(got) != 5 {
				t.Fatalf("the room holds %q, want \"ok\", \"Z\" and the drained \"xy\"", got)
			}
		})
	}
}

// A V2 GC struct that parks is relayed as the struct alone: the V2 decoder
// records its range in the transaction's delete set before it parks, and a
// peer that got the range as well would park it as a delete range.
func TestUnit_ApplyUpdateLanded_RelaysAParkedGCStructWithoutItsRange(t *testing.T) {
	// A GC struct of client 5 at clock 1, which waits for clock 0, then a
	// delete set cut short so the apply fails after it parks.
	gc := &Item{ID: ID{Client: 5, Clock: 1}, Content: NewContentDeleted(3), Deleted: true}
	update := pendingGuardUpdate(New(), [][]*Item{{gc}}, nil, 2)
	update = update[:len(update)-1]

	room, peer := New(WithClientID(1)), New(WithClientID(2))
	var landed []byte
	var err error
	room.Transact(func(txn *Transaction) {
		txn.Local = false
		if err = applyV2Txn(txn, update); err != nil {
			landed = encodeTxnV1Locked(txn)
		}
	}, nil)
	if err == nil {
		t.Fatal("the update with a short delete set applied")
	}
	if got := room.PendingStats().Items; got != 1 {
		t.Fatalf("the room parks %d structs, want the GC struct", got)
	}
	if err := ApplyUpdateV1(peer, landed, nil); err != nil {
		t.Fatalf("the peer refused the landed update: %v", err)
	}
	requireSame(t, room, peer, "after the failed apply")
}

func TestUnit_WithoutParked(t *testing.T) {
	item := func(clock uint64, n int) *Item {
		return &Item{ID: ID{Client: 5, Clock: clock}, Content: NewContentDeleted(n)}
	}
	for _, c := range []struct {
		name   string
		ranges []DeleteRange
		parked []*Item
		want   []DeleteRange
	}{
		{"no overlap", []DeleteRange{{0, 2}, {10, 2}}, []*Item{item(4, 3)}, []DeleteRange{{0, 2}, {10, 2}}},
		{"a struct covers a range", []DeleteRange{{4, 3}}, []*Item{item(3, 5)}, nil},
		{"two structs in one range", []DeleteRange{{0, 10}}, []*Item{item(2, 2), item(6, 1)}, []DeleteRange{{0, 2}, {4, 2}, {7, 3}}},
		{"a struct spans two ranges", []DeleteRange{{0, 3}, {5, 3}}, []*Item{item(2, 4)}, []DeleteRange{{0, 2}, {6, 2}}},
		{"adjacent structs", []DeleteRange{{0, 6}}, []*Item{item(1, 2), item(3, 2)}, []DeleteRange{{0, 1}, {5, 1}}},
	} {
		if got := withoutParked(c.ranges, c.parked); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// A struct the room parks while it already holds the start of its clocks is
// relayed whole, so the peer parks it too rather than integrating its tail.
func TestUnit_ApplyUpdateLanded_RelaysAParkedStructWhole(t *testing.T) {
	owner := New(WithClientID(77))
	om := owner.GetMap("m")
	owner.Transact(func(txn *Transaction) { om.Set(txn, "k", NewArrayPrelim()) })
	ownerU := EncodeStateAsUpdateV1(owner, nil)
	five := New(WithClientID(5))
	if err := ApplyUpdateV1(five, ownerU, nil); err != nil {
		t.Fatal(err)
	}
	v, _ := five.GetMap("m").Get("k")
	five.Transact(func(txn *Transaction) { v.(*YArray).Push(txn, []any{"a", "b", "c", "d"}) })
	fiveU := EncodeStateAsUpdateV1(five, owner.StateVector())
	// Another writer with client ID 5 wrote "xy" first, so the room holds
	// clocks 0 and 1 of client 5 when the array entries arrive.
	other := New(WithClientID(5))
	ot := other.GetText("t")
	other.Transact(func(txn *Transaction) { ot.Insert(txn, 0, "xy", nil) })
	prior := EncodeStateAsUpdateV1(other, nil)
	ok := New(WithClientID(6))
	okt := ok.GetText("t")
	ok.Transact(func(txn *Transaction) { okt.Insert(txn, 0, "ok", nil) })
	valid, err := MergeUpdatesV1(fiveU, EncodeStateAsUpdateV1(ok, nil))
	if err != nil {
		t.Fatal(err)
	}
	failing := append(valid[:len(valid)-1:len(valid)-1], 1, 5, 1)

	room, peer := New(WithClientID(1)), New(WithClientID(2))
	for _, d := range []*Doc{room, peer} {
		if err := ApplyUpdateV1(d, prior, nil); err != nil {
			t.Fatal(err)
		}
	}
	landed, err := ApplyUpdateV1Landed(room, failing, nil)
	if err == nil {
		t.Fatal("the update with a short delete set applied")
	}
	if err := ApplyUpdateV1(peer, landed, nil); err != nil {
		t.Fatalf("the peer refused the landed update: %v", err)
	}
	requireSame(t, room, peer, "after the failed apply")
	for _, d := range []*Doc{room, peer} {
		if err := ApplyUpdateV1(d, ownerU, nil); err != nil {
			t.Fatal(err)
		}
	}
	requireSame(t, room, peer, "after the owner's update")
}

// panicSplice is content whose Splice panics, so a drain that must split it
// fails the apply after the drain has integrated other structs.
type panicSplice struct{ Content }

func (p panicSplice) Splice(int) Content { panic("panicSplice") }
func (p panicSplice) Copy() Content      { return p }

// A struct the failed apply parked and its drain then integrated, before a
// later struct of the drain failed, is relayed once, as an added struct.
func TestUnit_ApplyUpdateLanded_RelaysADrainedStructOnce(t *testing.T) {
	text := func(d *Doc) *YText { return d.GetText("t") }
	insert := func(d *Doc, at int, s string) {
		yt := text(d)
		d.Transact(func(txn *Transaction) { yt.Insert(txn, at, s, nil) })
	}
	d60 := New(WithClientID(60))
	insert(d60, 0, "a")
	u60 := EncodeStateAsUpdateV1(d60, nil)
	d50 := New(WithClientID(50))
	_ = ApplyUpdateV1(d50, u60, nil)
	insert(d50, 1, "o")
	u50 := EncodeStateAsUpdateV1(d50, d60.StateVector())
	d70 := New(WithClientID(70))
	_ = ApplyUpdateV1(d70, u60, nil)
	_ = ApplyUpdateV1(d70, u50, nil)
	sv70 := d70.StateVector()
	insert(d70, 2, "p")
	u70 := EncodeStateAsUpdateV1(d70, sv70)
	d40 := New(WithClientID(40))
	insert(d40, 0, "Q")
	u40 := EncodeStateAsUpdateV1(d40, nil)
	// u50 parks on u60, which the update brings, so the drain integrates it.
	update, err := MergeUpdatesV1(u60, u70)
	if err != nil {
		t.Fatal(err)
	}
	room, peer := New(WithClientID(1)), New(WithClientID(2))
	for _, d := range []*Doc{room, peer} {
		for _, u := range [][]byte{u40, u50} {
			if err := ApplyUpdateV1(d, u, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	// An older parked struct that the drain reaches after client 70's "p"
	// and cannot split.
	room.store.pending.items = append(room.store.pending.items, &Item{
		ID: ID{Client: 40, Clock: 0}, Origin: &ID{Client: 70, Clock: 0},
		Content: panicSplice{NewContentString("QQ")},
	})
	landed, err := ApplyUpdateV1Landed(room, update, nil)
	if err == nil || landed == nil {
		t.Fatalf("apply err %v, landed %d bytes: want a failure with landed bytes", err, len(landed))
	}
	if err := ApplyUpdateV1(peer, landed, nil); err != nil {
		t.Fatal(err)
	}
	if rs, ps := room.StateVector(), peer.StateVector(); !reflect.DeepEqual(rs, ps) {
		t.Fatalf("room %q %v, peer %q %v", text(room).ToString(), rs, text(peer).ToString(), ps)
	}
}
