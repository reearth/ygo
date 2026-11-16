package crdt

import (
	"encoding/hex"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// A V2 type ref past a byte names no type. It must not alias a known ref, as
// 261 would alias the hook's 5 and make the decoder read a hook name.
func TestUnit_V2Decoder_TypeRefPastAByteAliasesNoType(t *testing.T) {
	var refs encoding.UintOptRleEncoder
	for _, v := range []uint64{261, 5 + 256*9, 5} {
		refs.Write(v)
	}
	d := &v2Decoder{typeRefDec: anycodec.NewUintOptRleDecoder(refs.Bytes())}
	for _, want := range []byte{math.MaxUint8, math.MaxUint8, 5} {
		got, err := d.readTypeRef()
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, want == 5, typeRefHasName(got))
	}
}

// Yjs 13.6.32 fixtures: client 1 sets m["hk"] to XmlHook("h") {a: 1} and inserts XmlHook("widget") and <div> into fragment "x"; client 2 sets hk["b"] and m["z"].
const (
	hookV1     = "010401002701016d02686b050168280001000161017d01070101780506776964676574870102030364697600"
	hookV2     = "0003000300024101020004000727002800070087160f6d686b6861787769646765746469760102410106030501000000010345000301010104007d0100"
	hookEditV1 = "0102020028000100016201770374776f2801016d017a017d0300"
	hookEditV2 = "000002020101000001280603626d7a41010300000100024100010200770374776f7d0300"
	// Y.mergeUpdates of the two.
	hookMergedV1 = "0202020028000100016201770374776f2801016d017a017d030401002701016d02686b050168280001000161017d01070101780506776964676574870102030364697600"
	hookMergedV2 = "0003000300030241020301000400092801270028000700871a12626d7a6d686b68617877696467657464697641020241010603070000010100000103450003024101020200770374776f7d0304007d0100"
	// Client 3 applies hookV1, deletes "hk" and undoes the delete; this is
	// Y.encodeStateAsUpdate of client 3's structs only.
	hookUndoOwnV1 = "01020300a70100050168280003000161017d010101010002"
)

func TestUnit_ContentType_XmlHookReencodesAsYjs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update string
		apply  func(*Doc, []byte, any) error
	}{
		{"v1", hookV1, ApplyUpdateV1},
		{"v2", hookV2, ApplyUpdateV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New(WithClientID(3))
			var emitted []byte
			doc.OnUpdate(func(u []byte, _ any) { emitted = u })
			require.NoError(t, tc.apply(doc, mustHex(t, tc.update), nil))
			require.Equal(t, hookV1, hex.EncodeToString(emitted), "OnUpdate")
			require.Equal(t, hookV1, hex.EncodeToString(EncodeStateAsUpdateV1(doc, nil)))
			require.Equal(t, hookV2, hex.EncodeToString(EncodeStateAsUpdateV2(doc, nil)))
		})
	}
}

// requireHookAndEdit checks that doc holds exactly client 1's hookV1 structs
// and client 2's hookEditV1 structs, each group byte for byte.
func requireHookAndEdit(t *testing.T, doc *Doc) {
	t.Helper()
	require.Equal(t, hookV1, hex.EncodeToString(EncodeStateAsUpdateV1(doc, StateVector{2: 2})))
	require.Equal(t, hookEditV1, hex.EncodeToString(EncodeStateAsUpdateV1(doc, StateVector{1: 4})))
}

func TestUnit_ContentType_XmlHookUpdateFunctionsMatchYjs(t *testing.T) {
	hook1, edit1 := mustHex(t, hookV1), mustHex(t, hookEditV1)
	hook2, edit2 := mustHex(t, hookV2), mustHex(t, hookEditV2)
	merged1, merged2 := mustHex(t, hookMergedV1), mustHex(t, hookMergedV2)
	for _, tc := range []struct {
		name  string
		run   func() ([]byte, error)
		apply func(*Doc, []byte, any) error
	}{
		{"MergeUpdatesV1", func() ([]byte, error) { return MergeUpdatesV1(hook1, edit1) }, ApplyUpdateV1},
		{"MergeUpdatesV2", func() ([]byte, error) { return MergeUpdatesV2(hook2, edit2) }, ApplyUpdateV2},
		{"DiffUpdateV1", func() ([]byte, error) { return DiffUpdateV1(merged1, nil) }, ApplyUpdateV1},
		{"DiffUpdateV2", func() ([]byte, error) { return DiffUpdateV2(merged2, nil) }, ApplyUpdateV2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.run()
			require.NoError(t, err)
			doc := New(WithClientID(3))
			require.NoError(t, tc.apply(doc, got, nil))
			requireHookAndEdit(t, doc)
		})
	}
	t.Run("UpdateV1ToV2", func(t *testing.T) {
		got, err := UpdateV1ToV2(hook1)
		require.NoError(t, err)
		require.Equal(t, hookV2, hex.EncodeToString(got))
	})
	t.Run("UpdateV2ToV1", func(t *testing.T) {
		got, err := UpdateV2ToV1(hook2)
		require.NoError(t, err)
		require.Equal(t, hookV1, hex.EncodeToString(got))
	})
}

func TestUnit_ContentType_XmlHookUndoKeepsName(t *testing.T) {
	doc := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(doc, mustHex(t, hookV1), nil))
	m := doc.GetMap("m")
	um := NewUndoManager(doc, []SharedType{m})
	defer um.Destroy()
	doc.Transact(func(txn *Transaction) { m.Delete(txn, "hk") })
	require.True(t, um.Undo())
	require.Equal(t, hookUndoOwnV1, hex.EncodeToString(EncodeStateAsUpdateV1(doc, StateVector{1: 4})))
}
