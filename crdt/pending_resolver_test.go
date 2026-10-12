package crdt

import (
	"encoding/hex"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func resolverTestItems(doc *Doc, n int, shape string) []*Item {
	text := doc.GetText("text")
	root := doc.GetMap("root")
	items := make([]*Item, n)
	for i := range items {
		id := ClientID(i + 1)
		item := &Item{ID: ID{Client: id}, Parent: &text.abstractType, Content: NewContentString(fmt.Sprintf("%03d|", i+1))}
		switch shape {
		case "left":
			if i+1 < n {
				item.Origin = &ID{Client: id + 1}
			}
		case "right":
			if i+1 < n {
				item.OriginRight = &ID{Client: id + 1}
			}
		case "tree":
			if i > 0 {
				item.Origin = &ID{Client: ClientID((i-1)/2 + 1)}
			}
		case "missing":
			item.Origin = &ID{Client: 9999}
		case "cycle":
			item.Origin = &ID{Client: ClientID((i+1)%n + 1)}
		case "parents":
			item.Parent = nil
			item.Content = NewContentType(&NewMapPrelim().abstractType)
			key := "child"
			item.ParentSub = &key
			if i+1 < n {
				item.parentID = &ID{Client: id + 1}
			} else {
				item.Parent = &root.abstractType
			}
		}
		items[i] = item
	}
	return items
}

func TestUnit_PendingResolver_MatchesFixedPoint(t *testing.T) {
	const n = 128
	for _, shape := range []string{"left", "right", "tree", "missing", "cycle", "parents"} {
		for seed := int64(0); seed < 12; seed++ {
			t.Run(fmt.Sprintf("%s/%d", shape, seed), func(t *testing.T) {
				a, b := New(WithClientID(100001)), New(WithClientID(100001))
				defer a.Destroy()
				defer b.Destroy()
				var errs [2]error
				for i, doc := range []*Doc{a, b} {
					pending := resolverTestItems(doc, n, shape)
					rand.New(rand.NewSource(seed)).Shuffle(n, func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
					doc.Transact(func(txn *Transaction) {
						if i == 0 {
							errs[i] = referenceWithinUpdatePending(txn, pending)
						} else {
							errs[i] = resolveWithinUpdatePending(txn, pending)
						}
					})
				}
				require.NoError(t, errs[0])
				require.NoError(t, errs[1])
				require.Equal(t, a.StateVector(), b.StateVector())
				require.Equal(t, a.PendingStats(), b.PendingStats())
				require.Equal(t, a.GetText("text").ToString(), b.GetText("text").ToString())
				require.Equal(t, EncodeStateAsUpdateV1(a, nil), EncodeStateAsUpdateV1(b, nil))
				require.Equal(t, EncodeStateAsUpdateV2(a, nil), EncodeStateAsUpdateV2(b, nil))
			})
		}
	}
}

func TestUnit_PendingResolver_ImmutableRanges(t *testing.T) {
	items := []*Item{
		{ID: ID{Client: 1, Clock: 1}, Content: NewContentDeleted(100), Deleted: true},
		{ID: ID{Client: 1, Clock: 2}, Content: NewContentString("x")},
		{ID: ID{Client: 2}, Content: NewContentString("abc")},
	}
	order := indexPendingProducers(items)
	// Model GC integration trimming into the range. Search keys/ends stay frozen.
	items[0].ID.Clock = 50
	items[0].Content = items[0].Content.Splice(49)
	require.Equal(t, 0, findPendingProducer(order, ID{Client: 1, Clock: 1}))
	require.Equal(t, 2, findPendingProducer(order, ID{Client: 2, Clock: 2}))
	require.Equal(t, -1, findPendingProducer(order, ID{Client: 2, Clock: 3}))
	require.Equal(t, -1, findPendingProducer(order, ID{Client: 3}))
}

func TestUnit_PendingResolver_ReverseCheckpoint(t *testing.T) {
	const n = 128
	for _, version := range []int{1, 2} {
		source := New()
		text := source.GetText("text")
		groups := make(map[ClientID][]*Item, n)
		var want strings.Builder
		for i := 1; i <= n; i++ {
			item := &Item{ID: ID{Client: ClientID(i)}, Parent: &text.abstractType, Content: NewContentString(fmt.Sprintf("%03d|", i))}
			if version == 1 && i < n {
				item.Origin = &ID{Client: ClientID(i + 1), Clock: 3}
			}
			if version == 2 && i > 1 {
				item.Origin = &ID{Client: ClientID(i - 1), Clock: 3}
			}
			groups[item.ID.Client] = []*Item{item}
			value := i
			if version == 1 {
				value = n - i + 1
			}
			fmt.Fprintf(&want, "%03d|", value)
		}
		encode, apply := encodeStructStoreV1, ApplyUpdateV1
		if version == 2 {
			encode, apply = encodeStructStoreV2, ApplyUpdateV2
		}
		data := encode(groups, newDeleteSet(), nil, source.store)
		source.Destroy()
		target := New(WithMaxPendingItems(16))
		require.NoError(t, apply(target, data, nil))
		require.Zero(t, target.PendingStats().Items)
		require.Len(t, target.StateVector(), n)
		require.Equal(t, want.String(), target.GetText("text").ToString())
		target.Destroy()
	}
}

func TestUnit_PendingResolver_PersistentLimit(t *testing.T) {
	for _, resolve := range []func(*Transaction, []*Item) error{referenceWithinUpdatePending, resolveWithinUpdatePending} {
		doc := New(WithMaxPendingItems(16))
		text := doc.GetText("text")
		previous := []*Item{
			{ID: ID{Client: 900001}, Parent: &text.abstractType, Origin: &ID{Client: 9999}, Content: NewContentString("a")},
			{ID: ID{Client: 900002}, Parent: &text.abstractType, Origin: &ID{Client: 9999}, Content: NewContentString("b")},
		}
		var err error
		doc.Transact(func(txn *Transaction) { err = resolve(txn, previous) })
		require.NoError(t, err)
		pending := resolverTestItems(doc, 128, "missing")
		doc.Transact(func(txn *Transaction) { err = resolve(txn, pending) })
		require.ErrorIs(t, err, ErrInvalidUpdate)
		require.Equal(t, 16, doc.PendingStats().Items)
		require.Same(t, previous[0], doc.store.pending.items[0])
		require.Same(t, previous[1], doc.store.pending.items[1])
		require.Empty(t, doc.StateVector())
		doc.Destroy()
	}
}

// referenceWithinUpdatePending is the fixed-point resolver, kept as a reference. It is
// independent of the scheduler, immutable index and shared retry-pass helper.
func referenceWithinUpdatePending(txn *Transaction, pending []*Item) error {
	for len(pending) > 0 {
		var remaining []*Item
		for _, item := range pending {
			if item.Origin != nil {
				if oi := txn.doc.store.Find(*item.Origin); oi != nil {
					item.Parent = oi.Parent
					if item.ParentSub == nil {
						item.ParentSub = oi.ParentSub
					}
				}
			}
			if item.Parent == nil && item.OriginRight != nil {
				if ori := txn.doc.store.Find(*item.OriginRight); ori != nil {
					item.Parent = ori.Parent
					if item.ParentSub == nil {
						item.ParentSub = ori.ParentSub
					}
				}
			}
			if item.Parent == nil && item.parentID != nil {
				if pi := txn.doc.store.Find(*item.parentID); pi != nil {
					if ct, ok := pi.Content.(*ContentType); ok {
						item.Parent = ct.Type
					}
				}
			}
			if item.Parent != nil {
				if _, _, isFuture := itemFutureDep(item, txn.doc.store); isFuture {
					remaining = append(remaining, item)
					continue
				}
				if item.Origin != nil {
					item.Left = txn.doc.store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
				}
				item.integrate(txn, 0)
			} else {
				remaining = append(remaining, item)
			}
		}
		if len(remaining) == len(pending) {
			for _, item := range remaining {
				if client, parkedAt, isFuture := itemFutureDep(item, txn.doc.store); isFuture {
					if txn.doc.store.pending != nil && len(txn.doc.store.pending.items) >= txn.doc.maxPendingItemsLimit() {
						return wrapUpdateErr(ErrInvalidUpdate)
					}
					if txn.doc.store.pending == nil {
						txn.doc.store.pending = &pendingUpdate{
							missing: make(StateVector),
						}
					}
					txn.doc.store.pending.items = append(txn.doc.store.pending.items, item)
					mergePendingMissing(txn.doc.store.pending.missing, client, parkedAt)
				} else {
					txn.doc.store.Append(item)
				}
			}
			break
		}
		pending = remaining
	}
	return nil
}

// Bound cumulative allocation through both public update entry points.
func TestUnit_PendingResolver_BoundsReverseChainBytes(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			data := pendingReverseChain(version, 2000)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			doc := New(WithMaxPendingItems(16))
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			err := apply(doc, data, nil)
			runtime.ReadMemStats(&after)
			require.NoError(t, err)
			require.Zero(t, doc.PendingStats().Items)
			require.Equal(t, 2000, doc.GetText("text").Len())
			require.LessOrEqual(t, after.TotalAlloc-before.TotalAlloc, uint64(16*1024*1024), "reverse-chain allocated bytes")
			doc.Destroy()
		})
	}
}

// Each client has a string and an own-origin embed, preserving two wire structs.
func resolverMultiStructUpdate(version, n int) []byte {
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	groups := make(map[ClientID][]*Item, n)
	for i := 1; i <= n; i++ {
		c := ClientID(i)
		var origin *ID
		if version == 1 && i < n {
			origin = &ID{Client: c + 1}
		}
		if version == 2 && i > 1 {
			origin = &ID{Client: c - 1}
		}
		groups[c] = []*Item{
			{ID: ID{Client: c}, Parent: &text.abstractType, Origin: origin, Content: NewContentString("x")},
			{ID: ID{Client: c, Clock: 1}, Parent: &text.abstractType, Origin: &ID{Client: c}, Content: NewContentEmbed("e")},
		}
	}
	if version == 2 {
		return encodeStructStoreV2(groups, newDeleteSet(), nil, source.store)
	}
	return encodeStructStoreV1(groups, newDeleteSet(), nil, source.store)
}

func TestUnit_PendingResolver_MultiStructCheckpoint(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			const n = 2000
			data := resolverMultiStructUpdate(version, n)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			doc := New(WithMaxPendingItems(16))
			defer doc.Destroy()
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			err := apply(doc, data, nil)
			runtime.ReadMemStats(&after)
			require.NoError(t, err)
			require.Equal(t, 2*n, doc.GetText("text").Len())
			require.Len(t, doc.StateVector(), n)
			require.Zero(t, doc.PendingStats().Items)
			for _, clock := range doc.StateVector() {
				require.Equal(t, uint64(2), clock)
			}
			require.LessOrEqual(t, after.TotalAlloc-before.TotalAlloc, uint64(n*4096), "ready tails must not create quadratic insertion allocations")
		})
	}
}

// Eager client tails must preserve content, clocks, pending items and events
// when producers are split by origins inside multi-character strings.
func TestUnit_PendingResolver_ContiguousTailsMatchReference(t *testing.T) {
	const n = 64
	for _, shape := range []string{"left", "right", "tree"} {
		for seed := int64(0); seed < 8; seed++ {
			t.Run(fmt.Sprintf("%s/%d", shape, seed), func(t *testing.T) {
				docs := []*Doc{New(WithClientID(100001)), New(WithClientID(100001))}
				var events [2][]string
				for which, doc := range docs {
					text := doc.GetText("text")
					text.Observe(func(event YTextEvent) { events[which] = append(events[which], fmt.Sprintf("%#v", event.Delta)) })
					pending := resolverTestItems(doc, n, shape)
					for _, head := range append([]*Item(nil), pending...) {
						length := uint64(head.Content.Len())
						pending = append(pending, &Item{ID: ID{Client: head.ID.Client, Clock: length}, Origin: &ID{Client: head.ID.Client, Clock: length - 1}, Content: NewContentEmbed(int(head.ID.Client))})
					}
					rand.New(rand.NewSource(seed)).Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
					var err error
					doc.Transact(func(txn *Transaction) {
						if which == 0 {
							err = referenceWithinUpdatePending(txn, pending)
						} else {
							err = resolveWithinUpdatePending(txn, pending)
						}
					})
					require.NoError(t, err)
				}
				require.Equal(t, docs[0].StateVector(), docs[1].StateVector())
				require.Equal(t, docs[0].PendingStats(), docs[1].PendingStats())
				require.Equal(t, docs[0].GetText("text").ToString(), docs[1].GetText("text").ToString())
				require.Equal(t, events[0], events[1])
				require.Equal(t, EncodeStateAsUpdateV1(docs[0], nil), EncodeStateAsUpdateV1(docs[1], nil))
				require.Equal(t, EncodeStateAsUpdateV2(docs[0], nil), EncodeStateAsUpdateV2(docs[1], nil))
				for _, doc := range docs {
					doc.Destroy()
				}
			})
		}
	}
}

// Yjs 13.6.30, client 1: capture a prefix containing a–d, then append e–f.
// Encode the suffix against a state vector at clock 2. Delivering that suffix
// before the prefix must trim the already received c–d when the queue drains.
func TestUnit_PendingResolver_PartialOverlapRetry(t *testing.T) {
	for _, tc := range []struct {
		kind           string
		version        int
		prefix, suffix string
	}{
		{"text", 1, "0101010004010474657874046162636400", "01010102840101046364656600"},
		{"text", 2, "00000101000001040b08746578746162636444000101000001010000", "000002410001020001840604636465660400000001010200"},
		{"array", 1, "0101010008010561727261790477016177016277016377016400", "010101028801010477016377016477016577016600"},
		{"array", 2, "00000101000001080705617272617905010100010401010077016177016277016377016400", "0000024100010200018801000000010401010277016377016477016577016600"},
	} {
		t.Run(fmt.Sprintf("%s/V%d", tc.kind, tc.version), func(t *testing.T) {
			prefix, err := hex.DecodeString(tc.prefix)
			require.NoError(t, err)
			suffix, err := hex.DecodeString(tc.suffix)
			require.NoError(t, err)
			apply := ApplyUpdateV1
			if tc.version == 2 {
				apply = ApplyUpdateV2
			}
			doc := New(WithClientID(100))
			defer doc.Destroy()
			require.NoError(t, apply(doc, suffix, nil))
			require.Zero(t, doc.StateVector().Clock(1))
			require.Equal(t, 1, doc.PendingStats().Items)

			check := func() {
				if tc.kind == "text" {
					require.Equal(t, "abcdef", doc.GetText("text").ToString())
				} else {
					require.Equal(t, []any{"a", "b", "c", "d", "e", "f"}, doc.GetArray("array").ToSlice())
				}
				require.Equal(t, uint64(6), doc.StateVector().Clock(1))
				require.Zero(t, doc.PendingStats().Items)
				var end uint64
				for _, item := range doc.store.clients[1] {
					require.Equal(t, end, item.ID.Clock, "stored ranges must stay contiguous without overlaps")
					end += uint64(item.Content.Len())
				}
				require.Equal(t, uint64(6), end)
			}
			require.NoError(t, apply(doc, prefix, nil))
			check()
			// The same suffix is now fully covered and must be a no-op.
			require.NoError(t, apply(doc, suffix, nil))
			check()
		})
	}
}

// Yjs 13.6.30: client 1 creates a nested text containing a–d (clocks 0..5).
// A peer deletes and GCs that container while client 1 appends e–f. The suffix
// at clock 3 arrives before the GC prefix; its parent is gone when it retries.
// The later update writes "ok" to a different root at clocks 7..9.
func TestUnit_PendingResolver_OrphanPartialOverlapSurvivesRestore(t *testing.T) {
	for _, tc := range []struct {
		version               int
		prefix, suffix, later string
	}{
		{1, "01020100210104726f6f74066e65737465640100040101010005", "01010103840102046364656600", "010101070401056c61746572026f6b00"},
		{2, "000001010000032100000d0a726f6f746e657374656404060101000201040102000101010004", "000002410001040001840604636465660400000001010300", "00000101000001040a076c617465726f6b05020101000001010700"},
	} {
		t.Run(fmt.Sprintf("V%d", tc.version), func(t *testing.T) {
			prefix, err := hex.DecodeString(tc.prefix)
			require.NoError(t, err)
			suffix, err := hex.DecodeString(tc.suffix)
			require.NoError(t, err)
			later, err := hex.DecodeString(tc.later)
			require.NoError(t, err)
			apply := ApplyUpdateV1
			if tc.version == 2 {
				apply = ApplyUpdateV2
			}
			doc := New(WithClientID(100))
			defer doc.Destroy()
			require.NoError(t, apply(doc, suffix, nil))
			require.Equal(t, 1, doc.PendingStats().Items)
			require.NoError(t, apply(doc, prefix, nil))
			require.Empty(t, doc.GetMap("root").Entries())
			require.Zero(t, doc.PendingStats().Items)
			require.Equal(t, uint64(7), doc.StateVector().Clock(1))
			var end uint64
			for _, item := range doc.store.clients[1] {
				require.Equal(t, end, item.ID.Clock, "orphan ranges must stay contiguous without overlaps")
				end += uint64(item.Content.Len())
			}
			require.Equal(t, uint64(7), end)
			for _, version := range []int{1, 2} {
				t.Run(fmt.Sprintf("restore/V%d", version), func(t *testing.T) {
					encode, restore := EncodeStateAsUpdateV1, ApplyUpdateV1
					if version == 2 {
						encode, restore = EncodeStateAsUpdateV2, ApplyUpdateV2
					}
					restored := New(WithClientID(101))
					defer restored.Destroy()
					require.NoError(t, restore(restored, encode(doc, nil), nil))
					require.Equal(t, doc.StateVector(), restored.StateVector())
					require.NoError(t, apply(restored, later, nil))
					require.Equal(t, "ok", restored.GetText("later").ToString())
					require.Equal(t, uint64(9), restored.StateVector().Clock(1))
					require.Zero(t, restored.PendingStats().Items)
				})
			}
		})
	}
}

// Overlapping copies must preserve content, clocks and events while selecting
// the long producer containing a dependency inside its wire range.
func TestUnit_PendingResolver_OverlappingRangesMatchFixedPoint(t *testing.T) {
	const n = 128
	for seed := int64(0); seed < 8; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			docs := []*Doc{New(WithClientID(100001)), New(WithClientID(100001))}
			var events [2][]string
			for which, doc := range docs {
				text := doc.GetText("text")
				text.Observe(func(event YTextEvent) { events[which] = append(events[which], fmt.Sprintf("%#v", event.Delta)) })
				pending := make([]*Item, 0, 3*n)
				for i := 0; i < n; i++ {
					client := ClientID(i + 1)
					value := fmt.Sprintf("%03d|", i+1)
					var parent *abstractType
					var origin *ID
					if i+1 < n {
						origin = &ID{Client: client + 1, Clock: 3}
					} else {
						parent = &text.abstractType
					}
					pending = append(pending,
						&Item{ID: ID{Client: client}, Parent: parent, Origin: origin, Content: NewContentString(value)},
						&Item{ID: ID{Client: client, Clock: 1}, Origin: &ID{Client: client}, Content: NewContentString(value[1:2])},
						&Item{ID: ID{Client: client, Clock: 2}, Origin: &ID{Client: client, Clock: 1}, Content: NewContentString(value[2:])})
				}
				rand.New(rand.NewSource(seed)).Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
				var err error
				doc.Transact(func(txn *Transaction) {
					if which == 0 {
						err = resolveWithinUpdatePending(txn, pending)
						return
					}
					for len(pending) > 0 {
						remaining := retryWithinUpdatePending(txn, pending)
						if len(remaining) == len(pending) {
							err = parkWithinUpdatePending(txn, remaining)
							return
						}
						pending = remaining
					}
				})
				require.NoError(t, err)
			}
			var want strings.Builder
			for i := n; i > 0; i-- {
				fmt.Fprintf(&want, "%03d|", i)
			}
			require.Equal(t, want.String(), docs[0].GetText("text").ToString())
			require.Equal(t, docs[0].GetText("text").ToString(), docs[1].GetText("text").ToString())
			require.Equal(t, docs[0].StateVector(), docs[1].StateVector())
			require.Zero(t, docs[0].PendingStats().Items)
			require.Equal(t, docs[0].PendingStats(), docs[1].PendingStats())
			require.Equal(t, events[0], events[1])
			require.Equal(t, EncodeStateAsUpdateV1(docs[0], nil), EncodeStateAsUpdateV1(docs[1], nil))
			require.Equal(t, EncodeStateAsUpdateV2(docs[0], nil), EncodeStateAsUpdateV2(docs[1], nil))
			for _, doc := range docs {
				doc.Destroy()
			}
		})
	}
}
