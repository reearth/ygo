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
//
// Delta is a Quill-compatible changeset describing the array modifications:
// a sequence of Insert(values), Retain(n), and Delete(n) ops. Inserts carry
// Insert as a []any of newly-added values. Added in v1.15.0 (#74 vector D1)
// to match Yjs JS's `ArrayEvent.delta` and yrs's `ArrayEvent::delta`.
type YArrayEvent struct {
	Target *YArray
	Txn    *Transaction
	Delta  []Delta
}

// KeyChangeAction describes how a single key was affected by a transaction.
type KeyChangeAction string

const (
	// KeyAdded means the key did not exist before the transaction and now does.
	KeyAdded KeyChangeAction = "add"
	// KeyUpdated means the key existed before and now holds a different value.
	KeyUpdated KeyChangeAction = "update"
	// KeyDeleted means the key existed before and no longer does.
	KeyDeleted KeyChangeAction = "delete"
)

// KeyChange describes the per-key delta for a YMapEvent: what happened to
// the key and, when applicable, what its previous value was. Mirrors Yjs JS's
// `event.keys` Map<key, {action, oldValue}> and yrs's `MapEvent::keys()`.
type KeyChange struct {
	Action   KeyChangeAction
	OldValue any // nil when Action == KeyAdded
}

// YMapEvent is emitted after a transaction that modifies a YMap.
//
// KeysChanged is the legacy field — a set of every map key touched during
// the transaction. Kept for backward compatibility.
//
// Keys is the structured replacement (added in v1.15.0, #74 vector D2):
// per-key Action + OldValue, enabling undo/redo, replication consumers, and
// audit-log integrations to reconstruct the pre-transaction state from the
// event alone.
type YMapEvent struct {
	Target      *YMap
	Txn         *Transaction
	KeysChanged map[string]struct{}
	Keys        map[string]KeyChange
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
