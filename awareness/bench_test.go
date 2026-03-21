package awareness_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/reearth/ygo/awareness"
)

// BenchmarkSetLocalState measures SetLocalState called repeatedly.
// Each call increments the internal clock, so each iteration is a genuine
// update (not a no-op).
func BenchmarkSetLocalState(b *testing.B) {
	a := awareness.New(1)
	state := map[string]any{"cursor": 0, "user": "alice"}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		state["cursor"] = i
		a.SetLocalState(state)
	}
}

// BenchmarkEncodeUpdate_Single measures encoding an awareness update for a
// single client.
func BenchmarkEncodeUpdate_Single(b *testing.B) {
	a := awareness.New(1)
	a.SetLocalState(map[string]any{"cursor": 42, "user": "alice"})

	ids := []uint64{1}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = a.EncodeUpdate(ids)
	}
}

// BenchmarkEncodeUpdate_Many measures encoding an awareness update for 50
// clients, simulating a large collaborative room.
func BenchmarkEncodeUpdate_Many(b *testing.B) {
	const numClients = 50

	// Build a single Awareness instance that knows about 50 peers.
	// Client 1 is the "local" instance; clients 2..50 are applied via updates.
	hub := awareness.New(1)
	hub.SetLocalState(map[string]any{"cursor": 0, "user": "client-1"})

	ids := make([]uint64, numClients)
	ids[0] = 1
	for i := 2; i <= numClients; i++ {
		peer := awareness.New(uint64(i))
		peer.SetLocalState(map[string]any{
			"cursor": i * 10,
			"user":   fmt.Sprintf("client-%d", i),
		})
		update := peer.EncodeUpdate(nil)
		if err := hub.ApplyUpdate(update, nil); err != nil {
			b.Fatal(err)
		}
		ids[i-1] = uint64(i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_ = hub.EncodeUpdate(ids)
	}
}

// BenchmarkApplyUpdate_Single measures applying an incoming awareness update
// from a single client.
func BenchmarkApplyUpdate_Single(b *testing.B) {
	// Build the update payload once outside the timed loop.
	sender := awareness.New(99)
	sender.SetLocalState(map[string]any{"cursor": 7, "color": "#abc"})
	update := sender.EncodeUpdate(nil)

	// The receiver needs a fresh clock-state for client 99 each iteration so
	// the incoming clock (1) is always strictly greater than what it holds.
	// We achieve this by creating a new receiver per iteration inside the loop,
	// which is cheap (empty map).

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		recv := awareness.New(1)
		if err := recv.ApplyUpdate(update, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyUpdate_Many measures applying a 50-client awareness update.
func BenchmarkApplyUpdate_Many(b *testing.B) {
	const numClients = 50

	// Build a hub that knows about all 50 clients, then encode all of them.
	hub := awareness.New(1)
	hub.SetLocalState(map[string]any{"cursor": 0})
	for i := 2; i <= numClients; i++ {
		peer := awareness.New(uint64(i))
		peer.SetLocalState(map[string]any{"cursor": i})
		if err := hub.ApplyUpdate(peer.EncodeUpdate(nil), nil); err != nil {
			b.Fatal(err)
		}
	}
	bigUpdate := hub.EncodeUpdate(nil) // encodes all 50 clients

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		// Fresh receiver each time so every client ID is new (clock 0 < 1).
		recv := awareness.New(999)
		if err := recv.ApplyUpdate(bigUpdate, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRemoveExpired measures RemoveExpired on an Awareness with 50 clients
// all of whose last-update timestamps are old enough to be expired.
func BenchmarkRemoveExpired(b *testing.B) {
	const numClients = 50

	// Helper that builds a fully-populated, all-expired awareness instance.
	// We re-build it each iteration because RemoveExpired mutates state.
	build := func() *awareness.Awareness {
		a := awareness.New(1)
		a.SetLocalState(map[string]any{"x": 0})
		for i := 2; i <= numClients; i++ {
			peer := awareness.New(uint64(i))
			peer.SetLocalState(map[string]any{"x": i})
			if err := a.ApplyUpdate(peer.EncodeUpdate(nil), nil); err != nil {
				b.Fatal(err)
			}
		}
		return a
	}

	// Pre-build one instance to warm up, not measured.
	_ = build()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a := build()
		b.StartTimer()

		// timeout=0 expires every client immediately.
		a.RemoveExpired(0 * time.Second)
	}
}
