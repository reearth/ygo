package crdt

import "testing"

func TestNew_DefaultsGUIDToUUID(t *testing.T) {
	a, b := New(), New()
	guidA, guidB := a.GUID(), b.GUID()

	// Check distinctness and basic length
	if guidA == "" || guidA == guidB || len(guidA) != 36 {
		t.Fatalf("expected distinct uuid guids, got %q / %q", guidA, guidB)
	}

	// Validate RFC-4122 v4 format for guidA
	// Format: xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx
	// where y is one of 8, 9, a, b
	if guidA[14] != '4' {
		t.Errorf("guid[14] should be '4' (version), got %q", string(guidA[14]))
	}
	if guidA[19] != '8' && guidA[19] != '9' && guidA[19] != 'a' && guidA[19] != 'b' {
		t.Errorf("guid[19] should be one of '8','9','a','b' (variant), got %q", string(guidA[19]))
	}
	// Check dashes at the correct positions
	if guidA[8] != '-' || guidA[13] != '-' || guidA[18] != '-' || guidA[23] != '-' {
		t.Errorf("guid format broken: dashes expected at 8,13,18,23, got %q", guidA)
	}
}

func TestSubdocOptionsAndAccessors(t *testing.T) {
	d := New(WithAutoLoad(true), WithCollectionID("workspace-1"))
	if !d.AutoLoad() || !d.ShouldLoad() || d.CollectionID() != "workspace-1" {
		t.Fatalf("autoLoad=%v shouldLoad=%v cid=%q", d.AutoLoad(), d.ShouldLoad(), d.CollectionID())
	}
	if New().AutoLoad() {
		t.Error("AutoLoad() should default false")
	}
}

func TestOnSubdocs_SubscribeUnsubscribe(t *testing.T) {
	d := New()
	got := 0
	unsub := d.OnSubdocs(func(SubdocsEvent) { got++ })
	d.fireSubdocsForTest(SubdocsEvent{Added: []*Doc{New()}})
	if got != 1 {
		t.Fatalf("fired %d, want 1", got)
	}
	unsub()
	d.fireSubdocsForTest(SubdocsEvent{Added: []*Doc{New()}})
	if got != 1 {
		t.Fatalf("fired after unsubscribe: %d", got)
	}
}

func TestGetSubdocs_EmptyInitially(t *testing.T) {
	d := New()
	if len(d.GetSubdocs()) != 0 || len(d.GetSubdocGUIDs()) != 0 {
		t.Fatal("fresh doc has no subdocs")
	}
}

// fireSubdocsForTest fires subdocs callbacks synchronously (test-only helper).
// Must be called from same package to access onSubdocs field.
func (d *Doc) fireSubdocsForTest(ev SubdocsEvent) {
	d.mu.Lock()
	fns := make([]func(SubdocsEvent), len(d.onSubdocs))
	for i, s := range d.onSubdocs {
		fns[i] = s.fn
	}
	d.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

func TestYMap_SetGetSubdoc(t *testing.T) {
	parent := New()
	child := New(WithGUID("child-1"))
	m := parent.GetMap("root")
	parent.Transact(func(txn *Transaction) { m.Set(txn, "a", child) })

	got, ok := m.Get("a")
	if !ok {
		t.Fatal("subdoc key should be present")
	}
	sd, ok := got.(*Doc)
	if !ok {
		t.Fatalf("Get returned %T, want *Doc", got)
	}
	if sd.GUID() != "child-1" {
		t.Fatalf("guid = %q, want child-1", sd.GUID())
	}
	// Non-doc values still round-trip as before.
	parent.Transact(func(txn *Transaction) { m.Set(txn, "n", float64(42)) })
	if v, _ := m.Get("n"); v != float64(42) {
		t.Fatalf("scalar value regressed: %v", v)
	}
}

// embedSubdoc embeds child into parent's root map under key inside a single
// transaction. Shared helper used by the subdoc lifecycle test suite (#63).
func embedSubdoc(t *testing.T, parent *Doc, key string, child *Doc) {
	t.Helper()
	m := parent.GetMap("root")
	parent.Transact(func(txn *Transaction) { m.Set(txn, key, child) })
}

func TestSubdocs_AddedEventAndRegistry(t *testing.T) {
	parent := New()
	var ev SubdocsEvent
	fires := 0
	parent.OnSubdocs(func(e SubdocsEvent) { ev = e; fires++ })
	embedSubdoc(t, parent, "a", New(WithGUID("child-1")))
	if fires != 1 || len(ev.Added) != 1 || ev.Added[0].GUID() != "child-1" {
		t.Fatalf("fires=%d Added=%v", fires, ev.Added)
	}
	if g := parent.GetSubdocGUIDs(); len(g) != 1 || g[0] != "child-1" {
		t.Fatalf("registry=%v", g)
	}
}

func TestSubdocs_RemovedOnDelete(t *testing.T) {
	parent := New()
	embedSubdoc(t, parent, "a", New(WithGUID("c")))
	var ev SubdocsEvent
	parent.OnSubdocs(func(e SubdocsEvent) { ev = e })
	m := parent.GetMap("root")
	parent.Transact(func(txn *Transaction) { m.Delete(txn, "a") })
	if len(ev.Removed) != 1 || ev.Removed[0].GUID() != "c" || len(parent.GetSubdocs()) != 0 {
		t.Fatalf("Removed=%v registry=%v", ev.Removed, parent.GetSubdocs())
	}
}

func TestSubdocs_AddThenDeleteSameTxnCancels(t *testing.T) {
	parent := New()
	fires := 0
	parent.OnSubdocs(func(SubdocsEvent) { fires++ })
	m := parent.GetMap("root")
	parent.Transact(func(txn *Transaction) {
		m.Set(txn, "a", New(WithGUID("c")))
		m.Delete(txn, "a")
	})
	if fires != 0 {
		t.Fatalf("add+delete in one txn should fire nothing, fired %d", fires)
	}
}

func TestSubdocs_LocalSubdocLoadsOnIntegrate(t *testing.T) {
	parent := New()
	var ev SubdocsEvent
	parent.OnSubdocs(func(e SubdocsEvent) { ev = e })
	embedSubdoc(t, parent, "a", New(WithGUID("c"))) // shouldLoad defaults true
	if len(ev.Loaded) != 1 || ev.Loaded[0].GUID() != "c" {
		t.Fatalf("local subdoc should load on integrate; Loaded=%v", ev.Loaded)
	}
}

func TestSubdocs_LoadEmitsLoadedForUnloadedChild(t *testing.T) {
	parent := New()
	child := New(WithGUID("c"))
	child.shouldLoad = false // simulate a decoded/remote child (no autoLoad)
	embedSubdoc(t, parent, "a", child)
	var ev SubdocsEvent
	fires := 0
	parent.OnSubdocs(func(e SubdocsEvent) { ev = e; fires++ })
	child.Load()
	if fires != 1 || len(ev.Loaded) != 1 || ev.Loaded[0] != child || !child.ShouldLoad() {
		t.Fatalf("Load() should emit loaded once + set shouldLoad; fires=%d Loaded=%v", fires, ev.Loaded)
	}
	fires = 0
	child.Load()
	if fires != 0 {
		t.Error("second Load() must be a no-op")
	}
}

// TestSubdocs_LoadAfterRemoveIsNoOp guards against a spurious "loaded" event on
// a detached subdoc. After a subdoc's map entry is deleted, its backing item is
// tombstoned but d.item still points at it. Calling Load() on the now-detached
// doc (shouldLoad still false) must NOT fire a Loaded event on the former
// parent — the doc is no longer resident. Mirrors Yjs, where a removed subdoc
// is destroyed (its _item nulled) so load() is a no-op.
func TestSubdocs_LoadAfterRemoveIsNoOp(t *testing.T) {
	parent := New()
	child := New(WithGUID("c"))
	child.shouldLoad = false // decoded/remote child, not auto-loaded
	embedSubdoc(t, parent, "a", child)

	m := parent.GetMap("root")
	parent.Transact(func(txn *Transaction) { m.Delete(txn, "a") })
	if len(parent.GetSubdocs()) != 0 {
		t.Fatalf("precondition: expected empty registry after delete, got %v", parent.GetSubdocGUIDs())
	}

	fires := 0
	var ev SubdocsEvent
	parent.OnSubdocs(func(e SubdocsEvent) { fires++; ev = e })
	child.Load()
	if fires != 0 {
		t.Fatalf("Load() on a removed subdoc must not fire a subdocs event; fired %d (Loaded=%v)", fires, ev.Loaded)
	}
}

func TestContentDoc_CopyClonesFreshDoc(t *testing.T) {
	orig := New(WithGUID("g"), WithAutoLoad(true), WithCollectionID("cid"))
	cp := (&ContentDoc{orig}).Copy().(*ContentDoc)
	if cp.Doc == orig || cp.Doc.GUID() != "g" || !cp.Doc.AutoLoad() || cp.Doc.CollectionID() != "cid" {
		t.Fatalf("Copy must clone opts into a fresh Doc; got %+v", cp.Doc)
	}
}

func TestSubdocs_DoubleEmbedGuard(t *testing.T) {
	parent := New()
	child := New(WithGUID("c"))
	embedSubdoc(t, parent, "a", child)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("re-embedding an integrated subdoc should panic")
		}
	}()
	m := parent.GetMap("root")
	parent.Transact(func(txn *Transaction) { m.Set(txn, "b", child) })
}

func TestSubdocs_V1OptsRoundTrip(t *testing.T) {
	parent := New()
	embedSubdoc(t, parent, "a", New(WithGUID("c"), WithAutoLoad(true), WithCollectionID("cid")))
	fresh := New()
	if err := ApplyUpdateV1(fresh, EncodeStateAsUpdateV1(parent, nil), nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	subs := fresh.GetSubdocs()
	if len(subs) != 1 || subs[0].GUID() != "c" || !subs[0].AutoLoad() || subs[0].CollectionID() != "cid" || !subs[0].ShouldLoad() {
		t.Fatalf("autoLoad subdoc opts lost: %+v", subs)
	}

	p2 := New()
	embedSubdoc(t, p2, "a", New(WithGUID("d"))) // autoLoad false
	f2 := New()
	if err := ApplyUpdateV1(f2, EncodeStateAsUpdateV1(p2, nil), nil); err != nil {
		t.Fatal(err)
	}
	if f2.GetSubdocs()[0].ShouldLoad() {
		t.Error("non-autoLoad subdoc should decode shouldLoad=false")
	}
}

func TestSubdocs_V2AndMergeRoundTrip(t *testing.T) {
	parent := New()
	embedSubdoc(t, parent, "a", New(WithGUID("c"), WithAutoLoad(true)))

	f2 := New()
	if err := ApplyUpdateV2(f2, EncodeStateAsUpdateV2(parent, nil), nil); err != nil {
		t.Fatalf("apply v2: %v", err)
	}
	if s := f2.GetSubdocs(); len(s) != 1 || !s[0].AutoLoad() {
		t.Fatalf("v2 lost subdoc/opts: %v", s)
	}

	merged, err := MergeUpdatesV1(EncodeStateAsUpdateV1(parent, nil))
	if err != nil {
		t.Fatalf("merge v1: %v", err)
	}
	fm := New()
	if err := ApplyUpdateV1(fm, merged, nil); err != nil {
		t.Fatalf("apply merged v1: %v", err)
	}
	if s := fm.GetSubdocs(); len(s) != 1 || s[0].GUID() != "c" || !s[0].AutoLoad() {
		t.Fatalf("merge v1 lost subdoc/opts: %v", s)
	}

	mergedV2, err := MergeUpdatesV2(EncodeStateAsUpdateV2(parent, nil))
	if err != nil {
		t.Fatalf("merge v2: %v", err)
	}
	fm2 := New()
	if err := ApplyUpdateV2(fm2, mergedV2, nil); err != nil {
		t.Fatalf("apply merged v2: %v", err)
	}
	if s := fm2.GetSubdocs(); len(s) != 1 || !s[0].AutoLoad() {
		t.Fatalf("merge v2 lost subdoc/opts: %v", s)
	}
}

// noDocInBothAddedAndRemoved fails the test if any event has the same *Doc
// appearing in both Added and Removed — the corruption signature of the #63
// final-review I-1 bug, where the tail-positioned ContentDoc registration ran
// AFTER the ParentSub last-writer-wins self-delete, so a losing concurrent
// embed got routed to subdocsRemoved and then unconditionally re-added.
func noDocInBothAddedAndRemoved(t *testing.T, events []SubdocsEvent) {
	t.Helper()
	for i, ev := range events {
		added := map[*Doc]bool{}
		for _, d := range ev.Added {
			added[d] = true
		}
		for _, d := range ev.Removed {
			if added[d] {
				t.Fatalf("event %d: doc %q present in both Added and Removed: %+v", i, d.GUID(), ev)
			}
		}
	}
}

// TestSubdocs_ConcurrentSameKeySameGUID_RegistryNotEvicted reproduces #63
// final-review finding I-1 for the same-guid variant: two peers concurrently
// embed a subdocument (same guid, different Go *Doc objects) under the same
// map key. YATA arbitration makes one lose (integrates non-rightmost) and
// self-delete. Before the fix, the loser's registration ran after its own
// self-delete, so the loser got recorded in both subdocsAdded and
// subdocsRemoved for the same guid string — and since the registry
// (d.subdocs) is keyed by guid, the Added pass (last write wins by guid) and
// then the Removed pass (delete by guid) could evict the WINNER's live entry
// entirely, even though the winner was added in an earlier, independent
// transaction. GetSubdocGUIDs() must keep listing the winner.
//
// Deterministic ClientIDs pin the YATA arbitration: an item's Origin/Right
// are both nil for a first write to a fresh key, so the tie-break is a raw
// ID.Client comparison (crdt/item.go's conflict scan). The item that
// integrates SECOND with the LOWER ClientID ends up placed to the LEFT of
// (i.e. non-rightmost relative to) the already-integrated higher-ClientID
// item, and therefore self-deletes via the ParentSub LWW block.
func TestSubdocs_ConcurrentSameKeySameGUID_RegistryNotEvicted(t *testing.T) {
	parentHi := New(WithClientID(2))
	parentLo := New(WithClientID(1))
	embedSubdoc(t, parentHi, "k", New(WithGUID("shared")))
	embedSubdoc(t, parentLo, "k", New(WithGUID("shared")))

	updHi := EncodeStateAsUpdateV1(parentHi, nil)
	updLo := EncodeStateAsUpdateV1(parentLo, nil)

	target := New()
	var events []SubdocsEvent
	target.OnSubdocs(func(ev SubdocsEvent) { events = append(events, ev) })

	// Apply the higher-ClientID update first so its item becomes parent.start
	// (trivially rightmost, wins). Then apply the lower-ClientID update: its
	// item loses YATA arbitration against the already-placed item and
	// integrates non-rightmost, triggering the self-delete path.
	if err := ApplyUpdateV1(target, updHi, nil); err != nil {
		t.Fatalf("apply hi: %v", err)
	}
	if err := ApplyUpdateV1(target, updLo, nil); err != nil {
		t.Fatalf("apply lo: %v", err)
	}

	if guids := target.GetSubdocGUIDs(); len(guids) != 1 || guids[0] != "shared" {
		t.Fatalf("registry lost the live winner: guids=%v (want [\"shared\"])", guids)
	}
	if subs := target.GetSubdocs(); len(subs) != 1 || subs[0].GUID() != "shared" {
		t.Fatalf("GetSubdocs() lost the live winner: %+v", subs)
	}
	noDocInBothAddedAndRemoved(t, events)
}

// TestSubdocs_ConcurrentSameKeyDistinctGUID_NoPhantomEvent is the
// distinct-guid variant of the same #63 I-1 finding. The loser's embed+
// self-delete happen inside the single ApplyUpdateV1 transaction that
// decodes it, so — once registration precedes the LWW check — the add is
// cancelled in-transaction and nets to zero subdocs events for that call
// (mirrors TestSubdocs_AddThenDeleteSameTxnCancels, but here the cancel
// spans item.integrate's registration and item.delete's cancel logic rather
// than two explicit calls). Before the fix, the loser's registration ran
// after the self-delete, so it landed in Added AND Removed (and Loaded) of
// the SAME event instead of cancelling out.
func TestSubdocs_ConcurrentSameKeyDistinctGUID_NoPhantomEvent(t *testing.T) {
	parentHi := New(WithClientID(2))
	parentLo := New(WithClientID(1))
	embedSubdoc(t, parentHi, "k", New(WithGUID("doc-hi")))
	embedSubdoc(t, parentLo, "k", New(WithGUID("doc-lo")))

	updHi := EncodeStateAsUpdateV1(parentHi, nil)
	updLo := EncodeStateAsUpdateV1(parentLo, nil)

	target := New()
	var events []SubdocsEvent
	target.OnSubdocs(func(ev SubdocsEvent) { events = append(events, ev) })

	if err := ApplyUpdateV1(target, updHi, nil); err != nil {
		t.Fatalf("apply hi: %v", err)
	}
	events = events[:0] // only the second apply (the losing embed) is under test

	if err := ApplyUpdateV1(target, updLo, nil); err != nil {
		t.Fatalf("apply lo: %v", err)
	}

	if guids := target.GetSubdocGUIDs(); len(guids) != 1 || guids[0] != "doc-hi" {
		t.Fatalf("expected only the winner resident, got %v", guids)
	}
	if len(events) != 0 {
		t.Fatalf("losing concurrent embed should net to zero subdocs events (add+self-delete cancel in one txn), got %d: %+v", len(events), events)
	}
	noDocInBothAddedAndRemoved(t, events)
}
