package crdt

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// Yjs 13.6.32 writes a gc-enabled subdocument's opts as an empty object. The
// expected updates come from Yjs for a parent with client 1 that sets m.child
// to a Doc with guid "child".
func TestCompat_ContentDoc_DefaultGCOptsMatchYjs13632(t *testing.T) {
	want := map[string]string{
		"V1": "010101002901016d056368696c64056368696c64760000",
		"V2": "00000101000001290f0b6d6368696c646368696c6401450001010000010100760000",
	}
	doc := New(WithClientID(1), WithGC(true))
	m := doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		m.Set(txn, "child", New(WithGUID("child"), WithGC(true), WithCollectionID("workspace")))
	})
	for name, encode := range map[string]func(*Doc, StateVector) []byte{
		"V1": EncodeStateAsUpdateV1,
		"V2": EncodeStateAsUpdateV2,
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want[name], hex.EncodeToString(encode(doc, nil)))
		})
	}
}

// Yjs 13.6.32 writes gc:false, autoLoad and meta in that order when they are
// set. ygo reads each update and writes it back byte for byte.
func TestCompat_ContentDoc_ExplicitOptsMatchYjs13632(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    string
		apply  func(*Doc, []byte, any) error
		encode func(*Doc, StateVector) []byte
	}{
		{"V1", "010101002901016d056368696c640a6368696c642d6d657461760302676379086175746f4c6f616478046d657461760106736f7572636577076669787475726500", ApplyUpdateV1, EncodeStateAsUpdateV1},
		{"V2", "000001010000012914106d6368696c646368696c642d6d65746101050a01010000010100760302676379086175746f4c6f616478046d657461760106736f7572636577076669787475726500", ApplyUpdateV2, EncodeStateAsUpdateV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New(WithClientID(2))
			require.NoError(t, tc.apply(doc, mustHex(t, tc.raw), nil))
			require.Equal(t, tc.raw, hex.EncodeToString(tc.encode(doc, nil)))
		})
	}
}

// A collectionId key that an earlier ygo wrote still decodes.
func TestUnit_ContentDoc_ReadsLegacyCollectionID(t *testing.T) {
	d := newSubdocFromOpts("g", map[string]any{"collectionId": "workspace"})
	require.Equal(t, "workspace", d.CollectionID())
}
