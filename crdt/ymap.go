package crdt

// YMap is a shared key-value store with last-write-wins semantics.
// It embeds abstractType, which owns the underlying doubly-linked Item list.
// Every key maps to at most one live Item; concurrent writes to the same key
// are resolved deterministically: the item with the higher ClientID wins.
type YMap struct {
	abstractType
	observers []func(YMapEvent)
}

func (m *YMap) baseType() *abstractType { return &m.abstractType }

func (m *YMap) fire(txn *Transaction, keysChanged map[string]struct{}) {
	if len(m.observers) == 0 {
		return
	}
	e := YMapEvent{Target: m, Txn: txn, KeysChanged: keysChanged}
	for _, fn := range m.observers {
		fn(e)
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
		ID:        ID{Client: txn.doc.ClientID, Clock: txn.doc.store.NextClock(txn.doc.ClientID)},
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
func (m *YMap) Get(key string) (any, bool) {
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
func (m *YMap) Has(key string) bool {
	t := &m.abstractType
	item, ok := t.itemMap[key]
	return ok && !item.Deleted
}

// Keys returns all keys with live entries.
func (m *YMap) Keys() []string {
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
func (m *YMap) Entries() map[string]any {
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

// Observe registers fn to be called after every transaction that modifies this
// map. Returns an unsubscribe function.
func (m *YMap) Observe(fn func(YMapEvent)) func() {
	m.observers = append(m.observers, fn)
	idx := len(m.observers) - 1
	return func() {
		m.observers = append(m.observers[:idx], m.observers[idx+1:]...)
	}
}

// ObserveDeep registers fn to be called after any transaction that modifies
// this map or any nested shared type within it. Returns an unsubscribe function.
func (m *YMap) ObserveDeep(fn func(*Transaction)) func() {
	m.abstractType.deepObservers = append(m.abstractType.deepObservers, fn)
	idx := len(m.abstractType.deepObservers) - 1
	return func() {
		obs := m.abstractType.deepObservers
		m.abstractType.deepObservers = append(obs[:idx], obs[idx+1:]...)
	}
}
