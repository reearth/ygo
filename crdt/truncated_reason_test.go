package crdt

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// manyWritersUpdate is a document that 60 forked clients edited, as a server
// that forks a fresh client per write stores it.
func manyWritersUpdate(t *testing.T) []byte {
	doc := New(WithClientID(1))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, strings.Repeat("seed text ", 40), nil) })
	for writer := 0; writer < 60; writer++ {
		fork := New(WithClientID(ClientID(1000 + writer*7919)))
		if err := ApplyUpdateV1(fork, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
			t.Fatal(err)
		}
		forked := fork.GetText("t")
		before := fork.StateVector()
		fork.Transact(func(txn *Transaction) {
			forked.Delete(txn, (writer*13)%300, 5)
			forked.Insert(txn, (writer*17)%300, fmt.Sprintf("edit %d ", writer), nil)
		})
		if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(fork, before), nil); err != nil {
			t.Fatal(err)
		}
	}
	return EncodeStateAsUpdateV1(doc, nil)
}

// A truncated update is refused with ErrInvalidUpdate named once and, past its
// first few structs, with the decoder's "unexpected end of input".
func TestUnit_ApplyUpdate_ATruncatedUpdateNamesWhyAtEveryLength(t *testing.T) {
	update := manyWritersUpdate(t)
	const reason = "encoding: unexpected end of input"
	for k := 1; k <= 200; k++ {
		cut := update[:len(update)*k/201]
		for name, target := range map[string]*Doc{"plain": New(), "capped": New(WithMaxPendingItems(1))} {
			err := ApplyUpdateV1(target, cut, nil)
			if !errors.Is(err, ErrInvalidUpdate) || strings.Count(err.Error(), ErrInvalidUpdate.Error()) != 1 {
				t.Errorf("%s document, %d of %d bytes: %v, want ErrInvalidUpdate named once", name, len(cut), len(update), err)
			} else if k > 10 && !strings.HasSuffix(err.Error(), ": "+reason) {
				t.Errorf("%s document, %d of %d bytes: %v, want it to name %q", name, len(cut), len(update), err, reason)
			}
		}
	}
}

// format is an update in one wire format with its apply.
type format struct {
	name   string
	update []byte
	apply  func(*Doc, []byte, any) error
}

// formats is a V1 update in V1 and in V2.
func formats(t *testing.T, v1 []byte) []format {
	t.Helper()
	v2, err := UpdateV1ToV2(v1)
	if err != nil {
		t.Fatal(err)
	}
	return []format{{"V1", v1, ApplyUpdateV1}, {"V2", v2, ApplyUpdateV2}}
}

// refuse applies update to a plain document and to one capped at capped items.
func refuse(f format, update []byte, capped int) (plain, cappedErr error) {
	return f.apply(New(), update, nil), f.apply(New(WithMaxPendingItems(capped)), update, nil)
}

// A V2 update cut to its group count, past every column, is refused for the
// read past the end, in a plain and in a capped document.
func TestUnit_ApplyUpdate_AV2UpdateCutInItsRestNamesWhy(t *testing.T) {
	one := oneClient()
	v2, err := UpdateV1ToV2(EncodeStateAsUpdateV1(one, nil))
	if err != nil {
		t.Fatal(err)
	}
	// The rest ends with the group count, the group's struct count, its clock
	// and an empty delete set, one byte each.
	cut := v2[:len(v2)-3]
	for _, target := range []*Doc{New(), New(WithMaxPendingItems(1))} {
		if err := ApplyUpdateV2(target, cut, nil); err == nil || !strings.HasSuffix(err.Error(), ": encoding: unexpected end of input") {
			t.Errorf("%d of %d bytes: %v, want it to name the end of input", len(cut), len(v2), err)
		}
	}
}

// A document whose pending cap makes the checks read an update before the
// decoder refuses it with the decoder's reason wherever the decoder gives one.
func TestUnit_ApplyUpdate_ACappedDocumentRefusesWithTheDecodersReason(t *testing.T) {
	same := func(name string, plain, capped error, suffix string) {
		t.Helper()
		if plain == nil || capped == nil || capped.Error() != plain.Error() || !strings.HasSuffix(plain.Error(), suffix) {
			t.Errorf("%s: capped %v, plain %v, want both to end %q", name, capped, plain, suffix)
		}
	}
	one := oneClient()
	values := New(WithClientID(3))
	array := values.GetArray("a")
	for i := range 20 {
		values.Transact(func(txn *Transaction) { array.Insert(txn, 0, []any{float64(i), "s", map[string]any{"k": true}}) })
	}
	parking := func(client ClientID, inserts ...int) []byte {
		return EncodeStateAsUpdateV1(parkingFork(t, one, client, inserts...), one.StateVector())
	}
	// Each insert at 0 is a struct of its own, as Yjs keeps it.
	many := make([]int, 20)
	updates := []struct {
		name string
		v1   []byte
		// exact is the cap under which every cut is refused with the plain
		// document's error, reason or none, as no decoder check the pending
		// checks skip applies to one group; 0 compares only where the plain
		// document gives a reason.
		exact int
		// tight is a cap of 1 under which every cut but the delete set's last
		// byte is refused as the plain document refuses it.
		tight bool
		// pastCap is whether the whole update parks past a one-item cap, which
		// refuses it for the cap alone.
		pastCap bool
		// column is whether the V2 update's client column, made to read past its
		// end, is refused for that.
		column bool
	}{
		{name: "one client", v1: EncodeStateAsUpdateV1(one, nil), exact: 1},
		{name: "array", v1: EncodeStateAsUpdateV1(values, nil), exact: 1, column: true},
		{name: "parking", v1: parking(8, many...), exact: 50, pastCap: true},
		{name: "two parking", v1: parking(9, 0, 9), exact: 50, tight: true, pastCap: true, column: true},
		{name: "json", v1: jsonUpdate(), exact: 1},
		{name: "many writers", v1: manyWritersUpdate(t)},
	}
	for _, u := range updates {
		for _, f := range formats(t, u.v1) {
			capped, differ := u.exact, 0
			if capped == 0 {
				capped = 1
			}
			for n := 1; n < len(f.update); n++ {
				plain, cappedErr := refuse(f, f.update[:n], capped)
				named := u.exact > 0 || (plain != nil && plain.Error() != ErrInvalidUpdate.Error())
				if plain == nil || cappedErr == nil || (named && cappedErr.Error() != plain.Error()) {
					if differ++; differ <= 3 {
						t.Errorf("%s %s, %d of %d bytes: capped %v, plain %v", u.name, f.name, n, len(f.update), cappedErr, plain)
					}
				}
			}
			if differ > 0 {
				t.Errorf("%s %s: %d of %d cuts differ", u.name, f.name, differ, len(f.update)-1)
			}
			for n := 1; u.tight && n < len(f.update)-1; n++ {
				if plain, cappedErr := refuse(f, f.update[:n], 1); plain == nil || cappedErr == nil || cappedErr.Error() != plain.Error() {
					t.Errorf("%s %s at a one-item cap, %d of %d bytes: capped %v, plain %v", u.name, f.name, n, len(f.update), cappedErr, plain)
					break
				}
			}
			if err := f.apply(New(WithMaxPendingItems(1)), f.update, nil); u.pastCap && (err == nil || err.Error() != ErrInvalidUpdate.Error()) {
				t.Errorf("%s %s past a one-item cap: %v, want %q", u.name, f.name, err, ErrInvalidUpdate)
			}
			if u.column && f.name == "V2" {
				// Its first byte, past the feature flag and the empty key column's
				// length and the client column's, continues its varint.
				bad := append([]byte(nil), f.update...)
				bad[3] ^= 0x80
				plain, cappedErr := refuse(f, bad, 1)
				same(u.name+" with a V2 column read past its end", plain, cappedErr, ": encoding: unexpected end of input")
			}
		}
	}
	// A group count, or a group's struct count, past the decoder's limit has no
	// reason to give.
	for _, counts := range [][]uint64{{maxV2Items + 1}, {2, maxV2Items + 1, 1, 0}} {
		v1, v2 := encoding.NewEncoder(), newV2Encoder()
		for _, n := range counts {
			v1.WriteVarUint(n)
		}
		v2.restEnc.WriteVarUint(counts[0])
		if len(counts) > 1 {
			v2.restEnc.WriteVarUint(counts[1])
			v2.writeClient(ClientID(counts[2]))
			v2.restEnc.WriteVarUint(counts[3])
		}
		for _, f := range []format{{"V1", v1.Bytes(), ApplyUpdateV1}, {"V2", v2.toBytes(), ApplyUpdateV2}} {
			if plain, capped := refuse(f, f.update, 1); plain == nil || capped == nil || plain.Error() != ErrInvalidUpdate.Error() || capped.Error() != ErrInvalidUpdate.Error() {
				t.Errorf("%s counts %v: capped %v, plain %v, want %q", f.name, counts, capped, plain, ErrInvalidUpdate)
			}
		}
	}
	// The one-client update's first struct info byte follows its 4 header bytes.
	tagged := append([]byte(nil), updates[0].v1...)
	tagged[4] = tagged[4]&^0x1f | 12
	plain, capped := refuse(format{"V1", tagged, ApplyUpdateV1}, tagged, 1)
	same("an unknown content tag", plain, capped, ": unknown content tag: 12")
	for _, v2 := range []bool{false, true} {
		name, want, apply := "V1", ": unknown content tag: 31", ApplyUpdateV1
		if v2 {
			name, want, apply = "V2", ": unknown V2 content tag: 31", ApplyUpdateV2
		}
		version := 1
		if v2 {
			version = 2
		}
		for what, update := range map[string][]byte{
			"groups, then a struct":      repeatedGroupsUpdate(t, version, true),
			"one group of a struct":      withUnknownTag(New(), 10, v2),
			"one group, its last struct": withUnknownTag(one, 7, v2),
		} {
			plain, capped := refuse(format{name, update, apply}, update, 1)
			same(name+" "+what+" of unknown tag 31", plain, capped, want)
		}
	}
}

// withUnknownTag encodes client's structs of doc, then one more of unknown
// content tag 31, as one group.
func withUnknownTag(doc *Doc, client ClientID, v2 bool) []byte {
	items := doc.store.clients[client]
	if !v2 {
		enc := encoding.NewEncoder()
		for _, n := range []uint64{1, uint64(len(items) + 1), uint64(client), 0} {
			enc.WriteVarUint(n)
		}
		for _, it := range items {
			encodeItem(enc, it, 0, doc.store)
		}
		enc.WriteUint8(0x1f)
		enc.WriteUint8(1)
		enc.WriteVarString("t")
		encodeDeleteSet(enc, newDeleteSet())
		return enc.Bytes()
	}
	e2 := newV2Encoder()
	e2.restEnc.WriteVarUint(1)
	e2.restEnc.WriteVarUint(uint64(len(items) + 1))
	e2.writeClient(client)
	e2.restEnc.WriteVarUint(0)
	for _, it := range items {
		encodeItemV2(e2, it, 0, doc.store)
	}
	e2.writeInfo(0x1f)
	e2.writeParentInfo(true)
	e2.writeString("t")
	encodeDeleteSetV2(e2, newDeleteSet())
	return e2.toBytes()
}

// A scan's unknown-tag error, even where a check returns it unmapped, names the
// tag as the decoder of its format does.
func TestUnit_ContentTagError_NamesTheTagAsItsDecoderDoes(t *testing.T) {
	for _, c := range []struct {
		v2   bool
		want string
	}{{false, "unknown content tag: 31"}, {true, "unknown V2 content tag: 31"}} {
		err := tagError(31, c.v2)
		if err.Error() != c.want || scanReason(err).Error() != c.want {
			t.Errorf("V2 %v: %q, mapped %q, want %q", c.v2, err.Error(), scanReason(err), c.want)
		}
	}
}

// A group whose first struct has an unknown tag, cut anywhere, is refused by a
// capped document as by the plain one: the checks read the group's clock
// before the tag, and keep the first fault they meet, as the decoder does.
func TestUnit_ApplyUpdate_ACutGroupWithAnUnknownFirstTagRefusesAsTheDecoderDoes(t *testing.T) {
	v1 := encoding.NewEncoder()
	for _, n := range []uint64{1, 2, 7, 0} {
		v1.WriteVarUint(n)
	}
	v1.WriteUint8(0x1f)
	v1.WriteUint8(1)
	v1.WriteVarString("t")
	v1.WriteUint8(wireString)
	v1.WriteUint8(1)
	v1.WriteVarString("t")
	v1.WriteVarString("a")
	encodeDeleteSet(v1, newDeleteSet())
	e2 := newV2Encoder()
	e2.restEnc.WriteVarUint(1)
	e2.restEnc.WriteVarUint(2)
	e2.writeClient(7)
	e2.restEnc.WriteVarUint(0)
	e2.writeInfo(0x1f)
	e2.writeParentInfo(true)
	e2.writeString("t")
	e2.writeInfo(wireString)
	e2.writeParentInfo(true)
	e2.writeString("t")
	e2.writeString("a")
	encodeDeleteSetV2(e2, newDeleteSet())
	for _, f := range []format{{"V1", v1.Bytes(), ApplyUpdateV1}, {"V2", e2.toBytes(), ApplyUpdateV2}} {
		for n := 1; n <= len(f.update); n++ {
			if plain, capped := refuse(f, f.update[:n], 1); plain == nil || capped == nil || capped.Error() != plain.Error() {
				t.Errorf("%s, %d of %d bytes: capped %v, plain %v", f.name, n, len(f.update), capped, plain)
			}
		}
	}
}

// scanReason keeps each decoder error value and maps any other error, but an
// unknown tag, to ErrInvalidUpdate.
func TestUnit_ScanReason_KeepsTheDecodersReasons(t *testing.T) {
	for _, reason := range []error{encoding.ErrUnexpectedEOF, encoding.ErrOverflow, encoding.ErrUnknownTag, encoding.ErrInvalidUTF8, encoding.ErrDepthExceeded} {
		if got := scanReason(reason); got != reason {
			t.Errorf("scanReason(%v) = %v", reason, got)
		}
	}
	if got := scanReason(errors.New("other")); got != ErrInvalidUpdate {
		t.Errorf("scanReason(other) = %v, want ErrInvalidUpdate", got)
	}
}

// A document with room for one more pending item refuses a cut of an update
// that parks two structs, or one struct then an unknown tag, with the plain
// document's reason.
func TestUnit_ApplyUpdate_ADocumentWithLittleRoomRefusesWithTheDecodersReason(t *testing.T) {
	one := oneClient()
	const parkedItems = 20
	parked := EncodeStateAsUpdateV1(parkingFork(t, one, 8, make([]int, parkedItems)...), one.StateVector())
	room := parkedItems + 1
	target := func(capped int) *Doc {
		doc := New()
		if capped > 0 {
			doc = New(WithMaxPendingItems(capped))
		}
		if err := ApplyUpdateV1(doc, parked, nil); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	two := EncodeStateAsUpdateV1(parkingFork(t, one, 9, 0, 0), one.StateVector())
	for _, f := range formats(t, two) {
		if err := f.apply(target(room), f.update, nil); err == nil || err.Error() != ErrInvalidUpdate.Error() {
			t.Errorf("%s whole: %v, want %q", f.name, err, ErrInvalidUpdate)
		}
		// Every cut but the delete set's last byte leaves the second struct
		// unread.
		for n := 1; n < len(f.update)-1; n++ {
			plain, capped := f.apply(target(0), f.update[:n], nil), f.apply(target(room), f.update[:n], nil)
			if plain == nil || capped == nil || capped.Error() != plain.Error() {
				t.Errorf("%s, %d of %d bytes: capped %v, plain %v", f.name, n, len(f.update), capped, plain)
				break
			}
		}
	}
	// One parking struct fits the room; the unknown tag after it is the reason.
	single := parkingFork(t, one, 10, 0)
	for _, c := range []struct {
		name, want string
		apply      func(*Doc, []byte, any) error
		update     []byte
	}{
		{"V1", ": unknown content tag: 31", ApplyUpdateV1, withUnknownTag(single, 10, false)},
		{"V2", ": unknown V2 content tag: 31", ApplyUpdateV2, withUnknownTag(single, 10, true)},
	} {
		plain, capped := c.apply(target(0), c.update, nil), c.apply(target(room), c.update, nil)
		if !errors.Is(capped, ErrInvalidUpdate) || plain == nil || capped.Error() != plain.Error() || !strings.HasSuffix(plain.Error(), c.want) {
			t.Errorf("%s parking struct then an unknown tag: capped %v, plain %v, want both to end %q", c.name, capped, plain, c.want)
		}
	}
}

// oneClient is a document of 40 inserts by client 7, each a struct of its own.
func oneClient() *Doc {
	one := New(WithClientID(7))
	text := one.GetText("t")
	for i := range 40 {
		one.Transact(func(txn *Transaction) { text.Insert(txn, 0, fmt.Sprintf("word %d ", i), nil) })
	}
	return one
}

// parkingFork is client's fork of one with an insert of "p" at each index; one
// struct each where the inserts do not follow each other, as at index 0.
func parkingFork(t *testing.T, one *Doc, client ClientID, inserts ...int) *Doc {
	t.Helper()
	fork := New(WithClientID(client))
	if err := ApplyUpdateV1(fork, EncodeStateAsUpdateV1(one, nil), nil); err != nil {
		t.Fatal(err)
	}
	forked := fork.GetText("t")
	for _, at := range inserts {
		fork.Transact(func(txn *Transaction) { forked.Insert(txn, at, "p", nil) })
	}
	return fork
}

// jsonUpdate is one group of JSON content, the encoding older Yjs writes.
func jsonUpdate() []byte {
	src := New()
	text := src.GetText("t")
	items := []*Item{{ID: ID{Client: 5}, Parent: &text.abstractType, Content: NewContentJSON("a", 1.5, map[string]any{"k": true})}}
	return encodeStructStoreV1(map[ClientID][]*Item{5: items}, newDeleteSet(), nil, src.store)
}
