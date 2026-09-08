package redis

// StreamStats is a point-in-time snapshot of the Streams tier's counters.
//
// Deliberately separate from Stats rather than extra fields on it. Stats
// carries pub/sub concepts (Coalesced, AwarenessSuperseded, HardDrops) that
// are permanently zero under Streams, and these are permanently zero under
// pub/sub. A type whose fields are meaningless half the time is how a metric
// stops being trusted.
//
// Every counter is monotonic for the life of the Relay, so a scrape should
// take rates rather than absolute values — except Gaps, where presence alone
// is the signal.
type StreamStats struct {
	// Replayed counts entries re-delivered from before a cursor, i.e. the
	// merge savings on catch-up. Routine on activation and after a restart —
	// this is the tier working. Alert on a sustained rate, which means
	// cursors are being lost repeatedly.
	Replayed uint64

	// Gaps counts PROVABLE losses: a source node's seq jumped, so entries
	// existed and were trimmed before this reader reached them.
	//
	// ALERT ON PRESENCE, not on rate. A single gap means data was lost and
	// the retention window was too small for the reader's actual lag.
	Gaps uint64

	// Restarts counts source nodes observed restarting, inferred from a seq
	// DECREASE. Informational: expected once per node per deploy. It exists
	// so a restart is never miscounted as a Gap.
	Restarts uint64

	// Trimmed counts entries removed by the MINID sweeper. Routine.
	Trimmed uint64

	// Stalled counts cursor advances declined because a room's inbound lane
	// was full.
	//
	// Routine in bursts: declining is the safe response — the entries stay in
	// the stream and are read again next cycle, so nothing is discarded and
	// the lane is not made to coalesce. What that costs is lag, and lag is
	// only safe while the entries survive it: delivery is at-least-once within
	// min(retention, MaxLen/publish-rate), and a stall that outlasts that
	// window shows up as Gaps.
	//
	// Alert on a sustained rate, which means a room's consumer cannot keep up.
	// See Deferred for the other reason an advance is declined.
	Stalled uint64

	// Deferred counts cursor advances declined because the room had no inbound
	// delivery worker yet — the window inside RoomActivated between a room
	// joining a reader's assignment set and its worker existing. Routine and
	// self-clearing.
	//
	// What becomes of the deferred entries depends on which stream they were
	// on. On a SYNC stream they are re-read: that cursor is relay-scoped and
	// was not advanced, so the next cycle asks for the same entries and the
	// worker, once it exists, receives all of them. On an AWARENESS stream
	// they are NOT re-read — the baseline there is the residency's own cursor
	// (roomWorker.awCursor), which does not exist yet, so the next read
	// starts from tailID and those entries are already behind it. That is the
	// intended outcome rather than a loss to fix: presence appended before a
	// room had any local residency belongs to nobody here, and live clients
	// re-announce within one heartbeat interval.
	//
	// Deliberately NOT folded into Stalled, though both count a declined
	// advance on entries that were kept. The two ask for opposite responses: a
	// sustained Stalled rate means a room's consumer cannot keep up and wants
	// capacity, while a sustained Deferred rate means rooms are being read
	// without ever acquiring a worker, which is an activation bug and no
	// amount of capacity fixes it. One counter carrying both would answer
	// neither question — the same reason Restarts is not folded into Gaps.
	Deferred uint64
}

// StreamStats returns a snapshot of the Streams tier's counters. Safe to call
// concurrently, and safe on a PubSub-mode relay, where every field is zero.
func (r *Relay) StreamStats() StreamStats {
	return StreamStats{
		Replayed: r.replayed.Load(),
		Gaps:     r.gaps.Load(),
		Restarts: r.restarts.Load(),
		Trimmed:  r.trimmed.Load(),
		Stalled:  r.stalled.Load(),
		Deferred: r.deferred.Load(),
	}
}
