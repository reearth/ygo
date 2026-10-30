package crdt

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

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

// Yjs 13.6.32 refuses each update below unchanged; ygo keeps an earlier struct of the same client group (kept).
func TestUnit_ContentType_UnknownTypeRefRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		update string
		v2     bool
		kept   StateVector
	}{
		{"ref7/v1", "010105002701016d000700", false, nil},
		{"ref7/v2", "000001050000012704016d0100010101070001010000", true, nil},
		{"ref48/v1", "010105002701016d003000", false, nil},
		{"ref48/v2", "000001050000012704016d0100010101300001010000", true, nil},
		{"ref261/v1", "010105002701016d008502016800", false, nil},
		{"ref261/v2", "00010001050000012706026d6801000101010285040001010000", true, nil},
		{"after-other-client/v1", "020109002801016d0161017d010105002701016d003000", false, nil},
		{"after-other-client/v2", "000002090500000328002707036d616d4101000101013001010201007d01010000", true, nil},
		{"ref7-after-other-client/v1", "020109002801016d0161017d010105002701016d000700", false, nil},
		{"ref7-after-other-client/v2", "000002090500000328002707036d616d4101000101010701010201007d01010000", true, nil},
		{"after-same-client/v1", "010205002801016d0161017d012701016d003000", false, StateVector{5: 1}},
		{"after-same-client/v2", "0000010500000328002707036d616d4101000101013001010102007d0100", true, StateVector{5: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			update := mustHex(t, tc.update)
			apply, merge, diff, sv, convert, ids := ApplyUpdateV1, MergeUpdatesV1, DiffUpdateV1, EncodeStateVectorFromUpdate, UpdateV1ToV2, ContentIDsFromUpdateV1
			valid := mustHex(t, hookV1)
			if tc.v2 {
				apply, merge, diff, sv, convert, ids = ApplyUpdateV2, MergeUpdatesV2, DiffUpdateV2, EncodeStateVectorFromUpdateV2, UpdateV2ToV1, ContentIDsFromUpdateV2
				valid = mustHex(t, hookV2)
			}
			doc := New(WithClientID(3))
			require.ErrorIs(t, apply(doc, update, nil), ErrInvalidUpdate)
			if tc.kept == nil {
				require.Empty(t, doc.StateVector())
				require.Empty(t, doc.GetMap("m").Keys())
			} else {
				require.Equal(t, tc.kept, doc.StateVector())
				require.Equal(t, []string{"a"}, doc.GetMap("m").Keys())
			}
			require.Equal(t, PendingStats{}, doc.PendingStats())

			checks := map[string]func() error{
				"merge":       func() error { _, err := merge(valid, update); return err },
				"diff":        func() error { _, err := diff(update, nil); return err },
				"stateVector": func() error { _, err := sv(update); return err },
				"convert":     func() error { _, err := convert(update); return err },
				"contentIDs":  func() error { _, err := ids(update); return err },
			}
			if !tc.v2 {
				checks["docContains"] = func() error { _, err := DocContainsUpdateV1(New(), update); return err }
				checks["snapshotContains"] = func() error { _, err := SnapshotContainsUpdateV1(CaptureSnapshot(New()), update); return err }
			}
			for name, run := range checks {
				if err := run(); !errors.Is(err, ErrInvalidUpdate) {
					t.Errorf("%s: err = %v, want ErrInvalidUpdate", name, err)
				}
			}
		})
	}
}
