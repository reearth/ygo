// Package redis is a Redis-backed implementation of cluster.Relay, intended
// for multi-process ygo deployments where two or more websocket.Server
// instances behind a load balancer need to share one logical document per
// room (#62).
//
// # Model
//
// One Redis pub/sub channel per room (default name format
// "ygo:cluster:<room>"). Each node subscribes to the channels for its active
// rooms via RoomActivated and unsubscribes via RoomDeactivated, so a node
// only receives traffic for rooms it actually hosts. Publish encodes the
// Outbound (prefixed with this node's stable nodeID) and PUBLISHes it on the
// room's channel; the subscriber goroutine on every node decodes the inbound
// and — for non-self payloads — hands it to Sink.Inject, which the websocket
// provider applies with the relay sentinel origin so the local observer drops
// the echo.
//
// # Self-delivery
//
// Redis pub/sub mirrors every publish back to the publisher's own
// subscription. The wire format carries a per-relay nodeID; the subscriber
// drops payloads whose nodeID matches its own, so the local node never pays
// the decode + Inject + observer round trip for its own writes.
//
// # Delivery guarantee
//
// Redis pub/sub is fire-and-forget. A node that subscribes AFTER a publish
// will not receive that publish — there is no replay. ygo's existing
// versioned-persistence layer (provider/persistence) is the right place for
// late-joiner catch-up: a node that comes online late loads the head state
// from persistence and only THEN starts relying on the relay for incremental
// updates. The Redis relay does not attempt to provide at-least-once
// semantics; if a deployment needs that, Redis Streams or a different bus
// would replace this adapter.
//
// # Echo guard
//
// The relay package contract is "Origin is observer-local and never crosses
// the wire" (see cluster/relay.go); this adapter honours that. Inbound
// payloads are handed to Sink.Inject which the websocket provider applies
// with its own per-server sentinel; the local observer then drops the
// re-publish via pointer-identity. The self-delivery skip above is the
// adapter's own first-line defence, but the sentinel guard is the
// authoritative one.
//
// # Wire format (v1.21.0)
//
//	VarBytes(nodeID) + VarUint(kind) + VarString(room) + VarBytes(data)
//
// # Usage
//
//	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
//	relay, _ := ygoredis.New(rdb, ygoredis.Config{ChannelPrefix: "ygo:cluster:"})
//	defer relay.Close()
//
//	srv := ygws.NewServer()
//	_ = srv.AttachRelay(relay)
package redis

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	goredis "github.com/redis/go-redis/v9"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/encoding"
)

// DefaultChannelPrefix is prepended to the room name to form the pub/sub
// channel for that room. Override via Config.ChannelPrefix if multiple
// independent ygo deployments share one Redis instance.
const DefaultChannelPrefix = "ygo:cluster:"

// DefaultOutboundBuffer is the capacity of the internal queue that
// decouples Publish (called from doc.OnUpdate, on the Transact path) from
// the actual Redis PUBLISH RPC. Override via Config.OutboundBuffer when
// expected per-room update bursts exceed the default.
const DefaultOutboundBuffer = 256

// DefaultChannelSize is the capacity of the in-memory channel go-redis
// feeds inbound pub/sub messages onto. Override via Config.ChannelSize for
// rooms with bursty traffic; go-redis logs and drops messages when this
// buffer fills, which would manifest as silent divergence between nodes
// until the next persistence catch-up.
const DefaultChannelSize = 1024

// nodeIDLen is the length in bytes of the per-relay nodeID used for
// self-delivery suppression. 16 bytes is overkill for collision avoidance
// across any plausible cluster size but keeps the wire framing tidy.
const nodeIDLen = 16

// Errors returned by the relay. Callers should compare with errors.Is.
var (
	// ErrRelayClosed is returned by Publish / Start after Close.
	ErrRelayClosed = errors.New("cluster/redis: relay closed")
	// ErrRelayNotStarted is returned by Publish before Start has bound a
	// Sink and the subscriber/publisher goroutines are running.
	ErrRelayNotStarted = errors.New("cluster/redis: relay not started")
	// ErrNilClient is returned by New when the supplied *redis.Client is
	// nil. Construct a client with redis.NewClient(...) and pass it in.
	ErrNilClient = errors.New("cluster/redis: nil redis client")
	// ErrNilSink is returned by Start when the supplied Sink is nil.
	ErrNilSink = errors.New("cluster/redis: nil sink")
	// ErrSinkMismatch is returned by Start when called a second time with a
	// different Sink than the first. A Relay binds to one Sink for its
	// lifetime.
	ErrSinkMismatch = errors.New("cluster/redis: relay already started with a different sink")
)

// Config configures a Relay. All fields are optional.
type Config struct {
	// ChannelPrefix is prepended to the room name to form the per-room
	// pub/sub channel. Empty uses DefaultChannelPrefix. Use a deployment-
	// specific prefix when multiple independent ygo clusters share one
	// Redis instance (e.g. "ygo:prod:" vs "ygo:staging:") so cross-talk
	// is impossible.
	ChannelPrefix string

	// OutboundBuffer is the internal Publish→PUBLISH queue capacity. Zero
	// uses DefaultOutboundBuffer. When the queue is full, Publish blocks
	// until a slot frees, the context cancels, or the relay closes —
	// matching cluster.MemRelay's bounded-publish back-pressure semantics.
	OutboundBuffer int

	// ChannelSize is the capacity of go-redis's in-memory inbound channel
	// (see redis.WithChannelSize). Zero uses DefaultChannelSize. When this
	// channel fills, go-redis logs and DROPS messages — which for CRDT
	// updates means silent inter-node divergence until persistence
	// catch-up. Size this generously for bursty rooms.
	ChannelSize int

	// Logger receives Warn-level entries when a delivery fails (decode
	// error, sink.Inject error, etc.). nil falls back to slog.Default().
	Logger *slog.Logger

	// NodeID is the per-relay identifier used to suppress self-delivery
	// (Redis pub/sub mirrors every publish back to the publisher's own
	// subscription; this lets us drop those before sink.Inject). Empty
	// (the usual case) auto-generates 16 crypto-random bytes. Provide a
	// stable value for tests that need deterministic identity.
	NodeID []byte
}

// Relay is the Redis-backed cluster.Relay.
type Relay struct {
	client   *goredis.Client
	prefix   string
	log      *slog.Logger
	nodeID   []byte
	chanSize int

	// outbound carries Publish calls to the publisher goroutine. A bounded
	// channel back-pressures the caller, matching MemRelay.
	outbound chan cluster.Outbound

	// done is closed once by Close to signal all goroutines to exit.
	done chan struct{}

	wg sync.WaitGroup

	// mu serializes lifecycle transitions (Start/Close) and room-membership
	// ops (RoomActivated/RoomDeactivated). Holding mu across the underlying
	// Redis SUBSCRIBE/UNSUBSCRIBE RPC is what prevents the TOCTOU race in
	// which two concurrent calls for the same room could reorder the RPCs.
	// Publish does NOT take mu — it uses the started/closed atomics + the
	// done/outbound channels (the publisher hot path must not be gated on
	// a lock that low-frequency lifecycle ops can hold).
	mu          sync.Mutex
	started     atomic.Bool // released after r.sink / r.pubSub / r.startCtx are committed
	closed      atomic.Bool // set under mu in Close
	sink        cluster.Sink
	pubSub      *goredis.PubSub
	startCtx    context.Context //nolint:containedctx // captured intentionally: Publish blocks on its Done so callers don't hang after Shutdown
	activeRooms map[string]int

	closeOnce sync.Once
	closeErr  error
}

// Compile-time assertion: *Relay satisfies cluster.Relay.
var _ cluster.Relay = (*Relay)(nil)

// New constructs a Redis-backed Relay using the supplied client and config.
// The client is owned by the caller — Close on the Relay does NOT close the
// client (callers usually share the client across the application). Returns
// ErrNilClient if client is nil.
func New(client *goredis.Client, cfg Config) (*Relay, error) {
	if client == nil {
		return nil, ErrNilClient
	}
	if cfg.ChannelPrefix == "" {
		cfg.ChannelPrefix = DefaultChannelPrefix
	}
	if cfg.OutboundBuffer <= 0 {
		cfg.OutboundBuffer = DefaultOutboundBuffer
	}
	if cfg.ChannelSize <= 0 {
		cfg.ChannelSize = DefaultChannelSize
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	nodeID := cfg.NodeID
	if len(nodeID) == 0 {
		nodeID = make([]byte, nodeIDLen)
		if _, err := rand.Read(nodeID); err != nil {
			return nil, fmt.Errorf("cluster/redis: generate node id: %w", err)
		}
	} else {
		// Defensive copy so the caller can't mutate it post-hoc.
		nodeID = append([]byte(nil), nodeID...)
	}

	return &Relay{
		client:      client,
		prefix:      cfg.ChannelPrefix,
		log:         cfg.Logger,
		nodeID:      nodeID,
		chanSize:    cfg.ChannelSize,
		outbound:    make(chan cluster.Outbound, cfg.OutboundBuffer),
		done:        make(chan struct{}),
		activeRooms: make(map[string]int),
	}, nil
}

// channelFor returns the Redis channel name for a given room.
func (r *Relay) channelFor(room string) string {
	return r.prefix + room
}

// NodeID returns a copy of this relay's nodeID. Exposed for diagnostic /
// test purposes; production callers shouldn't need it.
func (r *Relay) NodeID() []byte {
	return append([]byte(nil), r.nodeID...)
}

// Start binds the relay to a Sink and starts the subscriber + publisher
// goroutines. Called exactly once by Server.AttachRelay. Subsequent calls
// with the same Sink are no-ops; calls with a different Sink return
// ErrSinkMismatch.
//
// Start holds r.mu for the full duration so it cannot race with Close:
// either Start runs to completion and Close then tears down a fully-formed
// relay, or Close runs first and Start returns ErrRelayClosed.
func (r *Relay) Start(ctx context.Context, sink cluster.Sink) error {
	if sink == nil {
		return ErrNilSink
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed.Load() {
		return ErrRelayClosed
	}
	if r.started.Load() {
		if r.sink != sink {
			return ErrSinkMismatch
		}
		return nil
	}

	r.sink = sink
	r.startCtx = ctx
	r.pubSub = r.client.Subscribe(ctx)

	r.wg.Add(2)
	go r.runSubscriber(ctx)
	go r.runPublisher(ctx)

	// started is set LAST: the atomic Store acts as a release barrier so
	// any goroutine that observes started=true via Load() sees the writes
	// to sink/pubSub/startCtx that preceded it.
	r.started.Store(true)
	return nil
}

// runPublisher drains the outbound queue and PUBLISHes each Outbound to its
// room's channel. Errors are logged at Warn (Redis transient hiccups are
// expected to recover; we don't want to crash the dispatcher).
func (r *Relay) runPublisher(ctx context.Context) {
	defer r.wg.Done()
	for {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		case out := <-r.outbound:
			body := encodeOutbound(r.nodeID, out)
			if err := r.client.Publish(ctx, r.channelFor(out.Room), body).Err(); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				r.log.Warn("cluster/redis: PUBLISH failed",
					"room", out.Room, "kind", out.Kind, "err", err)
			}
		}
	}
}

// runSubscriber reads from the PubSub channel and dispatches each message
// to Sink.Inject — after skipping self-published payloads via nodeID
// comparison and re-checking the closed flag (so a message buffered at the
// moment Close fires does not leak past Close).
func (r *Relay) runSubscriber(ctx context.Context) {
	defer r.wg.Done()
	ch := r.pubSub.Channel(goredis.WithChannelSize(r.chanSize))
	for {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return // PubSub closed
			}
			// H1: prefer Close — Go's select is pseudo-random when multiple
			// cases are ready, so a buffered msg could fire even after
			// r.done was closed. Re-check before any user-visible effect.
			if r.closed.Load() {
				return
			}
			srcNodeID, room, kind, data, err := decodeInbound([]byte(msg.Payload))
			if err != nil {
				r.log.Warn("cluster/redis: decodeInbound failed; drop",
					"channel", msg.Channel, "err", err)
				continue
			}
			// H2: drop self-deliveries before any further work.
			if bytes.Equal(srcNodeID, r.nodeID) {
				continue
			}
			if err := r.sink.Inject(ctx, cluster.Inbound{
				Room: room, Kind: kind, Data: data,
			}); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				r.log.Warn("cluster/redis: sink.Inject failed",
					"room", room, "kind", kind, "err", err)
			}
		}
	}
}

// Publish hands an Outbound to the publisher goroutine. Blocks if the
// internal buffer is full (back-pressuring the caller — same contract as
// MemRelay). Returns ErrRelayClosed if Close has been called or the
// relay's bound context has been cancelled; ErrRelayNotStarted if Start
// has not been called.
//
// Publish does NOT acquire r.mu: the started/closed atomics combined with
// the done/startCtx channels give it all the ordering it needs, and the
// hot path must not be serialised against lifecycle/room-membership ops.
func (r *Relay) Publish(ctx context.Context, out cluster.Outbound) error {
	if r.closed.Load() {
		return ErrRelayClosed
	}
	if !r.started.Load() {
		return ErrRelayNotStarted
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Safe to read r.startCtx unlocked: started.Store(true) happens-after
	// the startCtx write in Start, so the started.Load()==true above acts
	// as the matching acquire.
	startCtx := r.startCtx
	select {
	case r.outbound <- out:
		return nil
	case <-r.done:
		return ErrRelayClosed
	case <-startCtx.Done():
		// H3: the relay's bound context is cancelled (e.g. Server.Shutdown).
		// runPublisher has stopped draining outbound; surface this to the
		// caller as a clean close rather than hanging forever.
		return ErrRelayClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RoomActivated subscribes to the room's pub/sub channel. Calls are
// idempotent and reference-counted: a duplicate RoomActivated increments a
// counter but performs no Redis call; RoomDeactivated decrements and
// unsubscribes only when the count reaches zero.
//
// The Redis SUBSCRIBE RPC is held under r.mu (see C2 in the v1.21.0 review)
// so two concurrent Activate/Deactivate calls for the same room cannot
// reorder the underlying RPCs and leave the channel subscribed with a
// zero refcount.
func (r *Relay) RoomActivated(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started.Load() || r.closed.Load() {
		return
	}

	r.activeRooms[room]++
	if r.activeRooms[room] > 1 {
		return // already subscribed
	}
	if err := r.pubSub.Subscribe(context.Background(), r.channelFor(room)); err != nil {
		r.log.Warn("cluster/redis: SUBSCRIBE failed", "room", room, "err", err)
	}
}

// RoomDeactivated unsubscribes from the room's pub/sub channel. See the
// RoomActivated godoc for idempotency / reference-counting / locking
// semantics.
func (r *Relay) RoomDeactivated(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started.Load() || r.closed.Load() {
		return
	}
	if r.activeRooms[room] <= 0 {
		return
	}

	r.activeRooms[room]--
	count := r.activeRooms[room]
	if count == 0 {
		delete(r.activeRooms, room)
	}
	if count > 0 {
		return // still active elsewhere on this node
	}
	if err := r.pubSub.Unsubscribe(context.Background(), r.channelFor(room)); err != nil {
		r.log.Warn("cluster/redis: UNSUBSCRIBE failed", "room", room, "err", err)
	}
}

// Close stops the relay. Idempotent. Does NOT close the underlying
// *redis.Client — clients are commonly shared across the application and
// owned by the caller.
//
// Close blocks until any in-flight Start has committed (it acquires r.mu)
// and all background goroutines have exited (wg.Wait). This is what makes
// the relay safe under racy callers: there is no window in which the
// publisher/subscriber can fire after Close returns.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		// Lock-protected handshake: if Start is mid-flight, we wait for it
		// to commit before reading r.pubSub. If Start hasn't started yet,
		// we set closed=true here and Start will see it under the same mu.
		r.mu.Lock()
		r.closed.Store(true)
		pubSub := r.pubSub
		r.mu.Unlock()

		close(r.done)
		if pubSub != nil {
			if err := pubSub.Close(); err != nil {
				r.closeErr = fmt.Errorf("cluster/redis: PubSub close: %w", err)
			}
		}
		r.wg.Wait()
	})
	return r.closeErr
}

// encodeOutbound serialises an Outbound into the wire format documented at
// the top of this file: VarBytes(nodeID) + VarUint(kind) + VarString(room)
// + VarBytes(data). Origin is observer-local and intentionally not encoded.
func encodeOutbound(nodeID []byte, out cluster.Outbound) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarBytes(nodeID)
		enc.WriteVarUint(uint64(out.Kind))
		enc.WriteVarString(out.Room)
		enc.WriteVarBytes(out.Data)
	})
}

// decodeInbound is the inverse of encodeOutbound. The returned nodeID is
// used by runSubscriber to suppress self-delivery before any further work.
//
// The data sub-slice aliases the input buffer; the caller passes a fresh
// []byte(msg.Payload) (Go's string→[]byte conversion always copies) so the
// alias is safe to retain — no additional copy needed here.
func decodeInbound(b []byte) (nodeID []byte, room string, kind cluster.Kind, data []byte, err error) {
	dec := encoding.NewDecoder(b)
	nid, err := dec.ReadVarBytes()
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read nodeID: %w", err)
	}
	k, err := dec.ReadVarUint()
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read kind: %w", err)
	}
	roomStr, err := dec.ReadVarString()
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read room: %w", err)
	}
	payload, err := dec.ReadVarBytes()
	if err != nil {
		return nil, "", 0, nil, fmt.Errorf("read data: %w", err)
	}
	return nid, roomStr, cluster.Kind(k), payload, nil
}
