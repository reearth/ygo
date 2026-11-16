package crdt

import (
	"fmt"
	"slices"
)

// ContentKind names one kind of content a struct can carry, which is what the
// answer of WithRefusedContent is made of. Its values are the content tags of
// the Yjs wire format, which the format fixes, so a list an owner writes down
// keeps its meaning across releases.
type ContentKind uint8

// The content kinds a struct of a remote update can carry. A GC struct and a
// skip struct carry no content and are of no kind.
const (
	ContentKindDeleted = ContentKind(wireDeleted)
	ContentKindJSON    = ContentKind(wireJSON)
	ContentKindBinary  = ContentKind(wireBinary)
	ContentKindString  = ContentKind(wireString)
	ContentKindEmbed   = ContentKind(wireEmbed)
	ContentKindFormat  = ContentKind(wireFormat)
	ContentKindType    = ContentKind(wireType)
	ContentKindAny     = ContentKind(wireAny)
	ContentKindDoc     = ContentKind(wireDoc)
	ContentKindMove    = ContentKind(wireMove)
)

// ErrContentNotAllowed is returned, wrapped in a RefusedUpdateError, by a
// remote apply whose update carries a struct of a kind the WithRefusedContent
// answer names. It wraps ErrInvalidUpdate. The update is refused before any
// of its structs enters the document, so the document is as it was.
var ErrContentNotAllowed = fmt.Errorf("%w: it carries a kind of content the document does not allow", ErrInvalidUpdate)

// refuseContentKind is ErrContentNotAllowed when tag, the content tag of a
// struct the remote apply running on doc is decoding, is a kind the apply's
// WithRefusedContent answer names, and nil otherwise, as for every decode
// outside an apply. It runs before the struct's content is decoded, so a
// refused kind costs nothing to read; the answer is short, so it is scanned
// rather than put in a map each apply would build.
func refuseContentKind(doc *Doc, tag byte) error {
	t := doc.tally()
	if t == nil || !slices.Contains(t.refused, ContentKind(tag)) {
		return nil
	}
	return ErrContentNotAllowed
}
