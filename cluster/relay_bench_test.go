//go:build benchheavy

// Package cluster_test benchmark for issue #180 (Task 6): MemRelay fan-out.
// Measures Publish throughput fanning a sync update out to M subscriber
// sinks (BenchmarkMemRelay_Fanout/Fanout), and exercises the small-buffer +
// slow-subscriber backpressure path that #187 raised concerns about
// (BenchmarkMemRelay_Fanout/Backpressure) — MemRelay blocks rather than drops
// on a full buffer, so this measures caller-side publish timeouts against
// that blocking, not relay-internal drops; see the Backpressure doc below.
//
// Run:
//
//	go test -tags benchheavy ./cluster/ -run '^$' -bench MemRelay_Fanout -benchtime=1x -benchmem
package cluster_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// countingSink is a minimal cluster.Sink that atomically counts every
// delivered Inject call and returns immediately. MemRelay.Start already runs
// one dedicated delivery goroutine per node (memNode.run in mem_relay.go)
// that drains that node's channel and calls Sink.Inject synchronously — since
// Inject here never blocks, that goroutine drains as fast as Publish can
// enqueue, so no extra "drainer" goroutine is needed on top of what MemRelay
// already provides.
type countingSink struct {
	room     string
	received atomic.Int64
}

func newCountingSink(room string) *countingSink { return &countingSink{room: room} }

func (s *countingSink) Inject(_ context.Context, _ cluster.Inbound) error {
	s.received.Add(1)
	return nil
}

func (s *countingSink) Rooms() []string { return []string{s.room} }

func (s *countingSink) GetAwareness(string) (*awareness.Awareness, bool) { return nil, false }

func (s *countingSink) GetDoc(string) *crdt.Doc { return nil }

// benchFanoutSubscribers is the number of Sink nodes (M) started on the
// shared relay for the Fanout sub-benchmark, modelling M peers all receiving
// every Publish.
const benchFanoutSubscribers = 8

// benchFanoutDrainTimeout bounds how long the Fanout sub-benchmark waits,
// after the timed Publish loop, for every subscriber to observe all b.N
// deliveries before returning. This keeps in-flight deliveries from one
// b.N iteration from bleeding into the next sub-benchmark's fresh relay.
const benchFanoutDrainTimeout = 5 * time.Second

// BenchmarkMemRelay_Fanout groups the two relay fan-out scenarios from issue
// #180 Task 6: plain fan-out throughput (Fanout) and the backpressure path
// under a saturated small buffer + slow subscriber (Backpressure).
func BenchmarkMemRelay_Fanout(b *testing.B) {
	b.Run("Fanout", benchMemRelayFanout)
	b.Run("Backpressure", benchMemRelayBackpressure)
}

// benchMemRelayFanout starts a MemRelay with benchFanoutSubscribers counting
// sinks behind a generous buffer (so Publish never blocks on delivery — that
// backpressure path is what Backpressure covers instead), then times b.N
// Publish calls of a representative sync-update payload.
func benchMemRelayFanout(b *testing.B) {
	relay := cluster.NewMemRelay(cluster.WithBufferSize(4096))
	defer func() {
		if err := relay.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
	}()

	sinks := make([]*countingSink, benchFanoutSubscribers)
	for i := range sinks {
		sinks[i] = newCountingSink("room")
		if err := relay.Start(context.Background(), sinks[i]); err != nil {
			b.Fatalf("Start: %v", err)
		}
	}

	payload := make([]byte, 256) // representative V1 sync-update size

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := relay.Publish(context.Background(), cluster.Outbound{
			Room: "room", Kind: cluster.KindSync, Data: payload,
		}); err != nil {
			b.Fatalf("Publish: %v", err)
		}
	}
	b.StopTimer()

	// Drain (bounded): wait for every subscriber to have observed all b.N
	// deliveries so relay.Close() above tears down settled goroutines rather
	// than ones mid-delivery. Best-effort — a timeout here would mean
	// delivery is unexpectedly slow, not a benchmark failure, so it is not
	// asserted.
	want := int64(b.N)
	deadline := time.Now().Add(benchFanoutDrainTimeout)
	for _, s := range sinks {
		for time.Now().Before(deadline) && s.received.Load() < want {
			time.Sleep(time.Millisecond)
		}
	}
}

// slowSink is a cluster.Sink whose Inject stalls for a fixed delay, standing
// in for a slow/backed-up subscriber.
//
// MemRelay itself has no drop-on-full path: mem_relay.go's Publish doc is
// explicit that it "intentionally does NOT drop on full" — a full per-node
// channel makes Publish BLOCK until the node drains, ctx is cancelled, or the
// node shuts down (that blocking-not-dropping choice is exactly what #187
// raised). Since MemRelay is production code and this task must not modify
// production code, Backpressure exercises that real blocking path from the
// benchmark side instead: pairing WithBufferSize(1) with this slow sink
// saturates the node's channel almost immediately, and each Publish call is
// given a short per-call context deadline. A Publish that would otherwise
// block past that deadline returns ctx.Err() (MemRelay's Publish selects on
// ctx.Done()) and is counted as a caller-side publish-timeout here at the
// call site — i.e. this benchmark is the thing choosing not to wait, not
// MemRelay dropping anything. Real drop-on-full semantics (if ever added) are
// deferred to the #187 Redis-relay work; MemRelay has none today.
type slowSink struct {
	room  string
	delay time.Duration
}

func (s *slowSink) Inject(_ context.Context, _ cluster.Inbound) error {
	time.Sleep(s.delay)
	return nil
}

func (s *slowSink) Rooms() []string { return []string{s.room} }

func (s *slowSink) GetAwareness(string) (*awareness.Awareness, bool) { return nil, false }

func (s *slowSink) GetDoc(string) *crdt.Doc { return nil }

const (
	// benchBackpressureMessages is K, the number of Publish calls attempted
	// per b.N iteration against the saturated single-slot buffer.
	benchBackpressureMessages = 200
	// benchBackpressureSinkDelay is how long the slow sink's Inject stalls —
	// long enough that, combined with a 1-slot buffer, most Publish calls
	// below hit their deadline instead of enqueuing.
	benchBackpressureSinkDelay = 20 * time.Millisecond
	// benchBackpressurePublishDeadline is the per-Publish context timeout
	// that stands in for a bounded-wait caller giving up on a full buffer.
	benchBackpressurePublishDeadline = 2 * time.Millisecond
)

// benchMemRelayBackpressure starts a fresh MemRelay(WithBufferSize(1)) with
// one slowSink per b.N iteration, publishes benchBackpressureMessages
// messages each under a short deadline, and reports "publish-timeouts"
// (Publish calls that hit their caller-side deadline against the full
// buffer) and "published" (K) via b.ReportMetric.
func benchMemRelayBackpressure(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		relay := cluster.NewMemRelay(cluster.WithBufferSize(1))
		sink := &slowSink{room: "room", delay: benchBackpressureSinkDelay}
		if err := relay.Start(context.Background(), sink); err != nil {
			b.Fatalf("Start: %v", err)
		}

		var published, publishTimeouts int64
		for j := 0; j < benchBackpressureMessages; j++ {
			ctx, cancel := context.WithTimeout(context.Background(), benchBackpressurePublishDeadline)
			err := relay.Publish(ctx, cluster.Outbound{
				Room: "room", Kind: cluster.KindSync, Data: []byte{byte(j)},
			})
			cancel()
			published++
			if err != nil {
				publishTimeouts++
			}
		}

		if err := relay.Close(); err != nil {
			b.Fatalf("Close: %v", err)
		}
		// relay.Close only signals shutdown (mem_relay.go's Close doc: it
		// closes the relay-level done channel but never the per-node
		// channel, precisely so a concurrent send can't panic). At loop end
		// the node is often mid-Inject AND still holding one queued item
		// (buffer size 1), so up to ~2×benchBackpressureSinkDelay of work can
		// remain when Close returns. This sleep is a probabilistic bound, not
		// a guaranteed join (MemRelay exposes no WaitGroup/join API) — it
		// relies on iteration isolation (each b.N iteration gets its own
		// relay/sink) plus generous per-iteration wall time for the run()
		// goroutine to actually exit before the next iteration or the
		// benchmark proceeds, so no goroutine leaks pile up across iterations
		// or sub-benchmarks.
		time.Sleep(2*benchBackpressureSinkDelay + 10*time.Millisecond)

		// These are CALLER-SIDE context-deadline timeouts against MemRelay's
		// blocking backpressure, NOT relay-internal drops: MemRelay has no
		// drop-on-full path (mem_relay.go's Publish doc: "intentionally does
		// NOT drop on full"). Real drop-on-full semantics are deferred to the
		// #187 Redis-relay work. Metric name reflects that deliberately.
		b.ReportMetric(float64(publishTimeouts), "publish-timeouts")
		b.ReportMetric(float64(published), "published")
	}
}
