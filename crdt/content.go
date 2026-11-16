package crdt

import (
	"unicode/utf8"

	"github.com/reearth/ygo/encoding"
)

// Content is the payload carried by an Item.
// Every concrete content type must implement this interface.
type Content interface {
	// Len returns how many logical positions this content occupies.
	// For ContentString this is the number of UTF-16 code units, matching Yjs
	// wire-protocol semantics (JavaScript's string length model).
	Len() uint64
	// IsCountable reports whether this content contributes to a type's length.
	// Deleted and format-marker content do not count.
	IsCountable() bool
	// Copy returns a deep copy.
	Copy() Content
	// Splice splits the content at offset, mutates the receiver to hold [0, offset),
	// and returns a new Content holding [offset, Len()).
	Splice(offset uint64) Content
}

// nativeContentLen bounds a materialized content length before using a rendered position.
func nativeContentLen(c Content) int {
	n := c.Len()
	if n > uint64(^uint(0)>>1) {
		panic("crdt: content length exceeds native positions")
	}
	return int(n)
}

// utf16Len returns the number of UTF-16 code units in s.
// Characters in the Basic Multilingual Plane (U+0000–U+FFFF) count as 1 unit;
// supplementary characters (U+10000 and above, e.g. most emoji) count as 2.
// This matches JavaScript's String.length and the Yjs wire-protocol index model.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// replacementChar is U+FFFD, emitted when a split lands inside a surrogate pair.
const replacementChar = "�"

// splitUTF16 splits s at the given UTF-16 code-unit offset and returns the
// (left, right) substrings. Total UTF-16 length is preserved.
//
// If offset falls in the middle of a surrogate pair — i.e. it bisects a
// supplementary character (U+10000 and above), which occupies 2 UTF-16 code
// units — the straddled character is replaced by U+FFFD on BOTH halves. This
// matches how JavaScript (and therefore Yjs) slices a string mid-surrogate:
// String.prototype.slice yields a lone surrogate on each side, which normalises
// to the replacement character. The 2-unit character becomes two 1-unit
// replacement chars, so left has exactly `offset` units and right the rest.
// Verified against yjs@13.6.30; see TestUnit_YText_Insert_MidSurrogate_MatchesYjs.
func splitUTF16(s string, offset int) (left, right string) {
	u16 := 0
	for i, r := range s {
		if u16 == offset {
			return s[:i], s[i:]
		}
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if u16+w > offset {
			// offset bisects this supplementary rune (w == 2, u16 == offset-1).
			runeBytes := utf8.RuneLen(r)
			return s[:i] + replacementChar, replacementChar + s[i+runeBytes:]
		}
		u16 += w
	}
	// offset >= utf16Len(s): everything is on the left.
	return s, ""
}

// ContentDeleted is a tombstone. It replaces real content when an item is
// deleted but must stay in the linked list to preserve position references.
type ContentDeleted struct{ length uint64 }

func NewContentDeleted(length uint64) *ContentDeleted { return &ContentDeleted{length} }
func (c *ContentDeleted) Len() uint64                 { return c.length }
func (c *ContentDeleted) IsCountable() bool           { return false }
func (c *ContentDeleted) Copy() Content               { return &ContentDeleted{c.length} }
func (c *ContentDeleted) Splice(offset uint64) Content {
	right := &ContentDeleted{c.length - offset}
	c.length = offset
	return right
}

// ContentString holds a run of UTF-8 text from a single client.
// Multiple consecutive characters typed by the same client are squashed into
// one item, keeping the linked list short.
//
// utf16Len caches the UTF-16 code unit length of Str so that Len() is O(1).
// The Yjs wire protocol and JavaScript String.length both count in UTF-16 units,
// so indices exchanged with JS peers must use the same unit. Characters in the
// Basic Multilingual Plane count as 1; supplementary characters (e.g. most emoji)
// count as 2.
// Any code that mutates Str directly must also update utf16Len.
type ContentString struct {
	Str      string
	utf16Len int
}

func NewContentString(s string) *ContentString {
	return &ContentString{Str: s, utf16Len: utf16Len(s)}
}

// Len returns the number of UTF-16 code units, matching Yjs wire-protocol
// index semantics. This is NOT the same as len(s) (bytes) or rune count when
// the string contains characters outside the Basic Multilingual Plane.
func (c *ContentString) Len() uint64       { return uint64(c.utf16Len) }
func (c *ContentString) IsCountable() bool { return true }
func (c *ContentString) Copy() Content {
	return &ContentString{Str: c.Str, utf16Len: c.utf16Len}
}
func (c *ContentString) Splice(offset uint64) Content {
	if offset > uint64(c.utf16Len) {
		panic("crdt: ContentString splice offset out of bounds")
	}
	n := int(offset)
	left, right := splitUTF16(c.Str, n)
	rightLen := c.utf16Len - n
	c.Str = left
	c.utf16Len = n
	return &ContentString{Str: right, utf16Len: rightLen}
}

// ContentBinary holds raw bytes (e.g. binary file attachments).
type ContentBinary struct{ Data []byte }

func NewContentBinary(b []byte) *ContentBinary { return &ContentBinary{b} }
func (c *ContentBinary) Len() uint64           { return 1 }
func (c *ContentBinary) IsCountable() bool     { return true }
func (c *ContentBinary) Copy() Content {
	cp := make([]byte, len(c.Data))
	copy(cp, c.Data)
	return &ContentBinary{cp}
}
func (c *ContentBinary) Splice(_ uint64) Content { panic("crdt: ContentBinary is not splittable") }

// ContentAny holds a slice of arbitrary JSON-compatible values.
// Used by YArray when storing heterogeneous elements.
type ContentAny struct{ Vals []any }

// normalizeAnyScalar maps a Go scalar to the type it will hold once it has
// round-tripped through a V1/V2 update: Go int becomes int64 (ReadAny returns
// int64 for VarInt-encoded integers). Everything else is returned unchanged.
// It is the single source of truth for this normalisation so that values
// buffered locally (e.g. detached XML prelim attributes) read back with the
// exact same type as values that were actually encoded — see NewContentAny and
// YXmlElement.SetAttributeValue.
func normalizeAnyScalar(v any) any {
	if n, ok := v.(int); ok {
		return int64(n)
	}
	return v
}

// NewContentAny creates a ContentAny, normalising Go int values to int64 so
// that locally-stored integers are wire-compatible with values decoded from
// V1/V2 updates (ReadAny returns int64 for VarInt-encoded integers).
func NewContentAny(vals ...any) *ContentAny {
	normalized := make([]any, len(vals))
	for i, v := range vals {
		normalized[i] = normalizeAnyScalar(v)
	}
	return &ContentAny{normalized}
}
func (c *ContentAny) Len() uint64       { return uint64(len(c.Vals)) }
func (c *ContentAny) IsCountable() bool { return true }
func (c *ContentAny) Copy() Content {
	cp := make([]any, len(c.Vals))
	copy(cp, c.Vals)
	return &ContentAny{cp}
}
func (c *ContentAny) Splice(offset uint64) Content {
	right := &ContentAny{append([]any{}, c.Vals[offset:]...)}
	c.Vals = c.Vals[:offset]
	return right
}

// ContentJSON holds legacy JSON-serializable values. Functionally equivalent
// to ContentAny; kept separate to maintain wire-format compatibility.
type ContentJSON struct{ Vals []any }

func NewContentJSON(vals ...any) *ContentJSON { return &ContentJSON{vals} }
func (c *ContentJSON) Len() uint64            { return uint64(len(c.Vals)) }
func (c *ContentJSON) IsCountable() bool      { return true }
func (c *ContentJSON) Copy() Content {
	cp := make([]any, len(c.Vals))
	copy(cp, c.Vals)
	return &ContentJSON{cp}
}
func (c *ContentJSON) Splice(offset uint64) Content {
	right := &ContentJSON{append([]any{}, c.Vals[offset:]...)}
	c.Vals = c.Vals[:offset]
	return right
}

// plainVals returns the values of a plain-value item: ContentAny, or the
// legacy ContentJSON that every reader must treat identically.
func plainVals(c Content) ([]any, bool) {
	switch ct := c.(type) {
	case *ContentAny:
		return ct.Vals, true
	case *ContentJSON:
		return ct.Vals, true
	}
	return nil, false
}

// lastPlainVal returns a map entry's value: the last value of a plain-value
// item, as Yjs reads getContent()[length-1].
func lastPlainVal(c Content) (any, bool) {
	vals, ok := plainVals(c)
	if !ok || len(vals) == 0 {
		return nil, false
	}
	return vals[len(vals)-1], true
}

// ContentEmbed holds a single embedded object (e.g. an image or formula in rich text).
type ContentEmbed struct{ Val any }

func NewContentEmbed(val any) *ContentEmbed { return &ContentEmbed{val} }
func (c *ContentEmbed) Len() uint64         { return 1 }
func (c *ContentEmbed) IsCountable() bool   { return true }
func (c *ContentEmbed) Copy() Content       { return &ContentEmbed{c.Val} }
func (c *ContentEmbed) Splice(_ uint64) Content {
	panic("crdt: ContentEmbed is not splittable")
}

// ContentFormat marks the start of a formatting attribute span in YText.
// It does not contribute to the document's logical length.
type ContentFormat struct {
	Key string
	Val any
}

func NewContentFormat(key string, val any) *ContentFormat { return &ContentFormat{key, val} }
func (c *ContentFormat) Len() uint64                      { return 1 }
func (c *ContentFormat) IsCountable() bool                { return false }
func (c *ContentFormat) Copy() Content                    { return &ContentFormat{c.Key, c.Val} }
func (c *ContentFormat) Splice(_ uint64) Content {
	panic("crdt: ContentFormat is not splittable")
}

// ContentType holds a reference to a nested shared type (e.g. a YMap nested
// inside a YArray). The linked item acts as the "container" for the child type.
type ContentType struct{ Type *abstractType }

func NewContentType(t *abstractType) *ContentType { return &ContentType{t} }
func (c *ContentType) Len() uint64                { return 1 }
func (c *ContentType) IsCountable() bool          { return true }
func (c *ContentType) Splice(_ uint64) Content    { panic("crdt: ContentType is not splittable") }

// Copy wraps a FRESH, empty type of the same kind: a type's item back-pointer
// and child list are single-valued, so a re-inserted copy must not alias the
// original. Mirrors Yjs ContentType.copy; UndoManager.redoItem re-inserts the
// children itself.
func (c *ContentType) Copy() Content {
	if c.Type == nil {
		return &ContentType{nil}
	}
	return &ContentType{c.Type.emptyCopy()}
}

// emptyCopy returns a new detached type of the same concrete kind as t.
func (t *abstractType) emptyCopy() *abstractType {
	var at *abstractType
	switch v := t.owner.(type) {
	case *YArray:
		a := &YArray{}
		a.owner = a
		at = &a.abstractType
	case *YMap:
		m := &YMap{}
		m.owner = m
		at = &m.abstractType
	case *YText:
		x := &YText{}
		x.owner = x
		at = &x.abstractType
	case *YXmlElement:
		at = &NewYXmlElement(v.NodeName).abstractType
	case *YXmlFragment:
		f := &YXmlFragment{}
		f.owner = f
		at = &f.abstractType
	case *yxmlHook:
		at = &newYXmlHook(v.hookName).abstractType
	case *YXmlText:
		at = &NewYXmlText().abstractType
	default:
		r := &rawType{}
		r.owner = r
		at = &r.abstractType
	}
	if at.itemMap == nil {
		at.itemMap = make(map[string]*Item)
	}
	return at
}

// ContentDoc holds a reference to a subdocument.
type ContentDoc struct{ Doc *Doc }

func NewContentDoc(d *Doc) *ContentDoc  { return &ContentDoc{d} }
func (c *ContentDoc) Len() uint64       { return 1 }
func (c *ContentDoc) IsCountable() bool { return true }

// Copy returns a ContentDoc wrapping a FRESH Doc with the same guid and opts. A
// Doc may be embedded only once (its item back-reference is single-valued), so
// item copies during split/merge must not alias one inner Doc. Mirrors Yjs
// ContentDoc.copy / createDocFromOpts, which derives shouldLoad from autoLoad,
// so a copy reads no state that a concurrent Load changes.
func (c *ContentDoc) Copy() Content {
	if c.Doc == nil {
		return &ContentDoc{nil}
	}
	s := c.Doc
	copied := New(
		WithGUID(s.guid), WithGC(s.gc), WithAutoLoad(s.autoLoad),
		WithShouldLoad(s.autoLoad),
	)
	copied.meta = s.meta
	return &ContentDoc{copied}
}

func (c *ContentDoc) Splice(_ uint64) Content { panic("crdt: ContentDoc is not splittable") }

// ContentMove is a CRDT-safe array move marker. It sits at the destination
// position in the linked list and causes the target item to be rendered there
// instead of at its original position. ContentMove is non-countable (it does
// not contribute to the array's logical length) and occupies one clock slot.
//
// When two ContentMove items target the same item, the lower ClientID wins,
// and between one client's moves the latest wins. The losing ContentMove stays in the linked list but renders
// nothing because target.MovedBy points to the winning item.
//
// TargetLen is the expected length of the target item (always 1 for
// single-element moves). It is stored in the wire format so that receivers can
// force-split a multi-value ContentAny item at the exact boundary when applying
// delta updates, ensuring the same item granularity on all peers.
type ContentMove struct {
	Target    *ID
	TargetLen uint64
}

func NewContentMove(target *ID, targetLen uint64) *ContentMove {
	return &ContentMove{Target: target, TargetLen: targetLen}
}
func (c *ContentMove) Len() uint64       { return 1 }
func (c *ContentMove) IsCountable() bool { return false }
func (c *ContentMove) Copy() Content {
	id := *c.Target
	return &ContentMove{Target: &id, TargetLen: c.TargetLen}
}
func (c *ContentMove) Splice(_ uint64) Content { panic("crdt: ContentMove is not splittable") }

// contentSkip is a decode-only placeholder for V1 skip structs (tag 10).
// Skip structs represent clock ranges the sender intentionally omits.
// They are consumed during decoding and never stored in the document.
type contentSkip struct{ length uint64 }

func (s *contentSkip) Len() uint64       { return s.length }
func (s *contentSkip) IsCountable() bool { return false }
func (s *contentSkip) Copy() Content     { return &contentSkip{s.length} }
func (s *contentSkip) Splice(_ uint64) Content {
	panic("crdt: contentSkip is not splittable")
}

// writeSubdocOpts writes a subdocument's opts after its guid as Yjs
// ContentDoc.write does: gc:false, autoLoad:true and meta in that order, each
// only when set, so a gc-enabled doc writes {}. shouldLoad and the
// collection ID are not written.
func writeSubdocOpts(enc *encoding.Encoder, d *Doc) {
	n := 0
	if d != nil {
		if !d.gc {
			n++
		}
		if d.autoLoad {
			n++
		}
		if d.meta != nil {
			n++
		}
	}
	enc.WriteUint8(118) // lib0 Any object
	enc.WriteVarUint(uint64(n))
	if d == nil {
		return
	}
	if !d.gc {
		enc.WriteVarString("gc")
		enc.WriteAny(false)
	}
	if d.autoLoad {
		enc.WriteVarString("autoLoad")
		enc.WriteAny(true)
	}
	if d.meta != nil {
		enc.WriteVarString("meta")
		enc.WriteAny(d.meta)
	}
}

// newSubdocFromOpts builds an embedded Doc from a decoded guid + opts map. Per
// Yjs, shouldLoad is derived as opts.autoLoad. A collectionId key, which
// earlier ygo versions wrote, is still read. Unknown keys and a non-map value
// are ignored (robust decode).
func newSubdocFromOpts(guid string, optsAny any) *Doc {
	gc, autoLoad, collectionID := true, false, ""
	var meta any
	if m, ok := optsAny.(map[string]any); ok {
		if v, ok := m["gc"].(bool); ok {
			gc = v
		}
		if v, ok := m["autoLoad"].(bool); ok {
			autoLoad = v
		}
		if v, ok := m["collectionId"].(string); ok {
			collectionID = v
		}
		meta = m["meta"]
	}
	d := New(WithGUID(guid), WithGC(gc), WithAutoLoad(autoLoad),
		WithShouldLoad(autoLoad), WithCollectionID(collectionID))
	d.meta = meta
	return d
}
