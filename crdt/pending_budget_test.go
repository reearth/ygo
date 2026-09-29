package crdt

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Use the real codecs with many separate keyed structs: missing-parent updates
// must not allocate objects for the entire tail merely to discover it is missing.
func pendingBudgetUpdate(version, n int, dependency string) (update, parentUpdate []byte) {
	doc := New()
	defer doc.Destroy()
	childClient, parentClient := ClientID(1), ClientID(2)
	if version == 2 {
		childClient, parentClient = 2, 1
	}
	root := doc.GetMap("root")
	key := "child"
	container := &Item{ID: ID{Client: parentClient}, Parent: &root.abstractType, ParentSub: &key, Content: NewContentType(&NewMapPrelim().abstractType)}
	encode := encodeStructStoreV1
	if version == 2 {
		encode = encodeStructStoreV2
	}
	parentUpdate = encode(map[ClientID][]*Item{parentClient: {container}}, newDeleteSet(), nil, doc.store)
	items := make([]*Item, n)
	for i := range items {
		name := fmt.Sprint(i)
		clock := uint64(i)
		if dependency == "gap" {
			clock++
		}
		items[i] = &Item{ID: ID{Client: childClient, Clock: clock}, parentID: &container.ID, ParentSub: &name, Content: NewContentAny(i)}
	}
	groups := map[ClientID][]*Item{childClient: items}
	switch dependency {
	case "complete", "gap":
		groups[parentClient] = []*Item{container}
	case "unrelated":
		groups[3] = []*Item{{ID: ID{Client: 3}, Content: NewContentDeleted(1), Deleted: true}}
	case "chain", "cycle":
		container.Parent = nil
		container.parentID = &ID{Client: 3}
		if dependency == "cycle" {
			container.parentID = &items[0].ID
		}
		groups[parentClient] = []*Item{container}
	}
	return encode(groups, newDeleteSet(), nil, doc.store), parentUpdate
}

func TestUnit_PendingBudget_BoundsIncompleteAllocations(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, dependency := range []string{"missing", "unrelated", "chain", "cycle", "gap"} {
			t.Run(fmt.Sprintf("v%d/%s", version, dependency), func(t *testing.T) {
				apply := ApplyUpdateV1
				if version == 2 {
					apply = ApplyUpdateV2
				}
				counts := make([]float64, 0, 2)
				for _, n := range []int{100, 20_000} {
					update, _ := pendingBudgetUpdate(version, n, dependency)
					var applyErr error
					var pending int
					count := testing.AllocsPerRun(1, func() {
						doc := New(WithMaxPendingItems(16))
						applyErr = apply(doc, update, nil)
						pending = doc.PendingStats().Items
						doc.Destroy()
					})
					require.ErrorIs(t, applyErr, ErrInvalidUpdate)
					require.LessOrEqual(t, pending, 16)
					counts = append(counts, count)
				}
				// A 200x larger blocked tail must not materialize 200x more
				// objects. Leave ample slack for codec and runtime bookkeeping.
				require.Less(t, counts[1], counts[0]*2+256, "allocations small/large: %v", counts)
			})
		}
	}
}

func TestUnit_PendingBudget_CompleteAndRetry(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			complete, _ := pendingBudgetUpdate(version, 100, "complete")
			doc := New(WithMaxPendingItems(3))
			defer doc.Destroy()
			require.NoError(t, apply(doc, complete, nil))
			require.Zero(t, doc.PendingStats().Items)
			value, exists := doc.GetMap("root").Get("child")
			require.True(t, exists)
			require.Len(t, value.(*YMap).Entries(), 100)

			missing, parent := pendingBudgetUpdate(version, 3, "missing")
			retry := New(WithMaxPendingItems(3))
			defer retry.Destroy()
			require.NoError(t, apply(retry, missing, nil))
			require.Equal(t, 3, retry.PendingStats().Items)
			// The budget includes pending items from earlier messages.
			require.ErrorIs(t, apply(retry, missing, nil), ErrInvalidUpdate)
			require.Equal(t, 3, retry.PendingStats().Items)
			require.NoError(t, apply(retry, parent, nil))
			require.Zero(t, retry.PendingStats().Items)
			value, exists = retry.GetMap("root").Get("child")
			require.True(t, exists)
			require.Len(t, value.(*YMap).Entries(), 3)
		})
	}
}

func TestUnit_PendingScanner_ContentCursor(t *testing.T) {
	doc := New()
	defer doc.Destroy()
	subdoc := New()
	defer subdoc.Destroy()
	contents := []Content{
		NewContentDeleted(3), NewContentJSON("a", nil, 7), NewContentBinary([]byte{1, 2}),
		NewContentString("А🐷z"), NewContentEmbed(map[string]any{"n": []any{1, true}}),
		NewContentFormat("bold", true), NewContentFormat("bold", false),
		NewContentType(&NewMapPrelim().abstractType), NewContentType(&NewYXmlElement("div").abstractType),
		NewContentAny([]any{1, "a"}, map[string]any{"key": 2}), NewContentDoc(subdoc),
		NewContentMove(&ID{Client: 9}, 2),
	}
	parent := ID{Client: 9}
	items := make([]*Item, 0, len(contents)+1)
	var clock uint64
	for i, content := range contents {
		name := fmt.Sprint(i)
		items = append(items, &Item{ID: ID{Client: 1, Clock: clock}, parentID: &parent, ParentSub: &name, Content: content})
		clock += uint64(content.Len())
	}
	// An internal gap exercises skip structs as well as every content tag.
	items = append(items, &Item{ID: ID{Client: 1, Clock: clock + 4}, Content: NewContentDeleted(2), Deleted: true})
	for _, version := range []int{1, 2} {
		encode := encodeStructStoreV1
		if version == 2 {
			encode = encodeStructStoreV2
		}
		data := encode(map[ClientID][]*Item{1: items}, newDeleteSet(), nil, doc.store)
		s := newPendingScanner(data, version == 2)
		require.EqualValues(t, 1, s.uint())
		require.EqualValues(t, len(contents)+2, s.uint())
		require.EqualValues(t, 1, s.client())
		require.Zero(t, s.uint())
		for _, content := range contents {
			length, skip, deps, count := s.item()
			require.NoError(t, s.err)
			require.EqualValues(t, content.Len(), length)
			require.False(t, skip)
			require.Equal(t, 1, count)
			require.Equal(t, parent, deps[0])
		}
		length, skip, _, _ := s.item()
		require.True(t, skip)
		require.EqualValues(t, 4, length)
		length, skip, _, _ = s.item()
		require.False(t, skip)
		require.EqualValues(t, 2, length)
		require.Zero(t, s.uint(), "delete set remains aligned")
		require.Zero(t, s.rest.Remaining())
		require.NoError(t, s.err)
	}
}
