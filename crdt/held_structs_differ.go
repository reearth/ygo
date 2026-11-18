package crdt

import (
	"bytes"
	"slices"
	"sort"
	"unicode/utf16"

	"github.com/reearth/ygo/encoding"
)

// HeldStructsDifferV1 reports whether the V1 update carries a struct of
// client, at a clock from from on, that differs from what doc holds at the
// same (client, clock). Each clock that a struct of the update and a struct
// doc holds both cover is one content unit, and the two differ when that
// unit's content differs (a UTF-16 unit of a string, a value of an array or
// JSON content, or the whole of a binary, embed, format, type, subdocument or
// move, compared as its V1 encoding), or its origin (the struct's own origin
// at its first clock, and the unit before it at every later one), its right
// origin, or, for a unit that names its place by its parent rather than by
// origins, its parent (a root type's name, or the ID of the item holding a
// nested type) or its key (ParentSub). So two structs holding the same units
// cut at other clocks do not differ, as a struct split there would not.
//
// Only what both sides still say is compared. A struct doc does not hold -
// past doc's clock for client, or parked in its pending queue - is not
// compared, so an update that only extends what doc holds never differs.
// Neither is a GC struct on either side, which keeps no origins or content,
// nor the content of a unit either side holds as deleted content
// (ContentDeleted, which a document collecting garbage keeps for deleted
// content). Such a struct keeps its length, the clocks it covers, so each of
// its units is still found by its clock and compared by its origin, right
// origin, parent and key, but not by its content: when either side holds
// deleted content, only its origins, parent and key are compared, so if doc
// has collected deleted text, a second writer with the same origins but
// other content there reports no difference. Comparing content doc no
// longer holds would report a difference for an honest client whose own
// text another client deleted. A clock before from is not compared, and nor
// is any other client's struct.
//
// It is for a server that tells an honest resend of a client's structs from
// a second writer under the same client ID: an apply skips every struct at
// clocks the document already holds, so the second writer's content would be
// dropped without a word, and the same (client, clock) with other content is
// what shows two documents wrote it.
//
// Cost: update is decoded whole, as DocContainsUpdateV1 decodes it, before
// doc is locked and without integrating it. Then, under doc's read lock,
// each struct of client the update carries from from on is found in doc's
// store by one binary search over client's structs, and the structs doc
// holds at its clocks are walked forward and their units compared: linear in
// the update's bytes and in the content of the structs compared, plus a
// search per struct. A string is converted to UTF-16 whole, once for each
// struct whose units are compared, so a long run doc holds costs its length
// even where one unit of it is compared. An update that does not decode
// returns false and an error wrapping ErrInvalidUpdate. Do not call it from
// a Transact callback on doc, which holds doc's lock already.
func HeldStructsDifferV1(doc *Doc, update []byte, client ClientID, from uint64) (bool, error) {
	structs, _, err := decodeStructsV1(New(), update)
	if err != nil {
		return false, err
	}
	sent := structs[client]
	if len(sent) == 0 {
		return false, nil
	}
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	held := doc.store.clients[client]
	if len(held) == 0 {
		return false, nil
	}
	last := held[len(held)-1]
	heldEnd := last.ID.Clock + uint64(last.Content.Len())
	var units unitCache
	for _, u := range sent {
		lo := max(u.ID.Clock, from)
		hi := min(u.ID.Clock+uint64(u.Content.Len()), heldEnd)
		if lo >= hi || u.isGCOrphan() {
			continue
		}
		i := max(sort.Search(len(held), func(i int) bool { return held[i].ID.Clock > lo })-1, 0)
		for ; i < len(held) && held[i].ID.Clock < hi; i++ {
			h := held[i]
			a, b := max(lo, h.ID.Clock), min(hi, h.ID.Clock+uint64(h.Content.Len()))
			if a >= b || h.isGCOrphan() {
				continue
			}
			if unitsDiffer(u, h, a, b, &units) {
				return true, nil
			}
		}
	}
	return false, nil
}

// unitsDiffer is whether struct u of an update and struct h of a store differ
// at clocks [a, b), which both cover (HeldStructsDifferV1).
func unitsDiffer(u, h *Item, a, b uint64, units *unitCache) bool {
	if unitOrigin(u, a) != unitOrigin(h, a) || optionalID(u.OriginRight) != optionalID(h.OriginRight) {
		return true
	}
	// A unit with no origin and no right origin names its parent and key;
	// one with an origin is placed by it, and its parent is the origin's.
	// Equal origins at a leave h's unit with none either.
	if a == u.ID.Clock && u.Origin == nil && u.OriginRight == nil &&
		(parentOf(u) != parentOf(h) || !parentSubEqual(u.ParentSub, h.ParentSub)) {
		return true
	}
	return contentDiffers(u, h, a-u.ID.Clock, a-h.ID.Clock, b-a, units)
}

// idOrNone is an optional ID as a comparable value.
type idOrNone struct {
	id  ID
	set bool
}

func optionalID(id *ID) idOrNone {
	if id == nil {
		return idOrNone{}
	}
	return idOrNone{id: *id, set: true}
}

// unitOrigin is the origin of item's unit at clock: the item's own origin at
// its first clock, and the unit before it after that, as a split there gives
// the right part.
func unitOrigin(item *Item, clock uint64) idOrNone {
	if clock == item.ID.Clock {
		return optionalID(item.Origin)
	}
	return idOrNone{id: ID{Client: item.ID.Client, Clock: clock - 1}, set: true}
}

// structParent is where an item without origins says it lives: a root type by
// name, or a nested type by the ID of the item holding it.
type structParent struct {
	root   string
	nested idOrNone
}

// parentOf is item's parent as its V1 encoding names it: a struct decoded
// without integrating it holds a root parent as a type and a nested one as
// its parent ID, and an integrated struct holds either as a type.
func parentOf(item *Item) structParent {
	switch {
	case item.Parent != nil && item.Parent.item != nil:
		return structParent{nested: optionalID(&item.Parent.item.ID)}
	case item.Parent != nil:
		return structParent{root: item.Parent.name}
	default:
		return structParent{nested: optionalID(item.parentID)}
	}
}

// contentDiffers is whether n units of u's content from offset ui and of
// h's from offset hi differ. Deleted content on either side is not compared.
func contentDiffers(u, h *Item, ui, hi, n uint64, units *unitCache) bool {
	uc, hc := u.Content, h.Content
	_, uDeleted := uc.(*ContentDeleted)
	_, hDeleted := hc.(*ContentDeleted)
	if uDeleted || hDeleted {
		return false
	}
	if contentTagOf(uc) != contentTagOf(hc) {
		return true
	}
	switch x := uc.(type) {
	case *ContentString:
		us, hs := units.of(&units.sent, u, x), units.of(&units.held, h, hc.(*ContentString))
		if ui+n > uint64(len(us)) || hi+n > uint64(len(hs)) {
			return true
		}
		return !slices.Equal(us[ui:ui+n], hs[hi:hi+n])
	case *ContentAny:
		return valuesDiffer(x.Vals, hc.(*ContentAny).Vals, ui, hi, n, func(enc *encoding.Encoder, v any) { enc.WriteAny(v) })
	case *ContentJSON:
		return valuesDiffer(x.Vals, hc.(*ContentJSON).Vals, ui, hi, n, func(enc *encoding.Encoder, v any) { enc.WriteVarString(fmtValToJSON(v)) })
	default:
		// Every other content is one unit long.
		return !bytes.Equal(encodedContent(uc), encodedContent(hc))
	}
}

// valuesDiffer is whether n values of us from ui and of hs from hi encode
// differently.
func valuesDiffer(us, hs []any, ui, hi, n uint64, write func(*encoding.Encoder, any)) bool {
	if ui+n > uint64(len(us)) || hi+n > uint64(len(hs)) {
		return true
	}
	for k := range n {
		a := encoding.EncodeBytes(func(enc *encoding.Encoder) { write(enc, us[ui+k]) })
		b := encoding.EncodeBytes(func(enc *encoding.Encoder) { write(enc, hs[hi+k]) })
		if !bytes.Equal(a, b) {
			return true
		}
	}
	return false
}

// encodedContent is c as an item's V1 encoding writes it.
func encodedContent(c Content) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) { encodeContent(enc, c, 0) })
}

// unitCache keeps the UTF-16 units of the string struct last compared on
// each side, since consecutive comparisons mostly reuse one of them.
type unitCache struct {
	sent, held unitsOf
}

type unitsOf struct {
	item  *Item
	units []uint16
}

// of returns c's UTF-16 units, the units item's string holds, from slot when
// it holds item's.
func (*unitCache) of(slot *unitsOf, item *Item, c *ContentString) []uint16 {
	if slot.item != item {
		slot.item, slot.units = item, utf16.Encode([]rune(c.Str))
	}
	return slot.units
}
