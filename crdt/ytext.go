package crdt

import (
	"encoding/json"
	"strings"
)

// textSub pairs a unique subscription ID with a YTextEvent callback.
type textSub struct {
	id uint64
	fn func(YTextEvent)
}

// YText is a shared rich-text type. Characters are stored as ContentString
// items; formatting spans are ContentFormat items (which do not count toward
// logical length).
type YText struct {
	abstractType
	subIDGen  uint64
	observers []textSub
}

func (txt *YText) baseType() *abstractType { return &txt.abstractType }

func (txt *YText) fire(txn *Transaction, _ map[string]struct{}) {
	if len(txt.observers) == 0 {
		return
	}
	delta := txt.computeDelta(txn)
	e := YTextEvent{Target: txt, Txn: txn, Delta: delta}
	for _, s := range txt.observers {
		s.fn(e)
	}
}

// computeDelta builds a Quill-compatible delta for the changes in txn.
// Each ContentString item is classified as Insert (new in this txn),
// Delete (removed by this txn), or Retain (unchanged and visible).
// A trailing Retain is omitted following the Quill convention.
func (txt *YText) computeDelta(txn *Transaction) []Delta {
	var ops []Delta
	retain := 0

	flushRetain := func() {
		if retain > 0 {
			ops = append(ops, Delta{Op: DeltaOpRetain, Retain: retain})
			retain = 0
		}
	}

	for item := txt.start; item != nil; item = item.Right {
		cs, isStr := item.Content.(*ContentString)
		if !isStr {
			continue
		}
		beforeClock := txn.beforeState.Clock(item.ID.Client)
		isNew := item.ID.Clock >= beforeClock
		if isNew {
			if !item.Deleted {
				flushRetain()
				ops = append(ops, Delta{Op: DeltaOpInsert, Insert: cs.Str})
			}
			// inserted and immediately deleted in the same txn → net no-op; skip
		} else if txn.deleteSet.IsDeleted(item.ID) {
			flushRetain()
			ops = append(ops, Delta{Op: DeltaOpDelete, Delete: cs.Len()})
		} else if !item.Deleted {
			retain += cs.Len()
		}
	}
	// Trailing retain omitted (Quill convention).
	return ops
}

// Len returns the number of non-deleted Unicode code points.
func (txt *YText) Len() int { return txt.length }

// Insert inserts text at logical character position index.
// attrs may be nil for unstyled text. Formatting is applied by wrapping the
// new content with ContentFormat items for each attribute.
func (txt *YText) Insert(txn *Transaction, index int, text string, attrs Attributes) {
	if text == "" {
		return
	}
	t := &txt.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
		// left is now the left half; left.Right is the right half.
	}

	var origin *ID
	var originRight *ID
	if left != nil {
		end := left.ID.Clock + uint64(left.Content.Len()) - 1
		origin = &ID{Client: left.ID.Client, Clock: end}
		if left.Right != nil {
			id := left.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}

	clock := txn.doc.store.NextClock(txn.doc.ClientID)

	if len(attrs) > 0 {
		// Insert an opening ContentFormat item for each attribute.
		for k, v := range attrs {
			fmtItem := &Item{
				ID:          ID{Client: txn.doc.ClientID, Clock: clock},
				Origin:      origin,
				OriginRight: originRight,
				Left:        left,
				Parent:      t,
				Content:     NewContentFormat(k, v),
			}
			fmtItem.integrate(txn, 0)
			left = fmtItem
			origin = &ID{Client: fmtItem.ID.Client, Clock: fmtItem.ID.Clock}
			originRight = nil
			clock = txn.doc.store.NextClock(txn.doc.ClientID)
		}
	}

	item := &Item{
		ID:          ID{Client: txn.doc.ClientID, Clock: clock},
		Origin:      origin,
		OriginRight: originRight,
		Left:        left,
		Parent:      t,
		Content:     NewContentString(text),
	}
	// Signal to integrate that this is a local insert at a known position so
	// it can do a partial cache invalidation instead of a full clear.
	if index > 0 {
		t.insertHint = index
	}
	item.integrate(txn, 0)
}

// Delete removes length characters starting at logical position index.
func (txt *YText) Delete(txn *Transaction, index, length int) {
	deleteRange(&txt.abstractType, txn, index, length)
}

// Format applies attrs to the character range [index, index+length).
// Each attribute is represented as a ContentFormat item inserted at the
// start of the range.
func (txt *YText) Format(txn *Transaction, index, length int, attrs Attributes) {
	if len(attrs) == 0 || length <= 0 {
		return
	}
	t := &txt.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
	}

	var origin *ID
	if left != nil {
		end := left.ID.Clock + uint64(left.Content.Len()) - 1
		origin = &ID{Client: left.ID.Client, Clock: end}
	}

	for k, v := range attrs {
		fmtItem := &Item{
			ID:      ID{Client: txn.doc.ClientID, Clock: txn.doc.store.NextClock(txn.doc.ClientID)},
			Origin:  origin,
			Left:    left,
			Parent:  t,
			Content: NewContentFormat(k, v),
		}
		fmtItem.integrate(txn, 0)
		left = fmtItem
		id := fmtItem.ID
		origin = &id
	}
}

// ToString returns the concatenation of all non-deleted character runs,
// excluding format markers.
// Must not be called from inside a Transact callback.
func (txt *YText) ToString() string {
	if doc := txt.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &txt.abstractType
	var sb strings.Builder
	for item := t.start; item != nil; item = item.Right {
		if !item.Deleted {
			if cs, ok := item.Content.(*ContentString); ok {
				sb.WriteString(cs.Str)
			}
		}
	}
	return sb.String()
}

// ToJSON returns the text content serialised as a JSON string.
// Must not be called from inside a Transact callback.
func (txt *YText) ToJSON() ([]byte, error) {
	return json.Marshal(txt.ToString())
}

// ToDelta returns a Quill-compatible delta representing the current document
// state as a sequence of insert operations.
//
// Each run of plain text becomes one Delta with Op=DeltaOpInsert.
// Formatting attributes accumulated from ContentFormat markers are attached to
// the text run they precede. A nil attribute value signals the end of a span
// and is omitted from the output attributes map.
// Must not be called from inside a Transact callback.
func (txt *YText) ToDelta() []Delta {
	if doc := txt.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	var deltas []Delta
	currentAttrs := make(Attributes)

	for item := txt.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		switch c := item.Content.(type) {
		case *ContentString:
			d := Delta{Op: DeltaOpInsert, Insert: c.Str}
			if len(currentAttrs) > 0 {
				attrs := make(Attributes, len(currentAttrs))
				for k, v := range currentAttrs {
					attrs[k] = v
				}
				d.Attributes = attrs
			}
			deltas = append(deltas, d)
		case *ContentFormat:
			if c.Val == nil {
				delete(currentAttrs, c.Key)
			} else {
				currentAttrs[c.Key] = c.Val
			}
		}
	}
	return deltas
}

// Observe registers fn to be called after every transaction that modifies this
// text. Returns an unsubscribe function. Uses ID-based lookup so out-of-order
// unsubscription removes the correct entry (C5).
func (txt *YText) Observe(fn func(YTextEvent)) func() {
	txt.subIDGen++
	id := txt.subIDGen
	txt.observers = append(txt.observers, textSub{id: id, fn: fn})
	return func() {
		for i, s := range txt.observers {
			if s.id == id {
				txt.observers = append(txt.observers[:i], txt.observers[i+1:]...)
				return
			}
		}
	}
}

// ObserveDeep registers fn to be called after any transaction that modifies
// this text or any nested shared type within it. Returns an unsubscribe function.
func (txt *YText) ObserveDeep(fn func(*Transaction)) func() {
	return txt.abstractType.observeDeep(fn)
}
