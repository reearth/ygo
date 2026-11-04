package crdt

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

// truncatedDSState is a document's state as the fixtures record it.
type truncatedDSState struct {
	Text           string                 `json:"text"`
	SV             map[string]uint64      `json:"sv"`
	Deleted        map[string][][2]uint64 `json:"deleted"`
	PendingStructs bool                   `json:"pendingStructs"`
	PendingDeletes bool                   `json:"pendingDeletes"`
	Update         string                 `json:"update"`
}

type truncatedDSFixture struct {
	Name     string `json:"name"`
	Format   string `json:"format"`
	GC       bool   `json:"gc"`
	PrefixV1 string `json:"prefixV1"`
	Update   string `json:"update"`
	Expected struct {
		Update        string           `json:"update"`
		Error         bool             `json:"error"`
		MergeThrows   bool             `json:"mergeThrows"`
		DiffThrows    bool             `json:"diffThrows"`
		ConvertThrows bool             `json:"convertThrows"`
		Settled       truncatedDSState `json:"settled"`
	} `json:"expected"`
}

// An update cut inside its delete set keeps, as in yjs, its structs and the
// delete ranges read in full before the cut. The document then holds what yjs
// holds after its next retry of its parked structs and deletions, since ygo
// drains its queue in the same apply, so a peer that applies the update
// ApplyUpdateV1Landed returns ends where the document does.
func TestConformance_TruncatedDeleteSet_MatchYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "truncated_ds_yjs_fixtures.json"))
	require.NoError(t, err)
	var fixtures []truncatedDSFixture
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.NotEmpty(t, fixtures)
	for _, fx := range fixtures {
		t.Run(fx.Name, func(t *testing.T) {
			prefix, err := hex.DecodeString(fx.PrefixV1)
			require.NoError(t, err)
			update, err := hex.DecodeString(fx.Update)
			require.NoError(t, err)
			want := fx.Expected
			v2 := fx.Format == "v2"
			withPrefix := func() *Doc {
				d := New(WithClientID(99), WithGC(fx.GC))
				require.NoError(t, ApplyUpdateV1(d, prefix, nil))
				return d
			}

			doc := withPrefix()
			var events []string
			doc.OnUpdate(func(u []byte, _ any) { events = append(events, hex.EncodeToString(u)) })
			if v2 {
				err = ApplyUpdateV2(doc, update, nil)
			} else {
				err = ApplyUpdateV1(doc, update, nil)
			}
			if want.Error {
				require.ErrorIs(t, err, ErrInvalidUpdate)
			} else {
				require.NoError(t, err)
			}
			requireTruncatedDSState(t, want.Settled, doc)

			if want.Settled.Update == "" {
				if want.Update != "" {
					assert.Equal(t, []string{want.Update}, events)
				} else {
					// yjs emits nothing here; ygo may still report an empty update.
					for _, e := range events {
						assert.Equal(t, "0000", e)
					}
				}
			} else {
				// yjs reports the retry in an event of its own; ygo's one event
				// carries the same structs and deletions.
				fromYjs, fromYgo := withPrefix(), withPrefix()
				for _, u := range []string{want.Update, want.Settled.Update} {
					applyHex(t, fromYjs, u)
				}
				for _, u := range events {
					applyHex(t, fromYgo, u)
				}
				assert.Equal(t, EncodeStateAsUpdateV1(fromYjs, nil), EncodeStateAsUpdateV1(fromYgo, nil))
			}

			if !v2 {
				room := withPrefix()
				landed, err := ApplyUpdateV1Landed(room, update, nil)
				assert.Equal(t, want.Error, err != nil)
				if !want.Error {
					// An update that applies is relayed whole.
					assert.Nil(t, landed)
					landed = update
				}
				peer := withPrefix()
				if landed != nil {
					require.NoError(t, ApplyUpdateV1(peer, landed, nil))
				}
				assert.Equal(t, EncodeStateAsUpdateV1(room, nil), EncodeStateAsUpdateV1(peer, nil), "a peer given what landed holds what the room holds")
				assert.Equal(t, room.PendingStats(), peer.PendingStats(), "a peer given what landed parks what the room parks")
			}

			merge, diff, convert := MergeUpdatesV1, DiffUpdateV1, UpdateV1ToV2
			other := prefix
			if v2 {
				merge, diff, convert = MergeUpdatesV2, DiffUpdateV2, UpdateV2ToV1
				other, err = UpdateV1ToV2(prefix)
				require.NoError(t, err)
			}
			_, err = merge(update, other)
			assert.Equal(t, want.MergeThrows, err != nil, "merge")
			_, err = diff(update, StateVector{})
			assert.Equal(t, want.DiffThrows, err != nil, "diff")
			_, err = convert(update)
			assert.Equal(t, want.ConvertThrows, err != nil, "convert")
		})
	}
}

// The pending cap does not decide what an update cut in its delete set keeps.
// Under a tight cap, and with the parked-range room used up, the structs and
// deletions read before the cut apply as they do with room to spare.
func TestUnit_TruncatedDeleteSet_KeepsWhatItReadUnderAnyCap(t *testing.T) {
	// Without GC the deleted characters keep their content, so only the
	// delete set deletes them.
	src := New(WithClientID(1), WithGC(false))
	text := src.GetText("t")
	src.Transact(func(txn *Transaction) { text.Insert(txn, 0, "abc"+strings.Repeat("x", 150), nil) })
	src.Transact(func(txn *Transaction) { text.Delete(txn, 50, 1) })
	src.Transact(func(txn *Transaction) { text.Delete(txn, 100, 1) })

	// Twenty ranges of a client nobody holds, which park whole.
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0)
	enc.WriteVarUint(1)
	enc.WriteVarUint(500)
	enc.WriteVarUint(20)
	for i := range 20 {
		enc.WriteVarUint(uint64(i * 10))
		enc.WriteVarUint(1)
	}
	parksTwenty := enc.Bytes()

	for _, format := range []struct {
		name   string
		encode func(*Doc, StateVector) []byte
		apply  func(*Doc, []byte, any) error
	}{{"V1", EncodeStateAsUpdateV1, ApplyUpdateV1}, {"V2", EncodeStateAsUpdateV2, ApplyUpdateV2}} {
		t.Run(format.name, func(t *testing.T) {
			whole := format.encode(src, nil)
			// One byte short, inside the second delete range.
			update := whole[:len(whole)-1]
			noRoom := New(WithClientID(9), WithMaxPendingItems(20))
			require.NoError(t, ApplyUpdateV1(noRoom, parksTwenty, nil))
			require.Equal(t, 20, noRoom.PendingStats().DeleteRanges)
			for _, c := range []struct {
				name string
				doc  *Doc
			}{
				{"room to spare", New(WithClientID(9))},
				{"a tight cap", New(WithClientID(9), WithMaxPendingItems(10))},
				{"no parked-range room", noRoom},
			} {
				if err := format.apply(c.doc, update, nil); !errors.Is(err, ErrInvalidUpdate) {
					t.Fatalf("%s: err %v, want ErrInvalidUpdate for the cut", c.name, err)
				}
				if got := len([]rune(c.doc.GetText("t").ToString())); got != 152 {
					t.Fatalf("%s: the text holds %d characters, want 152: the structs and the first deletion", c.name, got)
				}
			}
		})
	}
}

// requireTruncatedDSState checks doc's text, state vector, deletions and
// parked state against want.
func requireTruncatedDSState(t *testing.T, want truncatedDSState, doc *Doc) {
	t.Helper()
	assert.Equal(t, want.Text, doc.GetText("t").ToString())
	sv := map[string]uint64{}
	for c, clock := range doc.StateVector() {
		sv[strconv.FormatUint(uint64(c), 10)] = clock
	}
	assert.Equal(t, want.SV, sv)
	deleted := map[string][][2]uint64{}
	for c, ranges := range buildDeleteSet(doc.store).clients {
		for _, r := range ranges {
			key := strconv.FormatUint(uint64(c), 10)
			deleted[key] = append(deleted[key], [2]uint64{r.Clock, r.Len})
		}
	}
	assert.Equal(t, want.Deleted, deleted)
	stats := doc.PendingStats()
	assert.Equal(t, want.PendingStructs, stats.Items > 0, "parked structs")
	assert.Equal(t, want.PendingDeletes, stats.DeleteRanges > 0, "parked delete ranges")
}

// applyHex applies the hex V1 update u to doc, if any.
func applyHex(t *testing.T, doc *Doc, u string) {
	t.Helper()
	if u == "" {
		return
	}
	b, err := hex.DecodeString(u)
	require.NoError(t, err)
	require.NoError(t, ApplyUpdateV1(doc, b, nil))
}
