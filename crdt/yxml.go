package crdt

import (
	"fmt"
	"strings"
)

// xmlNode is implemented by all XML node types (*YXmlFragment, *YXmlElement,
// *YXmlText). It is used internally to walk and serialise XML trees.
type xmlNode interface {
	ToXML() string
	baseXMLType() *abstractType
}

// xmlSub pairs a unique subscription ID with a YXmlEvent callback.
type xmlSub struct {
	id uint64
	fn func(YXmlEvent)
}

// ── YXmlFragment ──────────────────────────────────────────────────────────────

// YXmlFragment is an ordered container of XML child nodes. It is the base
// type for YXmlElement and can also be used directly as a root XML type.
type YXmlFragment struct {
	abstractType
	subIDGen  uint64
	observers []xmlSub
}

func (f *YXmlFragment) baseType() *abstractType    { return &f.abstractType }
func (f *YXmlFragment) baseXMLType() *abstractType { return &f.abstractType }

// prepareFire snapshots the current observer slice inside the document write
// lock and returns a closure that fires all snapshotted observers (N-C1).
func (f *YXmlFragment) prepareFire(txn *Transaction, keysChanged map[string]struct{}) func() {
	if len(f.observers) == 0 {
		return nil
	}
	snap := make([]xmlSub, len(f.observers))
	copy(snap, f.observers)
	ev := YXmlEvent{Target: f, Txn: txn, KeysChanged: keysChanged}
	return func() {
		for _, s := range snap {
			s.fn(ev)
		}
	}
}

// Len returns the number of non-deleted child nodes (attributes are excluded).
func (f *YXmlFragment) Len() int {
	count := 0
	for item := f.start; item != nil; item = item.Right {
		if !item.Deleted && item.Content.IsCountable() && item.ParentSub == "" {
			count += item.Content.Len()
		}
	}
	return count
}

// Insert inserts XML nodes at child position index (0 = prepend).
func (f *YXmlFragment) Insert(txn *Transaction, index int, nodes ...xmlNode) {
	t := &f.abstractType
	left, offset := leftChildAt(t, index)
	if offset > 0 {
		splitItem(txn, left, offset)
	}

	for _, node := range nodes {
		at := node.baseXMLType()
		if at.doc == nil {
			at.doc = txn.doc
		}

		var origin *ID
		var originRight *ID
		if left != nil {
			end := left.ID.Clock + uint64(left.Content.Len()) - 1
			origin = &ID{Client: left.ID.Client, Clock: end}
			// originRight is the next child item (skip attribute items).
			for r := left.Right; r != nil; r = r.Right {
				if r.ParentSub == "" {
					id := r.ID
					originRight = &id
					break
				}
			}
		} else {
			// Inserting at start: find first existing child as originRight.
			for it := t.start; it != nil; it = it.Right {
				if it.ParentSub == "" {
					id := it.ID
					originRight = &id
					break
				}
			}
		}

		item := &Item{
			ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
			Origin:      origin,
			OriginRight: originRight,
			Left:        left,
			Parent:      t,
			Content:     NewContentType(at),
		}
		at.item = item
		item.integrate(txn, 0)
		left = item
	}
}

// Delete removes length child nodes starting at child position index.
func (f *YXmlFragment) Delete(txn *Transaction, index, length int) {
	deleteChildRange(&f.abstractType, txn, index, length)
}

// Children returns all non-deleted child XML nodes in document order.
func (f *YXmlFragment) Children() []xmlNode {
	var result []xmlNode
	for item := f.start; item != nil; item = item.Right {
		if item.Deleted || item.ParentSub != "" {
			continue
		}
		if ct, ok := item.Content.(*ContentType); ok {
			if node, ok := ct.Type.owner.(xmlNode); ok {
				result = append(result, node)
			}
		}
	}
	return result
}

// ToXML returns the XML serialisation of this fragment's children concatenated.
func (f *YXmlFragment) ToXML() string {
	var sb strings.Builder
	for _, child := range f.Children() {
		sb.WriteString(child.ToXML())
	}
	return sb.String()
}

// Observe registers fn to be called after every transaction that modifies this
// fragment. Returns an unsubscribe function. Uses ID-based lookup so out-of-order
// unsubscription removes the correct entry (C5).
//
// Acquiring doc.mu.Lock() serialises registration against Transact (N-C1).
// Do not call Observe from inside a Transact callback — that would deadlock.
func (f *YXmlFragment) Observe(fn func(YXmlEvent)) func() {
	doc := f.doc
	if doc != nil {
		doc.mu.Lock()
		defer doc.mu.Unlock()
	}
	f.subIDGen++
	id := f.subIDGen
	f.observers = append(f.observers, xmlSub{id: id, fn: fn})
	return func() {
		if doc := f.doc; doc != nil {
			doc.mu.Lock()
			defer doc.mu.Unlock()
		}
		for i, s := range f.observers {
			if s.id == id {
				f.observers = append(f.observers[:i], f.observers[i+1:]...)
				return
			}
		}
	}
}

// ── YXmlElement ───────────────────────────────────────────────────────────────

// YXmlElement is an XML element: a named tag with optional attributes and
// ordered child nodes. It embeds YXmlFragment for child management.
//
// Attributes are stored as map-keyed items (ParentSub = attribute name) in the
// same abstractType as children. Children are ContentType items with
// ParentSub = "".
type YXmlElement struct {
	YXmlFragment
	NodeName    string
	elemSubGen uint64
	elemObs    []xmlSub
}

// baseType and baseXMLType both route to the single embedded abstractType.
func (e *YXmlElement) baseType() *abstractType    { return &e.abstractType }
func (e *YXmlElement) baseXMLType() *abstractType { return &e.abstractType }

// prepareFire overrides YXmlFragment.prepareFire to use the element observer
// list (elemObs) rather than the fragment observer list (N-C1).
func (e *YXmlElement) prepareFire(txn *Transaction, keysChanged map[string]struct{}) func() {
	if len(e.elemObs) == 0 {
		return nil
	}
	snap := make([]xmlSub, len(e.elemObs))
	copy(snap, e.elemObs)
	ev := YXmlEvent{Target: e, Txn: txn, KeysChanged: keysChanged}
	return func() {
		for _, s := range snap {
			s.fn(ev)
		}
	}
}

// SetAttribute sets the XML attribute key to value.
func (e *YXmlElement) SetAttribute(txn *Transaction, key, value string) {
	t := &e.abstractType
	var left *Item
	var origin *ID
	if existing, ok := t.itemMap[key]; ok {
		left = existing
		id := existing.ID
		origin = &id
	}
	item := &Item{
		ID:        ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:    origin,
		Left:      left,
		Parent:    t,
		ParentSub: key,
		Content:   NewContentAny(value),
	}
	item.integrate(txn, 0)
}

// DeleteAttribute removes the attribute with the given key if it exists.
func (e *YXmlElement) DeleteAttribute(txn *Transaction, key string) {
	t := &e.abstractType
	if item, ok := t.itemMap[key]; ok && !item.Deleted {
		item.delete(txn)
	}
}

// GetAttribute returns the value of attribute key and whether it is present.
func (e *YXmlElement) GetAttribute(key string) (string, bool) {
	t := &e.abstractType
	item, ok := t.itemMap[key]
	if !ok || item.Deleted {
		return "", false
	}
	if ca, ok := item.Content.(*ContentAny); ok && len(ca.Vals) > 0 {
		if s, ok := ca.Vals[0].(string); ok {
			return s, true
		}
	}
	return "", false
}

// GetAttributes returns all live attributes as a string-keyed map.
func (e *YXmlElement) GetAttributes() map[string]string {
	t := &e.abstractType
	result := make(map[string]string)
	for k, item := range t.itemMap {
		if item.Deleted {
			continue
		}
		if ca, ok := item.Content.(*ContentAny); ok && len(ca.Vals) > 0 {
			if s, ok := ca.Vals[0].(string); ok {
				result[k] = s
			}
		}
	}
	return result
}

// ToXML serialises the element as <NodeName attrs>children</NodeName>.
// Attribute keys are sorted alphabetically for deterministic output.
func (e *YXmlElement) ToXML() string {
	attrs := e.GetAttributes()
	var sb strings.Builder
	sb.WriteByte('<')
	sb.WriteString(e.NodeName)
	for _, k := range xmlSortedKeys(attrs) {
		fmt.Fprintf(&sb, ` %s="%s"`, k, xmlEscapeAttr(attrs[k]))
	}
	sb.WriteByte('>')
	sb.WriteString(e.YXmlFragment.ToXML())
	sb.WriteString("</")
	sb.WriteString(e.NodeName)
	sb.WriteByte('>')
	return sb.String()
}

// Observe registers fn to be called after every transaction that modifies this
// element (children added/removed or attributes changed). Returns an
// unsubscribe function. Uses ID-based lookup so out-of-order unsubscription
// removes the correct entry (C5).
//
// Acquiring doc.mu.Lock() serialises registration against Transact (N-C1).
// Do not call Observe from inside a Transact callback — that would deadlock.
func (e *YXmlElement) Observe(fn func(YXmlEvent)) func() {
	doc := e.doc
	if doc != nil {
		doc.mu.Lock()
		defer doc.mu.Unlock()
	}
	e.elemSubGen++
	id := e.elemSubGen
	e.elemObs = append(e.elemObs, xmlSub{id: id, fn: fn})
	return func() {
		if doc := e.doc; doc != nil {
			doc.mu.Lock()
			defer doc.mu.Unlock()
		}
		for i, s := range e.elemObs {
			if s.id == id {
				e.elemObs = append(e.elemObs[:i], e.elemObs[i+1:]...)
				return
			}
		}
	}
}

// ── YXmlText ──────────────────────────────────────────────────────────────────

// YXmlText is a text node inside an XML tree. It embeds YText, inheriting all
// text-editing methods (Insert, Delete, Format, ToString, Observe).
type YXmlText struct {
	YText
}

func (t *YXmlText) baseXMLType() *abstractType { return &t.abstractType }

// ToXML returns the text content with XML-special characters escaped.
func (t *YXmlText) ToXML() string {
	return xmlEscapeText(t.YText.ToString()) //nolint:staticcheck // intentional: avoids recursion with YXmlText.ToXML
}

// ── Constructors ──────────────────────────────────────────────────────────────

// NewYXmlElement creates a standalone YXmlElement ready to be inserted into a
// YXmlFragment or another YXmlElement.
func NewYXmlElement(nodeName string) *YXmlElement {
	e := &YXmlElement{NodeName: nodeName}
	e.itemMap = make(map[string]*Item)
	e.owner = e
	return e
}

// NewYXmlText creates a standalone YXmlText ready to be inserted into a
// YXmlFragment or YXmlElement.
func NewYXmlText() *YXmlText {
	t := &YXmlText{}
	t.itemMap = make(map[string]*Item)
	t.owner = t
	return t
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// leftChildAt is like abstractType.leftNeighbourAt but skips attribute items
// (ParentSub != ""), counting only child nodes.
func leftChildAt(t *abstractType, index int) (*Item, int) {
	if index == 0 {
		return nil, 0
	}
	counted := 0
	var lastItem *Item
	for item := t.start; item != nil; item = item.Right {
		if !item.Deleted && item.Content.IsCountable() && item.ParentSub == "" {
			n := item.Content.Len()
			if counted+n >= index {
				offset := index - counted
				if offset == n {
					return item, 0
				}
				return item, offset
			}
			counted += n
			lastItem = item
		}
	}
	return lastItem, 0
}

// deleteChildRange deletes length child nodes (ParentSub == "") starting at
// child position index. Mirrors deleteRange from yarray.go.
func deleteChildRange(t *abstractType, txn *Transaction, index, length int) {
	if length <= 0 {
		return
	}
	counted := 0
	item := t.start
	for item != nil && length > 0 {
		if item.Deleted || !item.Content.IsCountable() || item.ParentSub != "" {
			item = item.Right
			continue
		}
		n := item.Content.Len()
		if counted+n <= index {
			counted += n
			item = item.Right
			continue
		}
		if counted < index {
			right := splitItem(txn, item, index-counted)
			counted = index
			item = right
			n = right.Content.Len()
		}
		if n <= length {
			item.delete(txn)
			length -= n
			item = item.Right
		} else {
			splitItem(txn, item, length)
			item.delete(txn)
			length = 0
		}
	}
}

// xmlSortedKeys returns the keys of m sorted alphabetically.
func xmlSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort — attribute lists are small
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// xmlEscapeText escapes XML text-node content (&, <, >).
func xmlEscapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// xmlEscapeAttr escapes an XML attribute value (double-quote context).
func xmlEscapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}
