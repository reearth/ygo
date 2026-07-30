package crdt

// Every internal piece for locally-created nested types already existed:
// ContentType, abstractType.detached(), the prelimFlusher contract, and the
// generic ContentType branch in item.integrate that links the child and
// replays its buffered ops. What it lacked was a way for code OUTSIDE the
// package to build a detached type (abstractType is unexported) and a way to
// insert one into a YArray as its own item (Insert batches plain values into
// a single ContentAny).
//
// Together with the ContentType branch in YMap.Get and the detached buffering
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

// NewTextPrelim returns a DETACHED YText. Mutations are buffered until it is
// attached (via YMap.Set or YArray.PushType) and replayed then, so its items
// get clocks above the container item's — the ordering genuine Yjs produces.
func NewTextPrelim() *YText {
	t := &YText{}
	t.owner = t
	return t
}

// NewMapPrelim returns a DETACHED YMap. Sets are buffered until attached.
func NewMapPrelim() *YMap {
	m := &YMap{}
	m.owner = m
	m.itemMap = make(map[string]*Item)
	return m
}

// NewArrayPrelim returns a DETACHED YArray.
func NewArrayPrelim() *YArray {
	a := &YArray{}
	a.owner = a
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
	t := &a.abstractType

	var last *Item
	for it := t.start; it != nil; it = it.Right {
		last = it
	}

	var origin *ID
	var originRight *ID
	if last != nil {
		end := last.ID.Clock + uint64(last.Content.Len()) - 1
		origin = &ID{Client: last.ID.Client, Clock: end}
		if last.Right != nil {
			id := last.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}

	item := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:      origin,
		OriginRight: originRight,
		Left:        last,
		Parent:      t,
		Content:     NewContentType(bt),
	}
	// item.integrate sets bt.item, assigns bt.doc, and calls flushPrelim on
	// the owner — so buffered children materialise top-down from here.
	item.integrate(txn, 0)
}
