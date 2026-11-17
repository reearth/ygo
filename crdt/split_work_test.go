package crdt

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainedClientsUpdate is the state of a document every client of a room has
// typed one character into at the end of the text: one struct per client,
// each naming the struct of the client before it as its left origin and no
// right origin, over a seed of four characters. It is the longest chain of
// dependencies a document of that many structs can hold, and the walk has to
// place its structs in the order the chain runs, which is the reverse of the
// order the encoder writes the clients in.
func chainedClientsUpdate(t testing.TB, clients int) []byte {
	t.Helper()
	const base = ClientID(100_000)
	doc := New(WithClientID(base))
	seed := &Item{ID: ID{Client: base}, Parent: doc.GetText("t").baseType(), Content: NewContentString("seed")}
	groups := map[ClientID][]*Item{base: {seed}}
	left := ID{Client: base, Clock: seed.Content.Len() - 1}
	for i := range clients {
		c := base + 1 + ClientID(i)
		// A struct that names an origin names no parent: it takes the parent
		// of the struct its origin names, as a client typing at the end of
		// the text after the one before it does.
		it := &Item{ID: ID{Client: c}, Origin: &ID{Client: left.Client, Clock: left.Clock}, Content: NewContentString("x")}
		groups[c] = []*Item{it}
		left = it.ID
	}
	return encodeStructStoreV1(groups, newDeleteSet(), StateVector{})
}

// The walk places a chain of structs in work proportional to the structs, not
// to structs times clients: a document a client can craft is a chain as long
// as the room has clients, and a walk that looked at every client in every
// pass took a pass per link, which is what made a room's answer to a
// browser's SyncStep1 take minutes of CPU.
//
// The bound is on the readiness checks the walk makes rather than on the
// clock, because the count is the same on every machine. A struct is checked
// once for the placement that takes it, and once more for each visit that
// leaves it where it is: the first visit of its client, and one per
// dependency it waits through, of which blocker names three at most: five a
// struct at worst, and two for this shape (8,001 checks for 4,001 structs).
// Eight leaves room above that ceiling. The walk this replaces checked a
// client per pass and took a pass per link: 8,006,001 checks for the same
// update, 250 times the bound.
func TestUnit_SplitUpdateV1_WalksAChainOfClientsInWorkPerStruct(t *testing.T) {
	const (
		clients  = 4_000
		maxBytes = 64 << 10
	)
	update := chainedClientsUpdate(t, clients)
	perClient, ds, err := buildMergeStore([][]byte{update}, decodeStructsV1)
	require.NoError(t, err)
	structs := clients + 1
	require.Len(t, perClient, structs, "one struct per client, and the seed")

	queues := make([]splitQueue, 0, len(perClient))
	for c, items := range perClient {
		queues = appendUnsent(queues, c, items, nil)
	}
	s := newUpdateSplitter(queues, ds, nil, maxBytes, false)
	s.run()

	assert.LessOrEqual(t, s.checks, 8*structs, "readiness checks for %d structs", structs)
	got := applySeries(t, s.out, maxBytes)
	assert.Equal(t, "seed"+strings.Repeat("x", clients), got.GetText("t").ToString())
}

// A room's own structs are walked as the decoded ones are: the split that
// reads them where they are, which answers a SyncStep1 in every room holding
// nothing parked, places the same chain in the same work per struct, and
// sends what the split of the encoded state sends.
func TestUnit_SplitStateAsUpdateV1_WalksAChainOfClientsInWorkPerStruct(t *testing.T) {
	const (
		clients  = 4_000
		maxBytes = 64 << 10
	)
	doc := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(doc, chainedClientsUpdate(t, clients), nil))
	stats := doc.PendingStats()
	require.Zero(t, stats.Items, "the room parks no struct, so it is cut from its own")
	require.Zero(t, stats.DeleteRanges, "the room parks no deletion, so it is cut from its own")
	structs := clients + 1
	require.Len(t, doc.store.clients, structs, "one struct per client, and the seed")

	queues := make([]splitQueue, 0, len(doc.store.clients))
	for c, items := range doc.store.clients {
		queues = appendUnsent(queues, c, items, nil)
	}
	s := newUpdateSplitter(queues, buildDeleteSet(doc.store), nil, maxBytes, true)
	s.run()
	require.NoError(t, s.err)

	assert.LessOrEqual(t, s.checks, 8*structs, "readiness checks for %d structs", structs)
	want, err := SplitUpdateV1(EncodeStateAsUpdateV1(doc, nil), nil, maxBytes)
	require.NoError(t, err)
	requireSameSeries(t, want, s.out, "the room's own structs against its encoded state")
	got := applySeries(t, s.out, maxBytes)
	assert.Equal(t, "seed"+strings.Repeat("x", clients), got.GetText("t").ToString())
}
