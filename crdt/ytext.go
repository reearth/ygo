package crdt

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// textSub pairs a unique subscription ID with a YTextEvent callback.
type textSub struct {
	id uint64
	fn func(YTextEvent)
}

// YText is a shared rich-text type. Characters are stored as ContentString
// items; formatting spans are ContentFormat items (which do not count toward
// logical length).
type YText struct {
	abstractType
	subIDGen  uint64
	observers []textSub
}

func (txt *YText) baseType() *abstractType { return &txt.abstractType }

// prepareFire snapshots the current observer slice inside the document write
// lock and returns a closure that fires all snapshotted observers (N-C1).
// computeDelta is called here (under the lock) so it sees a consistent view of
// the item list; calling it outside the lock after releasing would risk racing
// with the next transaction.
func (txt *YText) prepareFire(txn *Transaction, _ map[string]struct{}) func() {
	if len(txt.observers) == 0 {
		return nil
	}
	delta := txt.computeDelta(txn)
	snap := make([]textSub, len(txt.observers))
	copy(snap, txt.observers)
	e := YTextEvent{Target: txt, Txn: txn, Delta: delta}
	return func() {
		for _, s := range snap {
			s.fn(e)
		}
	}
}

// computeDelta builds a Quill-compatible delta for the changes in txn.
//
// In addition to ContentString inserts/deletes, it now accounts for
// ContentFormat changes so that a Format() call produces the correct
// Retain+Attributes ops in the observer delta.
//
// Two attribute maps are maintained as the item list is walked:
//   - currentAttrs: formatting state in the document after the transaction.
//   - oldAttrs:     formatting state before the transaction.
//
// When the two diverge at a retained text segment, a Retain+Attributes delta
// is emitted expressing the diff. A trailing plain Retain is omitted per the
// Quill convention; a trailing Retain with attributes is kept because it
// expresses a real formatting change.
func (txt *YText) computeDelta(txn *Transaction) []Delta {
	var ops []Delta
	retain := 0
	currentAttrs := make(Attributes) // format state in the final document
	oldAttrs := make(Attributes)     // format state before the transaction

	// flushRetain emits any accumulated retain characters. If the formatting
	// changed over this segment (currentAttrs ≠ oldAttrs), the retain carries
	// the attribute diff. oldAttrs is NOT updated here — it only changes when
	// pre-existing ContentFormat items are encountered (old/deleted), so that
	// the diff for subsequent segments is computed against the true pre-txn state.
	flushRetain := func() {
		if retain <= 0 {
			return
		}
		diff := attrDiff(currentAttrs, oldAttrs)
		if len(diff) > 0 {
			ops = append(ops, Delta{Op: DeltaOpRetain, Retain: retain, Attributes: diff})
		} else {
			ops = append(ops, Delta{Op: DeltaOpRetain, Retain: retain})
		}
		retain = 0
	}

	for item := txt.start; item != nil; item = item.Right {
		beforeClock := txn.beforeState.Clock(item.ID.Client)
		isNew := item.ID.Clock >= beforeClock

		switch c := item.Content.(type) {
		case *ContentString:
			if isNew {
				if !item.Deleted {
					flushRetain()
					d := Delta{Op: DeltaOpInsert, Insert: c.Str}
					if len(currentAttrs) > 0 {
						attrs := make(Attributes, len(currentAttrs))
						for k, v := range currentAttrs {
							attrs[k] = v
						}
						d.Attributes = attrs
					}
					ops = append(ops, d)
				}
				// new + immediately deleted → net no-op; skip
			} else if txn.deleteSet.IsDeleted(item.ID) {
				flushRetain()
				ops = append(ops, Delta{Op: DeltaOpDelete, Delete: c.Len()})
			} else if !item.Deleted {
				retain += c.Len()
			}

		case *ContentEmbed:
			// #74 D3: observers must see embed inserts/deletes/retains
			// alongside text. Each embed counts as one UTF-16 unit
			// (matches Yjs convention; see YText.InsertEmbed in v1.12.0).
			if isNew {
				if !item.Deleted {
					flushRetain()
					d := Delta{Op: DeltaOpInsert, Insert: c.Val}
					if len(currentAttrs) > 0 {
						attrs := make(Attributes, len(currentAttrs))
						for k, v := range currentAttrs {
							attrs[k] = v
						}
						d.Attributes = attrs
					}
					ops = append(ops, d)
				}
			} else if txn.deleteSet.IsDeleted(item.ID) {
				flushRetain()
				ops = append(ops, Delta{Op: DeltaOpDelete, Delete: 1})
			} else if !item.Deleted {
				retain += 1
			}

		case *ContentType:
			// #74 D3: rare in YText (used by some editors to embed YArray/
			// YMap as inline objects). Treat like an embed for delta
			// purposes — length 1, value is the unwrapped nested type.
			if isNew {
				if !item.Deleted {
					flushRetain()
					d := Delta{Op: DeltaOpInsert, Insert: toJSONValue(c)}
					if len(currentAttrs) > 0 {
						attrs := make(Attributes, len(currentAttrs))
						for k, v := range currentAttrs {
							attrs[k] = v
						}
						d.Attributes = attrs
					}
					ops = append(ops, d)
				}
			} else if txn.deleteSet.IsDeleted(item.ID) {
				flushRetain()
				ops = append(ops, Delta{Op: DeltaOpDelete, Delete: 1})
			} else if !item.Deleted {
				retain += 1
			}

		case *ContentFormat:
			if isNew {
				if !item.Deleted {
					// New format marker: flush the preceding retained text as a
					// plain retain (pre-format characters are unaffected), then
					// advance currentAttrs to reflect the new marker.
					flushRetain()
					if c.Val == nil {
						delete(currentAttrs, c.Key)
					} else {
						currentAttrs[c.Key] = c.Val
					}
				}
				// new + deleted → transient marker, no net effect; skip
			} else if txn.deleteSet.IsDeleted(item.ID) {
				// Pre-existing marker deleted in this txn: it was active before
				// (update oldAttrs) but is gone now (leave currentAttrs alone).
				// Flush any pending retain FIRST so the diff is computed against
				// the state BEFORE this marker took effect — without the flush
				// a retain that lives before this position gets a phantom attr
				// diff (surfaced by #71/A1 where Format now deletes overlapping
				// markers in its target range).
				flushRetain()
				if c.Val == nil {
					delete(oldAttrs, c.Key)
				} else {
					oldAttrs[c.Key] = c.Val
				}
			} else if !item.Deleted {
				// Unchanged pre-existing marker: advance both maps in sync.
				if c.Val == nil {
					delete(currentAttrs, c.Key)
					delete(oldAttrs, c.Key)
				} else {
					currentAttrs[c.Key] = c.Val
					oldAttrs[c.Key] = c.Val
				}
			}
		}
	}

	// Trailing retain: emit only when there is a formatting change (plain
	// trailing retain is omitted per Quill convention).
	if retain > 0 {
		diff := attrDiff(currentAttrs, oldAttrs)
		if len(diff) > 0 {
			ops = append(ops, Delta{Op: DeltaOpRetain, Retain: retain, Attributes: diff})
		}
	}
	return ops
}

// attrDiff returns the attribute changes needed to go from old to current.
// Keys present in current with a different value use the new value.
// Keys present in old but absent from current map to nil (removal signal).
// Returns nil when the two maps are equal.
func attrDiff(current, old Attributes) Attributes {
	var diff Attributes
	for k, v := range current {
		if oldV, exists := old[k]; !exists || oldV != v {
			if diff == nil {
				diff = make(Attributes)
			}
			diff[k] = v
		}
	}
	for k := range old {
		if _, exists := current[k]; !exists {
			if diff == nil {
				diff = make(Attributes)
			}
			diff[k] = nil
		}
	}
	return diff
}

// Len returns the number of non-deleted UTF-16 code units (not Unicode code
// points). Supplementary characters (U+10000 and above) count as 2 units,
// matching JavaScript's String.length semantics and the Yjs wire protocol.
func (txt *YText) Len() int { return txt.length }

// Insert inserts text at logical character position index.
//
// Attribute semantics (#71 vectors A2 + A3, v1.13.0):
//
//   - When attrs is nil or empty, the new text inherits whatever
//     ContentFormat markers are in effect at the cursor position
//     (computed via currentAttributesAt). Typing at the end of bold
//     text continues being bold without the caller passing {bold:true}
//     explicitly. Matches Yjs JS's insertText inheritance behavior.
//
//   - When attrs is non-empty, the difference between attrs and the
//     cursor's currentAttributes drives marker emission. Opening
//     markers are inserted for keys that need to change; after the
//     text item, negating closing markers are emitted to revert to the
//     pre-insert state. Without these closers, formatting would bleed
//     rightward through subsequent retained text — the A3 gap pre-fix.
//
// Keys whose requested value already matches the current state produce no
// markers (empty diff = no work).
func (txt *YText) Insert(txn *Transaction, index int, text string, attrs Attributes) {
	if text == "" {
		return
	}
	t := &txt.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
		// left is now the left half; left.Right is the right half.
	}

	// Fast path: when caller passed nil/empty attrs, there's nothing to diff,
	// no markers to emit, and the new text inherits surrounding formatting
	// via the existing markers in the linked list — ToDelta walks them as it
	// emits the new text. Skipping currentAttributesAt here is what keeps
	// sequential typing in a long doc at near-O(1) per Insert instead of
	// degrading to O(n²) — currentAttributesAt itself is O(n) (walks from
	// txt.start to the anchor).
	type diffEntry struct {
		key    string
		newVal any
		oldVal any // currentAttrs[key], or nil if absent
		hadKey bool
	}
	var diff []diffEntry
	if len(attrs) > 0 {
		// Caller passed explicit attrs — we need currentAttributes to compute
		// which keys actually need an open/close pair around the insert.
		currentAttrs := txt.currentAttributesAt(left)

		// Deterministic emission order — Go map iteration is randomized, but
		// linked-list order is observable, so sort keys.
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			newVal := attrs[k]
			oldVal, hadKey := currentAttrs[k]
			// Treat (absent in current) and (newVal == nil) as the same state
			// — both mean "no formatting for this key." No marker needed.
			if !hadKey && newVal == nil {
				continue
			}
			// reflect.DeepEqual safely compares any JSON-decoded value
			// (including []any / map[string]any), where `==` would panic.
			same := hadKey && reflect.DeepEqual(oldVal, newVal)
			if same {
				continue
			}
			diff = append(diff, diffEntry{key: k, newVal: newVal, oldVal: oldVal, hadKey: hadKey})
		}
	}

	var origin *ID
	var originRight *ID
	if left != nil {
		end := left.ID.Clock + uint64(left.Content.Len()) - 1
		origin = &ID{Client: left.ID.Client, Clock: end}
		if left.Right != nil {
			id := left.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}

	clock := txn.doc.store.NextClock(txn.doc.clientID)

	// Opening markers — one per key whose value needs to change.
	for _, d := range diff {
		fmtItem := &Item{
			ID:          ID{Client: txn.doc.clientID, Clock: clock},
			Origin:      origin,
			OriginRight: originRight,
			Left:        left,
			Parent:      t,
			Content:     NewContentFormat(d.key, d.newVal),
		}
		fmtItem.integrate(txn, 0)
		left = fmtItem
		origin = &ID{Client: fmtItem.ID.Client, Clock: fmtItem.ID.Clock}
		originRight = nil
		clock = txn.doc.store.NextClock(txn.doc.clientID)
	}

	// The text item itself.
	item := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: clock},
		Origin:      origin,
		OriginRight: originRight,
		Left:        left,
		Parent:      t,
		Content:     NewContentString(text),
	}
	// Signal to integrate that this is a local insert at a known position so
	// it can do a partial cache invalidation instead of a full clear.
	if index > 0 {
		t.insertHint = index
	}
	item.integrate(txn, 0)

	// Negating (closing) markers — for each diff key, revert to the pre-insert
	// state. If the key was absent in currentAttrs, emit a nil-value marker
	// (which deletes the key from currentAttributes going forward). If the key
	// had a prior value, emit a marker carrying that value (restoring it).
	//
	// Without this loop, the opened attributes would bleed past the inserted
	// text into subsequent retained content — the A3 gap.
	if len(diff) > 0 {
		left = item
		// Origin must be the LAST clock of the text item (item.ID.Clock + Len - 1),
		// not the first — otherwise YATA places the closer mid-text rather than
		// immediately after.
		origin = &ID{
			Client: item.ID.Client,
			Clock:  item.ID.Clock + uint64(item.Content.Len()) - 1,
		}
		if left.Right != nil {
			rid := left.Right.ID
			originRight = &rid
		} else {
			originRight = nil
		}
		clock = txn.doc.store.NextClock(txn.doc.clientID)

		for _, d := range diff {
			var revertVal any
			if d.hadKey {
				revertVal = d.oldVal
			}
			closeItem := &Item{
				ID:          ID{Client: txn.doc.clientID, Clock: clock},
				Origin:      origin,
				OriginRight: originRight,
				Left:        left,
				Parent:      t,
				Content:     NewContentFormat(d.key, revertVal),
			}
			closeItem.integrate(txn, 0)
			left = closeItem
			origin = &ID{Client: closeItem.ID.Client, Clock: closeItem.ID.Clock}
			originRight = nil
			clock = txn.doc.store.NextClock(txn.doc.clientID)
		}
	}
}

// currentAttributesAt computes the format state in effect at the cursor
// position immediately AFTER `anchor`. Walks from txt.start to anchor,
// applying each live ContentFormat marker in linked-list order. Tombstoned
// markers are skipped — they no longer contribute to the formatting state.
//
// When anchor is nil, the cursor is BEFORE any item (insertion at the
// document start), so no markers can be in effect — returns an empty map
// without walking the list. Without this early exit, the loop would walk
// the whole document and return the end-state attrs instead of the
// start-state attrs.
//
// Added in v1.13.0 (#71 vectors A2 + A3). The caller is responsible for
// not invoking this with stale anchor pointers; YText.Insert calls it
// immediately after leftNeighbourAt.
func (txt *YText) currentAttributesAt(anchor *Item) Attributes {
	attrs := make(Attributes)
	if anchor == nil {
		return attrs
	}
	for item := txt.start; item != nil; item = item.Right {
		if !item.Deleted {
			if cf, ok := item.Content.(*ContentFormat); ok {
				if cf.Val == nil {
					delete(attrs, cf.Key)
				} else {
					attrs[cf.Key] = cf.Val
				}
			}
		}
		if item == anchor {
			break
		}
	}
	return attrs
}

// InsertEmbed inserts an embedded object (image, formula, video metadata, or
// any other inline non-text payload) at logical UTF-16 position index. Each
// embed counts as one UTF-16 code unit in the document's length, matching
// Yjs JS's `YText.insertEmbed` semantics.
//
// attrs may carry inline attributes that apply ONLY to this embed item.
// They are emitted as opening + closing ContentFormat markers around the
// embed so subsequent inserts are unaffected. Pass nil for an unstyled embed.
//
// Must be called from inside a Transact callback.
//
// Added in v1.12.0 (#76).
func (txt *YText) InsertEmbed(txn *Transaction, index int, embed any, attrs Attributes) {
	t := &txt.abstractType
	left, offset := t.leftNeighbourAt(index)
	if offset > 0 {
		splitItem(txn, left, offset)
	}

	var origin *ID
	var originRight *ID
	if left != nil {
		end := left.ID.Clock + uint64(left.Content.Len()) - 1
		origin = &ID{Client: left.ID.Client, Clock: end}
		if left.Right != nil {
			id := left.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}

	clock := txn.doc.store.NextClock(txn.doc.clientID)

	// Opening attr markers — same pattern as Insert with attrs.
	if len(attrs) > 0 {
		for k, v := range attrs {
			fmtItem := &Item{
				ID:          ID{Client: txn.doc.clientID, Clock: clock},
				Origin:      origin,
				OriginRight: originRight,
				Left:        left,
				Parent:      t,
				Content:     NewContentFormat(k, v),
			}
			fmtItem.integrate(txn, 0)
			left = fmtItem
			origin = &ID{Client: fmtItem.ID.Client, Clock: fmtItem.ID.Clock}
			originRight = nil
			clock = txn.doc.store.NextClock(txn.doc.clientID)
		}
	}

	// The embed item itself.
	item := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: clock},
		Origin:      origin,
		OriginRight: originRight,
		Left:        left,
		Parent:      t,
		Content:     NewContentEmbed(embed),
	}
	if index > 0 {
		t.insertHint = index
	}
	item.integrate(txn, 0)

	// Closing (negating) attr markers — without these the attrs would bleed
	// into subsequent content. Unlike Insert (which currently doesn't emit
	// negated attrs — that's the deferred A3 work), InsertEmbed scopes attrs
	// to the embed exactly, so we always close here.
	if len(attrs) > 0 {
		left = item
		origin = &ID{Client: item.ID.Client, Clock: item.ID.Clock}
		if left.Right != nil {
			rid := left.Right.ID
			originRight = &rid
		} else {
			originRight = nil
		}
		clock = txn.doc.store.NextClock(txn.doc.clientID)

		for k := range attrs {
			closeItem := &Item{
				ID:          ID{Client: txn.doc.clientID, Clock: clock},
				Origin:      origin,
				OriginRight: originRight,
				Left:        left,
				Parent:      t,
				Content:     NewContentFormat(k, nil),
			}
			closeItem.integrate(txn, 0)
			left = closeItem
			origin = &ID{Client: closeItem.ID.Client, Clock: closeItem.ID.Clock}
			originRight = nil
			clock = txn.doc.store.NextClock(txn.doc.clientID)
		}
	}
}

// Delete removes length characters starting at logical position index.
//
// After the range is tombstoned, Delete examines ContentFormat markers in
// the local deletion region and tombstones any that no longer wrap live
// content — matching Yjs JS's cleanupFormattingGap. Without this, repeated
// edits leave orphan markers that bloat the store and inflate the encoded
// delete-set. See #71 vector A4.
//
// The scan is gated on txt.hasFormatting: YText that has never had a
// ContentFormat integrated (the common plain-text case) skips the walk
// entirely. This mirrors Yjs's `_hasFormatting` flag gating and is the
// dominant perf win on head-delete workloads in plain-text documents (#86).
//
// When the scan does run it is scoped to the region between the item just
// before the deletion and the first live countable item after it, so the
// per-Delete cost is O(region size + scope size of any markers found)
// rather than O(document).
func (txt *YText) Delete(txn *Transaction, index, length int) {
	// Capture the anchor immediately before the deletion start so we can
	// walk forward from there post-deletion. nil means "start of document."
	var startAnchor *Item
	if index > 0 {
		startAnchor, _ = txt.leftNeighbourAt(index)
	}

	deleteRange(&txt.abstractType, txn, index, length)

	if !txt.hasFormatting {
		// No ContentFormat has ever been integrated into this type — nothing
		// to clean up. Skip the walk: this is the head-delete benchmark's
		// dominant saving (#86).
		return
	}
	txt.cleanupDanglingFormatsInRegion(txn, startAnchor)
}

// cleanupDanglingFormatsInRegion tombstones ContentFormat items in the local
// region of a recent deletion whose effect zone now contains no live
// countable content. Called from Delete with the item immediately before
// the deletion (#71 vector A4).
//
// Two categories of redundancy:
//   - An opener `{key: val}` is redundant when no live countable item lies
//     between it and the next same-key marker (the scope boundary).
//   - A closer `{key: nil}` is redundant when no live opener for the same
//     key precedes it within scope.
//
// The outer walk is bounded: we stop after seeing one live countable item
// past the deletion, which is enough to cover markers whose scope was
// changed by this delete. Markers further away are unaffected by this
// specific deletion.
func (txt *YText) cleanupDanglingFormatsInRegion(txn *Transaction, startAnchor *Item) {
	var node *Item
	if startAnchor == nil {
		// Sequential head-deletes (the BenchmarkYText_Delete worst case)
		// accumulate tombstones at txt.start; without the memoised pointer
		// every cleanup re-walks all of them and the per-delete cost is
		// O(deletes-so-far). Use the cache instead — issue #86.
		node = txt.firstLiveFromStart()
	} else {
		node = startAnchor.Right
	}
	seenLivePast := false
	for node != nil {
		next := node.Right
		if node.Deleted {
			node = next
			continue
		}
		if cf, ok := node.Content.(*ContentFormat); ok {
			if cf.Val == nil {
				// Closer: walk Left until a same-key marker. A tombstoned
				// marker doesn't count as a live opener.
				hasLiveOpener := false
				for p := node.Left; p != nil; p = p.Left {
					if p.Deleted {
						continue
					}
					pcf, isFmt := p.Content.(*ContentFormat)
					if !isFmt {
						continue
					}
					if pcf.Key != cf.Key {
						continue
					}
					if pcf.Val != nil {
						hasLiveOpener = true
					}
					break
				}
				if !hasLiveOpener {
					node.delete(txn)
				}
			} else {
				// Opener: walk Right looking for live countable content in
				// scope (until the next same-key marker).
				hasLiveInScope := false
				for n := node.Right; n != nil; n = n.Right {
					if n.Deleted {
						continue
					}
					if ncf, isFmt := n.Content.(*ContentFormat); isFmt {
						if ncf.Key == cf.Key {
							break
						}
						continue
					}
					if n.Content.IsCountable() {
						hasLiveInScope = true
						break
					}
				}
				if !hasLiveInScope {
					node.delete(txn)
				}
			}
			node = next
			continue
		}
		if node.Content.IsCountable() {
			if seenLivePast {
				// We've seen one live countable past the deletion; markers
				// further on couldn't have been affected by this delete.
				break
			}
			seenLivePast = true
		}
		node = next
	}
}

// Format applies attrs to the character range [index, index+length).
//
// For each attribute being set (non-nil value) two ContentFormat items are
// inserted: an opening marker at index and a closing nil marker at
// index+length. This bounds the formatting to the requested range so that
// text inserted after the range is not implicitly formatted.
//
// For attribute removal (nil value) only the opening nil marker is inserted.
// The removal marker overrides any preceding non-nil marker for the same key
// when the document state is read left-to-right by ToDelta.
//
// Note: removal of an attribute whose source marker was inserted by a
// concurrent peer may not produce the intended result because YATA places the
// removal marker before the source marker when both share the same origin.
// Full concurrent attribute removal is tracked as a follow-up improvement.
func (txt *YText) Format(txn *Transaction, index, length int, attrs Attributes) {
	if len(attrs) == 0 || length <= 0 {
		return
	}
	t := &txt.abstractType

	// Deterministic emission order — Go map iteration is randomized, but
	// linked-list order is observable, so sort keys.
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Faithful port of Yjs JS YText.formatText (yjs@13.6.30), built on an
	// ItemTextListPosition-equivalent cursor (itemTextPos). The previous ygo
	// implementation deleted ALL same-key markers in the range, which
	// over-deleted the closing marker bounding content after index+length and
	// stripped formatting from the surrounding run (#123). This version inserts
	// opening markers only where the value changes, records the value to restore
	// after the range (the "negated" map), deletes only in-range overlapping
	// markers, and restores the post-range state — matching Yjs across fresh and
	// applyUpdate-loaded docs (the un-split-run case, yjs#606).
	pos := t.findTextPos(txn, index)
	minimizeAttributeChanges(pos, attrs)
	negated := t.insertAttributes(txn, pos, keys, attrs)

	remaining := length
	for pos.right != nil &&
		(remaining > 0 || (len(negated) > 0 && (pos.right.Deleted || isContentFormat(pos.right)))) {
		if !pos.right.Deleted {
			if cf, ok := pos.right.Content.(*ContentFormat); ok {
				if _, touched := attrs[cf.Key]; touched {
					if attrEqual(attrs[cf.Key], cf.Val) {
						// Past this marker the value already matches the target.
						delete(negated, cf.Key)
					} else {
						if remaining == 0 {
							// Don't extend the restore set past the range.
							break
						}
						// This value follows the range — restore it afterwards.
						negated[cf.Key] = cf.Val
					}
					pos.right.delete(txn)
				}
			} else {
				// Countable text/embed item.
				n := pos.right.Content.Len()
				if remaining < n {
					splitItem(txn, pos.right, remaining) // clean boundary at index+length
					n = pos.right.Content.Len()          // now == remaining
				}
				remaining -= n
			}
		}
		pos.forward()
	}

	t.insertNegatedAttributes(txn, pos, negated, keys)
}

// isContentFormat reports whether item carries a ContentFormat marker.
func isContentFormat(item *Item) bool {
	_, ok := item.Content.(*ContentFormat)
	return ok
}

// attrEqual reports whether two attribute values are equal, treating a missing
// key and a nil value as the same "no formatting" state. reflect.DeepEqual is
// used so JSON-decoded composite values (maps/slices) compare without panicking.
func attrEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.DeepEqual(a, b)
}

// itemTextPos is a cursor into a YText's item list, mirroring Yjs JS's
// ItemTextListPosition. cur holds the formatting state in effect at the cursor
// (i.e. just before right). It backs the Yjs-faithful formatText algorithm.
type itemTextPos struct {
	left  *Item
	right *Item
	index int
	cur   Attributes
}

// forward advances the cursor one item rightward, updating cur when passing a
// live ContentFormat and index when passing live countable content.
func (p *itemTextPos) forward() {
	if p.right == nil {
		return
	}
	if cf, ok := p.right.Content.(*ContentFormat); ok {
		if !p.right.Deleted {
			updateAttr(p.cur, cf)
		}
	} else if !p.right.Deleted {
		p.index += p.right.Content.Len()
	}
	p.left = p.right
	p.right = p.right.Right
}

// updateAttr applies a ContentFormat marker to an attribute map: a nil value
// clears the key (no formatting), a non-nil value sets it.
func updateAttr(attrs Attributes, cf *ContentFormat) {
	if cf.Val == nil {
		delete(attrs, cf.Key)
	} else {
		attrs[cf.Key] = cf.Val
	}
}

// findTextPos returns a cursor at logical position index, splitting the
// boundary item when index falls inside it, with cur reflecting the format
// state at index. Mirrors Yjs findPosition/findNextPosition.
func (t *abstractType) findTextPos(txn *Transaction, index int) *itemTextPos {
	pos := &itemTextPos{right: t.start, cur: make(Attributes)}
	count := index
	for pos.right != nil && count > 0 {
		if cf, ok := pos.right.Content.(*ContentFormat); ok {
			if !pos.right.Deleted {
				updateAttr(pos.cur, cf)
			}
		} else if !pos.right.Deleted {
			n := pos.right.Content.Len()
			if count < n {
				splitItem(txn, pos.right, count) // clean boundary at index
				n = pos.right.Content.Len()
			}
			pos.index += n
			count -= n
		}
		pos.left = pos.right
		pos.right = pos.right.Right
	}
	return pos
}

// insertFormatAt inserts a ContentFormat(key,val) marker at the cursor and
// advances the cursor past it. Mirrors the marker insertion in Yjs
// insertAttributes / insertNegatedAttributes.
func (t *abstractType) insertFormatAt(txn *Transaction, pos *itemTextPos, key string, val any) {
	origin, originRight := itemOrigins(pos.left, t)
	it := &Item{
		ID:          ID{Client: txn.doc.clientID, Clock: txn.doc.store.NextClock(txn.doc.clientID)},
		Origin:      origin,
		OriginRight: originRight,
		Left:        pos.left,
		Parent:      t,
		Content:     NewContentFormat(key, val),
	}
	it.integrate(txn, 0)
	pos.right = it
	pos.forward()
}

// minimizeAttributeChanges advances the cursor past leading deleted items and
// ContentFormat markers that already match the requested attributes, so no
// redundant opening marker is inserted. Mirrors Yjs minimizeAttributeChanges.
func minimizeAttributeChanges(pos *itemTextPos, attrs Attributes) {
	for pos.right != nil {
		if pos.right.Deleted {
			// advance
		} else if cf, ok := pos.right.Content.(*ContentFormat); ok && attrEqual(attrs[cf.Key], cf.Val) {
			// redundant marker already matching the target — advance
		} else {
			break
		}
		pos.forward()
	}
}

// insertAttributes opens a marker for each key whose value differs from the
// state at the cursor, returning the negated map of values to restore after the
// range (a key present with a nil value means "restore to no formatting").
// Mirrors Yjs insertAttributes.
func (t *abstractType) insertAttributes(txn *Transaction, pos *itemTextPos, keys []string, attrs Attributes) Attributes {
	negated := make(Attributes)
	for _, key := range keys {
		val := attrs[key]
		curVal := pos.cur[key] // nil when absent
		if !attrEqual(curVal, val) {
			negated[key] = curVal
			t.insertFormatAt(txn, pos, key, val)
		}
	}
	return negated
}

// insertNegatedAttributes restores the negated values at the cursor. It first
// skips over deleted items and existing markers that already provide a negated
// value (dropping those keys), then inserts a marker for each remaining negated
// key. Mirrors Yjs insertNegatedAttributes.
func (t *abstractType) insertNegatedAttributes(txn *Transaction, pos *itemTextPos, negated Attributes, keys []string) {
	for pos.right != nil {
		if pos.right.Deleted {
			pos.forward()
			continue
		}
		if cf, ok := pos.right.Content.(*ContentFormat); ok {
			if v, has := negated[cf.Key]; has && attrEqual(v, cf.Val) {
				delete(negated, cf.Key)
				pos.forward()
				continue
			}
		}
		break
	}
	for _, key := range keys {
		if v, has := negated[key]; has {
			t.insertFormatAt(txn, pos, key, v)
		}
	}
}

// itemOrigins returns the origin and originRight IDs for a new item to be
// inserted immediately after left. Handles ContentFormat items (Len == 0)
// correctly: their origin clock equals their own ID clock.
func itemOrigins(left *Item, t *abstractType) (origin, originRight *ID) {
	if left != nil {
		n := left.Content.Len()
		var clock uint64
		if n > 0 {
			clock = left.ID.Clock + uint64(n) - 1
		} else {
			clock = left.ID.Clock
		}
		origin = &ID{Client: left.ID.Client, Clock: clock}
		if left.Right != nil {
			id := left.Right.ID
			originRight = &id
		}
	} else if t.start != nil {
		id := t.start.ID
		originRight = &id
	}
	return
}

// ToString returns the concatenation of all non-deleted character runs,
// excluding format markers.
// Must not be called from inside a Transact callback.
func (txt *YText) ToString() string {
	if doc := txt.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	return txt.toStringLocked()
}

// toStringLocked is the lock-free body of ToString; callers must already
// hold the doc lock. Used by ToString (top-level) and toJSONValue (#75)
// when a YText appears as a value inside a YArray or YMap.
func (txt *YText) toStringLocked() string {
	t := &txt.abstractType
	var sb strings.Builder
	for item := t.start; item != nil; item = item.Right {
		if !item.Deleted {
			if cs, ok := item.Content.(*ContentString); ok {
				sb.WriteString(cs.Str)
			}
		}
	}
	return sb.String()
}

// ToJSON returns the text content serialised as a JSON string.
// Must not be called from inside a Transact callback.
func (txt *YText) ToJSON() ([]byte, error) {
	return json.Marshal(txt.ToString())
}

// ToDelta returns a Quill-compatible delta representing the current document
// state as a sequence of insert operations.
//
// Each run of plain text becomes one Delta with Op=DeltaOpInsert.
// Formatting attributes accumulated from ContentFormat markers are attached to
// the text run they precede. A nil attribute value signals the end of a span
// and is omitted from the output attributes map.
// Must not be called from inside a Transact callback.
func (txt *YText) ToDelta() []Delta {
	if doc := txt.doc; doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	var deltas []Delta
	currentAttrs := make(Attributes)

	// Consecutive string inserts that share the same attributes are coalesced
	// into one op, matching Yjs JS toDelta. Without this, a run split across
	// multiple Items (e.g. after Format splits a run at a range boundary) would
	// emit several adjacent ops with identical attributes, diverging from the
	// reference delta. Embeds always flush and emit their own op.
	var buf strings.Builder
	var bufAttrs Attributes // attributes of the buffered run; nil == unformatted
	// attrsDirty is set when a live ContentFormat changes currentAttrs, so we
	// only re-snapshot/compare attributes at an actual attribute boundary rather
	// than once per string item (the common case is a long run with no markers).
	attrsDirty := false

	attrsCopy := func() Attributes {
		if len(currentAttrs) == 0 {
			return nil
		}
		a := make(Attributes, len(currentAttrs))
		for k, v := range currentAttrs {
			a[k] = v
		}
		return a
	}
	// sameAsBuffered reports whether the live currentAttrs equals the buffered
	// run's attributes, treating an empty map and a nil snapshot as equal — so
	// no allocation is needed just to compare.
	sameAsBuffered := func() bool {
		if len(currentAttrs) != len(bufAttrs) {
			return false
		}
		for k, v := range currentAttrs {
			if bv, ok := bufAttrs[k]; !ok || !reflect.DeepEqual(v, bv) {
				return false
			}
		}
		return true
	}
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		deltas = append(deltas, Delta{Op: DeltaOpInsert, Insert: buf.String(), Attributes: bufAttrs})
		buf.Reset()
		bufAttrs = nil
	}

	for item := txt.start; item != nil; item = item.Right {
		if item.Deleted {
			continue
		}
		switch c := item.Content.(type) {
		case *ContentString:
			switch {
			case buf.Len() == 0:
				bufAttrs = attrsCopy()
			case attrsDirty && !sameAsBuffered():
				flush()
				bufAttrs = attrsCopy()
			}
			attrsDirty = false
			buf.WriteString(c.Str)
		case *ContentEmbed:
			// #76: embeds are emitted as their own Delta entries with Insert
			// carrying the embed value (not a string). Attributes attached
			// inline via opening + closing markers around the embed apply.
			flush()
			deltas = append(deltas, Delta{Op: DeltaOpInsert, Insert: c.Val, Attributes: attrsCopy()})
		case *ContentFormat:
			updateAttr(currentAttrs, c)
			attrsDirty = true
		}
	}
	flush()
	return deltas
}

// Observe registers fn to be called after every transaction that modifies this
// text. Returns an unsubscribe function. Uses ID-based lookup so out-of-order
// unsubscription removes the correct entry (C5).
//
// Acquiring doc.mu.Lock() serialises registration against Transact (N-C1).
// Do not call Observe from inside a Transact callback — that would deadlock.
func (txt *YText) Observe(fn func(YTextEvent)) func() {
	doc := txt.doc
	if doc != nil {
		doc.mu.Lock()
		defer doc.mu.Unlock()
	}
	txt.subIDGen++
	id := txt.subIDGen
	txt.observers = append(txt.observers, textSub{id: id, fn: fn})
	return func() {
		if doc := txt.doc; doc != nil {
			doc.mu.Lock()
			defer doc.mu.Unlock()
		}
		for i, s := range txt.observers {
			if s.id == id {
				txt.observers = append(txt.observers[:i], txt.observers[i+1:]...)
				return
			}
		}
	}
}

// ApplyDelta applies a Quill-compatible delta to the text within the given
// transaction. Each Delta must have exactly one of Op set:
//   - DeltaOpInsert: inserts d.Insert at the current cursor position with optional d.Attributes
//   - DeltaOpDelete: deletes d.Delete UTF-16 code units at the current cursor position
//   - DeltaOpRetain: advances the cursor by d.Retain UTF-16 code units; if d.Attributes is
//     non-nil, applies formatting to the retained range
//
// The cursor starts at position 0. ApplyDelta must be called from inside a
// Transact callback.
func (txt *YText) ApplyDelta(txn *Transaction, delta []Delta) {
	pos := 0
	for _, d := range delta {
		switch d.Op {
		case DeltaOpInsert:
			if s, ok := d.Insert.(string); ok {
				txt.Insert(txn, pos, s, d.Attributes)
				pos += utf16Len(s)
			}
		case DeltaOpDelete:
			deleteRange(&txt.abstractType, txn, pos, d.Delete)
		case DeltaOpRetain:
			if len(d.Attributes) > 0 {
				txt.Format(txn, pos, d.Retain, d.Attributes)
			}
			pos += d.Retain
		}
	}
}

// ObserveDeep registers fn to be called after any transaction that modifies
// this text or any nested shared type within it. Returns an unsubscribe function.
func (txt *YText) ObserveDeep(fn func(*Transaction)) func() {
	return txt.observeDeep(fn)
}
