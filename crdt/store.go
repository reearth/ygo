package crdt

import "sort"

// StructStore holds all Items across all clients in the document.
// Items for each client are stored in a slice sorted by Clock (append-only).
// This structure enables O(log n) lookup by ID via binary search and O(1) append.
type StructStore struct {
	clients map[ClientID][]*Item

	// pending holds items whose Origin / OriginRight / Parent references
	// clocks not yet integrated, and items that form a same-client clock
	// gap with the integrated state. Retried at the end of every
	// ApplyUpdateV1 / ApplyUpdateV2 call. nil when empty.
	//
	// See docs/superpowers/specs/2026-04-23-cross-update-origin-resolution-design.md
	// for the full rationale. Matches Yjs JS's pendingStructs and yrs's Store.pending.
	pending *pendingUpdate

	// pendingDs holds delete-set entries targeting items not yet integrated.
	// Accumulated across updates and retried whenever pending drains.
	pendingDs DeleteSet
	// pendingDsRanges counts pendingDs's ranges so an apply need not.
	pendingDsRanges int
	// pendingDsMoved holds the parked clients given a struct since the last
	// retry, the only ones whose ranges can apply.
	pendingDsMoved map[ClientID]struct{}

	// pendingMoves holds ContentMove items whose target item was not yet
	// integrated when the move itself integrated (e.g. a fresh peer applying a
	// merged update where the move's client sorts before the target's client).
	// Keyed by the target ID's ClientID. Each entry is retried whenever an item
	// for that client is integrated, so the move can claim its target as soon as
	// the target arrives — in this or any later update. Without this, the move's
	// arbitration (target.MovedBy = move) would be silently dropped and the array
	// would diverge by merge/integration order. nil until first used.
	// Same bounded-leak posture as pendingDs: a move whose target never
	// integrates (or that is itself deleted/undone) lingers here until the
	// next resolve pass for that target client — bounded by move-op count,
	// and it is never applied without its target.
	pendingMoves map[ClientID][]*Item

	// parked is allocated only for a transaction that parks (Transaction.parkRecord).
	parked *transactionParked
}

// pendingUpdate holds decoded items parked because of unresolved
// dependencies, plus a per-client watermark of the store's clock at
// park time. A retry is worth attempting when the store's current
// clock for any client in `missing` has advanced past its recorded value.
type pendingUpdate struct {
	// items are appended, or emptied in place (nil) when a merge drops them,
	// so a transaction can record its park as a view of this queue.
	items   []*Item
	dead    int         // the nil slots of items
	missing StateVector // clientID -> store clock at park time for that client
	// cover is the clocks items span, built on demand and kept by addCover.
	cover *pendingCover
	// tail is the single client's clock past every parked struct, when tail
	// is valid and no struct of another client parked; it lets a tail append
	// skip building cover.
	tailClient ClientID
	tail       uint64
	tailValid  bool
	// ready latches a clock past missing until the drain replaces this queue.
	ready bool
}

func newStructStore() *StructStore {
	return &StructStore{
		clients:   make(map[ClientID][]*Item),
		pendingDs: newDeleteSet(),
	}
}

// Append adds item to the store. Items must be appended in Clock order per client.
func (s *StructStore) Append(item *Item) {
	s.clients[item.ID.Client] = append(s.clients[item.ID.Client], item)
	s.noteAppend(item)
}

// noteAppend keeps parked-clock bookkeeping out of Append's inlined fast path.
func (s *StructStore) noteAppend(item *Item) {
	if p := s.pending; p != nil && !p.ready {
		if parkedAt, waits := p.missing[item.ID.Client]; waits && item.ID.Clock+item.Content.Len() > parkedAt {
			p.ready = true
		}
	}
	if _, parked := s.pendingDs.clients[item.ID.Client]; parked {
		if s.pendingDsMoved == nil {
			s.pendingDsMoved = make(map[ClientID]struct{})
		}
		s.pendingDsMoved[item.ID.Client] = struct{}{}
	}
}

// parkDeletes merges ds into pendingDs.
func (s *StructStore) parkDeletes(ds DeleteSet) {
	if len(s.pendingDs.clients) == 0 && len(ds.clients) > 1 {
		// Sized once, so a park of many clients does not regrow the map.
		s.pendingDs = DeleteSet{clients: make(map[ClientID][]DeleteRange, len(ds.clients))}
	}
	for client := range ds.clients {
		s.pendingDsRanges -= len(s.pendingDs.clients[client])
	}
	s.pendingDs.Merge(ds)
	for client := range ds.clients {
		s.pendingDsRanges += len(s.pendingDs.clients[client])
	}
}

// takeReadyDeletes removes and returns the parked ranges of clients whose new
// structs reach their lowest range, the only ones that can apply.
func (s *StructStore) takeReadyDeletes() DeleteSet {
	var ready DeleteSet
	for client := range s.pendingDsMoved {
		if ranges := s.pendingDs.clients[client]; len(ranges) > 0 && s.NextClock(client) > ranges[0].Clock {
			if ready.clients == nil {
				ready = newDeleteSet()
			}
			ready.clients[client] = ranges
			ready.order = append(ready.order, client)
			delete(s.pendingDs.clients, client)
			s.pendingDsRanges -= len(ranges)
		}
	}
	clear(s.pendingDsMoved)
	return ready
}

// Find returns the Item that contains the given ID, or nil if not found.
// An item with Clock c and length l contains IDs with clocks [c, c+l).
//
// The binary search uses only the start Clock (a plain integer comparison) to
// avoid calling Content.Len() — which requires a utf8.RuneCountInString scan —
// inside the hot O(log n) predicate. A single Content.Len() call after the
// search verifies that id.Clock falls within the candidate item's range.
func (s *StructStore) Find(id ID) *Item {
	items := s.clients[id.Client]
	n := len(items)
	if n == 0 {
		return nil
	}
	// Find the last item whose start Clock is ≤ id.Clock.
	idx := sort.Search(n, func(i int) bool {
		return items[i].ID.Clock > id.Clock
	}) - 1
	if idx < 0 {
		return nil
	}
	item := items[idx]
	if item.ID.Clock+item.Content.Len() > id.Clock {
		return item
	}
	return nil
}

// getItemCleanEnd returns the item ending at exactly (client, clock).
// If the item at that position spans past clock it is split so the returned
// item ends exactly at clock. Used when a new item's origin falls inside an
// existing multi-character item.
func (s *StructStore) getItemCleanEnd(txn *Transaction, client ClientID, clock uint64) *Item {
	item := s.Find(ID{Client: client, Clock: clock})
	if item == nil {
		return nil
	}
	end := item.ID.Clock + item.Content.Len() - 1
	if end == clock {
		return item
	}
	// Guard against malformed updates where clock < item.ID.Clock: the
	// subtraction would underflow, producing a huge splitAt that causes a
	// panic in Splice (N-H2).
	if clock < item.ID.Clock {
		return item
	}
	// Split so the left half ends exactly at clock.
	splitAt := clock - item.ID.Clock + 1
	splitItem(txn, item, splitAt)
	return item // item is now the left half, ending at clock
}

// getItemCleanStart returns the item starting at exactly (client, clock).
// If the item containing that position starts before clock it is split so the
// returned item starts exactly at clock. Used when a new item's right-origin
// falls inside an existing multi-character item — the conflict-scan loop in
// Item.integrate needs the right boundary to be a clean item start so the
// loop terminates at the correct position.
//
// Mirrors Yjs JS's `getItemCleanStart` and yrs's `get_item_clean_start`.
// Returns nil if no item contains the given clock (e.g. the target hasn't
// been integrated yet — the caller is responsible for parking via the
// pending queue in that case).
func (s *StructStore) getItemCleanStart(txn *Transaction, id ID) *Item {
	item := s.Find(id)
	if item == nil {
		return nil
	}
	if item.ID.Clock == id.Clock {
		return item
	}
	// Guard against malformed updates where id.Clock < item.ID.Clock: Find
	// shouldn't return such an item, but bail defensively to avoid producing
	// an invalid splitAt that would panic in Splice.
	if id.Clock < item.ID.Clock {
		return item
	}
	// Split so the right half starts exactly at id.Clock.
	splitAt := id.Clock - item.ID.Clock
	return splitItem(txn, item, splitAt)
}

// addPendingMove records a ContentMove whose target was not resolvable at
// integration time, keyed by the target's ClientID, so it can be retried when
// that client's items arrive. See the pendingMoves field doc.
func (s *StructStore) addPendingMove(targetClient ClientID, move *Item) {
	if s.pendingMoves == nil {
		s.pendingMoves = make(map[ClientID][]*Item)
	}
	s.pendingMoves[targetClient] = append(s.pendingMoves[targetClient], move)
}

// resolvePendingMoves retries every deferred ContentMove whose target belongs to
// client. Called after an item for that client integrates: the target may now be
// present, letting the move claim it. Moves that resolve are dropped from the
// pending set; the arbitration rule (moveBeats) is order-independent,
// so retrying deferred moves yields the same MovedBy on every peer regardless of
// the order in which moves and targets were integrated.
func (s *StructStore) resolvePendingMoves(txn *Transaction, client ClientID) {
	moves := s.pendingMoves[client]
	if len(moves) == 0 {
		return
	}
	remaining := moves[:0]
	for _, mv := range moves {
		cm, ok := mv.Content.(*ContentMove)
		if !ok || mv.Deleted || cm.Target == nil {
			// Move item was deleted (its move undone) or is malformed — drop it.
			continue
		}
		target := resolveMovedItem(txn, cm.Target, cm.TargetLen)
		if target == nil {
			// Target still not present — keep waiting.
			remaining = append(remaining, mv)
			continue
		}
		if moveBeats(mv, target.MovedBy) {
			txn.setMovedBy(target, mv)
			// Setting MovedBy re-renders the target at the move's destination and
			// blanks its original slot, so any cached position marker is stale.
			if mv.Parent != nil {
				mv.Parent.clearMarkers()
			}
		}
	}
	if len(remaining) == 0 {
		delete(s.pendingMoves, client)
	} else {
		s.pendingMoves[client] = remaining
	}
}

// StateVector computes the current state vector: for each client, the clock of
// the last item + its length (i.e. the next expected clock from that client).
func (s *StructStore) StateVector() StateVector {
	sv := make(StateVector, len(s.clients))
	for client, items := range s.clients {
		if len(items) > 0 {
			last := items[len(items)-1]
			sv[client] = last.ID.Clock + last.Content.Len()
		}
	}
	return sv
}

// NextClock returns the next available clock value for the given client.
func (s *StructStore) NextClock(client ClientID) uint64 {
	items := s.clients[client]
	if len(items) == 0 {
		return 0
	}
	last := items[len(items)-1]
	return last.ID.Clock + last.Content.Len()
}

// insertItem inserts item into the per-client slice at the correct clock position.
// Used when splitting an existing item to register the right half.
func (s *StructStore) insertItem(item *Item) {
	items := s.clients[item.ID.Client]
	pos := sort.Search(len(items), func(i int) bool {
		return items[i].ID.Clock >= item.ID.Clock
	})
	items = append(items, nil)
	copy(items[pos+1:], items[pos:])
	items[pos] = item
	s.clients[item.ID.Client] = items
}

// IterateFrom calls fn for every Item whose ID is not yet in sv,
// visiting items in client order, then clock order.
func (s *StructStore) IterateFrom(sv StateVector, fn func(*Item)) {
	for client, items := range s.clients {
		start := sv.Clock(client)
		for _, item := range items {
			if item.ID.Clock >= start {
				fn(item)
			}
		}
	}
}

// retryable reports whether an append advanced a clock the pending queue waits on.
func (s *StructStore) retryable() bool {
	return s.pending != nil && s.pending.ready
}

// mergePendingMissing sets missing[client] to the minimum of its
// current value and clk — matching yrs' StateVector::set_min. Used at
// park time to accumulate the tightest watermark across multiple items
// referencing the same client.
func mergePendingMissing(missing StateVector, client ClientID, clk uint64) {
	if existing, ok := missing[client]; ok && existing <= clk {
		return
	}
	missing[client] = clk
}
