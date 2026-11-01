package crdt

import (
	"errors"
	"math"
	"runtime"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// rawDeleteEntry is one client entry of an encoded delete set, its ranges as
// written, neither sorted nor merged; a declared count above zero replaces the
// number of ranges in the header.
type rawDeleteEntry struct {
	client   ClientID
	ranges   []DeleteRange
	declared uint64
}

func (e rawDeleteEntry) count() uint64 {
	if e.declared > 0 {
		return e.declared
	}
	return uint64(len(e.ranges))
}

// encodeRawDeleteSet is a struct-free update whose delete set holds entries as
// given, in V1 or V2.
func encodeRawDeleteSet(entries []rawDeleteEntry, v2 bool) []byte {
	if !v2 {
		return encoding.EncodeBytes(func(enc *encoding.Encoder) {
			enc.WriteVarUint(0)
			enc.WriteVarUint(uint64(len(entries)))
			for _, e := range entries {
				enc.WriteVarUint(uint64(e.client))
				enc.WriteVarUint(e.count())
				for _, r := range e.ranges {
					enc.WriteVarUint(r.Clock)
					enc.WriteVarUint(r.Len)
				}
			}
		})
	}
	enc := newV2Encoder()
	enc.restEnc.WriteVarUint(0)
	enc.restEnc.WriteVarUint(uint64(len(entries)))
	for _, e := range entries {
		enc.resetDsCurVal()
		enc.restEnc.WriteVarUint(uint64(e.client))
		enc.restEnc.WriteVarUint(e.count())
		for _, r := range e.ranges {
			enc.writeDsClock(r.Clock)
			enc.writeDsLen(r.Len)
		}
	}
	return enc.toBytes()
}

// craftedDeleteSet is a delete set for clients a fresh document has no struct
// of; a cut one ends before its last declared range.
type craftedDeleteSet struct {
	name    string
	entries []rawDeleteEntry
	cut     bool
}

// craftedDeleteSets are delete sets of about a MiB, as written in V1 or V2.
func craftedDeleteSets(v2 bool) []craftedDeleteSet {
	oneClient := func(n int, clock func(int) uint64) []rawDeleteEntry {
		ranges := make([]DeleteRange, n)
		for i := range ranges {
			ranges[i] = DeleteRange{Clock: clock(i), Len: 1}
		}
		return []rawDeleteEntry{{client: 4242, ranges: ranges}}
	}
	// Two bytes a range, the fewest, in ranges that merge into one: V1 writes
	// each clock whole, and V2 writes the step from the last range's end.
	merging := func(i int) uint64 { return uint64(i%120) + 1 }
	if v2 {
		merging = func(i int) uint64 { return uint64(i) }
	}
	many := make([]rawDeleteEntry, 170_000)
	for c := range many {
		many[c] = rawDeleteEntry{client: ClientID(5000 + c), ranges: []DeleteRange{{Clock: 0, Len: 1}}}
	}
	// Clients that the header declares and that add no range must cost nothing.
	empty := make([]rawDeleteEntry, 250_000)
	for c := range empty {
		empty[c] = rawDeleteEntry{client: ClientID(5000 + c)}
	}
	// A client named in many entries must get one list, sized once.
	repeated := func(clients int) []rawDeleteEntry {
		entries := make([]rawDeleteEntry, 1<<18)
		for i := range entries {
			entries[i] = rawDeleteEntry{client: ClientID(7 + i%clients), ranges: []DeleteRange{{Clock: uint64(i%120) + 1, Len: 1}}}
		}
		return entries
	}
	// A range count past what the bytes hold must not size a larger list; the
	// apply refuses the cut set after it applies the ranges before the cut.
	cut := oneClient(1<<18, func(i int) uint64 { return uint64(2 * i) })
	cut[0].declared = 1 << 40
	return []craftedDeleteSet{
		{name: "one client, ranges that merge", entries: oneClient(1<<19, merging)},
		{name: "one client, distinct ranges", entries: oneClient(1<<18, func(i int) uint64 { return uint64(2 * i) })},
		{name: "one client, a count past the bytes", entries: cut, cut: true},
		{name: "one client in every entry", entries: repeated(1)},
		{name: "two clients in turn", entries: repeated(2)},
		{name: "many clients, one range each", entries: many},
		{name: "many clients, no range", entries: empty},
	}
}

// allocatedBy is the fewest bytes f allocates over three runs, since a run can
// also count what another goroutine allocates meanwhile.
func allocatedBy(f func()) uint64 {
	fewest := uint64(math.MaxUint64)
	for range 3 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		fewest = min(fewest, after.TotalAlloc-before.TotalAlloc)
	}
	return fewest
}

// An apply allocates for a delete set at most a fixed multiple of its bytes,
// which bounds the heap it adds, because a counting first pass lets the decoder
// size each list once: 16 bytes for a range that takes at least two, and one
// map entry per client, however many entries name it. Refused or parked at the
// default cap it holds the decoded set; parked whole with the cap lifted it
// also holds the store's own copy.
func TestUnit_ApplyUpdate_ADeleteSetAllocatesInProportionToItsBytes(t *testing.T) {
	const atTheCap, capLifted = 20, 36
	for _, format := range applyFormats {
		for _, shape := range craftedDeleteSets(format.v2) {
			update, apply := encodeRawDeleteSet(shape.entries, format.v2), ApplyUpdateV1
			if format.v2 {
				apply = ApplyUpdateV2
			}
			for _, lifted := range []bool{false, true} {
				name, bound, opts := shape.name+" "+format.name, atTheCap, []DocOption{WithClientID(1)}
				if lifted {
					name, bound, opts = name+", cap lifted", capLifted, append(opts, WithMaxPendingItems(math.MaxInt))
				}
				var err error
				allocated := allocatedBy(func() { err = apply(New(opts...), update, nil) })
				if shape.cut && !errors.Is(err, ErrInvalidUpdate) || !shape.cut && err != nil && (lifted || !errors.Is(err, ErrInvalidUpdate)) {
					t.Fatalf("%s: err %v", name, err)
				}
				ratio := float64(allocated) / float64(len(update))
				t.Logf("%s: %d bytes allocated for %d update bytes, %.1f times, err %v", name, allocated, len(update), ratio, err)
				if ratio > float64(bound) {
					t.Errorf("%s: allocated %.1f times the update's bytes, want at most %d", name, ratio, bound)
				}
			}
		}
	}
}
