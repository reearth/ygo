package crdt

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// v1DeleteSetUpdate is a V1 update with no structs and one delete range.
func v1DeleteSetUpdate(clock uint64, length int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0)
	ds := newDeleteSet()
	ds.add(ID{Client: 3, Clock: clock}, length)
	encodeDeleteSet(enc, ds)
	return enc.Bytes()
}

// v2DeleteSetUpdate is a V2 update with no structs and one delete range.
func v2DeleteSetUpdate(clock uint64, length int) []byte {
	enc := newV2Encoder()
	enc.restEnc.WriteVarUint(0)
	ds := newDeleteSet()
	ds.add(ID{Client: 3, Clock: clock}, length)
	encodeDeleteSetV2(enc, ds)
	return enc.toBytes()
}

// v1SkipUpdate is a V1 update of client 30 whose skips move its clock to
// skips*skip, then a string "q" on root text "t".
func v1SkipUpdate(skips int, skip uint64) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(uint64(skips + 1))
	enc.WriteVarUint(30)
	enc.WriteVarUint(0)
	for range skips {
		enc.WriteUint8(10)
		enc.WriteVarUint(skip)
	}
	enc.WriteUint8(wireString) // no origins: parent info follows
	enc.WriteUint8(1)
	enc.WriteVarString("t")
	enc.WriteVarString("q")
	encodeDeleteSet(enc, newDeleteSet())
	return enc.Bytes()
}

// v1OriginUpdate is a V1 update of client 30's "q", after client 9's struct at
// clock originClock.
func v1OriginUpdate(originClock uint64) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(30)
	enc.WriteVarUint(0)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(9)
	enc.WriteVarUint(originClock)
	enc.WriteVarString("q")
	encodeDeleteSet(enc, newDeleteSet())
	return enc.Bytes()
}

// v1ItemUpdate is a V1 update of one struct of client at clock, whose bytes
// after the group header item writes.
func v1ItemUpdate(client, clock uint64, item func(enc *encoding.Encoder)) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteVarUint(client)
	enc.WriteVarUint(clock)
	item(enc)
	encodeDeleteSet(enc, newDeleteSet())
	return enc.Bytes()
}

// v1String is a V1 update of client's "q" at clock on root text "t".
func v1String(client, clock uint64) []byte {
	return v1ItemUpdate(client, clock, func(enc *encoding.Encoder) {
		enc.WriteUint8(wireString)
		enc.WriteUint8(1)
		enc.WriteVarString("t")
		enc.WriteVarString("q")
	})
}

// v1Ref is a V1 update of client 30's "q" whose origin, right origin or
// parent (by flag; 0 for a parent) is the struct of refClient at refClock.
func v1Ref(flag byte, refClient, refClock uint64) []byte {
	return v1ItemUpdate(30, 0, func(enc *encoding.Encoder) {
		enc.WriteUint8(wireString | flag)
		if flag == 0 {
			enc.WriteUint8(0) // parent by ID
		}
		enc.WriteVarUint(refClient)
		enc.WriteVarUint(refClock)
		enc.WriteVarString("q")
	})
}

// v1Move is a V1 update of client 30's move of the range at clock 9:clock.
func v1Move(clock uint64) []byte {
	return v1ItemUpdate(30, 0, func(enc *encoding.Encoder) {
		enc.WriteUint8(wireMove)
		enc.WriteUint8(1)
		enc.WriteVarString("t")
		enc.WriteVarUint(9)
		enc.WriteVarUint(clock)
		enc.WriteVarUint(1)
	})
}

// v2GC is a V2 update of a GC struct of client 30 at clock, length long.
func v2GC(clock uint64, length int) []byte {
	gc := &Item{ID: ID{Client: 30, Clock: clock}, Content: NewContentDeleted(length), Deleted: true}
	return encodeStructStoreV2(map[ClientID][]*Item{30: {gc}}, newDeleteSet(), nil, New().store)
}

// v1Replaced is a V1 update with two groups of client 30, each one "qq" on
// root text "t": the first at clock, which the second, at 0, replaces.
func v1Replaced(clock uint64) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(2)
	for _, c := range []uint64{clock, 0} {
		enc.WriteVarUint(1)
		enc.WriteVarUint(30)
		enc.WriteVarUint(c)
		enc.WriteUint8(wireString)
		enc.WriteUint8(1)
		enc.WriteVarString("t")
		enc.WriteVarString("qq")
	}
	encodeDeleteSet(enc, newDeleteSet())
	return enc.Bytes()
}

// v2Replaced is v1Replaced as a V2 update.
func v2Replaced(clock uint64) []byte {
	root := New().GetText("t")
	enc := newV2Encoder()
	enc.restEnc.WriteVarUint(2)
	for _, c := range []uint64{clock, 0} {
		enc.restEnc.WriteVarUint(1)
		enc.writeClient(30)
		enc.restEnc.WriteVarUint(c)
		encodeItemV2(enc, &Item{ID: ID{Client: 30, Clock: c}, Parent: &root.abstractType, Content: NewContentString("qq")}, 0, New().store)
	}
	encodeDeleteSetV2(enc, newDeleteSet())
	return enc.toBytes()
}

// v2Item is a V2 update of one struct, item, of client 30 at clock 0.
func v2Item(item *Item) []byte {
	enc := newV2Encoder()
	enc.restEnc.WriteVarUint(1)
	enc.restEnc.WriteVarUint(1)
	enc.writeClient(30)
	enc.restEnc.WriteVarUint(0)
	encodeItemV2(enc, item, 0, New().store)
	encodeDeleteSetV2(enc, newDeleteSet())
	return enc.toBytes()
}

// v2Ref is a V2 update of client 30's "q" whose origin (or, with right, right
// origin) is client 9's struct at clock. The encoder writes a clock past 2^63
// as a negative one.
func v2Ref(right bool, clock uint64) []byte {
	ref := &ID{Client: 9, Clock: clock}
	item := &Item{ID: ID{Client: 30}, Content: NewContentString("q")}
	if right {
		item.OriginRight = ref
	} else {
		item.Origin = ref
	}
	return v2Item(item)
}

// v2Move is a V2 update of client 30's move of the range at 9:clock.
func v2Move(clock uint64) []byte {
	root := New().GetText("t")
	return v2Item(&Item{ID: ID{Client: 30}, Parent: &root.abstractType, Content: NewContentMove(&ID{Client: 9, Clock: clock}, 1)})
}

// No Yjs peer holds a clock past 2^53-1 (Number.MAX_SAFE_INTEGER), so both
// decoders, apply and merge alike, refuse a struct, origin, right-origin,
// parent, move or delete-range clock past it, V2 a negative origin clock, and
// V1 a client past what V2 can carry (2^55-1).
func TestUnit_ApplyUpdate_RefusesClocksPastMaxSafeInteger(t *testing.T) {
	for _, c := range []struct {
		name   string
		v2     bool
		update []byte
		ok     bool
	}{
		{"V1 range ending at 2^53", false, v1DeleteSetUpdate(1<<53-1, 1), true},
		{"V1 range at 2^53", false, v1DeleteSetUpdate(1<<53, 1), false},
		{"V1 range ending past 2^53", false, v1DeleteSetUpdate(1<<53-1, 2), false},
		{"V2 range ending at 2^53", true, v2DeleteSetUpdate(1<<53-1, 1), true},
		{"V2 range at 2^53", true, v2DeleteSetUpdate(1<<53, 1), false},
		{"V2 range at 2^55", true, v2DeleteSetUpdate(1<<55, 1), false},
		{"V2 range ending past 2^53", true, v2DeleteSetUpdate(1<<53-1, 2), false},
		{"V1 struct ending at 2^53", false, v1SkipUpdate(1, 1<<53-1), true},
		{"V1 struct past 2^53", false, v1SkipUpdate(1, 1<<53), false},
		{"V1 skips past 2^56", false, v1SkipUpdate(2, 1<<56-1), false},
		{"V1 origin at 2^53-1", false, v1OriginUpdate(1<<53 - 1), true},
		{"V1 origin at 2^53", false, v1OriginUpdate(1 << 53), false},
		{"V1 struct at 2^53-1", false, v1String(30, 1<<53-1), true},
		{"V1 struct at 2^53", false, v1String(30, 1<<53), false},
		{"V1 client 2^55-1", false, v1String(1<<55-1, 0), true},
		{"V1 client 2^55", false, v1String(1<<55, 0), false},
		{"V1 right origin at 2^53", false, v1Ref(flagHasRightOrigin, 9, 1<<53), false},
		{"V1 parent at 2^53", false, v1Ref(0, 9, 1<<53), false},
		{"V1 origin client 2^55", false, v1Ref(flagHasOrigin, 1<<55, 0), false},
		{"V1 right origin client 2^55", false, v1Ref(flagHasRightOrigin, 1<<55, 0), false},
		{"V1 parent client 2^55", false, v1Ref(0, 1<<55, 0), false},
		{"V1 move at 2^53", false, v1Move(1 << 53), false},
		{"V2 GC ending at 2^53", true, v2GC(1<<53-1, 1), true},
		{"V2 GC at 2^53", true, v2GC(1<<53, 1), false},
		{"V2 GC ending past 2^53", true, v2GC(1<<53-1, 2), false},
		{"V1 replaced group ending at 2^53", false, v1Replaced(1<<53 - 2), true},
		{"V1 replaced group ending past 2^53", false, v1Replaced(1<<53 - 1), false},
		{"V2 replaced group ending at 2^53", true, v2Replaced(1<<53 - 2), true},
		{"V2 replaced group ending past 2^53", true, v2Replaced(1<<53 - 1), false},
		{"V2 replaced group at 2^55", true, v2Replaced(1 << 55), false},
		{"V2 origin at 2^53-1", true, v2Ref(false, 1<<53-1), true},
		{"V2 origin at 2^53", true, v2Ref(false, 1<<53), false},
		{"V2 origin below zero", true, v2Ref(false, 1<<64-1), false},
		{"V2 right origin at 2^53", true, v2Ref(true, 1<<53), false},
		{"V2 right origin below zero", true, v2Ref(true, 1<<64-1), false},
		{"V2 move at 2^53-1", true, v2Move(1<<53 - 1), true},
		{"V2 move at 2^53", true, v2Move(1 << 53), false},
	} {
		var err, mergeErr error
		if c.v2 {
			err = ApplyUpdateV2(New(), c.update, nil)
			_, mergeErr = MergeUpdatesV2(c.update)
		} else {
			err = ApplyUpdateV1(New(), c.update, nil)
			_, mergeErr = MergeUpdatesV1(c.update)
		}
		for _, e := range []struct {
			path string
			err  error
		}{{"apply", err}, {"merge", mergeErr}} {
			if (e.err == nil) != c.ok {
				t.Errorf("%s: %s err %v, want accepted %v", c.name, e.path, e.err, c.ok)
			}
			if e.err != nil && !errors.Is(e.err, ErrInvalidUpdate) {
				t.Errorf("%s: %s err %v is not ErrInvalidUpdate", c.name, e.path, e.err)
			}
		}
	}
	for _, c := range []struct {
		clock int64
		ok    bool
	}{{-1, false}, {0, true}, {1<<53 - 1, true}, {1 << 53, false}} {
		if _, err := v2ID(1, c.clock); (err == nil) != c.ok {
			t.Errorf("v2ID clock %d: err %v, want accepted %v", c.clock, err, c.ok)
		}
	}
}

// parkedCD is a room that parks client 20's "cd" for want of its "ab", and
// the "ab" update.
func parkedCD(t *testing.T) (room *Doc, ab []byte) {
	t.Helper()
	author := New(WithClientID(20))
	text := author.GetText("t")
	author.Transact(func(txn *Transaction) { text.Insert(txn, 0, "ab", nil) })
	ab = EncodeStateAsUpdateV1(author, nil)
	before := author.StateVector()
	author.Transact(func(txn *Transaction) { text.Insert(txn, 2, "cd", nil) })
	room = New(WithClientID(1))
	if err := ApplyUpdateV1(room, EncodeStateAsUpdateV1(author, before), nil); err != nil {
		t.Fatal(err)
	}
	if got := room.PendingStats().Items; got != 1 {
		t.Fatalf("the room parks %d structs, want \"cd\"", got)
	}
	return room, ab
}

// requireStateKeepsCD fails t unless a copy loaded from room's V1 and V2 state,
// once it gets "ab", holds "abcd".
func requireStateKeepsCD(t *testing.T, room *Doc, ab []byte) {
	t.Helper()
	for _, v2 := range []bool{false, true} {
		copyDoc := New(WithClientID(2))
		var err error
		if v2 {
			err = ApplyUpdateV2(copyDoc, EncodeStateAsUpdateV2(room, nil), nil)
		} else {
			err = ApplyUpdateV1(copyDoc, EncodeStateAsUpdateV1(room, nil), nil)
		}
		if err != nil {
			t.Fatalf("V2=%v: the room's state does not load: %v", v2, err)
		}
		if err := ApplyUpdateV1(copyDoc, ab, nil); err != nil {
			t.Fatal(err)
		}
		if got := copyDoc.GetText("t").ToString(); got != "abcd" {
			t.Fatalf("V2=%v: a copy loaded from the room's state holds %q, want \"abcd\"", v2, got)
		}
	}
}

// A crafted V2 update (an origin clock that runs below zero) must not poison
// the room's state: a copy loaded from it keeps the parked "cd".
func TestUnit_EncodeState_KeepsParkedStructsAfterACraftedV2Update(t *testing.T) {
	room, ab := parkedCD(t)
	crafted, err := hex.DecodeString("0000024b01010001720544000a0084080520f09f9880010200000001030103050b0100e006")
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(room, crafted, nil)
	requireStateKeepsCD(t, room, ab)
}

// A V1 update whose skips move a clock past 2^56 must not poison the room's
// state: a copy loaded from it keeps the parked "cd".
func TestUnit_EncodeState_KeepsParkedStructsAfterASkipPast2To56(t *testing.T) {
	room, ab := parkedCD(t)
	_ = ApplyUpdateV1(room, v1SkipUpdate(2, 1<<56-1), nil)
	requireStateKeepsCD(t, room, ab)
}

// A V1 struct after an origin at clock 2^54 must not make the V2 state
// encoding panic (its clock columns hold 55 bits).
func TestUnit_EncodeStateAsUpdateV2_DoesNotPanicAfterAnOriginAt2To54(t *testing.T) {
	room := New(WithClientID(1))
	_ = ApplyUpdateV1(room, v1OriginUpdate(1<<54), nil)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EncodeStateAsUpdateV2 panicked: %v", r)
		}
	}()
	if err := ApplyUpdateV2(New(), EncodeStateAsUpdateV2(room, nil), nil); err != nil {
		t.Fatalf("the room's V2 state does not load: %v", err)
	}
}

// A parked part that does not merge is left out alone, at one merge per part,
// and a state that does not merge costs two merges whatever the part count.
func TestUnit_MergeParts_LeavesOutOnlyThePartThatDoesNotMerge(t *testing.T) {
	room, ab := parkedCD(t)
	// A parked struct whose origin clock V1 encodes and cannot read back, as
	// the V2 decoder could once park. (V2 cannot encode it at all.)
	room.store.pending.items = append(room.store.pending.items, &Item{
		ID: ID{Client: 30, Clock: 0}, Origin: &ID{Client: 31, Clock: 1 << 60},
		Content: NewContentString("q"),
	})
	copyDoc := New(WithClientID(2))
	if err := ApplyUpdateV1(copyDoc, EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatalf("the room's state does not load: %v", err)
	}
	if err := ApplyUpdateV1(copyDoc, ab, nil); err != nil {
		t.Fatal(err)
	}
	if got := copyDoc.GetText("t").ToString(); got != "abcd" {
		t.Fatalf("a copy loaded from the room's state holds %q, want \"abcd\"", got)
	}

	const n = 2000
	state := EncodeStateAsUpdateV1(New(), nil)
	parts := make([][]byte, 0, n+1)
	for i := range n {
		d := New(WithClientID(ClientID(100 + i)))
		dt := d.GetText("t")
		d.Transact(func(txn *Transaction) { dt.Insert(txn, 0, "x", nil) })
		parts = append(parts, EncodeStateAsUpdateV1(d, nil))
	}
	parts = append(parts, []byte{0xff}) // does not decode
	calls := 0
	merge := func(updates ...[]byte) ([]byte, error) {
		calls++
		return MergeUpdatesV1(updates...)
	}
	merged := mergeParts(state, parts, merge)
	doc := New()
	if err := ApplyUpdateV1(doc, merged, nil); err != nil {
		t.Fatal(err)
	}
	if got := doc.GetText("t").Len(); got != n {
		t.Fatalf("the merged state holds %d of the %d good parts", got, n)
	}
	if limit := n + 4; calls > limit {
		t.Fatalf("mergeParts called merge %d times, want at most %d", calls, limit)
	}

	calls = 0
	bad := []byte{0xff}
	if got := mergeParts(bad, parts, merge); !bytes.Equal(got, bad) {
		t.Fatalf("a state that does not merge came back as %x", got)
	}
	if calls != 2 {
		t.Fatalf("a state that does not merge cost %d merges, want 2", calls)
	}
}
