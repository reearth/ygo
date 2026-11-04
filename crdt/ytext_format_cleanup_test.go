package crdt

import (
	"math"
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func syncText(t *testing.T, from, to *Doc, origin any) {
	t.Helper()
	require.NoError(t, ApplyUpdateV1(to, EncodeStateAsUpdateV1(from, to.StateVector()), origin))
}

// A empties the text while B bolds its last character. Once A's
// deletions reach B, B drops the emptied range's redundant opener, so text A
// inserts there later is plain on B, as in Yjs.
func TestUnit_YText_RemoteDeleteCleansFormatMarkers(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })
	require.Equal(t, 2, countLiveContentFormat(b))

	syncText(t, a, b, nil)
	assert.Equal(t, 1, countLiveContentFormat(b), "the emptied range keeps only its last marker")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "x", nil) })
	syncText(t, a, b, nil)
	assert.Equal(t, []Delta{{Op: DeltaOpInsert, Insert: "x"}}, bt.ToDelta())

	syncText(t, b, a, nil)
	assert.Equal(t, at.ToDelta(), bt.ToDelta())
	assert.Equal(t, a.StateVector(), b.StateVector())
}

// The cleanup runs after the remote transaction as its own local transaction
// with a nil origin, emitting the deletions as an update of its own (Yjs
// cleanupYTextAfterTransaction parity).
func TestUnit_YText_FormatCleanup_IsLocalFollowUpTxn(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })

	type txnInfo struct {
		origin any
		local  bool
	}
	var txns []txnInfo
	var updates [][]byte
	b.OnAfterTransaction(func(txn *Transaction) { txns = append(txns, txnInfo{txn.Origin, txn.Local}) })
	b.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	syncText(t, a, b, "remote")
	require.Equal(t, []txnInfo{{"remote", false}, {nil, true}}, txns)
	require.Len(t, updates, 2)

	// The cleanup update alone carries B's marker deletion to a peer.
	c := newTestDoc(3)
	syncText(t, b, c, nil)
	assert.Equal(t, bt.ToDelta(), c.GetText("t").ToDelta())
	assert.Equal(t, 1, countLiveContentFormat(c))
}

// An UndoManager with the default tracked origins captures the cleanup, as
// Yjs's does; one tracking other origins does not.
func TestUnit_YText_FormatCleanup_UndoCapture(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		a, b := newTestDoc(1), newTestDoc(2)
		at, bt := a.GetText("t"), b.GetText("t")
		a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
		syncText(t, a, b, nil)
		a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
		b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })
		var opts []UndoManagerOption
		if tracked {
			opts = append(opts, WithTrackedOrigins("user"))
		}
		um := NewUndoManager(b, []SharedType{bt}, opts...)
		syncText(t, a, b, nil)
		require.Equal(t, 1, countLiveContentFormat(b))
		want := 1
		if tracked {
			want = 0
		}
		assert.Equal(t, want, um.UndoStackSize(), "tracked origins: %t", tracked)
		um.Destroy()
	}
}

// Plain text and local edits never start a cleanup transaction.
func TestUnit_YText_FormatCleanup_SkipsPlainAndLocal(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	var n int
	b.OnAfterTransaction(func(*Transaction) { n++ })
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 2) })
	syncText(t, a, b, nil)
	assert.Equal(t, 2, n, "plain remote updates: one transaction each")

	b.Transact(func(txn *Transaction) {
		bt.Insert(txn, 1, "xy", Attributes{"bold": true})
		bt.Delete(txn, 0, 3)
	})
	assert.Equal(t, 3, n, "a local edit: one transaction")
}

// A panic in the cleanup that follows a remote edit reaches the caller with
// the document unlocked and, as from a panicking Transact, the deletions the
// cleanup made emitted.
func TestUnit_YText_RemoteCleanupPanicUnlocks(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })
	c := newTestDoc(3)
	syncText(t, b, c, nil)
	var updates [][]byte
	b.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	cleanupTestHook = func() { panic("cleanup") }
	defer func() { cleanupTestHook = nil }()

	assert.PanicsWithValue(t, "cleanup", func() { syncText(t, a, b, nil) })
	cleanupTestHook = nil
	done := make(chan struct{})
	go func() {
		b.Transact(func(txn *Transaction) { bt.Insert(txn, 0, "x", nil) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("document still locked after the cleanup panicked")
	}
	assert.Equal(t, "x", bt.ToString())
	require.Len(t, updates, 3, "remote, cleanup and local updates")
	for _, u := range updates {
		require.NoError(t, ApplyUpdateV1(c, u, nil))
	}
	assert.Equal(t, 1, countLiveContentFormat(b))
	assert.Equal(t, 1, countLiveContentFormat(c), "the cleanup's deletion reached the peer")
}

// The cleanup and the remote change's GC run even when an observer of the
// change panics, as Yjs runs every cleanup step; the panic then reaches the
// caller with the document unlocked.
func TestUnit_YText_FormatCleanup_AfterObserverPanic(t *testing.T) {
	a, b := newTestDoc(1), newTestDoc(2)
	at, bt := a.GetText("t"), b.GetText("t")
	a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
	syncText(t, a, b, nil)
	a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
	b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })
	var origins []any
	b.OnUpdate(func(_ []byte, origin any) { origins = append(origins, origin) })
	bt.Observe(func(e YTextEvent) {
		if !e.Txn.Local {
			assert.Equal(t, 2, countLiveContentFormat(b), "the observer runs before the cleanup")
			panic("observer")
		}
	})

	assert.PanicsWithValue(t, "observer", func() { syncText(t, a, b, "remote") })
	assert.Equal(t, 1, countLiveContentFormat(b))
	assert.Equal(t, []any{nil}, origins, "the cleanup's update; the panic skipped the remote one's")
	for _, it := range b.store.clients[1] {
		assert.IsType(t, &ContentDeleted{}, it.Content, "the remote deletion is GC'd")
	}
	done := make(chan struct{})
	go func() {
		b.Transact(func(txn *Transaction) { bt.Insert(txn, 0, "x", nil) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("document still locked after the observer panicked")
	}
}

// A root text first accessed after its format markers arrived does not clean
// up until a marker integrates into it, as Yjs's lazily created YText starts
// without _hasFormatting; one never accessed is cleaned only when the same
// remote transaction changes an accessed formatted text.
func TestUnit_YText_FormatCleanup_LazyRoot(t *testing.T) {
	rec := func(d *Doc, fn func(*Transaction)) []byte {
		sv := d.StateVector()
		d.Transact(fn)
		return EncodeStateAsUpdateV1(d, sv)
	}
	b, c, e, f := newTestDoc(2), newTestDoc(3), newTestDoc(4), newTestDoc(5)
	base := rec(b, func(txn *Transaction) {
		txn.GetText("t").Insert(txn, 0, "xay", nil)
		txn.GetText("u").Insert(txn, 0, "q", Attributes{"b": true})
	})
	syncText(t, b, c, nil)
	syncText(t, b, e, nil)
	syncText(t, b, f, nil)
	// B and C bold "a" concurrently, leaving a redundant opener that a
	// deletion beside it cleans up.
	fb := rec(b, func(txn *Transaction) { txn.GetText("t").Format(txn, 1, 1, Attributes{"b": true}) })
	fc := rec(c, func(txn *Transaction) { txn.GetText("t").Format(txn, 1, 1, Attributes{"b": true}) })
	del := rec(e, func(txn *Transaction) { txn.GetText("t").Delete(txn, 0, 1) })
	delAndU := rec(f, func(txn *Transaction) {
		txn.GetText("t").Delete(txn, 0, 1)
		txn.GetText("u").Insert(txn, 1, "r", nil)
	})

	// Cleanups Yjs runs when the receiver first accesses "t" just before
	// update accessAt (4: not before the deletion) and "u" up front or never.
	for _, tc := range []struct {
		accessAt int
		accessU  bool
		last     []byte
		want     int
	}{
		{0, false, del, 1}, {1, false, del, 1}, {2, false, del, 1},
		{3, false, del, 0}, {4, false, del, 0}, {4, true, delAndU, 1},
	} {
		a := newTestDoc(1)
		if tc.accessU {
			a.GetText("u")
		}
		cleanups := 0
		a.OnUpdate(func(_ []byte, origin any) {
			if origin == nil {
				cleanups++
			}
		})
		for i, u := range [][]byte{base, fb, fc, tc.last} {
			if i == tc.accessAt {
				a.GetText("t")
			}
			require.NoError(t, ApplyUpdateV1(a, u, "remote"))
		}
		assert.Equal(t, tc.want, cleanups, "first access before update %d, u accessed %t", tc.accessAt, tc.accessU)
		assert.Equal(t, []Delta{
			{Op: DeltaOpInsert, Insert: "a", Attributes: Attributes{"b": true}},
			{Op: DeltaOpInsert, Insert: "y"},
		}, a.GetText("t").ToDelta())
	}
}

// Parked deletions are retried clients descending, as Yjs re-reads its
// encoded pending delete set. Here the higher client's marker deletions are
// visited first and queue the full cleanup, which skips the contextless
// cleanup the lower client's character deletion would otherwise run.
func TestUnit_YText_FormatCleanup_PendingDeleteOrder(t *testing.T) {
	rec := func(d *Doc, fn func(*YText, *Transaction)) []byte {
		sv := d.StateVector()
		txt := d.GetText("t")
		d.Transact(func(txn *Transaction) { fn(txt, txn) })
		return EncodeStateAsUpdateV1(d, sv)
	}
	bold := func(t *YText, txn *Transaction) { t.Format(txn, 1, 1, Attributes{"b": true}) }
	src := newTestDoc(4)
	base := rec(src, func(t *YText, txn *Transaction) { t.Insert(txn, 0, "zay", nil) })
	peer := func(id uint64) *Doc {
		d := newTestDoc(id)
		require.NoError(t, ApplyUpdateV1(d, base, nil))
		return d
	}
	fb, fd := rec(peer(2), bold), rec(peer(5), bold) // duplicate openers before "a"
	c := peer(3)
	fc := rec(c, bold)
	x := rec(peer(1), func(t *YText, txn *Transaction) { t.Insert(txn, 1, "x", nil) })
	ds := newDeleteSet()
	ds.add(ID{1, 0}, 1)
	ds.add(ID{3, 0}, c.StateVector()[3])
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0)
	encodeDeleteSet(enc, ds)
	late, err := MergeUpdatesV1(fc, x)
	require.NoError(t, err)

	r := newTestDoc(9)
	r.GetText("t")
	cleanups := 0
	r.OnUpdate(func(_ []byte, origin any) {
		if origin == nil {
			cleanups++
		}
	})
	for _, u := range [][]byte{base, fb, fd, enc.Bytes(), late} {
		require.NoError(t, ApplyUpdateV1(r, u, "remote"))
	}
	require.Empty(t, r.store.pendingDs.clients)
	assert.Equal(t, 0, cleanups)
	assert.Equal(t, 4, countLiveContentFormat(r))
}

// The cleanup visits deleted clients in the order they joined the remote
// transaction's delete set. An item that arrives with deleted content joins
// when it integrates, as in Yjs's ContentDeleted.integrate, so here client
// 10's deleted "e" is visited first and its gap cleanup deletes 11:5. A GC
// struct in its place joins nothing, so client 11's marker deletion comes
// first and the full cleanup keeps 11:5. The prior state and the deleted
// item's update are Yjs's; the GC update swaps that item for a GC struct.
func TestUnit_YText_FormatCleanup_ArrivedDeletionOrder(t *testing.T) {
	const (
		priorV1   = "02060b0006010174066974616c69630474727565840b000164860b01066974616c6963046e756c6c440a00026465c60a000a01066974616c696303227822c60a010b00066974616c6963046e756c6c020a00040101740164840a00016500"
		priorV2   = "0003000301074b014a020b4a00050002420242030002420d0600840086004400c601040084291f746974616c6963646974616c696364656974616c69636974616c696374646501060106024600410101010000020600787e7701787e020000"
		deletedV1 = "01010a02410a0001020b0100030a010102"
		deletedV2 = "0000024a000001000141010000000101010102020b0100020a010101"
		gcV1      = "01010a020001020b0100030a010102"
		gcV2      = "0000010a00000100010000000101010102020b0100020a010101"
	)
	for _, tc := range []struct {
		name          string
		apply         func(*Doc, []byte, any) error
		prior, update string
		want          []ID
	}{
		{"deleted_v1", ApplyUpdateV1, priorV1, deletedV1, []ID{{11, 6}}},
		{"deleted_v2", ApplyUpdateV2, priorV2, deletedV2, []ID{{11, 6}}},
		{"gc_v1", ApplyUpdateV1, priorV1, gcV1, []ID{{11, 5}, {11, 6}}},
		{"gc_v2", ApplyUpdateV2, priorV2, gcV2, []ID{{11, 5}, {11, 6}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := newTestDoc(2)
			txt := doc.GetText("t")
			for _, u := range []string{tc.prior, tc.update} {
				require.NoError(t, tc.apply(doc, mustHex(t, u), nil))
			}
			var live []ID
			for it := txt.start; it != nil; it = it.Right {
				if _, ok := it.Content.(*ContentFormat); ok && !it.Deleted {
					live = append(live, it.ID)
				}
			}
			assert.Equal(t, tc.want, live)
		})
	}
}

// Attribute values compare as JavaScript's === does: scalars by value, and a
// composite value only to itself, each marker holding its own object as a
// decoded Yjs marker does.
func TestUnit_YText_FormatCleanup_ValueIdentity(t *testing.T) {
	f := func(v any) *ContentFormat { return NewContentFormat("k", v) }
	buf := []byte{1, 2, 3}
	m := map[string]any{"a": 1}
	same := f([]any{})
	for _, tc := range []struct {
		name string
		a, b *ContentFormat
		want bool
	}{
		{"byte views of one buffer", f(buf[:2]), f(buf[:2]), false},
		{"empty slices", f([]any{}), f([]any{}), false},
		{"empty byte slices", f([]byte{}), f([]byte{}), false},
		{"empty maps", f(map[string]any{}), f(map[string]any{}), false},
		{"one map in two markers", f(m), f(m), false},
		{"one marker", same, same, true},
		{"integer and float", f(int64(1)), f(1.0), true},
		{"NaN", f(math.NaN()), f(math.NaN()), false},
		{"strings", f("x"), f("x"), true},
		{"bigint and number", f(encoding.BigInt(1)), f(int64(1)), false},
		{"bigints", f(encoding.BigInt(1)), f(encoding.BigInt(1)), true},
		{"absent and null", nil, f(nil), true},
		{"absent and false", nil, f(false), false},
	} {
		assert.Equal(t, tc.want, jsIdentical(tc.a, tc.b), tc.name)
	}
}
