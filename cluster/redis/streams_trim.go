// Package-internal: the Redis Streams delivery tier. See Config.Transport.
package redis

import (
	"context"
	"fmt"
	"time"
)

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
// else's to sweep, and its entries are still bounded by the inline MAXLEN, so
// scanning the keyspace for orphans would cost more than it saves.
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

	now := time.Now()
	for _, room := range rooms {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.trimKey(ctx, r.scfg.syncKey(room), now.Add(-r.scfg.retention)); err != nil {
			return err
		}
		if err := r.trimKey(ctx, r.scfg.awKey(room), now.Add(-r.scfg.awRetention)); err != nil {
			return err
		}
	}
	return nil
}

// trimKey removes entries older than cutoff from one stream.
//
// Stream IDs are milliseconds-sequence, so the cutoff is expressed as
// "<unix-millis>-0": every entry with a smaller ID is older than the window.
// A key that does not exist yet (a room activated but never published to)
// trims zero entries rather than erroring — XTRIM MINID on a missing key
// returns 0 in both Redis and miniredis.
func (r *Relay) trimKey(ctx context.Context, key string, cutoff time.Time) error {
	minID := fmt.Sprintf("%d-0", cutoff.UnixMilli())
	n, err := r.client.XTrimMinID(ctx, key, minID).Result()
	if err != nil {
		return fmt.Errorf("XTRIM MINID %s %s: %w", key, minID, err)
	}
	if n > 0 {
		r.trimmed.Add(uint64(n)) //nolint:gosec // XTRIM returns a non-negative count
	}
	return nil
}
