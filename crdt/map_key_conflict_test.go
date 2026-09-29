package crdt

import (
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
