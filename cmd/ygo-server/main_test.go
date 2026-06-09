package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/persistence/sqlite"
)

// Persistence survives a simulated restart: store an update via the same
// LegacyAdapter+sqlite the binary wires, close, reopen, and load it back.
func TestServer_PersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "srv.db")
	// Build a valid V1 update for room "doc".
	d := crdt.New(crdt.WithClientID(1))
	txt := d.GetText("t")
	d.Transact(func(tx *crdt.Transaction) { txt.Insert(tx, 0, "hi", nil) })
	upd := crdt.EncodeStateAsUpdateV1(d, nil)

	s1, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	a1 := persistence.NewLegacyAdapter(s1)
	if err := a1.StoreUpdate("doc", upd); err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen (simulated restart) and load.
	s2, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	a2 := persistence.NewLegacyAdapter(s2)
	loaded, err := a2.LoadDoc("doc")
	if err != nil || loaded == nil {
		t.Fatalf("loaddoc: blob=%v err=%v", loaded, err)
	}
	dd := crdt.New()
	if err := crdt.ApplyUpdateV1(dd, loaded, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := dd.GetText("t").ToString(); got != "hi" {
		t.Fatalf("restored text = %q want %q", got, "hi")
	}
}

// run() must start, serve, and shut down cleanly on context cancel.
func TestRun_StartsAndShutsDown(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:0", Store: filepath.Join(t.TempDir(), "s.db"), MaxMessageBytes: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	ready := make(chan struct{})
	go func() { errCh <- run(ctx, cfg, ready) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not shut down")
	}
}

// run() with an empty Store opens the ephemeral in-memory backend (no
// NewServer/no-persistence branch): it must wire, become ready, and close the
// store cleanly on cancel.
func TestRun_InMemoryStore_StartsAndShutsDown(t *testing.T) {
	cfg := Config{Addr: "127.0.0.1:0", Store: "", MaxMessageBytes: 1 << 20}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	ready := make(chan struct{})
	go func() { errCh <- run(ctx, cfg, ready) }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not shut down")
	}
}
