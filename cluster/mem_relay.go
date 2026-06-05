package cluster

import (
	"context"
	"errors"
	"sync"
)

// ErrRelayClosed is returned by MemRelay.Publish / Start after Close.
var ErrRelayClosed = errors.New("cluster: relay closed")

// ErrRelayNotStarted is returned by MemRelay.Publish before any node has
// Started (no Sink is bound yet), and by Start when sink is nil. MemRelay
// supports multiple Start calls (one per node) and does not reject subsequent
// ones.
var ErrRelayNotStarted = errors.New("cluster: relay not started")

// MemRelay is an in-process, channel-backed Relay. It is the reference
// implementation: multiple nodes (each a websocket.Server) share one MemRelay
// instance; a change Published by one node is delivered to every node's Sink
// (including the publisher, which drops its own change via the echo sentinel).
// It is safe for concurrent use and primarily intended for tests and
// single-process multi-server simulations.
//
// Delivery is asynchronous: Publish enqueues to a per-node buffered channel
// drained by a goroutine started in Start. Each node's deliveries are processed
// in order; deliveries to different nodes proceed independently.
type MemRelay struct {
	bufSize int

	mu     sync.Mutex
	nodes  []*memNode // every node that has called Start
	closed bool
	// done is closed exactly once by Close. Delivery goroutines and in-flight
	// Publish sends select on it so they unwind without ever closing the
	// per-node channels (n.ch) — closing those would race a concurrent send
	// and panic ("send on closed channel").
	done chan struct{}
}

// memNode is one Start-ed Sink with its own delivery queue and goroutine.
type memNode struct {
	sink  Sink
	ch    chan Inbound
	ctx   context.Context
	relay *MemRelay
}

// MemRelayOption configures a MemRelay.
type MemRelayOption func(*MemRelay)

// WithBufferSize sets the per-node delivery channel capacity. Values < 1 use
// the default (256).
func WithBufferSize(n int) MemRelayOption {
	return func(m *MemRelay) {
		if n >= 1 {
			m.bufSize = n
		}
	}
}

// NewMemRelay returns a started-on-demand in-process relay.
func NewMemRelay(opts ...MemRelayOption) *MemRelay {
	m := &MemRelay{bufSize: 256, done: make(chan struct{})}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start registers sink as a node and begins delivering inbound changes to it.
// Each distinct node (websocket.Server) calls Start once on the shared relay.
func (m *MemRelay) Start(ctx context.Context, sink Sink) error {
	if sink == nil {
		return ErrRelayNotStarted
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrRelayClosed
	}
	n := &memNode{
		sink:  sink,
		ch:    make(chan Inbound, m.bufSize),
		ctx:   ctx,
		relay: m,
	}
	m.nodes = append(m.nodes, n)
	m.mu.Unlock()

	go n.run()
	return nil
}

// run drains the node's delivery queue until ctx is cancelled or the relay is
// Closed. The per-node channel (n.ch) is never closed; the goroutine instead
// exits on n.ctx or the relay-level done signal, so a concurrent Publish send
// can never race a channel close.
func (n *memNode) run() {
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-n.relay.done:
			return
		case in := <-n.ch:
			// Best-effort: a Sink.Inject error (e.g. room evicted between
			// publish and delivery) must not stall delivery to other events
			// or panic the goroutine. There is no caller to surface it to.
			_ = n.sink.Inject(n.ctx, in)
		}
	}
}

// Publish fans the change out to every registered node.
//
// MemRelay does not know which node originated the change, so it delivers to
// all nodes including the publisher. Correctness of the "no echo" property does
// not depend on excluding the publisher: inbound changes are applied with the
// relay sentinel origin, and the provider wiring drops sentinel-origin
// observations before they reach Publish. A publisher therefore re-injects its
// own change idempotently (CRDT updates are commutative/idempotent) and does
// NOT re-publish it, so no loop forms. To avoid even that redundant
// self-delivery, the provider wiring should not Publish changes that arrived
// via Inject — which the sentinel guard already enforces.
//
// Origin is observer-local and is not delivered (Inbound has no Origin field).
//
// Backpressure: delivery is via a bounded per-node channel. When a node's
// channel is FULL, Publish BLOCKS until the node drains, ctx is cancelled, or
// the node shuts down — and because Publish is called from the doc.OnUpdate
// observer, a full channel back-pressures the publishing node's Transact
// caller. MemRelay intentionally does NOT drop on full: it is an in-process
// reference relay, not a real transport. A production relay over a message bus
// is where drop-on-full (or persistent buffering) belongs; MemRelay favours
// lossless delivery and lets the (large) WithBufferSize absorb bursts.
func (m *MemRelay) Publish(ctx context.Context, out Outbound) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrRelayClosed
	}
	if len(m.nodes) == 0 {
		m.mu.Unlock()
		return ErrRelayNotStarted
	}
	targets := make([]*memNode, len(m.nodes))
	copy(targets, m.nodes)
	m.mu.Unlock()

	in := Inbound{Room: out.Room, Kind: out.Kind, Data: out.Data}
	for _, n := range targets {
		select {
		case n.ch <- in:
		case <-m.done:
			// Relay Closed mid-publish. n.ch is never closed, so the send
			// above can never panic; we just stop fanning out and report it.
			return ErrRelayClosed
		case <-ctx.Done():
			return ctx.Err()
		case <-n.ctx.Done():
			// node shutting down; skip it
		}
	}
	return nil
}

// RoomActivated is a no-op for MemRelay: it has no per-room subscription model,
// every node receives every room's traffic and applies only rooms it hosts.
func (m *MemRelay) RoomActivated(string) {}

// RoomDeactivated is a no-op for MemRelay. See RoomActivated.
func (m *MemRelay) RoomDeactivated(string) {}

// Close stops all node-delivery goroutines and rejects further Publish/Start.
//
// Close signals shutdown by closing the relay-level done channel exactly once;
// it deliberately does NOT close the per-node delivery channels (n.ch). Closing
// n.ch would race a concurrent Publish send and panic ("send on closed
// channel"). Instead, run() and Publish both select on done and unwind. A send
// that wins the race against done merely buffers an item nobody drains — benign,
// and the node goroutine has already exited so there is no leak.
func (m *MemRelay) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.nodes = nil
	close(m.done)
	m.mu.Unlock()
	return nil
}
