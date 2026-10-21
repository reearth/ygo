package client

import (
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// TestHarness_ConnectSynced_WaitsForInitialSyncedEvent pins the harness
// contract the reconnect tests rely on: once connectSynced returns, the
// initial StateSynced has been delivered, so a StateSynced waiter armed
// afterwards can only be released by a later handshake.
//
// markSynced closes Synced() before emitStatus snapshots its subscribers. The
// test holds statusMu across that gap, so the snapshot cannot happen until
// connectSynced has had every chance to return early.
func TestHarness_ConnectSynced_WaitsForInitialSyncedEvent(t *testing.T) {
	_, ts := startServer(t)
	c, err := New(Options{URL: wsURL(ts, "harness-synced"), Doc: crdt.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var holdOnce sync.Once
	held := make(chan struct{})
	unsub := c.OnStatus(func(s Status) {
		if s.State == StateConnected {
			holdOnce.Do(func() {
				c.statusMu.Lock()
				close(held)
			})
		}
	})
	t.Cleanup(unsub)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		connectSynced(t, c)
	}()

	<-held
	<-c.Synced()
	select {
	case <-returned:
		c.statusMu.Unlock()
		t.Fatal("connectSynced returned before the initial StateSynced was delivered")
	case <-time.After(200 * time.Millisecond):
	}
	c.statusMu.Unlock()
	<-returned
}
