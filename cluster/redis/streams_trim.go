// Package-internal: the Redis Streams delivery tier. See Config.Transport.
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// trimBatch is how many XTRIMs go into one pipelined round trip. Sequentially
// the 10k-room target is 20k round trips per sweep, which cannot finish inside
// TrimInterval at any realistic latency; 256 bounds one command's reply and
// keeps the ctx check frequent.
const trimBatch = 256

// trimTarget is one key and the MINID cutoff for its kind.
type trimTarget struct {
	key   string
	minID string
}

// runTrimSweeper enforces the age half of the delivery guarantee.
//
// Both strategies are required. Inline MAXLEN on XADD (see publishStream) is
// a memory ceiling with no age bound — a hot room's 4096 entries might be two
// seconds. MINID is an age bound with no memory ceiling. The honest window is
// therefore min(StreamRetention, StreamMaxLen/rate), which is what the docs
// state.
//
// The select watches r.done as well as ctx.Done(), matching runPublisher and
// runSubscriber rather than the stream readers' derived streamReadCtx. The
// readers need that derived context because a blocked XREAD cannot be
// interrupted by cancellation at all (go-redis arms the socket deadline from
// ctx.Deadline(), never watches ctx.Done() mid-read — see maxReadBlock), so
// they need the r.done→cancel forwarding streamReadCtx provides. A
// time.Ticker has no such blind spot: selecting on r.done directly ends the
// loop the instant Close fires, with no forwarding goroutine required. ctx
// ordinarily OUTLIVES the relay (Server cancels relayCtx only after Close
// returns — see streamReadCtx's doc), so a sweeper that watched only
// ctx.Done() would still be running when Close calls r.wg.Wait(), and would
// not exit until the caller cancels ctx afterwards — which for Server never
// happens before Close returns. That is a shutdown deadlock of the exact
// shape #202/#229 already fixed twice for other goroutines in this package.
func (r *Relay) runTrimSweeper(ctx context.Context) {
	defer r.wg.Done()
	t := time.NewTicker(r.scfg.trimInterval)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.trimOnce(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("cluster/redis: stream trim failed", "err", err)
			}
		}
	}
}

// trimOnce sweeps every active room's streams once.
//
// Only ACTIVE rooms are swept. A room nobody on this node holds is somebody
// else's to sweep while anybody holds it at all, and its ENTRIES are still
// bounded by the inline MAXLEN, so scanning the keyspace for orphans on every
// sweep would cost more than it saves.
//
// KNOWN LIMITATION, stated plainly because the trim story reads as complete
// without it: MAXLEN and MINID bound the SIZE of each stream, not the NUMBER
// of streams. Nothing in this package ever sets an EXPIRE, and a room that has
// gone idle on every node in the cluster is active nowhere, so it is swept
// nowhere — its two stream keys keep whatever they last held, for as long as
// the Redis instance lives. Redis memory therefore grows with the number of
// DISTINCT ROOMS EVER PUBLISHED TO, not with the number concurrently active,
// and operators must size for the former. An EXPIRE-based reclaim on each
// XADD (so an untouched key falls out on its own, with no keyspace scan and
// nothing to coordinate between nodes) is the intended fix and is tracked as a
// follow-up (#248); docs/CLUSTERING.md says the same in the cost model.
//
// The room list is copied out under streamMu and the lock released before any
// Redis call — streamMu must never be held across I/O (see its doc on
// Relay), and a sweep of thousands of rooms would otherwise pin the lock for
// the length of that many round trips, blocking RoomActivated/RoomDeactivated
// bookkeeping on every other room for the whole sweep.
func (r *Relay) trimOnce(ctx context.Context) error {
	r.streamMu.Lock()
	rooms := make([]string, 0, len(r.streamRooms))
	for room, n := range r.streamRooms {
		if n > 0 {
			rooms = append(rooms, room)
		}
	}
	r.streamMu.Unlock()

	if len(rooms) == 0 {
		return nil
	}

	// The cutoff must come from the clock that MINTED the ids: XADD * takes
	// its milliseconds from the Redis server, so a cutoff read off an app host
	// running ahead deletes entries still inside the window — and the streams
	// are shared, so one skewed node shrinks it for every node. One TIME per
	// sweep, because the drift a sweep's own duration adds is bounded by that
	// duration while host skew is not bounded at all; a failed TIME skips the
	// sweep, since trimming nothing only costs memory until the next tick.
	now, err := r.client.Time(ctx).Result()
	if err != nil {
		return fmt.Errorf("TIME: %w", err)
	}
	syncMinID := minIDAt(now.Add(-r.scfg.retention))
	awMinID := minIDAt(now.Add(-r.scfg.awRetention))

	targets := make([]trimTarget, 0, len(rooms)*streamsPerRoom)
	for _, room := range rooms {
		targets = append(targets,
			trimTarget{key: r.scfg.syncKey(room), minID: syncMinID},
			trimTarget{key: r.scfg.awKey(room), minID: awMinID},
		)
	}
	for start := 0; start < len(targets); start += trimBatch {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.trimChunk(ctx, targets[start:min(start+trimBatch, len(targets))])
	}
	return nil
}

// minIDAt renders a cutoff as an XTRIM MINID argument. Stream IDs are
// milliseconds-sequence, so every entry with a smaller id is older than the
// window.
func minIDAt(cutoff time.Time) string {
	return fmt.Sprintf("%d-0", cutoff.UnixMilli())
}

// trimChunk issues one pipelined round trip of XTRIMs and counts what they
// removed. A failing key is logged and the rest still go: aborting would leave
// every key behind it untrimmed until the next tick, and one log line per
// chunk rather than per key because a broken connection fails all of them
// alike.
//
// A key that does not exist yet (a room activated but never published to)
// trims zero entries rather than erroring — XTRIM MINID on a missing key
// returns 0 in both Redis and miniredis.
func (r *Relay) trimChunk(ctx context.Context, targets []trimTarget) {
	cmds := make([]*goredis.IntCmd, len(targets))
	_, _ = r.client.Pipelined(ctx, func(p goredis.Pipeliner) error {
		for i, t := range targets {
			cmds[i] = p.XTrimMinID(ctx, t.key, t.minID)
		}
		return nil
	})

	failed := 0
	var first error
	for i, cmd := range cmds {
		n, err := cmd.Result()
		if err != nil {
			if failed == 0 {
				first = fmt.Errorf("XTRIM MINID %s %s: %w", targets[i].key, targets[i].minID, err)
			}
			failed++
			continue
		}
		if n > 0 {
			r.trimmed.Add(uint64(n)) //nolint:gosec // XTRIM returns a non-negative count
		}
	}
	if failed > 0 && ctx.Err() == nil {
		r.log.Warn("cluster/redis: stream trim failed", "keys", failed, "err", first)
	}
}
