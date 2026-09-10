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

// readerFor hash-assigns a room to one of the Readers goroutines. Stable by
// construction: a room migrating between readers would leave two readers
// holding cursors for it, replaying each other's entries.
func (r *Relay) readerFor(room string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(room))
	return int(h.Sum32() % uint32(r.scfg.readers)) //nolint:gosec // readers is validated > 0
}

// roomsForReader returns the rooms assigned to one reader, sorted so an XREAD's
// key order is deterministic and easy to align a cursor map with.
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

// keyBatches splits keys into runs of at most max, bounding one XREAD's command
// size (it takes N keys plus N IDs). Independent of Readers, which bounds
// concurrency: 10k rooms across 4 readers is 2500 keys per reader.
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
// oldestID starts a sync stream at the OLDEST retained entry, not the tail: V1
// updates are idempotent and commutative, so replay is harmless, and this
// ELIMINATES rather than narrows the race between loading a snapshot and
// beginning to read, with late-joiner catch-up free and nothing persisted.
//
// tailID starts an awareness stream at the tail, because replaying presence
// resurrects clients long gone. Cost: an entry appended between two XREADs of a
// never-yet-read awareness stream is skipped, "$" being re-resolved server-side
// per call — self-healing, and the reason awareness is excluded from gaps.
const (
	oldestID = "0"
	tailID   = "$"
)

// streamsPerRoom is how many stream keys one room contributes to an XREAD: its
// sync stream and its awareness stream. readOnce batches rooms in groups of
// maxKeysPerRead/streamsPerRoom; batching against maxKeysPerRead directly would
// build a command with twice the intended number of keys.
const streamsPerRoom = 2

// maxEntriesPerStream bounds how many entries one XREAD pulls from a single
// stream, and so bounds one catch-up cycle's memory and the batch handed to
// crdt.MergeUpdatesV1. A deeper backlog is not lost: the cursor advances and
// the next cycle takes the next slice. Its own constant rather than reusing
// maxKeysPerRead as XREAD's Count, since a key budget and an entry budget only
// happen to share a number today.
const maxEntriesPerStream = 512

// readErrorBackoff is the pause after a failed XREAD, so an unreachable Redis
// is retried at a bounded rate. Deliberately not stalledBackoffBase, which is
// the LANE-backpressure backoff: one knob for both would make either one's
// tuning change the other's behaviour.
const readErrorBackoff = 100 * time.Millisecond

// deferredReadBackoff is the pause after a cycle that left entries under their
// cursor because a room had no delivery worker yet (see handleStream). Without
// it the cycle spins, re-reading the same entries as fast as Redis can answer.
//
// Short because the window it waits out is short — inside RoomActivated,
// between a room being assigned to a reader and its worker existing. Applied
// per cycle, not per stream, so a co-batched room can pay this much latency in
// that window: deliberate, to keep pacing one decision per cycle. Lane
// backpressure escalates instead — see stallBackoff.
const deferredReadBackoff = 20 * time.Millisecond

// nonBlockingRead is the XReadArgs.Block value for a read that must not wait. It
// must be NEGATIVE, not zero: go-redis emits the BLOCK argument for any
// Block >= 0, and Redis reads "BLOCK 0" as block FOREVER.
const nonBlockingRead = -1 * time.Nanosecond

// cursorLimit bounds how many stream cursors are remembered. SYNC cursors only
// — the only kind this map holds, and they deliberately outlive any one
// residency (see roomWorker.awCursor for the retention rule), so they
// accumulate across room churn. See evictStaleCursorsLocked for what goes.
const cursorLimit = 4096

// streamTarget is one XREAD key together with what it means. readBatch already
// knows the room and the kind, so carrying them forward means handleStream
// never derives a room from a key, and a returned key nobody asked for is
// something to ignore rather than to interpret.
type streamTarget struct {
	key         string
	room        string
	isAwareness bool
	// residency is the roomWorker that owned the room when this read's id
	// vector was built, and is the fence awareness delivery is accepted against
	// (see deliverAwareness). Sync delivery is deliberately unfenced, because
	// replaying a sync entry is idempotent whereas replaying presence
	// resurrects departed clients.
	//
	// A worker POINTER, not a generation counter: a live reference keeps a
	// retired worker's address from being recycled, where a counter could be
	// deleted and reissued, comparing equal across residencies.
	residency *roomWorker
}

// readResult is what one batch's XREAD found: what the next batch's blocking
// decision and the cycle's pacing are made from.
type readResult struct {
	// got: at least one entry came back for some key in the batch.
	got bool
	// deferred: entries were deliberately left under their cursor, for EITHER
	// cause (see streamOutcome) — both mean the next XREAD returns them again
	// immediately and so has to be paced.
	deferred bool
	// stalled narrows that to lane backpressure, which escalates its backoff: a
	// wedged consumer can last minutes where a missing worker is a
	// sub-millisecond window. See stallBackoff.
	stalled bool
}

// streamOutcome is what handleStream did with one stream's entries. Three
// outcomes rather than a bool, because the two non-consuming ones look alike to
// a caller that only learns "the cursor did not move", yet need different
// pacing and different counters.
type streamOutcome int

const (
	// streamConsumed: entries dealt with — delivered, filtered or dropped —
	// and the cursor advanced past them.
	streamConsumed streamOutcome = iota
	// streamUnready: no delivery worker yet, so nowhere to put them.
	// StreamStats.Deferred.
	streamUnready
	// streamStalled: the room's lane is at capacity, so delivering would force
	// it to coalesce. StreamStats.Stalled.
	streamStalled
)

// streamReadCtx derives the readers' context from the relay's bound context so
// that Close cancels it too: a caller's context ordinarily OUTLIVES the relay
// (Server cancels relayCtx after Close), and readers bound to one would keep
// issuing XREADs against a closed relay.
//
// It does NOT shorten Close — an in-flight XREAD runs to its block deadline
// whatever happens to its context (see maxReadBlock). What ends a reader is the
// r.closed check in runStreamReader's loop, bounded by ReadBlock.
//
// The watcher is on r.wg like every other relay goroutine, and cannot
// self-deadlock: it exits on r.done, which Close closes BEFORE wg.Wait.
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

// blockForBatch is how long batch i of n may block: only the LAST batch of a
// cycle does, and only when every batch before it came back empty.
//
// Blocking each batch in turn would make inbound latency scale with batch COUNT
// rather than with Redis — a reader owning 2500 rooms has 10 batches, so an
// entry in batch 10 would sit up to 10 x ReadBlock behind an idle cluster. The
// trade is command rate: that reader went from ~4 commands a second to ~40, and
// its shutdown and activation latency divided by its batch count, which is what
// Config.ReadBlock's bound is supposed to mean.
func (r *Relay) blockForBatch(i, n int, sawEntries bool) time.Duration {
	if i == n-1 && !sawEntries {
		return r.scfg.readBlock
	}
	return nonBlockingRead
}

// stallBackoff is the pause after n consecutive cycles that declined a cursor
// advance for lane backpressure: doubling from stalledBackoffBase, capped at
// limit (the reader's ReadBlock). Doubling stops a room whose consumer is
// wedged for minutes from re-reading as fast as Redis can answer; the streak
// resets on any non-stalled cycle, so recovery takes one cycle.
//
// Capped at ReadBlock because that is already this tier's stated bound on
// inbound latency and on how long Close and a newly activated room wait (see
// maxReadBlock). The cap can therefore be SMALLER than stalledBackoffBase —
// ReadBlock accepts values down to 1ms — and the cap wins.
//
// Multiplication, not stalledBackoffBase << n-1: the streak has no upper bound
// and that shift goes negative at n=38.
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
// long to wait, and what the stall streak becomes. Pure, so escalate-and-reset
// is asserted directly rather than inferred from goroutine timing.
//
// stalls is per READER, not per room: the pause is one decision per cycle
// anyway, escalation is driven by the reader's worst room, and a per-room map
// would need its own bound and eviction pass. A stalled cycle outranks a merely
// deferred one, a wedged consumer outlasting an activation window by orders of
// magnitude.
func nextPause(res readResult, stalls int, readBlock time.Duration) (time.Duration, int) {
	switch {
	case res.stalled:
		stalls++
		return stallBackoff(stalls, readBlock), stalls
	case res.deferred:
		// The next XREAD returns the same entries immediately; pace the retry
		// instead of spinning. See deferredReadBackoff.
		return deferredReadBackoff, 0
	default:
		return 0, 0
	}
}

// runStreamReader is one reader goroutine: it owns the rooms hash-assigned to
// idx and multiplexes them over XREAD. It carries the pacing state, because the
// stall streak is the one thing this loop needs from the PREVIOUS cycle and
// readOnce is deliberately stateless.
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
			// The streak is deliberately left alone: a Redis error says nothing
			// about whether the lane that stalled has drained.
			continue
		}
		var d time.Duration
		if d, stalls = nextPause(res, stalls, r.scfg.readBlock); d > 0 {
			r.pause(ctx, d)
		}
	}
}

// readOnce performs one XREAD cycle for the reader's rooms and reports what it
// saw. It does not pace itself: the retry pause depends on the stall streak,
// which runStreamReader keeps.
//
// A failing batch abandons the cycle and runStreamReader restarts from batch 0,
// re-reading the earlier batches. Deliberate: those are non-blocking reads
// whose cursors did not move, so the re-read costs one round trip each and
// cannot double-deliver — advancing a cursor is what marks an entry consumed.
func (r *Relay) readOnce(ctx context.Context, idx int) (readResult, error) {
	var cycle readResult

	rooms := r.roomsForReader(idx)
	if len(rooms) == 0 {
		// XREAD with zero keys is invalid, so an idle reader sleeps instead of
		// blocking on Redis. Bounded by ReadBlock, because this wait is also
		// how long a newly activated room and Close wait on this reader.
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

// readBatch reads one XREAD's worth of rooms: each room's sync stream from the
// relay's cursor (default: oldest retained entry) and its awareness stream from
// its RESIDENCY's cursor (default: the tail). See oldestID/tailID for why the
// defaults differ and roomWorker.awCursor for why the cursors live apart.
func (r *Relay) readBatch(ctx context.Context, rooms []string, block time.Duration) (readResult, error) {
	var out readResult

	n := len(rooms) * streamsPerRoom
	targets := make(map[string]streamTarget, n)
	keys := make([]string, 0, n)
	ids := make([]string, 0, n)

	// from is resolved by the caller: the two kinds read their cursor out of
	// different places, and the awareness one has to be resolved together with
	// the residency that owns it.
	add := func(tgt streamTarget, from string) {
		// Unreachable for distinct rooms — syncKey and awKey cannot collide for
		// ANY pair of room names (see the kindDiscrim constants) and
		// roomsForReader yields each room once — but kept as the structural
		// guarantee that keys and ids stay index-aligned.
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
		// Resolved as a pair under one lock: the awareness position and the
		// residency it belongs to are one fact.
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
			// The block expired, or a non-blocking read found nothing past the
			// cursors. Normal.
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
			// Ignored, never guessed at: attribution comes from the key set
			// this call built, so a key nobody asked for has no room to be
			// delivered to. parseStreamKey only makes the log actionable, and
			// says "unrecognised" rather than coercing a foreign key into a
			// room name.
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
// reads, keeping every cursor whose room is still assigned.
//
// The selectivity is the point: on a node with more than cursorLimit/2 active
// rooms, evicting an ARBITRARY entry lands on a LIVE room's cursor about half
// the time, and a live room that loses its cursor replays its entire retention
// window — the condition StreamStats.Replayed tells operators to alert on.
//
// If every cursor is live the map is left above cursorLimit, which is correct:
// the residual is then bounded by real load, the same order as streamRooms.
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
// or reports which of the two reasons it left them for a later cycle.
//
// Entries go to the room's LANE, never to Sink.Inject directly, for four
// reasons:
//
//  1. Inject is the caller's code and may be arbitrarily slow, and one reader
//     serves up to maxKeysPerRead/streamsPerRoom rooms — injecting inline is
//     the head-of-line stall #187 exists to remove.
//  2. The Sink contract (cluster/relay.go) requires calls for the SAME room to
//     be serialised, and under Transport Both the pub/sub router pushes to the
//     lane while a direct Inject ran — two concurrent Injects for one room.
//  3. Close promises nothing reaches the Sink after it returns (redis.go),
//     enforced by drainLane's closed check; a direct Inject has no such gate.
//  4. Lane.Push never blocks on capacity and never drops, so the isolation
//     costs nothing.
//
// Push does take the lane's mutex, so no relay-wide lock may be held across it
// — see deliverAwareness.
//
// Sync entries are MERGED into one payload first: otherwise a catch-up of N
// entries pushes N payloads, each re-broadcast to every local peer, turning one
// reader's restart into an N-fold broadcast storm. Awareness is not merged
// (each payload carries its own clock and the receiver gates per client), but
// only the LAST payload of a read is pushed, because the lane's awareness slot
// holds one blob and would supersede the rest untouched. Awareness also goes
// via deliverAwareness, which drops the read if the room changed residency
// (see streamTarget.residency).
func (r *Relay) handleStream(tgt streamTarget, msgs []goredis.XMessage) streamOutcome {
	if len(msgs) == 0 {
		return streamConsumed
	}

	// Resolved once, as the router does: a room retired mid-batch leaves a
	// stale handle, the same accepted staleness runSubscriber has.
	w, resident := r.workerForInbound(tgt.room)
	if !resident {
		// The room is in this reader's assignment set but has no delivery
		// worker: RoomActivated adds to streamRooms BEFORE it creates one.
		//
		// Every entry stays under its cursor and is read again next cycle.
		// Advancing would consume the room's entire retained backlog — a reader
		// reaching a brand-new room reads from the oldest retained entry — and
		// discard it before the worker existed to receive any of it: silent
		// loss of precisely what this tier prevents. Deferring costs one read.
		//
		// routerDrops is deliberately NOT incremented: nothing was dropped, and
		// operators watch its rate. StreamStats.Deferred counts this instead.
		r.deferred.Add(1)
		return streamUnready
	}

	// This branch is the whole argument for this tier over pub/sub: when a
	// room's consumer falls behind, pub/sub can only merge the backlog or drop
	// it, because the message exists nowhere else. A stream entry is durable, so
	// there is a third option — leave the entries under the cursor and read them
	// again next cycle, discarding nothing and merging nothing.
	//
	// Placed BEFORE the decode loop, not before the push, because the loop calls
	// noteSeq: stalling after it would leave lastSeq holding sequences from
	// entries about to be re-read, so next cycle's lower numbers would classify
	// as a publisher RESTART — inflating StreamStats.Restarts once per stalled
	// cycle on a healthy cluster. The unready branch returns early likewise.
	//
	// Awareness is exempt: an awareness push replaces a single latest-only
	// slot, which Lane.Full's cap does not govern, so declining would cost
	// presence freshness and buy nothing.
	if !tgt.isAwareness && w.lane.Full() {
		r.stalled.Add(1)
		return streamStalled
	}

	syncPayloads := make([][]byte, 0, len(msgs))
	// Collected rather than pushed inline: the push and the cursor advance have
	// to happen as ONE residency-fenced operation — see deliverAwareness.
	var awPayloads [][]byte
	lastID := ""
	for _, msg := range msgs {
		// Recorded before every skip below, deliberately: a malformed, foreign
		// or self-published entry is fully accounted for, and leaving it under
		// the cursor would re-read it forever. The one skip that must NOT
		// advance the cursor returned above.
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
		// publishStream chooses the key from the kind, so a disagreement means
		// a foreign or corrupt writer; trusting either side would either replay
		// presence or merge a non-update blob.
		if tgt.isAwareness != (kind == cluster.KindAwareness) {
			r.log.Warn("cluster/redis: stream/kind mismatch; drop",
				"stream", tgt.key, "room", tgt.room, "kind", kind)
			r.routerDrops.Add(1)
			continue
		}
		if tgt.isAwareness {
			// Not counted in noteSeq: awareness is read from the tail and
			// bounded to AwarenessMaxLen, so skipped sequence numbers are the
			// designed behaviour, and feeding them to gap detection would make
			// Gaps — "alert on presence" — nonzero on every healthy node.
			awPayloads = append(awPayloads, data)
			continue
		}
		r.noteSeq(tgt.key, nodeID, seq)
		syncPayloads = append(syncPayloads, data)
	}

	if tgt.isAwareness {
		// Declining inside deliverAwareness is a deliberate discard of presence
		// published before a reactivation, not a deferral — the successor
		// residency reads from the tail — hence streamConsumed unconditionally.
		r.deliverAwareness(tgt.room, tgt.residency, awPayloads, lastID)
		return streamConsumed
	}

	return r.applySync(w, tgt, syncPayloads, lastID)
}

// applySync delivers one stream's sync entries and advances its cursor, but
// only while w is still the room's residency: stopWorker can retire w after
// handleStream resolved it, and a worker whose final drain has already run
// leaves the payload on a lane nobody reads, so advancing past it would lose
// the entry for good. Declining re-reads next cycle onto the successor; the
// duplicate push onto the retired lane is harmless because V1 is idempotent.
//
// The check follows the push rather than sharing one lock with it, for the
// reason set out in deliverAwareness: workersMu held across a Push couples
// every room on the node to one room's merge (#187).
func (r *Relay) applySync(w *roomWorker, tgt streamTarget, payloads [][]byte, lastID string) streamOutcome {
	if len(payloads) > 0 {
		r.pushSync(w, tgt, payloads)
		if !r.stillResident(tgt.room, w) {
			r.deferred.Add(1)
			return streamUnready
		}
	}
	// Every path that reaches here has finished with the entries it read.
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
		// rebroadcast is a cost, losing the entries would be divergence.
		r.log.Warn("cluster/redis: catch-up merge failed; injecting individually",
			"room", tgt.room, "entries", len(payloads), "err", err)
		for _, p := range payloads {
			w.lane.Push(cluster.KindSync, p)
		}
		return
	}
	w.lane.Push(cluster.KindSync, merged)
	// Counted on EVERY successful multi-entry merge, delivered before or not,
	// which is why StreamStats.Replayed is a merge/batching gauge rather than a
	// counter to alert on.
	r.replayed.Add(uint64(len(payloads) - 1))
}

// seqSource identifies one sequence series: one publishing node's entries in
// one stream. A struct key rather than a concatenated string because both
// halves are arbitrary bytes, so any joining character could be forged by one
// half into the other's territory.
type seqSource struct {
	node   string // publisher nodeID, as raw bytes held in a string
	stream string // stream key the entry was read from
}

// seqState is what this reader remembers about one sequence series.
// afterDecrease rides alongside the number because "the previous observation
// went backwards" is not recoverable from the baseline value alone; see noteSeq.
type seqState struct {
	last          uint64
	afterDecrease bool
}

// noteSeq tracks one source node's sequence numbers ON ONE STREAM and
// classifies what it sees.
//
// Keyed by (nodeID, stream key), matching nextSeq's per-stream counter. Both
// halves are needed: nodeID keeps two nodes' series apart, and the stream key
// keeps one node's two streams apart — their legitimate interleaving, [1 3 5]
// in one and [2 4 6] in the other, would otherwise read as gaps.
//
// A JUMP is a provable gap, and the only detectable form of loss: XREAD from a
// trimmed ID returns the next surviving entry with no error, and stream IDs are
// ms-seq rather than contiguous.
//
// A DECREASE is a restart, not a gap — counters are in-memory, so a node
// restarts them at 0, under the same identity if its NodeID is STATIC. As a gap
// it would cry data loss on every deploy; as already-seen it would stall that
// source forever.
//
// THE FIRST COMPARISON AFTER A DECREASE CANNOT REPORT A GAP, because two
// routine causes put entries out of order. nextSeq releases streamMu before its
// XADD and Relay.Publish's contract requires tolerating two concurrent
// Publishes for one room (an eviction/reload handoff does exactly that,
// continuously), so entries can land as [seq2, seq1] — a decrease, then a jump
// seq1→seq3; holding streamMu across the XAdd is refused, being a lock across a
// Redis call that couples every room to one slow write (#187/#200). And a
// restarted node's fresh [1 2 3…] can interleave with its own pre-restart tail.
//
// The suppression is one comparison deep, so it loses exactly one REAL gap: a
// trim-away landing immediately after a restart on the same series. Worth it,
// because StreamStats.Gaps is documented "ALERT ON PRESENCE… a single gap means
// data was lost" — a signal firing on healthy room churn is worth less than no
// signal, while one missed increment costs an alert any longer stall raises.
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
// reads, keeping every baseline whose room is still assigned: keying by (node,
// stream) turns a map bounded by CLUSTER SIZE into one that also grows with
// room churn. Same selective policy as evictStaleCursorsLocked.
//
// Safe in the same direction: a dropped baseline makes the next entry a FIRST
// entry, counted as neither gap nor restart. It can only hide a gap on a stream
// this node had stopped reading — where nothing was being delivered to lose —
// and can never invent one.
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
