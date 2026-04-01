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
func (m *YMap) prepareFire(txn *Transaction, keysChanged map[string]struct{}) func() {
	if len(m.observers) == 0 {
		return nil
	}
	snap := make([]mapSub, len(m.observers))
	copy(snap, m.observers)
	e := YMapEvent{Target: m, Txn: txn, KeysChanged: keysChanged}
	return func() {
		for _, s := range snap {
			s.fn(e)
		}
	}
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
		ParentSub: key,
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

// Entries returns a snapshot of all live key-value pairs.
// Must not be called from inside a Transact callback.
func (m *YMap) Entries() map[string]any {
	if doc := m.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	t := &m.abstractType
	out := make(map[string]any, len(t.itemMap))
	for k, item := range t.itemMap {
		if item.Deleted {
			continue
		}
		if ca, ok := item.Content.(*ContentAny); ok && len(ca.Vals) > 0 {
			out[k] = ca.Vals[0]
		}
	}
	return out
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
