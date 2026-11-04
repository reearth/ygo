package crdt

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type structDecodeFixture struct {
	Name     string `json:"name"`
	Fmt      string `json:"fmt"`
	Cut      int    `json:"cut"`
	Len      int    `json:"len"`
	Base     string `json:"base"`
	Update   string `json:"update"`
	Expected struct {
		Error   bool              `json:"error"`
		Delta   any               `json:"delta"`
		SV      map[string]uint64 `json:"sv"`
		Pending bool              `json:"pending"`
		Events  []string          `json:"events"`
	} `json:"expected"`
}

// yjs decodes every struct of an update before it integrates any, so an
// update cut inside its structs, or with a malformed struct after a valid one,
// changes nothing and fires no update event.
func TestConformance_StructsThatDoNotDecode_MatchYjs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "structdecode_yjs_fixtures.json"))
	require.NoError(t, err)
	var fixtures []structDecodeFixture
	require.NoError(t, json.Unmarshal(raw, &fixtures))
	require.NotEmpty(t, fixtures)

	for _, fx := range fixtures {
		t.Run(fmt.Sprintf("%s/%s/cut=%d", fx.Fmt, fx.Name, fx.Cut), func(t *testing.T) {
			apply := ApplyUpdateV1
			if fx.Fmt == "v2" {
				apply = ApplyUpdateV2
			}
			base, err := hex.DecodeString(fx.Base)
			require.NoError(t, err)
			update, err := hex.DecodeString(fx.Update)
			require.NoError(t, err)

			doc := newTestDoc(99)
			require.NoError(t, apply(doc, base, nil))
			var events []string
			doc.OnUpdate(func(u []byte, _ any) {
				// ygo also notifies a transaction that changed nothing.
				if h := hex.EncodeToString(u); h != "0000" {
					events = append(events, h)
				}
			})
			err = apply(doc, update, nil)

			assert.Equal(t, fx.Expected.Error, err != nil, "error: %v", err)
			assert.Equal(t, fx.Expected.Delta, deltaAsYjsJSON(t, doc.GetText("t").ToDelta()), "text")
			sv := map[string]uint64{}
			for client, clock := range doc.StateVector() {
				sv[fmt.Sprint(client)] = clock
			}
			assert.Equal(t, fx.Expected.SV, sv, "state vector")
			assert.Equal(t, fx.Expected.Pending, doc.PendingStats().Items > 0, "pending structs")
			assert.Len(t, events, len(fx.Expected.Events), "update events: %v", events)
			for i := range min(len(events), len(fx.Expected.Events)) {
				assertSameUpdateV1(t, fx.Expected.Events[i], events[i])
			}

			if fx.Fmt == "v1" {
				// The websocket provider relays what a failed apply integrated.
				room := newTestDoc(98)
				require.NoError(t, ApplyUpdateV1(room, base, nil))
				landed, err := ApplyUpdateV1Landed(room, update, nil)
				if err == nil || len(fx.Expected.Events) == 0 {
					assert.Nil(t, landed, "landed")
				} else {
					assertSameUpdateV1(t, fx.Expected.Events[0], hex.EncodeToString(landed))
				}
			}
		})
	}
}

// assertSameUpdateV1 checks that two V1 updates carry the same structs and
// deletions.
func assertSameUpdateV1(t *testing.T, want, got string) {
	t.Helper()
	w, err := hex.DecodeString(want)
	require.NoError(t, err)
	g, err := hex.DecodeString(got)
	require.NoError(t, err)
	a, b := newTestDoc(1000), newTestDoc(1001)
	require.NoError(t, ApplyUpdateV1(a, w, nil))
	require.NoError(t, ApplyUpdateV1(b, g, nil))
	assert.Equal(t, EncodeStateAsUpdateV1(a, nil), EncodeStateAsUpdateV1(b, nil), "update %s, want %s", got, want)
}
