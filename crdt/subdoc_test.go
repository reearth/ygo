package crdt

import "testing"

func TestNew_DefaultsGUIDToUUID(t *testing.T) {
	a, b := New(), New()
	if a.GUID() == "" || a.GUID() == b.GUID() || len(a.GUID()) != 36 {
		t.Fatalf("expected distinct uuid guids, got %q / %q", a.GUID(), b.GUID())
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
