package crdt

import (
	"reflect"
	"testing"
)

func TestEncodeDecodeIDSet_RoundTrip(t *testing.T) {
	s := NewIDSet()
	s.Add(1, 0, 5)
	s.Add(1, 10, 8)
	s.Add(7, 3, 1)
	s.Add(42, 100, 1000)

	data := EncodeIDSet(s)
	got, err := DecodeIDSet(data)
	if err != nil {
		t.Fatalf("DecodeIDSet: %v", err)
	}
	if !reflect.DeepEqual(got.Clients(), s.Clients()) {
		t.Fatalf("clients = %v, want %v", got.Clients(), s.Clients())
	}
	for _, c := range s.Clients() {
		if !reflect.DeepEqual(got.Ranges(c), s.Ranges(c)) {
			t.Fatalf("client %d ranges = %v, want %v", c, got.Ranges(c), s.Ranges(c))
		}
	}
	// Canonical re-encode stability.
	if again := EncodeIDSet(got); !reflect.DeepEqual(again, data) {
		t.Fatal("re-encode of decoded IDSet is not byte-stable")
	}
}

func TestEncodeIDSet_EmptyAndOmission(t *testing.T) {
	if got, err := DecodeIDSet(EncodeIDSet(NewIDSet())); err != nil || !got.IsEmpty() {
		t.Fatalf("empty round-trip: got %v err %v", got, err)
	}
}

func TestDecodeIDSet_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"truncated header":  {},
		"huge client count": {0xFF, 0xFF, 0xFF, 0xFF, 0x0F}, // numClients ≫ remaining bytes
		"truncated ranges":  {1, 1, 5},                      // 1 client, client=1, 5 ranges, no data
	}
	for name, data := range cases {
		if _, err := DecodeIDSet(data); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestEncodeDecodeIDMap_RoundTripWithDedup(t *testing.T) {
	m := NewIDMap()
	shared := attrs(t, "user", "alice", "ts", int64(1000))
	m.Add(3, 0, 4, shared)
	m.Add(3, 10, 2, shared)                     // same attrs -> attr table hit
	m.Add(1, 5, 5, attrs(t, "user", "bob"))     // new attr, shared name "user"
	m.Add(1, 20, 1, attrs(t, "reviewed", true)) // new name

	data := EncodeIDMap(m)
	got, err := DecodeIDMap(data)
	if err != nil {
		t.Fatalf("DecodeIDMap: %v", err)
	}
	if !reflect.DeepEqual(got.Clients(), m.Clients()) {
		t.Fatalf("clients = %v, want %v", got.Clients(), m.Clients())
	}
	for _, c := range m.Clients() {
		w, g := m.Ranges(c), got.Ranges(c)
		if len(w) != len(g) {
			t.Fatalf("client %d: %d ranges, want %d", c, len(g), len(w))
		}
		for i := range w {
			if w[i].Clock != g[i].Clock || w[i].Len != g[i].Len || len(w[i].Attrs) != len(g[i].Attrs) {
				t.Fatalf("client %d range %d = %+v, want %+v", c, i, g[i], w[i])
			}
			for j := range w[i].Attrs {
				if attrKey(w[i].Attrs[j]) != attrKey(g[i].Attrs[j]) {
					t.Fatalf("client %d range %d attr %d differs", c, i, j)
				}
			}
		}
	}
	// Decoded attrs with equal values must be the SAME interned instance.
	r3 := got.Ranges(3)
	if r3[0].Attrs[0] != r3[1].Attrs[0] {
		t.Fatal("decoded equal attrs should be one interned instance")
	}
	// Canonical re-encode stability.
	if again := EncodeIDMap(got); !reflect.DeepEqual(again, data) {
		t.Fatal("re-encode of decoded IDMap is not byte-stable")
	}
}

func TestDecodeIDMap_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"truncated":        {},
		"huge clients":     {0xFF, 0xFF, 0xFF, 0x0F},
		"huge attr count":  {1, 1, 1, 0, 0, 0xFF, 0xFF, 0xFF, 0x0F},
		"dangling attr id": {1, 1, 1, 0, 0, 1, 9}, // attrID 9 refers past the table with no definition bytes
	}
	for name, data := range cases {
		if _, err := DecodeIDMap(data); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// --- Nil-half tolerance (issue #56 final review: nil-half dereference panic) ---

func TestEncodeIDSet_Nil(t *testing.T) {
	got := EncodeIDSet(nil)
	want := EncodeIDSet(NewIDSet())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EncodeIDSet(nil) = %v, want %v (same as empty IDSet)", got, want)
	}
	decoded, err := DecodeIDSet(got)
	if err != nil {
		t.Fatalf("DecodeIDSet(EncodeIDSet(nil)): %v", err)
	}
	if !decoded.IsEmpty() {
		t.Fatalf("DecodeIDSet(EncodeIDSet(nil)) = %v, want empty", decoded)
	}
}

func TestEncodeIDMap_Nil(t *testing.T) {
	got := EncodeIDMap(nil)
	want := EncodeIDMap(NewIDMap())
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EncodeIDMap(nil) = %v, want %v (same as empty IDMap)", got, want)
	}
	decoded, err := DecodeIDMap(got)
	if err != nil {
		t.Fatalf("DecodeIDMap(EncodeIDMap(nil)): %v", err)
	}
	if !decoded.IsEmpty() {
		t.Fatalf("DecodeIDMap(EncodeIDMap(nil)) = %v, want empty", decoded)
	}
}

func TestEncodeContentIDs_ZeroValue(t *testing.T) {
	data := EncodeContentIDs(ContentIDs{}) // both halves nil
	got, err := DecodeContentIDs(data)
	if err != nil {
		t.Fatalf("DecodeContentIDs: %v", err)
	}
	if !got.Inserts.IsEmpty() || !got.Deletes.IsEmpty() {
		t.Fatalf("DecodeContentIDs(EncodeContentIDs(ContentIDs{})) = %+v, want two empty halves", got)
	}
}

func TestEncodeContentMap_ZeroValue(t *testing.T) {
	data := EncodeContentMap(ContentMap{}) // both halves nil
	got, err := DecodeContentMap(data)
	if err != nil {
		t.Fatalf("DecodeContentMap: %v", err)
	}
	if !got.Inserts.IsEmpty() || !got.Deletes.IsEmpty() {
		t.Fatalf("DecodeContentMap(EncodeContentMap(ContentMap{})) = %+v, want two empty halves", got)
	}
}
