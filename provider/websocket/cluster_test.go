package websocket_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// recordingRelay wraps a MemRelay to count Publish calls per kind, so a test
// can assert that local edits publish and relay-injected edits do NOT.
type recordingRelay struct {
	*cluster.MemRelay
	mu        sync.Mutex
	syncPubs  int
	awarePubs int
}

func newRecordingRelay() *recordingRelay {
	return &recordingRelay{MemRelay: cluster.NewMemRelay(cluster.WithBufferSize(1024))}
}

func (r *recordingRelay) Publish(ctx context.Context, out cluster.Outbound) error {
	r.mu.Lock()
	switch out.Kind {
	case cluster.KindSync:
		r.syncPubs++
	case cluster.KindAwareness:
		r.awarePubs++
	}
	r.mu.Unlock()
	return r.MemRelay.Publish(ctx, out)
}

func (r *recordingRelay) counts() (sync, aware int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.syncPubs, r.awarePubs
}

// sendUpdate frames a V1 update as an outer msgSync + MsgUpdate and sends it.
func sendUpdate(t *testing.T, conn *gws.Conn, update []byte) {
	t.Helper()
	enc := encoding.NewEncoder()
	enc.WriteVarUint(ygsync.MsgUpdate)
	enc.WriteVarBytes(update)
	sendSync(t, conn, enc.Bytes())
}

func TestUnit_AttachRelay_NilRelay(t *testing.T) {
	srv := ygws.NewServer()
	err := srv.AttachRelay(nil)
	assert.ErrorIs(t, err, ygws.ErrNilRelay)
}

func TestUnit_AttachRelay_AlreadyAttached(t *testing.T) {
	srv := ygws.NewServer()
	r1 := cluster.NewMemRelay()
	r2 := cluster.NewMemRelay()
	require.NoError(t, srv.AttachRelay(r1))
	err := srv.AttachRelay(r2)
	assert.ErrorIs(t, err, ygws.ErrRelayAlreadyAttached)
}

func TestInteg_Cluster_SyncPropagatesAcrossServers(t *testing.T) {
	relay := newRecordingRelay()
	defer func() { require.NoError(t, relay.Close()) }()

	srvA := ygws.NewServer()
	srvB := ygws.NewServer()
	require.NoError(t, srvA.AttachRelay(relay))
	require.NoError(t, srvB.AttachRelay(relay))

	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	tsB := httptest.NewServer(srvB)
	defer tsB.Close()

	// Peer on A and peer on B join the same logical room.
	docA := crdt.New(crdt.WithClientID(1))
	connA := dial(t, tsA, "room")
	drainHandshake(t, connA, docA)

	docB := crdt.New(crdt.WithClientID(2))
	connB := dial(t, tsB, "room")
	drainHandshake(t, connB, docB)

	// A's peer makes an edit and sends it to server A.
	txtA := docA.GetText("t")
	docA.Transact(func(txn *crdt.Transaction) { txtA.Insert(txn, 0, "hello", nil) })
	sendUpdate(t, connA, crdt.EncodeStateAsUpdateV1(docA, nil))

	// The edit should arrive on server B's doc via the relay, and be
	// broadcast to B's connected peer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d := srvB.GetDoc("room"); d != nil && d.GetText("t").ToString() == "hello" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NotNil(t, srvB.GetDoc("room"))
	assert.Equal(t, "hello", srvB.GetDoc("room").GetText("t").ToString())

	// B's connected peer should have received the update too.
	outerType, payload := readOne(t, connB, 2*time.Second)
	require.Equal(t, uint64(0), outerType, "expected a sync message")
	_, _ = ygsync.ApplySyncMessage(docB, payload, nil)
	assert.Equal(t, "hello", docB.GetText("t").ToString())

	// Echo guard: B injected the relay update with the sentinel origin, so B
	// must NOT have re-published it. Only A's local edit publishes.
	time.Sleep(100 * time.Millisecond)
	syncPubs, _ := relay.counts()
	assert.Equal(t, 1, syncPubs, "exactly one sync publish (A's local edit; no B echo)")
}

func TestInteg_Cluster_AwarenessPropagatesAcrossServers(t *testing.T) {
	relay := newRecordingRelay()
	defer func() { require.NoError(t, relay.Close()) }()

	srvA := ygws.NewServer()
	srvB := ygws.NewServer()
	require.NoError(t, srvA.AttachRelay(relay))
	require.NoError(t, srvB.AttachRelay(relay))

	tsA := httptest.NewServer(srvA)
	defer tsA.Close()
	tsB := httptest.NewServer(srvB)
	defer tsB.Close()

	connA := dial(t, tsA, "room")
	drainHandshake(t, connA, crdt.New())
	connB := dial(t, tsB, "room")
	drainHandshake(t, connB, crdt.New())

	// A's peer publishes an awareness update.
	awAState := buildAwarenessUpdate(t, 42, 1, map[string]any{"cursor": 7})
	sendAwareness(t, connA, awAState)

	// Server B's awareness should learn client 42.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if aw, ok := srvB.GetAwareness("room"); ok {
			if _, found := aw.GetStates()[42]; found {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	awB, ok := srvB.GetAwareness("room")
	require.True(t, ok)
	states := awB.GetStates()
	cs, found := states[42]
	require.True(t, found, "server B should know client 42 via relay")
	assert.EqualValues(t, 7, cs.State["cursor"])

	// Echo guard: no awareness re-publish from B.
	time.Sleep(100 * time.Millisecond)
	_, awarePubs := relay.counts()
	assert.Equal(t, 1, awarePubs, "exactly one awareness publish (no echo)")
}

// buildAwarenessUpdate encodes a single-client awareness update.
func buildAwarenessUpdate(t *testing.T, clientID, clock uint64, state map[string]any) []byte {
	t.Helper()
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(1) // one client
		enc.WriteVarUint(clientID)
		enc.WriteVarUint(clock)
		b, err := json.Marshal(state)
		require.NoError(t, err)
		enc.WriteVarString(string(b))
	})
}
