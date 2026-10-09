package crdt

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
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
					require.Zero(t, pending, "preflight rejection must not park new items")
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

func TestUnit_PendingScanner_ContentJSONV1MatchesDecoder(t *testing.T) {
	values := [][]any{
		{}, {nil, "tail"}, {"a", 7, map[string]any{"quoted": "1e9999\\\"", "number": 2}},
	}
	for n := 116; n <= 127; n++ {
		values = append(values, []any{strings.Repeat("a", n-2), "tail"})
	}
	for i, vals := range values {
		enc := encoding.NewEncoder()
		encodeContent(enc, NewContentJSON(vals...), 0)
		for name, data := range map[string][]byte{
			"json": enc.Bytes(), "legacy": legacyContentJSONV1(vals...),
		} {
			t.Run(fmt.Sprintf("%s/%d", name, i), func(t *testing.T) {
				data = append(append([]byte(nil), data...), 0x42)
				dec := encoding.NewDecoder(data)
				_, err := decodeContent(dec, nil, wireJSON)
				require.NoError(t, err)
				s := &pendingScanner{rest: encoding.NewDecoder(data)}
				require.EqualValues(t, len(vals), s.content(wireJSON))
				require.NoError(t, s.err)
				require.Equal(t, dec.RemainingBytes(), s.rest.RemainingBytes())
				require.Equal(t, []byte{0x42}, s.rest.RemainingBytes())
			})
		}
	}
	for _, value := range []string{"undefined", "1e9999", "[1e9999]", "{", "\xff", strings.Repeat(" ", 118) + "1e9999", `{"s":"1e9999","v":3}`} {
		t.Run(value, func(t *testing.T) {
			enc := encoding.NewEncoder()
			enc.WriteVarUint(1)
			enc.WriteVarBytes([]byte(value))
			data := enc.Bytes()
			dec := encoding.NewDecoder(data)
			_, wantErr := decodeContent(dec, nil, wireJSON)
			s := &pendingScanner{rest: encoding.NewDecoder(data)}
			s.content(wireJSON)
			require.Equal(t, wantErr == nil, s.err == nil)
			if wantErr == nil {
				require.Equal(t, dec.RemainingBytes(), s.rest.RemainingBytes())
			}
		})
	}
}

// A skipped clock is absent, even when real structs exist on both sides of it.
// Neither the skipped interval nor a dependent item can make its clocks known.
func TestUnit_PendingBudget_SkipsDoNotSatisfyDependencies(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			source := New()
			defer source.Destroy()
			text := source.GetText("text")
			groups := map[ClientID][]*Item{
				1: {{ID: ID{Client: 1}, Content: NewContentDeleted(1)}, {ID: ID{Client: 1, Clock: 3}, Content: NewContentDeleted(1)}},
				2: {{ID: ID{Client: 2}, Origin: &ID{Client: 1, Clock: 2}, Parent: &text.abstractType, Content: NewContentString("a")}},
				3: {{ID: ID{Client: 3}, Origin: &ID{Client: 1, Clock: 2}, Parent: &text.abstractType, Content: NewContentString("b")}},
			}
			encode, apply := encodeStructStoreV1, ApplyUpdateV1
			if version == 2 {
				encode, apply = encodeStructStoreV2, ApplyUpdateV2
			}
			update := encode(groups, newDeleteSet(), nil, source.store)
			target := New(WithMaxPendingItems(1))
			defer target.Destroy()
			budget := newPendingBudget(target, target.StateVector(), update, version == 2)
			require.ErrorIs(t, budget.check(1), ErrInvalidUpdate)
			require.False(t, budget.checked)
			require.ErrorIs(t, apply(target, update, nil), ErrInvalidUpdate)
			require.Zero(t, target.PendingStats().Items, "rejection must not park the update's deferred items")
		})
	}
}

func TestUnit_PendingBudget_ReverseChainCompletes(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			doc := New(WithMaxPendingItems(1))
			defer doc.Destroy()
			apply := ApplyUpdateV1
			if version == 2 {
				apply = ApplyUpdateV2
			}
			const n = 32
			require.NoError(t, apply(doc, pendingReverseChain(version, n), nil))
			require.Equal(t, strings.Repeat("x", n), doc.GetText("text").ToString())
			require.Len(t, doc.StateVector(), n)
			require.Zero(t, doc.PendingStats().Items)
		})
	}
}

// The reference scanner protects the prior fixed-point acceptance decision,
// independently of bounded passes, group cursors and wakeup ordering.
func TestUnit_PendingBudget_GroupHeadsMatchFixedPoint(t *testing.T) {
	rng := rand.New(rand.NewSource(260))
	source := New()
	defer source.Destroy()
	text := source.GetText("text")
	for scenario := 0; scenario < 200; scenario++ {
		groups := make(map[ClientID][]*Item)
		initial := make(StateVector)
		for c := 1; c <= 8; c++ {
			client := ClientID(c)
			initial[client] = uint64(rng.Intn(5))
			clock := uint64(rng.Intn(3))
			for j, n := 0, 1+rng.Intn(5); j < n; j++ {
				content := NewContentString(strings.Repeat("x", 1+rng.Intn(3)))
				item := &Item{ID: ID{Client: client, Clock: clock}, Parent: &text.abstractType, Content: content}
				if rng.Intn(2) == 0 {
					item.Origin = &ID{Client: ClientID(1 + rng.Intn(10)), Clock: uint64(rng.Intn(12))}
				}
				if rng.Intn(3) == 0 {
					item.OriginRight = &ID{Client: ClientID(1 + rng.Intn(10)), Clock: uint64(rng.Intn(12))}
				}
				groups[client] = append(groups[client], item)
				clock += uint64(content.Len() + rng.Intn(3))
			}
		}
		for _, version := range []int{1, 2} {
			encode := encodeStructStoreV1
			if version == 2 {
				encode = encodeStructStoreV2
			}
			update := encode(groups, newDeleteSet(), nil, source.store)
			for _, cap := range []int{0, 1, 4, 100} {
				budget := pendingBudget{initial: initial, update: update, remaining: cap, v2: version == 2}
				want := referencePendingBudgetCheck(budget, cap)
				got := budget.check(cap)
				require.Equal(t, want == nil, got == nil, "scenario=%d V%d cap=%d: reference=%v worklist=%v", scenario, version, cap, want, got)
			}
		}
	}
}

func referencePendingBudgetCheck(b pendingBudget, count int) error {
	if b.checked || count < b.remaining {
		return nil
	}
	known := make(StateVector, len(b.initial))
	for client, clock := range b.initial {
		known[client] = clock
	}
	for first := true; ; first = false {
		s := newPendingScanner(b.update, b.v2)
		clients := s.uint()
		if clients > maxV2Items {
			return ErrInvalidUpdate
		}
		var total uint64
		pending, progressed := 0, false
		for i := uint64(0); i < clients && s.err == nil; i++ {
			n := s.uint()
			total += n
			if total > maxV2Items {
				return ErrInvalidUpdate
			}
			client, clock := s.client(), s.uint()
			existingEnd := known.Clock(client)
			if first {
				existingEnd = b.initial.Clock(client)
			}
			for j := uint64(0); j < n && s.err == nil; j++ {
				length, skip, deps, numDeps := s.item()
				end := clock + length
				if end < clock {
					return ErrInvalidUpdate
				}
				// A skip denotes clocks absent from this message. It advances
				// the wire cursor only and cannot satisfy a dependency.
				if !skip && end > existingEnd {
					ready := clock <= existingEnd
					for _, dep := range deps[:numDeps] {
						if dep.Clock >= known.Clock(dep.Client) {
							ready = false
						}
					}
					if ready {
						existingEnd = end
						if end > known.Clock(client) {
							known[client] = end
							progressed = true
						}
					} else {
						pending++
					}
				}
				clock = end
			}
		}
		if s.err != nil {
			return wrapUpdateErr(s.err)
		}
		if pending <= b.remaining {
			b.checked = true
			return nil
		}
		if !progressed {
			return ErrInvalidUpdate
		}
	}
}

// The scanner shares V2's rest cursor. Advancing Any must retain that cursor
// and match the public decoder's error position, including truncated values.
func TestUnit_PendingScanner_AnyCursor(t *testing.T) {
	values := []any{nil, true, int64(123456), float64(1.25), encoding.BigInt(55), "Ключ 🐷", []byte{1, 2}, []any{1, "s", map[string]any{"nested": []any{true, nil}}}}
	for _, value := range values {
		enc := encoding.NewEncoder()
		enc.WriteAny(value)
		enc.WriteUint8(42)
		data := enc.Bytes()
		for length := 0; length <= len(data); length++ {
			expected := encoding.NewDecoder(data[:length])
			_, wantErr := expected.ReadAny()
			s := &pendingScanner{rest: encoding.NewDecoder(data[:length])}
			s.v2 = &v2Decoder{restDec: s.rest}
			s.any()
			require.Equal(t, wantErr, s.err)
			require.Equal(t, expected.RemainingBytes(), s.rest.RemainingBytes())
			require.Same(t, s.rest, s.v2.restDec, "V2 must observe the advanced rest cursor")
		}
	}
}

// pendingReverseChain puts each origin in the next wire client group. Each
// client has one item; only the last group can integrate on the first pass.
func pendingReverseChain(version, n int) []byte {
	doc := New()
	defer doc.Destroy()
	text := doc.GetText("text")
	groups := make(map[ClientID][]*Item, n)
	for i := 1; i <= n; i++ {
		client := ClientID(i)
		var origin *ID
		if version == 1 && i < n {
			origin = &ID{Client: client + 1}
		}
		if version == 2 && i > 1 {
			origin = &ID{Client: client - 1}
		}
		groups[client] = []*Item{{ID: ID{Client: client}, Parent: &text.abstractType, Origin: origin, Content: NewContentString("x")}}
	}
	if version == 2 {
		return encodeStructStoreV2(groups, newDeleteSet(), nil, doc.store)
	}
	return encodeStructStoreV1(groups, newDeleteSet(), nil, doc.store)
}
