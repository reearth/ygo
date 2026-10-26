package crdt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

type interleavedFixture struct {
	Name     string            `json:"name"`
	V1       interleavedWire   `json:"v1"`
	V2       interleavedWire   `json:"v2"`
	Order    []int             `json:"order"`
	Expected interleavedResult `json:"expected"`
}

type interleavedWire struct {
	State   string   `json:"state"`
	Updates []string `json:"updates"`
}

type interleavedResult struct {
	Text    any     `json:"text"`
	Array   any     `json:"array"`
	Map     any     `json:"map"`
	Live    [][]int `json:"live"`
	Deleted [][]int `json:"deleted"`
}

func interleavedRanges(set *IDSet) [][]int {
	out := [][]int{}
	for _, client := range set.Clients() {
		for _, r := range set.Ranges(client) {
			out = append(out, []int{int(client), int(r.Clock), int(r.Len)})
		}
	}
	return out
}

func interleavedJSON(t *testing.T, b []byte, err error) any {
	t.Helper()
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func interleavedResultOf(t *testing.T, doc *Doc) interleavedResult {
	t.Helper()
	array, arrayErr := doc.GetArray("array").ToJSON()
	m, mapErr := doc.GetMap("map").ToJSON()
	return interleavedResult{
		Text:    deltaAsYjsJSON(t, doc.GetText("text").ToDelta()),
		Array:   interleavedJSON(t, array, arrayErr),
		Map:     interleavedJSON(t, m, mapErr),
		Live:    interleavedRanges(InsertSetFromDoc(doc, true)),
		Deleted: interleavedRanges(DeleteSetFromDoc(doc)),
	}
}

type interleavedCodec struct {
	name  string
	apply func(*Doc, []byte, any) error
	merge func(...[]byte) ([]byte, error)
}

var interleavedCodecs = []interleavedCodec{
	{"v1", ApplyUpdateV1, MergeUpdatesV1},
	{"v2", ApplyUpdateV2, MergeUpdatesV2},
}

// interleavedDeliveries replays one history every way a peer or server
// receives it: per-transaction updates in order, reversed and shuffled, the
// encoded full state, and merged updates.
func interleavedDeliveries(t *testing.T, c interleavedCodec, state []byte, updates [][]byte, order []int) map[string]*Doc {
	t.Helper()
	apply := func(doc *Doc, us ...[]byte) *Doc {
		for _, u := range us {
			require.NoError(t, c.apply(doc, u, nil))
		}
		return doc
	}
	merge := func(us ...[]byte) []byte {
		merged, err := c.merge(us...)
		require.NoError(t, err)
		return merged
	}
	reversed := make([][]byte, len(updates))
	for i, u := range updates {
		reversed[len(updates)-1-i] = u
	}
	shuffled := make([][]byte, len(order))
	for i, j := range order {
		shuffled[i] = updates[j]
	}
	half := len(updates) / 2
	tail := updates[len(updates)-min(len(updates), 20):]
	return map[string]*Doc{
		"in order":                   apply(New(), updates...),
		"reversed":                   apply(New(), reversed...),
		"shuffled":                   apply(New(), shuffled...),
		"full state":                 apply(New(), state),
		"merged":                     apply(New(), merge(updates...)),
		"state + merged tail":        apply(New(), merge(append([][]byte{state}, tail...)...)),
		"merged half, rest reversed": apply(New(), append([][]byte{merge(updates[:half]...)}, reversed[:len(updates)-half]...)...),
	}
}

// TestConformance_InterleavedHistories_MatchYjs replays yjs-authored
// multi-peer histories whose clients depend on each other's structs and
// asserts every delivery ends where yjs does.
func TestConformance_InterleavedHistories_MatchYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "interleaved_yjs_fixtures.json"))
	require.NoError(t, err)
	var fixtures []interleavedFixture
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.NotEmpty(t, fixtures)
	for _, fx := range fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			want := fx.Expected
			if want.Map == nil {
				want.Map = map[string]any{}
			}
			for _, c := range interleavedCodecs {
				wire := fx.V1
				if c.name == "v2" {
					wire = fx.V2
				}
				updates := make([][]byte, len(wire.Updates))
				for i, u := range wire.Updates {
					updates[i] = mustHex(t, u)
				}
				for how, doc := range interleavedDeliveries(t, c, mustHex(t, wire.State), updates, fx.Order) {
					require.Zero(t, doc.PendingStats().Items, "%s %s: items left pending", c.name, how)
					if got := interleavedResultOf(t, doc); !reflect.DeepEqual(got, want) {
						t.Errorf("%s %s:\n got %+v\n yjs %+v", c.name, how, got, want)
					}
				}
			}
		})
	}
}

// interleavedTyping is the turn-taking shape: peers append to one text in
// turn, so every struct's origin is the previous peer's struct.
func interleavedTyping(people, steps int) (state []byte, updates [][]byte, want string) {
	typists := make([]*Doc, people)
	for i := range typists {
		typists[i] = New(WithClientID(ClientID(i + 1)))
	}
	page := New()
	for step := 0; step < steps; step++ {
		typist := typists[step%people]
		before := typist.StateVector()
		typist.Transact(func(txn *Transaction) {
			text := txn.GetText("t")
			text.Insert(txn, text.Len(), fmt.Sprintf(" %d-%d", step%people, step), nil)
		})
		u := EncodeStateAsUpdateV1(typist, before)
		for i, other := range typists {
			if i != step%people {
				_ = ApplyUpdateV1(other, u, nil)
			}
		}
		_ = ApplyUpdateV1(page, u, nil)
		updates = append(updates, u)
	}
	return EncodeStateAsUpdateV1(page, nil), updates, page.GetText("t").ToString()
}

// Large interleaved histories exercise the scheduled resolvers rather than
// the small-queue retry passes.
func TestRegression_InterleavedTyping_AllDeliveriesMatch(t *testing.T) {
	state, updates, want := interleavedTyping(5, 3000)
	order := make([]int, len(updates))
	for i := range order {
		order[i] = (i * 7919) % len(updates)
	}
	for how, doc := range interleavedDeliveries(t, interleavedCodecs[0], state, updates, order) {
		require.Zero(t, doc.PendingStats().Items, how)
		require.Equal(t, want, doc.GetText("t").ToString(), how)
	}
}

// interleavedWorkItems models n steps of people peers typing in turn: step k
// is client k%people at clock k/people, with origin step k-1. Step 0 is
// returned separately; the rest are in wire order (client by client).
func interleavedWorkItems(doc *Doc, people, n int, work *resolverWork) (*Item, []*Item) {
	text := doc.GetText("text")
	steps := make([]*Item, n)
	for k := range steps {
		steps[k] = &Item{
			ID:      ID{Client: ClientID(k%people + 1), Clock: uint64(k / people)},
			Parent:  &text.abstractType,
			Content: &resolverWorkContent{NewContentDeleted(1), work},
		}
		if k > 0 {
			steps[k].Origin = &ID{Client: steps[k-1].ID.Client, Clock: steps[k-1].ID.Clock}
		}
	}
	rest := make([]*Item, 0, n-1)
	for c := 0; c < people; c++ {
		for k := c; k < n; k += people {
			if k > 0 {
				rest = append(rest, steps[k])
			}
		}
	}
	return steps[0], rest
}

func TestUnit_PendingResolver_InterleavedClientsBoundWork(t *testing.T) {
	const people, n = 5, 2000
	doc := New(WithClientID(100001))
	defer doc.Destroy()
	work := resolverWork{}
	first, rest := interleavedWorkItems(doc, people, n, &work)
	var err error
	doc.Transact(func(txn *Transaction) {
		require.True(t, tryIntegrate(txn, first))
		err = resolveWithinUpdatePending(txn, rest)
	})
	require.NoError(t, err)
	require.Zero(t, doc.PendingStats().Items)
	require.Equal(t, uint64(n/people), doc.StateVector().Clock(1))
	require.LessOrEqual(t, work.lengths, 32*n, "interleaved clients must not fall back to full retry passes")
}

// Parked items in newest-first order: each full retry pass lands only one.
func TestUnit_DrainPending_NewestFirstBoundWork(t *testing.T) {
	const people, n = 5, 2000
	doc := New(WithClientID(100001), WithMaxPendingItems(n))
	defer doc.Destroy()
	work := resolverWork{}
	first, rest := interleavedWorkItems(doc, people, n, &work)
	parked := make([]*Item, 0, len(rest))
	for k := n - 1; k > 0; k-- {
		for _, item := range rest {
			if item.ID.Client == ClientID(k%people+1) && item.ID.Clock == uint64(k/people) {
				parked = append(parked, item)
				break
			}
		}
	}
	doc.store.pending = &pendingUpdate{items: parked, missing: StateVector{first.ID.Client: 0}}
	work.lengths = 0
	doc.Transact(func(txn *Transaction) {
		require.True(t, tryIntegrate(txn, first))
		drainPending(txn)
	})
	require.Zero(t, doc.PendingStats().Items)
	require.Equal(t, uint64(n/people), doc.StateVector().Clock(1))
	require.LessOrEqual(t, work.lengths, 32*n, "draining must not retry the whole parked list per integrated item")
}

// A merge's origin chain hops between clients on every struct.
func TestUnit_ResolveParents_InterleavedChainBoundWork(t *testing.T) {
	const people, n = 5, 2000
	doc := New()
	defer doc.Destroy()
	work := resolverWork{}
	first, rest := interleavedWorkItems(doc, people, n, &work)
	store := &StructStore{clients: make(map[ClientID][]*Item)}
	for _, item := range append([]*Item{first}, rest...) {
		if item != first {
			item.Parent = nil
		}
		store.clients[item.ID.Client] = append(store.clients[item.ID.Client], item)
	}
	work.lengths = 0
	resolveParents(store)
	for _, items := range store.clients {
		for _, item := range items {
			require.Equal(t, first.Parent, item.Parent)
		}
	}
	require.LessOrEqual(t, work.lengths, 8*n, "each origin chain must be followed once")
}

// Origins forming cycles (self-loops, pairs, and a long loop with a tail) never
// reach a parent; each must be walked a bounded number of times.
func TestUnit_ResolveParents_OriginCyclesBoundWork(t *testing.T) {
	const n = 3000
	work := resolverWork{}
	store := &StructStore{clients: make(map[ClientID][]*Item)}
	add := func(client ClientID, origin ClientID) {
		store.clients[client] = append(store.clients[client], &Item{
			ID:      ID{Client: client},
			Origin:  &ID{Client: origin},
			Content: &resolverWorkContent{NewContentDeleted(1), &work},
		})
	}
	for c := ClientID(1); c <= n; c++ {
		add(c, c) // self-loop
	}
	for c := ClientID(n + 1); c <= 2*n; c += 2 {
		add(c, c+1)
		add(c+1, c)
	}
	const loop = ClientID(4 * n)
	for c := loop; c < loop+n; c++ {
		add(c, c+1) // tail into a loop over the last half
	}
	add(loop+n, loop+n/2)
	resolveParents(store)
	for _, items := range store.clients {
		for _, item := range items {
			require.Nil(t, item.Parent)
		}
	}
	require.LessOrEqual(t, work.lengths, 64*n, "origin cycles must not cost a walk the length of the store")
}
