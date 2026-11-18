package crdt

// Clone returns a new Doc holding an independent copy of this document's
// state, built by copying the structures themselves: no update is encoded, no
// bytes are parsed, and no struct is integrated. It is the copy
//
//	nd := New(opts...)
//	ApplyUpdateV1(nd, EncodeStateAsUpdateV1(d, nil), nil)
//
// makes, for a caller that wants a document to write to and not a wire
// message. opts are New's options and nothing is taken from the source
// document: a clone of a document built with WithGC(false),
// WithFormatCleanup(false), WithMaxPendingItems or a scan bound keeps those
// settings only when the same options are passed here, exactly as the
// encode-and-apply copy above keeps them only when they are passed to New.
// If that client ID already has structs in the copied state, the clone takes
// a fresh client ID, as the encode-and-apply copy does.
//
// Clone takes the document's read lock for as long as it copies, so other
// readers may read the source meanwhile; the source is not written to. Like
// every other method that takes that lock, it must not be called from inside
// a Transact callback, which holds the write lock. Clone the document before
// the transaction, or from another goroutine.
//
// # What the clone holds
//
// Copied, as new objects the source does not share:
//
//   - Every struct of every client - items, the GC structs a wire GC
//     placeholder leaves parented to nothing, and split halves - in
//     per-client clock order, with its ID, delete flag, deferred parent ID and
//     undo redone link, and with every link between structs repointed at the
//     copy: left, right, origin, right origin, parent, parent key, the move
//     that claimed it, and each type's start, key map ("itemMap"), container
//     item and length. The copy is then brought into the applied form, the
//     form an apply of the source's encoding leaves (see below): a run of
//     same-client text structs is squashed into one, and a struct whose
//     parent an update cannot name is collected.
//   - Every struct's content, as Content.Copy makes it: a new content object
//     holding the same values, with a nested type (ContentType) copied as this
//     document copies a type rather than as the empty type Content.Copy makes
//     for a re-insert.
//   - The delete state, which structs carry themselves (Item.Deleted) and
//     which DeleteSetFromDoc and an encode derive from them.
//   - The pending queue: the parked structs with the content bytes they hold,
//     the per-client clocks they wait on, the parked delete ranges with their
//     count and the clients a struct has reached since the last retry, and
//     the deferred moves whose target has not arrived. The queue's clock
//     cover is a cache and is rebuilt on demand rather than copied.
//   - The named root types (Doc.share), each as the class the source holds, so
//     GetText, GetMap, GetArray and GetXmlFragment on the clone return the
//     clone's own type, with the same name, length, formatting flags and
//     (for an XML element) node name. A root the source never opened by name
//     stays the internal placeholder it is there, and the clone's first
//     GetText/GetMap/... upgrades it exactly as the source's would.
//
// Shared with the source, because ygo never writes through them:
//
//   - Strings. A Go string is immutable, and every content that holds one
//     (ContentString.Str, ContentFormat.Key, a type's name) is replaced
//     wholesale, never written into (transaction.go's mergeRuns.finish
//     assigns a newly built string; splitUTF16 returns new ones).
//   - The ID a struct's origin, right origin, deferred parent and redone link
//     point at. Nothing assigns through such a pointer: integrate, split and
//     merge all install a new *ID. (A move's target ID is not shared, since
//     ContentMove.Copy duplicates it.)
//   - A map key pointer (Item.ParentSub). strPtr makes a fresh pointer per
//     key and nothing assigns through one; parentSubEqual and parentSubKey
//     read the value.
//   - The values inside ContentAny, ContentJSON, ContentEmbed and
//     ContentFormat. The slice holding them is copied (Content.Copy), the
//     values in it are not: ygo stores what the caller passed and never
//     writes into it, as Item split and merge already share these values
//     between two items of one document. The one in-place widening of decoded
//     values, jsonNumbers, runs on values that are not yet stored.
//
// Not copied:
//
//   - Observers and subscriptions: OnUpdate, OnAfterTransaction, OnSubdocs,
//     per-type Observe and ObserveDeep. The clone fires nothing until
//     something observes it.
//   - Update logs (OpenUpdateLog), undo managers, and transaction state. The
//     clone has no open transaction and no undo history.
//   - Caches, which the clone rebuilds as it is used: search markers, the
//     first-live-item memo, the YText attribute cache, and the insert hint.
//     None of them is document state; a document with a cold cache answers
//     every query as one with a warm cache does.
//   - Awareness, which is not document state at all (package awareness keeps
//     it outside the Doc).
//   - The item that embeds the source as a subdocument of another document:
//     a clone is a root document, as the encode-and-apply copy is, and can be
//     embedded into one document of the caller's choosing.
//   - The content of a subdocument, which no update of the containing
//     document carries either. A ContentDoc is copied as the V1 decoder
//     builds one (newSubdocFromOpts): a new empty Doc with the source
//     subdocument's guid, gc flag, auto-load flag and meta, and shouldLoad
//     derived from auto-load. A live subdocument is attached and registered
//     in the clone, inheriting the clone's collection ID as an apply does.
//     Clone a subdocument's own state by cloning the subdocument.
//
// # The applied form
//
// EncodeStateAsUpdateV1 of a clone is byte-identical to EncodeStateAsUpdateV1
// of the encode-and-apply copy of the same document, which is what a caller
// replacing that copy with a clone needs. Two things an apply does to the
// structs it reads, a copy of the structures has to do as well, and Clone
// does them with the same code the apply runs, over the copied structs:
//
//   - It squashes a run of same-client text structs that one transaction
//     created into one struct (squashRuns). An apply of a whole state creates
//     every struct in one transaction, so it squashes every run the document
//     holds, while the document those bytes came from squashed only each
//     transaction's own: a text typed one character per transaction holds one
//     struct per character and its copy holds one per run.
//   - It collects a struct whose parent an update cannot name
//     (dropUnnameableParents): an update names a nested type by the id of the
//     container struct holding it, and once that container is deleted and
//     collected, the type is gone from the document the bytes build.
//
// Both change tombstones and struct boundaries, never what a document reads.
// A clone of a document that has neither - most documents, and every document
// loaded from an update - holds its source's structs one for one, and encodes
// as the source as well.
//
// A clone keeps one thing the encode-and-apply copy loses: the content of a
// deleted struct that the source still holds. A document with an UndoManager
// attached runs no auto-GC, so it keeps that content to undo with; the apply
// that builds the copy has no UndoManager and collects the deletion at
// commit, while a clone, which needs no UndoManager of its own to copy the
// content, keeps it. The clone and the copy read the same and hold the same
// delete set; their encodings differ in the collected content alone. With no
// UndoManager attached - every other document - the source has collected it
// already and clone and copy are byte-identical.
//
// The clone keeps d's SetParkedOutOfState setting, so it encodes as d does;
// it holds d's parked structs and deletions either way.
func (d *Doc) Clone(opts ...DocOption) *Doc {
	nd := New(opts...)
	d.mu.RLock()
	defer d.mu.RUnlock()
	c := &docCloner{
		src:   d,
		dst:   nd,
		types: make(map[*abstractType]*abstractType, len(d.share)),
	}
	c.run()
	nd.store.parkedOutOfState = d.store.parkedOutOfState
	if nd.store.NextClock(nd.clientID) != 0 {
		nd.setClientID(NewClientID())
	}
	return nd
}

// docCloner carries the state of one Clone call: the two documents, the copy
// of each shared type the source holds, and the index that maps a struct of
// the source store to its copy.
type docCloner struct {
	src *Doc
	dst *Doc

	// types maps each source type to its copy. fill holds the source types
	// whose copy has been made but whose state (start, container item, key
	// map, length, flags) has not been filled in yet, because filling it
	// needs every struct copy to exist.
	types map[*abstractType]*abstractType
	fill  []*abstractType

	index itemIndex

	// parked and parkedSrc are the copies of the pending queue's structs and
	// the structs they were copied from, in the queue's order.
	parked    []*Item
	parkedSrc []*Item
}

func (c *docCloner) run() {
	c.copyStructs()
	c.copyPending()
	for name, st := range c.src.share {
		c.dst.share[name] = c.typeFor(st.baseType()).owner
	}
	c.fillTypes()
	c.linkStructs()
	// A type reached only from a struct's parent link is discovered by
	// linkStructs, after the first fill pass.
	c.fillTypes()
	c.copyPendingDeletes()
	c.copyPendingMoves()
	c.dropUnnameableParents()
	c.squashCopiedRuns()
}

// dropUnnameableParents turns every copied struct whose parent an update
// cannot name into the GC struct an apply makes of it, so a clone holds the
// encode-and-apply copy's structs.
//
// An update names a nested type by the id of the container struct that holds
// it (encodeItem). Once that container is deleted and the garbage collector
// has replaced its content with a tombstone, the type is gone from the
// document the bytes build: the decoder finds the container, finds no type in
// it, and the reader integrates the struct as the GC struct Yjs integrates it
// as (markGCOrphan) - deleted, tombstone content, no parent, no key and no
// origins. A document that was never encoded still holds the link, and so
// would a copy of its structures.
//
// Only a tombstone is ever reached this way: a container is collected only
// once it is deleted, and deleting a container deletes what is under it. The
// pass repeats until nothing changes, since a struct it collects carries a
// tombstone where it carried a nested type, which leaves that type unnameable
// in turn.
func (c *docCloner) dropUnnameableParents() {
	for again := true; again; {
		again = false
		for _, items := range c.dst.store.clients {
			for _, it := range items {
				if it.Parent == nil || !unnameableType(it.Parent) {
					continue
				}
				it.markGCOrphan()
				// The reader never links such a struct into a list, since it
				// has no type to link it into.
				it.Left, it.Right, it.MovedBy = nil, nil, nil
				again = true
			}
		}
	}
}

// unnameableType reports whether an update can no longer name t: it is a
// nested type whose container struct holds a tombstone in place of it.
func unnameableType(t *abstractType) bool {
	if t.item == nil {
		return false // a named root, which an update names by name
	}
	_, held := t.item.Content.(*ContentType)
	return !held
}

// squashCopiedRuns merges the copied structs the way the apply that builds the
// encode-and-apply copy merges them, so a clone holds that copy's structs and
// encodes as it does.
//
// An apply ends its transaction with squashRuns (transaction.go), which joins
// a run of same-client text structs one transaction created into one struct.
// The apply of a whole state creates every struct in one transaction against
// an empty before-state, so it squashes every run the document holds, while
// the document those bytes came from squashed only each transaction's own
// run: a text typed one character per transaction holds one struct per
// character and its copy holds one struct per run. The clone runs that same
// pass, over the same structs and with the same empty before-state, rather
// than a rule of its own.
//
// The transaction's other merge pass, tryMergeWithLefts, has nothing to do
// here: it re-merges the halves an apply's own splitting made, which a copy
// that splits nothing never makes.
func (c *docCloner) squashCopiedRuns() {
	var runs []*Item
	for _, items := range c.dst.store.clients {
		for _, it := range items {
			if inRun(it) {
				runs = append(runs, it)
			}
		}
	}
	if len(runs) == 0 {
		return
	}
	// squashRuns reads the document's store, the structs it is given and the
	// before-state; a fresh document's apply has none of the rest.
	squashRuns(&Transaction{doc: c.dst, newItems: runs})
}

// inRun reports whether a struct lies in a run the squash could merge: the
// struct next to it on its type's list is text of the same client whose
// clocks continue it. Every merge the squash makes is of such a pair, and
// the others are left out of the pass rather than started a run at, which
// costs a string builder each.
func inRun(it *Item) bool {
	if _, text := it.Content.(*ContentString); !text {
		return false
	}
	return continuesRun(it, it.Right) || continuesRun(it.Left, it)
}

// continuesRun reports whether right is text of left's client that starts
// where left ends.
func continuesRun(left, right *Item) bool {
	if left == nil || right == nil || left.ID.Client != right.ID.Client {
		return false
	}
	lc, text := left.Content.(*ContentString)
	if !text {
		return false
	}
	if _, text := right.Content.(*ContentString); !text {
		return false
	}
	return left.ID.Clock+lc.Len() == right.ID.Clock
}

// copyStructs copies every client's structs, in clock order, and indexes the
// copies so the later passes can repoint the links at them.
func (c *docCloner) copyStructs() {
	src, dst := c.src.store, c.dst.store
	dst.clients = make(map[ClientID][]*Item, len(src.clients))
	c.index.byClient = make(map[ClientID]*clientStructs, len(src.clients))
	for client, items := range src.clients {
		// One block per client: a document's structs live as long as the
		// document, so the copy allocates once per client rather than once
		// per struct.
		block := make([]Item, len(items))
		copies := make([]*Item, len(items))
		for i, it := range items {
			cp := &block[i]
			c.copyStruct(it, cp, true)
			copies[i] = cp
		}
		dst.clients[client] = copies
		c.index.byClient[client] = &clientStructs{src: items, dst: copies}
	}
}

// copyPending copies the parked structs and the clocks they wait on. The
// queue's clock cover is left nil: pending_cover.go builds it when it is first
// asked for.
func (c *docCloner) copyPending() {
	sp := c.src.store.pending
	if sp == nil {
		return
	}
	block := make([]Item, len(sp.items))
	items := make([]*Item, len(sp.items))
	for i, it := range sp.items {
		if it == nil {
			continue
		}
		cp := &block[i]
		c.copyStruct(it, cp, false)
		items[i] = cp
	}
	missing := make(StateVector, len(sp.missing))
	for client, clock := range sp.missing {
		missing[client] = clock
	}
	c.dst.store.pending = &pendingUpdate{items: items, dead: sp.dead, bytes: sp.bytes, missing: missing, ready: sp.ready}
	c.parked, c.parkedSrc = items, sp.items
}

// copyStruct copies it's own fields into cp. The links to other structs and to
// the parent type are repointed later, by linkStructs. integrated is false for
// a parked struct, which holds no subdocument of the document yet.
func (c *docCloner) copyStruct(it, cp *Item, integrated bool) {
	cp.ID = it.ID
	cp.Origin = it.Origin
	cp.OriginRight = it.OriginRight
	cp.ParentSub = it.ParentSub
	cp.parentID = it.parentID
	cp.redone = it.redone
	cp.Deleted = it.Deleted
	cp.Content = c.copyContent(it.Content)
	if !integrated {
		return
	}
	if cd, ok := cp.Content.(*ContentDoc); ok && cd.Doc != nil && !cp.Deleted {
		cd.Doc.attachSubdoc(cp, c.dst)
		cd.Doc.inheritCollectionID(c.dst.CollectionID())
		c.dst.subdocs[cd.Doc.guid] = cd.Doc
	}
}

// copyContent preserves a nested type's copied children instead of the empty
// type Content.Copy creates for a re-insert; other content uses its own copy.
func (c *docCloner) copyContent(content Content) Content {
	switch ct := content.(type) {
	case *ContentType:
		if ct.Type == nil {
			return &ContentType{}
		}
		return &ContentType{Type: c.typeFor(ct.Type)}
	default:
		return content.Copy()
	}
}

// typeFor returns the copy of source type s, making an empty one of the same
// class the first time it is asked for. Its state is filled in by fillTypes,
// once every struct copy exists.
func (c *docCloner) typeFor(s *abstractType) *abstractType {
	if t, ok := c.types[s]; ok {
		return t
	}
	t := s.emptyCopy()
	c.types[s] = t
	c.fill = append(c.fill, s)
	return t
}

// fillTypes fills in the state of every type copy made since the last call.
func (c *docCloner) fillTypes() {
	for len(c.fill) > 0 {
		s := c.fill[len(c.fill)-1]
		c.fill = c.fill[:len(c.fill)-1]
		c.fillType(s, c.types[s])
	}
}

// fillType copies s's own state into t. The caches - search markers, the
// first-live memo, the attribute cache, the insert hint - are left empty: they
// hold no state a reader of the clone could tell from a cold cache. stagedOn
// and the prelim buffers are left empty too: they hold the content of a type
// that is detached from every document, and a type reachable from a document's
// structs or root names is attached.
func (c *docCloner) fillType(s, t *abstractType) {
	t.doc = c.dst
	t.name = s.name
	t.length = s.length
	t.start = c.index.copyOf(s.start)
	t.item = c.index.copyOf(s.item)
	if s.itemMap == nil {
		t.itemMap = nil
	} else {
		m := make(map[string]*Item, len(s.itemMap))
		for key, it := range s.itemMap {
			m[key] = c.index.copyOf(it)
		}
		t.itemMap = m
	}
	t.hasFormatting = s.hasFormatting
	t.cleanFormatting = s.cleanFormatting
	t.fmtGen = s.fmtGen
	t.disableMarkers = s.disableMarkers
}

// linkStructs repoints every copied struct's links at the copies.
func (c *docCloner) linkStructs() {
	for _, cs := range c.index.byClient {
		for i, it := range cs.src {
			c.linkStruct(it, cs.dst[i])
		}
	}
	for i, it := range c.parkedSrc {
		if it != nil {
			c.linkStruct(it, c.parked[i])
		}
	}
}

func (c *docCloner) linkStruct(it, cp *Item) {
	cp.Left = c.index.copyOf(it.Left)
	cp.Right = c.index.copyOf(it.Right)
	cp.MovedBy = c.index.copyOf(it.MovedBy)
	if it.Parent != nil {
		cp.Parent = c.typeFor(it.Parent)
	}
}

// copyPendingDeletes copies the parked delete ranges, their count, and the
// clients a struct has reached since the last retry.
func (c *docCloner) copyPendingDeletes() {
	src, dst := c.src.store, c.dst.store
	// The ranges as parked, neither sorted nor merged, so a retry finds them
	// as on the source.
	ds := DeleteSet{clients: make(map[ClientID][]DeleteRange, len(src.pendingDs.clients))}
	for client, ranges := range src.pendingDs.clients {
		ds.clients[client] = append([]DeleteRange(nil), ranges...)
	}
	if src.pendingDs.order != nil {
		ds.order = append([]ClientID{}, src.pendingDs.order...)
	}
	dst.pendingDs = ds
	dst.pendingDsRanges = src.pendingDsRanges
	if src.pendingDsMoved != nil {
		moved := make(map[ClientID]struct{}, len(src.pendingDsMoved))
		for client := range src.pendingDsMoved {
			moved[client] = struct{}{}
		}
		dst.pendingDsMoved = moved
	}
}

// copyPendingMoves copies the deferred moves, which are structs of the store.
func (c *docCloner) copyPendingMoves() {
	src, dst := c.src.store, c.dst.store
	if src.pendingMoves == nil {
		return
	}
	moves := make(map[ClientID]pendingMoveHeap, len(src.pendingMoves))
	for client, items := range src.pendingMoves {
		cp := make(pendingMoveHeap, 0, len(items))
		for _, entry := range items {
			if copied := c.index.copyOf(entry.move); copied != nil {
				cp = append(cp, pendingMove{clock: entry.clock, move: copied})
			}
		}
		moves[client] = cp
	}
	dst.pendingMoves = moves
	dst.pendingMoveCount = src.pendingMoveCount
	dst.deletedPendingMoves = src.deletedPendingMoves
	dst.pendingMoveMapCap = src.pendingMoveMapCap
}

// itemIndex maps a struct of the source store to its copy. Both stores hold
// each client's structs in one clock-ordered slice, so a struct's copy sits at
// its own index in the copied slice: a lookup is a check of the cursor the
// last lookup left, then of the next entry, then a binary search by clock. A
// list is walked in near-clock order, so most lookups stop at the cursor.
type itemIndex struct {
	byClient map[ClientID]*clientStructs
	last     *clientStructs
	lastID   ClientID
	hasLast  bool
}

// clientStructs is one client's structs and their copies, at equal indices.
type clientStructs struct {
	src []*Item
	dst []*Item
	cur int
}

func (x *itemIndex) client(id ClientID) *clientStructs {
	if x.hasLast && x.lastID == id {
		return x.last
	}
	cs := x.byClient[id]
	x.last, x.lastID, x.hasLast = cs, id, true
	return cs
}

// copyOf returns the copy of struct p, or nil for a nil p and for a struct the
// store does not hold - which a consistent store never links to.
func (x *itemIndex) copyOf(p *Item) *Item {
	if p == nil {
		return nil
	}
	cs := x.client(p.ID.Client)
	if cs == nil {
		return nil
	}
	if cs.cur < len(cs.src) && cs.src[cs.cur] == p {
		return cs.dst[cs.cur]
	}
	if cs.cur+1 < len(cs.src) && cs.src[cs.cur+1] == p {
		cs.cur++
		return cs.dst[cs.cur]
	}
	lo, hi := 0, len(cs.src)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if cs.src[mid].ID.Clock < p.ID.Clock {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(cs.src) && cs.src[lo] == p {
		cs.cur = lo
		return cs.dst[lo]
	}
	return nil
}
