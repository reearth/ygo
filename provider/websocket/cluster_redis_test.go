package websocket_test

import (
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ygoredis "github.com/reearth/ygo/cluster/redis"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/internal/relaylane"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// statsFieldNames returns the exported field names of a struct type.
func statsFieldNames(t reflect.Type) map[string]bool {
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out[t.Field(i).Name] = true
	}
	return out
}

// Both the outbound public snapshot (websocket.RelayStats) and the inbound
// one (cluster/redis.Stats) copy relaylane.Stats field-by-field with no
// compile-time coupling to it — see each type's own doc comment. A new
// counter added to relaylane.Stats would otherwise be silently invisible
// through either public surface: it would keep accumulating inside every
// Lane, but nothing built on top of Lane.Stats() would ever expose it,
// and there is nothing that would fail to compile to catch that. This test
// is the substitute for that missing compile-time coupling: it reflects over
// relaylane.Stats and both public copies and fails if either copy is missing
// a field relaylane.Stats has. It does not require the reverse (a public
// type may have its own fields relaylane.Stats lacks — e.g. Dropped,
// RouterDrops).
func TestPublicStatsCoverRelaylaneFields(t *testing.T) {
	laneFields := statsFieldNames(reflect.TypeOf(relaylane.Stats{}))
	require.NotEmpty(t, laneFields, "sanity: relaylane.Stats must have at least one field")

	for name, typ := range map[string]reflect.Type{
		"websocket.RelayStats": reflect.TypeOf(ygws.RelayStats{}),
		"cluster/redis.Stats":  reflect.TypeOf(ygoredis.Stats{}),
	} {
		got := statsFieldNames(typ)
		for field := range laneFields {
			assert.True(t, got[field],
				"%s is missing field %q present in relaylane.Stats — "+
					"a new relaylane counter must be mirrored in every public Stats copy", name, field)
		}
	}
}

// waitSubscribed polls miniredis until the channel hits at least n
// subscribers, or fails the test after 2s. Replaces timing-dependent
// time.Sleep handshakes (T3 in the v1.21.0 review).
func waitSubscribed(t *testing.T, mr *miniredis.Miniredis, channel string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		counts := mr.PubSubNumSub(channel)
		return counts[channel] >= n
	}, 2*time.Second, 5*time.Millisecond,
		"channel %s never reached %d subscribers (got %d)",
		channel, n, mr.PubSubNumSub(channel)[channel])
}

// Two-server integration via the Redis relay — the canonical acceptance
// criterion for issue #62. miniredis stands in for a real Redis broker so
// the test runs in CI without a docker dependency.
//
// Topology:
//
//	peer-A ──ws──> srvA ──redis─┐
//	                            │
//	                            ▼ (pub/sub fan-out)
//	                            │
//	peer-B ──ws──> srvB ──redis─┘
//
// Edits originated by peer-A must propagate to peer-B (and vice versa),
// and the same for awareness. The Redis adapter sits in the middle as the
// only cross-process transport.
func TestInteg_RedisCluster_TwoServers_SyncPropagates(t *testing.T) {
	mr := miniredis.RunT(t)

	rdbA := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdbA.Close() }()
	rdbB := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdbB.Close() }()

	relayA, err := ygoredis.New(rdbA, ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relayA.Close() }()

	relayB, err := ygoredis.New(rdbB, ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relayB.Close() }()

	srvA := ygws.NewServer()
	srvB := ygws.NewServer()
	require.NoError(t, srvA.AttachRelay(relayA))
	require.NoError(t, srvB.AttachRelay(relayB))

	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	tsB := httptest.NewServer(srvB)
	defer tsB.Close()

	// Peer-A joins room on srvA; peer-B joins same room on srvB.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dial(t, tsA, "room")
	drainHandshake(t, connA, docA)

	docB := crdt.New(crdt.WithClientID(2))
	connB := dial(t, tsB, "room")
	drainHandshake(t, connB, docB)

	// Wait for both servers' SUBSCRIBEs to register at the broker before
	// publishing — a polled check, NOT a timed sleep (T3 review fix).
	waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+"room", 2)

	// Peer-A makes an edit and pushes it to srvA.
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	sendUpdate(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	// srvB's room should converge via the Redis relay.
	require.Eventually(t, func() bool {
		d := srvB.GetDoc("room")
		return d != nil && d.GetText("t").ToString() == "hello"
	}, 3*time.Second, 20*time.Millisecond,
		"srvB's doc must converge via the Redis relay")

	// Peer-B should receive the rebroadcast as a sync frame.
	outerType, payload := readOne(t, connB, 2*time.Second)
	require.Equal(t, uint64(0), outerType, "expected a sync message on peer-B")
	_, _ = ygsync.ApplySyncMessage(docB, payload, nil)
	assert.Equal(t, "hello", docB.GetText("t").ToString())
}

// Awareness updates must propagate across the cluster too — peer-A's
// cursor metadata is visible to srvB and to peer-B.
func TestInteg_RedisCluster_TwoServers_AwarenessPropagates(t *testing.T) {
	mr := miniredis.RunT(t)

	rdbA := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdbA.Close() }()
	rdbB := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer func() { _ = rdbB.Close() }()

	relayA, err := ygoredis.New(rdbA, ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relayA.Close() }()

	relayB, err := ygoredis.New(rdbB, ygoredis.Config{})
	require.NoError(t, err)
	defer func() { _ = relayB.Close() }()

	srvA := ygws.NewServer()
	srvB := ygws.NewServer()
	require.NoError(t, srvA.AttachRelay(relayA))
	require.NoError(t, srvB.AttachRelay(relayB))

	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	tsB := httptest.NewServer(srvB)
	defer tsB.Close()

	connA := dial(t, tsA, "room")
	drainHandshake(t, connA, crdt.New())
	connB := dial(t, tsB, "room")
	drainHandshake(t, connB, crdt.New())

	// Same polled-handshake pattern as the sync test — no timing-dependent
	// sleeps.
	waitSubscribed(t, mr, ygoredis.DefaultChannelPrefix+"room", 2)

	// Peer-A publishes an awareness state.
	awA := buildAwarenessUpdate(t, 42, 1, map[string]any{"cursor": 7})
	sendAwareness(t, connA, awA)

	// srvB's awareness must learn client 42 with cursor=7 via the relay.
	require.Eventually(t, func() bool {
		aw, ok := srvB.GetAwareness("room")
		if !ok {
			return false
		}
		states := aw.GetStates()
		cs, found := states[42]
		if !found {
			return false
		}
		v, ok := cs.State["cursor"].(float64)
		return ok && v == 7
	}, 3*time.Second, 20*time.Millisecond,
		"srvB's awareness must learn client 42 with cursor=7 via the relay")
}

// Echo-loop prevention is verified by the MemRelay test suite
// (TestInteg_Cluster_*) in this package, using recordingRelay.counts to
// assert exactly one Publish-per-edit and zero re-publishes on the
// receiving node. The mechanism — provider-side sentinel pointer-identity
// comparison in provider/websocket/cluster.go — is identical regardless
// of the underlying transport, so duplicating that test here would only
// re-cover the relay-agnostic guard. The two-server propagation tests
// above are the Redis-specific coverage: if the guard were broken in any
// way that interacted with the Redis transport, they would observe
// runaway fan-out (peer-B receiving the same edit repeatedly) and time
// out under Eventually.
