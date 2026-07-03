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
