package crdt

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// The update events of Yjs 13.6.32 for each transaction below, client 1: Yjs
// encodes them after its GC, so deleted content goes out as ContentDeleted.
const (
	gcUpdateTextV1    = "010301000401017401618101000184010101630101010101"
	gcUpdateTextAllV1 = "0101010001010174050101010005"
	gcUpdateMapV1     = "010201002101016d016b01a80100017d020101010001"
	gcUpdateArrayV1   = "0103010008010161017d0181010001880101017d030101010101"
	// With GC off, Yjs keeps the deleted "b".
	gcUpdateTextGCOffV1 = "01030100040101740161840100016284010101630101010101"
)

// The state of a Yjs 13.6.32 document with GC off, client 1, after one
// transaction inserts "abc" (bold for the formatted pair) and a second
// deletes "b", in V1 and V2, and the update event of a GC-on Yjs document
// that applies it.
const (
	gcSentV1         = "01030100040101740161840100016284010101630101010101"
	gcSentV2         = "000002410102000200030400840704746162634102010100000103000101010100"
	gcSentBoldV1     = "010501000601017404626f6c64047472756584010001618401010162840102016386010304626f6c64046e756c6c0101010201"
	gcSentBoldV2     = "000200020241030300030100050600840286120c74626f6c64616263626f6c64010441010401010000010500787e0101010200"
	gcRemoteUpdateV1 = gcUpdateTextV1
	gcRemoteBoldV1   = "010501000601017404626f6c640474727565840100016181010101840102016386010304626f6c64046e756c6c0101010201"
)

// A transaction's update is encoded after its GC, as Yjs cleanupTransactions
// encodes it after tryGcDeleteSet: content the transaction both inserts and
// deletes goes out as ContentDeleted.
func TestUnit_Doc_OnUpdateEncodesAfterGC(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   []DocOption
		fn     func(doc *Doc, txn *Transaction)
		wantV1 string
	}{
		{"text", nil, func(doc *Doc, txn *Transaction) {
			doc.share["t"].(*YText).Insert(txn, 0, "abc", nil)
			doc.share["t"].(*YText).Delete(txn, 1, 1)
		}, gcUpdateTextV1},
		{"text deleted whole", nil, func(doc *Doc, txn *Transaction) {
			doc.share["t"].(*YText).Insert(txn, 0, "hello", nil)
			doc.share["t"].(*YText).Delete(txn, 0, 5)
		}, gcUpdateTextAllV1},
		{"map key set twice", nil, func(doc *Doc, txn *Transaction) {
			doc.share["m"].(*YMap).Set(txn, "k", 1)
			doc.share["m"].(*YMap).Set(txn, "k", 2)
		}, gcUpdateMapV1},
		{"array", nil, func(doc *Doc, txn *Transaction) {
			doc.share["a"].(*YArray).Insert(txn, 0, []any{1, 2, 3})
			doc.share["a"].(*YArray).Delete(txn, 1, 1)
		}, gcUpdateArrayV1},
		{"text with GC off", []DocOption{WithGC(false)}, func(doc *Doc, txn *Transaction) {
			doc.share["t"].(*YText).Insert(txn, 0, "abc", nil)
			doc.share["t"].(*YText).Delete(txn, 1, 1)
		}, gcUpdateTextGCOffV1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := New(append([]DocOption{WithClientID(1)}, tc.opts...)...)
			doc.GetText("t")
			doc.GetMap("m")
			doc.GetArray("a")
			var got []string
			doc.OnUpdate(func(u []byte, _ any) { got = append(got, hex.EncodeToString(u)) })
			doc.Transact(func(txn *Transaction) { tc.fn(doc, txn) })
			require.Equal(t, []string{tc.wantV1}, got)
		})
	}
}

// A remote apply that inserts and deletes "b" relays it as ContentDeleted,
// as Yjs does, also when a text observer or format markers make ygo clean the
// text up after the observers.
func TestUnit_Doc_OnUpdateEncodesRemoteApplyAfterGC(t *testing.T) {
	for _, f := range []struct {
		name  string
		apply func(*Doc, []byte, any) error
		sent  string
		bold  string
	}{
		{"V1", ApplyUpdateV1, gcSentV1, gcSentBoldV1},
		{"V2", ApplyUpdateV2, gcSentV2, gcSentBoldV2},
	} {
		for _, tc := range []struct {
			name     string
			sent     string
			observed bool
			want     string
		}{
			{"plain", f.sent, false, gcRemoteUpdateV1},
			{"observed text", f.sent, true, gcRemoteUpdateV1},
			{"formatted text", f.bold, false, gcRemoteBoldV1},
		} {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				doc := New(WithClientID(2))
				if tc.observed {
					doc.GetText("t").Observe(func(YTextEvent) {})
				}
				var got []string
				doc.OnUpdate(func(u []byte, _ any) { got = append(got, hex.EncodeToString(u)) })
				require.NoError(t, f.apply(doc, mustHex(t, tc.sent), nil))
				require.Equal(t, []string{tc.want}, got)
			})
		}
	}
}

// A Yjs 13.6.32 state of client 1 whose delete set covers a GC struct (the
// entries of a deleted nested map) and that inserts a bold "abc" and deletes
// "b".
const gcSentWithGCStruct = "010701002101016d01700100020601017404626f6c640474727565840103016181010401840105016386010604626f6c64046e756c6c01010200030501"

// The format cleanup of a remote apply reads a record of the items the apply
// deleted. A V2 update lists the GC struct it carries in the delete set, so
// the record meets an item with no parent and leaves it out; the update the
// doc relays rebuilds the text.
func TestUnit_Doc_RemoteApplyWithGCStructIntoFormattedText(t *testing.T) {
	v1 := mustHex(t, gcSentWithGCStruct)
	v2, err := UpdateV1ToV2(v1)
	require.NoError(t, err)
	for _, f := range []struct {
		name   string
		apply  func(*Doc, []byte, any) error
		update []byte
	}{{"V1", ApplyUpdateV1, v1}, {"V2", ApplyUpdateV2, v2}} {
		t.Run(f.name, func(t *testing.T) {
			doc := New(WithClientID(2))
			doc.GetText("t")
			var got [][]byte
			doc.OnUpdate(func(u []byte, _ any) { got = append(got, u) })
			require.NoError(t, f.apply(doc, f.update, nil))
			require.Len(t, got, 1)
			replica := New(WithClientID(3))
			require.NoError(t, ApplyUpdateV1(replica, got[0], nil))
			require.Equal(t, doc.GetText("t").ToDelta(), replica.GetText("t").ToDelta())
		})
	}
}

// When the encode fails while a panicking transaction commits, no observer
// fires, and the panic and the lock are released as usual.
func TestUnit_Doc_PanickingTransactionWhoseEncodeFails(t *testing.T) {
	doc := New(WithClientID(1))
	text := doc.GetText("t")
	fired := 0
	doc.OnUpdate(func([]byte, any) { fired++ })
	doc.OnAfterTransaction(func(*Transaction) { fired++ })
	bad := &Item{ID: ID{Client: 9}}
	require.PanicsWithValue(t, "boom", func() {
		doc.Transact(func(txn *Transaction) {
			text.Insert(txn, 0, "a", nil)
			doc.store.clients[9] = []*Item{bad} // no content: encoding it panics
			panic("boom")
		})
	})
	require.Zero(t, fired)
	doc.mu.Lock()
	delete(doc.store.clients, 9)
	doc.mu.Unlock()
	doc.Transact(func(txn *Transaction) { text.Insert(txn, 1, "b", nil) })
	require.Equal(t, "ab", text.ToString())
	require.Equal(t, 2, fired)
}

// liveFormatMarkers counts the live format markers in txt.
func liveFormatMarkers(txt *YText) int {
	txt.doc.mu.RLock()
	defer txt.doc.mu.RUnlock()
	n := 0
	for it := txt.start; it != nil; it = it.Right {
		if isContentFormat(it) && !it.Deleted {
			n++
		}
	}
	return n
}

// An observer that splits an item the remote apply deleted does not hide the
// second part from the format cleanup: as in Yjs 13.6.32, which leaves 3 live
// markers, the cleanup visits every part of the deleted range.
func TestUnit_YText_FormatCleanupAfterObserverSplitsDeletedItem(t *testing.T) {
	a := New(WithClientID(1))
	at := a.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "PabcdQ", nil) })
	s := EncodeStateAsUpdateV1(a, nil)
	docs := map[ClientID]*Doc{}
	for _, id := range []ClientID{2, 3, 4, 5} {
		docs[id] = New(WithClientID(id))
		require.NoError(t, ApplyUpdateV1(docs[id], s, nil))
	}
	b, c := docs[2], docs[3]
	bt := b.GetText("t")
	for _, id := range []ClientID{4, 5} {
		d := docs[id]
		dt, sv := d.GetText("t"), d.StateVector()
		d.Transact(func(txn *Transaction) { dt.Format(txn, 5, 1, Attributes{"bold": true}) })
		require.NoError(t, ApplyUpdateV1(b, EncodeStateAsUpdateV1(d, sv), nil))
	}
	require.Equal(t, 4, liveFormatMarkers(bt))
	ct, sv := c.GetText("t"), c.StateVector()
	c.Transact(func(txn *Transaction) { ct.Insert(txn, 4, "Z", nil) })
	split := EncodeStateAsUpdateV1(c, sv)
	sv = a.StateVector()
	a.Transact(func(txn *Transaction) { at.Delete(txn, 2, 3) })
	del := EncodeStateAsUpdateV1(a, sv)
	done := false
	bt.Observe(func(YTextEvent) {
		if !done {
			done = true
			require.NoError(t, ApplyUpdateV1(b, split, nil))
		}
	})
	require.NoError(t, ApplyUpdateV1(b, del, nil))
	require.Equal(t, "PaZQ", bt.ToString())
	require.Equal(t, 3, liveFormatMarkers(bt))
}

// Yjs 13.6.32 updates: client 1 types "PQ", inserts "x" and then a bold "y"
// after it; clients 4 and 5 each make "Q" italic; then client 1 deletes "xy",
// clocks 2 to 5 of client 1, which hold a format marker.
var gcMergedMarkerSent = []string{
	"010601000401017401508401000151c4010001010178c60102010104626f6c640474727565c4010301010179c60104010104626f6c64046e756c6c00",
	"01020400c601050101066974616c69630474727565860101066974616c6963046e756c6c00",
	"01020500c601050101066974616c69630474727565860101066974616c6963046e756c6c00",
}

const gcMergedMarkerDelete = "000101010204"

// A deleted format marker that an observer's RunGC merges into the tombstone
// of the text before it still gets that text's gap cleaned first, so the
// cleanup deletes the same markers as without the merge.
func TestUnit_YText_FormatCleanupAfterObserverMergesDeletedMarker(t *testing.T) {
	apply := func(runGC bool) (int, []Delta) {
		b := New(WithClientID(2))
		bt := b.GetText("t")
		for _, u := range gcMergedMarkerSent {
			require.NoError(t, ApplyUpdateV1(b, mustHex(t, u), nil))
		}
		require.Equal(t, 6, liveFormatMarkers(bt))
		done := false
		bt.Observe(func(YTextEvent) {
			if runGC && !done {
				done = true
				RunGC(b)
			}
		})
		require.NoError(t, ApplyUpdateV1(b, mustHex(t, gcMergedMarkerDelete), nil))
		require.Equal(t, "PQ", bt.ToString())
		return liveFormatMarkers(bt), bt.ToDelta()
	}
	n, delta := apply(false)
	gcN, gcDelta := apply(true)
	require.Equal(t, n, gcN, "live markers")
	require.Equal(t, delta, gcDelta)
}

// The record finds a deleted marker even when the transaction deleted the
// markers in falling clock order.
func TestUnit_YText_RecordDeletedFormatsOutOfOrder(t *testing.T) {
	doc := New(WithClientID(1))
	text := doc.GetText("t")
	doc.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, "ab", nil)
		text.Format(txn, 0, 1, Attributes{"bold": true})
		text.Format(txn, 1, 1, Attributes{"italic": true})
	})
	var markers []uint64
	doc.mu.RLock()
	for it := text.start; it != nil; it = it.Right {
		if isContentFormat(it) {
			markers = append(markers, it.ID.Clock)
		}
	}
	doc.mu.RUnlock()
	require.Len(t, markers, 4)
	doc.Transact(func(txn *Transaction) {
		for i := len(markers) - 1; i >= 0; i-- {
			doc.store.Find(ID{Client: 1, Clock: markers[i]}).delete(txn)
		}
		recordDeletedFormats(txn)
		for _, m := range markers {
			f, ok := firstDeletedFormat(txn, 1, m, m+1)
			require.True(t, ok, "marker at %d", m)
			require.Equal(t, m, f)
		}
	})
}
