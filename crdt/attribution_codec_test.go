package crdt

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// A single range carrying more attributes than maxPreallocAttrs must still
// round-trip intact: the capped pre-allocation in readIDMap (which prevents a
// crafted attrCount from forcing a huge up-front pointer-slice allocation) is
// only a capacity hint, so append must grow the slice past the cap for a
// genuinely large, valid range. Guards against a future change turning the cap
// into a truncation or a hard rejection of valid input.
func TestDecodeIDMap_AttrPreallocCapDoesNotTruncate(t *testing.T) {
	n := maxPreallocAttrs + 5
	attrs := make([]*ContentAttribute, 0, n)
	for i := 0; i < n; i++ {
		attrs = append(attrs, MustContentAttribute("k"+strconv.Itoa(i), int64(i)))
	}
	m := NewIDMap()
	m.Add(1, 0, 1, attrs)

	got, err := DecodeIDMap(EncodeIDMap(m))
	if err != nil {
		t.Fatalf("DecodeIDMap: %v", err)
	}
	r := got.Ranges(1)
	if len(r) != 1 {
		t.Fatalf("expected 1 range, got %d", len(r))
	}
	if len(r[0].Attrs) != n {
		t.Fatalf("attrs truncated: got %d, want %d", len(r[0].Attrs), n)
	}
}

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

// attrWords is words as varuints, the raw form of an IdSet or IdMap.
func attrWords(words ...uint64) []byte {
	enc := encoding.NewEncoder()
	for _, w := range words {
		enc.WriteVarUint(w)
	}
	return enc.Bytes()
}

// rangeWords is one client's ranges, each given as its clock diff and length
// minus one, with an IdMap range's attribute count of zero after each pair.
func rangeWords(idMap bool, pairs ...uint64) []uint64 {
	words := []uint64{1, 1, uint64(len(pairs) / 2)}
	for i := 0; i < len(pairs); i += 2 {
		words = append(words, pairs[i], pairs[i+1])
		if idMap {
			words = append(words, 0)
		}
	}
	return words
}

// runningPairs is 2,048 one-clock ranges whose running clock ends at 2^64-1
// or, with wrap, 2,049 whose clock would pass it and come back to clock 1.
func runningPairs(wrap bool) []uint64 {
	var pairs []uint64
	for range 2047 {
		pairs = append(pairs, maxSafeInteger, 0) // each moves the clock by 2^53
	}
	if wrap {
		return append(pairs, maxSafeInteger, 0, 1, 0)
	}
	return append(pairs, maxSafeInteger-1, 0)
}

// The attribution decoders read a range's clock as a running sum and refuse
// one that would pass 2^64-1, so it cannot wrap back onto a low clock, and
// an IdMap's client sum past 2^55-1; clocks Yjs v14 holds past 2^53 decode.
func TestUnit_DecodeAttribution_RefusesARunningClockPast2To64(t *testing.T) {
	idSet := func(data []byte) error { _, err := DecodeIDSet(data); return err }
	idMap := func(data []byte) error { _, err := DecodeIDMap(data); return err }
	contentIDs := func(data []byte) error { _, err := DecodeContentIDs(data); return err }
	contentMap := func(data []byte) error { _, err := DecodeContentMap(data); return err }
	type decodeCase struct {
		name   string
		decode func([]byte) error
		data   []byte
		ok     bool
	}
	var cases []decodeCase
	for _, kind := range []struct {
		name   string
		idMap  bool
		decode func([]byte) error
	}{{"IdSet", false, idSet}, {"IdMap", true, idMap}} {
		ranges := func(pairs ...uint64) []byte { return attrWords(rangeWords(kind.idMap, pairs...)...) }
		cases = append(cases,
			decodeCase{kind.name + " range of 2 at 2^53", kind.decode, ranges(1<<53, 1), true},
			decodeCase{kind.name + " range ending past 2^53", kind.decode, ranges(maxSafeInteger, 1), true},
			decodeCase{kind.name + " running clock ending at 2^64-1", kind.decode, ranges(runningPairs(false)...), true},
			decodeCase{kind.name + " running clock past 2^64-1", kind.decode, ranges(runningPairs(true)...), false},
		)
	}
	wrapped := func(idMap bool) []byte {
		return append([]byte{0}, attrWords(rangeWords(idMap, runningPairs(true)...)...)...)
	}
	cases = append(cases,
		decodeCase{"IdMap client 2^55-1", idMap, attrWords(1, maxClient, 1, 0, 0, 0), true},
		decodeCase{"IdMap client sum 2^55", idMap, attrWords(2, maxClient, 1, 0, 0, 0, 1, 1, 0, 0, 0), false},
		decodeCase{"ContentIDs deletes past 2^64-1", contentIDs, wrapped(false), false},
		decodeCase{"ContentMap deletes past 2^64-1", contentMap, wrapped(true), false},
	)
	for _, c := range cases {
		err := c.decode(c.data)
		if (err == nil) != c.ok {
			t.Errorf("%s: err %v, want accepted %v", c.name, err, c.ok)
		}
		if err != nil && (!errors.Is(err, ErrAttributionDecode) || !strings.Contains(err.Error(), "past 2^")) {
			t.Errorf("%s: err %v is not ErrAttributionDecode for a bound", c.name, err)
		}
	}
}

// Ranges at and past 2^53, which Yjs v14 writes and reads, round-trip through
// the encoders and match Yjs v14's bytes.
func TestUnit_AttributionCodecs_RoundTripClocksPast2To53(t *testing.T) {
	set := NewIDSet()
	set.Add(1, 1<<53, 2)
	set.Add(2, maxSafeInteger, 3)
	// Yjs 14.0.0-16 writeIdSet of the same set.
	yjs, err := hex.DecodeString("020201ffffffffffffff0f020101808080808080801001")
	if err != nil {
		t.Fatal(err)
	}
	if got := EncodeIDSet(set); !bytes.Equal(got, yjs) {
		t.Errorf("EncodeIDSet: %x, want Yjs v14's %x", got, yjs)
	}
	back, err := DecodeIDSet(yjs)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []ClientID{1, 2} {
		if !reflect.DeepEqual(back.Ranges(c), set.Ranges(c)) {
			t.Errorf("IdSet client %d: %v, want %v", c, back.Ranges(c), set.Ranges(c))
		}
	}
	m := NewIDMap()
	m.Add(1, 1<<53, 2, attrs(t, "user", "alice"))
	mb, err := DecodeIDMap(EncodeIDMap(m))
	if err != nil {
		t.Fatal(err)
	}
	if got := mb.Ranges(1); len(got) != 1 || got[0].Clock != 1<<53 || got[0].Len != 2 {
		t.Errorf("IdMap client 1: %+v, want one range of 2 at 2^53", got)
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
