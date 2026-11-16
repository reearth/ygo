package crdt

import (
	"fmt"
	"slices"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// ErrApplyBudget is returned, wrapped in a RefusedUpdateError, by a remote
// apply that would decode or add more than the ApplyBudget that
// WithApplyBudget gave it. It wraps ErrInvalidUpdate. The document keeps what
// the apply integrated before it reached the budget, or nothing on a document
// with WithWholeOrNothingApplies.
var ErrApplyBudget = fmt.Errorf("%w: it decodes or adds more than the document allows", ErrInvalidUpdate)

// ErrRootNotAllowed is returned, wrapped in a RefusedUpdateError whose Root
// names the root, by a remote apply that would create, write under or delete
// under a root type the WithAllowedRoots list does not name. It wraps
// ErrInvalidUpdate. The document keeps what the apply integrated before that
// struct, or nothing on a document with WithWholeOrNothingApplies.
var ErrRootNotAllowed = fmt.Errorf("%w: it writes under a root the document does not allow", ErrInvalidUpdate)

// RefusedUpdateError is the error a remote apply (ApplyUpdate and its V1 and
// V2 forms) returns on a document with WithApplyBudget, WithAllowedRoots,
// WithRefusedContent or WithApplyReport when the apply fails, for any
// reason, with what it did before it stopped.
//
// Err is why it failed: ErrApplyBudget, ErrRootNotAllowed,
// ErrContentNotAllowed, or any other error of a remote apply, such as
// ErrIntegrateScanBudget or one wrapping ErrInvalidUpdate. Unwrap returns it,
// so errors.Is matches it, and Error is its message.
//
// Decoded is the values the apply read, the run that passed the budget
// included. Structs and Bytes are the structs of the update the apply added
// to the document, parked ones included, and the bytes they carry; the struct
// that would have passed the budget is not among them. Split is the structs
// the apply made by splitting items the document held. On a document with
// WithWholeOrNothingApplies the document keeps none of it, and otherwise it
// keeps what Structs, Bytes and Split count. Root is the root a refused
// update named or reached that the list does not allow, and empty for any
// other failure.
type RefusedUpdateError struct {
	Err     error
	Decoded int
	Structs int
	Bytes   int
	Split   int
	Root    string
}

// Error is the message of Err, or a bare message when Err is nil.
func (e *RefusedUpdateError) Error() string {
	if e.Err == nil {
		return "crdt: update refused"
	}
	return e.Err.Error()
}

// Unwrap returns Err.
func (e *RefusedUpdateError) Unwrap() error { return e.Err }

// applyBudgetExceeded is the panic the tally raises when a remote apply
// passes its budget, at the value or the struct that passes it and before the
// struct enters the document; the apply recovers it as ErrApplyBudget.
type applyBudgetExceeded struct{}

// rootNotAllowed is the panic the tally raises when a remote apply would add
// to or delete under a root its list does not allow, before the struct
// enters the document or the item is deleted; the apply recovers it as
// ErrRootNotAllowed.
type rootNotAllowed struct{}

// budgetCallbackPanic wraps a panic the callback of option raised, so the
// apply's error names the callback rather than item integration.
type budgetCallbackPanic struct {
	option string
	v      any
}

// applyTally is what one remote apply has decoded and added, and the budget,
// roots and refused content kinds it may do so within, each asked for as the
// apply began (beginApplyTally).
type applyTally struct {
	// budget is what the apply may decode and add; a field of zero or below
	// is no limit (WithApplyBudget).
	budget ApplyBudget
	// roots is what the update may write under, nil or empty for every root.
	// allowedTypes are those of roots the document holds, resolved as the
	// apply began, and the ones the apply creates by an allowed name; rootOf
	// is the root above each type an item of the apply hung under.
	roots        []string
	allowedTypes map[*abstractType]bool
	rootOf       map[*abstractType]*abstractType
	// refused is the kinds of content the update may not carry, nil or empty
	// for every kind (WithRefusedContent).
	refused []ContentKind
	// draining is set while the drain of the parked queue runs: its structs
	// are not the update's, so they are not counted against Structs or Bytes.
	draining bool
	// rootDrops is the client of each parked struct the drain dropped under a
	// root the list does not allow, which another apply sent; the document
	// counts them as the apply ends (PendingStats.RootsDropped).
	rootDrops []ClientID
	// decoded, structs, bytes and split are RefusedUpdateError's counts, and
	// root its Root.
	decoded, structs, bytes, split int
	root                           string
	// stateCount is what the apply adds to the encoded state, on a document
	// that reports (ApplyReport.StateBytes, state_bytes.go).
	stateCount
}

// over reports whether used passes limit, a field of ApplyBudget, where zero
// or below is no limit.
func over(limit, used int) bool {
	return limit > 0 && used > limit
}

// tally is the tally of the remote apply running on d, or nil when none runs,
// when d has no budget, roots list, refused content kinds or report, or when
// d is nil.
func (d *Doc) tally() *applyTally {
	if d == nil {
		return nil
	}
	return d.counting
}

// beginApplyTally starts the tally of txn's apply when its document has a
// budget, a roots list, refused content kinds or a report, asking each
// callback once, with the apply's origin.
func beginApplyTally(txn *Transaction) {
	d := txn.doc
	if d.applyBudget == nil && d.allowedRoots == nil && d.refusedContent == nil && d.applyReport == nil {
		return
	}
	// The tally starts before the callbacks run, so an apply whose callback
	// panics returns a RefusedUpdateError too.
	t := &applyTally{}
	d.counting = t
	if d.applyBudget != nil {
		t.budget = d.callApplyBudget(txn.Origin)
	}
	if d.allowedRoots != nil {
		t.roots = d.callAllowedRoots(txn.Origin)
		if len(t.roots) > 0 {
			t.allowedTypes = make(map[*abstractType]bool, len(t.roots))
			for _, name := range t.roots {
				if shared, held := d.share[name]; held {
					t.allowedTypes[shared.baseType()] = true
				}
			}
		}
	}
	if d.refusedContent != nil {
		t.refused = d.callRefusedContent(txn.Origin)
	}
	if d.applyReport != nil {
		t.beginStateCount(d.store)
	}
}

// endApplyTally ends txn's tally and returns err, the apply's error, as a
// RefusedUpdateError carrying the counts when the apply failed. It counts the
// parked structs the drain dropped, unless a whole-or-nothing document took
// the apply back. An apply whose transaction commits what it did, one neither
// refused before it integrated nor rolled back, keeps the tally for its
// report, which the commit makes (WithApplyReport).
func endApplyTally(txn *Transaction, err error) error {
	d := txn.doc
	t := d.counting
	if t == nil {
		return err
	}
	d.counting = nil
	if len(t.rootDrops) > 0 && (err == nil || !d.wholeOrNothing) {
		if d.rootsDropped == nil {
			d.rootsDropped = make(map[ClientID]int)
		}
		for _, client := range t.rootDrops {
			d.rootsDropped[client]++
		}
	}
	if d.applyReport != nil && !txn.refused && !txn.rolledBack {
		// The apply's deletions are all made, so the delete set's growth is
		// read here; the commit takes off what its merges remove (stateDue).
		if t.stateOn {
			t.state += deleteSetGrowth(txn, t)
		}
		d.reporting = t
	}
	if err == nil {
		return nil
	}
	return &RefusedUpdateError{Err: err, Decoded: t.decoded, Structs: t.structs, Bytes: t.bytes, Split: t.split, Root: t.root}
}

// drainCount is the count as a drain of the pending queue began, which a
// drain taken back returns to (takeBackDrain).
type drainCount struct {
	split int
	state stateAtDrain
}

// beginDrain is the count as the drain of s's parked queue begins.
func (t *applyTally) beginDrain(s *StructStore) drainCount {
	if t == nil {
		return drainCount{}
	}
	return drainCount{split: t.split, state: t.beginStateDrain(s)}
}

// takeBackDrain returns the counts to at, where the drain began, once
// WithWholeOrNothingApplies has taken the drain back and the store is where
// the drain began: the document keeps none of its splits.
func (t *applyTally) takeBackDrain(at drainCount, s *StructStore) {
	if t != nil {
		t.split = at.split
		t.takeBackStateDrain(at.state, s)
	}
}

// decode counts n values the apply read and refuses the apply once they pass
// the budget, where they are read and before a list is made for them.
func (t *applyTally) decode(n int) {
	if t == nil {
		return
	}
	t.decoded += n
	if over(t.budget.Decoded, t.decoded) {
		panic(applyBudgetExceeded{})
	}
}

// value counts v, one decoded JSON value, and every element and entry inside
// it, to the depth the decoders accept.
func (t *applyTally) value(v any) {
	if t == nil {
		return
	}
	t.decode(1)
	t.nested(v)
}

// nested counts what v holds and not v itself, for a value a declared length
// already counted.
func (t *applyTally) nested(v any) {
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			t.value(e)
		}
	case map[string]any:
		for _, e := range v {
			t.value(e)
		}
	}
}

// nestedAll counts what each of vals holds.
func (t *applyTally) nestedAll(vals []any) {
	if t == nil {
		return
	}
	for _, v := range vals {
		t.nested(v)
	}
}

// noteIntegrated checks item, a struct about to enter txn's document, before
// it does: it refuses the apply when item would hang under a root the list
// does not allow (checkItemRoot), or, parked, under one the list of the apply
// that sent it does not allow (checkParkedRoot), and when the update's own
// structs or bytes would pass the budget with it, and otherwise counts it. A
// struct the drain of the parked queue adds is checked against the lists but
// not counted: the update that parked it was charged for it (noteParked).
func noteIntegrated(txn *Transaction, item *Item) {
	d := txn.doc
	checkParkedRoot(d, item)
	t := d.tally()
	if t == nil {
		return
	}
	t.checkItemRoot(d, item.Parent)
	if !t.draining {
		t.count(item)
	}
}

// parkedFrom is what the document keeps of the apply that parked a struct:
// its origin, and its roots list when the struct's parent was not known yet.
// The document keeps it until nothing is parked, so a drain a whole-or-nothing
// document takes back finds it again.
type parkedFrom struct {
	origin any
	roots  []string
}

// noteParkedRoot checks item, a struct of the update about to park, against
// the apply's roots: one whose parent is known is refused when it hangs under
// a root the list does not allow. A document with a budget or a roots list
// keeps the apply's origin for the struct, and its list when the parent is
// not known yet, for the drain that integrates it (checkParkedRoot,
// dropOutsideRoots).
func noteParkedRoot(txn *Transaction, item *Item) {
	t := txn.doc.tally()
	if t == nil || isGCStruct(item) || txn.doc.applyBudget == nil && txn.doc.allowedRoots == nil {
		return
	}
	from := parkedFrom{origin: txn.Origin}
	if len(t.roots) > 0 {
		if item.Parent != nil {
			t.checkItemRoot(txn.doc, item.Parent)
		} else {
			from.roots = t.roots
		}
	}
	d := txn.doc
	if d.parkedBy == nil {
		d.parkedBy = make(map[*Item]parkedFrom)
	}
	d.parkedBy[item] = from
}

// checkParkedRoot refuses the apply integrating item, a parked struct whose
// parent was not known when it parked, when the root it now hangs under is
// not in the list of the apply that parked it, so a parked write stays inside
// its own sender's roots whoever fills its gap.
func checkParkedRoot(d *Doc, item *Item) {
	from, parked := d.parkedBy[item]
	if !parked || len(from.roots) == 0 || item.Parent == nil {
		return
	}
	root := item.Parent
	for root.item != nil && root.item.Parent != nil {
		root = root.item.Parent
	}
	name, isRoot := rootName(d, root)
	if isRoot && slices.Contains(from.roots, name) {
		return
	}
	if t := d.tally(); t != nil {
		t.root = name
	}
	panic(rootNotAllowed{})
}

// dropOutsideRoots reports whether item, a parked struct the drain of txn's
// apply is dropping because it lands under a root a list does not allow, was
// sent by another apply than txn's, which then does not fail for it: the
// document counts the drop for item's client instead
// (PendingStats.RootsDropped). An apply with the origin of the one that
// parked the struct fails, since it sent it.
func dropOutsideRoots(txn *Transaction, item *Item) bool {
	d := txn.doc
	from, parked := d.parkedBy[item]
	if parked && sameOrigin(from.origin, txn.Origin) {
		return false
	}
	if t := d.tally(); t != nil {
		t.rootDrops = append(t.rootDrops, item.ID.Client)
		t.root = ""
	}
	return true
}

// sameOrigin reports whether a and b are the same origin, and false for two
// origins Go cannot compare.
func sameOrigin(a, b any) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return a == b
}

// noteParked counts item, a struct of the update about to park behind a
// clock the document lacks, against the update's structs and bytes, and
// refuses the apply before it parks when it would pass the budget: the
// document holds a parked struct and integrates it once the gap is filled.
func noteParked(txn *Transaction, item *Item) {
	if t := txn.doc.tally(); t != nil && !t.draining {
		t.count(item)
	}
}

// count refuses the apply when item would take the update's structs or bytes
// past the budget, and otherwise counts it. Its bytes are its content's and
// the map key it is set under, which the document keeps while the entry lives.
func (t *applyTally) count(item *Item) {
	bytes := contentBytes(item.Content)
	if item.ParentSub != nil {
		bytes += len(*item.ParentSub)
	}
	if b := t.budget; over(b.Structs, t.structs+1) || over(b.Bytes, t.bytes+bytes) {
		panic(applyBudgetExceeded{})
	}
	t.structs++
	t.bytes += bytes
}

// noteDeleted refuses txn's apply before it deletes item when item hangs
// under a root the list does not allow. A delete adds nothing to count but
// the splits at its edges (countSplit).
func noteDeleted(txn *Transaction, item *Item) {
	if t := txn.doc.tally(); t != nil {
		t.checkItemRoot(txn.doc, item.Parent)
	}
}

// checkItemRoot refuses the apply when parent, the type an item the apply
// adds or deletes hangs under, is under a root the apply's list does not
// name, or under no root of the document. It walks up from parent to its
// root once per type: one walk records the root for every type it passed.
func (t *applyTally) checkItemRoot(d *Doc, parent *abstractType) {
	// A GC struct hangs under no type: it adds nothing to any root.
	if len(t.roots) == 0 || parent == nil {
		return
	}
	root, known := t.rootOf[parent]
	if !known {
		var chain []*abstractType
		root = parent
		for {
			if above, seen := t.rootOf[root]; seen {
				root = above
				break
			}
			chain = append(chain, root)
			if root.item == nil || root.item.Parent == nil {
				break
			}
			root = root.item.Parent
		}
		if t.rootOf == nil {
			t.rootOf = make(map[*abstractType]*abstractType, len(chain))
		}
		for _, passed := range chain {
			t.rootOf[passed] = root
		}
	}
	if t.allowedTypes[root] {
		return
	}
	name, isRoot := rootName(d, root)
	if root.item == nil && isRoot && slices.Contains(t.roots, name) {
		// A root the apply created by a name the list allows.
		t.allowedTypes[root] = true
		return
	}
	t.root = name
	panic(rootNotAllowed{})
}

// rootName is the name d holds root under, and false for a type that is no
// root of d; the empty name is a name like any other.
func rootName(d *Doc, root *abstractType) (string, bool) {
	if root.name != "" {
		return root.name, true
	}
	for name, shared := range d.share {
		if shared.baseType() == root {
			return name, true
		}
	}
	return "", false
}

// countSplit counts the struct a split of an item the document held adds,
// where txn's apply splits one (splitItem), and refuses the apply before the
// split when it would pass the budget, so a refused split leaves the item
// whole.
func countSplit(txn *Transaction) {
	t := txn.doc.tally()
	if t == nil {
		return
	}
	if over(t.budget.Split, t.split+1) {
		panic(applyBudgetExceeded{})
	}
	t.split++
}

// countDecodedRanges counts n, the delete ranges the apply read off the frame
// before the decode sorted and merged them, before one of them deletes or
// splits anything.
func countDecodedRanges(txn *Transaction, n int) {
	txn.doc.tally().decode(n)
}

// refuseRoot is ErrRootNotAllowed when a remote apply's decode is about to
// create name, a root doc does not hold, and the apply's list does not name
// it. A root doc holds is left to checkItemRoot, so an update that names a
// held root and writes nothing under it, as a peer handing back a document's
// whole state does, is not refused.
func refuseRoot(doc *Doc, name string) error {
	t := doc.tally()
	if t == nil || len(t.roots) == 0 || slices.Contains(t.roots, name) {
		return nil
	}
	if _, held := doc.share[name]; held {
		return nil
	}
	t.root = name
	return ErrRootNotAllowed
}

// noteRootName charges name, a root a remote apply's decode is about to
// create, against the apply's bytes, since the document keeps the name for as
// long as it holds the root, and refuses the apply before the root is created
// when it would pass the budget. A root the document holds costs nothing.
func noteRootName(doc *Doc, name string) {
	t := doc.tally()
	if t == nil {
		return
	}
	if _, held := doc.share[name]; held {
		return
	}
	if over(t.budget.Bytes, t.bytes+len(name)) {
		panic(applyBudgetExceeded{})
	}
	t.bytes += len(name)
}

// anyScalarBytes is what a decoded scalar, or the slot of an element or an
// entry, counts as: the two words of an interface value.
const anyScalarBytes = 16

// contentBytes is the bytes a struct's content holds: its text, its binary,
// its values, and the name an XML element or hook carries. A deleted range
// and a type's own structure hold none.
func contentBytes(c Content) int {
	switch c := c.(type) {
	case *ContentString:
		return len(c.Str)
	case *ContentBinary:
		return len(c.Data)
	case *ContentAny:
		return valsBytes(c.Vals)
	case *ContentJSON:
		return valsBytes(c.Vals)
	case *ContentEmbed:
		return anyBytes(c.Val)
	case *ContentFormat:
		return len(c.Key) + anyBytes(c.Val)
	case *ContentType:
		return typeNameBytes(c.Type)
	}
	return 0
}

// typeNameBytes is the length of the node name of an XML element or the name
// of an XML hook; other types carry no name.
func typeNameBytes(t *abstractType) int {
	if t == nil {
		return 0
	}
	switch owner := t.owner.(type) {
	case *YXmlElement:
		return len(owner.NodeName)
	case *yxmlHook:
		return len(owner.hookName)
	}
	return 0
}

// valsBytes is the bytes the values of one content hold.
func valsBytes(vals []any) int {
	n := 0
	for _, v := range vals {
		n += anyBytes(v)
	}
	return n
}

// anyBytes is the bytes a decoded value holds: text and binary as they are, a
// scalar as the words it takes, and a container as its parts with a slot
// for each.
func anyBytes(v any) int {
	switch v := v.(type) {
	case string:
		return len(v)
	case []byte:
		return len(v)
	case []any:
		n := 0
		for _, e := range v {
			n += anyScalarBytes + anyBytes(e)
		}
		return n
	case map[string]any:
		n := 0
		for k, e := range v {
			n += anyScalarBytes + len(k) + anyBytes(e)
		}
		return n
	}
	return anyScalarBytes
}

// countAnyRun counts, against t, the n lib0 Any values dec is about to read
// and everything inside them, off the frame's own bytes on a copy of the
// cursor, so a declared length the budget cannot hold is refused before
// anything is allocated for it. It allocates nothing, and stops without an
// error where the decode that follows will fail.
func countAnyRun(t *applyTally, dec *encoding.Decoder, n uint64) {
	if t == nil {
		return
	}
	scan := *dec
	for range n {
		if !countAnyValue(t, &scan, 0) {
			return
		}
	}
}

// countAnyValue counts one lib0 Any value at depth and what it holds, and
// reports whether it read the whole value.
func countAnyValue(t *applyTally, dec *encoding.Decoder, depth int) bool {
	if depth > anycodec.MaxDepth {
		return false
	}
	t.decode(1)
	tag, err := dec.ReadUint8()
	if err != nil {
		return false
	}
	switch tag {
	case 127, 126, 120, 121: // undefined, null, true, false
		return true
	case 125:
		_, err = dec.ReadVarInt()
	case 124:
		_, err = dec.ReadFloat32()
	case 123:
		_, err = dec.ReadFloat64()
	case 122:
		_, err = dec.ReadBigInt64()
	case 119, 116: // string, binary: read as the bytes they alias
		_, err = dec.ReadVarBytes()
	case 117, 118: // array, map
		elements, err := dec.ReadVarUint()
		if err != nil || elements > anycodec.MaxElements || elements > uint64(dec.Remaining()) {
			return false
		}
		for range elements {
			if tag == 118 {
				if _, err := dec.ReadVarBytes(); err != nil {
					return false
				}
			}
			if !countAnyValue(t, dec, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
	return err == nil
}
