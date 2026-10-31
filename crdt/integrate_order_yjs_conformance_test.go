package crdt

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// integrateOrderStep is one applied update and what yjs did while applying
// it: the string items in integration order and the transaction delete set's
// clients in insertion order.
type integrateOrderStep struct {
	V1      string      `json:"v1"`
	V2      string      `json:"v2"`
	Strings [][2]uint64 `json:"strings"`
	DS      []uint64    `json:"ds"`
}

type integrateOrderFixture struct {
	Name       string                          `json:"name"`
	Deliveries map[string][]integrateOrderStep `json:"deliveries"`
}

// applyRecordingOrder applies update as ApplyUpdateV1/V2 do and returns the
// string items in integration order and the delete set's client order.
func applyRecordingOrder(t *testing.T, doc *Doc, update []byte, v2 bool) (strings [][2]uint64, ds []uint64) {
	t.Helper()
	apply := applyV1Txn
	if v2 {
		apply = applyV2Txn
	}
	strings, ds = [][2]uint64{}, []uint64{}
	var err error
	doc.Transact(func(txn *Transaction) {
		txn.Local = false
		err = apply(txn, update)
		for _, item := range txn.newItems {
			strings = append(strings, [2]uint64{uint64(item.ID.Client), item.ID.Clock})
		}
		for _, client := range txn.deleteSet.orderedClients() {
			ds = append(ds, uint64(client))
		}
	})
	require.NoError(t, err)
	return strings, ds
}

// Fixtures: testutil/gen_fixtures_integrateorder.js. Each delivery is applied
// to a fresh receiver in V1 and V2, and every update must integrate its
// structs, and those parked by earlier updates, in yjs's order.
func TestConformance_IntegrateOrder_MatchesYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "integrateorder_yjs_fixtures.json"))
	require.NoError(t, err)
	var fixtures []integrateOrderFixture
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.NotEmpty(t, fixtures)
	for _, fx := range fixtures {
		deliveries := make([]string, 0, len(fx.Deliveries))
		for how := range fx.Deliveries {
			deliveries = append(deliveries, how)
		}
		sort.Strings(deliveries)
		for _, how := range deliveries {
			for _, v2 := range []bool{false, true} {
				name := fx.Name + "/" + how + map[bool]string{false: "/v1", true: "/v2"}[v2]
				t.Run(name, func(t *testing.T) {
					doc := New(WithClientID(1), WithGC(false))
					defer doc.Destroy()
					for i, step := range fx.Deliveries[how] {
						hexUpdate := step.V1
						if v2 {
							hexUpdate = step.V2
						}
						update, err := hex.DecodeString(hexUpdate)
						require.NoError(t, err)
						strings, ds := applyRecordingOrder(t, doc, update, v2)
						require.Equal(t, step.Strings, strings, "update %d: string items in integration order", i)
						require.Equal(t, step.DS, ds, "update %d: delete-set client order", i)
					}
				})
			}
		}
	}
}

// Client 12's text insert has its origin in client 11's group of the same
// update, and each client's next struct replaces a map entry of the other.
// Yjs takes client 12 first and integrates 11:1 for it, so 12:2 deletes 11:0
// before 11:2 deletes 12:0. Bytes from Yjs 13.6.30 (lww case).
func TestUnit_ApplyUpdate_IntegratesDependencyFirst(t *testing.T) {
	for _, tc := range []struct {
		v2            bool
		prior, update string
	}{
		{false, "02010c002801016d026b310177026230010b002801016d026b32017702613000",
			"02020c01840b010171a80b000177026231020b01040101740170a80c000177026131020c0100010b010001"},
		{true, "0000020c0b000001280b066d6b316d6b32010201020101000241000201007702623001007702613000",
			"0000040c4b010c030242000007840088000400880603717470410101010002410002020177026231020177026131020c0100000b010000"},
	} {
		doc := New(WithClientID(2))
		prior, err := hex.DecodeString(tc.prior)
		require.NoError(t, err)
		update, err := hex.DecodeString(tc.update)
		require.NoError(t, err)
		applyRecordingOrder(t, doc, prior, tc.v2)
		strings, ds := applyRecordingOrder(t, doc, update, tc.v2)
		require.Equal(t, [][2]uint64{{11, 1}, {12, 1}}, strings, "v2=%t", tc.v2)
		require.Equal(t, []uint64{11, 12}, ds, "v2=%t", tc.v2)
		doc.Destroy()
	}
}
