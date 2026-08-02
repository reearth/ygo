package crdt

import (
	"fmt"
	"unicode/utf8"

	"github.com/reearth/ygo/encoding"
)

// ErrInvalidUTF8 is crdt's re-export of encoding.ErrInvalidUTF8, for entry
// points (like NewContentAttribute) whose signature already returns an error
// rather than panicking. It is the SAME sentinel value as encoding's, not an
// independent one, so errors.Is matches regardless of which package's name
// callers compare against.
var ErrInvalidUTF8 = encoding.ErrInvalidUTF8

// checkUTF8 panics if s is not valid UTF-8. op names the calling method and
// what names the offending input, so the failure points at a call site rather
// than at the encoder (#209).
func checkUTF8(op, what, s string) {
	if !utf8.ValidString(s) {
		panic(fmt.Sprintf("crdt: %s: %s: invalid UTF-8", op, what))
	}
}

// checkAnyUTF8 walks v and panics on the first invalid string it finds.
//
// It mirrors the string-bearing cases of encoding.Encoder.WriteAny — string,
// []any, and map[string]any (keys AND values). Every other case WriteAny
// handles (nil, bool, the integer and float types, BigInt, []byte) carries no
// text. TestUnit_CheckAnyUTF8_CoversWriteAnyStringCases guards the pairing.
//
// Note this deliberately does not match named map/slice types: WriteAny does
// not either, so such a value fails at encode for its own reasons.
func checkAnyUTF8(op, what string, v any) {
	switch t := v.(type) {
	case string:
		checkUTF8(op, what, t)
	case []any:
		for i, e := range t {
			checkAnyUTF8(op, fmt.Sprintf("%s[%d]", what, i), e)
		}
	case map[string]any:
		for k, e := range t {
			checkUTF8(op, fmt.Sprintf("%s key %q", what, k), k)
			checkAnyUTF8(op, fmt.Sprintf("%s[%q]", what, k), e)
		}
	}
}

// checkAttrsUTF8 validates formatting attribute keys and values.
//
// Attributes is a named map type, so it does not match checkAnyUTF8's
// map[string]any case; its entries are walked explicitly. This matters because
// V1 writes attribute values through json.Marshal (which sanitises) while V2
// writes them through WriteAny (which does not) — validating only keys would
// leave a version-dependent failure.
func checkAttrsUTF8(op string, attrs Attributes) {
	for k, v := range attrs {
		checkUTF8(op, fmt.Sprintf("attribute key %q", k), k)
		checkAnyUTF8(op, fmt.Sprintf("attribute %q", k), v)
	}
}
