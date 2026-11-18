package crdt

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/reearth/ygo/encoding"
)

func TestClone_SparsePendingQueueAcceptsResend(t *testing.T) {
	// Client 2's clocks 5..8 park behind its missing clock 0. A GC
	// replacement at clock 5 removes one of four slots without compacting.
	first := []byte{1, 4, 2, 5, 1, 1, 1, 't', 1, 1, 1, 1, 't', 1, 1, 1, 1, 't', 1, 1, 1, 1, 't', 1, 0}
	replacement := []byte{1, 1, 2, 5, 0, 1, 0}
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%v", v2), func(t *testing.T) {
			doc := New(WithClientID(99), WithGC(false))
			defer doc.Destroy()
			require.NoError(t, applyVersioned(t, doc, v2, first))
			require.NoError(t, applyVersioned(t, doc, v2, replacement))
			require.Equal(t, 4, doc.PendingStats().Items)
			clone := doc.Clone(WithClientID(98))
			defer clone.Destroy()
			require.NoError(t, applyVersioned(t, doc, v2, first), "original resend")
			require.NoError(t, applyVersioned(t, clone, v2, first), "cloned resend")
			require.Equal(t, doc.PendingStats(), clone.PendingStats())
			require.Equal(t, EncodeStateAsUpdateV1(doc, nil), EncodeStateAsUpdateV1(clone, nil))
		})
	}
}

// ── Equivalence ──────────────────────────────────────────────────────────────

// TestClone_Equivalence clones every document of the corpus and checks the
// clone against the encode-and-apply copy of the same document: the encodings
// are byte-identical, the state vectors and the delete sets are equal, and
// both documents read the same - the same live content, in the same order,
// under the same keys, which the encoding alone does not say, since it is
// written from the store and not from the types' lists.
//
// It also checks the clone against its source: the clone shares no object
// with it, the source is left as it was, and - for every document whose
// applied form holds the source's structs, which is every document that was
// not typed a transaction at a time - the clone is isomorphic to the source
// down to every link. The count of each is reported.
func TestClone_Equivalence(t *testing.T) {
	cases := cloneCorpus(t)
	var identical, isomorphic, applyFailed int
	var differ []string
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srcEnc := EncodeStateAsUpdateV1(tc.doc, nil)
			srcSV := tc.doc.StateVector()
			srcDS := deleteSetKey(tc.doc)

			clone := tc.doc.Clone(tc.opts...)
			cloneEnc := EncodeStateAsUpdateV1(clone, nil)
			requireCloneDisjoint(t, tc.doc, clone)
			if cloneKeepsSourceStructs(tc.doc, clone) {
				isomorphic++
				requireCloneIsomorphic(t, tc.doc, clone)
			}

			// The source is untouched by the clone.
			require.Equal(t, srcEnc, EncodeStateAsUpdateV1(tc.doc, nil))
			require.Equal(t, srcSV, tc.doc.StateVector())
			require.Equal(t, srcDS, deleteSetKey(tc.doc))

			decodeCopy := New(tc.opts...)
			if err := ApplyUpdateV1(decodeCopy, srcEnc, nil); err != nil {
				// A fuzz seed can leave a document whose own encoding the
				// decoder refuses; there is no copy to compare against then.
				applyFailed++
				return
			}
			copyEnc := EncodeStateAsUpdateV1(decodeCopy, nil)
			if bytes.Equal(cloneEnc, copyEnc) {
				identical++
			} else {
				differ = append(differ, tc.name)
			}
			require.Equal(t, copyEnc, cloneEnc, "clone encodes as the encode-and-apply copy")
			require.Equal(t, decodeCopy.StateVector(), clone.StateVector(),
				"clone's state vector is the encode-and-apply copy's")
			require.Equal(t, deleteSetKey(decodeCopy), deleteSetKey(clone),
				"clone's delete set is the encode-and-apply copy's")
			require.Equal(t, liveDigest(decodeCopy), liveDigest(clone),
				"clone reads as the encode-and-apply copy")
		})
	}
	t.Logf("cloned %d documents: %d encode byte-identically to the encode-and-apply copy, "+
		"%d differ (%v), %d have an encoding the decoder refuses; "+
		"%d hold their source's structs and were checked isomorphic to it",
		len(cases), identical, len(differ), differ, applyFailed, isomorphic)
}

// TestClone_RootTypesAreTheClones checks that the named root types of a clone
// are the clone's own types, of the class the source holds, and that a root
// the source never opened by name is still openable on the clone.
func TestClone_RootTypesAreTheClones(t *testing.T) {
	src := New(WithClientID(1))
	frag, txt, m, arr := src.GetXmlFragment("x"), src.GetText("t"), src.GetMap("m"), src.GetArray("a")
	src.Transact(func(tx *Transaction) {
		e := NewYXmlElement("p")
		frag.InsertElement(tx, 0, e)
		e.SetAttribute(tx, "k", "v")
		txt.Insert(tx, 0, "text", nil)
		m.Set(tx, "k", "v")
		arr.Push(tx, []any{1, 2})
	})

	// A root type the source document itself never opened by name: it exists
	// only as the placeholder an apply creates.
	loaded := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(loaded, EncodeStateAsUpdateV1(src, nil), nil))

	for _, doc := range []*Doc{src, loaded} {
		clone := doc.Clone(WithClientID(3))
		require.Equal(t, doc.GetXmlFragment("x").ToXML(), clone.GetXmlFragment("x").ToXML())
		require.Equal(t, doc.GetText("t").ToString(), clone.GetText("t").ToString())
		require.Equal(t, doc.GetMap("m").Entries(), clone.GetMap("m").Entries())
		require.Equal(t, doc.GetArray("a").ToSlice(), clone.GetArray("a").ToSlice())
		require.NotSame(t, doc.GetText("t"), clone.GetText("t"))
		require.NotSame(t, doc.GetXmlFragment("x"), clone.GetXmlFragment("x"))
	}
}

// TestClone_Subdocs checks the subdocument semantics Clone matches: the shells
// an apply builds, registered as an apply registers them, with no content.
func TestClone_Subdocs(t *testing.T) {
	src := New(WithClientID(1))
	m := src.GetMap("m")
	kept := New(WithGUID("kept"), WithAutoLoad(true), WithCollectionID("c1"))
	kept.meta = map[string]any{"kind": "test"}
	gone := New(WithGUID("gone"))
	src.Transact(func(tx *Transaction) {
		m.Set(tx, "kept", kept)
		m.Set(tx, "gone", gone)
	})
	kept.Transact(func(tx *Transaction) { tx.GetText("inner").Insert(tx, 0, "inner", nil) })
	src.Transact(func(tx *Transaction) { m.Delete(tx, "gone") })

	decodeCopy := New(WithClientID(2), WithCollectionID("destination"))
	require.NoError(t, ApplyUpdateV1(decodeCopy, EncodeStateAsUpdateV1(src, nil), nil))
	clone := src.Clone(WithClientID(2), WithCollectionID("destination"))

	require.Equal(t, decodeCopy.GetSubdocGUIDs(), clone.GetSubdocGUIDs())
	require.Equal(t, []string{"kept"}, clone.GetSubdocGUIDs())

	cloned, fromCopy := clone.GetSubdocs()[0], decodeCopy.GetSubdocs()[0]
	require.NotSame(t, kept, cloned)
	require.Equal(t, kept.GUID(), cloned.GUID())
	require.Equal(t, fromCopy.AutoLoad(), cloned.AutoLoad())
	require.Equal(t, fromCopy.ShouldLoad(), cloned.ShouldLoad())
	require.Equal(t, fromCopy.CollectionID(), cloned.CollectionID())
	require.Equal(t, "destination", cloned.CollectionID())
	require.Same(t, clone, cloned.parent)
	require.Equal(t, kept.meta, cloned.meta)
	// The subdocument's own content travels in no update of the parent, so
	// neither copy holds it.
	require.Equal(t, EncodeStateAsUpdateV1(fromCopy, nil), EncodeStateAsUpdateV1(cloned, nil))
	require.Empty(t, cloned.GetText("inner").ToString())
	require.Equal(t, "inner", kept.GetText("inner").ToString())
}

// TestClone_KeepsContentTheSourceStillHolds pins the one place a clone is not
// byte-identical to the encode-and-apply copy: a document with an UndoManager
// attached runs no auto-GC, so it still holds the content of a struct it
// deleted. A clone holds that content too, since copying it needs no
// UndoManager of its own; the copy loses it, because the apply that builds it
// collects the deletion at commit. With no UndoManager attached - every other
// document - the source has collected it already and the two agree.
func TestClone_KeepsContentTheSourceStillHolds(t *testing.T) {
	withUndo := New(WithClientID(1))
	txt := withUndo.GetText("t")
	withUndo.Transact(func(tx *Transaction) { txt.Insert(tx, 0, "abcdef", nil) })
	um := NewUndoManager(withUndo, []SharedType{txt})
	defer um.Destroy()
	withUndo.Transact(func(tx *Transaction) { txt.Delete(tx, 0, 3) })

	srcEnc := EncodeStateAsUpdateV1(withUndo, nil)
	require.Contains(t, string(srcEnc), "abc", "the source must still hold the deleted text")

	clone := withUndo.Clone(WithClientID(2))
	require.Equal(t, srcEnc, EncodeStateAsUpdateV1(clone, nil))

	copyDoc := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(copyDoc, srcEnc, nil))
	require.NotContains(t, string(EncodeStateAsUpdateV1(copyDoc, nil)), "abc",
		"the encode-and-apply copy drops the content of the deletion it applies")

	// Both copies still read the same text, and both encode the same deletes.
	require.Equal(t, "def", clone.GetText("t").ToString())
	require.Equal(t, "def", copyDoc.GetText("t").ToString())
	require.Equal(t, deleteSetKey(copyDoc), deleteSetKey(clone))

	// The same document with no UndoManager attached: the deletion is
	// collected as it is made, and clone and copy are byte-identical.
	plain := New(WithClientID(1))
	plainText := plain.GetText("t")
	plain.Transact(func(tx *Transaction) { plainText.Insert(tx, 0, "abcdef", nil) })
	plain.Transact(func(tx *Transaction) { plainText.Delete(tx, 0, 3) })
	plainEnc := EncodeStateAsUpdateV1(plain, nil)
	require.NotContains(t, string(plainEnc), "abc", "a gc document collects the deletion itself")

	plainCopy := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(plainCopy, plainEnc, nil))
	require.Equal(t, EncodeStateAsUpdateV1(plainCopy, nil),
		EncodeStateAsUpdateV1(plain.Clone(WithClientID(2)), nil))
}

// ── Independence ─────────────────────────────────────────────────────────────

// TestClone_Independence writes to a clone and to its source and checks that
// neither is visible in the other, for every kind of write: a text insert, a
// delete, a format, a map set and an XML attribute.
func TestClone_Independence(t *testing.T) {
	src := newIndependenceDoc(t)
	srcEnc := EncodeStateAsUpdateV1(src, nil)
	srcSV := src.StateVector()
	clone := src.Clone(WithClientID(2))

	// Every kind of write on the clone leaves the source as it was.
	writeEverything(clone, 2)
	require.Equal(t, srcEnc, EncodeStateAsUpdateV1(src, nil), "writing the clone leaves the source's encoding")
	require.Equal(t, srcSV, src.StateVector(), "writing the clone leaves the source's state vector")

	// And every kind of write on the source leaves the clone as it was.
	cloneEnc := EncodeStateAsUpdateV1(clone, nil)
	cloneSV := clone.StateVector()
	require.NotEqual(t, srcEnc, cloneEnc, "the writes did change the clone")
	writeEverything(src, 1)
	require.Equal(t, cloneEnc, EncodeStateAsUpdateV1(clone, nil), "writing the source leaves the clone's encoding")
	require.Equal(t, cloneSV, clone.StateVector(), "writing the source leaves the clone's state vector")
	require.NotEqual(t, srcEnc, EncodeStateAsUpdateV1(src, nil), "the writes did change the source")
}

// TestClone_IndependenceOfPlainValues checks that a write to one document's
// run of plain values - the content a clone copies as a slice, holding the
// values themselves - is not visible in the other's.
func TestClone_IndependenceOfPlainValues(t *testing.T) {
	src := New(WithClientID(1))
	arr := src.GetArray("a")
	src.Transact(func(tx *Transaction) { arr.Push(tx, []any{"a", "b", "c"}) })
	clone := src.Clone(WithClientID(2))
	cloneArr := clone.GetArray("a")

	// Deleting from the clone's array does not shorten the source's.
	clone.Transact(func(tx *Transaction) { cloneArr.Delete(tx, 1, 1) })
	require.Equal(t, []any{"a", "b", "c"}, arr.ToSlice())
	require.Equal(t, []any{"a", "c"}, cloneArr.ToSlice())

	// Appending to the source's run does not reach the clone's.
	src.Transact(func(tx *Transaction) { arr.Push(tx, []any{"d"}) })
	require.Equal(t, []any{"a", "b", "c", "d"}, arr.ToSlice())
	require.Equal(t, []any{"a", "c"}, cloneArr.ToSlice())
}

// TestClone_IntegratesFutureUpdatesLikeTheCopy applies the same sequence of
// remote updates to a clone and to the encode-and-apply copy of one source,
// and checks their encodings match after every one: a clone integrates what
// arrives later exactly as the copy does.
func TestClone_IntegratesFutureUpdatesLikeTheCopy(t *testing.T) {
	// The source is in applied form, so clone and copy start byte-identical.
	builder := newIndependenceDoc(t)
	src := New(WithClientID(1))
	require.NoError(t, ApplyUpdateV1(src, EncodeStateAsUpdateV1(builder, nil), nil))

	clone := src.Clone(WithClientID(2))
	copyDoc := New(WithClientID(2))
	require.NoError(t, ApplyUpdateV1(copyDoc, EncodeStateAsUpdateV1(src, nil), nil))
	require.Equal(t, EncodeStateAsUpdateV1(copyDoc, nil), EncodeStateAsUpdateV1(clone, nil))

	// A third peer, holding the same state, writes the remote updates.
	peer := New(WithClientID(3))
	require.NoError(t, ApplyUpdateV1(peer, EncodeStateAsUpdateV1(src, nil), nil))
	rng := rand.New(rand.NewPCG(7, 11))
	sv := peer.StateVector()
	for round := range 25 {
		randomOps(peer, rng)
		update := EncodeStateAsUpdateV1(peer, sv)
		sv = peer.StateVector()
		require.NoError(t, ApplyUpdateV1(clone, update, nil))
		require.NoError(t, ApplyUpdateV1(copyDoc, update, nil))
		require.Equal(t, EncodeStateAsUpdateV1(copyDoc, nil), EncodeStateAsUpdateV1(clone, nil),
			"round %d", round)
		require.Equal(t, copyDoc.StateVector(), clone.StateVector(), "round %d", round)
	}
}

// newIndependenceDoc builds a document holding every type a write below
// touches: text with formatting, a map, an array and an XML element.
func newIndependenceDoc(t *testing.T) *Doc {
	t.Helper()
	d := New(WithClientID(1))
	txt, m, arr, frag := d.GetText("t"), d.GetMap("m"), d.GetArray("a"), d.GetXmlFragment("x")
	d.Transact(func(tx *Transaction) {
		txt.Insert(tx, 0, "the quick brown fox", nil)
		txt.Format(tx, 4, 5, Attributes{"bold": true})
		m.Set(tx, "k", "v")
		arr.Push(tx, []any{1, 2, 3})
		e := NewYXmlElement("p")
		frag.InsertElement(tx, 0, e)
		e.SetAttribute(tx, "id", "p0")
	})
	return d
}

// writeEverything runs one of each kind of write on d.
func writeEverything(d *Doc, tag int) {
	txt, m, arr, frag := d.GetText("t"), d.GetMap("m"), d.GetArray("a"), d.GetXmlFragment("x")
	d.Transact(func(tx *Transaction) {
		txt.Insert(tx, 0, fmt.Sprintf("w%d ", tag), nil)
		txt.Delete(tx, 5, 3)
		txt.Format(tx, 1, 4, Attributes{"italic": true})
		m.Set(tx, "k", fmt.Sprintf("v%d", tag))
		m.Set(tx, fmt.Sprintf("only%d", tag), true)
		arr.Push(tx, []any{tag})
		if e, ok := frag.Children()[0].(*YXmlElement); ok {
			e.SetAttribute(tx, "id", fmt.Sprintf("p%d", tag))
		}
	})
}

// ── Races ────────────────────────────────────────────────────────────────────

// TestClone_ConcurrentReadsWhileCloning clones a document while other
// goroutines read it, which -race checks for unsynchronised access, and checks
// that every clone is the document the readers see.
func TestClone_ConcurrentReadsWhileCloning(t *testing.T) {
	src := newIndependenceDoc(t)
	srcText := src.GetText("t")
	for i := range 20 {
		src.Transact(func(tx *Transaction) {
			srcText.Insert(tx, 0, fmt.Sprintf("line %d\n", i), nil)
		})
	}
	srcEnc := EncodeStateAsUpdateV1(src, nil)
	srcSV := src.StateVector()
	// Every clone must be the encode-and-apply copy of the document the
	// readers see, byte for byte.
	decodeCopy := New(WithClientID(1000))
	require.NoError(t, ApplyUpdateV1(decodeCopy, srcEnc, nil))
	want := EncodeStateAsUpdateV1(decodeCopy, nil)

	const readers, cloners, rounds = 6, 4, 25
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range rounds {
				_ = src.GetText("t").ToString()
				_ = src.GetMap("m").Entries()
				_ = src.GetArray("a").ToSlice()
				_ = src.GetXmlFragment("x").ToXML()
				_ = src.StateVector()
				_ = src.GetSubdocGUIDs()
				_ = EncodeStateAsUpdateV1(src, nil)
			}
		}()
	}
	errs := make(chan error, cloners*rounds)
	for c := range cloners {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			for i := range rounds {
				clone := src.Clone(WithClientID(ClientID(1000 + id)))
				if got := EncodeStateAsUpdateV1(clone, nil); !bytes.Equal(want, got) {
					errs <- fmt.Errorf("cloner %d round %d: encoding differs", id, i)
				}
			}
		}(c)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// The readers and the cloners left the source as it was.
	require.Equal(t, srcEnc, EncodeStateAsUpdateV1(src, nil))
	require.Equal(t, srcSV, src.StateVector())
}

// cloneItemAfter is one string item of client at clock, with origin as its
// left origin and no right origin.
func cloneItemAfter(client ClientID, clock uint64, origin ID, s string) []byte {
	enc := encoding.NewEncoder()
	for _, v := range []uint64{1, 1, uint64(client), clock} { // one client, one struct
		enc.WriteVarUint(v)
	}
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(uint64(origin.Client))
	enc.WriteVarUint(origin.Clock)
	enc.WriteVarString(s)
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

func TestClone_PendingQueueWithEmptiedSlots(t *testing.T) {
	src := New(WithClientID(1))
	src.GetText("t")
	for _, clock := range []uint64{1, 10, 20} {
		require.NoError(t, ApplyUpdateV1(src, cloneItemAfter(99, clock, ID{Client: 5}, "abc"), nil))
	}
	require.NoError(t, ApplyUpdateV1(src, cloneItemAfter(99, 0, ID{Client: 5}, "xyzw"), nil))
	require.Equal(t, 1, src.store.pending.dead)
	clone := src.Clone(WithClientID(2))
	requireCloneIsomorphic(t, src, clone)
	requireCloneDisjoint(t, src, clone)
	require.Equal(t, src.PendingStats(), clone.PendingStats())
	filler := New(WithClientID(5))
	filler.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "a", nil) })
	update := EncodeStateAsUpdateV1(filler, nil)
	for _, doc := range []*Doc{src, clone} {
		require.NoError(t, ApplyUpdateV1(doc, update, nil))
	}
	require.Equal(t, EncodeStateAsUpdateV1(src, nil), EncodeStateAsUpdateV1(clone, nil))
}

func TestClone_DeferredMoveHeapAndCounters(t *testing.T) {
	src := New(WithClientID(1), WithGC(false))
	arr := src.GetArray("a")
	var moves []*Item
	src.Transact(func(txn *Transaction) {
		for i := range 4 {
			target := ID{Client: 8, Clock: uint64(i)}
			move := &Item{ID: ID{Client: 1, Clock: uint64(i)}, Parent: &arr.abstractType, Content: NewContentMove(&target, 1)}
			require.True(t, integrated(tryIntegrate(txn, move)))
			moves = append(moves, move)
		}
	})
	src.Transact(func(txn *Transaction) { moves[0].delete(txn) })
	clone := src.Clone(WithClientID(2), WithGC(false))
	requireCloneIsomorphic(t, src, clone)
	require.Equal(t, src.store.pendingMoveCount, clone.store.pendingMoveCount)
	require.Equal(t, src.store.deletedPendingMoves, clone.store.deletedPendingMoves)
	require.Equal(t, src.store.pendingMoveMapCap, clone.store.pendingMoveMapCap)
	clone.Transact(func(txn *Transaction) { clone.store.Find(moves[1].ID).delete(txn) })
	require.Equal(t, 1, src.store.deletedPendingMoves)
	require.Equal(t, 2, clone.store.deletedPendingMoves)
}

func TestClone_ConcurrentRootCreation(t *testing.T) {
	src := New()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 100 {
			src.GetText(fmt.Sprintf("root-%d", i))
		}
	}()
	for range 100 {
		require.NotNil(t, src.Clone())
	}
	wg.Wait()
}

func TestClone_ReplacesAClientIDAlreadyPresentInTheCopiedState(t *testing.T) {
	src := New(WithClientID(1))
	src.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "hello", nil) })
	clone := src.Clone(WithClientID(1))
	require.NotEqual(t, ClientID(1), clone.ClientID())
	require.Equal(t, ClientID(99), src.Clone(WithClientID(99)).ClientID())
	clone.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 5, " copy", nil) })
	require.NoError(t, ApplyUpdateV1(src, EncodeStateAsUpdateV1(clone, src.StateVector()), nil))
	require.Equal(t, "hello copy", src.GetText("t").ToString())
}
