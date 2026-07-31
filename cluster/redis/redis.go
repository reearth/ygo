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
	"github.com/reearth/ygo/internal/relaylane"
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

	// RoomQueueSize bounds how many inbound KindSync updates a single room
	// may have queued for delivery before the relay starts coalescing them
	// (merging the backlog into one update via crdt.MergeUpdatesV1). Zero
	// uses relaylane.DefaultCap. Coalescing never loses an edit — it trades
	// per-update delivery granularity for bounded memory on a wedged room.
	RoomQueueSize int
}

// Stats is a point-in-time snapshot of the relay's inbound degraded-path
// counters, summed across every room worker this relay has ever had — both
// currently live ones and ones since retired by RoomDeactivated (their final
// counts are folded into the relay's running total before the worker is
// discarded; see stopWorker). Every field here is therefore monotonically
// non-decreasing for the life of the Relay: a room deactivating (e.g. via
// idle eviction) never causes Stats() to go backwards, which matters because
// operators are expected to alert on these with a Prometheus-style rate() /
// increase() — a decrease reads as a counter reset and silently discards the
// delta across it.
//
// Coalesced going non-zero is routine, not alarming by itself: it increments
// on every ordinary drain merge (2+ blobs queued between drains), which a
// busy room hits far below its capacity, not only on the over-cap collapse.
// Alert on its RATE trending up, not on its mere presence. HardDrops going
// non-zero means data was lost and nodes may be diverged — alert on that by
// presence; it should always be zero. RouterDrops going non-zero is also
// routine (see its own doc) — alert on its rate, not its presence.
type Stats struct {
	// Coalesced counts inbound KindSync updates absorbed into another blob
	// by a merge. Expected to be non-zero on busy rooms; see the type doc.
	Coalesced uint64
	// AwarenessSuperseded counts awareness blobs replaced before delivery.
	// Benign: awareness is idempotent heartbeat state.
	AwarenessSuperseded uint64
	// HardDrops counts payloads lost outright. Should always be zero.
	HardDrops uint64
	// RouterDrops counts inbound messages the subscriber router discarded
	// because it found no live worker for the room (workerForInbound
	// missed) — a straggler for a room this node no longer hosts, or one
	// arriving inside RoomActivated's own increment-then-create window.
	// This is a routine, expected event under ordinary room churn (rooms
	// deactivating while a message is already in flight), not a data-loss
	// signal: a message from a departed room reaching this node too late to
	// matter is not a correctness problem in the way HardDrops is. Alert on
	// its rate trending up (e.g. relative to churn volume), not its mere
	// presence.
	RouterDrops uint64
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

	// workers holds one delivery worker per room. Guarded by workersMu, which
	// is separate from mu: the router takes it on the hot path and must not
	// contend with lifecycle ops (in particular it must never take r.mu,
	// which the Redis SUBSCRIBE/UNSUBSCRIBE RPCs hold — see mu's doc above).
	// A worker is created on RoomActivated, before the SUBSCRIBE RPC, so a
	// message that arrives the instant the broker has us subscribed finds a
	// live lane. The router (workerForInbound) does NOT create workers on a
	// miss: it is a pure hit-or-drop lookup. Sink.Inject's non-resident-room
	// auto-create guarantee is preserved by that pre-creation in
	// RoomActivated, not by anything the router itself checks — so a
	// message for a room this relay has activated is delivered even if the
	// Sink has never heard of it; a message for a room this relay has not
	// (or no longer) activated finds no worker and is dropped (counted in
	// routerDrops), the same acceptable-drop class as the self-delivery drop
	// in runSubscriber.
	//
	// retired accumulates the Coalesced/AwarenessSuperseded/HardDrops of
	// every worker stopWorker has ever retired, folded in at retirement time
	// while workersMu is already held (see stopWorker) — otherwise a
	// deactivated room's counters would simply vanish from Stats(), letting
	// the sum go backwards. Stats() also holds workersMu across its entire
	// computation (not just this map's snapshot), which is required for
	// monotonicity — see Stats()'s doc for the three-call race that a
	// narrower lock scope leaves open.
	//
	// One narrow, deliberately accepted gap remains even with that fix: if
	// the router is mid-flight with a *stale* worker reference obtained from
	// workerForInbound just before stopWorker removes it, and that Push
	// (with its own possible coalesce) lands strictly after stopWorker's
	// snapshot, that increment is lost for good — invisible to every Stats()
	// call from then on, since the lane is no longer reachable from
	// r.workers and its value was already folded into retired before the
	// push landed. This makes Stats() an UNDERcount in that narrow window,
	// never an OVERcount and — because the lost increment was never
	// observed by any earlier call either — never a decrease relative to
	// one. Monotonic: guaranteed. Exact: not guaranteed.
	workersMu sync.Mutex
	workers   map[string]*roomWorker
	retired   relaylane.Stats
	laneCap   int

	// routerDrops counts inbound messages the subscriber router discarded on
	// a workerForInbound miss (see Stats.RouterDrops). A dedicated atomic
	// rather than folding into retired/workersMu: it is incremented on the
	// router's hot path for every miss, including misses that are not tied
	// to any worker's lifecycle at all (e.g. a genuinely unknown room), so
	// it has no natural home under a per-worker lock. A plain atomic counter
	// is trivially monotonic and adds no contention to the hot path.
	routerDrops atomic.Uint64

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
	if cfg.RoomQueueSize <= 0 {
		cfg.RoomQueueSize = relaylane.DefaultCap
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
		laneCap:     cfg.RoomQueueSize,
		workers:     make(map[string]*roomWorker),
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

// Stats returns a snapshot of the inbound delivery counters, summed across
// every room worker this relay has ever had — currently live ones plus the
// running total folded in from retired ones (see the retired field's doc).
//
// Coalesced/AwarenessSuperseded/HardDrops are guaranteed MONOTONIC across
// sequential calls (never decrease) but not guaranteed EXACT: a message that
// races a room's retirement — pushed onto a lane via a stale
// workerForInbound reference after stopWorker has already folded that
// lane's counters into r.retired and dropped it from r.workers — is
// delivered but its contribution to these counters is lost for good (see
// the retired field's doc for why this residual gap exists and can't be
// closed without holding workersMu across the router's Push, which would
// reintroduce lock contention on the hot path this whole change set exists
// to remove). RouterDrops is exact (a single atomic counter, incremented
// exactly once per drop) and therefore also monotonic.
//
// Getting monotonicity right requires holding workersMu for the ENTIRE
// computation below, not just the map snapshot — an earlier version of this
// method unlocked before summing the live lanes, which left a genuine
// three-call race: (1) this method snapshots the map (including live worker
// w) and reads retired, then unlocks; (2) stopWorker runs, folding w's
// current stats into retired and removing w from the map; (3) a stale
// router reference to that same w (obtained from workerForInbound just
// before step 2 — the residual gap described above) pushes more data onto
// it; (4) this method, still holding only its stale slice and no lock, reads
// w.lane.Stats() through that reference and picks up the step-3 push,
// returning a total that includes it; (5) a later call, with w now gone
// from the map, returns retired alone — smaller than what step 4 returned,
// a real decrease. Holding workersMu across the whole loop below closes
// this: stopWorker cannot run between this method's retired-read and its
// per-lane reads, so every value it sums is one a later call, seeded from
// the resulting retired total, can only match or exceed. The remaining gap
// (a step-3-style push landing AFTER stopWorker's own fold, once this
// method is no longer involved at all) is the residual undercount described
// above — invisible to every call from then on, but never a decrease
// relative to one that already ran. The cost of holding the lock this long
// is that Stats() briefly blocks worker creation (workerFor) and retirement
// (stopWorker) for the duration of the loop — acceptable because Stats() is
// a polled diagnostic and each Lane.Stats() call is just a mutex acquire
// plus a small struct copy, not anything that can block on Sink.Inject or
// Redis I/O.
//
// Safe to call concurrently, including before Start (no workers yet, so a
// zero Stats) and after Close (workers have all exited but their counters,
// held in each Lane, are still readable memory).
func (r *Relay) Stats() Stats {
	r.workersMu.Lock()
	out := Stats{
		Coalesced:           r.retired.Coalesced,
		AwarenessSuperseded: r.retired.AwarenessSuperseded,
		HardDrops:           r.retired.HardDrops,
	}
	for _, w := range r.workers {
		s := w.lane.Stats()
		out.Coalesced += s.Coalesced
		out.AwarenessSuperseded += s.AwarenessSuperseded
		out.HardDrops += s.HardDrops
	}
	r.workersMu.Unlock()
	out.RouterDrops = r.routerDrops.Load()
	return out
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

// runSubscriber reads from the PubSub channel and routes each message to its
// room's worker — after decoding, skipping self-published payloads via
// nodeID comparison, and re-checking the closed flag (so a message buffered
// at the moment Close fires does not leak past Close). This loop must never
// call Inject itself: that is exactly the head-of-line stall #187 reports,
// because one room's slow Inject would block delivery for every room on the
// node. Instead it hands off to workerForInbound(room)'s lane, which never
// blocks, and returns straight to reading; the per-room worker goroutine
// (worker.go) does the actual Inject call.
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
			// H2: drop self-deliveries before any further work — in the
			// router, so a node's own payloads never consume lane capacity.
			if bytes.Equal(srcNodeID, r.nodeID) {
				continue
			}
			// Hand off and go straight back to reading. This loop must never
			// call Inject: doing so is exactly the head-of-line stall #187
			// reports, because one room's slow Inject would block every room.
			// A miss (no worker for this room — see workerForInbound) is
			// dropped here, symmetric with H2 above, and counted in
			// routerDrops (Stats.RouterDrops) — an expected, routine event
			// under ordinary room churn, not a data-loss signal.
			if w, ok := r.workerForInbound(room); ok {
				w.lane.Push(kind, data)
			} else {
				r.routerDrops.Add(1)
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
//
// The RPC uses the relay's bound start context (the one passed to Start, in
// practice Server.relayCtx) so a slow/unreachable Redis cannot pin this
// goroutine indefinitely — Server.Shutdown cancels the ctx and the SUBSCRIBE
// returns promptly. Note that the websocket provider calls RoomActivated
// under s.rmu.Lock during room creation; a non-cancelable Background ctx
// here would let a Redis stall block the entire server's room-create path.
func (r *Relay) RoomActivated(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started.Load() || r.closed.Load() {
		return
	}
	// Short-circuit if the relay's bound ctx is already cancelled (e.g.
	// Shutdown is in flight): skip the doomed RPC and the bookkeeping it
	// would leave behind.
	select {
	case <-r.startCtx.Done():
		return
	default:
	}

	r.activeRooms[room]++
	if r.activeRooms[room] > 1 {
		return // already subscribed
	}
	// Start the worker BEFORE SUBSCRIBE: once the broker has us subscribed a
	// message can arrive immediately, and it must find a live lane.
	r.workerFor(room)
	if err := r.pubSub.Subscribe(r.startCtx, r.channelFor(room)); err != nil {
		r.log.Warn("cluster/redis: SUBSCRIBE failed", "room", room, "err", err)
	}
}

// RoomDeactivated unsubscribes from the room's pub/sub channel and retires
// the room's delivery worker. See the RoomActivated godoc for idempotency /
// reference-counting / locking semantics. The UNSUBSCRIBE RPC is cancelable
// via the relay's bound start context for the same reason: a stalled Redis
// must not pin the websocket provider's room-teardown path.
//
// stopWorker is called last, still under r.mu (consistent with the
// established r.mu → workersMu order: RoomActivated already nests workersMu
// under r.mu the same way via workerFor). This is safe because stopWorker
// only deletes a map entry and closes a channel — it does not drain the
// lane or call Sink.Inject itself, so it cannot block on a wedged Sink; the
// actual drain happens later, asynchronously, on the worker's own goroutine
// (see runRoomWorker's w.done case), which Close's wg.Wait joins.
func (r *Relay) RoomDeactivated(room string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.started.Load() || r.closed.Load() {
		return
	}
	select {
	case <-r.startCtx.Done():
		return
	default:
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
	if err := r.pubSub.Unsubscribe(r.startCtx, r.channelFor(room)); err != nil {
		r.log.Warn("cluster/redis: UNSUBSCRIBE failed", "room", room, "err", err)
	}
	// Retire the room's delivery worker. Ordering matters: we unsubscribed
	// above so no new payload can be routed to it, then stop it.
	r.stopWorker(room)
}

// Close stops the relay. Idempotent. Does NOT close the underlying
// *redis.Client — clients are commonly shared across the application and
// owned by the caller.
//
// Close blocks until any in-flight Start has committed (it acquires r.mu) and
// all background goroutines have exited (wg.Wait) — the publisher, the
// subscriber router, and every per-room delivery worker, all of which select
// on r.done and are registered on r.wg. Combined with the closed-flag
// re-check in drainLane, this is what makes the relay safe under racy
// callers: there is no window in which anything can fire after Close
// returns.
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
