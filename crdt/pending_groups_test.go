package crdt

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// Distinct parent clocks and lengths cannot collapse into equal-dependency
// spans. A fixed number of wire groups must not retain metadata for every item.
func pendingDiverseUpdate(version, n int, shape string) []byte {
	doc := New()
	defer doc.Destroy()
	key := "child"
	children := make([]*Item, n)
	parents := make([]*Item, 0)
	clock := uint64(0)
	for i := range children {
		parent := ID{Client: 99, Clock: uint64(i)}
		content := Content(NewContentAny(i))
		if shape == "lengths" {
			content = NewContentString(strings.Repeat("x", 1+i%3))
		}
		if shape == "cycle" {
			parent.Client = 2
		}
		children[i] = &Item{ID: ID{Client: 1, Clock: clock}, parentID: &parent, ParentSub: &key, Content: content}
		clock += uint64(content.Len())
		if shape == "cycle" {
			child := children[i].ID
			parents = append(parents, &Item{ID: ID{Client: 2, Clock: uint64(i)}, parentID: &child, ParentSub: &key, Content: NewContentAny(i)})
		}
	}
	groups := map[ClientID][]*Item{1: children}
	if shape == "cycle" {
		groups[2] = parents
	}
	if shape == "groups" || shape == "groups-unrelated" {
		groups = make(map[ClientID][]*Item, n+1)
		for i, child := range children {
			child.ID = ID{Client: ClientID(i + 100)}
			groups[child.ID.Client] = []*Item{child}
		}
	}
	if shape == "unrelated" || shape == "groups-unrelated" {
		groups[3] = []*Item{{ID: ID{Client: 3}, Content: NewContentDeleted(1), Deleted: true}}
	}
	encode := encodeStructStoreV1
	if version == 2 {
		encode = encodeStructStoreV2
	}
	return encode(groups, newDeleteSet(), nil, doc.store)
}

func TestUnit_PendingBudget_BoundsDiverseBytes(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, shape := range []string{"missing", "lengths", "unrelated", "cycle", "groups", "groups-unrelated"} {
			t.Run(fmt.Sprintf("V%d/%s", version, shape), func(t *testing.T) {
				for _, n := range []int{100, 20000} {
					update := pendingDiverseUpdate(version, n, shape)
					// Isolate Apply's bytes from fixture construction. This deliberately
					// checks bytes, not AllocsPerRun: a growing flat slice has few allocations.
					runtime.GC()
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					doc := New(WithMaxPendingItems(16))
					apply := ApplyUpdateV1
					if version == 2 {
						apply = ApplyUpdateV2
					}
					err := apply(doc, update, nil)
					runtime.ReadMemStats(&after)
					allocated := after.TotalAlloc - before.TotalAlloc
					require.ErrorIs(t, err, ErrInvalidUpdate)
					require.Zero(t, doc.PendingStats().Items)
					// V2 may copy its normal compressed columns and string pool. Everything
					// beyond that is small per-group/cap metadata, not per-struct tuples.
					require.LessOrEqual(t, allocated, uint64(2*len(update)+64*1024), "n=%d wire=%d allocated=%d", n, len(update), allocated)
					doc.Destroy()
				}
			})
		}
	}
}

// The sizing pass must predict the filled summary exactly, and every entry
// must decode back to the struct the scan saw.
func TestUnit_PendingSummary_RoundTrip(t *testing.T) {
	type entry struct {
		skip   bool
		length uint64
		deps   []ID
	}
	top := ^uint64(0)
	entries := []entry{
		{false, 1, []ID{{Client: 1, Clock: 0}}},
		{true, 1 << 40, nil},
		{false, 0, nil},
		{false, top >> 1, []ID{{Client: ClientID(top), Clock: top}, {Client: 127, Clock: 128}, {Client: 0, Clock: 1 << 35}}},
		{false, 3, []ID{{Client: 9, Clock: 2}, {Client: 300, Clock: 0}}},
	}
	sizing, fill := pendingCollector{}, pendingCollector{fill: true}
	for _, e := range entries {
		sizing.put(e.skip, e.length, e.deps)
		fill.put(e.skip, e.length, e.deps)
	}
	require.Len(t, fill.sum, sizing.size)
	at := uint32(0)
	for _, e := range entries {
		skip, length, deps, n, next := pendingEntry(fill.sum, at)
		require.Equal(t, e.skip, skip)
		require.Equal(t, e.length, length)
		require.Equal(t, len(e.deps), n)
		for i := range e.deps {
			require.Equal(t, e.deps[i], deps[i])
		}
		at = next
	}
	require.EqualValues(t, len(fill.sum), at)
}

// A later group for client 1 supersedes its blocked 100-struct group, so only
// client 3's missing parent stays pending.
func pendingOverlappingGroups(version int) []byte {
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	groups := [][]*Item{make([]*Item, 100), {{ID: ID{Client: 1}, Origin: &ID{Client: 2}, Parent: &text.abstractType, Content: NewContentString(strings.Repeat("x", 100))}}, {{ID: ID{Client: 3}, parentID: &ID{Client: 99}, Content: NewContentAny(1)}}, {{ID: ID{Client: 2}, Parent: &text.abstractType, Content: NewContentString("Z")}}}
	for i := range groups[0] {
		groups[0][i] = &Item{ID: ID{Client: 1, Clock: uint64(i)}, parentID: &ID{Client: 99, Clock: uint64(i)}, Content: NewContentAny(i)}
	}
	if version == 1 {
		enc := encoding.NewEncoder()
		enc.WriteVarUint(uint64(len(groups)))
		for _, items := range groups {
			enc.WriteVarUint(uint64(len(items)))
			enc.WriteVarUint(uint64(items[0].ID.Client))
			enc.WriteVarUint(items[0].ID.Clock)
			for _, item := range items {
				encodeItem(enc, item, 0, source.store)
			}
		}
		encodeDeleteSet(enc, newDeleteSet())
		return enc.Bytes()
	}
	enc := newV2Encoder()
	enc.restEnc.WriteVarUint(uint64(len(groups)))
	for _, items := range groups {
		enc.restEnc.WriteVarUint(uint64(len(items)))
		enc.writeClient(items[0].ID.Client)
		enc.restEnc.WriteVarUint(items[0].ID.Clock)
		for _, item := range items {
			encodeItemV2(enc, item, 0, source.store)
		}
	}
	encodeDeleteSetV2(enc, newDeleteSet())
	return enc.toBytes()
}

func TestUnit_PendingBudget_OverlappingGroups(t *testing.T) {
	for _, version := range []int{1, 2} {
		data := pendingOverlappingGroups(version)
		for _, cap := range []int{0, 1, 2} {
			budget := pendingBudget{initial: StateVector{}, update: data, remaining: cap, v2: version == 2}
			want := referencePendingBudgetCheck(budget, cap)
			got := budget.check(cap)
			require.Equal(t, want == nil, got == nil, "V%d cap=%d want=%v got=%v", version, cap, want, got)
		}
		doc := New(WithMaxPendingItems(1))
		defer doc.Destroy()
		apply := ApplyUpdateV1
		if version == 2 {
			apply = ApplyUpdateV2
		}
		require.NoError(t, apply(doc, data, nil))
		require.Equal(t, 1, doc.PendingStats().Items)
		require.Equal(t, "Z"+strings.Repeat("x", 100), doc.GetText("text").ToString())
	}
}

func TestUnit_PendingScanner_JSONValidation(t *testing.T) {
	values := []string{"{", "1e9999", "[1e9999]", "undefined", "null", `{"s":"1e9999","n":3}`}
	for _, version := range []int{1, 2} {
		tags := []byte{wireJSON, wireEmbed, wireFormat}
		if version == 2 {
			tags = []byte{wireJSON}
		}
		for _, tag := range tags {
			for _, value := range values {
				t.Run(fmt.Sprintf("V%d/tag=%d/%s", version, tag, value), func(t *testing.T) {
					var data []byte
					var wantErr error
					if version == 1 {
						enc := encoding.NewEncoder()
						if tag == wireJSON {
							enc.WriteVarUint(1)
						}
						if tag == wireFormat {
							enc.WriteVarString("bold")
						}
						enc.WriteVarBytes([]byte(value))
						data = enc.Bytes()
						_, wantErr = decodeContent(encoding.NewDecoder(data), nil, tag)
					} else {
						enc := newV2Encoder()
						enc.writeLen(1)
						enc.writeString(value)
						data = enc.toBytes()
						dec, err := newV2Decoder(data)
						require.NoError(t, err)
						_, wantErr = decodeContentV2(dec, nil, tag)
					}
					s := newPendingScanner(data, version == 2)
					s.content(tag)
					require.Equal(t, wantErr == nil, s.err == nil, "decoder=%v scanner=%v", wantErr, s.err)
				})
			}
		}
	}
}
