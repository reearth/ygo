package websocket_test

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"
)

// numbersUpdate is an update of client 1 that inserts n numbers into the
// array "a" in one struct, so a decode of it counts one struct and n values.
// Each number is past 255, so a decode allocates each one it builds.
func numbersUpdate(n int) []byte {
	doc := crdt.New(crdt.WithClientID(1))
	arr := doc.GetArray("a")
	vals := make([]any, n)
	for i := range vals {
		vals[i] = 1000 + i
	}
	doc.Transact(func(txn *crdt.Transaction) { arr.Insert(txn, 0, vals) })
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}

// With ReadOnlySyncStep2MaxValues set, a read-only peer's SyncStep2 that
// carries more values than it is answered 0, under Sync and SyncReply alike,
// though the room holds all of it, and its decode stops at the limit: the
// process allocates far fewer objects while the step is answered than the
// step carries values. The limit counts one for the step's struct and one for
// each of its values, so a step at the limit is still answered by whether the
// room holds it, and 0 sets no limit. The step is the editor's own update, in
// one struct.
func TestInteg_ReadOnlySyncStep2MaxValues_AStep2PastItIsAnsweredZeroAndDecodedNoFurther(t *testing.T) {
	const values = 20_000
	numbers := numbersUpdate(values)
	held := crdt.New(crdt.WithClientID(2))
	require.NoError(t, crdt.ApplyUpdateV1(held, numbers, nil))
	step2 := wholeStep2(t, held)
	for name, tc := range map[string]struct {
		limit int
		want  uint64
	}{
		"at the limit":  {limit: 1 + values, want: 1}, // the struct and its values
		"one past it":   {limit: values, want: 0},     // one short of the step's count
		"far past it":   {limit: 1000, want: 0},
		"with no limit": {limit: 0, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, ts := hocuspocusServer(t, func(srv *ygws.Server) {
				srv.Authorize = readOnlyByHeader
				srv.ReadOnlySyncStep2MaxValues = tc.limit
			})
			editor := hpDial(t, ts, "room", false, crdt.New(crdt.WithClientID(1)))
			hpSend(t, editor, "room", tagSync, ygsync.EncodeUpdate(numbers))
			require.Equal(t, []uint64{1}, syncStatuses(t, editor, "room"))

			reader := hpDial(t, ts, "room", true, crdt.New(crdt.WithClientID(3)))
			for _, tag := range []uint64{tagSync, tagSyncReply} {
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				hpSend(t, reader, "room", tag, step2)
				got := syncStatuses(t, reader, "room")
				runtime.ReadMemStats(&after)
				t.Logf("tag %d: answered %v, with %d allocations while the step was answered", tag, got, after.Mallocs-before.Mallocs)
				assert.Equal(t, []uint64{tc.want}, got, "a SyncStep2 of %d values the room holds, under tag %d", values, tag)
				if tc.want == 0 {
					assert.Less(t, after.Mallocs-before.Mallocs, uint64(values/10),
						"the decode must stop at the limit of %d values, not build the step's %d", tc.limit, values)
				}
			}
		})
	}
}
