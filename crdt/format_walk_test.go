package crdt

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestUnit_YText_WalkFormatting_InvisibleBoundaries(t *testing.T) {
	for _, writers := range []int{1, 3, 15} {
		t.Run(fmt.Sprint(writers), func(t *testing.T) {
			seed := New(WithClientID(1))
			defer seed.Destroy()
			root := seed.GetMap("root")
			seed.Transact(func(tx *Transaction) {
				container := NewYXmlElement("doc")
				root.Set(tx, "field", container)
				p := NewYXmlElement("paragraph")
				container.InsertElement(tx, 0, p)
				text := NewYXmlText()
				p.InsertText(tx, 0, text)
				text.Insert(tx, 0, "abc", Attributes{"format": nil})
			})
			base := EncodeStateAsUpdateV1(seed, nil)
			clone := func(id ClientID) *Doc {
				d := New(WithClientID(id))
				d.GetMap("root")
				if err := ApplyUpdateV1(d, base, nil); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(d.Destroy)
				return d
			}
			textOf := func(d *Doc) *YXmlText {
				v, _ := d.GetMap("root").Get("field")
				return v.(*YXmlElement).Children()[0].(*YXmlElement).Children()[0].(*YXmlText)
			}
			updates := make([][]byte, 0, writers+1)
			for i := range writers {
				d := clone(ClientID(100 + i))
				text := textOf(d)
				before := d.StateVector()
				d.Transact(func(tx *Transaction) { text.Format(tx, 2, 1, Attributes{"format": map[string]any{"weight": i + 1}}) })
				updates = append(updates, EncodeStateAsUpdateV1(d, before))
			}
			deleted := clone(999)
			text := textOf(deleted)
			before := deleted.StateVector()
			deleted.Transact(func(tx *Transaction) { text.Delete(tx, 0, 3) })
			updates = append(updates, EncodeStateAsUpdateV1(deleted, before))
			merged, err := MergeUpdatesV1(updates...)
			if err != nil {
				t.Fatal(err)
			}
			actual := clone(1000)
			if err := ApplyUpdateV1(actual, merged, nil); err != nil {
				t.Fatal(err)
			}
			current := textOf(actual)
			if current.Len() != 0 || len(current.ToDelta()) != 0 {
				t.Fatal("expected empty rich text")
			}
			checkpoint := EncodeStateAsUpdateV1(actual, nil)
			markers := uint64(0)
			if err := current.WalkFormatting(func(key string, value any) error {
				if key != "format" {
					t.Fatal(key)
				}
				markers++
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			total := uint64(0)
			live := InsertSetFromDoc(actual, true)
			for _, client := range live.Clients() {
				for _, span := range live.Ranges(client) {
					total += span.Len
				}
			}
			if markers == 0 || total != 3+markers {
				t.Fatal("independent live-clock mismatch", markers, total)
			}
			if !bytes.Equal(checkpoint, EncodeStateAsUpdateV1(actual, nil)) {
				t.Fatal("visitor modified native document")
			}
			sentinel := errors.New("stop")
			calls := 0
			err = current.WalkFormatting(func(string, any) error { calls++; return sentinel })
			if !errors.Is(err, sentinel) || calls != 1 {
				t.Fatal("callback failure not preserved", err, calls)
			}
			t.Log("live markers", markers, "total live clocks", total)
		})
	}
}

func TestUnit_YText_WalkFormatting_ConcurrentEdit(t *testing.T) {
	t.Parallel()
	doc := New()
	defer doc.Destroy()
	text := doc.GetText("text")
	doc.Transact(func(tx *Transaction) { text.Insert(tx, 0, "abc", Attributes{"format": 0}) })
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		for range 1000 {
			if err := text.WalkFormatting(func(string, any) error { return nil }); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer workers.Done()
		<-start
		for i := range 1000 {
			doc.Transact(func(tx *Transaction) { text.Format(tx, 0, 3, Attributes{"format": i % 2}) })
		}
	}()
	close(start)
	workers.Wait()
}

func TestUnit_YText_WalkFormatting_ValuesAndEarlyReturn(t *testing.T) {
	doc := New()
	defer doc.Destroy()
	text := doc.GetText("text")
	doc.Transact(func(tx *Transaction) {
		text.Insert(tx, 0, "abc", nil)
		text.Format(tx, 0, 1, Attributes{"bold": true})
	})
	var values []any
	if err := text.WalkFormatting(func(key string, value any) error {
		if key != "bold" {
			t.Fatalf("unexpected key %q", key)
		}
		values = append(values, value)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0] != true || values[1] != nil {
		t.Fatalf("unexpected markers: %#v", values)
	}
	sentinel := errors.New("stop")
	if err := text.WalkFormatting(func(string, any) error { return sentinel }); err != sentinel {
		t.Fatalf("expected callback error, got %v", err)
	}
	doc.Transact(func(tx *Transaction) { text.Insert(tx, 3, "d", nil) })
	if got := text.ToString(); got != "abcd" {
		t.Fatalf("unexpected text after callback error: %q", got)
	}
}
