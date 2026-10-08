package crdt

import (
	"reflect"
	"testing"
)

// A map entry (YMap key or XML attribute) held by a multi-value item reads
// its last value on every read path, as Yjs's getContent()[length-1] does;
// Yjs's API never writes one, but its decoder accepts them.

// integrateMapEntry writes content under key as one raw item.
func integrateMapEntry(txn *Transaction, parent *abstractType, key string, c Content) {
	var left *Item
	var origin *ID
	if existing, ok := parent.itemMap[key]; ok {
		left = existing
		id := existing.ID
		origin = &id
	}
	item := &Item{
		ID:        ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:    origin,
		Left:      left,
		Parent:    parent,
		ParentSub: strPtr(key),
		Content:   c,
	}
	item.integrate(txn, 0)
}

func TestUnit_MapMultiValue_EveryReadReturnsLast(t *testing.T) {
	for name, mk := range map[string]func(...any) Content{
		"any":  func(v ...any) Content { return NewContentAny(v...) },
		"json": func(v ...any) Content { return NewContentJSON(v...) },
	} {
		t.Run(name, func(t *testing.T) {
			doc := newTestDoc(1)
			m := doc.GetMap("m")
			var events []YMapEvent
			m.Observe(func(e YMapEvent) { events = append(events, e) })
			doc.Transact(func(txn *Transaction) {
				integrateMapEntry(txn, &m.abstractType, "k", mk("first", "last"))
			})

			if v, ok := m.Get("k"); !ok || v != "last" {
				t.Errorf("Get = %#v, %v; want last", v, ok)
			}
			if got := m.Entries(); !reflect.DeepEqual(got, map[string]any{"k": "last"}) {
				t.Errorf("Entries = %#v", got)
			}
			m.ForEach(func(k string, v any) {
				if v != "last" {
					t.Errorf("ForEach(%q) = %#v", k, v)
				}
			})
			if j, _ := m.ToJSON(); string(j) != `{"k":"last"}` {
				t.Errorf("ToJSON = %s", j)
			}
			if !m.Has("k") || !reflect.DeepEqual(m.Keys(), []string{"k"}) {
				t.Errorf("Has/Keys = %v/%v", m.Has("k"), m.Keys())
			}

			doc.Transact(func(txn *Transaction) { m.Set(txn, "k", "new") })
			if n := len(events); n != 2 {
				t.Fatalf("%d events, want 2", n)
			}
			if got := events[1].Keys["k"]; got.Action != KeyUpdated || got.OldValue != "last" {
				t.Errorf("update event = %+v, want OldValue last", got)
			}
		})
	}
}

func TestUnit_XMLAttributeMultiValue_ReadsLast(t *testing.T) {
	for name, mk := range map[string]func(...any) Content{
		"any":  func(v ...any) Content { return NewContentAny(v...) },
		"json": func(v ...any) Content { return NewContentJSON(v...) },
	} {
		t.Run(name, func(t *testing.T) {
			doc := newTestDoc(1)
			frag := doc.GetXmlFragment("x")
			p := NewYXmlElement("p")
			doc.Transact(func(txn *Transaction) {
				frag.InsertElement(txn, 0, p)
				integrateMapEntry(txn, &p.abstractType, "a", mk("old", "new"))
			})
			if v, ok := p.GetAttributeValue("a"); !ok || v != "new" {
				t.Errorf("GetAttributeValue = %#v, %v; want new", v, ok)
			}
			if got := p.GetAttributeValues(); !reflect.DeepEqual(got, map[string]any{"a": "new"}) {
				t.Errorf("GetAttributeValues = %#v", got)
			}
			if got := frag.ToXML(); got != `<p a="new"></p>` {
				t.Errorf("ToXML = %q", got)
			}
		})
	}
}
