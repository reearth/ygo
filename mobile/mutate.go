package mobile

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/reearth/ygo/crdt"
)

// mobileLocalOrigin tags transactions started by mobile mutators so the Doc
// change-observer (added in a later task) can report local=true. Unique pointer.
var mobileLocalOrigin = &struct{ name string }{"ygo/mobile.local"}

// toIndex converts a mobile int64 index/length to a validated int (>=0, fits int).
func toIndex(v int64) (int, error) {
	if v < 0 {
		return 0, fmt.Errorf("ygo/mobile: index/length must be >= 0, got %d", v)
	}
	if v > math.MaxInt32 {
		return 0, fmt.Errorf("ygo/mobile: index/length %d too large", v)
	}
	return int(v), nil
}

// toRange validates a start index and length (both >=0, fit int). The
// idx+length<=Len() bound is checked inside the transaction against a live length.
func toRange(index, length int64) (int, int, error) {
	idx, err := toIndex(index)
	if err != nil {
		return 0, 0, err
	}
	l, err := toIndex(length)
	if err != nil {
		return 0, 0, err
	}
	return idx, l, nil
}

// decodeAttrs turns attrsJSON into crdt.Attributes. Empty JSON -> nil (plain).
// A null VALUE inside the object is preserved (Yjs formatting-removal convention).
func decodeAttrs(attrsJSON []byte) (crdt.Attributes, error) {
	if len(attrsJSON) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(attrsJSON, &m); err != nil {
		return nil, err
	}
	return crdt.Attributes(m), nil
}

// InsertText inserts text at the given character index of the named YText root,
// inheriting whatever formatting is in effect at the cursor. index must be in
// [0, Len]. Returns ErrClosed after Close.
func (m *Doc) InsertText(name string, index int64, text string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return ErrClosed
	}
	idx, err := toIndex(index)
	if err != nil {
		return err
	}
	return m.d.TransactE(func(txn *crdt.Transaction) error {
		t := txn.GetText(name)
		if idx > t.Len() {
			return fmt.Errorf("ygo/mobile: index %d out of range [0, %d]", idx, t.Len())
		}
		t.Insert(txn, idx, text, nil)
		return nil
	}, mobileLocalOrigin)
}

// InsertTextWithAttributes inserts text carrying the given formatting attributes
// (a JSON object, e.g. {"bold":true}) at the given index. A null attribute value
// follows Yjs's formatting-removal convention. index must be in [0, Len].
// Returns ErrClosed after Close.
func (m *Doc) InsertTextWithAttributes(name string, index int64, text string, attrsJSON []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return ErrClosed
	}
	idx, err := toIndex(index)
	if err != nil {
		return err
	}
	attrs, err := decodeAttrs(attrsJSON)
	if err != nil {
		return err
	}
	return m.d.TransactE(func(txn *crdt.Transaction) error {
		t := txn.GetText(name)
		if idx > t.Len() {
			return fmt.Errorf("ygo/mobile: index %d out of range [0, %d]", idx, t.Len())
		}
		t.Insert(txn, idx, text, attrs)
		return nil
	}, mobileLocalOrigin)
}

// DeleteText removes length characters starting at index from the named YText
// root. index and length must be >= 0 and index+length must be <= Len. Returns
// ErrClosed after Close.
func (m *Doc) DeleteText(name string, index, length int64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return ErrClosed
	}
	idx, l, err := toRange(index, length)
	if err != nil {
		return err
	}
	return m.d.TransactE(func(txn *crdt.Transaction) error {
		t := txn.GetText(name)
		// idx and l are both <= MaxInt32, so idx+l cannot overflow int.
		if idx+l > t.Len() {
			return fmt.Errorf("ygo/mobile: delete range [%d, %d) out of bounds (len %d)", idx, idx+l, t.Len())
		}
		t.Delete(txn, idx, l)
		return nil
	}, mobileLocalOrigin)
}

// FormatText applies the given formatting attributes (a JSON object, e.g.
// {"bold":true}) to length characters starting at index. A null attribute value
// removes that attribute over the range (Yjs's formatting-removal convention).
// index and length must be >= 0 and index+length must be <= Len. Returns
// ErrClosed after Close.
func (m *Doc) FormatText(name string, index, length int64, attrsJSON []byte) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.d == nil {
		return ErrClosed
	}
	idx, l, err := toRange(index, length)
	if err != nil {
		return err
	}
	attrs, err := decodeAttrs(attrsJSON)
	if err != nil {
		return err
	}
	return m.d.TransactE(func(txn *crdt.Transaction) error {
		t := txn.GetText(name)
		// idx and l are both <= MaxInt32, so idx+l cannot overflow int.
		if idx+l > t.Len() {
			return fmt.Errorf("ygo/mobile: format range [%d, %d) out of bounds (len %d)", idx, idx+l, t.Len())
		}
		t.Format(txn, idx, l, attrs)
		return nil
	}, mobileLocalOrigin)
}
