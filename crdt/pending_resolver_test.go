package crdt

import (
	"cmp"
	"encoding/hex"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
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
		data := encode(groups, newDeleteSet(), nil)
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
						return ErrInvalidUpdate
					}
					if txn.doc.store.pending == nil {
						txn.doc.store.pending = &pendingUpdate{
							missing: make(StateVector),
						}
					}
					txn.doc.store.pending.items = append(txn.doc.store.pending.items, item)
					txn.doc.store.pending.addCover([]*Item{item}, len(txn.doc.store.pending.items)-1)
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

// retryPassesWithinUpdatePending retries the whole queue through tryIntegrate
// until a pass integrates nothing. Unlike referenceWithinUpdatePending it
// trims overlapping ranges, so it is the reference for overlapping copies.
func retryPassesWithinUpdatePending(txn *Transaction, pending []*Item) error {
	for len(pending) > 0 {
		var remaining []*Item
		for _, item := range pending {
			if ok, _, _ := tryIntegrate(txn, item); !ok {
				remaining = append(remaining, item)
			}
		}
		if len(remaining) == len(pending) {
			return parkWithinUpdatePending(txn, remaining)
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
		return encodeStructStoreV2(groups, newDeleteSet(), nil)
	}
	return encodeStructStoreV1(groups, newDeleteSet(), nil)
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
						length := head.Content.Len()
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
					end += item.Content.Len()
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
				end += item.Content.Len()
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
					} else {
						err = retryPassesWithinUpdatePending(txn, pending)
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

// Two copies of a range start at the same clock; a consumer of the longer
// copy's tail must be resolved through it, whichever copy sorts first.
func TestUnit_PendingResolver_SameStartCopies(t *testing.T) {
	for _, longFirst := range []bool{false, true} {
		doc := New(WithClientID(100001))
		text := doc.GetText("t")
		short := &Item{ID: ID{Client: 1}, Parent: &text.abstractType, Content: NewContentString("a")}
		long := &Item{ID: ID{Client: 1}, Parent: &text.abstractType, Content: NewContentString("abc")}
		consumer := &Item{ID: ID{Client: 2}, Origin: &ID{Client: 1, Clock: 2}, Content: NewContentString("x")}
		pending := []*Item{consumer, short, long}
		if longFirst {
			pending = []*Item{consumer, long, short}
		}
		var err error
		doc.Transact(func(txn *Transaction) {
			err = resolveWithinUpdatePending(txn, pending)
		})
		require.NoError(t, err)
		require.Zero(t, doc.PendingStats().Items)
		require.Equal(t, "abcx", text.ToString())
		doc.Destroy()
	}
}

// An item whose parent is already known still waits for a future origin.
func TestUnit_TryIntegrate_FutureOriginWithKnownParent(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Destroy()
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "abc", nil) })
	item := &Item{
		ID:      ID{Client: 2},
		Parent:  &text.abstractType,
		Origin:  &ID{Client: 1, Clock: 5},
		Content: NewContentString("x"),
	}
	doc.Transact(func(txn *Transaction) {
		ok, waitsOn, err := tryIntegrate(txn, item)
		require.False(t, ok)
		require.Equal(t, ClientID(1), waitsOn)
		require.NoError(t, err)
	})
	require.Equal(t, "abc", text.ToString())
	require.Equal(t, uint64(0), doc.StateVector().Clock(2))
}

// A struct that cannot integrate yet names the client whose next clock it
// waits on: its own past a gap, then its origin, its right origin, and its
// parent last.
func TestUnit_TryIntegrate_NamesTheClientItWaitsOn(t *testing.T) {
	key := "k"
	for _, tc := range []struct {
		name    string
		item    func(text *YText) *Item
		waitsOn ClientID
	}{
		{"own gap", func(text *YText) *Item {
			return &Item{ID: ID{Client: 5, Clock: 3}, Parent: &text.abstractType, Content: NewContentString("x")}
		}, 5},
		{"origin of a client with no structs", func(text *YText) *Item {
			return &Item{ID: ID{Client: 5}, Parent: &text.abstractType, Origin: &ID{Client: 7}, Content: NewContentString("x")}
		}, 7},
		{"right origin past the known clock", func(text *YText) *Item {
			return &Item{ID: ID{Client: 5}, Parent: &text.abstractType, Origin: &ID{Client: 1, Clock: 1}, OriginRight: &ID{Client: 1, Clock: 9}, Content: NewContentString("x")}
		}, 1},
		{"right origin with no parent", func(*YText) *Item {
			return &Item{ID: ID{Client: 5}, OriginRight: &ID{Client: 8}, Content: NewContentString("x")}
		}, 8},
		{"right origin after an origin that gives the parent", func(*YText) *Item {
			return &Item{ID: ID{Client: 5}, Origin: &ID{Client: 1}, OriginRight: &ID{Client: 8}, Content: NewContentString("x")}
		}, 8},
		{"parent", func(*YText) *Item {
			return &Item{ID: ID{Client: 5}, parentID: &ID{Client: 9}, ParentSub: &key, Content: NewContentAny([]any{1})}
		}, 9},
		{"origin before parent", func(*YText) *Item {
			return &Item{ID: ID{Client: 5}, Origin: &ID{Client: 7}, parentID: &ID{Client: 9}, Content: NewContentString("x")}
		}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New(WithClientID(1))
			defer doc.Destroy()
			text := doc.GetText("t")
			doc.Transact(func(txn *Transaction) { text.Insert(txn, 0, "abc", nil) })
			item := tc.item(text)
			doc.Transact(func(txn *Transaction) {
				ok, waitsOn, err := tryIntegrate(txn, item)
				require.False(t, ok)
				require.NoError(t, err)
				require.Equal(t, tc.waitsOn, waitsOn)
			})
			require.Equal(t, "abc", text.ToString())
		})
	}
}

// A decoded update's structs, one run per client in client order, ascending as
// ygo writes them or descending as Yjs does, integrate without a group per
// client when no struct waits on a client that still has structs to take.
func TestUnit_PendingResolver_ClientRunsBuildNoGroups(t *testing.T) {
	const n = 1000
	for _, shape := range []string{"missing", "left"} {
		for _, descending := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/descending=%t", shape, descending), func(t *testing.T) {
				doc := New(WithClientID(100001))
				defer doc.Destroy()
				pending := resolverTestItems(doc, n, shape)
				if descending {
					slices.Reverse(pending)
				}
				done := make([]bool, n)
				var allocs float64
				doc.Transact(func(txn *Transaction) {
					// The first run integrates "left"; the later ones find it done.
					allocs = testing.AllocsPerRun(5, func() {
						clear(done)
						require.NoError(t, integratePending(txn, pending, done, nil))
					})
				})
				require.Zero(t, allocs)
			})
		}
	}
}

// Structs in one clock-ordered run per client, in either client order,
// integrate in the order, and leave out the structs, that the same structs in
// any other order do.
func TestUnit_PendingResolver_ClientRunsIntegrateAsGroupsDo(t *testing.T) {
	for seed := int64(0); seed < 300; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			var got [3]string
			for k := range got {
				rng := rand.New(rand.NewSource(seed))
				doc := New(WithClientID(100001))
				text := doc.GetText("t")
				pending := clientRunItems(rng, text)
				switch k {
				case 1: // descending runs, each still in clock order
					slices.SortStableFunc(pending, func(a, b *Item) int { return cmp.Compare(b.ID.Client, a.ID.Client) })
				case 2: // no order the runs can be read in
					for readsAsClientRuns(pending) {
						rng.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
					}
				}
				var integrated, left []string
				doc.Transact(func(txn *Transaction) {
					done := make([]bool, len(pending))
					require.NoError(t, integratePending(txn, pending, done, nil))
					for _, it := range txn.newItems {
						integrated = append(integrated, fmt.Sprint(it.ID))
					}
					for i, it := range pending {
						if !done[i] {
							left = append(left, fmt.Sprint(it.ID))
						}
					}
				})
				slices.Sort(left)
				got[k] = fmt.Sprintf("integrated %v, left %v, text %q", integrated, left, text.ToString())
				doc.Destroy()
			}
			require.Equal(t, got[2], got[0], "ascending runs")
			require.Equal(t, got[2], got[1], "descending runs")
		})
	}
}

// clientRunItems is one run of one to three text structs for each of three to
// ten clients, in ascending client order, whose origins name an earlier struct
// of their own client, a struct of another client, a clock past another
// client's run, or a client absent from the update.
func clientRunItems(rng *rand.Rand, text *YText) []*Item {
	clients := rng.Perm(30)[:3+rng.Intn(8)]
	slices.Sort(clients)
	lengths := make(map[ClientID]int, len(clients))
	for _, c := range clients {
		lengths[ClientID(c+1)] = 1 + rng.Intn(3)
	}
	var pending []*Item
	for _, c := range clients {
		client := ClientID(c + 1)
		for clock := range lengths[client] {
			item := &Item{ID: ID{Client: client, Clock: uint64(clock)}, Parent: &text.abstractType, Content: NewContentString("x")}
			other := ClientID(clients[rng.Intn(len(clients))] + 1)
			switch r := rng.Intn(10); {
			case clock > 0 && r < 4:
				item.Origin = &ID{Client: client, Clock: uint64(clock - 1)}
			case r < 7 && other != client:
				item.Origin = &ID{Client: other, Clock: uint64(rng.Intn(lengths[other] + 1))}
			case r < 8:
				item.Origin = &ID{Client: 40}
			case clock > 0:
				item.Origin = &ID{Client: client, Clock: uint64(clock - 1)}
			}
			pending = append(pending, item)
		}
	}
	return pending
}

// readsAsClientRuns reports whether pending holds one clock-ordered run per
// client, the runs in ascending or descending client order.
func readsAsClientRuns(pending []*Item) bool {
	up, down := true, true
	for i := 1; i < len(pending); i++ {
		a, b := pending[i-1].ID, pending[i].ID
		switch {
		case a.Client == b.Client:
			if b.Clock <= a.Clock {
				return false
			}
		case a.Client < b.Client:
			down = false
		default:
			up = false
		}
	}
	return up || down
}

// A client run stays one run when an earlier overlapping item is trimmed
// before another client makes the resolver fall back to groups.
func TestUnit_PendingResolver_RunFallbackKeepsUnrelatedParkedItem(t *testing.T) {
	updates := []string{
		"010109018407000961626364656667686900",
		"01010903000100",
		"0101090a840500017a00",
		"01010500840700017100",
		"01010300841400017200",
		"020109000401017405313233343501070004010174014100",
	}
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("V%d", version), func(t *testing.T) {
			doc := New(WithClientID(100), WithGC(false))
			defer doc.Destroy()
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			for i, text := range updates {
				update, err := hex.DecodeString(text)
				require.NoError(t, err)
				if version == 2 {
					update, err = UpdateV1ToV2(update)
					require.NoError(t, err)
				}
				err = apply(doc, update, nil)
				require.NoErrorf(t, err, "update %d", i)
			}
			require.Equal(t, PendingStats{Items: 1, MissingFor: []ClientID{20}}, doc.PendingStats())
		})
	}
}
