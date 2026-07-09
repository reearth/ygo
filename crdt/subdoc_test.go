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
