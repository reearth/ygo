package crdt

import (
	"cmp"
	"math"
	"slices"
	"sort"
	"strconv"

	"github.com/reearth/ygo/encoding"
)

// This file keeps ApplyReport.StateBytes: the bytes a remote apply adds to the
// document's encoded state, as EncodeStateAsUpdateV1(doc, nil) measures it,
// counted where the apply makes each change rather than by encoding the
// document (WithApplyReport).
//
// The encoded state is a sum of parts, and an apply changes them one at a
// time. For each client the store or the pending queue holds a struct of, it
// writes a section: a count of the section's entries, the client, the clock
// the section starts at, and each entry - a struct, or the skip struct that
// stands for clocks between two the section holds (withParked merges each
// parked struct into the store's encoding). Then the delete set: a count of
// its clients and, for each, the client, a count of its ranges and each
// range. The count follows each part where the apply changes it: a struct
// appended to the store (noteStateAppend), parked (noteStatePark), cut, grown
// or dropped by a park's merge into the queue (queueMerge), taken out of the
// queue by a drain (stateLeaving) or split (measureSplit); the commit's
// merges and collection, which take bytes out (squashRuns, tryMergeWithLeft,
// gcTxnDeleteSet); and the delete set, read once the apply's deletions are
// all made (deleteSetGrowth). What the queue holds of each client is read
// from a cover of its clocks built once per queue and kept up by each park
// (parkedCover), so no apply walks the queue.
//
// An apply that fails partway keeps what it integrated, and the count is of
// that: every change is counted where it is made, after the check that could
// refuse it. A drain that WithWholeOrNothingApplies takes back returns the
// count to where the drain began (applyTally.takeBackDrain), and an apply it
// rolls back makes no report.
//
// EncodeStateAsUpdateV1 writes the direct encode while nothing is pending,
// and a merge of it with the parked parts otherwise (withParked), and the two
// forms write two classes of value at different lengths. Each is counted at
// its longer form, from the apply that brings it to the one that removes it,
// so the total is at or above the state in either form: an integer outside
// int32 that a float32 holds exactly is a float64 of nine bytes in the direct
// form (anyIntLen) and, read back as a float64, a float32 of five merged; an
// integer in JSON text that a float64 rounds up a digit is a byte longer
// merged (jsonLen). The total runs four bytes above a merged state for each
// of the first and a byte above a direct state for each of the second.
//
// Some counts the state writes are not kept by the document, and where an
// apply changes one the report takes the most it can add rather than walk
// what holds it. Each bound over-counts and none under-counts, since an owner
// refuses from the total and a total short of the state is what the report
// exists to end:
//   - a client's delete ranges: where an apply adds ranges to a client that
//     had some, the varuint counting them is taken to grow by a byte once the
//     client holds more than 254 structs, the most that can hold 127 ranges
//     with every second struct deleted (by two where more than 127 ranges
//     join a client of more than 32,766 structs);
//   - the delete set's clients: where a client's first range joins a delete
//     set the store and the pending deletes could give more than 127 clients,
//     a byte;
//   - a struct appended, split or merged at clocks a parked struct shares,
//     which the state writes once (dedupClientStructs): the cut the merge can
//     make of either, 2 x (the varuint of the client + the varuint of the
//     clock it ends at) a struct (overlapSlack), and the parked copy's length
//     until it leaves the queue;
//   - a section's entries and the state's sections, where the queue holds
//     structs of the client and what it holds is not known exactly - where a
//     parked struct shares clocks with the store, or in a drain that dropped
//     a struct of the client - a byte where a varuint boundary falls within
//     what the count can be, and a park's skip and start clock at their
//     bound.
//   - a parked struct the drain drops: all of it is taken off but the skip
//     that may stand for its clocks, 1 + the varuint of its length, and its
//     client's section header stays counted where it was the section's only
//     struct and the state drops the section with it (stateLeaving.note).
//
// What the queue holds of each client is read before a park's merge changes
// it (measureQueueMerge). Read after, it lacks the clocks of each queued
// struct the merge dropped for the copy the update re-sent, and the copy,
// parking where the dropped struct was, would be priced against a gap the
// state never had: at the edge of what the queue held it would count again
// the skip or section header the state keeps, and between parked structs it
// would take off a skip the state never wrote. The merge's own change to the
// section, a GC struct grown over the skip after it or a dropped struct's
// entry, is counted there too (queueMerge.sectionChange).
//
// Whether a client held a deleted struct before, which decides whether the
// delete set gains its entry, is walked out from the apply's deletes to the
// nearest one (deletedOutside): a walk of the client's structs only for the
// first delete of a client that holds none.

// uvarLen is the bytes WriteVarUint writes for v.
func uvarLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// strLen is the bytes WriteVarString writes for s.
func strLen(s string) int { return uvarLen(uint64(len(s))) + len(s) }

// varIntLen is the bytes WriteVarInt writes for v: a sign bit and six bits of
// magnitude in the first byte, seven in each after.
func varIntLen(v int64) int {
	mag := uint64(v)
	if v < 0 {
		mag = uint64(-v)
	}
	if mag < 64 {
		return 1
	}
	n := 2
	for mag >>= 6; mag >= 128; mag >>= 7 {
		n++
	}
	return n
}

// anyLen is the bytes WriteAny writes for v, by the same dispatch.
func anyLen(v any) int {
	switch val := v.(type) {
	case nil, bool:
		return 1
	case int:
		return anyIntLen(int64(val))
	case int8:
		return anyIntLen(int64(val))
	case int16:
		return anyIntLen(int64(val))
	case int32:
		return anyIntLen(int64(val))
	case int64:
		return anyIntLen(val)
	case uint:
		return anyUintLen(uint64(val))
	case uint8:
		return anyIntLen(int64(val))
	case uint16:
		return anyIntLen(int64(val))
	case uint32:
		return anyIntLen(int64(val))
	case uint64:
		return anyUintLen(val)
	case float32:
		return 5
	case float64:
		if float64(float32(val)) == val {
			return 5
		}
		return 9
	case encoding.BigInt:
		return 9
	case string:
		return 1 + strLen(val)
	case []byte:
		return 1 + uvarLen(uint64(len(val))) + len(val)
	case []any:
		n := 1 + uvarLen(uint64(len(val)))
		for _, e := range val {
			n += anyLen(e)
		}
		return n
	case map[string]any:
		n := 1 + uvarLen(uint64(len(val)))
		for k, e := range val {
			n += strLen(k) + anyLen(e)
		}
		return n
	}
	// WriteAny panics on any other type, so no state holds one.
	return 0
}

// anyIntLen is writeAnyInt's length: a tag and a VarInt in int32 range, and a
// tag and eight bytes, as a float64 or a BigInt, beyond it.
func anyIntLen(v int64) int {
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		return 1 + varIntLen(v)
	}
	return 9
}

// anyUintLen is writeAnyUint's length.
func anyUintLen(v uint64) int {
	if v <= math.MaxInt64 {
		return anyIntLen(int64(v))
	}
	return 9
}

// structLen is the bytes encodeItem writes for item past offset: the struct as
// the encoded state carries it. With values false it leaves out the values a
// ContentAny or ContentJSON holds, for a split or a merge, which moves them
// from one struct to the other without changing their bytes.
func structLen(item *Item, offset uint64, values bool) int {
	if item.isGCOrphan() {
		return 1 + uvarLen(item.Content.Len()-offset)
	}
	return headerLen(item, offset) + contentLen(item.Content, offset, values)
}

// headerLen is what encodeItem writes for an item before its content: the info
// byte, the origins, and the parent and key of an item that has neither.
func headerLen(item *Item, offset uint64) int {
	n := 1
	origin := offset > 0 || item.Origin != nil
	if offset > 0 {
		n += uvarLen(uint64(item.ID.Client)) + uvarLen(item.ID.Clock+offset-1)
	} else if o := item.Origin; o != nil {
		n += uvarLen(uint64(o.Client)) + uvarLen(o.Clock)
	}
	if o := item.OriginRight; o != nil {
		n += uvarLen(uint64(o.Client)) + uvarLen(o.Clock)
		origin = true
	}
	if origin {
		return n
	}
	switch {
	case item.Parent != nil && item.Parent.item != nil:
		id := item.Parent.item.ID
		n += 1 + uvarLen(uint64(id.Client)) + uvarLen(id.Clock)
	case item.parentID != nil:
		n += 1 + uvarLen(uint64(item.parentID.Client)) + uvarLen(item.parentID.Clock)
	default:
		n += 1 + strLen(item.Parent.name)
	}
	if item.ParentSub != nil {
		n += strLen(*item.ParentSub)
	}
	return n
}

// contentLen is the bytes encodeContent writes for c past offset; values is
// structLen's.
func contentLen(c Content, offset uint64, values bool) int {
	switch ct := c.(type) {
	case *ContentDeleted:
		return uvarLen(ct.length - offset)
	case *ContentString:
		if offset == 0 {
			return strLen(ct.Str)
		}
		_, tail := splitUTF16(ct.Str, int(min(offset, uint64(ct.utf16Len))))
		return strLen(tail)
	case *ContentAny:
		vals := ct.Vals[offset:]
		n := uvarLen(uint64(len(vals)))
		if values {
			for _, v := range vals {
				n += anyLen(v)
			}
		}
		return n
	case *ContentJSON:
		vals := ct.Vals[offset:]
		n := uvarLen(uint64(len(vals)))
		if values {
			for _, v := range vals {
				n += jsonLen(v)
			}
		}
		return n
	case *ContentBinary:
		return uvarLen(uint64(len(ct.Data))) + len(ct.Data)
	case *ContentEmbed:
		return jsonLen(ct.Val)
	case *ContentFormat:
		return strLen(ct.Key) + jsonLen(ct.Val)
	case *ContentType:
		tc, name := typeClassOf(ct)
		if typeRefHasName(tc) {
			return 1 + strLen(name)
		}
		return 1
	case *ContentDoc:
		guid := ""
		if ct.Doc != nil {
			guid = ct.Doc.GUID()
		}
		return strLen(guid) + subdocOptsLen(ct.Doc)
	case *ContentMove:
		return uvarLen(uint64(ct.Target.Client)) + uvarLen(ct.Target.Clock) + uvarLen(ct.TargetLen)
	}
	// encodeContent writes nothing for any other content.
	return 0
}

// subdocOptsLen is the bytes writeSubdocOpts writes for d: a lib0 Any object
// of the options d does not hold at their defaults.
func subdocOptsLen(d *Doc) int {
	if d == nil {
		return 1 + uvarLen(0)
	}
	n, entries := 1, 0
	if !d.gc {
		n, entries = n+strLen("gc")+anyLen(false), entries+1
	}
	if d.autoLoad {
		n, entries = n+strLen("autoLoad")+anyLen(true), entries+1
	}
	if d.meta != nil {
		n, entries = n+strLen("meta")+anyLen(d.meta), entries+1
	}
	return n + uvarLen(uint64(entries))
}

// jsonLen is what V1 writes for v as JSON text, at the longer of the state's
// two forms. The direct form prints an integer v holds - an int64 or BigInt
// a V2 update or a local write brought - as its digits, and the merge an
// encode with parked state makes reads that text back as a float64
// (fmtValFromJSON) and prints the float64, a digit longer where it rounds up
// to the next power of ten: 9999999999999999 becomes 10000000000000000
// (roundUps). Every V1 decode rounds the same way, so only a document fed V2
// or written locally holds such an integer.
func jsonLen(v any) int {
	n := len(fmtValToJSON(v)) + roundUps(v)
	return uvarLen(uint64(n)) + n
}

// roundUps is how many integers v holds whose float64 prints a digit more
// than they do.
func roundUps(v any) int {
	switch t := v.(type) {
	case int64:
		return intRoundsUp(t)
	case int:
		return intRoundsUp(int64(t))
	case encoding.BigInt:
		return intRoundsUp(int64(t))
	case uint64:
		if t <= math.MaxInt64 {
			return intRoundsUp(int64(t))
		}
		var f, u [24]byte
		if len(strconv.AppendFloat(f[:0], float64(t), 'f', -1, 64)) > len(strconv.AppendUint(u[:0], t, 10)) {
			return 1
		}
	case []any:
		n := 0
		for _, e := range t {
			n += roundUps(e)
		}
		return n
	case map[string]any:
		n := 0
		for _, e := range t {
			n += roundUps(e)
		}
		return n
	}
	return 0
}

// intRoundsUp is 1 where i's float64 prints a digit more than i, which needs
// a magnitude past 2^53, from where a float64 no longer holds every integer.
func intRoundsUp(i int64) int {
	if i > -(1<<53) && i < 1<<53 {
		return 0
	}
	var f, d [24]byte
	if len(strconv.AppendFloat(f[:0], float64(i), 'f', -1, 64)) > len(strconv.AppendInt(d[:0], i, 10)) {
		return 1
	}
	return 0
}

// mergedLenGrowth is what a run's length prefix grows by when it takes in the
// a and b units of two structs, which wrote a prefix each: never above zero.
func mergedLenGrowth(a, b uint64) int {
	return uvarLen(a+b) - uvarLen(a) - uvarLen(b)
}

// stateCount is what a remote apply on a document that reports counts of the
// encoded state (ApplyReport.StateBytes), part of the apply's tally.
type stateCount struct {
	// stateOn is whether the apply counts, which only a document that
	// reports does, and state is the count as the apply has made it, but for
	// the delete set, which is read where the apply ends (deleteSetGrowth).
	// stateClients is the clients the store holds a struct of, read on first
	// need and -1 before (clients); arrivedDeleted is the structs the apply
	// appended deleted, which no delete records (noteStateAppend).
	stateOn        bool
	state          int
	stateClients   int
	arrivedDeleted DeleteSet
	// held is the most structs the pending queue has held during the apply,
	// which bounds the counts the queue adds to (sectionCountGrowth); fresh is
	// whether it held none as the apply began, and parked, the structs the
	// apply then parked of each client, and parkedSections, the clients among
	// them the state had no section of, make each park's count exact
	// (noteStatePark).
	held           int
	fresh          bool
	parked         map[ClientID]freshPark
	parkedSections int
	// drainQueue is the queue the running drain takes apart, which the
	// report reads in its place (applyTally.queue), drainSV the store's
	// clocks as the drain began (drained), and drainMoved the structs the
	// drain landed of each client, -1 once one of the client's left the
	// queue without landing. drainKept and drainKeptBy are what the drain's
	// splits of structs the store held before it added, and how many of them
	// each client's are, which a take-back leaves in place
	// (takeBackStateDrain).
	drainQueue  *pendingUpdate
	drainSV     StateVector
	drainMoved  map[ClientID]int
	drainKept   int
	drainKeptBy map[ClientID]int
	// parkedDeletes is the clients the apply parked delete ranges of, and
	// pendingKept each client's pending deletes as the apply found them,
	// kept before the apply's first write into them (keepPendingDs).
	parkedDeletes []ClientID
	pendingKept   map[ClientID][]DeleteRange
}

// beginStateCount starts the count of an apply on a document that reports,
// whose store is s.
func (t *applyTally) beginStateCount(s *StructStore) {
	t.stateOn, t.stateClients = true, -1
	t.held = s.pending.heldStructs()
	t.fresh = t.held == 0
}

// keepPendingDs keeps client c's pending deletes as the apply found them,
// before the apply's first write into them, for the delete set's growth
// (deleteSetGrowth).
func (t *applyTally) keepPendingDs(s *StructStore, c ClientID) {
	if _, kept := t.pendingKept[c]; kept {
		return
	}
	if t.pendingKept == nil {
		t.pendingKept = make(map[ClientID][]DeleteRange)
	}
	t.pendingKept[c] = slices.Clone(s.pendingDs.clients[c])
}

// pendingBefore is client c's pending deletes as the apply found them.
func (t *applyTally) pendingBefore(s *StructStore, c ClientID) []DeleteRange {
	if ranges, kept := t.pendingKept[c]; kept {
		return ranges
	}
	return s.pendingDs.clients[c]
}

// noteStateParkDeletes keeps, before ds is merged into the pending deletes,
// what they held of its clients, and the clients, whose growth the delete
// set's is read for (deleteSetGrowth).
func noteStateParkDeletes(txn *Transaction, ds DeleteSet) {
	t := txn.doc.tally()
	if t == nil || !t.stateOn {
		return
	}
	for c := range ds.clients {
		t.keepPendingDs(txn.doc.store, c)
		t.parkedDeletes = append(t.parkedDeletes, c)
	}
}

// noteStateRetry keeps, before a retry of the pending deletes takes the ready
// ones out, what they held of each client the retry can take.
func noteStateRetry(txn *Transaction) {
	t := txn.doc.tally()
	if t == nil || !t.stateOn {
		return
	}
	s := txn.doc.store
	for c := range s.pendingDsMoved {
		if len(s.pendingDs.clients[c]) > 0 {
			t.keepPendingDs(s, c)
		}
	}
}

// stateDue is the StateBytes of the report txn's commit is to make, for the
// commit's merges and collection to take what they remove off it, or nil for
// a transaction that makes no report (WithApplyReport). The document keeps
// the tally from the apply's end until its commit reports it, and runs one
// transaction at a time.
func (txn *Transaction) stateDue() *int {
	if t := txn.doc.reporting; t != nil && t.stateOn {
		return &t.state
	}
	return nil
}

// storeClients is how many clients the store holds a struct of, which the
// encoded state's first varuint counts.
func storeClients(s *StructStore) int {
	n := 0
	for _, items := range s.clients {
		if len(items) > 0 {
			n++
		}
	}
	return n
}

// crossesVarUint is whether a count from lo to hi, both included, passes one
// at which its varuint is a byte longer than at the count before it: 128,
// 16,384, and on by factors of 128.
func crossesVarUint(lo, hi uint64) bool {
	for t := uint64(1) << 7; t != 0 && t <= hi; t <<= 7 {
		if t >= lo {
			return true
		}
	}
	return false
}

// sectionCountGrowth is what the varuint counting a client's section grows by
// when its entries go from n+queued to n+queued+delta, n being the store's
// structs of the client and queued the entries its parked structs add
// (queuedEntries): exact where queued is known. Where it is not, the queue
// holds at most held structs, each with a skip before it, and the report takes
// the byte a varuint boundary in what the count can be costs on the way up,
// and nothing on the way down.
func sectionCountGrowth(n uint64, queued int, known bool, delta int, held int) int {
	if known {
		from := n + uint64(queued)
		return uvarLen(uint64(int64(from)+int64(delta))) - uvarLen(from)
	}
	if delta > 0 && crossesVarUint(n+1, n+2*uint64(held)+1+uint64(delta)) {
		return 1
	}
	return 0
}

// sectionsGrowth is what the varuint counting the state's sections grows by
// when the store gains its g+1st client while the queue may hold sections of
// as many as queued clients the store holds none of.
func sectionsGrowth(g uint64, queued int) int {
	if queued > 0 && crossesVarUint(g+1, g+uint64(queued)+1) {
		return 1
	}
	return uvarLen(g+1) - uvarLen(g)
}

// overlapSlack bounds what a struct of client c adds to the state where it
// shares clocks, up to end, with a parked struct that stays parked - one that
// waits on a missing struct of another client, as a GC struct for the same
// clocks does not. The state keeps one of the two at each clock
// (dedupClientStructs), and the one it cuts may then name an origin where it
// named its parent before: two such cuts, each at most the client and a clock.
func overlapSlack(c ClientID, end uint64) int {
	return 2 * (uvarLen(uint64(c)) + uvarLen(end))
}

// queuedClocks is what the pending queue holds of one client: the clocks its
// structs span, and how many structs it holds (parkedCover).
type queuedClocks struct {
	spans   seq[clockSpan]
	structs int
}

// parkedCover is what the pending queue holds of each client, read to count
// what a client's parked structs make of a change to its section. It is built
// from the queue on first need and kept by each park (noteStatePark) and each
// merge of a park into the queue (queueMerge.note), which keeps the clocks the
// queue held, so it is exact; a rollback of the apply drops it, to be built
// again (applyJournal.rollBack), and a drain replaces the queue with one that
// has no cover yet. Once built it is read even while a merge has left the
// queue empty, the arrivals that hold the clocks of what it dropped not yet
// parked.
func (p *pendingUpdate) parkedCover() map[ClientID]*queuedClocks {
	if p == nil || p.stateCover == nil && p.heldStructs() == 0 {
		return nil
	}
	if p.stateCover == nil {
		held := p.heldItems()
		lists := sortedSpans(held)
		p.stateCover = make(map[ClientID]*queuedClocks, len(lists))
		for client, list := range lists {
			p.stateCover[client] = &queuedClocks{spans: seqOf(list)}
		}
		for _, it := range held {
			if pc := p.stateCover[it.ID.Client]; pc != nil {
				pc.structs++
			}
		}
	}
	return p.stateCover
}

// coverParked adds item, just parked or grown by a merge, to the cover, if it
// is built: its clocks, and with parked a struct more.
func (p *pendingUpdate) coverParked(item *Item, parked bool) {
	if p == nil || p.stateCover == nil {
		return
	}
	pc := p.stateCover[item.ID.Client]
	if pc == nil {
		pc = &queuedClocks{}
		p.stateCover[item.ID.Client] = pc
	}
	if end := item.ID.Clock + item.Content.Len(); end > item.ID.Clock {
		addSpan(&pc.spans, clockSpan{item.ID.Clock, end})
	}
	if parked {
		pc.structs++
	}
}

// uncoverDropped takes item, a queued struct a merge dropped, off the cover's
// count; its clocks stay, held by the struct the merge kept in its place.
func (p *pendingUpdate) uncoverDropped(item *Item) {
	if p == nil || p.stateCover == nil {
		return
	}
	if pc := p.stateCover[item.ID.Client]; pc != nil {
		pc.structs--
	}
}

// queuedEntries is the entries client c's section holds for its parked
// structs pc, beside n store structs of it ending at storeEnd: each parked
// struct, and a skip before each run of them that does not follow straight on
// from what precedes it. known is false where a parked struct shares clocks
// with the store, which the state's merge then keeps one of
// (dedupClientStructs).
func queuedEntries(pc *queuedClocks, n int, storeEnd uint64) (entries int, known bool) {
	if pc == nil || pc.spans.len() == 0 {
		return 0, true
	}
	first := pc.spans.at(0).start
	if n > 0 && first < storeEnd {
		return 0, false
	}
	gaps := pc.spans.len() - 1
	if n > 0 && first > storeEnd {
		gaps++
	}
	return pc.structs + gaps, true
}

// skipLen is what the skip struct of a gap of g clocks is written in, and
// nothing for no gap.
func skipLen(g uint64) int {
	if g == 0 {
		return 0
	}
	return 1 + uvarLen(g)
}

// gapOf is the clocks between from and to, and 0 where to is not past from.
func gapOf(from, to uint64) uint64 {
	if to > from {
		return to - from
	}
	return 0
}

// parkGrowth is what adding the clocks [from, to) of a parked struct to
// spans, the clocks the queue holds of a client whose section exists, changes
// of the section's skip structs and start clock: the bytes, and the entries
// the skips add. stored is whether the store holds structs of the client, up
// to storeEnd, which from is not before; the section starts at clock 0 then,
// and at its first parked clock otherwise.
func parkGrowth(spans seq[clockSpan], stored bool, storeEnd, from, to uint64) (bytes, entries int) {
	m := spans.len()
	i := spans.search(func(x clockSpan) bool { return x.end >= from })
	j := spans.search(func(x clockSpan) bool { return x.start > to })
	lo, hi := from, to
	if i < j {
		lo, hi = min(lo, spans.at(i).start), max(hi, spans.at(j-1).end)
	}
	prev, hasPrev := storeEnd, stored
	if i > 0 {
		prev, hasPrev = spans.at(i-1).end, true
	}
	// The skips the window between prev and the span after the struct held.
	cur, has := prev, hasPrev
	for k := i; k <= j && k < m; k++ {
		if has {
			g := gapOf(cur, spans.at(k).start)
			bytes -= skipLen(g)
			if g > 0 {
				entries--
			}
		}
		cur, has = spans.at(k).end, true
	}
	// And the skips it holds with the struct's clocks joined in.
	if hasPrev {
		if g := gapOf(prev, lo); g > 0 {
			bytes += skipLen(g)
			entries++
		}
	} else if m > 0 {
		bytes += uvarLen(lo) - uvarLen(spans.at(0).start)
	}
	if j < m {
		if g := gapOf(hi, spans.at(j).start); g > 0 {
			bytes += skipLen(g)
			entries++
		}
	}
	return bytes, entries
}

// overlapEnd is how far parked clocks of spans reach when they share a clock
// with [from, to), and 0 when they share none.
func overlapEnd(spans seq[clockSpan], from, to uint64) uint64 {
	i := spans.search(func(x clockSpan) bool { return x.end > from })
	if i == spans.len() || spans.at(i).start >= to {
		return 0
	}
	return spans.at(spans.search(func(x clockSpan) bool { return x.start >= to }) - 1).end
}

// queueShare is what the queue holds of client c, for a change to the store's
// structs of it in [from, to), with the overlap slack where a parked struct
// shares those clocks. queue is the queue as the change finds it: the
// store's, or during a drain the one the drain took apart
// (applyTally.drainQueue).
func queueShare(queue *pendingUpdate, c ClientID, from, to uint64) (*queuedClocks, int) {
	pc := queue.parkedCover()[c]
	if pc == nil || pc.spans.len() == 0 {
		return nil, 0
	}
	slack := 0
	if end := overlapEnd(pc.spans, from, to); end > 0 {
		slack = overlapSlack(c, max(end, to))
	}
	return pc, slack
}

// fillGrowth is what appending the store's struct [from, to) of a client
// changes of its section, where the queue holds the client's parked structs
// pc: the skip between the store's end and the first parked struct, which the
// struct shortens or closes, and for a section the queue alone held its start
// clock, which becomes 0; and the entries that adds. ok is false where the
// struct shares clocks with a parked one, which the bound counts instead.
func fillGrowth(pc *queuedClocks, stored bool, from, to uint64) (bytes, entries int, ok bool) {
	if pc == nil || pc.spans.len() == 0 {
		return 0, 0, false
	}
	first := pc.spans.at(0).start
	if first < to {
		return 0, 0, false
	}
	after := gapOf(to, first)
	bytes, entries = skipLen(after), 1
	if after > 0 {
		entries++
	}
	if stored {
		if before := gapOf(from, first); before > 0 {
			bytes -= skipLen(before)
			entries--
		}
		return bytes, entries, true
	}
	return bytes + uvarLen(0) - uvarLen(first), entries, true
}

// queue is the pending queue as the apply's current step finds it: during a
// drain, the queue the drain began from, whose structs are either still in it
// or landing; at any other time the store's.
func (t *applyTally) queue(s *StructStore) *pendingUpdate {
	if t.drainQueue != nil {
		return t.drainQueue
	}
	return s.pending
}

// clients is the store's clients with a struct, read once per apply and kept
// as each new client arrives: only a drain taken back removes a client during
// an apply, and the drain is the apply's last step.
func (t *applyTally) clients(s *StructStore) uint64 {
	if t.stateClients < 0 {
		t.stateClients = storeClients(s)
	}
	return uint64(t.stateClients)
}

// noteStateAppend counts item, which the apply is about to append to the
// store, on a document that reports (WithApplyReport): at its encoded length,
// with what its client's section grows by - the varuint counting the section's
// entries, and for a client new to the store the section's count, client and
// start clock, with the growth of the varuint counting sections. For a client
// the queue holds structs of, the struct also shortens or closes the skip
// before the first of them, and a section the queue alone held starts at
// clock 0 from here (fillGrowth); where that is not known, a new section's
// count, client and start clock bound it.
//
// A struct the drain lands moves from the queue's share of the state to the
// store's: its section keeps its count, so only its length is counted here,
// and what it held parked is taken off where it leaves the queue
// (stateLeaving). A struct that arrives deleted - a GC struct, or an item
// whose right origin is one - is noted for the delete set's growth
// (deleteSetGrowth), which no delete of it records.
func noteStateAppend(txn *Transaction, item *Item) {
	t := txn.doc.tally()
	if t == nil || !t.stateOn {
		return
	}
	s := txn.doc.store
	c, n := item.ID.Client, len(s.clients[item.ID.Client])
	from, to := item.ID.Clock, item.ID.Clock+item.Content.Len()
	grown := structLen(item, 0, true)
	if t.drainQueue != nil {
		if n == 0 {
			t.clients(s)
			t.stateClients++
		}
		if moved := t.drainMoved[c]; moved >= 0 {
			if t.drainMoved == nil {
				t.drainMoved = make(map[ClientID]int)
			}
			t.drainMoved[c] = moved + 1
		}
	} else {
		queue := t.queue(s)
		pc, slack := queueShare(queue, c, from, to)
		queued, known := queuedEntries(pc, n, from)
		bytes, added, ok := fillGrowth(pc, n > 0, from, to)
		switch {
		case ok && known:
			if n == 0 {
				t.clients(s)
				t.stateClients++
			}
			grown += bytes + sectionCountGrowth(uint64(n), queued, true, added, t.held)
		case n > 0:
			grown += sectionCountGrowth(uint64(n), queued, known, 1, t.held) + slack
		default:
			g := t.clients(s)
			t.stateClients++
			grown += 1 + uvarLen(uint64(c)) + uvarLen(0) + sectionsGrowth(g, t.held) + slack
		}
	}
	t.state += grown
	if item.Deleted {
		if t.arrivedDeleted.clients == nil {
			t.arrivedDeleted = newDeleteSet()
		}
		t.arrivedDeleted.add(item.ID, item.Content.Len())
	}
}

// freshPark is the structs an apply has parked of one client while the queue
// held nothing else: where the last ends, and the section entries they and
// the skips before them added; entries is -1 once one arrived out of clock
// order, which leaves the rest to the bound (noteStatePark).
type freshPark struct {
	end     uint64
	entries int
}

// noteStatePark counts item, which the apply is about to park, at what the
// state's merge of the queue (withParked) adds for it: its encoded length, the
// skip structs standing for the clocks between it and what is beside it in its
// client's section, the growth of the varuint counting the section's entries,
// and for a client with no section the section's count, client and start
// clock, with the growth of the varuint counting sections.
//
// Where the queue held nothing as the apply began, every struct of the section
// is the store's or this apply's, and each is counted exactly. Otherwise what
// it lands beside is read from what the queue holds of its client
// (parkedCover). Where that is not known exact, the count is the bound: the
// skip at most 1 + the varuint of item's clock (the gap it splits is no longer
// than its clock), a client's section counted as new where the store holds
// none of it, which covers the skip and the start clock of one the queue
// holds, and the byte a varuint boundary can cost.
func noteStatePark(txn *Transaction, item *Item) {
	t := txn.doc.tally()
	if t == nil || !t.stateOn {
		return
	}
	s := txn.doc.store
	c, n, end := item.ID.Client, len(s.clients[item.ID.Client]), s.NextClock(item.ID.Client)
	clock, last := item.ID.Clock, item.ID.Clock+item.Content.Len()
	grown := structLen(item, 0, true)
	run, ran := t.parked[c]
	if t.fresh && (clock < end || ran && (run.entries < 0 || clock < run.end)) {
		run, ran = freshPark{entries: -1}, true
	}
	queued, known := 0, false
	if !t.fresh && clock >= end {
		queued, known = queuedEntries(s.pending.parkedCover()[c], n, end)
	}
	switch {
	case t.fresh && run.entries >= 0:
		base, entries := end, uint64(n)
		if ran {
			base, entries = run.end, uint64(n+run.entries)
		}
		added := 1
		if !ran && n == 0 {
			// A section of its own: its count, client and start clock.
			grown += uvarLen(1) + uvarLen(uint64(c)) + uvarLen(clock)
			g := t.clients(s) + uint64(t.parkedSections)
			grown += uvarLen(g+1) - uvarLen(g)
			t.parkedSections++
		} else {
			if clock > base {
				grown += 1 + uvarLen(clock-base)
				added++
			}
			grown += uvarLen(entries+uint64(added)) - uvarLen(entries)
		}
		run = freshPark{end: last, entries: run.entries + added}
	case known:
		pc := s.pending.stateCover[c]
		if pc == nil || pc.spans.len() == 0 {
			if n == 0 {
				grown += uvarLen(1) + uvarLen(uint64(c)) + uvarLen(clock) + sectionsGrowth(t.clients(s), t.held)
				break
			}
			pc = &queuedClocks{}
		}
		bytes, added := parkGrowth(pc.spans, n > 0, end, clock, last)
		grown += bytes + sectionCountGrowth(uint64(n), queued, true, added+1, t.held)
	default:
		if n == 0 {
			grown += 1 + uvarLen(uint64(c)) + uvarLen(clock) + sectionsGrowth(t.clients(s), t.held)
		} else {
			grown += 1 + uvarLen(clock)
		}
		grown += sectionCountGrowth(uint64(n), 0, false, 2, t.held)
		if clock < end {
			grown += overlapSlack(c, max(last, end))
		}
	}
	if t.fresh {
		if t.parked == nil {
			t.parked = make(map[ClientID]freshPark)
		}
		t.parked[c] = run
	}
	t.state += grown
	t.held++
	s.pending.coverParked(item, true)
}

// queueMerge is the queued structs a park's merge is about to cut, grow or
// drop (pendingUpdate.apply), as the state carries them, for the report to
// count what the merge changed of them once it is made.
type queueMerge struct {
	t      *applyTally
	s      *StructStore
	before []queuedShare
}

// queuedShare is what one queued struct held in the state before a change to
// it: its encoded length, or, for a struct that shared clocks with the store
// as the apply began, the store structs it shares them with, which the state's
// merge (dedupClientStructs) may have left out for it and the change can let
// back in; changed is whether the change touches it at all.
type queuedShare struct {
	size, hidden int
	overlaps     bool
	changed      bool
}

// shareOf is item's queuedShare as the apply finds it, start being the store's
// clocks as the apply began.
func shareOf(s *StructStore, start StateVector, item *Item) queuedShare {
	c, from, to := item.ID.Client, item.ID.Clock, item.ID.Clock+item.Content.Len()
	if from >= start.Clock(c) {
		return queuedShare{size: structLen(item, 0, true), changed: true}
	}
	return queuedShare{hidden: storeBytesIn(s, c, from, to), overlaps: true, changed: true}
}

// storeBytesIn is the encoded length of client c's store structs that share a
// clock with [from, to).
func storeBytesIn(s *StructStore, c ClientID, from, to uint64) int {
	items := s.clients[c]
	i := sort.Search(len(items), func(i int) bool { return items[i].ID.Clock+items[i].Content.Len() > from })
	n := 0
	for ; i < len(items) && items[i].ID.Clock < to; i++ {
		n += structLen(items[i], 0, true)
	}
	return n
}

// measureQueueMerge measures the queued structs pl changes, on a document that
// reports. The queue's cover is read before the merge, so it keeps the clocks
// of each struct the merge drops for the arrival that holds them, which parks
// next to what the section already held: read after, it would lack them, and
// the arrival would count again the skip, or the section's count, client and
// start clock, that the dropped struct stood in.
func measureQueueMerge(txn *Transaction, pl mergePlan) queueMerge {
	t := txn.doc.tally()
	if t == nil || !t.stateOn {
		return queueMerge{}
	}
	s, start := txn.doc.store, txn.beforeState
	s.pending.parkedCover()
	m := queueMerge{t: t, s: s, before: make([]queuedShare, len(pl.held))}
	for i, q := range pl.held {
		if pl.keepHeld[i] != (mergeKeep{start: q.ID.Clock, end: q.ID.Clock + q.Content.Len()}) {
			m.before[i] = shareOf(s, start, q.Item)
		}
	}
	return m
}

// note counts the change: each struct the merge cut or grew at its new length
// less its old, and each it dropped - one whose clocks a struct it kept holds,
// so no gap opens where it was - at its old length taken off, each with what
// it changes of its client's section (sectionChange). A struct that shared
// clocks with the store is counted at the most the change can add: its new
// length with what a cut of it can add, and the store structs it may have
// hidden, its old share being at least nothing.
func (m queueMerge) note(pl mergePlan, queue *pendingUpdate) {
	if m.t == nil {
		return
	}
	for i, q := range pl.held {
		was := m.before[i]
		if !was.changed {
			continue
		}
		kept := pl.keepHeld[i].kept()
		if !was.overlaps {
			m.t.state += m.sectionChange(queue, q.Item, kept)
		}
		if kept {
			queue.coverParked(q.Item, false)
		} else {
			queue.uncoverDropped(q.Item)
		}
		switch {
		case was.overlaps && kept:
			end := q.ID.Clock + q.Content.Len()
			m.t.state += structLen(q.Item, 0, true) + overlapSlack(q.ID.Client, end) + was.hidden
		case was.overlaps:
			m.t.state += was.hidden
		case kept:
			m.t.state += structLen(q.Item, 0, true) - was.size
		default:
			m.t.state -= was.size
		}
	}
}

// sectionChange is what the merge's change to item, a queued struct it kept
// as it now is or dropped, makes of its client's section, read before the
// cover takes the change: the skips a GC struct grown over the clocks past it
// shortens or closes, or the entry a dropped struct held, its clocks held on
// by the struct kept in its place. Where what the queue holds of the client
// is not known it is nothing, which leaves the shrink uncounted and the count
// above the state.
func (m queueMerge) sectionChange(queue *pendingUpdate, item *Item, kept bool) int {
	c := item.ID.Client
	pc := queue.parkedCover()[c]
	n, end := len(m.s.clients[c]), m.s.NextClock(c)
	queued, known := queuedEntries(pc, n, end)
	if !known || pc == nil {
		return 0
	}
	bytes, entries := 0, -1
	if kept {
		bytes, entries = parkGrowth(pc.spans, n > 0, end, item.ID.Clock, item.ID.Clock+item.Content.Len())
	}
	return bytes + sectionCountGrowth(uint64(n), queued, true, entries, m.t.held)
}

// stateLeaving is a parked struct the drain is about to try, as the state
// carries it, for the report to take off what it held there if it leaves the
// queue (tryIntegrateOrDrop).
type stateLeaving struct {
	t          *applyTally
	s          *StructStore
	item       *Item
	share      queuedShare
	end, units uint64
	moved      int
}

// measureLeaving measures item before the drain's try changes it, in a drain
// of a document that reports.
func measureLeaving(txn *Transaction, item *Item) stateLeaving {
	t := txn.doc.tally()
	if t == nil || !t.stateOn || t.drainQueue == nil {
		return stateLeaving{}
	}
	s, units := txn.doc.store, item.Content.Len()
	return stateLeaving{t: t, s: s, item: item, share: shareOf(s, txn.beforeState, item),
		end: item.ID.Clock + units, units: units, moved: t.drainMoved[item.ID.Client]}
}

// note takes off what the struct held in the state, once the try took it out
// of the queue (left): all of it where the store now holds its clocks, landed
// or already there, and otherwise - a struct the drain dropped - all but what
// the skip standing for its clocks, joined with the gaps on either side, can
// take back of it. A struct that shared clocks with the store takes nothing
// off and adds the store structs it may have hidden there.
func (m stateLeaving) note(left bool) {
	if m.t == nil || !left {
		return
	}
	if c := m.item.ID.Client; m.t.drainMoved[c] == m.moved {
		// It left without landing, which takes an entry out of its
		// client's section: what the drain's splits find there is not known
		// from here on (measureSplit).
		if m.t.drainMoved == nil {
			m.t.drainMoved = make(map[ClientID]int)
		}
		m.t.drainMoved[c] = -1
	}
	switch {
	case m.share.overlaps:
		m.t.state += m.share.hidden
	case m.s.NextClock(m.item.ID.Client) >= m.end:
		m.t.state -= m.share.size
	default:
		m.t.state -= max(0, m.share.size-1-uvarLen(m.units))
	}
}

// splitMeasure is an item about to be split as the encoded state carries it,
// for the report to count what the split adds once it is made. kept is whether
// the split is one a drain taken back leaves in place: of a struct the store
// held before the drain (applyTally.drained).
type splitMeasure struct {
	t             *applyTally
	before, n     int
	queued, slack int
	known, kept   bool
}

// measureSplit measures item before splitItem cuts it, on a document that
// reports, and is a no-op measure on any other. During a drain the queue's
// share of the client's section is what it was as the drain began, less the
// structs the drain has landed of the client, which became the store's: a
// landing keeps the section's count, so that is known while every struct of
// the client that left the queue landed. A struct the drain landed shares
// clocks with the queue the drain began from because it came out of it, which
// is no overlap.
func measureSplit(txn *Transaction, item *Item) splitMeasure {
	t := txn.doc.tally()
	if t == nil || !t.stateOn {
		return splitMeasure{}
	}
	s, c := txn.doc.store, item.ID.Client
	queue := t.queue(s)
	pc, slack := queueShare(queue, c, item.ID.Clock, item.ID.Clock+item.Content.Len())
	m := splitMeasure{t: t, before: structLen(item, 0, false), n: len(s.clients[c]), slack: slack,
		kept: t.drainQueue != nil && !t.drained(item)}
	switch {
	case pc == nil:
		m.known = true
	case t.drainQueue == nil:
		m.queued, m.known = queuedEntries(pc, m.n, s.NextClock(c))
	default:
		if t.drained(item) {
			m.slack = 0
		}
		if moved := t.drainMoved[c]; moved >= 0 {
			began := t.drainSV.Clock(c)
			stored := 0
			if began > 0 {
				stored = 1
			}
			if queued, known := queuedEntries(pc, stored, began); known {
				m.queued, m.known = queued-moved, true
			}
		}
	}
	return m
}

// note counts what the split of the measured item into left and right added:
// the right half's header - an info byte and its origin, the last clock of the
// left half, with the right origin both halves keep - and the length prefix
// each half's content now carries, with the growth of the varuint counting the
// client's structs. The content itself moves and is not counted again.
func (m splitMeasure) note(left, right *Item) {
	if m.t == nil {
		return
	}
	grown := structLen(left, 0, false) + structLen(right, 0, false) - m.before + m.slack
	m.t.state += grown + sectionCountGrowth(uint64(m.n), m.queued, m.known, 1, m.t.held)
	if m.kept {
		m.t.drainKept += grown
		if m.t.drainKeptBy == nil {
			m.t.drainKeptBy = make(map[ClientID]int)
		}
		m.t.drainKeptBy[left.ID.Client]++
	}
}

// drained is whether item is a struct the running drain added to the store:
// a parked struct it integrated, or a part split from one.
func (t *applyTally) drained(item *Item) bool {
	return t.drainQueue != nil && item.ID.Clock >= t.drainSV.Clock(item.ID.Client)
}

// stateAtDrain is the count of the encoded state as a drain of the pending
// queue began, which a drain taken back returns to (takeBackStateDrain).
type stateAtDrain struct{ state, clients int }

// beginStateDrain is the count as the drain of s's pending queue begins. On
// a document that reports, until the drain ends the report reads what the
// queue held from the queue the drain takes apart (applyTally.queue), and the
// store's clocks as the drain began tell the structs it adds (drained).
func (t *applyTally) beginStateDrain(s *StructStore) stateAtDrain {
	t.drainKept, t.drainKeptBy, t.drainMoved = 0, nil, nil
	if t.stateOn && s.pending != nil {
		t.drainQueue, t.drainSV = s.pending, s.StateVector()
	}
	return stateAtDrain{t.state, t.stateClients}
}

// endDrain is the end of the drain, taken back or not.
func (t *applyTally) endDrain() {
	if t != nil {
		t.drainQueue, t.drainSV = nil, nil
	}
}

// takeBackStateDrain returns the count to at once the store is where the
// drain began: every struct the drain added is gone and every struct it took
// out of the queue is parked again. What stays is the split of each struct
// the store held before it, which the commit merges back (tryMergeWithLefts)
// and so takes off the count again. The varuint counting each such client's
// structs is read again, the drain's own structs having moved it too.
func (t *applyTally) takeBackStateDrain(at stateAtDrain, s *StructStore) {
	if !t.stateOn {
		return
	}
	queue := t.drainQueue
	t.state, t.stateClients = at.state+t.drainKept, at.clients
	for c, kept := range t.drainKeptBy {
		n := uint64(len(s.clients[c]))
		_, parked := queue.parkedCover()[c]
		t.state += sectionCountGrowth(n-uint64(kept), 0, !parked, kept, t.held)
	}
	t.drainKept, t.drainKeptBy = 0, nil
}

// commitShare is what the queue the apply left holds of client c, for the
// commit's merges and collection of its structs in [from, to), which run once
// the apply has ended: the entries it adds to c's section and whether they are
// known (queuedEntries), whether a parked struct shares those clocks, and how
// many structs the queue holds.
func commitShare(s *StructStore, c ClientID, from, to uint64) (queued int, known, overlaps bool, held int) {
	pc := s.pending.parkedCover()[c]
	if pc == nil || pc.spans.len() == 0 {
		return 0, true, false, 0
	}
	queued, known = queuedEntries(pc, len(s.clients[c]), s.NextClock(c))
	return queued, known, overlapEnd(pc.spans, from, to) > 0, s.pending.heldStructs()
}

// collectedGrowth is what the commit's collection of item changes the encoded
// state by (gcTxnDeleteSet): its content written as its length, or, with gc,
// the whole struct as a GC struct, which is an info byte and its length. It is
// never above zero, and nothing where item shares clocks with a struct the
// queue holds, which the state's merge may keep in its place.
func collectedGrowth(s *StructStore, item *Item, gc bool) int {
	before, units := structLen(item, 0, true), item.Content.Len()
	after := 1 + uvarLen(units)
	if !gc && !item.isGCOrphan() {
		after = headerLen(item, 0) + uvarLen(units)
	}
	if _, _, overlaps, _ := commitShare(s, item.ID.Client, item.ID.Clock, item.ID.Clock+units); overlaps {
		return 0
	}
	return after - before
}

// deleteSetGrowth is what txn's apply added to the delete set of the encoded
// state, read where the apply ends with its deletions all made
// (endApplyTally). The delete set the state writes is the store's deleted
// structs merged with the pending deletes (withParked), each client's as
// maximal ranges of clocks, so it grows by the clocks the apply deleted or
// parked that neither held before, and by nothing for a parked range a retry
// applied, which only moves from the one to the other. Each client's growth
// is exact but for the varuints of the two counts (see the top of this file).
func deleteSetGrowth(txn *Transaction, t *applyTally) int {
	s := txn.doc.store
	after := s.pendingDs.clients
	arrived, parkedTo := t.arrivedDeleted.clients, t.parkedDeletes
	if len(txn.deleteSet.clients)+len(arrived)+len(parkedTo) == 0 {
		return 0
	}
	seen := make(map[ClientID]struct{}, len(txn.deleteSet.clients)+len(arrived)+len(parkedTo))
	total, entered := 0, 0
	grow := func(c ClientID) {
		if _, done := seen[c]; done {
			return
		}
		seen[c] = struct{}{}
		deleted := joinRanges(txn.deleteSet.clients[c], arrived[c], s.NextClock(c))
		held, parked := t.pendingBefore(s, c), after[c]
		added := deleted
		if len(held) > 0 || len(parked) > 0 {
			added = joinRanges(subtractRanges(deleted, held), subtractRanges(parked, held), math.MaxUint64)
		}
		if len(added) == 0 {
			return
		}
		grown, isNew := rangeGrowth(s.clients[c], parked, len(held) > 0, c, added)
		total += grown
		if isNew {
			entered++
		}
	}
	for c := range txn.deleteSet.clients {
		grow(c)
	}
	for c := range arrived {
		grow(c)
	}
	// A client whose pending deletes the apply changed but did not add to
	// only had a retry apply some of them, which adds nothing.
	for _, c := range parkedTo {
		grow(c)
	}
	if entered > 0 {
		// The delete set's clients are the store's with a deleted struct and
		// those with pending deletes, at most this many.
		most := uint64(len(s.clients) + len(after))
		total += min(uvarLen(most)-1, uvarLen(uint64(entered)))
	}
	return total
}

// rangeGrowth is what client c's entry in the encoded delete set grows by when
// added, the clocks the apply deleted or parked that the entry did not hold,
// joins it: each range of the client's deleted clocks - its deleted structs,
// items, and its pending deletes, parked - that added reaches, written once in
// place of the ranges it joined, and the entry's client and range count when
// the entry is new. held is whether the client had pending deletes before the
// apply; isNew is whether the entry held nothing before.
//
// Each range is found by walking out from added's clocks over the deleted
// structs and pending deletes beside them, so the walk costs the structs the
// apply deleted and the deleted ones they touch, not the client's.
func rangeGrowth(items []*Item, parked []DeleteRange, held bool, c ClientID, added []DeleteRange) (grown int, isNew bool) {
	end := uint64(0)
	if n := len(items); n > 0 {
		end = items[n-1].ID.Clock + items[n-1].Content.Len()
	}
	// deletedAt is the clocks of the deleted struct or pending delete that
	// holds clock x, joined where both do.
	deletedAt := func(x uint64) (from, to uint64, ok bool) {
		if x < end {
			i := sort.Search(len(items), func(i int) bool { return items[i].ID.Clock+items[i].Content.Len() > x })
			if it := items[i]; it.Deleted {
				from, to, ok = it.ID.Clock, it.ID.Clock+it.Content.Len(), true
			}
		}
		i := sort.Search(len(parked), func(i int) bool { return parked[i].Clock+parked[i].Len > x })
		if i < len(parked) && parked[i].Clock <= x {
			r := parked[i]
			if !ok {
				from, to = r.Clock, r.Clock+r.Len
			}
			from, to, ok = min(from, r.Clock), max(to, r.Clock+r.Len), true
		}
		return from, to, ok
	}
	runs, joined := 0, 0
	for k := 0; k < len(added); {
		first := k
		start, stop := added[k].Clock, added[k].Clock+added[k].Len
		for start > 0 {
			from, _, ok := deletedAt(start - 1)
			if !ok {
				break
			}
			start = from
		}
		for {
			for k < len(added) && added[k].Clock <= stop {
				stop = max(stop, added[k].Clock+added[k].Len)
				k++
			}
			_, to, ok := deletedAt(stop)
			if !ok {
				break
			}
			stop = to
		}
		// The range [start, stop) is written in place of what the entry held
		// of it: each part outside added, a range of its own.
		at := start
		for _, r := range added[first:k] {
			if r.Clock > at {
				grown -= uvarLen(at) + uvarLen(r.Clock-at)
				joined++
			}
			at = r.Clock + r.Len
		}
		if stop > at {
			grown -= uvarLen(at) + uvarLen(stop-at)
			joined++
		}
		grown += uvarLen(start) + uvarLen(stop-start)
		runs++
	}
	if joined == 0 && !held && !deletedOutside(items, added) {
		return grown + uvarLen(uint64(c)) + uvarLen(uint64(runs)), true
	}
	if more := runs - joined; more > 0 {
		// Each range holds a run of deleted structs, of which a client of n
		// structs has at most ceil(n/2), or a pending delete.
		most := uint64(len(items)+1)/2 + uint64(len(parked))
		grown += min(uvarLen(most)-1, uvarLen(uint64(more)))
	}
	return grown, false
}

// deletedOutside is whether items holds a deleted struct outside deleted: one
// deleted before the apply. It looks outward from deleted's first clock, where
// a client's earlier deletions most likely are.
func deletedOutside(items []*Item, deleted []DeleteRange) bool {
	if len(items) == 0 {
		return false
	}
	isNew := func(it *Item) bool {
		i := sort.Search(len(deleted), func(i int) bool { return deleted[i].Clock+deleted[i].Len > it.ID.Clock })
		return i < len(deleted) && deleted[i].Clock <= it.ID.Clock
	}
	mid := 0
	if len(deleted) > 0 {
		mid = sort.Search(len(items), func(i int) bool { return items[i].ID.Clock >= deleted[0].Clock })
	}
	for lo, hi := mid-1, mid; lo >= 0 || hi < len(items); lo, hi = lo-1, hi+1 {
		if lo >= 0 && items[lo].Deleted && !isNew(items[lo]) {
			return true
		}
		if hi < len(items) && items[hi].Deleted && !isNew(items[hi]) {
			return true
		}
	}
	return false
}

// joinRanges is the union of a and b below clock below, sorted and with the
// ranges that touch merged, as a delete set's encoding writes a client's.
func joinRanges(a, b []DeleteRange, below uint64) []DeleteRange {
	out := make([]DeleteRange, 0, len(a)+len(b))
	for _, ranges := range [2][]DeleteRange{a, b} {
		for _, r := range ranges {
			if r.Len == 0 || r.Clock >= below {
				continue
			}
			if end := r.Clock + r.Len; end > below || end < r.Clock {
				r.Len = below - r.Clock
			}
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(x, y DeleteRange) int { return cmp.Compare(x.Clock, y.Clock) })
	merged := out[:0]
	for _, r := range out {
		if last := len(merged) - 1; last >= 0 && merged[last].Clock+merged[last].Len >= r.Clock {
			merged[last].Len = max(merged[last].Clock+merged[last].Len, r.Clock+r.Len) - merged[last].Clock
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// subtractRanges is a less b, both sorted with no two ranges touching.
func subtractRanges(a, b []DeleteRange) []DeleteRange {
	if len(b) == 0 || len(a) == 0 {
		return a
	}
	var out []DeleteRange
	j := 0
	for _, r := range a {
		start, end := r.Clock, r.Clock+r.Len
		for j < len(b) && b[j].Clock+b[j].Len <= start {
			j++
		}
		for k := j; k < len(b) && b[k].Clock < end && start < end; k++ {
			if b[k].Clock > start {
				out = append(out, DeleteRange{Clock: start, Len: b[k].Clock - start})
			}
			start = max(start, b[k].Clock+b[k].Len)
		}
		if start < end {
			out = append(out, DeleteRange{Clock: start, Len: end - start})
		}
	}
	return out
}
