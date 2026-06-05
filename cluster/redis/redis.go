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
// Outbound and PUBLISHes it on the room's channel; the subscriber goroutine
// decodes the inbound on every node and hands it to Sink.Inject, which the
// websocket provider applies with the relay sentinel origin so the local
// observer drops the echo.
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
// # Echo guard (still entirely on the provider side)
//
// The relay package contract is "Origin is observer-local and never crosses
// the wire" (see cluster/relay.go); this adapter honours that. Echo
// prevention rides the provider's per-server sentinel — Publish here just
// forwards whatever the provider hands it.
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
	"context"
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

	// Logger receives Warn-level entries when a delivery fails (decode
	// error, sink.Inject error, etc.). nil falls back to slog.Default().
	Logger *slog.Logger
}

// Relay is the Redis-backed cluster.Relay.
type Relay struct {
	client *goredis.Client
	prefix string
	log    *slog.Logger

	// outbound carries Publish calls to the publisher goroutine. A bounded
	// channel back-pressures the caller, matching MemRelay.
	outbound chan cluster.Outbound

	// pubSub is created in Start; subscribed channels are added/removed
	// dynamically via RoomActivated / RoomDeactivated.
	pubSub *goredis.PubSub

	// sink is the locally-bound websocket.Server; set once in Start.
	sink cluster.Sink

	// done is closed once by Close to signal all goroutines to exit.
	done chan struct{}

	// startedOnce / closedOnce keep Start and Close idempotent under racy
	// callers.
	startedOnce sync.Once
	closedOnce  sync.Once
	closed      atomic.Bool
	started     atomic.Bool

	wg sync.WaitGroup

	// activeRooms tracks SUBSCRIBE state so RoomActivated/RoomDeactivated
	// are idempotent at the relay layer (Redis SUBSCRIBE on an already-
	// subscribed channel is a no-op, but we still avoid the round trip).
	roomsMu     sync.Mutex
	activeRooms map[string]int
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Relay{
		client:      client,
		prefix:      cfg.ChannelPrefix,
		log:         cfg.Logger,
		outbound:    make(chan cluster.Outbound, cfg.OutboundBuffer),
		done:        make(chan struct{}),
		activeRooms: make(map[string]int),
	}, nil
}

// channelFor returns the Redis channel name for a given room.
func (r *Relay) channelFor(room string) string {
	return r.prefix + room
}

// Start binds the relay to a Sink and starts the subscriber + publisher
// goroutines. It is called exactly once by Server.AttachRelay.
func (r *Relay) Start(ctx context.Context, sink cluster.Sink) error {
	if sink == nil {
		return ErrRelayNotStarted
	}
	if r.closed.Load() {
		return ErrRelayClosed
	}
	r.startedOnce.Do(func() {
		r.sink = sink
		// Create the PubSub with no channels. go-redis lazily opens the
		// connection on first SUBSCRIBE; transient connectivity failures
		// surface there rather than here (Receive on an empty
		// subscription blocks forever with no message to deliver).
		r.pubSub = r.client.Subscribe(ctx)
		r.started.Store(true)

		r.wg.Add(2)
		go r.runSubscriber(ctx)
		go r.runPublisher(ctx)
	})
	if r.sink != sink {
		// startedOnce already fired with a different sink.
		return errors.New("cluster/redis: relay already started with a different Sink")
	}
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
			body := encodeOutbound(out)
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
// to Sink.Inject. The PubSub Go channel closes when pubSub.Close() is
// called (by our Close); we exit the loop on that or on done/ctx.
func (r *Relay) runSubscriber(ctx context.Context) {
	defer r.wg.Done()
	ch := r.pubSub.Channel()
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
			room, kind, data, err := decodeInbound([]byte(msg.Payload))
			if err != nil {
				r.log.Warn("cluster/redis: decodeInbound failed; drop",
					"channel", msg.Channel, "err", err)
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
// MemRelay). Returns ErrRelayClosed if Close has been called or
// ErrRelayNotStarted if Start has not.
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
	select {
	case r.outbound <- out:
		return nil
	case <-r.done:
		return ErrRelayClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RoomActivated subscribes to the room's pub/sub channel. Calls are
// idempotent and reference-counted: a duplicate RoomActivated increments a
// counter but performs no Redis call; RoomDeactivated decrements and
// unsubscribes only when the count reaches zero.
func (r *Relay) RoomActivated(room string) {
	if !r.started.Load() || r.closed.Load() {
		return
	}
	r.roomsMu.Lock()
	r.activeRooms[room]++
	count := r.activeRooms[room]
	r.roomsMu.Unlock()
	if count > 1 {
		return // already subscribed
	}
	if err := r.pubSub.Subscribe(context.Background(), r.channelFor(room)); err != nil {
		r.log.Warn("cluster/redis: SUBSCRIBE failed", "room", room, "err", err)
	}
}

// RoomDeactivated unsubscribes from the room's pub/sub channel. See the
// RoomActivated godoc for idempotency / reference-counting semantics.
func (r *Relay) RoomDeactivated(room string) {
	if !r.started.Load() || r.closed.Load() {
		return
	}
	r.roomsMu.Lock()
	if r.activeRooms[room] <= 0 {
		r.roomsMu.Unlock()
		return
	}
	r.activeRooms[room]--
	count := r.activeRooms[room]
	if count == 0 {
		delete(r.activeRooms, room)
	}
	r.roomsMu.Unlock()
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
func (r *Relay) Close() error {
	var closeErr error
	r.closedOnce.Do(func() {
		r.closed.Store(true)
		close(r.done)
		if r.pubSub != nil {
			if err := r.pubSub.Close(); err != nil {
				closeErr = fmt.Errorf("cluster/redis: PubSub close: %w", err)
			}
		}
		r.wg.Wait()
	})
	return closeErr
}

// encodeOutbound serialises an Outbound into the wire format documented at
// the top of this file: VarUint(kind) + VarString(room) + VarBytes(data).
// Origin is observer-local and intentionally not encoded. The encoding
// is infallible because all fields are length-prefixed primitives.
func encodeOutbound(out cluster.Outbound) []byte {
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(out.Kind))
		enc.WriteVarString(out.Room)
		enc.WriteVarBytes(out.Data)
	})
}

// decodeInbound is the inverse of encodeOutbound. Returns the room, kind,
// and data payload (the inbound has no Origin field — the relay package
// contract is that Origin is observer-local and never crosses the wire).
func decodeInbound(b []byte) (room string, kind cluster.Kind, data []byte, err error) {
	dec := encoding.NewDecoder(b)
	k, err := dec.ReadVarUint()
	if err != nil {
		return "", 0, nil, fmt.Errorf("read kind: %w", err)
	}
	roomStr, err := dec.ReadVarString()
	if err != nil {
		return "", 0, nil, fmt.Errorf("read room: %w", err)
	}
	payload, err := dec.ReadVarBytes()
	if err != nil {
		return "", 0, nil, fmt.Errorf("read data: %w", err)
	}
	// The decoder's ReadVarBytes returns a sub-slice of the input — the
	// input here is the Redis message payload, which go-redis owns for the
	// duration of the channel receive. Copy so the slice can outlive the
	// receive.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	return roomStr, cluster.Kind(k), cp, nil
}
