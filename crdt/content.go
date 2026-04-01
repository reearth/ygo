package crdt

// Content is the payload carried by an Item.
// Every concrete content type must implement this interface.
type Content interface {
	// Len returns how many logical positions this content occupies.
	// For ContentString this is the number of UTF-16 code units, matching Yjs
	// wire-protocol semantics (JavaScript's string length model).
	Len() int
	// IsCountable reports whether this content contributes to a type's length.
	// Deleted and format-marker content do not count.
	IsCountable() bool
	// Copy returns a deep copy.
	Copy() Content
	// Splice splits the content at offset, mutates the receiver to hold [0, offset),
	// and returns a new Content holding [offset, Len()).
	Splice(offset int) Content
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

// utf16ByteOffset returns the byte index in s that corresponds to utf16Units
// UTF-16 code units from the start of s.
func utf16ByteOffset(s string, utf16Units int) int {
	u16 := 0
	for i, r := range s {
		if u16 >= utf16Units {
			return i
		}
		if r > 0xFFFF {
			u16 += 2
		} else {
			u16++
		}
	}
	return len(s)
}

// ContentDeleted is a tombstone. It replaces real content when an item is
// deleted but must stay in the linked list to preserve position references.
type ContentDeleted struct{ length int }

func NewContentDeleted(length int) *ContentDeleted { return &ContentDeleted{length} }
func (c *ContentDeleted) Len() int                 { return c.length }
func (c *ContentDeleted) IsCountable() bool        { return false }
func (c *ContentDeleted) Copy() Content            { return &ContentDeleted{c.length} }
func (c *ContentDeleted) Splice(offset int) Content {
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
func (c *ContentString) Len() int          { return c.utf16Len }
func (c *ContentString) IsCountable() bool { return true }
func (c *ContentString) Copy() Content {
	return &ContentString{Str: c.Str, utf16Len: c.utf16Len}
}
func (c *ContentString) Splice(offset int) Content {
	splitByte := utf16ByteOffset(c.Str, offset)
	rightStr := c.Str[splitByte:]
	rightLen := c.utf16Len - offset
	c.Str = c.Str[:splitByte]
	c.utf16Len = offset
	return &ContentString{Str: rightStr, utf16Len: rightLen}
}

// ContentBinary holds raw bytes (e.g. binary file attachments).
type ContentBinary struct{ Data []byte }

func NewContentBinary(b []byte) *ContentBinary { return &ContentBinary{b} }
func (c *ContentBinary) Len() int              { return 1 }
func (c *ContentBinary) IsCountable() bool     { return true }
func (c *ContentBinary) Copy() Content {
	cp := make([]byte, len(c.Data))
	copy(cp, c.Data)
	return &ContentBinary{cp}
}
func (c *ContentBinary) Splice(_ int) Content { panic("crdt: ContentBinary is not splittable") }

// ContentAny holds a slice of arbitrary JSON-compatible values.
// Used by YArray when storing heterogeneous elements.
type ContentAny struct{ Vals []any }

func NewContentAny(vals ...any) *ContentAny { return &ContentAny{vals} }
func (c *ContentAny) Len() int              { return len(c.Vals) }
func (c *ContentAny) IsCountable() bool     { return true }
func (c *ContentAny) Copy() Content {
	cp := make([]any, len(c.Vals))
	copy(cp, c.Vals)
	return &ContentAny{cp}
}
func (c *ContentAny) Splice(offset int) Content {
	right := &ContentAny{append([]any{}, c.Vals[offset:]...)}
	c.Vals = c.Vals[:offset]
	return right
}

// ContentJSON holds legacy JSON-serializable values. Functionally equivalent
// to ContentAny; kept separate to maintain wire-format compatibility.
type ContentJSON struct{ Vals []any }

func NewContentJSON(vals ...any) *ContentJSON { return &ContentJSON{vals} }
func (c *ContentJSON) Len() int               { return len(c.Vals) }
func (c *ContentJSON) IsCountable() bool      { return true }
func (c *ContentJSON) Copy() Content {
	cp := make([]any, len(c.Vals))
	copy(cp, c.Vals)
	return &ContentJSON{cp}
}
func (c *ContentJSON) Splice(offset int) Content {
	right := &ContentJSON{append([]any{}, c.Vals[offset:]...)}
	c.Vals = c.Vals[:offset]
	return right
}

// ContentEmbed holds a single embedded object (e.g. an image or formula in rich text).
type ContentEmbed struct{ Val any }

func NewContentEmbed(val any) *ContentEmbed { return &ContentEmbed{val} }
func (c *ContentEmbed) Len() int            { return 1 }
func (c *ContentEmbed) IsCountable() bool   { return true }
func (c *ContentEmbed) Copy() Content       { return &ContentEmbed{c.Val} }
func (c *ContentEmbed) Splice(_ int) Content {
	panic("crdt: ContentEmbed is not splittable")
}

// ContentFormat marks the start of a formatting attribute span in YText.
// It does not contribute to the document's logical length.
type ContentFormat struct {
	Key string
	Val any
}

func NewContentFormat(key string, val any) *ContentFormat { return &ContentFormat{key, val} }
func (c *ContentFormat) Len() int                         { return 1 }
func (c *ContentFormat) IsCountable() bool                { return false }
func (c *ContentFormat) Copy() Content                    { return &ContentFormat{c.Key, c.Val} }
func (c *ContentFormat) Splice(_ int) Content {
	panic("crdt: ContentFormat is not splittable")
}

// ContentType holds a reference to a nested shared type (e.g. a YMap nested
// inside a YArray). The linked item acts as the "container" for the child type.
type ContentType struct{ Type *abstractType }

func NewContentType(t *abstractType) *ContentType { return &ContentType{t} }
func (c *ContentType) Len() int                   { return 1 }
func (c *ContentType) IsCountable() bool          { return true }
func (c *ContentType) Copy() Content              { return &ContentType{c.Type} }
func (c *ContentType) Splice(_ int) Content       { panic("crdt: ContentType is not splittable") }

// ContentDoc holds a reference to a subdocument.
type ContentDoc struct{ Doc *Doc }

func NewContentDoc(d *Doc) *ContentDoc     { return &ContentDoc{d} }
func (c *ContentDoc) Len() int             { return 1 }
func (c *ContentDoc) IsCountable() bool    { return true }
func (c *ContentDoc) Copy() Content        { return &ContentDoc{c.Doc} }
func (c *ContentDoc) Splice(_ int) Content { panic("crdt: ContentDoc is not splittable") }
