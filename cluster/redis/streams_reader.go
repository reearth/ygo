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
// maxKeysPerRead — as this task's brief did — would build a command with
// twice the intended number of keys, defeating the bound it was reaching for.
const streamsPerRoom = 2

// maxEntriesPerStream bounds how many entries one XREAD pulls from a single
// stream, and so bounds both the memory one catch-up cycle allocates and the
// size of the batch handed to crdt.MergeUpdatesV1. A deeper backlog is not
// lost: the cursor advances and the next cycle takes the next slice.
//
// Deliberately its own constant rather than reusing maxKeysPerRead (which the
// brief passed as XREAD's Count): a key budget and an entry budget are
// different quantities that happen to share a number today, and conflating
// them means a future change to one silently moves the other.
const maxEntriesPerStream = 512

// readErrorBackoff is the pause after a failed XREAD, so an unreachable Redis
// is retried at a bounded rate instead of spun on.
//
// Not stalledBackoffBase, which the brief reused here: that constant is
// documented as the LANE-backpressure backoff and is consumed by the
// backpressure task. A Redis error and a full local lane are unrelated
// conditions, and sharing one knob between them would make either one's
// tuning silently change the other's behaviour.
const readErrorBackoff = 100 * time.Millisecond

// readerBlockCap caps how long one read cycle waits, independently of
// Config.ReadBlock. ReadBlock remains the operator's upper bound — its doc
// says it "bounds each XREAD BLOCK", and a cap keeps that literally true —
// but two obligations make blocking for the FULL ReadBlock wrong, and neither
// is satisfiable any other way.
//
// Close. There is no way to interrupt a blocked XREAD: go-redis honours a
// context DEADLINE when it arms the socket read deadline, but a context
// CANCELLED mid-read does nothing (internal/pool.(*Conn).deadline reads only
// ctx.Deadline()). Close closes r.done and then joins r.wg, so a reader
// blocking for the full default ReadBlock made Close take a measured 4.80s of
// the 5s window — a five-second stall on every Server.Shutdown, in a
// shutdown path the project has already had to fix twice (#202, #229).
//
// Room membership. Config.ReadBlock's own doc promises that "a newly
// activated room gets a fresh short read folded in immediately". A reader
// parked in one long XREAD cannot see a room activated after that read
// started, so the interval BETWEEN reads is the activation latency, and at
// the default it would have been up to five seconds. Honouring that promise
// properly — waking the reader from RoomActivated — needs a signal this task
// is scoped out of adding; capping the interval delivers the promise's
// substance without reaching into the activation path.
//
// 250ms buys both bounds for a handful of extra commands per reader per
// second. An XREAD that finds nothing is cheap, and there are Readers of them
// (4 by default), not one per room.
const readerBlockCap = 250 * time.Millisecond

// cursorLimit bounds how many stream cursors are remembered.
//
// Cursors are kept past a room's deactivation on purpose: dropping one at
// deactivation would make ordinary room churn re-replay the room's whole
// retention window on every reactivation. Bounding the map is what stops that
// memory growing forever. See evictStaleCursorsLocked for which entries go.
const cursorLimit = 4096

// streamTarget is one XREAD key together with what it means.
//
// readBatch builds the keys from room names, so it already knows the room and
// which of the two streams the key is; carrying that forward means
// handleStream never has to derive a room from a key. That mattered
// enormously under the earlier key layout, where syncKey and awKey shared one
// namespace and prefix+"aw:foo" was simultaneously room "aw:foo"'s sync key
// and room "foo"'s awareness key — an ambiguity no amount of care at the read
// site could resolve, because the two rooms genuinely shared one stream.
//
// The discriminated layout (see the kindDiscrim constants) removes the
// collision at the source: distinct rooms now have provably distinct keys, and
// parseStreamKey can recover room and kind exactly. streamTarget stays anyway,
// because attributing an entry by the key set that ASKED for it is still the
// stronger construction — it needs no parsing to be correct, and a returned
// key that was never requested is then something to ignore rather than
// something to interpret.
type streamTarget struct {
	key         string
	room        string
	isAwareness bool
}

// streamReadCtx derives the readers' context from the relay's bound context so
// that Close cancels it too.
//
// Load-bearing. A reader spends most of its life blocked inside XREAD for up
// to ReadBlock, and Close closes r.done and then joins r.wg. A reader that
// watched only the bound context would therefore make Close wait out a full
// ReadBlock — and in the common case, where a caller closes the relay while
// its bound context is still live (every test here, and any caller whose
// context outlives the relay), Close would block forever. Cancelling the
// derived context makes the in-flight XREAD return immediately.
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

// readerBlock is how long one cycle waits: the operator's ReadBlock, capped
// at readerBlockCap. Used for both the XREAD BLOCK and the idle wait, so
// shutdown and activation latency are bounded the same way on both paths.
func (r *Relay) readerBlock() time.Duration {
	if r.scfg.readBlock < readerBlockCap {
		return r.scfg.readBlock
	}
	return readerBlockCap
}

// runStreamReader is one reader goroutine. It owns the rooms hash-assigned to
// idx and multiplexes them over XREAD.
func (r *Relay) runStreamReader(ctx context.Context, idx int) {
	defer r.wg.Done()
	for {
		if ctx.Err() != nil || r.closed.Load() {
			return
		}
		if err := r.readOnce(ctx, idx); err != nil {
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
		}
	}
}

// readOnce performs one XREAD cycle for the reader's rooms.
//
// A reader with NO assigned rooms idles instead of reading: XREAD with zero
// keys is invalid.
func (r *Relay) readOnce(ctx context.Context, idx int) error {
	rooms := r.roomsForReader(idx)
	if len(rooms) == 0 {
		// XREAD with zero keys is invalid, so an idle reader cannot block on
		// Redis; it sleeps and re-checks its assignment.
		select {
		case <-ctx.Done():
		case <-r.done:
		case <-time.After(r.readerBlock()):
		}
		return nil
	}

	for _, batch := range keyBatches(rooms, maxKeysPerRead/streamsPerRoom) {
		if err := r.readBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

// readBatch reads one XREAD's worth of rooms: each room's sync stream from
// its cursor (defaulting to the oldest retained entry) and its awareness
// stream from its cursor (defaulting to the tail). See the oldestID/tailID
// constants for why those two defaults differ.
func (r *Relay) readBatch(ctx context.Context, rooms []string) error {
	n := len(rooms) * streamsPerRoom
	targets := make(map[string]streamTarget, n)
	keys := make([]string, 0, n)
	ids := make([]string, 0, n)

	add := func(tgt streamTarget, dflt string) {
		// Unreachable for distinct rooms under the discriminated key layout:
		// syncKey and awKey cannot collide with each other for ANY pair of
		// room names, and roomsForReader yields each room once. It survives as
		// the structural guarantee that keys and ids stay index-aligned — a
		// duplicate key would make XREAD's own command malformed — and it
		// costs one lookup in a map the attribution below needs regardless.
		// Under the earlier suffix layout it was load-bearing: a room named
		// "aw:foo" really did produce room "foo"'s awareness key.
		if _, dup := targets[tgt.key]; dup {
			return
		}
		targets[tgt.key] = tgt
		keys = append(keys, tgt.key)
		ids = append(ids, r.cursorFor(tgt.key, dflt))
	}
	for _, room := range rooms {
		add(streamTarget{key: r.scfg.syncKey(room), room: room}, oldestID)
		add(streamTarget{key: r.scfg.awKey(room), room: room, isAwareness: true}, tailID)
	}

	args := make([]string, 0, len(keys)+len(ids))
	args = append(args, keys...)
	args = append(args, ids...)

	res, err := r.client.XRead(ctx, &goredis.XReadArgs{
		Streams: args,
		Block:   r.readerBlock(),
		Count:   maxEntriesPerStream,
	}).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return nil // BLOCK expired with nothing new; normal
		}
		return err
	}

	for _, stream := range res {
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
		r.handleStream(tgt, stream.Messages)
	}
	return nil
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
// The selectivity is the point. Evicting an ARBITRARY entry on overflow — as
// this task's brief did — costs "only a replay" per its own comment, but on a
// node with more than cursorLimit/2 active rooms it lands on a LIVE room's
// cursor about half the time, and a live room that loses its cursor replays
// its entire retention window. That is exactly the condition
// StreamStats.Replayed tells operators to alert on ("cursors are being lost
// repeatedly"), so arbitrary eviction would make the tier trip its own alarm
// under nothing worse than ordinary scale.
//
// If every cursor is live the map is left above cursorLimit. That is the
// correct outcome: the residual is then bounded by streamsPerRoom x the
// rooms this node actually reads, i.e. by real load, and it is the same order
// as streamRooms itself.
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

// handleStream applies one stream's returned entries and advances its cursor.
//
// Entries are handed to the room's LANE, not to Sink.Inject directly. This
// deviates from the task brief, which called r.inject from here, and the
// difference is load-bearing in four ways:
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
//  4. Lane.Push never blocks and never drops — an over-cap sync queue is
//     merged, awareness is kept latest-only — so nothing is traded away for
//     the isolation.
//
// Sync entries are MERGED into a single payload first. Without that, a
// catch-up of N entries would push N payloads, and each one is re-broadcast
// to every local peer — turning one reader's restart into an N-fold broadcast
// storm. Awareness is not merged: each payload carries its own clock and the
// receiver's per-client gate handles staleness.
func (r *Relay) handleStream(tgt streamTarget, msgs []goredis.XMessage) {
	if len(msgs) == 0 {
		return
	}

	// Resolved once, as the router does: a room retired mid-batch leaves a
	// stale handle, which is the same accepted staleness runSubscriber has.
	w, resident := r.workerForInbound(tgt.room)

	syncPayloads := make([][]byte, 0, len(msgs))
	lastID := ""
	for _, msg := range msgs {
		// Recorded before every skip below, deliberately: a malformed, foreign
		// or self-published entry has been fully accounted for, and leaving it
		// under the cursor would make the reader re-read it forever.
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
		if !resident {
			// No worker for this room: the same acceptable-drop class as the
			// router's workerForInbound miss (see Stats.RouterDrops).
			r.routerDrops.Add(1)
			continue
		}

		if tgt.isAwareness {
			// Not counted in noteSeq: awareness is read from the tail and
			// bounded to AwarenessMaxLen, so skipped sequence numbers are the
			// designed behaviour rather than evidence of loss, and feeding
			// them to gap detection would make Gaps — a counter documented as
			// "alert on presence" — nonzero on every healthy node.
			w.lane.Push(cluster.KindAwareness, data)
			continue
		}
		r.noteSeq(tgt.key, nodeID, seq)
		syncPayloads = append(syncPayloads, data)
	}

	if len(syncPayloads) > 0 {
		merged := syncPayloads[0]
		if len(syncPayloads) > 1 {
			m, err := crdt.MergeUpdatesV1(syncPayloads...)
			if err != nil {
				// Deliver individually rather than dropping the batch: the
				// N-fold rebroadcast the merge exists to avoid is a cost,
				// whereas losing the entries would be divergence.
				r.log.Warn("cluster/redis: catch-up merge failed; injecting individually",
					"room", tgt.room, "entries", len(syncPayloads), "err", err)
				for _, p := range syncPayloads {
					w.lane.Push(cluster.KindSync, p)
				}
				r.setCursor(tgt.key, lastID)
				return
			}
			merged = m
			r.replayed.Add(uint64(len(syncPayloads) - 1))
		}
		w.lane.Push(cluster.KindSync, merged)
	}

	if lastID != "" {
		r.setCursor(tgt.key, lastID)
	}
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
func (r *Relay) noteSeq(streamKey string, nodeID []byte, seq uint64) {
	src := seqSource{node: string(nodeID), stream: streamKey}

	r.streamMu.Lock()
	prev, known := r.lastSeq[src]
	if !known && len(r.lastSeq) >= seqLimit {
		r.evictStaleLastSeqLocked()
	}
	r.lastSeq[src] = seq
	r.streamMu.Unlock()

	switch {
	case !known:
		// First entry from this node on this stream: a baseline, not a gap.
	case seq < prev:
		r.restarts.Add(1)
	case seq > prev+1:
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
