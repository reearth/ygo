package crdt

import "strings"

// YText is a shared rich-text type. Characters are stored as ContentString
// items; formatting spans are ContentFormat items (which do not count toward
// logical length).
type YText struct {
	abstractType
	observers []func(YTextEvent)
}

func (txt *YText) baseType() *abstractType { return &txt.abstractType }

func (txt *YText) fire(txn *Transaction, _ map[string]struct{}) {
	if len(txt.observers) == 0 {
		return
	}
	e := YTextEvent{Target: txt, Txn: txn}
	for _, fn := range txt.observers {
		fn(e)
	}
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
func (txt *YText) ToString() string {
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

// ToDelta returns a Quill-compatible delta representing the current document
// state as a sequence of insert operations.
//
// Each run of plain text becomes one Delta with Op=DeltaOpInsert.
// Formatting attributes accumulated from ContentFormat markers are attached to
// the text run they precede. A nil attribute value signals the end of a span
// and is omitted from the output attributes map.
func (txt *YText) ToDelta() []Delta {
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
// text. Returns an unsubscribe function.
func (txt *YText) Observe(fn func(YTextEvent)) func() {
	txt.observers = append(txt.observers, fn)
	idx := len(txt.observers) - 1
	return func() {
		txt.observers = append(txt.observers[:idx], txt.observers[idx+1:]...)
	}
}

// ObserveDeep registers fn to be called after any transaction that modifies
// this text or any nested shared type within it. Returns an unsubscribe function.
func (txt *YText) ObserveDeep(fn func(*Transaction)) func() {
	txt.deepObservers = append(txt.deepObservers, fn)
	idx := len(txt.deepObservers) - 1
	return func() {
		obs := txt.deepObservers
		txt.deepObservers = append(obs[:idx], obs[idx+1:]...)
	}
}
