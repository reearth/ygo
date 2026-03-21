package crdt

// Attributes is a map of rich-text formatting attribute name → value.
type Attributes map[string]any

// DeltaOp is the kind of operation in a rich-text Delta.
type DeltaOp int

const (
	DeltaOpInsert DeltaOp = iota
	DeltaOpDelete
	DeltaOpRetain
)

// Delta represents one operation in a rich-text changeset:
// insert new content, delete existing content, or retain (and optionally
// re-format) existing content.
type Delta struct {
	Op         DeltaOp
	Insert     any        // string or embedded object; valid when Op == DeltaOpInsert
	Delete     int        // character count; valid when Op == DeltaOpDelete
	Retain     int        // character count; valid when Op == DeltaOpRetain
	Attributes Attributes // formatting change; valid for Insert and Retain
}

// YArrayEvent is emitted after a transaction that modifies a YArray.
type YArrayEvent struct {
	Target *YArray
	Txn    *Transaction
}

// YMapEvent is emitted after a transaction that modifies a YMap.
// KeysChanged contains every map key touched during the transaction.
type YMapEvent struct {
	Target      *YMap
	Txn         *Transaction
	KeysChanged map[string]struct{}
}

// YTextEvent is emitted after a transaction that modifies a YText.
type YTextEvent struct {
	Target *YText
	Txn    *Transaction
	Delta  []Delta
}

// YXmlEvent is emitted after a transaction that modifies a YXmlFragment,
// YXmlElement, or YXmlText node.
// Target holds the concrete type (*YXmlFragment, *YXmlElement, or *YXmlText).
// KeysChanged contains attribute keys that were added, updated, or deleted
// during the transaction; it is nil for child-only modifications.
type YXmlEvent struct {
	Target      interface{}
	Txn         *Transaction
	KeysChanged map[string]struct{}
}
