package redis

// StreamStats is a point-in-time snapshot of the Streams tier's counters.
//
// Separate from Stats rather than extra fields on it: Stats carries pub/sub
// concepts (Coalesced, AwarenessSuperseded, HardDrops) permanently zero under
// Streams, and these are permanently zero under pub/sub. A type whose fields
// are meaningless half the time is how a metric stops being trusted.
//
// Every counter is monotonic for the life of the Relay, so a scrape should take
// rates rather than absolute values — except Gaps, where presence is the signal.
type StreamStats struct {
	// Replayed counts entries a merge folded into another rather than pushing
	// separately: len(batch)-1 for every multi-entry batch handed to a lane.
	//
	// A MERGE/BATCHING gauge, not a replay alarm — do NOT alert on its rate. It
	// is not a count of entries re-delivered from before a cursor and cannot
	// distinguish catch-up from ordinary throughput: any room taking more than
	// one remote update per ReadBlock (250ms default) batches every cycle, so a
	// busy room shows a permanent, healthy rate with no cursor ever lost. Read
	// as volume it is useful — a catch-up burst is a spike against that room's
	// own baseline — but Gaps, Stalled and Deferred are what report loss.
	Replayed uint64

	// Gaps counts PROVABLE losses: a source node's seq jumped, so entries
	// existed and were trimmed before this reader reached them.
	//
	// ALERT ON PRESENCE, not on rate. A single gap means data was lost and the
	// retention window was too small for the reader's actual lag.
	//
	// Detects jumps only AFTER this process has observed a baseline for a
	// source, lastSeq being in-memory: the first entry seen from a source is a
	// baseline whatever its number, so loss that happened while this reader was
	// down is bounded by the retention window rather than reported here.
	Gaps uint64

	// Restarts counts source nodes observed restarting, inferred from a seq
	// DECREASE. Informational: expected once per node per deploy. It exists so
	// a restart is never miscounted as a Gap.
	Restarts uint64

	// Trimmed counts entries removed by the MINID sweeper. Routine.
	Trimmed uint64

	// Stalled counts cursor advances declined because a room's inbound lane was
	// full.
	//
	// Routine in bursts: the entries stay in the stream and are read again next
	// cycle, so nothing is discarded and the lane is not made to coalesce. What
	// that costs is lag, and lag is only safe while the entries survive it —
	// delivery is at-least-once within min(retention, MaxLen/publish-rate), and
	// a stall outlasting that window shows up as Gaps.
	//
	// Alert on a sustained rate, which means a room's consumer cannot keep up.
	Stalled uint64

	// Deferred counts cursor advances declined because the room had no inbound
	// delivery worker yet — the window inside RoomActivated between a room
	// joining a reader's assignment set and its worker existing. Routine and
	// self-clearing.
	//
	// SYNC entries are then re-read (the cursor is relay-scoped and was not
	// advanced), so the worker receives all of them once it exists. AWARENESS
	// entries are NOT: their baseline is the residency's own cursor
	// (roomWorker.awCursor), which does not exist yet, so the next read starts
	// from tailID and those entries are already behind it. Intended rather than
	// a loss to fix — presence appended before a room had any local residency
	// belongs to nobody here, and live clients re-announce within one interval.
	//
	// Deliberately NOT folded into Stalled: a sustained Stalled rate means a
	// consumer cannot keep up and wants capacity, while a sustained Deferred
	// rate means rooms are read without ever acquiring a worker, an activation
	// bug no capacity fixes.
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
