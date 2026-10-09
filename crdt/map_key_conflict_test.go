package crdt

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// An unrelated higher-client key must not interrupt same-key arbitration,
// including after snapshot reconstruction changes the global map list order.
func TestUnit_Item_MapKeysHaveIndependentConflictOrder(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			encode := EncodeStateAsUpdateV1
			apply := ApplyUpdateV1
			if version == 2 {
				encode = EncodeStateAsUpdateV2
				apply = ApplyUpdateV2
			}
			updates := make([][]byte, 3)
			for i, client := range []ClientID{10, 20, 999} {
				d := New(WithClientID(client))
				m := d.GetMap("map")
				d.Transact(func(txn *Transaction) {
					if client != 999 {
						m.Set(txn, "shared", fmt.Sprint(client))
					}
					m.Set(txn, fmt.Sprintf("other-%d", client), true)
				})
				updates[i] = encode(d, nil)
				d.Destroy()
			}
			for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
				d := New()
				for _, i := range order {
					require.NoError(t, apply(d, updates[i], nil))
				}
				value, ok := d.GetMap("map").Get("shared")
				require.True(t, ok)
				require.Equal(t, "20", value, "order %v", order)
				reopened := New()
				require.NoError(t, apply(reopened, encode(d, nil), nil))
				require.Equal(t, d.GetMap("map").Entries(), reopened.GetMap("map").Entries())
				// Causal replace and delete must survive the new global list order.
				m := reopened.GetMap("map")
				reopened.Transact(func(txn *Transaction) { m.Set(txn, "shared", "later"); m.Delete(txn, "other-999") })
				require.NoError(t, apply(d, encode(reopened, nil), nil))
				require.Equal(t, m.Entries(), d.GetMap("map").Entries())
				reopened.Destroy()
				d.Destroy()
			}
		})
	}
}

// Yjs compacts two deleted writes into one ContentDeleted run. A concurrent
// replacement references the first clock of that run, so decoding must split
// the run and retain its right half as the key's current item before arbitration.
func TestCompat_MapReplacementInsideDeletedRun(t *testing.T) {
	// Generated with pinned yjs@13.6.30: client 1 sets key=0, syncs to client 2,
	// client 1 sets key=1 while client 2 sets key=2, then merge full updates.
	for _, tc := range []struct {
		name     string
		snapshot string
		apply    func(*Doc, []byte, any) error
		encode   func(*Doc, StateVector) []byte
	}{
		{"v1", "AgECAKgBAAF9AgEBACEBAW0Da2V5AgEBAQAC", ApplyUpdateV1, EncodeStateAsUpdateV1},
		{"v2", "AAADAkEAAQAAA6gAIQcEbWtleQEDAQEAAgECAgEAfQIBAAEBAQAB", ApplyUpdateV2, EncodeStateAsUpdateV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := base64.StdEncoding.DecodeString(tc.snapshot)
			require.NoError(t, err)
			doc := New(WithClientID(3))
			defer doc.Destroy()
			require.NoError(t, tc.apply(doc, raw, nil))
			value, ok := doc.GetMap("m").Get("key")
			require.True(t, ok)
			require.EqualValues(t, 2, value)
			reopened := New(WithClientID(4))
			defer reopened.Destroy()
			require.NoError(t, tc.apply(reopened, tc.encode(doc, nil), nil))
			require.Equal(t, doc.GetMap("m").Entries(), reopened.GetMap("m").Entries())
			m := reopened.GetMap("m")
			reopened.Transact(func(txn *Transaction) { m.Set(txn, "key", 3) })
			require.NoError(t, tc.apply(doc, tc.encode(reopened, nil), nil))
			value, ok = doc.GetMap("m").Get("key")
			require.True(t, ok)
			require.EqualValues(t, 3, value)
			reopened.Transact(func(txn *Transaction) { m.Delete(txn, "key") })
			require.NoError(t, tc.apply(doc, tc.encode(reopened, nil), nil))
			_, ok = doc.GetMap("m").Get("key")
			require.False(t, ok)
		})
	}
}
