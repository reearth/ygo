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
