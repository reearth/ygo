package crdt

// Every internal piece for locally-created nested types already existed:
// ContentType, abstractType.detached(), the prelimFlusher contract, and the
// generic ContentType branch in item.integrate that links the child and
// flushes its staged content. What it lacked was a way for code OUTSIDE the
// package to build a detached type (abstractType is unexported) and a way to
// insert one into a YArray as its own item (Insert batches plain values into
// a single ContentAny).
//
// Together with the ContentType branch in YMap.Get and the detached staging
// in YMap.Set, this is what a nested document shape needs: for example a
// Jupyter notebook cell, which is a Y.Map holding a Y.Text.

// A note on locking, because it is easy to trip over: Doc.GetArray and
// Doc.GetMap take the document lock, and Transact already holds it. Resolve the
// container BEFORE opening the transaction:
//
//	root := doc.GetArray("cells")          // outside
//	doc.Transact(func(txn *Transaction) {
//		cell := NewMapPrelim()
//		src := NewTextPrelim()
//		src.Insert(txn, 0, "hello", nil)
//		cell.Set(txn, "source", src)
//		root.PushType(txn, cell)
//	})
//
// Accessors that read shared state (Keys, Has, ToJSON) are likewise not safe
// from inside a Transact callback.

// YMap and YArray STAGE their content while detached, as Yjs does with
// _prelimContent: mutations edit the staged content and the net result
// materialises once, at attach. So a key set twice emits once, a key set then
// deleted emits nothing, and consecutive pushes coalesce into a single item —
// a multi-call build emits what Yjs emits, and reads (Len, Get, Keys, ToJSON)
// report the staged content before attach.
//
// YText is the exception, and deliberately so: Yjs stages Y.Text as deferred
// operations (_pending) rather than as content, so YText buffers calls and
// replays them at attach. Its detached reads are empty until then.

// NewTextPrelim returns a DETACHED YText. Mutations are buffered until it is
// attached (via YMap.Set or YArray.PushType) and replayed then, so its items
// get clocks above the container item's — the ordering genuine Yjs produces.
func NewTextPrelim() *YText {
	t := &YText{}
	t.owner = t
	t.itemMap = make(map[string]*Item)
	return t
}

// NewMapPrelim returns a DETACHED YMap. Entries are staged until attached.
func NewMapPrelim() *YMap {
	m := &YMap{}
	m.owner = m
	m.itemMap = make(map[string]*Item)
	return m
}

// NewArrayPrelim returns a DETACHED YArray. Content is staged until attached.
func NewArrayPrelim() *YArray {
	a := &YArray{}
	a.owner = a
	a.itemMap = make(map[string]*Item)
	return a
}

// PushType appends a DETACHED shared type to the array as its own nested item.
// Plain values go through Push, which batches them into one ContentAny item; a
// nested type must occupy an item of its own, hence the separate entry point
// (the same reason YXmlFragment exposes InsertElement/InsertText).
//
// Placement mirrors Push: anchor after the last PHYSICAL item, tombstones
// included, matching Yjs's typeListPushGenerics.
func (a *YArray) PushType(txn *Transaction, st sharedType) {
	bt := st.baseType()
	if !bt.detached() {
		panic("crdt: PushType requires a detached type (use NewMapPrelim/NewTextPrelim)")
	}
	if a.detached() {
		a.prelim = append(a.prelim, st)
		return
	}
	t := &a.abstractType

	var last *Item
	for it := t.start; it != nil; it = it.Right {
		last = it
	}

	// The walk ends at the physical tail, so there is never a right
	// neighbour: OriginRight stays nil and last is nil only when the list is
	// empty.
	var origin *ID
	if last != nil {
		end := last.ID.Clock + uint64(last.Content.Len()) - 1
		origin = &ID{Client: last.ID.Client, Clock: end}
	}

	item := &Item{
		ID:      ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:  origin,
		Left:    last,
		Parent:  t,
		Content: NewContentType(bt),
	}
	// item.integrate sets bt.item, assigns bt.doc, and calls flushPrelim on
	// the owner — so staged children materialise top-down from here.
	item.integrate(txn, 0)
}

// InsertType inserts a DETACHED shared type at logical position index
// (0 = prepend, Len() = append), as its own nested item.
//
// It is to Insert what PushType is to Push: Insert batches plain values into a
// single ContentAny item, which a nested type cannot share. Without this, the
// only way to place a nested type anywhere but the end is PushType followed by
// Move — and Move emits ContentMove, a ygo extension other implementations
// mis-parse, usually silently (#207).
//
// Placement mirrors Insert: leftNeighbourAt uses LIVE-index semantics (it
// skips tombstones), splitting the neighbour when the index falls inside it,
// and an unresolvable index anchors at the tail. That is the deliberate
// difference from PushType, which anchors after the last PHYSICAL item so a
// concurrent Yjs push converges the same way.
func (a *YArray) InsertType(txn *Transaction, index int, st sharedType) {
	bt := st.baseType()
	if !bt.detached() {
		panic("crdt: InsertType requires a detached type (use NewMapPrelim/NewTextPrelim)")
	}
	if a.detached() {
		// Splice into the staged content; flushPrelim splits plain-value runs
		// around it at attach. Unresolvable indices anchor at the tail, the
		// attached rule.
		if index < 0 || index > len(a.prelim) {
			index = len(a.prelim)
		}
		a.prelim = spliceInto(a.prelim, index, []any{st})
		return
	}
	t := &a.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
		// left now holds the [0,offset) part; its Right is the new right half.
	}
	// Same anchor as Insert, differing only in the Content carried. integrate
	// sets bt.item, assigns bt.doc and calls flushPrelim on the owner, so
	// staged children materialise top-down from here.
	a.insertContentAfterItem(txn, left, NewContentType(bt), index)
}
