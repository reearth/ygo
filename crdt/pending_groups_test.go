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

func TestUnit_PendingScanner_CheckpointIsIndependent(t *testing.T) {
	source := New()
	defer source.Destroy()
	key := "key"
	parent := ID{Client: 99}
	contents := []Content{NewContentFormat("bold", true), NewContentFormat("bold", false), NewContentString("А🐷z"), NewContentJSON(nil, "tail", 7), NewContentAny([]any{true, "x"})}
	items := make([]*Item, len(contents))
	clock := uint64(0)
	for i, content := range contents {
		items[i] = &Item{ID: ID{Client: 1, Clock: clock}, parentID: &parent, ParentSub: &key, Content: content}
		clock += uint64(content.Len())
	}
	for _, version := range []int{1, 2} {
		encode := encodeStructStoreV1
		if version == 2 {
			encode = encodeStructStoreV2
		}
		data := encode(map[ClientID][]*Item{1: items}, newDeleteSet(), nil, source.store)
		s := newPendingScanner(data, version == 2)
		require.EqualValues(t, 1, s.uint())
		require.EqualValues(t, len(items), s.uint())
		s.client()
		s.uint()
		s.item()
		copy := s.checkpoint()
		// Consume the original first, including active RLE runs. Its advance must
		// not consume the copy's rest, columns, string position or key clocks.
		for range contents[1:] {
			s.item()
		}
		require.Zero(t, s.uint())
		require.NoError(t, s.err)
		for _, content := range contents[1:] {
			length, _, _, _ := copy.scanner.item()
			require.EqualValues(t, content.Len(), length)
			require.NoError(t, copy.scanner.err)
		}
		require.Zero(t, copy.scanner.uint())
		require.NoError(t, copy.scanner.err)
	}
}

// A later overlapping group can cover a blocked head independently of its
// missing external dependency. Waking coverage must cancel the other watch.
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
