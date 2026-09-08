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
	// was full. Routine in bursts — declining to advance is the safe,
	// zero-loss response, and the entries are re-read next cycle. Alert on a
	// sustained rate, which means a room's consumer cannot keep up.
	Stalled uint64
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
	}
}
