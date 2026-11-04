package crdt

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompat_DeletedItem_StoreTransactionAndSnapshotMatchYjs(t *testing.T) {
	var fixtures []struct {
		Name     string `json:"name"`
		GC       bool   `json:"gc"`
		Length   uint64 `json:"length"`
		Prior    string `json:"prior"`
		Input    string `json:"input"`
		InputV2  string `json:"inputV2"`
		State    string `json:"state"`
		StateV2  string `json:"stateV2"`
		Event    string `json:"event"`
		Snapshot string `json:"snapshot"`
		Text     string `json:"text"`
	}
	raw, err := os.ReadFile("testdata/deleted_yjs_fixtures.json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.NotEmpty(t, fixtures)
	for _, fx := range fixtures {
		for _, format := range []string{"v1", "v2"} {
			t.Run(fx.Name+"/"+format, func(t *testing.T) {
				doc := New(WithClientID(99), WithGC(fx.GC))
				require.NoError(t, ApplyUpdateV1(doc, mustHex(t, fx.Prior), nil))
				var transactionDS DeleteSet
				doc.OnAfterTransaction(func(txn *Transaction) { transactionDS = cloneDeleteSet(txn.deleteSet) })
				var event []byte
				doc.OnUpdate(func(update []byte, _ any) { event = update })
				apply, input := ApplyUpdateV1, fx.Input
				if format == "v2" {
					apply, input = ApplyUpdateV2, fx.InputV2
				}
				require.NoError(t, apply(doc, mustHex(t, input), nil))
				want := []DeleteRange{{Clock: 0, Len: fx.Length}}
				require.Equal(t, want, transactionDS.clients[123456789])
				require.Equal(t, []IDRange{{Clock: 0, Len: fx.Length}}, DeleteSetFromDoc(doc).Ranges(123456789))
				for _, item := range doc.store.clients[123456789] {
					require.True(t, item.Deleted)
				}
				require.Equal(t, fx.Event, hex.EncodeToString(event))
				state := EncodeStateAsUpdateV1(doc, nil)
				assertSameUpdateV1(t, fx.State, hex.EncodeToString(state))
				if strings.HasPrefix(fx.Name, "rooted/") {
					require.Equal(t, fx.State, hex.EncodeToString(state))
					require.Equal(t, fx.StateV2, hex.EncodeToString(EncodeStateAsUpdateV2(doc, nil)))
				}
				v2AsV1, err := UpdateV2ToV1(EncodeStateAsUpdateV2(doc, nil))
				require.NoError(t, err)
				wantV2AsV1, err := UpdateV2ToV1(mustHex(t, fx.StateV2))
				require.NoError(t, err)
				assertSameUpdateV1(t, hex.EncodeToString(wantV2AsV1), hex.EncodeToString(v2AsV1))
				require.Equal(t, fx.Text, doc.GetText("deleted-items").ToString())
				snapshot := CaptureSnapshot(doc)
				require.Equal(t, want, snapshot.DeleteSet.clients[123456789])
				wantSnapshot, err := DecodeSnapshot(mustHex(t, fx.Snapshot))
				require.NoError(t, err)
				require.True(t, EqualSnapshots(wantSnapshot, snapshot))
				decodedSnapshot, err := DecodeSnapshot(EncodeSnapshot(snapshot))
				require.NoError(t, err)
				require.True(t, EqualSnapshots(wantSnapshot, decodedSnapshot))
				peer := New(WithClientID(98), WithGC(fx.GC))
				require.NoError(t, ApplyUpdateV1(peer, EncodeStateAsUpdateV1(doc, nil), nil))
				require.Equal(t, []IDRange{{Clock: 0, Len: fx.Length}}, DeleteSetFromDoc(peer).Ranges(123456789))
				require.Equal(t, state, EncodeStateAsUpdateV1(peer, nil))
				if !fx.GC {
					restored, err := CreateDocFromSnapshot(doc, snapshot)
					require.NoError(t, err)
					require.Equal(t, []IDRange{{Clock: 0, Len: fx.Length}}, DeleteSetFromDoc(restored).Ranges(123456789))
					require.Equal(t, state, EncodeStateAsUpdateV1(restored, nil))
				}
			})
		}
	}
}
