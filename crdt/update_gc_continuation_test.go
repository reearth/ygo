package crdt_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/crdt"
)

// gcContinuationCase holds V1 updates in which an item's new clocks follow its
// client's GC struct, and what Yjs 13.6.32 does with them.
type gcContinuationCase struct {
	name    string
	setup   []string // V1 updates applied first
	update  string   // V1 update under test
	v2      string   // the update as Yjs encodes it in V2, when ygo's conversion differs
	text    string   // Yjs's text "t" afterwards
	state   string   // Yjs's EncodeStateAsUpdate afterwards
	changed bool     // Yjs's update event for the update under test carries structs
}

// asFormat returns a V1 hex update in the given format.
func asFormat(t *testing.T, format, v1hex string) []byte {
	t.Helper()
	v1 := mustDecodeHex(t, v1hex)
	if format == "V1" {
		return v1
	}
	v2, err := crdt.UpdateV1ToV2(v1)
	require.NoError(t, err)
	return v2
}

// applyGCContinuation applies tc's setup, then its update, and returns the
// document, whether an OnUpdate payload carried structs, and the update's error.
func applyGCContinuation(t *testing.T, f updateFormat, tc gcContinuationCase) (*crdt.Doc, bool, error) {
	t.Helper()
	doc := crdt.New()
	for _, u := range tc.setup {
		require.NoError(t, f.apply(doc, asFormat(t, f.name, u), nil))
	}
	changed := false
	doc.OnUpdate(func(u []byte, _ any) { changed = changed || u[0] != 0 })
	update := asFormat(t, f.name, tc.update)
	if f.name == "V2" && tc.v2 != "" {
		update = mustDecodeHex(t, tc.v2)
	}
	err := f.apply(doc, update, nil)
	return doc, changed, err
}

// assertYjsResult checks doc against what Yjs holds after tc.
func assertYjsResult(t *testing.T, doc *crdt.Doc, changed bool, tc gcContinuationCase) {
	t.Helper()
	assert.Equal(t, tc.text, doc.GetText("t").ToString())
	assert.Equal(t, len(tc.text), doc.GetText("t").Len())
	yjs := crdt.New()
	require.NoError(t, crdt.ApplyUpdateV1(yjs, mustDecodeHex(t, tc.state), nil))
	assert.Equal(t, crdt.EncodeStateAsUpdateV1(yjs, nil), crdt.EncodeStateAsUpdateV1(doc, nil))
	assert.Zero(t, doc.PendingStats().Items)
	assert.Equal(t, tc.changed, changed)
}

func TestUnit_ApplyUpdate_ItemContinuingAGCStructIsRefused(t *testing.T) {
	for _, tc := range []gcContinuationCase{
		{
			name:   "root parent",
			setup:  []string{"01010100000100"},
			update: "010101000401017402616200",
			state:  "0101010000010101010001",
		},
		{
			name:   "origin in the document",
			setup:  []string{"02010200040101740178010100000100"},
			update: "0101010084020002616200",
			text:   "x",
			state:  "0201020004010174017801010000010101010001",
		},
		{
			// Client 1's group applies before the item's origin resolves.
			name:    "origin later in the update",
			setup:   []string{"01010200000100"},
			update:  "0201020084010002616201010004010174017800",
			text:    "x",
			state:   "0201020000010101000401017401780102010001",
			changed: true,
		},
		{
			// Clients 15 and 13's items, deferred before the refusal, still
			// integrate, 15's after 13's.
			name:    "items deferred before it",
			setup:   []string{"02010e000001010c00000100"},
			update:  "03010f00840d010177010d00840e0002797a010c000401017402616200",
			state:   "04010f000001010e000001010d000002010c000001040f0100010e0100010d0100020c010001",
			changed: true,
		},
		{
			// Client 14's deferred item is refused ahead of client 15's, which
			// still integrates.
			name:    "refused item deferred ahead of another",
			setup:   []string{"01010e00000100"},
			update:  "03010e00840d00026162010f00840d000177010d0004010174017600",
			text:    "vw",
			state:   "03010f00840d000177010e000001010d00040101740176010e010001",
			changed: true,
		},
	} {
		for _, f := range updateFormats {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				doc, changed, err := applyGCContinuation(t, f, tc)
				assertYjsResult(t, doc, changed, tc)
				require.ErrorIs(t, err, crdt.ErrInvalidUpdate)
			})
		}
	}
}

// The item parks in the pending queue and is dropped when its dependency
// arrives, and the update that brings the dependency applies. Yjs throws on it.
func TestUnit_ApplyUpdate_ParkedItemContinuingAGCStructIsDropped(t *testing.T) {
	for _, tc := range []gcContinuationCase{
		{
			name:    "parked for its clocks",
			setup:   []string{"01010102040101740361626300"},
			update:  "01010100000300",
			state:   "0101010000030101010003",
			changed: true,
		},
		{
			// The other parked items still integrate, the second one after the first.
			name:    "parked beside others",
			setup:   []string{"030103008402010179010201040101740178010102040101740361626300"},
			update:  "02010200040101740177010100000300",
			text:    "xyw",
			state:   "03010300840201017902020004010174017704010174017801010000030101010003",
			changed: true,
		},
		{
			name:    "parked for its right origin",
			setup:   []string{"02010200040101740178010100000100", "02010100c40200030002616201000004010174017a00"},
			update:  "01010300840200017100",
			text:    "zxq",
			state:   "040103008402000171010200040101740178010100000101000004010174017a0101010001",
			changed: true,
		},
		{
			name:    "parked by the update that frees it",
			setup:   []string{"01010100000100", "01010300840400017900"},
			update:  "0201040004010174017a01010084030002616200",
			text:    "zy",
			state:   "0301040004010174017a010300840400017901010000010101010001",
			changed: true,
		},
	} {
		for _, f := range updateFormats {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				doc, changed, err := applyGCContinuation(t, f, tc)
				assertYjsResult(t, doc, changed, tc)
				require.NoError(t, err)
			})
		}
	}
}

// Yjs makes such an item a GC struct when its origin or right origin is one;
// ygo does not refuse it.
func TestUnit_ApplyUpdate_ItemContinuingAGCStructNextToAnotherIsNotRefused(t *testing.T) {
	for _, tc := range []gcContinuationCase{
		{
			name:   "right origin is a GC struct",
			setup:  []string{"02010200040101740178010100000100"},
			update: "01010100c40200010002616200",
		},
		{
			name:   "right origin is a GC struct later in the update",
			setup:  []string{"02010200040101740178010500000100"},
			update: "02010500c402000300026162010300000100",
			v2:     "000004050243000100010003c40000040261620200000101020100010000",
		},
		{
			name:   "origin is a GC struct",
			setup:  []string{"020103000001010100000100"},
			update: "0101010084030002616200",
		},
		{
			name:   "origin is a GC struct, right origin an item",
			setup:  []string{"030103000001010200040101740178010100000100"},
			update: "01010100c40300020002616200",
		},
	} {
		for _, f := range updateFormats {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				_, _, err := applyGCContinuation(t, f, tc)
				require.NoError(t, err)
			})
		}
	}
}
