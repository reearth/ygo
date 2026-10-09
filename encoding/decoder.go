package encoding

import "github.com/reearth/ygo/internal/anycodec"

var (
	// ErrUnexpectedEOF is returned when the buffer is exhausted before decoding completes.
	ErrUnexpectedEOF = anycodec.ErrUnexpectedEOF
	// ErrOverflow is returned when a VarUint exceeds the 53-bit safe integer range.
	ErrOverflow = anycodec.ErrOverflow
	// ErrUnknownTag is returned when an Any type tag is unknown.
	ErrUnknownTag = anycodec.ErrUnknownTag
	// ErrInvalidUTF8 is returned when a varstring is not valid UTF-8.
	ErrInvalidUTF8 = anycodec.ErrInvalidUTF8
	// ErrDepthExceeded is returned when an Any value exceeds the depth or element limit.
	ErrDepthExceeded = anycodec.ErrDepthExceeded
)

const maxAnyDepth = anycodec.MaxDepth
const maxAnyElements = anycodec.MaxElements

// Decoder reads values from a byte slice using the lib0 encoding format.
// Decoder is not safe for concurrent use; each goroutine should use its own instance.
type Decoder struct{ cursor anycodec.Decoder }

// NewDecoder returns a Decoder that reads from b.
func NewDecoder(b []byte) *Decoder { return &Decoder{cursor: anycodec.NewDecoder(b)} }

// Remaining returns the number of unread bytes.
func (d *Decoder) Remaining() int { return d.cursor.Remaining() }

// HasContent reports whether there are unread bytes remaining.
func (d *Decoder) HasContent() bool { return d.cursor.HasContent() }

// RemainingBytes returns the unread portion of the buffer as a sub-slice.
//
// The returned slice ALIASES the decoder's underlying buffer; mutating it
// (or extending its length via append beyond cap) corrupts the decoder.
// Callers must treat it as read-only and copy if they need a slice with
// an independent lifetime. Most callers in this codebase hand the bytes
// straight to ApplySyncMessage or json.Unmarshal, both of which read-only,
// so the zero-copy path is safe.
//
// Use RemainingBytesCopy if you need an independent allocation.
func (d *Decoder) RemainingBytes() []byte { return d.cursor.RemainingBytes() }

// RemainingBytesCopy returns an independently-allocated copy of the unread
// portion of the buffer. Use when the caller needs to retain the bytes
// across mutations of the decoder's underlying buffer.
func (d *Decoder) RemainingBytesCopy() []byte { return d.cursor.RemainingBytesCopy() }

// ReadUint8 reads a single byte.
func (d *Decoder) ReadUint8() (uint8, error) { return d.cursor.ReadUint8() }

// ReadVarUint decodes a variable-length unsigned integer.
// Returns ErrOverflow if the value exceeds 53 significant bits.
func (d *Decoder) ReadVarUint() (uint64, error) { return d.cursor.ReadVarUint() }

// ReadVarInt decodes a lib0 sign-magnitude variable-length integer.
// Returns ErrOverflow if the encoded magnitude exceeds 55 bits (the lib0 protocol's maximum).
func (d *Decoder) ReadVarInt() (int64, error) { return d.cursor.ReadVarInt() }

// ReadVarString decodes a length-prefixed UTF-8 string. Invalid UTF-8 byte
// sequences return ErrInvalidUTF8, matching lib0's TextDecoder fatal:true
// mode. Without this, malformed input would silently produce a corrupt Go
// string carrying non-UTF-8 bytes that downstream code may misinterpret.
func (d *Decoder) ReadVarString() (string, error) { return d.cursor.ReadVarString() }

// ReadVarBytes decodes a length-prefixed byte slice.
// The returned slice is a sub-slice of the decoder's buffer; copy if you need to retain it.
//
// A field can be no larger than the bytes that remain in the buffer; a declared
// length beyond that is malformed and returns ErrUnexpectedEOF. Because the
// returned slice aliases the buffer (no allocation), the buffer length is the
// real bound — a crafted multi-GiB length prefix is rejected here without any
// large allocation. Bounding by the remaining buffer rather than a fixed
// ceiling (previously 16 MiB) lets a single large field — e.g. a big text node
// or binary embed — sync inside an otherwise-valid message, with the maximum
// message size policed by the provider layer (MaxMessageBytes, default 64 MiB)
// instead of being silently rejected here (N-12).
func (d *Decoder) ReadVarBytes() ([]byte, error) { return d.cursor.ReadVarBytes() }

// ReadFloat32 reads a 32-bit big-endian IEEE 754 float.
func (d *Decoder) ReadFloat32() (float32, error) { return d.cursor.ReadFloat32() }

// ReadFloat64 reads a 64-bit big-endian IEEE 754 float.
func (d *Decoder) ReadFloat64() (float64, error) { return d.cursor.ReadFloat64() }

// ReadBigInt64 reads a signed 64-bit big-endian integer.
func (d *Decoder) ReadBigInt64() (int64, error) { return d.cursor.ReadBigInt64() }

// ReadAny decodes a tagged-union value written by Encoder.WriteAny.
// Nested arrays and maps are limited to maxAnyDepth levels to prevent
// stack-overflow DoS from crafted inputs.
func (d *Decoder) ReadAny() (any, error) {
	return d.cursor.ReadAny(func(v int64) any { return BigInt(v) })
}
