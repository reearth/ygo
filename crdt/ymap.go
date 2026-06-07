package crdt

import "encoding/json"

// mapSub pairs a unique subscription ID with a YMapEvent callback.
type mapSub struct {
	id uint64
	fn func(YMapEvent)
}

// YMap is a shared key-value store with last-write-wins semantics.
// It embeds abstractType, which owns the underlying doubly-linked Item list.
// Every key maps to at most one live Item; concurrent writes to the same key
// are resolved deterministically: the item with the higher ClientID wins.
type YMap struct {
	abstractType
	subIDGen  uint64
	observers []mapSub
}

func (m *YMap) baseType() *abstractType { return &m.abstractType }

// prepareFire snapshots the current observer slice inside the document write
// lock and returns a closure that fires all snapshotted observers (N-C1).
//
// The Keys map (#74 D2, v1.15.0) is computed under the lock so it sees a
// consistent view of the linked list before the lock is released.
func (m *YMap) prepareFire(txn *Transaction, keysChanged map[string]struct{}) func() {
	if len(m.observers) == 0 {
		return nil
	}
	keys := m.computeKeys(txn, keysChanged)
	snap := make([]mapSub, len(m.observers))
	copy(snap, m.observers)
	e := YMapEvent{Target: m, Txn: txn, KeysChanged: keysChanged, Keys: keys}
	return func() {
		for _, s := range snap {
			s.fn(e)
		}
	}
}

// computeKeys derives the per-key KeyChange map (#74 D2) for every key in
// keysChanged. For each key it walks the items with that ParentSub to
// determine:
//   - the pre-transaction winner (most-recent live item before the txn)
//   - the post-transaction winner (currently live item, or nil)
//
// and from the two derives an Action (add / update / delete) plus the
// pre-transaction value.
//
// "Live before the txn" = !isNew && (!Deleted || txn.deleteSet.IsDeleted(item)).
// That is: existed before this txn AND was not already-tombstoned at the
// start. Pre-existing items that were already deleted before this txn are
// ignored.
func (m *YMap) computeKeys(txn *Transaction, keysChanged map[string]struct{}) map[string]KeyChange {
	if len(keysChanged) == 0 {
		return nil
	}
	t := &m.abstractType
	out := make(map[string]KeyChange, len(keysChanged))
	for key := range keysChanged {
		var preWinner *Item // most recent item live before the txn
		var postWinner *Item
		// Walk items in linked-list (YATA) order, not clock order. For
		// map-keyed entries the rightmost item with a given ParentSub is the
		// last-write-wins winner regardless of client, so the LAST matching
		// item we encounter is the winning candidate — exactly what the
		// `if preLive` / `if !item.Deleted` reassignments below rely on.
		for item := t.start; item != nil; item = item.Right {
			if item.ParentSub == nil || *item.ParentSub != key {
				continue
			}
			beforeClock := txn.beforeState.Clock(item.ID.Client)
			isNew := item.ID.Clock >= beforeClock
			// Was live before this txn?
			deletedInTxn := txn.deleteSet.IsDeleted(item.ID)
			preLive := !isNew && (!item.Deleted || deletedInTxn)
			if preLive {
				preWinner = item
			}
			if !item.Deleted {
				postWinner = item
			}
		}

		var change KeyChange
		switch {
		case preWinner == nil && postWinner != nil:
			change.Action = KeyAdded
		case preWinner != nil && postWinner != nil && preWinner != postWinner:
			change.Action = KeyUpdated
			change.OldValue = extractMapValue(preWinner)
		case preWinner != nil && postWinner == nil:
			change.Action = KeyDeleted
			change.OldValue = extractMapValue(preWinner)
		default:
			// Either both nil (transient: created+deleted in same txn) or
			// same item (no real change). Skip — don't emit a Keys entry.
			continue
		}
		out[key] = change
	}
	return out
}

// extractMapValue pulls a single map value out of an item's Content, used
// when computing KeyChange.OldValue. Matches the unwrap rules in
// entriesLocked so consumers see consistent shapes.
func extractMapValue(item *Item) any {
	switch c := item.Content.(type) {
	case *ContentAny:
		if len(c.Vals) > 0 {
			return c.Vals[0]
		}
	case *ContentJSON:
		if len(c.Vals) > 0 {
			return c.Vals[0]
		}
	case *ContentEmbed:
		return c.Val
	case *ContentType:
		return toJSONValue(c)
	}
	return nil
}

// Set writes value under key. If a live entry already exists for key, it is
// deleted and the new item becomes the winner.
func (m *YMap) Set(txn *Transaction, key string, value any) {
	t := &m.abstractType

	// Establish a causal link from the previous value for this key so that
	// YATA places the new item right after the old one — not at the list head.
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
		ParentSub: strPtr(key),
		Content:   NewContentAny(value),
	}
	item.integrate(txn, 0)
}

// Delete removes the entry for key if it exists.
func (m *YMap) Delete(txn *Transaction, key string) {
	t := &m.abstractType
	if item, ok := t.itemMap[key]; ok && !item.Deleted {
		item.delete(txn)
	}
}

// Get returns the value for key and whether the key exists.
// Must not be called from inside a Transact callback.
func (m *YMap) Get(key string) (any, bool) {
	if doc := m.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &m.abstractType
	item, ok := t.itemMap[key]
	if !ok || item.Deleted {
		return nil, false
	}
	ca, ok := item.Content.(*ContentAny)
	if !ok || len(ca.Vals) == 0 {
		return nil, false
	}
	return ca.Vals[0], true
}

// Has reports whether key has a live (non-deleted) entry.
// Must not be called from inside a Transact callback.
func (m *YMap) Has(key string) bool {
	if doc := m.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &m.abstractType
	item, ok := t.itemMap[key]
	return ok && !item.Deleted
}

// Keys returns all keys with live entries.
// Must not be called from inside a Transact callback.
func (m *YMap) Keys() []string {
	if doc := m.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &m.abstractType
	keys := make([]string, 0)
	for k, item := range t.itemMap {
		if !item.Deleted {
			keys = append(keys, k)
		}
	}
	return keys
}

// Entries returns a snapshot of all live key-value pairs. Nested shared
// types are recursively unwrapped (#75): nested YArray → []any, nested
// YMap → map[string]any, nested YText → string. Pre-fix these were
// silently dropped from the output.
//
// Must not be called from inside a Transact callback.
func (m *YMap) Entries() map[string]any {
	if doc := m.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	return m.entriesLocked()
}

// entriesLocked is the lock-free body of Entries; callers must already
// hold the doc lock. Used by Entries (top-level) and toJSONValue (during
// recursive unwrap of nested types).
func (m *YMap) entriesLocked() map[string]any {
	t := &m.abstractType
	out := make(map[string]any, len(t.itemMap))
	for k, item := range t.itemMap {
		if item.Deleted {
			continue
		}
		switch c := item.Content.(type) {
		case *ContentAny:
			if len(c.Vals) > 0 {
				out[k] = c.Vals[0]
			}
		case *ContentJSON:
			// ContentJSON is the legacy JSON wire variant (tag wireJSON=2);
			// functionally equivalent to ContentAny. Without this case,
			// keys received via JS-peer updates would be silently dropped.
			if len(c.Vals) > 0 {
				out[k] = c.Vals[0]
			}
		case *ContentEmbed:
			out[k] = c.Val
		case *ContentType:
			out[k] = toJSONValue(c)
		}
	}
	return out
}

// ForEach calls fn for every live (non-deleted) key-value pair in the map,
// in an unspecified order. Must not be called from inside a Transact callback.
func (m *YMap) ForEach(fn func(key string, value any)) {
	if doc := m.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &m.abstractType
	for k, item := range t.itemMap {
		if item.Deleted {
			continue
		}
		if ca, ok := item.Content.(*ContentAny); ok && len(ca.Vals) > 0 {
			fn(k, ca.Vals[0])
		}
	}
}

// ToJSON returns the map serialised as a JSON object.
// Must not be called from inside a Transact callback.
func (m *YMap) ToJSON() ([]byte, error) {
	return json.Marshal(m.Entries())
}

// Observe registers fn to be called after every transaction that modifies this
// map. Returns an unsubscribe function. Uses ID-based lookup so out-of-order
// unsubscription removes the correct entry (C5).
//
// Acquiring doc.mu.Lock() serialises registration against Transact (N-C1).
// Do not call Observe from inside a Transact callback — that would deadlock.
func (m *YMap) Observe(fn func(YMapEvent)) func() {
	doc := m.doc
	if doc != nil {
		doc.mu.Lock()
		defer doc.mu.Unlock()
	}
	m.subIDGen++
	id := m.subIDGen
	m.observers = append(m.observers, mapSub{id: id, fn: fn})
	return func() {
		if doc := m.doc; doc != nil {
			doc.mu.Lock()
			defer doc.mu.Unlock()
		}
		for i, s := range m.observers {
			if s.id == id {
				m.observers = append(m.observers[:i], m.observers[i+1:]...)
				return
			}
		}
	}
}

// ObserveDeep registers fn to be called after any transaction that modifies
// this map or any nested shared type within it. Returns an unsubscribe function.
func (m *YMap) ObserveDeep(fn func(*Transaction)) func() {
	return m.observeDeep(fn)
}
