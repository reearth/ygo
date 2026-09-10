package redis

import (
	"bytes"
	"context"
	"errors"
	"hash/fnv"
	"sort"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/crdt"
)

// readerFor hash-assigns a room to one of the Readers goroutines.
//
// Stable by construction: a room that migrated between readers across cycles
// would leave two readers holding cursors for it, and they would replay each
// other's entries.
func (r *Relay) readerFor(room string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(room))
	return int(h.Sum32() % uint32(r.scfg.readers)) //nolint:gosec // readers is validated > 0
}

// roomsForReader returns the rooms currently assigned to one reader, sorted so
// an XREAD's key order is deterministic (which makes test failures readable
// and makes a cursor map easy to align with a response).
func (r *Relay) roomsForReader(idx int) []string {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()

	var out []string
	for room, n := range r.streamRooms {
		if n > 0 && r.readerFor(room) == idx {
			out = append(out, room)
		}
	}
	sort.Strings(out)
	return out
}

// keyBatches splits keys into runs of at most max.
//
// XREAD takes N keys plus N IDs, so an unbounded key set builds an unbounded
// command. Readers bounds concurrency; this bounds command size, and the two
// are independent — 10k rooms across 4 readers is 2500 keys per reader.
func keyBatches(keys []string, max int) [][]string {
	if len(keys) == 0 {
		return nil
	}
	var out [][]string
	for i := 0; i < len(keys); i += max {
		end := i + max
		if end > len(keys) {
			end = len(keys)
		}
		out = append(out, keys[i:end])
	}
	return out
}

// --- The reader loop -------------------------------------------------------

// Cursor start positions.
//
// oldestID starts a sync stream at the OLDEST retained entry rather than at
// the tail. V1 updates are idempotent and commutative, so replaying entries
// already applied is harmless, and starting at the oldest entry ELIMINATES
// the race between loading a snapshot and beginning to read rather than
// narrowing it. It also gives late-joiner catch-up for free, which the
// pub/sub tier has to punt to the persistence layer. Nothing has to be
// persisted anywhere for this to hold.
//
// tailID starts an awareness stream at the tail, because replaying presence
// resurrects clients that are long gone. The cost is a narrow window: an
// awareness entry appended between two XREADs of a never-yet-read awareness
// stream is skipped, because "$" is re-resolved server-side on each call.
// That is acceptable and self-healing — awareness is idempotent heartbeat
// state that a client re-sends within one interval — and it is the reason
// awareness entries are excluded from gap accounting (see handleStream).
const (
	oldestID = "0"
	tailID   = "$"
)

// streamsPerRoom is how many stream keys one room contributes to an XREAD:
// its sync stream and its awareness stream.
//
// maxKeysPerRead bounds KEYS, so readOnce batches rooms in groups of
// maxKeysPerRead/streamsPerRoom. Batching rooms directly against
// maxKeysPerRead would build a command with twice the intended number of
// keys, defeating the bound it is reaching for.
const streamsPerRoom = 2

// maxEntriesPerStream bounds how many entries one XREAD pulls from a single
// stream, and so bounds both the memory one catch-up cycle allocates and the
// size of the batch handed to crdt.MergeUpdatesV1. A deeper backlog is not
// lost: the cursor advances and the next cycle takes the next slice.
//
// Deliberately its own constant rather than reusing maxKeysPerRead as XREAD's
// Count: a key budget and an entry budget are different quantities that
// happen to share a number today, and conflating them means a future change
// to one silently moves the other.
const maxEntriesPerStream = 512

// readErrorBackoff is the pause after a failed XREAD, so an unreachable Redis
// is retried at a bounded rate instead of spun on.
//
// Deliberately not stalledBackoffBase: that constant is documented as the
// LANE-backpressure backoff. A Redis error and a full local lane are
// unrelated conditions, and sharing one knob between them would make either
// one's tuning silently change the other's behaviour.
const readErrorBackoff = 100 * time.Millisecond

// deferredReadBackoff is the pause after a cycle that deliberately left
// entries under their cursor because a room had no delivery worker yet (see
// handleStream's non-resident case). The other cause of a declined advance,
// lane backpressure, escalates instead — see stallBackoff for why the two are
// paced differently.
//
// Without it that cycle would spin: the entries are still there, so the next
// XREAD returns immediately with the same ones, at whatever rate Redis can
// answer. Re-reading is free per read and not free per second. The pause is
// short because the condition it waits out is short — the window inside
// RoomActivated between a room being assigned to a reader and its worker
// existing — and it applies to the whole cycle rather than to the one stream,
// which can cost a co-batched room this much extra latency during that
// window. That trade is deliberate: it keeps the loop's control flow one
// decision per cycle instead of one per stream.
const deferredReadBackoff = 20 * time.Millisecond

// nonBlockingRead is the XReadArgs.Block value for a read that must return
// whatever is already there and not wait.
//
// It must be NEGATIVE, not zero. go-redis emits the BLOCK argument for any
// Block >= 0 (stream_commands.go), and Redis reads "BLOCK 0" as block
// FOREVER; only a negative value omits BLOCK and makes XREAD non-blocking.
const nonBlockingRead = -1 * time.Nanosecond

// cursorLimit bounds how many stream cursors are remembered.
//
// It bounds SYNC cursors only, because those are the only kind this map
// holds. The two kinds need opposite retention across a room's
// deactivate/reactivate cycle, since they start from opposite defaults when
// they are missing, and they are stored in different places so that each
// retention follows from where the cursor lives rather than from remembering
// to delete it:
//
//   - A SYNC cursor survives its room's deactivation. Its default is the
//     oldest retained entry, so dropping it at deactivation would make
//     ordinary room churn replay the room's whole retention window on every
//     reactivation — the condition StreamStats.Replayed exists to alarm on.
//     Hence a relay-scoped map, outliving any one residency, and hence this
//     limit: sync cursors accumulate across churn and something has to stop
//     that growing forever. See evictStaleCursorsLocked for which entries go.
//   - An AWARENESS cursor must NOT survive it. Its default is the tail, so
//     keeping it would resume a reactivated room mid-stream and replay
//     presence for whoever was in the room last time. Hence it is not in
//     this map at all: it lives on the room's delivery worker
//     (roomWorker.awCursor), whose lifetime IS the residency, so a
//     reactivated room starts at the tail with nothing to delete and no
//     in-flight read able to write the old position back.
const cursorLimit = 4096

// streamTarget is one XREAD key together with what it means.
//
// readBatch builds the keys from room names, so it already knows the room and
// which of the two streams a key is; carrying that forward means handleStream
// never has to derive a room from a key. Attributing an entry by the key set
// that ASKED for it needs no parsing to be correct, which also makes a
// returned key nobody asked for something to ignore rather than something to
// interpret. parseStreamKey exists for the diagnostic path only.
type streamTarget struct {
	key         string
	room        string
	isAwareness bool
	// residency is the roomWorker that owned the room when this read's id
	// vector was built, and is the fence awareness delivery is accepted
	// against — see awarenessCursor and deliverAwareness. Meaningful for
	// awareness targets only.
	//
	// Sync delivery is deliberately NOT fenced this way: re-delivering a sync
	// entry to a reactivated room is idempotent and commutative (see
	// oldestID), whereas re-delivering presence resurrects departed clients.
	// A worker POINTER rather than a generation counter because the worker is
	// already the object whose lifetime is exactly one residency, so it needs
	// no second map to bound, and because a live reference to the retired
	// worker keeps its address from being recycled — a counter that could be
	// deleted and reissued would compare equal across residencies.
	residency *roomWorker
}

// readResult is what one batch's XREAD found, which is what the next batch's
// blocking decision and the cycle's pacing are made from.
type readResult struct {
	// got is true when XREAD returned at least one entry for any key in the
	// batch, whether or not that entry was delivered anywhere.
	got bool
	// deferred is true when a stream's entries were deliberately left under
	// their cursor, so the same entries will be read again next cycle. True
	// for EITHER cause (see streamOutcome), because either one means the next
	// XREAD returns immediately with the same entries and so has to be paced.
	deferred bool
	// stalled narrows that to the lane-backpressure cause, which is paced
	// differently: a missing worker is a sub-millisecond window inside
	// RoomActivated, whereas a wedged consumer can last minutes, so only this
	// one escalates its backoff. See stallBackoff.
	stalled bool
}

// streamOutcome is what handleStream did with one stream's entries.
//
// Three outcomes rather than the bool this started as, because the two
// non-consuming ones are indistinguishable to a caller that only learns "the
// cursor did not move" and yet need different pacing and different counters.
type streamOutcome int

const (
	// streamConsumed: the entries were dealt with — delivered, filtered or
	// dropped — and the cursor advanced past them.
	streamConsumed streamOutcome = iota
	// streamUnready: the room has no delivery worker yet, so there is nowhere
	// to put the entries. Counted as StreamStats.Deferred.
	streamUnready
	// streamStalled: the room's lane is at capacity, so delivering would force
	// it to coalesce. Counted as StreamStats.Stalled.
	streamStalled
)

// streamReadCtx derives the readers' context from the relay's bound context so
// that Close cancels it too.
//
// The ordinary case is a caller whose context OUTLIVES the relay — every
// test here, and Server, which cancels relayCtx after Close. Readers bound to
// that context would keep issuing XREADs against a closed relay until it was
// cancelled; the derived context is what makes Close stop them.
//
// What it does NOT do is shorten Close. An XREAD already in flight runs to
// its block deadline whatever happens to its context, because go-redis arms
// the socket deadline from ctx.Deadline() and never watches for cancellation
// (v9.18.0). What ends a reader is the r.closed check at the top of
// runStreamReader's loop, and what bounds how long that takes is
// maxReadBlock.
//
// The watcher is registered on r.wg like every other relay goroutine. That is
// safe rather than self-deadlocking because it exits on r.done, which Close
// closes BEFORE it calls wg.Wait.
func (r *Relay) streamReadCtx(parent context.Context) context.Context {
	ctx, cancel := context.WithCancel(parent)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cancel()
		select {
		case <-r.done:
		case <-ctx.Done():
		}
	}()
	return ctx
}

// blockForBatch is how long batch i of n may block.
//
// Only the LAST batch of a cycle blocks, and only when every batch before it
// came back empty. Blocking on each batch in turn would make a reader's
// inbound latency scale with its batch COUNT rather than with Redis: a reader
// owning 2500 rooms has 10 batches, so an entry waiting in batch 10 would sit
// there while batches 1-9 each waited out their own full block, up to 10 x
// ReadBlock behind an idle cluster. Reading the earlier batches without
// blocking costs one round trip each and finds anything that is already
// there; the single trailing block is what keeps an idle reader from spinning.
//
// sawEntries also suppresses the trailing block: if an earlier batch returned
// something there is work to do now, and the next cycle re-reads everything
// anyway.
//
// The trade is command rate. A 10-batch reader used to spend 10 x ReadBlock
// per cycle and so issued ~4 commands a second; it now completes a cycle in
// one ReadBlock and issues ~40. That is the right side of the trade for this
// tier — it also divides that reader's shutdown and activation latency by its
// batch count, which is what Config.ReadBlock's bound is supposed to mean —
// and an XREAD that finds nothing is cheap.
func (r *Relay) blockForBatch(i, n int, sawEntries bool) time.Duration {
	if i == n-1 && !sawEntries {
		return r.scfg.readBlock
	}
	return nonBlockingRead
}

// stallBackoff is the pause after n consecutive cycles that ended by declining
// a cursor advance for lane backpressure.
//
// It doubles from stalledBackoffBase and is capped at limit (the reader's
// ReadBlock). Doubling is what stops a room whose consumer is wedged for
// minutes from re-reading the same entries as fast as Redis can answer:
// re-reading is free per read and not free per second. Resetting the streak on
// any cycle that does NOT stall is what keeps recovery prompt — one burst
// leaves the reader at full speed on the very next cycle rather than serving
// out an escalated penalty it no longer needs.
//
// Capped at ReadBlock, not at some larger number, because ReadBlock is already
// this tier's stated bound on inbound latency and on how long Close and a
// newly activated room wait (see maxReadBlock); a backoff past it would
// silently break a bound Config.ReadBlock documents. The cap can therefore be
// SMALLER than stalledBackoffBase — ReadBlock accepts values down to 1ms — and
// the cap wins, because a reader must never pause longer than its own read
// interval.
//
// Doubled by multiplication rather than by shifting stalledBackoffBase << n-1:
// the streak has no upper bound, and that shift goes negative at n=38.
func stallBackoff(n int, limit time.Duration) time.Duration {
	d := stalledBackoffBase
	for i := 1; i < n && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	return d
}

// nextPause is a reader's whole pacing decision for one finished cycle: how
// long to wait before the next XREAD, and what the stall streak becomes.
//
// A pure function of (what the cycle saw, the streak so far, the read
// interval), so the escalate-and-reset behaviour can be asserted directly
// instead of inferred from how fast a goroutine happens to loop.
//
// stalls counts CONSECUTIVE cycles that ended by declining a cursor advance
// for lane backpressure, and any cycle that did not stall resets it — which is
// what makes recovery prompt after a single burst. It is per READER rather
// than per room for three reasons: the pause is one decision per cycle (see
// deferredReadBackoff), so a per-room map would have to be collapsed to one
// number here anyway; escalation is driven by the worst room on the reader,
// which is what that collapse would pick; and a per-room map would grow with
// room churn and so need its own bound and eviction pass, a cost cursors and
// lastSeq already each pay once.
//
// A stalled cycle outranks a merely deferred one because a wedged consumer
// outlasts an activation window by orders of magnitude, and a cycle that was
// both should be paced for the longer-lived condition.
func nextPause(res readResult, stalls int, readBlock time.Duration) (time.Duration, int) {
	switch {
	case res.stalled:
		stalls++
		return stallBackoff(stalls, readBlock), stalls
	case res.deferred:
		// Entries are still waiting under a cursor, so the next XREAD will
		// return immediately with the same ones; pace the retry instead of
		// spinning. See deferredReadBackoff.
		return deferredReadBackoff, 0
	default:
		return 0, 0
	}
}

// runStreamReader is one reader goroutine. It owns the rooms hash-assigned to
// idx and multiplexes them over XREAD.
//
// It also carries the cycle's pacing state, because pacing is the one decision
// in this loop that needs something from the PREVIOUS cycle (the stall streak)
// and readOnce is deliberately stateless. Goroutine-local, so no reader can
// contend with another for it.
func (r *Relay) runStreamReader(ctx context.Context, idx int) {
	defer r.wg.Done()

	stalls := 0
	for {
		if ctx.Err() != nil || r.closed.Load() {
			return
		}
		res, err := r.readOnce(ctx, idx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return
			}
			r.log.Warn("cluster/redis: stream read failed", "reader", idx, "err", err)
			select {
			case <-r.done:
				return
			case <-ctx.Done():
				return
			case <-time.After(readErrorBackoff):
			}
			// The streak is deliberately left alone: a Redis error says
			// nothing about whether the lane that stalled has drained.
			continue
		}
		var d time.Duration
		if d, stalls = nextPause(res, stalls, r.scfg.readBlock); d > 0 {
			r.pause(ctx, d)
		}
	}
}

// readOnce performs one XREAD cycle for the reader's rooms.
//
// A reader with NO assigned rooms idles instead of reading: XREAD with zero
// keys is invalid.
//
// A failing batch abandons the cycle and runStreamReader restarts it from
// batch 0, so the batches before the failure are read again. Deliberate: they
// are non-blocking reads whose cursors did not move, so the re-read costs one
// round trip each and cannot double-deliver anything (advancing a cursor is
// what marks an entry consumed), whereas resuming mid-cycle would mean
// carrying per-reader batch position across an error path for no correctness
// gain.
//
// It reports what the cycle saw and does NOT pace itself: the retry pause
// depends on how many cycles in a row have stalled, which is state
// runStreamReader keeps.
func (r *Relay) readOnce(ctx context.Context, idx int) (readResult, error) {
	var cycle readResult

	rooms := r.roomsForReader(idx)
	if len(rooms) == 0 {
		// XREAD with zero keys is invalid, so an idle reader cannot block on
		// Redis; it sleeps and re-checks its assignment. Bounded by ReadBlock
		// for the same reason the read is: this wait is also how long a
		// newly activated room and Close wait on this reader.
		r.pause(ctx, r.scfg.readBlock)
		return cycle, nil
	}

	batches := keyBatches(rooms, maxKeysPerRead/streamsPerRoom)
	for i := range batches {
		res, err := r.readBatch(ctx, batches[i], r.blockForBatch(i, len(batches), cycle.got))
		if err != nil {
			return cycle, err
		}
		cycle.got = cycle.got || res.got
		cycle.deferred = cycle.deferred || res.deferred
		cycle.stalled = cycle.stalled || res.stalled
	}
	return cycle, nil
}

// pause waits for d unless the relay is shutting down first, so no wait in
// the loop outlives Close by more than a scheduling hop.
func (r *Relay) pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-r.done:
	case <-time.After(d):
	}
}

// readBatch reads one XREAD's worth of rooms: each room's sync stream from
// the relay's cursor (defaulting to the oldest retained entry) and its
// awareness stream from its RESIDENCY's cursor (defaulting to the tail). See
// the oldestID/tailID constants for why those two defaults differ, and
// roomWorker.awCursor for why the two cursors are kept in different places.
//
// block is chosen by the caller per batch; see blockForBatch.
func (r *Relay) readBatch(ctx context.Context, rooms []string, block time.Duration) (readResult, error) {
	var out readResult

	n := len(rooms) * streamsPerRoom
	targets := make(map[string]streamTarget, n)
	keys := make([]string, 0, n)
	ids := make([]string, 0, n)

	// from is the ID this stream is read from, resolved by the caller: the two
	// kinds read their cursor out of different places (see
	// roomWorker.awCursor), and the awareness one has to be resolved together
	// with the residency that owns it.
	add := func(tgt streamTarget, from string) {
		// Unreachable for distinct rooms: syncKey and awKey cannot collide
		// with each other for ANY pair of room names (see the kindDiscrim
		// constants), and roomsForReader yields each room once. It survives as
		// the structural guarantee that keys and ids stay index-aligned — a
		// duplicate key would make XREAD's own command malformed — and it
		// costs one lookup in a map the attribution below needs regardless.
		if _, dup := targets[tgt.key]; dup {
			return
		}
		targets[tgt.key] = tgt
		keys = append(keys, tgt.key)
		ids = append(ids, from)
	}
	for _, room := range rooms {
		syncKey := r.scfg.syncKey(room)
		add(streamTarget{key: syncKey, room: room}, r.cursorFor(syncKey, oldestID))
		// Resolved as a pair, under one lock: the awareness position and the
		// residency it belongs to are one fact, and handleStream delivers the
		// response only while that residency is still the room's.
		residency, awFrom := r.awarenessCursor(room)
		add(streamTarget{
			key:         r.scfg.awKey(room),
			room:        room,
			isAwareness: true,
			residency:   residency,
		}, awFrom)
	}

	args := make([]string, 0, len(keys)+len(ids))
	args = append(args, keys...)
	args = append(args, ids...)

	res, err := r.client.XRead(ctx, &goredis.XReadArgs{
		Streams: args,
		Block:   block,
		Count:   maxEntriesPerStream,
	}).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			// Nothing new: the block expired, or this was a non-blocking read
			// of streams that had nothing past their cursors. Normal.
			return out, nil
		}
		return out, err
	}

	for _, stream := range res {
		if len(stream.Messages) > 0 {
			out.got = true
		}
		tgt, ok := targets[stream.Stream]
		if !ok {
			// Ignored, never guessed at. Attribution comes from the key set
			// this call built, so a key nobody asked for has no room to be
			// delivered to; parseStreamKey is used only to make the log
			// actionable, and a key that matches neither discriminator (a
			// foreign key sharing the prefix, say) reports as unrecognised
			// rather than being coerced into a room name.
			room, kind := "", "unrecognised"
			if parsed, isAw, ok := r.scfg.parseStreamKey(stream.Stream); ok {
				room, kind = parsed, "sync"
				if isAw {
					kind = "awareness"
				}
			}
			r.log.Warn("cluster/redis: XREAD returned an unrequested stream; skip",
				"stream", stream.Stream, "room", room, "kind", kind)
			continue
		}
		switch r.handleStream(tgt, stream.Messages) {
		case streamUnready:
			out.deferred = true
		case streamStalled:
			out.deferred = true
			out.stalled = true
		case streamConsumed:
		}
	}
	return out, nil
}

// cursorFor returns the remembered cursor for a key, or dflt when this reader
// has never read it.
func (r *Relay) cursorFor(key, dflt string) string {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if id, ok := r.cursors[key]; ok {
		return id
	}
	return dflt
}

// setCursor remembers a key's last delivered ID.
func (r *Relay) setCursor(key, id string) {
	r.streamMu.Lock()
	defer r.streamMu.Unlock()
	if _, known := r.cursors[key]; !known && len(r.cursors) >= cursorLimit {
		r.evictStaleCursorsLocked()
	}
	r.cursors[key] = id
}

// evictStaleCursorsLocked drops the cursors of rooms this relay no longer
// reads, in one pass, and keeps every cursor whose room is still assigned.
//
// The selectivity is the point. Evicting an ARBITRARY entry on overflow costs
// "only a replay", but on a node with more than cursorLimit/2 active rooms it
// lands on a LIVE room's cursor about half the time, and a live room that
// loses its cursor replays its entire retention window. That is exactly the
// condition
// StreamStats.Replayed tells operators to alert on ("cursors are being lost
// repeatedly"), so arbitrary eviction would make the tier trip its own alarm
// under nothing worse than ordinary scale.
//
// If every cursor is live the map is left above cursorLimit. That is the
// correct outcome: the residual is then bounded by the rooms this node
// actually reads — one sync cursor each, awareness being held on the workers
// instead (see roomWorker.awCursor) — i.e. by real load, and it is the same
// order as streamRooms itself.
//
// Caller must hold streamMu (which also guards streamRooms).
func (r *Relay) evictStaleCursorsLocked() {
	live := r.liveStreamKeysLocked()
	for key := range r.cursors {
		if _, ok := live[key]; !ok {
			delete(r.cursors, key)
		}
	}
}

// handleStream applies one stream's returned entries and advances its cursor,
// or reports which of the two reasons it left them for a later cycle instead.
//
// Entries are handed to the room's LANE, never to Sink.Inject directly, which
// is load-bearing in four ways:
//
//  1. Inject is the caller's code and may be arbitrarily slow. One reader
//     serves up to maxKeysPerRead/streamsPerRoom rooms, so injecting inline
//     would let one slow room stall inbound delivery for every other room on
//     that reader — precisely the head-of-line stall #187 exists to remove,
//     and the reason runSubscriber's own doc forbids Inject in that loop.
//  2. The Sink contract (cluster/relay.go) requires calls for the SAME room
//     to be serialised. Under Transport Both the pub/sub router pushes to the
//     lane while the reader would be injecting directly, giving two concurrent
//     Injects for one room. Both paths going through the lane means the room's
//     single worker goroutine remains the only caller.
//  3. Close promises that nothing reaches the Sink after it returns
//     (redis.go's Close doc), which drainLane enforces with its closed check.
//     A direct Inject from a reader has no such gate.
//  4. Lane.Push never blocks on capacity and never drops — an over-cap sync
//     queue is merged, awareness is kept latest-only — so nothing is traded
//     away for the isolation. (It does take the lane's own mutex, which is
//     why no relay-wide lock may be held across it — see deliverAwareness.)
//
// Sync entries are MERGED into a single payload first. Without that, a
// catch-up of N entries would push N payloads, and each one is re-broadcast
// to every local peer — turning one reader's restart into an N-fold broadcast
// storm. Awareness is not merged — each payload carries its own clock and the
// receiver's per-client gate handles staleness — but only the LAST payload of
// a read is pushed, because the lane's awareness slot holds one blob and
// would supersede the rest untouched.
//
// Awareness also takes a different route to the lane. It is handed over by
// deliverAwareness, which drops the read if the room has changed residency
// since the id vector was built, because presence published before a
// reactivation must not reach the room's new occupants. Sync goes straight to
// the lane resolved above: replaying a sync entry is harmless, so it inherits
// the same accepted staleness runSubscriber has.
func (r *Relay) handleStream(tgt streamTarget, msgs []goredis.XMessage) streamOutcome {
	if len(msgs) == 0 {
		return streamConsumed
	}

	// Resolved once, as the router does: a room retired mid-batch leaves a
	// stale handle, which is the same accepted staleness runSubscriber has.
	w, resident := r.workerForInbound(tgt.room)
	if !resident {
		// The room is in this reader's assignment set but has no delivery
		// worker: RoomActivated adds a room to streamRooms BEFORE it creates
		// the worker, so a reader can pick the room up in between.
		//
		// Every entry stays under its cursor and is read again next cycle.
		// Advancing here instead would consume the room's entire retained
		// backlog — a reader that reaches a brand-new room in that window
		// reads it from the oldest retained entry — and discard it before the
		// worker that was about to exist could receive any of it. That is
		// silent loss of precisely what this tier exists to prevent, and the
		// stream is durable, so deferring costs one extra read.
		//
		// routerDrops is deliberately NOT incremented. Nothing was dropped:
		// RouterDrops counts messages the router DISCARDED, and counting a
		// deferral there would report loss that did not happen, on a counter
		// operators are told to watch the rate of. StreamStats.Deferred counts
		// it instead, which is a declined advance rather than a loss.
		r.deferred.Add(1)
		return streamUnready
	}

	// This branch is the whole argument for this tier over pub/sub. When a
	// room's local consumer falls behind, pub/sub can only choose between
	// merging the backlog and dropping it, because the message it holds exists
	// nowhere else. A stream entry is durable, so there is a third option:
	// leave the entries under the cursor and read them again next cycle. That
	// costs one re-read, discards nothing, and asks the lane to merge nothing
	// — which is what makes backpressure toward Redis safe here.
	//
	// Placed BEFORE the decode loop, not before the push as would be the
	// obvious reading, because the loop is not side-effect free: it calls
	// noteSeq, which records each source's latest sequence number. Deciding to
	// stall after that would leave lastSeq holding sequences from entries we
	// are about to re-read, and next cycle's lower numbers would then be
	// classified as a publisher RESTART — inflating StreamStats.Restarts once
	// per stalled cycle on a perfectly healthy cluster. The unready branch
	// above returns before the loop for the same reason.
	//
	// Awareness streams are deliberately exempt. Lane.Full reports on the
	// SYNC queue, the only thing the lane's capacity governs; an awareness
	// push replaces a single latest-only slot, so it can neither deepen the
	// backlog this backoff exists to bound nor trigger a merge. Declining one
	// would cost presence freshness and buy nothing, and would re-read
	// presence entries that the next push supersedes anyway.
	if !tgt.isAwareness && w.lane.Full() {
		r.stalled.Add(1)
		return streamStalled
	}

	syncPayloads := make([][]byte, 0, len(msgs))
	// Collected rather than pushed inline, because the push and the cursor
	// advance have to happen as ONE residency-fenced operation — see
	// deliverAwareness.
	var awPayloads [][]byte
	lastID := ""
	for _, msg := range msgs {
		// Recorded before every skip below, deliberately: a malformed,
		// foreign or self-published entry has been fully accounted for, and
		// leaving it under the cursor would make the reader re-read it
		// forever. Every remaining path in this loop is such a case — the one
		// skip that must NOT advance the cursor returned above.
		lastID = msg.ID

		nodeID, seq, kind, data, err := decodeStreamEntry(msg.Values)
		if err != nil {
			r.log.Warn("cluster/redis: malformed stream entry; skip",
				"stream", tgt.key, "id", msg.ID, "err", err)
			r.routerDrops.Add(1)
			continue
		}
		if bytes.Equal(nodeID, r.nodeID) {
			continue // self-delivery
		}
		// Mirrors runSubscriber's H3: an unrecognised kind is dropped, never
		// guessed at, because a non-V1 blob filed as KindSync makes the lane's
		// merge fail and can cost the room its legitimate updates.
		switch kind {
		case cluster.KindSync, cluster.KindAwareness:
		default:
			r.log.Warn("cluster/redis: unrecognised kind in stream; drop",
				"stream", tgt.key, "room", tgt.room, "kind", kind)
			r.routerDrops.Add(1)
			continue
		}
		// publishStream chooses the key from the kind, so the two always
		// agree. A disagreement means a foreign or corrupt writer, and
		// trusting either side over the other would either replay presence or
		// merge a non-update blob. Drop instead.
		if tgt.isAwareness != (kind == cluster.KindAwareness) {
			r.log.Warn("cluster/redis: stream/kind mismatch; drop",
				"stream", tgt.key, "room", tgt.room, "kind", kind)
			r.routerDrops.Add(1)
			continue
		}
		if tgt.isAwareness {
			// Not counted in noteSeq: awareness is read from the tail and
			// bounded to AwarenessMaxLen, so skipped sequence numbers are the
			// designed behaviour rather than evidence of loss, and feeding
			// them to gap detection would make Gaps — a counter documented as
			// "alert on presence" — nonzero on every healthy node.
			awPayloads = append(awPayloads, data)
			continue
		}
		r.noteSeq(tgt.key, nodeID, seq)
		syncPayloads = append(syncPayloads, data)
	}

	if tgt.isAwareness {
		// Cursor and latest payload, accepted only while the room is still on
		// the residency this read was issued for. Declining is a deliberate
		// discard of presence published before a reactivation, not a
		// deferral — the successor residency reads from the tail, so the
		// entries are dealt with either way, hence streamConsumed
		// unconditionally. deliverAwareness pushes only the LAST of these
		// payloads; see its doc for why the rest are not worth a lane
		// acquisition each.
		r.deliverAwareness(tgt.room, tgt.residency, awPayloads, lastID)
		return streamConsumed
	}

	return r.applySync(w, tgt, syncPayloads, lastID)
}

// applySync delivers one stream's sync entries and advances its cursor, but
// only while w is still the room's residency: stopWorker can retire w after
// handleStream resolved it, and a worker whose final drain has already run
// leaves the payload on a lane nobody reads, so advancing past it would lose
// the entry for good. Declining re-reads the entries next cycle onto the
// successor residency; the duplicate push onto the retired lane is harmless
// because V1 updates are idempotent.
//
// The check follows the push rather than sharing one lock with it because
// Lane.Push holds the lane's mutex across crdt.MergeUpdatesV1, and workersMu
// held across that couples every room on the node to one room's merge (#187).
func (r *Relay) applySync(w *roomWorker, tgt streamTarget, payloads [][]byte, lastID string) streamOutcome {
	if len(payloads) > 0 {
		r.pushSync(w, tgt, payloads)
		if !r.stillResident(tgt.room, w) {
			r.deferred.Add(1)
			return streamUnready
		}
	}
	// One advance for every path that reaches here, because every path that
	// reaches here has finished with the entries it read.
	if lastID != "" {
		r.setCursor(tgt.key, lastID)
	}
	return streamConsumed
}

// pushSync hands a stream's sync entries to a room's lane as ONE merged
// update where it can. See handleStream for why merging matters.
func (r *Relay) pushSync(w *roomWorker, tgt streamTarget, payloads [][]byte) {
	if len(payloads) == 1 {
		w.lane.Push(cluster.KindSync, payloads[0])
		return
	}
	merged, err := crdt.MergeUpdatesV1(payloads...)
	if err != nil {
		// Deliver individually rather than dropping the batch: the N-fold
		// rebroadcast the merge exists to avoid is a cost, whereas losing the
		// entries would be divergence.
		r.log.Warn("cluster/redis: catch-up merge failed; injecting individually",
			"room", tgt.room, "entries", len(payloads), "err", err)
		for _, p := range payloads {
			w.lane.Push(cluster.KindSync, p)
		}
		return
	}
	w.lane.Push(cluster.KindSync, merged)
	// Counted on EVERY successful multi-entry merge, whether or not any of the
	// entries had been delivered before — which is why StreamStats.Replayed is
	// documented as a merge/batching gauge and explicitly not a counter to
	// alert on the rate of.
	r.replayed.Add(uint64(len(payloads) - 1))
}

// seqSource identifies one sequence series: one publishing node's entries in
// one stream.
//
// Both halves are load-bearing, and a struct key rather than a concatenated
// string because both halves are arbitrary bytes — nodeID is caller-supplied
// via Config.NodeID and a room name may contain anything — so any joining
// character could be forged by one half into the other's territory. A struct
// key has no encoding to get wrong.
type seqSource struct {
	node   string // publisher nodeID, as raw bytes held in a string
	stream string // stream key the entry was read from
}

// seqState is what this reader remembers about one sequence series.
//
// afterDecrease rides alongside the number rather than being derived from it,
// because "the previous observation went backwards" is not recoverable from
// the baseline value alone. See noteSeq for what it suppresses and why.
type seqState struct {
	last          uint64
	afterDecrease bool
}

// noteSeq tracks one source node's sequence numbers ON ONE STREAM and
// classifies what it sees.
//
// Keyed by (nodeID, stream key), matching nextSeq's per-stream counter. Both
// halves are required. Without the nodeID, two nodes' independent series
// would be compared against each other; without the stream key, one node's
// two streams would be folded into one series, and their legitimate
// interleaving — [1 3 5] in one stream, [2 4 6] in the other — would be
// reported as gaps on a perfectly healthy node.
//
// A JUMP is a provable gap: entries existed and were trimmed before this
// reader got to them. It is the only detectable form of loss, because XREAD
// from a trimmed ID returns the next surviving entry with no error and stream
// IDs are ms-seq rather than contiguous.
//
// A DECREASE is a restart, not a gap. Counters live in memory, so a node
// restarts them at 0 — and a node configured with a STATIC NodeID does that
// under the same identity. Reporting it as a gap would cry data loss on every
// deploy; worse, treating the lower numbers as already-seen would stall that
// source forever. So a decrease resets the baseline and is counted separately.
//
// THE FIRST COMPARISON AFTER A DECREASE CANNOT REPORT A GAP. A decrease is
// itself the statement "this source's numbering is not, right now, a reliable
// basis for inference", so the very next entry re-baselines instead of
// accusing. Two causes need this, and neither is exotic:
//
//   - nextSeq releases streamMu before the XADD it numbered is issued, and
//     Relay.Publish's contract explicitly requires tolerating two concurrent
//     Publish calls for the same room (a room's eviction/reload handoff does
//     exactly that, and room churn is continuous). So one publisher's entries
//     can land in the stream as [seq2, seq1] — a decrease, then a jump from
//     seq1 to seq3. The alternative fix is to hold streamMu across the XAdd,
//     which is refused: that is a lock held across a Redis call, and it would
//     couple every room on the node to one slow write — the cross-room
//     head-of-line coupling #187/#200 removed and this branch had to fix once
//     already.
//   - a restarted node's fresh [1 2 3…] can be read interleaved with the tail
//     of its own pre-restart series for the same reason.
//
// The trade is explicit and deliberate: this suppresses one REAL gap in the
// case where a genuine trim-away lands immediately after a genuine restart on
// the same series, and nothing else — the suppression is one comparison deep
// and clears on the next entry, so a persistent gap is still reported. That is
// the right way to be wrong for this counter. StreamStats.Gaps is documented
// "ALERT ON PRESENCE… a single gap means data was lost": its entire value is
// that a non-zero reading means something, and a signal that fires on healthy
// room churn is worth less than no signal at all. Losing one increment in a
// narrow conjunction costs an alert that a stall of any duration would raise
// again; a false positive costs the counter its credibility permanently.
func (r *Relay) noteSeq(streamKey string, nodeID []byte, seq uint64) {
	src := seqSource{node: string(nodeID), stream: streamKey}

	r.streamMu.Lock()
	prev, known := r.lastSeq[src]
	if !known && len(r.lastSeq) >= seqLimit {
		r.evictStaleLastSeqLocked()
	}
	decreased := known && seq < prev.last
	r.lastSeq[src] = seqState{last: seq, afterDecrease: decreased}
	r.streamMu.Unlock()

	switch {
	case !known:
		// First entry from this node on this stream: a baseline, not a gap.
	case decreased:
		r.restarts.Add(1)
	case seq > prev.last+1:
		if prev.afterDecrease {
			// Re-baselining, not accusing. See the doc comment.
			return
		}
		r.gaps.Add(1)
	}
}

// evictStaleLastSeqLocked drops the baselines of streams this relay no longer
// reads, keeping every baseline whose room is still assigned.
//
// Needed because keying by (node, stream) rather than by node alone turns a
// map bounded by CLUSTER SIZE into one that also grows with room churn. Same
// selective policy as evictStaleCursorsLocked, and safe in the same
// direction: a dropped baseline makes the next entry from that source a
// FIRST entry, which is counted as neither a gap nor a restart. It can only
// hide a gap on a stream this node had stopped reading — where nothing was
// being delivered to lose — and can never invent one.
//
// Caller must hold streamMu.
func (r *Relay) evictStaleLastSeqLocked() {
	live := r.liveStreamKeysLocked()
	for src := range r.lastSeq {
		if _, ok := live[src.stream]; !ok {
			delete(r.lastSeq, src)
		}
	}
}
