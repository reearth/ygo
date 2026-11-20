//go:build go1.24

package crdt

import (
	"runtime"
	"strings"
	"testing"
	"unsafe"
	"weak"
)

// The tests in this file need the weak package, which Go 1.24 added; the
// module still builds with Go 1.23, which leaves them out.

// subscribeHeld subscribes, through subscribe, a closure holding a large
// array, and returns the unsubscribe, the list's owner and a weak pointer to
// the array: once nothing reaches the closure, a collection frees the array
// and the weak pointer reads nil. The closure is made here, so no frame of
// the test holds it.
func subscribeHeld(subscribe func(touch func()) (func(), any)) (unsubscribe func(), owner any, freed weak.Pointer[[1 << 20]byte]) {
	big := new([1 << 20]byte)
	unsubscribe, owner = subscribe(func() { big[0]++ })
	return unsubscribe, owner, weak.Make(big)
}

// An unsubscribe clears the slot it vacates in the list it removes from, so
// the callback it removes is reachable from nothing once its caller drops it.
// Each list gets a subscription that stays and then the one under test, the
// last, whose slot is the one a removal by append(s[:i], s[i+1:]...) leaves
// holding it.
func TestUnit_Unsubscribe_ClearsTheVacatedSlot(t *testing.T) {
	for _, c := range []struct {
		name string
		// subscribe subscribes touch on a list, after another subscription,
		// and returns the unsubscribe and the list's owner.
		subscribe func(touch func()) (unsubscribe func(), owner any)
	}{
		{"Doc.OnUpdate", func(touch func()) (func(), any) {
			doc := New()
			doc.OnUpdate(func([]byte, any) {})
			return doc.OnUpdate(func([]byte, any) { touch() }), doc
		}},
		{"Doc.OnAfterTransaction", func(touch func()) (func(), any) {
			doc := New()
			doc.OnAfterTransaction(func(*Transaction) {})
			return doc.OnAfterTransaction(func(*Transaction) { touch() }), doc
		}},
		{"Doc.OnSubdocs", func(touch func()) (func(), any) {
			doc := New()
			doc.OnSubdocs(func(SubdocsEvent) {})
			return doc.OnSubdocs(func(SubdocsEvent) { touch() }), doc
		}},
		{"YArray.Observe", func(touch func()) (func(), any) {
			doc := New()
			a := doc.GetArray("a")
			a.Observe(func(YArrayEvent) {})
			return a.Observe(func(YArrayEvent) { touch() }), doc
		}},
		{"YArray.ObserveDeep", func(touch func()) (func(), any) {
			doc := New()
			a := doc.GetArray("a")
			a.ObserveDeep(func(*Transaction) {})
			return a.ObserveDeep(func(*Transaction) { touch() }), doc
		}},
		{"YMap.Observe", func(touch func()) (func(), any) {
			doc := New()
			m := doc.GetMap("m")
			m.Observe(func(YMapEvent) {})
			return m.Observe(func(YMapEvent) { touch() }), doc
		}},
		{"YText.Observe", func(touch func()) (func(), any) {
			doc := New()
			text := doc.GetText("t")
			text.Observe(func(YTextEvent) {})
			return text.Observe(func(YTextEvent) { touch() }), doc
		}},
		{"YXmlFragment.Observe", func(touch func()) (func(), any) {
			doc := New()
			f := doc.GetXmlFragment("x")
			f.Observe(func(YXmlEvent) {})
			return f.Observe(func(YXmlEvent) { touch() }), doc
		}},
		{"YXmlElement.Observe", func(touch func()) (func(), any) {
			e := NewYXmlElement("p")
			e.Observe(func(YXmlEvent) {})
			return e.Observe(func(YXmlEvent) { touch() }), e
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			unsubscribe, owner, freed := subscribeHeld(c.subscribe)
			unsubscribe()
			runtime.GC()
			if freed.Value() != nil {
				t.Errorf("%s: the unsubscribed callback is still reachable from the list it was removed from", c.name)
			}
			runtime.KeepAlive(owner)
		})
	}
}

// setHeld sets the attribute key of e, an element not yet in a document, to
// a large string, and returns a weak pointer to the string's bytes; no frame
// of the test holds the string.
func setHeld(e *YXmlElement, key string) weak.Pointer[byte] {
	s := strings.Repeat("x", 1<<20)
	e.SetAttribute(nil, key, s)
	return weak.Make(unsafe.StringData(s))
}

// Deleting an attribute of an element not yet in a document clears the slot
// it vacates in the element's buffered attributes, so the value it removes is
// reachable from nothing.
func TestUnit_DeleteAttribute_ClearsTheVacatedPrelimSlot(t *testing.T) {
	e := NewYXmlElement("p")
	e.SetAttribute(nil, "keep", "v")
	freed := setHeld(e, "big")
	e.DeleteAttribute(nil, "big")
	runtime.GC()
	if freed.Value() != nil {
		t.Error("the deleted attribute's value is still reachable from the element's buffered attributes")
	}
	runtime.KeepAlive(e)
}
