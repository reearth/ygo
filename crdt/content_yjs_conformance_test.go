package crdt_test

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// Content-type wire conformance against genuine yjs@13.6.30 bytes, found by a
// source-level diff of ygo's encode/decode vs the Yjs reference. Each fixture
// is real Yjs output; ygo must decode it without error or corruption.
//
// Key subtlety: Yjs's writeJSON differs by wire version — V1 writes a
// JSON-text varstring, V2 writes a structured lib0 `writeAny`. ContentEmbed /
// ContentFormat values ride writeJSON, so the V1 path must use JSON text (the
// V2 path correctly uses writeAny). (#wire-conformance)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// ContentEmbed: YText.insertEmbed(1, {image, w}) between "a" and "b".
// Yjs V1 encodes the embed object as a JSON-text varstring; ygo used WriteAny
// → "unknown Any tag" on decode. V2 uses writeAny (ygo already matched).
func TestConformance_Content_Embed(t *testing.T) {
	const (
		v1 = "0103bbab84b70d0004010174016184bbab84b70d000162c5bbab84b70d00bbab84b70d01207b22696d616765223a22687474703a2f2f782f792e706e67222c2277223a337d00"
		v2 = "000006fbd688ee1a0202010001020504008400c50603746162410101010000010300760205696d616765770e687474703a2f2f782f792e706e6701777d0300"
	)
	check := func(t *testing.T, raw []byte, v2enc bool) {
		doc := crdt.New()
		apply := crdt.ApplyUpdateV1
		if v2enc {
			apply = crdt.ApplyUpdateV2
		}
		require.NoError(t, apply(doc, raw, nil))

		delta := doc.GetText("t").ToDelta()
		var embed map[string]any
		for _, op := range delta {
			if m, ok := op.Insert.(map[string]any); ok {
				embed = m
			}
		}
		require.NotNil(t, embed, "expected an embed insert op in the delta")
		assert.Equal(t, "http://x/y.png", embed["image"])
		assert.EqualValues(t, 3, embed["w"])
	}
	t.Run("v1", func(t *testing.T) { check(t, mustHex(t, v1), false) })
	t.Run("v2", func(t *testing.T) { check(t, mustHex(t, v2), true) })
}

// ContentDoc (subdocument): m.set('child', new Y.Doc()). Yjs writes guid +
// writeAny(opts). ygo's V1 omitted opts entirely (decode stopped after the
// guid → stream desync → "unexpected end of input"); V2 wrote `null` for opts
// (genuine Yjs then crashes on null.shouldLoad). The wire fix: V1 reads/writes
// opts, V2 writes {} not null. ygo doesn't surface subdocs via Get() (a
// separate feature), so the conformance bar is: decode WITHOUT error or
// stream corruption, and the entry integrates into the map (visible in Keys).
func TestConformance_Content_SubDoc(t *testing.T) {
	const (
		v1 = "0101dfa4e78701002901016d056368696c642435356566333938332d613238352d343139302d623234312d646435643062336630343861760000"
		v2 = "0000059fc9ce8f02000001292e2a6d6368696c6435356566333938332d613238352d343139302d623234312d64643564306233663034386101052401010000010100760000"
	)
	for _, tc := range []struct {
		tag   string
		raw   string
		apply func(*crdt.Doc, []byte, any) error
	}{
		{"v1", v1, crdt.ApplyUpdateV1},
		{"v2", v2, crdt.ApplyUpdateV2},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			doc := crdt.New()
			require.NoError(t, tc.apply(doc, mustHex(t, tc.raw), nil),
				"subdoc update must decode without stream desync")
			assert.Contains(t, doc.GetMap("m").Keys(), "child",
				"subdoc entry must integrate into the map, not be dropped")
		})
	}
}

// YXmlHook (typeRef 5): a Yjs fragment with a hook followed by a <div>. ygo
// has no YXmlHook type, but the V1 decoder must consume the hook's name string
// so the rest of the update stays aligned — otherwise the trailing <div>
// struct is misread ("parent item not found"). V2 already degrades gracefully.
func TestConformance_Content_XmlHook_NoDesync(t *testing.T) {
	const v1 = "0102828adaef04000701016605066d79686f6f6b87828adaef0400030364697600"
	doc := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(doc, mustHex(t, v1), nil),
		"a YXmlHook from genuine Yjs must not desync the V1 decoder")
}
