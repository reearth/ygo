package crdt

import (
	"errors"

	"github.com/reearth/ygo/encoding"
)

// ErrInvalidRelativePosition is returned when a RelativePosition cannot be
// decoded from its binary representation.
var ErrInvalidRelativePosition = errors.New("crdt: invalid relative position")

// RelativePosition encodes a document cursor that remains stable as items are
// inserted or deleted around it. It anchors to a specific Item by ID rather
// than to a numeric index, so concurrent edits cannot shift it.
//
// Convert to an AbsolutePosition via ToAbsolutePosition whenever you need the
// current numeric index (e.g. before displaying a cursor or applying an edit).
//
// Wire format is compatible with Y.RelativePosition in the JavaScript Yjs
// reference implementation.
type RelativePosition struct {
	// Item is the ID of the item this position is anchored to.
	// Nil means the position is at the very start of the named root type (Tname).
	Item *ID

	// Tname is the root type name; used when Item is nil.
	Tname string

	// Assoc controls which side of Item this position is on.
	//   Assoc >= 0: cursor is after Item (default — use for most cursors).
	//   Assoc <  0: cursor is before Item (use when cursor is at end of type,
	//               since assoc=0 cannot stably represent end-of-type).
	Assoc int
}

// AbsolutePosition is the result of resolving a RelativePosition against the
// current document state. Index is expressed in UTF-16 code units (matching
// Yjs / JavaScript semantics) for YText, or in item count for YArray/YMap.
type AbsolutePosition struct {
	// Name is the root type name the position belongs to.
	Name string
	// Index is the logical position within the type.
	Index int
	// Assoc is carried over from the RelativePosition.
	Assoc int
}

// CreateRelativePositionFromIndex creates a RelativePosition that points to
// logical index within t. The position is anchored to the item at that index
// so that subsequent insertions elsewhere do not shift it.
//
// assoc controls which side of the boundary item the cursor is on:
//   - assoc >= 0 (default): cursor is after the anchor — use for most cursors.
//   - assoc <  0: cursor is before the anchor — required when positioning at
//     the exact end of a type (there is no item to the right to anchor to).
//
// The algorithm matches createRelativePositionFromTypeIndex in the Yjs JS
// reference implementation.
func CreateRelativePositionFromIndex(t sharedType, index int, assoc int) RelativePosition {
	at := t.baseType()
	name := at.name

	if assoc < 0 {
		if index == 0 {
			return RelativePosition{Tname: name, Assoc: assoc}
		}
		// For assoc < 0, anchor to the item whose right boundary sits at index.
		// Decrement so the walk stops at that item's left boundary.
		index--
	}

	remaining := index
	for item := at.start; item != nil; item = item.Right {
		if item.Deleted || !item.Content.IsCountable() {
			continue
		}
		n := item.Content.Len()
		if n > remaining {
			// Anchor to the specific clock within this item.
			id := ID{Client: item.ID.Client, Clock: item.ID.Clock + uint64(remaining)}
			return RelativePosition{Item: &id, Assoc: assoc}
		}
		remaining -= n
		// For assoc < 0 at the last item: anchor to that item's last clock.
		if item.Right == nil && assoc < 0 {
			lastClock := item.ID.Clock + uint64(item.Content.Len()) - 1
			return RelativePosition{Item: &ID{Client: item.ID.Client, Clock: lastClock}, Assoc: assoc}
		}
	}

	// index >= length with assoc >= 0: no item to the right.
	// Return a start-of-type anchor. Use assoc < 0 if you need a stable
	// end-of-type position.
	return RelativePosition{Tname: name, Assoc: assoc}
}

// ToAbsolutePosition resolves rp against doc's current state and returns the
// logical index within the named type.
//
// Returns (pos, true) on success. Returns (zero, false) if the anchor item no
// longer exists (e.g. it was GC'd after the position was created).
//
// The resolution algorithm matches toAbsolutePosition in the Yjs JS reference
// implementation.
func ToAbsolutePosition(doc *Doc, rp RelativePosition) (AbsolutePosition, bool) {
	doc.mu.Lock()
	defer doc.mu.Unlock()

	if rp.Item == nil {
		// Null anchor = start of the named type (index 0).
		return AbsolutePosition{Name: rp.Tname, Index: 0, Assoc: rp.Assoc}, true
	}

	item := doc.store.Find(*rp.Item)
	if item == nil {
		return AbsolutePosition{}, false
	}
	at := item.Parent
	if at == nil {
		return AbsolutePosition{}, false
	}

	// diff is the 0-based offset of the anchor clock within the item.
	// For assoc < 0 we add 1 so that the resolved index is to the LEFT of the
	// anchor character (i.e. includes the anchor in the count).
	diff := int(rp.Item.Clock-item.ID.Clock)
	if rp.Assoc < 0 {
		diff++
	}

	// Count all non-deleted countable items that come before anchor_item in the
	// linked list. These contribute to the absolute index.
	index := 0
	for cur := at.start; cur != nil; cur = cur.Right {
		if cur == item {
			break
		}
		if !cur.Deleted && cur.Content.IsCountable() {
			index += cur.Content.Len()
		}
	}
	index += diff

	return AbsolutePosition{Name: at.name, Index: index, Assoc: rp.Assoc}, true
}

// EncodeRelativePosition serialises rp to bytes using the Yjs wire format.
// The encoding is compatible with Y.encodeRelativePosition in the JS Yjs
// reference implementation.
//
// Wire layout:
//   - If Item != nil:  VarUint(1) + VarUint(client) + VarUint(clock) + VarInt(assoc)
//   - If Item == nil:  VarUint(2) + VarString(tname) + VarInt(assoc)
func EncodeRelativePosition(rp RelativePosition) []byte {
	enc := encoding.NewEncoder()
	if rp.Item != nil {
		enc.WriteVarUint(1)
		enc.WriteVarUint(uint64(rp.Item.Client))
		enc.WriteVarUint(rp.Item.Clock)
	} else {
		enc.WriteVarUint(2)
		enc.WriteVarString(rp.Tname)
	}
	enc.WriteVarInt(int64(rp.Assoc))
	return enc.Bytes()
}

// DecodeRelativePosition parses bytes produced by EncodeRelativePosition.
func DecodeRelativePosition(data []byte) (RelativePosition, error) {
	dec := encoding.NewDecoder(data)
	kind, err := dec.ReadVarUint()
	if err != nil {
		return RelativePosition{}, ErrInvalidRelativePosition
	}

	var rp RelativePosition
	switch kind {
	case 1: // anchored to a specific item ID
		client, err := dec.ReadVarUint()
		if err != nil {
			return RelativePosition{}, ErrInvalidRelativePosition
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return RelativePosition{}, ErrInvalidRelativePosition
		}
		id := ID{Client: ClientID(client), Clock: clock}
		rp.Item = &id
	case 2: // start of named type
		name, err := dec.ReadVarString()
		if err != nil {
			return RelativePosition{}, ErrInvalidRelativePosition
		}
		rp.Tname = name
	default:
		return RelativePosition{}, ErrInvalidRelativePosition
	}

	assoc, err := dec.ReadVarInt()
	if err != nil {
		return RelativePosition{}, ErrInvalidRelativePosition
	}
	rp.Assoc = int(assoc)
	return rp, nil
}
