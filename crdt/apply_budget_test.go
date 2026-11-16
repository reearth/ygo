package crdt

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// baseState is the whole state of a document whose root "t" holds base,
// written by client 1 in one transaction, so every document and peer of these
// tests starts from the same structs.
func baseState(t *testing.T, base string) []byte {
	t.Helper()
	writer := New(WithClientID(1))
	text := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 0, base, nil) })
	return EncodeStateAsUpdateV1(writer, nil)
}

// budgetDoc is a document holding base as the text of root "t", with
// whole-or-nothing applies, so a refused update leaves it as it was, and with
// no bound of any kind: a test sets one on it after it has loaded the state it
// trusts, as an owner that bounds only what arrives after that does.
func budgetDoc(t *testing.T, base string) *Doc {
	t.Helper()
	doc := New(WithClientID(7), WithWholeOrNothingApplies(true))
	if err := ApplyUpdateV1(doc, baseState(t, base), nil); err != nil {
		t.Fatalf("the base state was refused: %v", err)
	}
	return doc
}

// storeStructs is how many structs doc holds, every client's together.
func storeStructs(doc *Doc) int {
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	n := 0
	for _, items := range doc.store.clients {
		n += len(items)
	}
	return n
}

// editUpdate is a V1 update of what edit writes on a peer holding base, so a
// document holding base takes it as a remote update of that edit alone.
func editUpdate(t *testing.T, base string, edit func(txn *Transaction, text *YText)) []byte {
	t.Helper()
	return editUpdateFrom(t, base, 2, edit)
}

// editUpdateFrom is editUpdate from the peer client, for a test that applies
// two such updates to one document: a second update from the same client would
// carry clocks the document already holds, which integrate nothing.
func editUpdateFrom(t *testing.T, base string, client ClientID, edit func(txn *Transaction, text *YText)) []byte {
	t.Helper()
	peer := New(WithClientID(client))
	if err := ApplyUpdateV1(peer, baseState(t, base), nil); err != nil {
		t.Fatalf("the peer refused the base state: %v", err)
	}
	sv := peer.StateVector()
	text := peer.GetText("t")
	peer.Transact(func(txn *Transaction) { edit(txn, text) })
	return EncodeStateAsUpdateV1(peer, sv)
}

// mapSetUpdate is an update of one struct that names root "m" on the wire: a
// key a peer set on a map root with nothing in it, which the decode resolves
// by name because the struct has no neighbour to inherit its parent from, so
// it is refused by the name check in the decode (refuseRoot) rather than by
// the containment check over what the apply adds (checkItemRoot).
func mapSetUpdate(t *testing.T, base string, client ClientID) []byte {
	t.Helper()
	peer := New(WithClientID(client))
	if err := ApplyUpdateV1(peer, baseState(t, base), nil); err != nil {
		t.Fatalf("the peer refused the base state: %v", err)
	}
	sv := peer.StateVector()
	m := peer.GetMap("m")
	peer.Transact(func(txn *Transaction) { m.Set(txn, "k", "v") })
	return EncodeStateAsUpdateV1(peer, sv)
}

// threeInserts is an update of three one-character items, written at places
// far enough apart in base that none of them merges with another.
func threeInserts(t *testing.T, base string) []byte {
	t.Helper()
	return editUpdate(t, base, func(txn *Transaction, text *YText) {
		text.Insert(txn, 0, "a", nil)
		text.Insert(txn, 4, "b", nil)
		text.Insert(txn, 8, "c", nil)
	})
}

// appliedText is the text a document holding base holds once update is applied
// to it with no bound at all: what a document that takes the same update under
// a budget must hold too.
func appliedText(t *testing.T, base string, update []byte) string {
	t.Helper()
	twin := budgetDoc(t, base)
	if err := ApplyUpdateV1(twin, update, nil); err != nil {
		t.Fatalf("the unbounded twin refused the update: %v", err)
	}
	return twin.GetText("t").ToString()
}

// docState is everything a refused apply must leave as it was: the state
// vector, the delete set, the pending queue (its parked structs and its
// parked delete ranges), the rendered text, the entry of the map root the
// allowed-roots tests guard, and the struct count.
type docState struct {
	sv            string
	deletes       string
	parked        int
	parkedDeletes int
	text          string
	entry         string
	structs       int
}

// stateOf reads doc's state. It is called outside any transaction, where
// nothing is writing the store. Reading the roots "t" and "m" creates them on
// a document that does not hold them, which adds no struct and leaves every
// field below as it was.
func stateOf(doc *Doc) docState {
	parked := 0
	if p := doc.store.pending; p != nil {
		for _, item := range p.items {
			if item != nil {
				parked++
			}
		}
	}
	deletes := buildDeleteSet(doc.store)
	entry, _ := doc.GetMap("m").Get("k")
	return docState{
		sv:            fmt.Sprint(doc.StateVector()),
		deletes:       fmt.Sprint(deletes.orderedClients()),
		parked:        parked,
		parkedDeletes: doc.store.pendingDsRanges,
		text:          doc.GetText("t").ToString(),
		entry:         fmt.Sprint(entry),
		structs:       storeStructs(doc),
	}
}

// requireRefused requires err to be the refusal a document with a budget or a
// roots list returns for sentinel, and doc, which has whole-or-nothing
// applies, to read as before had it: the state vector, the delete set, the
// pending queue, the text and the struct count are all as they were.
func requireRefused(t *testing.T, doc *Doc, before docState, err error, sentinel error) *RefusedUpdateError {
	t.Helper()
	if !errors.Is(err, sentinel) || !errors.Is(err, ErrInvalidUpdate) {
		t.Fatalf("the update was applied with err %v, want %v", err, sentinel)
	}
	var refused *RefusedUpdateError
	if !errors.As(err, &refused) {
		t.Fatalf("the error is not a *RefusedUpdateError: %v", err)
	}
	if after := stateOf(doc); after != before {
		t.Fatalf("the refused update changed the document:\nbefore %+v\nafter  %+v", before, after)
	}
	return refused
}

// requireApplied requires update to have been applied and doc to have changed,
// so that a control proves the budget and not the update refused the others.
func requireApplied(t *testing.T, doc *Doc, before docState, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("the update was refused: %v", err)
	}
	if after := stateOf(doc); after == before {
		t.Fatal("the applied update changed nothing, so it is no control")
	}
}

// A document refuses a remote update that integrates more structs of its own
// than the apply's budget allows, and applies the same update when its budget
// holds it: the refusal is ErrApplyBudget inside a RefusedUpdateError
// reporting the structs the apply integrated before the one that passed the
// budget and the splits it made, it tells no observer, and on a document with
// whole-or-nothing applies it leaves the state vector, the delete set, the
// pending queue and the rendered text as they were.
func TestUnit_ApplyBudget_RefusesAnUpdateThatIntegratesMoreStructsThanItAllows(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Structs: 2} })
	before := stateOf(doc)
	updates := observed(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrApplyBudget)
	if refused.Structs != 2 {
		t.Fatalf("the refusal reports %d structs, want the 2 the budget took before the third", refused.Structs)
	}
	if refused.Split == 0 {
		t.Fatal("the refusal reports no split, want the splits three inserts into one run make")
	}
	if *updates != 0 {
		t.Fatalf("the refused update was handed to %d observers", *updates)
	}

	fits := budgetDoc(t, base)
	fits.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Structs: 3} })
	fitsBefore := stateOf(fits)
	requireApplied(t, fits, fitsBefore, ApplyUpdateV1(fits, update, nil))
	if got, want := fits.GetText("t").ToString(), appliedText(t, base, update); got != want {
		t.Fatalf("the applied update left %q, want %q", got, want)
	}
	if storeStructs(fits) <= fitsBefore.structs {
		t.Fatalf("the applied update left %d structs, want more than the %d before it", storeStructs(fits), fitsBefore.structs)
	}
}

// A document refuses a remote update that decodes more values than the apply's
// budget allows, from inside the decode, before any of its structs integrate;
// a document whose Decoded holds the update applies it.
func TestUnit_ApplyBudget_RefusesAnUpdateThatDecodesMoreValuesThanItAllows(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 2} })
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrApplyBudget)
	if refused.Decoded != 3 {
		t.Fatalf("the refusal reports %d decoded values, want the 3 it reached", refused.Decoded)
	}
	if refused.Structs != 0 {
		t.Fatalf("the refusal reports %d structs, want none: it was refused as it decoded", refused.Structs)
	}

	fits := budgetDoc(t, base)
	fits.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 3} })
	requireApplied(t, fits, stateOf(fits), ApplyUpdateV1(fits, update, nil))

	// The delete ranges an update carries are values it decodes too, since
	// each of them can split a struct the document holds, and a split adds a
	// struct no budget bounds (countDecodedRanges). Two deletes apart leave
	// two ranges.
	twoRanges := editUpdate(t, base, func(txn *Transaction, text *YText) {
		text.Delete(txn, 6, 1)
		text.Delete(txn, 3, 1)
	})
	ranges := budgetDoc(t, base)
	ranges.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 1} })
	rangesBefore := stateOf(ranges)
	refusedRanges := requireRefused(t, ranges, rangesBefore, ApplyUpdateV1(ranges, twoRanges, nil), ErrApplyBudget)
	if refusedRanges.Decoded != 2 {
		t.Fatalf("the refusal reports %d decoded values, want the two delete ranges it carries", refusedRanges.Decoded)
	}
	fitsRanges := budgetDoc(t, base)
	fitsRanges.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 2} })
	requireApplied(t, fitsRanges, stateOf(fitsRanges), ApplyUpdateV1(fitsRanges, twoRanges, nil))
}

// A document refuses a remote update that integrates more bytes of content
// than the apply's budget allows - the creep no count of structs can see,
// since one struct holds a whole paste - and takes the same update under a
// budget of exactly its bytes.
func TestUnit_ApplyBudget_RefusesAnUpdateThatIntegratesMoreBytesThanItAllows(t *testing.T) {
	const base, paste = "hello world", "pasted text"
	update := editUpdate(t, base, func(txn *Transaction, text *YText) {
		text.Insert(txn, 5, paste, nil)
	})

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget {
		return ApplyBudget{Bytes: len(paste) - 1}
	})
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrApplyBudget)
	if refused.Bytes != 0 || refused.Structs != 0 {
		t.Fatalf("the refusal reports %d structs and %d bytes, want none: the paste is the struct that passed",
			refused.Structs, refused.Bytes)
	}

	fits := budgetDoc(t, base)
	fits.SetApplyBudget(func(any) ApplyBudget {
		return ApplyBudget{Bytes: len(paste)}
	})
	requireApplied(t, fits, stateOf(fits), ApplyUpdateV1(fits, update, nil))
}

// Structs counts the structs of the update and not the ones a delete makes by
// splitting a run the document holds. Under Structs: 1, the smallest limit,
// an update that only deletes inside the run of "hello world" is applied
// though its splits add structs, an update of one character is applied, and
// an update of two characters at two places is refused at the second, its
// RefusedUpdateError reporting the one struct before it.
func TestUnit_ApplyBudget_StructsCountsTheUpdatesStructsAndNotADeletesSplits(t *testing.T) {
	const base = "hello world"
	one := func(any) ApplyBudget { return ApplyBudget{Structs: 1} }
	deleteOnly := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Delete(txn, 3, 5) })
	oneCharacter := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Insert(txn, 3, "x", nil) })
	twoCharacters := editUpdate(t, base, func(txn *Transaction, text *YText) {
		text.Insert(txn, 8, "y", nil)
		text.Insert(txn, 3, "x", nil)
	})

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(one)
	before := stateOf(doc)
	requireApplied(t, doc, before, ApplyUpdateV1(doc, deleteOnly, nil))
	if got, want := doc.GetText("t").ToString(), appliedText(t, base, deleteOnly); got != want {
		t.Fatalf("the delete left %q, want %q", got, want)
	}
	if storeStructs(doc) <= before.structs {
		t.Fatalf("the delete left %d structs, want more than the %d before it: its splits add structs",
			storeStructs(doc), before.structs)
	}

	single := budgetDoc(t, base)
	single.SetApplyBudget(one)
	requireApplied(t, single, stateOf(single), ApplyUpdateV1(single, oneCharacter, nil))

	two := budgetDoc(t, base)
	two.SetApplyBudget(one)
	twoBefore := stateOf(two)
	refused := requireRefused(t, two, twoBefore, ApplyUpdateV1(two, twoCharacters, nil), ErrApplyBudget)
	if refused.Structs != 1 {
		t.Fatalf("the refusal reports %d structs, want the one before the struct that passed", refused.Structs)
	}
}

// Bytes counts content and not structs: under Bytes: 1, the smallest limit,
// an update that only deletes held text is applied, one that integrates a
// single character is applied, and one that integrates two characters in one
// struct is refused, its RefusedUpdateError reporting no byte.
func TestUnit_ApplyBudget_BytesCountsContentAndNotADelete(t *testing.T) {
	const base = "hello world"
	one := func(any) ApplyBudget { return ApplyBudget{Bytes: 1} }
	deleteOnly := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Delete(txn, 3, 5) })
	oneCharacter := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Insert(txn, 3, "x", nil) })
	twoCharacters := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Insert(txn, 3, "xy", nil) })

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(one)
	requireApplied(t, doc, stateOf(doc), ApplyUpdateV1(doc, deleteOnly, nil))

	single := budgetDoc(t, base)
	single.SetApplyBudget(one)
	requireApplied(t, single, stateOf(single), ApplyUpdateV1(single, oneCharacter, nil))

	insert := budgetDoc(t, base)
	insert.SetApplyBudget(one)
	before := stateOf(insert)
	refused := requireRefused(t, insert, before, ApplyUpdateV1(insert, twoCharacters, nil), ErrApplyBudget)
	if refused.Bytes != 0 || refused.Structs != 0 {
		t.Fatalf("the refusal reports %d bytes and %d structs, want none: the two characters were the struct that passed",
			refused.Bytes, refused.Structs)
	}
}

// A field of zero or below is no limit, as WithIntegrateScanBudget reads its
// bound. For each field, 1, the smallest limit, refuses an update of three
// inserts that decodes, adds and splits past it, which shows the field is
// read; 0 and -1 apply the same update and leave the document as one with no
// budget leaves it. So do the zero value and a budget of -1 in every field.
func TestUnit_ApplyBudget_ZeroAndNegativeFieldsLimitNothing(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)
	open := budgetDoc(t, base)
	requireApplied(t, open, stateOf(open), ApplyUpdateV1(open, update, nil))

	run := func(t *testing.T, budget ApplyBudget, limited bool) {
		doc := budgetDoc(t, base)
		doc.SetApplyBudget(func(any) ApplyBudget { return budget })
		before := stateOf(doc)
		err := ApplyUpdateV1(doc, update, nil)
		if limited {
			requireRefused(t, doc, before, err, ErrApplyBudget)
			return
		}
		requireApplied(t, doc, before, err)
		if got, want := stateOf(doc), stateOf(open); got != want {
			t.Fatalf("the document is\n%+v\nwant it as with no budget\n%+v", got, want)
		}
	}
	for _, field := range []struct {
		name string
		set  func(b *ApplyBudget, n int)
	}{
		{"Decoded", func(b *ApplyBudget, n int) { b.Decoded = n }},
		{"Structs", func(b *ApplyBudget, n int) { b.Structs = n }},
		{"Bytes", func(b *ApplyBudget, n int) { b.Bytes = n }},
		{"Split", func(b *ApplyBudget, n int) { b.Split = n }},
	} {
		for _, n := range []int{1, 0, -1} {
			var budget ApplyBudget
			field.set(&budget, n)
			t.Run(fmt.Sprintf("%s: %d", field.name, n), func(t *testing.T) { run(t, budget, n > 0) })
		}
	}
	t.Run("the zero value", func(t *testing.T) { run(t, ApplyBudget{}, false) })
	t.Run("-1 in every field", func(t *testing.T) {
		run(t, ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1, Split: -1}, false)
	})
}

// A document with an allowed-roots list refuses a remote update naming a root
// outside it, reporting the name, and takes the same update when the list
// names it. A root the document already holds is subject to the list too: the
// list is what the owner allows the update to name, not what the document
// holds. A nil callback, and one answering nothing, allow every root.
func TestUnit_AllowedRoots_RefusesAnUpdateNamingARootTheDocumentDoesNotAllow(t *testing.T) {
	const base = "hello world"
	// A peer that writes into a second root, which the document does not hold.
	peer := New(WithClientID(2))
	if err := ApplyUpdateV1(peer, baseState(t, base), nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	other := peer.GetText("other")
	peer.Transact(func(txn *Transaction) { other.Insert(txn, 0, "elsewhere", nil) })
	update := EncodeStateAsUpdateV1(peer, sv)

	doc := budgetDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrRootNotAllowed)
	if refused.Root != "other" {
		t.Fatalf("the refusal reports root %q, want \"other\"", refused.Root)
	}
	if _, held := doc.share["other"]; held {
		t.Fatal("the refused update left the root it named in the document")
	}

	allowed := budgetDoc(t, base)
	allowed.SetAllowedRoots(func(any) []string { return []string{"t", "other"} })
	requireApplied(t, allowed, stateOf(allowed), ApplyUpdateV1(allowed, update, nil))
	if got, want := allowed.GetText("other").ToString(), "elsewhere"; got != want {
		t.Fatalf("the allowed root holds %q, want %q", got, want)
	}

	// A root the document holds already is refused when the list omits it;
	// an update that stays inside a root the list names is applied, whether
	// or not it names that root on the wire (checkItemRoot).
	held := budgetDoc(t, base)
	held.GetMap("m") // the document holds the root the update names
	held.SetAllowedRoots(func(any) []string { return []string{"t"} })
	heldBefore := stateOf(held)
	refusedHeld := requireRefused(t, held, heldBefore, ApplyUpdateV1(held, mapSetUpdate(t, base, 3), nil), ErrRootNotAllowed)
	if refusedHeld.Root != "m" {
		t.Fatalf("the refusal reports root %q, want \"m\"", refusedHeld.Root)
	}
	if err := ApplyUpdateV1(held, threeInserts(t, base), nil); err != nil {
		t.Fatalf("an update inside the allowed root was refused: %v", err)
	}

	for name, roots := range map[string]func(any) []string{
		"a nil callback": nil,
		"a nil answer":   func(any) []string { return nil },
		"an empty answer": func(any) []string {
			return []string{}
		},
	} {
		open := budgetDoc(t, base)
		open.SetAllowedRoots(roots)
		if err := ApplyUpdateV1(open, update, nil); err != nil {
			t.Fatalf("%s refused the update: %v", name, err)
		}
	}
}

// Without whole-or-nothing applies, a refusal keeps what the apply integrated
// before the struct that passed the budget, as the scan bound does: of three
// one-character inserts under Structs: 1, the first stays, the error reports
// it, and ApplyUpdateV1Landed returns it for a peer to apply.
func TestUnit_ApplyBudget_ARefusalKeepsWhatTheApplyIntegrated(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)
	keeps := func() *Doc {
		doc := New(WithClientID(7))
		if err := ApplyUpdateV1(doc, baseState(t, base), nil); err != nil {
			t.Fatal(err)
		}
		doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Structs: 1} })
		return doc
	}

	doc := keeps()
	err := ApplyUpdateV1(doc, update, nil)
	var refused *RefusedUpdateError
	if !errors.Is(err, ErrApplyBudget) || !errors.As(err, &refused) {
		t.Fatalf("the update was applied with err %v, want ErrApplyBudget in a RefusedUpdateError", err)
	}
	if refused.Structs != 1 {
		t.Fatalf("the refusal reports %d structs, want the one it kept", refused.Structs)
	}
	if got, want := doc.GetText("t").ToString(), "a"+base; got != want {
		t.Fatalf("the refused update left %q, want %q: its first insert, kept", got, want)
	}

	landedDoc := keeps()
	landed, err := ApplyUpdateV1Landed(landedDoc, update, nil)
	if !errors.Is(err, ErrApplyBudget) || landed == nil {
		t.Fatalf("ApplyUpdateV1Landed returned %d bytes and %v, want the kept insert and ErrApplyBudget", len(landed), err)
	}
	peer := New(WithClientID(8))
	if err := ApplyUpdateV1(peer, baseState(t, base), nil); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdateV1(peer, landed, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := peer.GetText("t").ToString(), "a"+base; got != want {
		t.Fatalf("the landed part applied to a peer leaves %q, want %q", got, want)
	}
}

// An apply budget that limits nothing, and an allowed-roots list that names
// every root an update writes, change nothing an apply does: an update that
// sets a plain key and a subdocument applies whole, as on a document with
// neither option.
func TestUnit_ApplyBudget_AnOptionThatLimitsNothingRefusesNothing(t *testing.T) {
	peer := New(WithClientID(2))
	m := peer.GetMap("m")
	peer.Transact(func(txn *Transaction) {
		m.Set(txn, "k", "v")
		m.Set(txn, "sub", New(WithGUID("sub")))
	})
	update := EncodeStateAsUpdateV1(peer, nil)

	zero := func(any) ApplyBudget { return ApplyBudget{} }
	negative := func(any) ApplyBudget { return ApplyBudget{Decoded: -1, Structs: -1, Bytes: -1, Split: -1} }
	everyRoot := func(any) []string { return []string{"m"} }
	for name, opts := range map[string][]DocOption{
		"no option":                  nil,
		"an apply budget of zero":    {WithApplyBudget(zero)},
		"an apply budget of -1":      {WithApplyBudget(negative)},
		"allowed roots, every root":  {WithAllowedRoots(everyRoot)},
		"both, limiting nothing":     {WithApplyBudget(zero), WithAllowedRoots(everyRoot)},
		"a nil budget and roots set": {WithApplyBudget(nil), WithAllowedRoots(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			doc := New(opts...)
			if err := ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatalf("the update was refused: %v", err)
			}
			got := doc.GetMap("m")
			if v, _ := got.Get("k"); v != "v" {
				t.Fatalf("the key holds %v, want \"v\"", v)
			}
			if sub, _ := got.Get("sub"); sub == nil {
				t.Fatal("the subdocument is missing")
			} else if _, isDoc := sub.(*Doc); !isDoc {
				t.Fatalf("the subdocument's key holds %T, want *Doc", sub)
			}
		})
	}
}

// The V2 decoder is bounded exactly as the V1 decoder is: each of the three
// counts, and the allowed-roots list, refuses the V2 form of the same update,
// and a budget that holds it applies it.
func TestUnit_ApplyBudget_BoundsTheV2DecoderToo(t *testing.T) {
	const base = "hello world"
	v1 := threeInserts(t, base)
	v2, err := UpdateV1ToV2(v1)
	if err != nil {
		t.Fatal(err)
	}

	for name, budget := range map[string]ApplyBudget{
		"decoded": {Decoded: 2},
		"structs": {Structs: 2},
		"bytes":   {Bytes: 2},
	} {
		t.Run(name, func(t *testing.T) {
			doc := budgetDoc(t, base)
			doc.SetApplyBudget(func(any) ApplyBudget { return budget })
			before := stateOf(doc)
			requireRefused(t, doc, before, ApplyUpdateV2(doc, v2, nil), ErrApplyBudget)
		})
	}

	namesARoot, err := UpdateV1ToV2(mapSetUpdate(t, base, 3))
	if err != nil {
		t.Fatal(err)
	}
	roots := budgetDoc(t, base)
	roots.SetAllowedRoots(func(any) []string { return []string{"t"} })
	rootsBefore := stateOf(roots)
	refused := requireRefused(t, roots, rootsBefore, ApplyUpdateV2(roots, namesARoot, nil), ErrRootNotAllowed)
	if refused.Root != "m" {
		t.Fatalf("the V2 refusal reports root %q, want \"m\"", refused.Root)
	}

	fits := budgetDoc(t, base)
	fits.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 3, Structs: 3, Bytes: 3} })
	fitsBefore := stateOf(fits)
	requireApplied(t, fits, fitsBefore, ApplyUpdateV2(fits, v2, nil))
	if got, want := fits.GetText("t").ToString(), appliedText(t, base, v1); got != want {
		t.Fatalf("the applied V2 update left %q, want %q", got, want)
	}
}

// A budget or allowed-roots callback that panics refuses the update and leaves
// the document as it was, as a panicking scan budget does: the panic is
// reported as an invalid update naming the callback, and the document goes on
// taking updates once the callback is removed.
func TestUnit_ApplyBudget_APanickingCallbackRefusesTheUpdateAndChangesNothing(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget { panic("boom in the apply budget") })
	before := stateOf(doc)
	err := ApplyUpdateV1(doc, update, nil)
	requireRefused(t, doc, before, err, ErrInvalidUpdate)
	if msg := err.Error(); !strings.Contains(msg, "WithApplyBudget callback") || !strings.Contains(msg, "boom in the apply budget") {
		t.Fatalf("the error does not name the callback and its panic: %v", err)
	}

	doc.SetApplyBudget(nil)
	doc.SetAllowedRoots(func(any) []string { panic("boom in the roots") })
	err = ApplyUpdateV1(doc, update, nil)
	requireRefused(t, doc, before, err, ErrInvalidUpdate)
	if msg := err.Error(); !strings.Contains(msg, "WithAllowedRoots callback") || !strings.Contains(msg, "boom in the roots") {
		t.Fatalf("the error does not name the callback and its panic: %v", err)
	}

	doc.SetAllowedRoots(nil)
	requireApplied(t, doc, before, ApplyUpdateV1(doc, update, nil))
}

// denseAnyValues is an update of structs items for client 9, each a lib0 Any
// content of vals null values parented to root "dense": one byte of frame for
// each value, and an interface word and a slot of a []any for each once
// decoded. No item names an origin, so each integrates at the head of the root
// and its conflict scan stops at the first item it meets (same client, same
// origins).
func denseAnyValues(structs, vals int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(structs))
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	for range structs {
		enc.WriteUint8(wireAny)
		enc.WriteUint8(1) // parent info: a named root follows
		enc.WriteVarString("dense")
		enc.WriteVarUint(uint64(vals))
		for range vals {
			enc.WriteUint8(126) // lib0 Any: null
		}
	}
	enc.WriteVarUint(0) // delete set: no clients
	return append([]byte(nil), enc.Bytes()...)
}

// A frame dense in lib0 Any values is refused by Decoded as it decodes, before
// the values it carries are in memory: the refused apply allocates a small
// fraction of what the same frame allocates on a document with no budget.
// Measured on this frame (2,000 structs of 50 null values each, 118,006
// bytes) the refused apply allocated 21,320 bytes against the unbudgeted
// apply's 3,933,056: a ratio of about 184 to 1, and 0.1 ms against 6 ms. The
// test requires a factor of 20, which a check made after the decode could not
// reach - it would have allocated every value of the frame first.
func TestUnit_ApplyBudget_ADenseFrameOfAnyValuesIsRefusedBeforeItAllocates(t *testing.T) {
	const base = "hello world"
	const structs, vals = 2_000, 50
	update := denseAnyValues(structs, vals)

	open := budgetDoc(t, base)
	openAllocated, openTook, err := applyMeasured(open, update)
	if err != nil {
		t.Fatalf("the document with no budget refused the dense frame: %v", err)
	}
	if got := storeStructs(open); got != 1+structs {
		t.Fatalf("the unbudgeted apply left %d structs, want %d", got, 1+structs)
	}

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 100} })
	before := stateOf(doc)
	allocated, took, err := applyMeasured(doc, update)
	t.Logf("a dense frame of %d bytes: the refused apply allocated %d bytes in %s, the unbudgeted one %d bytes in %s",
		len(update), allocated, took, openAllocated, openTook)
	requireRefused(t, doc, before, err, ErrApplyBudget)
	if allocated*20 > openAllocated {
		t.Fatalf("the refused dense frame allocated %d bytes, want far less than the %d the same frame allocates unbudgeted",
			allocated, openAllocated)
	}
}

// A document with whole-or-nothing applies whose apply passes its budget, or
// writes outside its roots, keeps nothing of the apply, though the struct
// that passed the budget came after two that integrated. Both documents go on
// taking what a peer sends once the bound is lifted.
func TestUnit_ApplyBudget_AWholeOrNothingRefusalKeepsNothing(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)

	budget := budgetDoc(t, base)
	budget.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Structs: 2} })
	before := stateOf(budget)
	refused := requireRefused(t, budget, before, ApplyUpdateV1(budget, update, nil), ErrApplyBudget)
	if refused.Structs != 2 {
		t.Fatalf("the refusal reports %d structs, want the 2 it reached before the third", refused.Structs)
	}

	roots := budgetDoc(t, base)
	roots.SetAllowedRoots(func(any) []string { return []string{"nothing"} })
	rootsBefore := stateOf(roots)
	requireRefused(t, roots, rootsBefore, ApplyUpdateV1(roots, mapSetUpdate(t, base, 3), nil), ErrRootNotAllowed)

	want := appliedText(t, base, update)
	for _, doc := range []*Doc{budget, roots} {
		doc.SetApplyBudget(nil)
		doc.SetAllowedRoots(nil)
		if err := ApplyUpdateV1(doc, update, nil); err != nil {
			t.Fatalf("the document refused the update with no bound set: %v", err)
		}
		if got := doc.GetText("t").ToString(); got != want {
			t.Fatalf("the document holds %q, want %q", got, want)
		}
	}
}

// A document with an apply budget reports its counts whatever fails an
// apply: an update the scan bound stops reports the values it had decoded and
// the structs it had integrated by then.
func TestUnit_ApplyBudget_AScanRefusalReportsWhatTheApplyDecoded(t *testing.T) {
	const l, n = 2_000, 20
	doc := New(
		WithIntegrateScanBudget(func(origin any) int {
			if origin == fromAClient {
				return 1
			}
			return 0
		}),
		WithApplyBudget(func(any) ApplyBudget { return ApplyBudget{} }),
	)
	if err := ApplyUpdateV1(doc, EncodeStateAsUpdateV1(chainedList(t, l), nil), nil); err != nil {
		t.Fatal(err)
	}
	before := stateOf(doc)
	update := originAtTheStart(n)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, fromAClient), ErrIntegrateScanBudget)
	if refused.Decoded != n {
		t.Fatalf("the refusal reports %d decoded values, want the %d structs the update carries", refused.Decoded, n)
	}
	if refused.Structs != 0 {
		t.Fatalf("the refusal reports %d structs, want none: the first item's scan passed the limit", refused.Structs)
	}

	requireApplied(t, doc, before, ApplyUpdateV1(doc, update, nil))
}

// guardedState is the whole state of a document with two roots written by
// client 1: the text "t" holding base, and the map "m" holding one entry. An
// update that writes beside either names no root on the wire - it inherits
// its parent from the item its origin names, and a delete range names nothing
// at all - so what keeps it out of a root is the containment check over what
// the apply adds and deletes (checkItemRoot), not the name check in the
// decode.
func guardedState(t *testing.T, base string) []byte {
	t.Helper()
	writer := New(WithClientID(1))
	text, m := writer.GetText("t"), writer.GetMap("m")
	writer.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, base, nil)
		m.Set(txn, "k", "old")
	})
	return EncodeStateAsUpdateV1(writer, nil)
}

// guardedDoc is a document holding guardedState, with no bound.
func guardedDoc(t *testing.T, base string) *Doc {
	t.Helper()
	doc := New(WithClientID(7), WithWholeOrNothingApplies(true))
	if err := ApplyUpdateV1(doc, guardedState(t, base), nil); err != nil {
		t.Fatalf("the guarded state was refused: %v", err)
	}
	return doc
}

// guardedEdit is a V1 update of what edit writes on a peer holding
// guardedState.
func guardedEdit(t *testing.T, base string, client ClientID, edit func(txn *Transaction, text *YText, m *YMap)) []byte {
	t.Helper()
	peer := New(WithClientID(client))
	if err := ApplyUpdateV1(peer, guardedState(t, base), nil); err != nil {
		t.Fatalf("the peer refused the guarded state: %v", err)
	}
	sv := peer.StateVector()
	text, m := peer.GetText("t"), peer.GetMap("m")
	peer.Transact(func(txn *Transaction) { edit(txn, text, m) })
	return EncodeStateAsUpdateV1(peer, sv)
}

// An allowed-roots list is a containment rule over the whole update, not a
// rule about the names the update writes: an update that overwrites a key of
// a root outside the list, deletes an item under it, or writes beside text it
// holds, is refused and rolled back whole, though none of those updates names
// the root on the wire. Each case is controlled by the same update applied
// with no list, which changes the document.
func TestUnit_AllowedRoots_RefusesWhatAnUpdateAddsOrDeletesUnderADisallowedRoot(t *testing.T) {
	const base = "hello world"
	for name, c := range map[string]struct {
		allowed []string
		edit    func(txn *Transaction, text *YText, m *YMap)
		root    string
	}{
		"an overwrite of a key of the map": {
			allowed: []string{"t"},
			edit:    func(txn *Transaction, _ *YText, m *YMap) { m.Set(txn, "k", "new") },
			root:    "m",
		},
		"a delete of the key the map holds": {
			allowed: []string{"t"},
			edit:    func(txn *Transaction, _ *YText, m *YMap) { m.Delete(txn, "k") },
			root:    "m",
		},
		"text written beside the text it holds": {
			allowed: []string{"m"},
			edit:    func(txn *Transaction, text *YText, _ *YMap) { text.Insert(txn, 3, "xyz", nil) },
			root:    "t",
		},
		"a delete of a range of the text it holds": {
			allowed: []string{"m"},
			edit:    func(txn *Transaction, text *YText, _ *YMap) { text.Delete(txn, 2, 4) },
			root:    "t",
		},
	} {
		t.Run(name, func(t *testing.T) {
			update := guardedEdit(t, base, 2, c.edit)

			doc := guardedDoc(t, base)
			doc.SetAllowedRoots(func(any) []string { return c.allowed })
			before := stateOf(doc)
			updates := observed(doc)
			refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrRootNotAllowed)
			if refused.Root != c.root {
				t.Fatalf("the refusal reports root %q, want %q", refused.Root, c.root)
			}
			if *updates != 0 {
				t.Fatalf("the refused update was handed to %d observers", *updates)
			}

			// The control: the same update with no list changes the document.
			open := guardedDoc(t, base)
			requireApplied(t, open, stateOf(open), ApplyUpdateV1(open, update, nil))
		})
	}
}

// An update that overwrites a key, deletes an item and writes beside held
// text, all under roots the list names, is applied whole and the document
// holds the result.
func TestUnit_AllowedRoots_AppliesAnUpdateThatStaysInsideTheRootsItNames(t *testing.T) {
	const base = "hello world"
	update := guardedEdit(t, base, 2, func(txn *Transaction, text *YText, m *YMap) {
		m.Set(txn, "k", "new")
		text.Insert(txn, 5, "XY", nil)
		text.Delete(txn, 0, 2)
	})

	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"t", "m"} })
	before := stateOf(doc)
	requireApplied(t, doc, before, ApplyUpdateV1(doc, update, nil))

	open := guardedDoc(t, base)
	if err := ApplyUpdateV1(open, update, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := stateOf(doc), stateOf(open); got != want {
		t.Fatalf("the document under the list reads\n%+v\nand the same update with no list leaves\n%+v", got, want)
	}
	if entry, _ := doc.GetMap("m").Get("k"); entry != "new" {
		t.Fatalf("the map holds %v, want the value the update set", entry)
	}
}

// An update that touches two roots, one the list names and one it does not,
// is refused whole: neither root changes, though the allowed root's items
// integrate before the refused one is reached.
func TestUnit_AllowedRoots_RefusesAnUpdateWholeWhenOneOfItsRootsIsNotAllowed(t *testing.T) {
	const base = "hello world"
	update := guardedEdit(t, base, 2, func(txn *Transaction, text *YText, m *YMap) {
		text.Insert(txn, 5, "XY", nil)
		m.Set(txn, "k", "new")
	})

	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrRootNotAllowed)
	if refused.Root != "m" {
		t.Fatalf("the refusal reports root %q, want \"m\"", refused.Root)
	}

	open := guardedDoc(t, base)
	requireApplied(t, open, stateOf(open), ApplyUpdateV1(open, update, nil))
}

// A long run of items under one allowed parent applies: the containment check
// walks each item's parent chain once and answers the rest from the map it
// memoises them in, so a run costs a lookup an item.
func TestUnit_AllowedRoots_ARunUnderOneAllowedParentApplies(t *testing.T) {
	const base, n = "hello world", 2_000
	update := guardedEdit(t, base, 2, func(txn *Transaction, text *YText, _ *YMap) {
		for i := range n {
			text.Insert(txn, i, "x", nil)
		}
	})

	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(doc)
	requireApplied(t, doc, before, ApplyUpdateV1(doc, update, nil))
	if got := doc.GetText("t").Len(); got != len(base)+n {
		t.Fatalf("the text is %d characters, want the %d it held and the %d the run added", got, len(base), n)
	}

	open := guardedDoc(t, base)
	if err := ApplyUpdateV1(open, update, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := doc.GetText("t").ToString(), open.GetText("t").ToString(); got != want {
		t.Fatal("the document under the list differs from one that took the run with no list")
	}
}

// A root the document does not hold when the apply begins is allowed by name:
// the update that creates it passes the name check in the decode, and every
// later item under it passes the containment check too, within the same
// apply.
func TestUnit_AllowedRoots_ARootTheUpdateCreatesIsAllowedByName(t *testing.T) {
	const base = "hello world"
	peer := New(WithClientID(2))
	if err := ApplyUpdateV1(peer, guardedState(t, base), nil); err != nil {
		t.Fatal(err)
	}
	sv := peer.StateVector()
	fresh := peer.GetText("fresh")
	peer.Transact(func(txn *Transaction) { fresh.Insert(txn, 0, "first", nil) })
	peer.Transact(func(txn *Transaction) { fresh.Insert(txn, 5, " and more", nil) })
	update := EncodeStateAsUpdateV1(peer, sv)

	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"fresh"} })
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatalf("an update under a root the list names was refused: %v", err)
	}
	if got, want := doc.GetText("fresh").ToString(), "first and more"; got != want {
		t.Fatalf("the new root holds %q, want %q", got, want)
	}

	// The same update under a list that does not name it is refused at the
	// decode, by the name check.
	other := guardedDoc(t, base)
	other.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(other)
	refused := requireRefused(t, other, before, ApplyUpdateV1(other, update, nil), ErrRootNotAllowed)
	if refused.Root != "fresh" {
		t.Fatalf("the refusal reports root %q, want \"fresh\"", refused.Root)
	}
}

// gapStringUpdate is one struct of client 2 at clock, holding value: parented
// to the root text "t" where origin is nil, and written beside the item at
// origin where it is not. It is hand-encoded because no writer document
// produces the shape the test below needs - a struct whose left origin is its
// own client's earlier clock, which a document that has not seen that clock
// parks and the drain later trims in place.
func gapStringUpdate(clock uint64, value string, origin *ID) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // one client
	enc.WriteVarUint(1) // one struct of it
	enc.WriteVarUint(2) // client 2
	enc.WriteVarUint(clock)
	if origin == nil {
		enc.WriteUint8(4) // no origins, a named parent
		enc.WriteVarUint(1)
		enc.WriteVarString("t")
	} else {
		enc.WriteUint8(0x84) // a left origin, string content
		enc.WriteVarUint(uint64(origin.Client))
		enc.WriteVarUint(origin.Clock)
	}
	enc.WriteVarString(value)
	enc.WriteVarUint(0) // an empty delete set
	return bytes.Clone(enc.Bytes())
}

// A refusal must never cost the document content it had already accepted.
// The apply that fills a gap drains the pending queue, and the drain changes
// the parked items it integrates in place: here the parked "BC" at clock 1,
// whose left origin is clock 0 of its own client, is trimmed against the
// arriving "AB", which carries clocks 0 and 1. A refusal raised after that
// drain had succeeded could not give the trim back - the whole-apply rollback
// restores the queue's pointers, and the content is gone - so this node would
// keep a document every peer disagrees with, a divergence no memory graph
// shows.
//
// The gap is then filled with LESS than the refused update carried, one
// character at clock 0, which is what makes the loss visible: the trimmed
// item now waits on a clock that fill never supplies, so "BC" can never land
// and the text stays "A". Re-applying the whole "AB" would cover the trimmed
// item's new dependency and hide the defect, which is why the fill is "A".
//
// The fix is that the struct and byte budget is read before the drain runs
// (drainAfterApply) and endRemoteApply raises no refusal of its own.
func TestUnit_ApplyBudget_ARefusalAfterTheDrainKeepsWhatTheDocumentHeld(t *testing.T) {
	for _, shape := range []struct {
		name string
		v2   bool
	}{{name: "v1"}, {name: "v2", v2: true}} {
		t.Run(shape.name, func(t *testing.T) {
			apply := func(doc *Doc, update []byte) error {
				if !shape.v2 {
					return ApplyUpdateV1(doc, update, "peer")
				}
				asV2, err := UpdateV1ToV2(update)
				if err != nil {
					t.Fatalf("re-encode the update as V2: %v", err)
				}
				return ApplyUpdateV2(doc, asV2, "peer")
			}

			doc := New(WithClientID(7), WithGC(false))
			if err := apply(doc, gapStringUpdate(1, "BC", &ID{Client: 2, Clock: 0})); err != nil {
				t.Fatalf("park the update behind the gap: %v", err)
			}
			if parked := doc.PendingStats(); parked.Items == 0 {
				t.Fatal("the update behind the gap did not park, so this drives nothing")
			}

			doc.SetApplyBudget(func(any) ApplyBudget {
				return ApplyBudget{Bytes: 1}
			})
			if err := apply(doc, gapStringUpdate(0, "AB", nil)); !errors.Is(err, ErrApplyBudget) {
				t.Fatalf("the apply answered %v, want the byte budget's refusal", err)
			}

			doc.SetApplyBudget(nil)
			if err := apply(doc, gapStringUpdate(0, "A", nil)); err != nil {
				t.Fatalf("the fill of the gap: %v", err)
			}
			if got := doc.GetText("t").ToString(); got != "ABC" {
				t.Fatalf("the document reads %q, want \"ABC\": the refused apply's drain kept its trim", got)
			}
		})
	}
}

// A frame whose ContentAny declares a length is refused on that length,
// before the list is allocated for it. The declared count used to be read
// only after make([]any, n) had run and after the first value was in memory,
// so a budget of one value refused a frame of two million of them having
// already taken 32 MB - the out-of-memory path the budget exists to close.
//
// Measured by this test, which logs both allocations: a frame declaring 1
// value, refused, allocates 2,144 bytes; one declaring 2,000,000, refused,
// allocates 2,368 bytes, against the 32,008,272 the same frame allocated
// before the fix (1,776 for the one-value frame). The test requires it under
// 1,000,000: a check made after the allocation cannot reach it.
func TestUnit_ApplyBudget_ADeclaredLengthIsRefusedBeforeItIsAllocated(t *testing.T) {
	const base = "hello world"
	const many = 2_000_000
	oneValue := ApplyBudget{Decoded: 1}

	one := budgetDoc(t, base)
	one.SetApplyBudget(func(any) ApplyBudget { return oneValue })
	oneBefore := stateOf(one)
	oneAllocated, oneTook, oneErr := applyMeasured(one, denseAnyValues(1, 1))
	requireRefused(t, one, oneBefore, oneErr, ErrApplyBudget)

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget { return oneValue })
	before := stateOf(doc)
	allocated, took, err := applyMeasured(doc, denseAnyValues(1, many))
	t.Logf("a frame declaring %d values was refused having allocated %d bytes in %s; one declaring a single value allocated %d bytes in %s",
		many, allocated, took, oneAllocated, oneTook)
	requireRefused(t, doc, before, err, ErrApplyBudget)
	if allocated >= 1_000_000 {
		t.Fatalf("the refused frame allocated %d bytes, want under 1,000,000: the declared length was allocated before it was charged", allocated)
	}

	// The control: the same shape at a length the budget holds applies, so
	// what refuses the frames above is the length and not the shape.
	fits := budgetDoc(t, base)
	fits.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 5} })
	fitsBefore := stateOf(fits)
	requireApplied(t, fits, fitsBefore, ApplyUpdateV1(fits, denseAnyValues(1, 4), nil))
	if got := storeStructs(fits); got != fitsBefore.structs+1 {
		t.Fatalf("the applied frame left %d structs, want the %d before it and its own", got, fitsBefore.structs)
	}
}

// nestedTypeChain is an update of n structs for client 9, each a YMap held
// under the key "c" of the one before it, the first of them under the root
// name: a chain of n types nested n deep, which is what makes the
// allowed-roots walk up the parents repeat itself.
func nestedTypeChain(root string, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	for i := range n {
		enc.WriteUint8(wireType | flagHasParentSub)
		if i == 0 {
			enc.WriteUint8(1) // a named root follows
			enc.WriteVarString(root)
		} else {
			enc.WriteUint8(0) // the parent is the struct before it, by ID
			enc.WriteVarUint(9)
			enc.WriteVarUint(uint64(i - 1))
		}
		enc.WriteVarString("c")
		enc.WriteUint8(1) // YMap
	}
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// An update of types nested inside one another costs the allowed-roots check
// one walk, not one for each item. The check memoises the root above each
// type it answers, and the walk stops at the first ancestor already answered
// and records the root for every type it passed, so the whole chain is
// answered once.
//
// Before that it recorded only the type it started from, so each new child
// walked to the root again: a 213 KB update of 32,768 nested types held the
// document's write lock for 11.235 s against 34.55 ms with no list, a factor
// of 325, bounded by neither the struct count nor the scan budget. Measured
// by this test, which logs both times, on the 278,408-byte update below:
// 146.3 ms with no list and 272.4 ms under one, a factor of
// 1.9. The test allows a factor of 5 and 200 ms on top of it, loose enough
// not to flake on a loaded machine and far under what the repeated walk
// takes.
func TestUnit_AllowedRoots_AChainOfNestedTypesIsWalkedOnce(t *testing.T) {
	const base, nested = "hello world", 32_768
	update := nestedTypeChain("deep", nested)

	open := budgetDoc(t, base)
	_, off, err := applyMeasured(open, update)
	if err != nil {
		t.Fatalf("the document with no list refused the chain: %v", err)
	}
	if got, want := storeStructs(open), 1+nested; got != want {
		t.Fatalf("the chain left %d structs, want %d", got, want)
	}

	doc := budgetDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"deep"} })
	before := stateOf(doc)
	_, on, err := applyMeasured(doc, update)
	t.Logf("a chain of %d nested types in a %d-byte update: %s with no list, %s under one", nested, len(update), off, on)
	requireApplied(t, doc, before, err)
	if got, want := storeStructs(doc), 1+nested; got != want {
		t.Fatalf("the chain under the list left %d structs, want %d", got, want)
	}
	if limit := 5*off + 200*time.Millisecond; on > limit {
		t.Fatalf("the chain under the list took %s, want within %s of the %s it takes with no list", on, limit, off)
	}
}

// subdocState is a document of client 7 holding two roots: the map "t", whose
// key "doc" holds child, and the map "m", whose key "k" holds a value. A
// remote update that deletes both deletes the subdocument's item first, since
// it carries the lower clock, so a refusal raised at the second delete is
// raised with the subdocument already detached.
func subdocState(t *testing.T, opts ...DocOption) (*Doc, *Doc) {
	t.Helper()
	doc := New(append([]DocOption{WithClientID(7)}, opts...)...)
	child := New(WithGUID("child"), WithShouldLoad(false))
	holder, m := doc.GetMap("t"), doc.GetMap("m")
	doc.Transact(func(txn *Transaction) {
		holder.Set(txn, "doc", child)
		m.Set(txn, "k", "old")
	})
	return doc, child
}

// deleteBothKeys is a V1 update of a peer deleting the key "doc" of the map
// "t" and the key "k" of the map "m" of doc, which holds neither structs of
// its own nor a root name: two delete ranges and nothing else.
func deleteBothKeys(t *testing.T, doc *Doc) []byte {
	t.Helper()
	peer := New(WithClientID(2))
	if err := ApplyUpdateV1(peer, EncodeStateAsUpdateV1(doc, nil), nil); err != nil {
		t.Fatalf("the peer refused the state: %v", err)
	}
	sv := peer.StateVector()
	// The roots are opened outside the transaction: GetMap takes the
	// document's lock, which the transaction already holds.
	holder, m := peer.GetMap("t"), peer.GetMap("m")
	peer.Transact(func(txn *Transaction) {
		holder.Delete(txn, "doc")
		m.Delete(txn, "k")
	})
	return EncodeStateAsUpdateV1(peer, sv)
}

// The allowed-roots rule refuses an update at its second delete, after the
// first has deleted the key holding a subdocument. On a document with
// whole-or-nothing applies the refused update leaves the subdocument under
// its key and attached, so its Load tells the document. On a document without,
// the refusal keeps the first delete, as it keeps anything the apply did
// before the item that passed the list, and the subdocument is removed as an
// applied delete removes it.
func TestUnit_AllowedRoots_ARefusalKeepsADeletedSubdocumentAttached(t *testing.T) {
	doc, child := subdocState(t, WithWholeOrNothingApplies(true))
	update := deleteBothKeys(t, doc)

	doc.SetAllowedRoots(func(any) []string { return []string{"t"} })
	err := ApplyUpdateV1(doc, update, nil)
	if !errors.Is(err, ErrRootNotAllowed) {
		t.Fatalf("the update was applied with err %v, want the roots rule's refusal", err)
	}
	if entry, held := doc.GetMap("t").Get("doc"); !held || entry != child {
		t.Fatalf("the refused update left %v under the key the subdocument held", entry)
	}
	if !loadNotifies(doc, child) {
		t.Fatal("the subdocument's Load told its parent nothing: the refused update left it detached")
	}

	kept, keptChild := subdocState(t)
	kept.SetAllowedRoots(func(any) []string { return []string{"t"} })
	if err := ApplyUpdateV1(kept, deleteBothKeys(t, kept), nil); !errors.Is(err, ErrRootNotAllowed) {
		t.Fatalf("the update was applied with err %v, want the roots rule's refusal", err)
	}
	if _, held := kept.GetMap("t").Get("doc"); held {
		t.Fatal("the refusal took back the first delete on a document without whole-or-nothing applies")
	}
	if entry, _ := kept.GetMap("m").Get("k"); entry != "old" {
		t.Fatalf("the refused delete left %v under the key, want the value it held", entry)
	}
	if loadNotifies(kept, keptChild) {
		t.Fatal("a removed subdocument still told its parent")
	}
}

// repeatedKeyMap is an update of one struct for client 9 whose ContentAny
// holds one lib0 map of n entries, every one of them the key "k": the Go map
// the decode builds holds one of them, and the frame carried n.
func repeatedKeyMap(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(1) // one struct
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(wireAny)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString("dense")
	enc.WriteVarUint(1) // one value
	enc.WriteUint8(118) // lib0 Any: a map
	enc.WriteVarUint(uint64(n))
	for range n {
		enc.WriteVarString("k")
		enc.WriteUint8(126) // lib0 Any: null
	}
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// adjacentDeletes is an update of no structs whose delete set names n
// one-clock ranges of client, each touching the one before it, which the
// decode sorts and merges into a single range before it returns.
func adjacentDeletes(client ClientID, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(0)              // no structs
	enc.WriteVarUint(1)              // one client in the delete set
	enc.WriteVarUint(uint64(client)) // the client that wrote the base text
	enc.WriteVarUint(uint64(n))
	for i := range n {
		enc.WriteVarUint(uint64(i)) // clock
		enc.WriteVarUint(1)         // one clock long
	}
	return bytes.Clone(enc.Bytes())
}

// Decoded counts what the apply read off the frame, not what survived the
// decode. Entries of a lib0 Any map that share a key overwrite one another in
// the Go map the decode builds, and delete ranges that touch are merged by
// the decode's own sort, so a count taken afterwards counted a hundred
// thousand map entries as one and eleven delete ranges as one: a frame of
// 100,000 entries was admitted under Decoded: 3.
func TestUnit_ApplyBudget_CountsEachEntryAndEachRangeAsItIsRead(t *testing.T) {
	const base = "hello world"
	const entries = 100_000

	t.Run("map entries that share a key", func(t *testing.T) {
		update := repeatedKeyMap(entries)

		doc := budgetDoc(t, base)
		doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 3} })
		before := stateOf(doc)
		requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrApplyBudget)

		// The control: a budget that holds the struct, the map and all of its
		// entries applies the same frame, and what it leaves is the one entry
		// the repeated key decodes to - which is what the count used to see.
		fits := budgetDoc(t, base)
		fits.SetApplyBudget(func(any) ApplyBudget {
			return ApplyBudget{Decoded: 2 + entries}
		})
		fitsBefore := stateOf(fits)
		requireApplied(t, fits, fitsBefore, ApplyUpdateV1(fits, update, nil))
		held, ok := fits.store.clients[9][0].Content.(*ContentAny)
		if !ok {
			t.Fatalf("the struct holds %T, want a ContentAny", fits.store.clients[9][0].Content)
		}
		decoded, ok := held.Vals[0].(map[string]any)
		if !ok || len(decoded) != 1 {
			t.Fatalf("the frame decoded to %v, want the one entry the repeated key leaves", held.Vals[0])
		}
	})

	t.Run("delete ranges that merge", func(t *testing.T) {
		// The base text is eleven clocks of client 1, deleted one clock at a
		// time: eleven ranges on the wire and one once the decode has sorted
		// and merged them.
		update := adjacentDeletes(1, len(base))

		doc := budgetDoc(t, base)
		doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 5} })
		before := stateOf(doc)
		refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrApplyBudget)
		if refused.Decoded != len(base) {
			t.Fatalf("the refusal reports %d decoded values, want the %d ranges the frame carried", refused.Decoded, len(base))
		}

		fits := budgetDoc(t, base)
		fits.SetApplyBudget(func(any) ApplyBudget {
			return ApplyBudget{Decoded: len(base)}
		})
		fitsBefore := stateOf(fits)
		requireApplied(t, fits, fitsBefore, ApplyUpdateV1(fits, update, nil))
		if got := fits.GetText("t").ToString(); got != "" {
			t.Fatalf("the applied ranges left %q, want the empty text they delete", got)
		}
	})
}

// xmlElementUpdate is an update of one struct for client 9: an XML element
// under the root name, whose node name is n bytes. The name is the sender's
// to choose and the document keeps it for as long as it holds the element.
func xmlElementUpdate(root string, n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(1) // one struct
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(wireType)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString(root)
	enc.WriteUint8(3) // YXmlElement
	enc.WriteVarString(strings.Repeat("x", n))
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// Bytes counts the name an XML element or hook carries. Every ContentType
// used to count as no bytes at all - a type is an empty container whose
// structs are counted as they integrate - but an element and a hook keep a
// name their sender sized, so a one-million-byte node name was admitted and
// retained under Bytes: 1.
func TestUnit_ApplyBudget_BytesCountsAnXMLTypeName(t *testing.T) {
	const base, name = "hello world", 1_000_000
	update := xmlElementUpdate("xml", name)

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: 1, Structs: 1, Bytes: name - 1} })
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrApplyBudget)
	// The refusal reports the bytes of the root the update names, which it
	// counted before the element passed the budget.
	if refused.Bytes != len("xml") || refused.Structs != 0 {
		t.Fatalf("the refusal reports %d structs and %d bytes, want none and the root name's %d", refused.Structs, refused.Bytes, len("xml"))
	}
	if _, held := doc.share["xml"]; held {
		t.Fatal("the refused update left its root on the document")
	}

	// The control: the same update with no budget applies and the document
	// keeps the name, which is what makes it a cost worth counting.
	open := budgetDoc(t, base)
	if err := ApplyUpdateV1(open, update, nil); err != nil {
		t.Fatalf("the document with no budget refused the element: %v", err)
	}
	ct, ok := open.store.clients[9][0].Content.(*ContentType)
	if !ok {
		t.Fatalf("the struct holds %T, want a ContentType", open.store.clients[9][0].Content)
	}
	elem, ok := ct.Type.owner.(*YXmlElement)
	if !ok || len(elem.NodeName) != name {
		t.Fatalf("the applied element holds a name of %T, want the %d bytes it was sent", ct.Type.owner, name)
	}
}

// A refused update leaves the type it formatted plain. Integrating a
// ContentFormat raises hasFormatting and cleanFormatting and bumps fmtGen on
// the type it hangs under, and nothing lowers them again, so a refused update
// used to move a plain text onto the formatting-aware path for the rest of
// the document's life - every later Delete paying for the cleanup walk a
// plain text skips.
//
// The update formats text under an allowed root and then overwrites a key of
// a root the list does not name. The overwrite carries an origin and names no
// root on the wire, so it is refused by the containment check as it
// integrates, after the format items have integrated and raised the flags.
func TestUnit_AllowedRoots_ARefusalPutsTheFormattingFlagsBack(t *testing.T) {
	const base = "hello world"
	update := guardedEdit(t, base, 2, func(txn *Transaction, text *YText, m *YMap) {
		text.Format(txn, 0, 5, Attributes{"b": true})
		m.Set(txn, "k", "new")
	})

	doc := guardedDoc(t, base)
	text := doc.GetText("t")
	if text.hasFormatting || text.cleanFormatting || text.fmtGen != 0 {
		t.Fatalf("the base text is already on the formatting path: %v %v %d", text.hasFormatting, text.cleanFormatting, text.fmtGen)
	}
	doc.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(doc)
	requireRefused(t, doc, before, ApplyUpdateV1(doc, update, nil), ErrRootNotAllowed)
	if text.hasFormatting || text.cleanFormatting || text.fmtGen != 0 {
		t.Fatalf("the refused update left the text on the formatting path: %v %v %d", text.hasFormatting, text.cleanFormatting, text.fmtGen)
	}

	// The control: the same update with no list applies and moves the text
	// onto that path, so the flags above were the refusal's to put back.
	open := guardedDoc(t, base)
	requireApplied(t, open, stateOf(open), ApplyUpdateV1(open, update, nil))
	applied := open.GetText("t")
	if !applied.hasFormatting || !applied.cleanFormatting || applied.fmtGen == 0 {
		t.Fatalf("the applied update left the text plain: %v %v %d", applied.hasFormatting, applied.cleanFormatting, applied.fmtGen)
	}
}

// rootNamedUpdate is an update of two string structs for client 9 under the
// root name: the first names the root on the wire, and the second carries a
// left origin and names nothing at all, so what must let the second through
// is the containment check over what the apply adds and not the name check
// in the decode.
func rootNamedUpdate(root, first, second string) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(2) // two structs
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(wireString)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString(root)
	enc.WriteVarString(first)
	enc.WriteUint8(wireString | flagHasOrigin)
	enc.WriteVarUint(9)                      // the left origin's client
	enc.WriteVarUint(uint64(len(first) - 1)) // its clock: the first struct's last
	enc.WriteVarString(second)
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// A root the document does not hold yet is allowed by its name, and the empty
// name is a name like any other. The containment check used to read the empty
// name as "this type is no root of the document" and refuse every item under
// a root named "" that the same update created, so a list that named it could
// not create it.
func TestUnit_AllowedRoots_AnAllowedEmptyRootNameCanBeCreated(t *testing.T) {
	const base = "hello world"
	update := rootNamedUpdate("", "first", " and more")

	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{""} })
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatalf("an update under the empty root name the list allows was refused: %v", err)
	}
	if got, want := doc.GetText("").ToString(), "first and more"; got != want {
		t.Fatalf("the root named %q holds %q, want %q", "", got, want)
	}

	// The control: a list that does not name it refuses the same update, at
	// the name check in the decode, and names the root it refused.
	other := guardedDoc(t, base)
	other.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(other)
	refused := requireRefused(t, other, before, ApplyUpdateV1(other, update, nil), ErrRootNotAllowed)
	if refused.Root != "" {
		t.Fatalf("the refusal reports root %q, want the empty name the update wrote", refused.Root)
	}

	// A named root takes the same two structs, so the shape itself applies
	// under a list that allows it whatever the name is.
	named := guardedDoc(t, base)
	named.SetAllowedRoots(func(any) []string { return []string{"fresh"} })
	if err := ApplyUpdateV1(named, rootNamedUpdate("fresh", "first", " and more"), nil); err != nil {
		t.Fatalf("the same shape under a named root was refused: %v", err)
	}
	if got, want := named.GetText("fresh").ToString(), "first and more"; got != want {
		t.Fatalf("the root \"fresh\" holds %q, want %q", got, want)
	}
}

// anyValueUpdate is an update of one struct for client 9 whose ContentAny
// holds the lib0 Any value write puts on the wire and then two nulls.
//
// The two nulls are what make a count the scan stops short of visible: a
// counter that cannot read the value, or reads the wrong number of bytes of
// it, has already charged for the value itself and would otherwise agree
// with the decoder by accident. Stopping before the nulls undercounts by
// two, whatever the tag.
func anyValueUpdate(write func(enc *encoding.Encoder)) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(1) // one struct
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(wireAny)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString("dense")
	enc.WriteVarUint(3) // the value below and two nulls after it
	write(enc)
	enc.WriteUint8(126)
	enc.WriteUint8(126)
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// decodedValues is how many values v is, counted as the budget counts them:
// v itself, each element of an array and each entry of a map, at any depth.
// It reads the value the decoder built, so it is the number the counter that
// reads the wire has to agree with.
func decodedValues(v any) int {
	n := 1
	switch v := v.(type) {
	case []any:
		for _, e := range v {
			n += decodedValues(e)
		}
	case map[string]any:
		for _, e := range v {
			n += decodedValues(e)
		}
	}
	return n
}

// anyValuesOf is the values the struct client 9 wrote decoded to, read off
// the document that applied it with no budget.
func anyValuesOf(t *testing.T, doc *Doc) []any {
	t.Helper()
	held := doc.store.clients[9]
	if len(held) != 1 {
		t.Fatalf("the document holds %d structs of client 9, want 1", len(held))
	}
	content, ok := held[0].Content.(*ContentAny)
	if !ok {
		t.Fatalf("the struct holds %T, want a ContentAny", held[0].Content)
	}
	return content.Vals
}

// Decoded counts a lib0 Any frame as exactly the values the decoder builds
// from it, for every tag of the format and for values nested inside one
// another.
//
// This is what holds the counter to the decoder. The count is taken off the
// frame's bytes by countAnyValue, a second reader of the lib0 Any grammar
// beside anycodec.readAny, kept there because counting off the decoded value
// cannot see a map's repeated keys or a declared length before it is
// allocated for (countAnyRun). Two readers can drift, and the dangerous
// drift is silent: a tag the decode accepts and the scan does not ends the
// scan early, so the apply is charged for less than it decoded and the
// budget admits more than it says, with no error raised anywhere. A frame
// the decode REJECTS is safe - the decode reports the fault - and is not
// what this guards.
//
// So every tag is covered by name, and the expected count is read off the
// values the decode produced rather than written down: the apply must be
// admitted by a budget of exactly that many values and refused by one value
// less. A tag added to isAnyTag with no row here fails the coverage check
// below rather than going unnoticed.
func TestUnit_ApplyBudget_CountsEveryLib0AnyTagAsTheDecoderReadsIt(t *testing.T) {
	const base = "hello world"
	rows := map[string]struct {
		tag   byte
		write func(enc *encoding.Encoder)
	}{
		"undefined":              {127, func(e *encoding.Encoder) { e.WriteUint8(127) }},
		"null":                   {126, func(e *encoding.Encoder) { e.WriteUint8(126) }},
		"true":                   {120, func(e *encoding.Encoder) { e.WriteUint8(120) }},
		"false":                  {121, func(e *encoding.Encoder) { e.WriteUint8(121) }},
		"an integer":             {125, func(e *encoding.Encoder) { e.WriteUint8(125); e.WriteVarInt(-42) }},
		"a 32-bit float":         {124, func(e *encoding.Encoder) { e.WriteUint8(124); e.WriteFloat32(1.5) }},
		"a 64-bit float":         {123, func(e *encoding.Encoder) { e.WriteUint8(123); e.WriteFloat64(2.5) }},
		"a big integer":          {122, func(e *encoding.Encoder) { e.WriteUint8(122); e.WriteBigInt64(1 << 40) }},
		"a string":               {119, func(e *encoding.Encoder) { e.WriteUint8(119); e.WriteVarString("hello") }},
		"binary":                 {116, func(e *encoding.Encoder) { e.WriteUint8(116); e.WriteVarBytes([]byte{1, 2, 3}) }},
		"an array of three":      {117, func(e *encoding.Encoder) { writeAnyArray(e, 3) }},
		"a map of three entries": {118, func(e *encoding.Encoder) { writeAnyMap(e, 3) }},
		"an array holding a map": {117, func(e *encoding.Encoder) {
			e.WriteUint8(117)
			e.WriteVarUint(2)
			e.WriteUint8(126)
			writeAnyMap(e, 3)
		}},
		"a map holding an array": {118, func(e *encoding.Encoder) {
			e.WriteUint8(118)
			e.WriteVarUint(2)
			e.WriteVarString("a")
			e.WriteUint8(126)
			e.WriteVarString("b")
			writeAnyArray(e, 3)
		}},
	}

	covered := map[byte]struct{}{}
	for name, row := range rows {
		covered[row.tag] = struct{}{}
		t.Run(name, func(t *testing.T) {
			update := anyValueUpdate(row.write)

			// What the decoder built, with no budget to refuse it: one
			// struct and every value of its content.
			open := budgetDoc(t, base)
			if err := ApplyUpdateV1(open, update, nil); err != nil {
				t.Fatalf("the document with no budget refused the frame: %v", err)
			}
			want := 1
			for _, v := range anyValuesOf(t, open) {
				want += decodedValues(v)
			}

			fits := budgetDoc(t, base)
			fits.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: want} })
			fitsBefore := stateOf(fits)
			if err := ApplyUpdateV1(fits, update, nil); err != nil {
				t.Fatalf("a budget of the %d values the decode produced refused the frame, so the count is above it: %v", want, err)
			}
			requireApplied(t, fits, fitsBefore, nil)

			tight := budgetDoc(t, base)
			tight.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Decoded: want - 1} })
			tightBefore := stateOf(tight)
			err := ApplyUpdateV1(tight, update, nil)
			if err == nil {
				t.Fatalf("a budget of %d values applied a frame of %d, so the count is below it: the counter and the decoder disagree", want-1, want)
			}
			requireRefused(t, tight, tightBefore, err, ErrApplyBudget)
		})
	}

	for tag := range 256 {
		if b := byte(tag); isAnyTag(b) {
			if _, held := covered[b]; !held {
				t.Fatalf("lib0 Any tag %d has no row here: the counter is unpinned for it", b)
			}
		}
	}
}

// writeAnyArray writes a lib0 Any array of n nulls, and writeAnyMap a lib0
// Any map of n entries under distinct keys, so that what the decoder builds
// holds every value the frame carried and the two counts must agree by
// construction.
func writeAnyArray(enc *encoding.Encoder, n int) {
	enc.WriteUint8(117)
	enc.WriteVarUint(uint64(n))
	for range n {
		enc.WriteUint8(126)
	}
}

func writeAnyMap(enc *encoding.Encoder, n int) {
	enc.WriteUint8(118)
	enc.WriteVarUint(uint64(n))
	for i := range n {
		enc.WriteVarString(fmt.Sprintf("k%d", i))
		enc.WriteUint8(126)
	}
}

// guardedHandoff is the whole state of a peer that holds guardedState and
// then ran edit: an update naming every root the document holds, as a client
// handing a document's whole state back does, carrying whatever edit wrote
// on top of it.
func guardedHandoff(t *testing.T, base string, client ClientID, edit func(txn *Transaction, text *YText, m *YMap)) []byte {
	t.Helper()
	peer := New(WithClientID(client))
	if err := ApplyUpdateV1(peer, guardedState(t, base), nil); err != nil {
		t.Fatalf("the peer refused the guarded state: %v", err)
	}
	text, m := peer.GetText("t"), peer.GetMap("m")
	peer.Transact(func(txn *Transaction) { edit(txn, text, m) })
	return EncodeStateAsUpdateV1(peer, nil)
}

// An update naming a root the document already holds is left to the
// containment rule: the name check refuses only a root the document does not
// hold, since creating one is what that check is for.
//
// Without the division a legitimate client is refused. A peer handing back
// the whole state of a document names every root the document holds,
// including one the owner allows no writes under, because the decode
// resolves each struct's root as it reads the struct - before the integrate
// discovers the struct is one the document already has and skips it. Such a
// handoff integrates nothing and deletes nothing under that root, and used
// to be refused all the same.
func TestUnit_AllowedRoots_AHandoffNamingAHeldRootItDoesNotWriteUnderApplies(t *testing.T) {
	const base = "hello world"
	onlyText := func(any) []string { return []string{"t"} }

	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(onlyText)
	before := stateOf(doc)
	if err := ApplyUpdateV1(doc, guardedState(t, base), nil); err != nil {
		t.Fatalf("a handoff of the state the document already holds was refused: %v", err)
	}
	if after := stateOf(doc); after != before {
		t.Fatalf("the handoff changed the document:\nbefore %+v\nafter  %+v", before, after)
	}

	// The same handoff carrying one new item under the root the list does
	// not name is refused by the containment rule, and so is one deleting an
	// item under it: what the name check stopped doing, containment does.
	for name, c := range map[string]struct {
		edit func(txn *Transaction, text *YText, m *YMap)
	}{
		"one new item under the held root": {func(txn *Transaction, _ *YText, m *YMap) { m.Set(txn, "k", "new") }},
		"a delete under the held root":     {func(txn *Transaction, _ *YText, m *YMap) { m.Delete(txn, "k") }},
	} {
		t.Run(name, func(t *testing.T) {
			update := guardedHandoff(t, base, 2, c.edit)

			guarded := guardedDoc(t, base)
			guarded.SetAllowedRoots(onlyText)
			guardedBefore := stateOf(guarded)
			refused := requireRefused(t, guarded, guardedBefore, ApplyUpdateV1(guarded, update, nil), ErrRootNotAllowed)
			if refused.Root != "m" {
				t.Fatalf("the refusal reports root %q, want \"m\"", refused.Root)
			}

			// The control: the same update with no list changes the document.
			open := guardedDoc(t, base)
			requireApplied(t, open, stateOf(open), ApplyUpdateV1(open, update, nil))
		})
	}

	// A root the document does not hold at all is still refused where the
	// decode resolves its name, before anything under it is read.
	fresh := guardedDoc(t, base)
	fresh.SetAllowedRoots(onlyText)
	freshBefore := stateOf(fresh)
	refused := requireRefused(t, fresh, freshBefore,
		ApplyUpdateV1(fresh, rootNamedUpdate("fresh", "first", " and more"), nil), ErrRootNotAllowed)
	if refused.Root != "fresh" {
		t.Fatalf("the refusal reports root %q, want \"fresh\"", refused.Root)
	}
	if refused.Structs != 0 {
		t.Fatalf("the refusal reports %d structs, want none: the name check refuses before one integrates", refused.Structs)
	}
}

// Split bounds the structs a delete makes by splitting items the document
// holds, which Structs does not count: a delete of five characters inside
// the run of "hello world" cuts it into three. Split: 1, the smallest limit,
// allows one split and refuses the second: the delete is refused at its
// second split, before that split cuts anything, and an insert inside the
// run, which makes one split, applies. Under Split: 2 the delete applies and
// the document keeps the two structs it made.
func TestUnit_ApplyBudget_RefusesADeleteWhoseSplitsPassWhatItAllows(t *testing.T) {
	const base = "hello world"
	deleteOnly := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Delete(txn, 3, 5) })
	split := func(n int) func(any) ApplyBudget {
		return func(any) ApplyBudget { return ApplyBudget{Split: n} }
	}

	doc := budgetDoc(t, base)
	doc.SetApplyBudget(split(1))
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, deleteOnly, nil), ErrApplyBudget)
	if refused.Split != 1 || refused.Structs != 0 {
		t.Fatalf("the refusal reports %d splits and %d structs, want the one split before the refused one and none",
			refused.Split, refused.Structs)
	}
	oneSplit := editUpdate(t, base, func(txn *Transaction, text *YText) { text.Insert(txn, 3, "x", nil) })
	requireApplied(t, doc, before, ApplyUpdateV1(doc, oneSplit, nil))
	if got, want := storeStructs(doc), before.structs+2; got != want {
		t.Fatalf("the insert left %d structs, want the %d held, the one split and the character", got, want)
	}

	fits := budgetDoc(t, base)
	fits.SetApplyBudget(split(2))
	fitsBefore := stateOf(fits)
	requireApplied(t, fits, fitsBefore, ApplyUpdateV1(fits, deleteOnly, nil))
	if got, want := storeStructs(fits), fitsBefore.structs+2; got != want {
		t.Fatalf("the applied delete left %d structs, want the %d it held and the 2 its splits made", got, want)
	}
}

// The splits the drain of the parked queue makes count against the apply that
// fills the gap. A drain that would pass Split stops and parks the rest, and
// the apply that filled the gap stands; a later apply with room in its budget
// drains the rest.
func TestUnit_ApplyBudget_ADrainsSplitsCountAgainstTheApplyThatFillsTheGap(t *testing.T) {
	const base = "hello world"
	writer := New(WithClientID(2))
	if err := ApplyUpdateV1(writer, baseState(t, base), nil); err != nil {
		t.Fatal(err)
	}
	text := writer.GetText("t")
	held := writer.StateVector()
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 3, "x", nil) })
	first := EncodeStateAsUpdateV1(writer, held)
	afterFirst := writer.StateVector()
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 7, "y", nil) })
	second := EncodeStateAsUpdateV1(writer, afterFirst)

	doc := New(WithClientID(7))
	if err := ApplyUpdateV1(doc, baseState(t, base), nil); err != nil {
		t.Fatal(err)
	}
	if err := ApplyUpdateV1(doc, second, nil); err != nil {
		t.Fatal(err)
	}
	// "x" makes one split to land; the parked "y" then needs a second.
	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{Split: 1} })
	if err := ApplyUpdateV1(doc, first, nil); err != nil {
		t.Fatalf("the apply that filled the gap returned %v, want it to stand", err)
	}
	if got := doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs are parked, want the one the drain could not split for", got)
	}
	if got, want := doc.GetText("t").ToString(), "helxlo world"; got != want {
		t.Fatalf("the document reads %q, want %q", got, want)
	}

	doc.SetApplyBudget(func(any) ApplyBudget { return ApplyBudget{} })
	if err := ApplyUpdateV1(doc, first, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := doc.GetText("t").ToString(), text.ToString(); got != want || doc.PendingStats().Items != 0 {
		t.Fatalf("the document reads %q with %d parked, want %q and none", got, doc.PendingStats().Items, want)
	}
}

// A struct of the update that parks behind a clock the document lacks counts
// against Structs and Bytes when it parks, so a sender cannot pass the budget
// by writing behind a clock it never sends: the document would hold the
// struct, carry it in its stored state and integrate it once the gap is
// filled. Under Structs: 1 an update that parks two structs is refused at the
// second and keeps the first parked; under Bytes: 1 one that parks "BC" is
// refused and parks nothing. The apply that fills the gap is not charged for
// a parked struct again.
func TestUnit_ApplyBudget_CountsTheStructsAnUpdateParks(t *testing.T) {
	parked := gapStringUpdate(1, "BC", &ID{Client: 2, Clock: 0}) // waits on 2@0
	budget := func(structs, bytes int) func(any) ApplyBudget {
		return func(any) ApplyBudget { return ApplyBudget{Structs: structs, Bytes: bytes} }
	}

	// Two structs of client 2 past a clock 0 the document never receives:
	// "B" beside it and "C" before it.
	writer := New(WithClientID(2))
	text := writer.GetText("t")
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 0, "A", nil) })
	held := writer.StateVector()
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 1, "B", nil) })
	writer.Transact(func(txn *Transaction) { text.Insert(txn, 0, "C", nil) })
	twoParked := EncodeStateAsUpdateV1(writer, held)

	for _, c := range []struct {
		name   string
		budget func(any) ApplyBudget
		update []byte
		kept   int // structs parked, and bytes, when the apply stops
	}{
		{"Structs: 1", budget(1, 0), twoParked, 1},
		{"Bytes: 1", budget(0, 1), parked, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := New(WithClientID(7))
			doc.SetApplyBudget(c.budget)
			err := ApplyUpdateV1(doc, c.update, nil)
			var refused *RefusedUpdateError
			if !errors.Is(err, ErrApplyBudget) || !errors.As(err, &refused) {
				t.Fatalf("the parking update returned %v, want ErrApplyBudget in a RefusedUpdateError", err)
			}
			if got := doc.PendingStats().Items; got != c.kept {
				t.Fatalf("%d structs parked, want %d", got, c.kept)
			}
			if refused.Structs != c.kept || refused.Bytes != c.kept {
				t.Fatalf("the refusal reports %d structs and %d bytes, want %d of each", refused.Structs, refused.Bytes, c.kept)
			}
		})
	}

	// A budget that holds the struct parks it, and the update that fills the
	// gap is charged for its own struct alone, though its drain lands both.
	doc := New(WithClientID(7))
	doc.SetApplyBudget(budget(1, 2))
	if err := ApplyUpdateV1(doc, parked, nil); err != nil {
		t.Fatalf("the parking update was refused under a budget that holds it: %v", err)
	}
	if got := doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked, want the one the update carried", got)
	}
	// The gap filler's own struct, and the name of the root "t" it creates.
	doc.SetApplyBudget(budget(1, 2))
	if err := ApplyUpdateV1(doc, gapStringUpdate(0, "A", nil), nil); err != nil {
		t.Fatalf("the update that fills the gap was refused: %v", err)
	}
	if got := doc.GetText("t").ToString(); got != "ABC" || doc.PendingStats().Items != 0 {
		t.Fatalf("the document reads %q with %d parked, want \"ABC\" and none", got, doc.PendingStats().Items)
	}
}

// parkedUnderRoot is an update of one string struct for client 2 at clock 1,
// parented to the named root: clock 0 is never sent, so a document that has
// not seen it parks the struct with its parent already known.
func parkedUnderRoot(root, value string) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(1) // one struct
	enc.WriteVarUint(2) // client
	enc.WriteVarUint(1) // first clock: clock 0 is the gap
	enc.WriteUint8(wireString)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString(root)
	enc.WriteVarString(value)
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// An allowed-roots list keeps a sender out of a root whether its struct
// integrates or parks: a parked struct is stored, carried in the document's
// state and integrated once its gap is filled. A struct that parks under a
// root the list names is taken.
func TestUnit_AllowedRoots_RefusesAStructThatParksUnderADisallowedRoot(t *testing.T) {
	const base = "hello world"
	doc := guardedDoc(t, base)
	doc.SetAllowedRoots(func(any) []string { return []string{"t"} })
	before := stateOf(doc)
	refused := requireRefused(t, doc, before, ApplyUpdateV1(doc, parkedUnderRoot("m", "forged"), nil), ErrRootNotAllowed)
	if refused.Root != "m" {
		t.Fatalf("the refusal reports root %q, want \"m\"", refused.Root)
	}

	allowed := guardedDoc(t, base)
	allowed.SetAllowedRoots(func(any) []string { return []string{"t"} })
	if err := ApplyUpdateV1(allowed, parkedUnderRoot("t", "later"), nil); err != nil {
		t.Fatalf("a struct parking under the allowed root was refused: %v", err)
	}
	if got := allowed.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked under the allowed root, want one", got)
	}
}

// A struct that parks before its parent is known keeps the list of the
// apply that parked it. A sender allowed only "m" writes beside an item of
// "t" that has not arrived; when a trusted peer, allowed every root, brings
// that item, the drain integrates the parked write under "t", which its
// sender may not touch, so the write is dropped. The trusted peer did not
// send it: its update applies without an error, and the document counts the
// drop for the sender's client (PendingStats.RootsDropped).
func TestUnit_AllowedRoots_AParkedWriteKeepsItsOwnSendersPermissions(t *testing.T) {
	trusted := New(WithClientID(3))
	trustedText := trusted.GetText("t")
	trusted.Transact(func(txn *Transaction) { trustedText.Insert(txn, 0, "Z", nil) })
	privateItem := EncodeStateAsUpdateV1(trusted, nil)

	sender := New(WithClientID(4))
	if err := ApplyUpdateV1(sender, privateItem, nil); err != nil {
		t.Fatal(err)
	}
	senderText := sender.GetText("t")
	sender.Transact(func(txn *Transaction) { senderText.Insert(txn, 1, "Q", nil) })
	forged := EncodeStateAsUpdateV1(sender, trusted.StateVector())

	doc := New(WithClientID(7), WithAllowedRoots(senderOnlyM))
	if err := ApplyUpdateV1(doc, forged, "sender"); err != nil {
		t.Fatalf("the write that waits on a missing item was refused at park: %v", err)
	}
	if got := doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked, want the forged write", got)
	}
	if err := ApplyUpdateV1(doc, privateItem, "trusted"); err != nil {
		t.Fatalf("the trusted update, which did not send the dropped write, returned %v", err)
	}
	stats := doc.PendingStats()
	if got := doc.GetText("t").ToString(); got != "Z" || stats.Items != 0 {
		t.Fatalf("the document reads %q with %d parked, want \"Z\" and none", got, stats.Items)
	}
	if got := stats.RootsDropped; len(got) != 1 || got[4] != 1 {
		t.Fatalf("the drops are reported as %v, want one for client 4", got)
	}
}

// senderOnlyM allows the origin "sender" the root "m" alone, and every other
// origin every root.
func senderOnlyM(origin any) []string {
	if origin == "sender" {
		return []string{"m"}
	}
	return nil
}

// The apply that fills a gap fails for a parked struct the drain drops only
// when it sent that struct. Here the sender's second edit, a write beside
// "Z" in "t", parks behind its own first, a key of "m", while the sender may
// write both roots; the owner then narrows the sender to "m". Another client,
// allowed "m" alone, forwarding the first edit fills the gap without error,
// and the drop is counted for client 4; the sender forwarding it itself gets
// ErrRootNotAllowed.
func TestUnit_AllowedRoots_TheSenderOfADroppedParkedWriteGetsTheError(t *testing.T) {
	trusted := New(WithClientID(3))
	trustedText := trusted.GetText("t")
	trusted.Transact(func(txn *Transaction) { trustedText.Insert(txn, 0, "Z", nil) })
	base := EncodeStateAsUpdateV1(trusted, nil)

	sender := New(WithClientID(4))
	if err := ApplyUpdateV1(sender, base, nil); err != nil {
		t.Fatal(err)
	}
	held := sender.StateVector()
	m, text := sender.GetMap("m"), sender.GetText("t")
	sender.Transact(func(txn *Transaction) { m.Set(txn, "a", "x") })
	first := EncodeStateAsUpdateV1(sender, held)
	afterFirst := sender.StateVector()
	sender.Transact(func(txn *Transaction) { text.Insert(txn, 1, "Q", nil) })
	second := EncodeStateAsUpdateV1(sender, afterFirst)

	for _, filler := range []string{"other", "sender"} {
		t.Run(filler, func(t *testing.T) {
			senderRoots := []string{"t", "m"}
			doc := New(WithClientID(7), WithAllowedRoots(func(origin any) []string {
				switch origin {
				case "sender":
					return senderRoots
				case "other":
					return []string{"m"}
				}
				return nil
			}))
			if err := ApplyUpdateV1(doc, base, nil); err != nil {
				t.Fatal(err)
			}
			if err := ApplyUpdateV1(doc, second, "sender"); err != nil {
				t.Fatalf("the parking update was refused: %v", err)
			}
			if got := doc.PendingStats().Items; got != 1 {
				t.Fatalf("%d structs parked, want the edit behind the gap", got)
			}
			senderRoots = []string{"m"}
			err := ApplyUpdateV1(doc, first, filler)
			stats := doc.PendingStats()
			if filler == "sender" {
				if !errors.Is(err, ErrRootNotAllowed) {
					t.Fatalf("the sender filling its own gap got %v, want ErrRootNotAllowed", err)
				}
				if len(stats.RootsDropped) != 0 {
					t.Fatalf("the drops are reported as %v, want none: the sender got the error", stats.RootsDropped)
				}
			} else {
				if err != nil {
					t.Fatalf("another client filling the gap got %v, want no error", err)
				}
				if got := stats.RootsDropped; len(got) != 1 || got[4] != 1 {
					t.Fatalf("the drops are reported as %v, want one for client 4", got)
				}
			}
			if got := doc.GetText("t").ToString(); got != "Z" || stats.Items != 0 {
				t.Fatalf("the document reads %q with %d parked, want \"Z\" and none", got, stats.Items)
			}
			if v, _ := doc.GetMap("m").Get("a"); v != "x" {
				t.Fatalf("the gap's own key holds %v, want \"x\"", v)
			}
		})
	}
}

// Bytes counts the map key an item is set under, which the document keeps for
// as long as the entry lives, and the name of a root the update creates,
// which it keeps for as long as it holds the root: both are strings their
// sender sized. A root the document already holds is not charged again.
func TestUnit_ApplyBudget_BytesCountsAMapKeyAndANewRootsName(t *testing.T) {
	const base = "hello world"
	bytesOnly := func(n int) func(any) ApplyBudget {
		return func(any) ApplyBudget { return ApplyBudget{Bytes: n} }
	}

	t.Run("a map key", func(t *testing.T) {
		long := strings.Repeat("k", 1_000)
		update := guardedEdit(t, base, 2, func(txn *Transaction, _ *YText, m *YMap) { m.Set(txn, long, "v") })
		want := len(long) + len("v")

		doc := guardedDoc(t, base)
		doc.SetApplyBudget(bytesOnly(want - 1))
		requireRefused(t, doc, stateOf(doc), ApplyUpdateV1(doc, update, nil), ErrApplyBudget)

		fits := guardedDoc(t, base)
		fits.SetApplyBudget(bytesOnly(want))
		requireApplied(t, fits, stateOf(fits), ApplyUpdateV1(fits, update, nil))
	})

	t.Run("a new root's name", func(t *testing.T) {
		name := strings.Repeat("r", 1_000)
		peer := New(WithClientID(2))
		text := peer.GetText(name)
		peer.Transact(func(txn *Transaction) { text.Insert(txn, 0, "x", nil) })
		update := EncodeStateAsUpdateV1(peer, nil)
		want := len(name) + len("x")

		doc := budgetDoc(t, base)
		doc.SetApplyBudget(bytesOnly(want - 1))
		requireRefused(t, doc, stateOf(doc), ApplyUpdateV1(doc, update, nil), ErrApplyBudget)
		if _, held := doc.share[name]; held {
			t.Fatal("the refused update left the root it named on the document")
		}

		fits := budgetDoc(t, base)
		fits.SetApplyBudget(bytesOnly(want))
		requireApplied(t, fits, stateOf(fits), ApplyUpdateV1(fits, update, nil))

		// Held already, the root costs its writer nothing more.
		held := budgetDoc(t, base)
		held.GetText(name)
		held.SetApplyBudget(bytesOnly(len("x")))
		requireApplied(t, held, stateOf(held), ApplyUpdateV1(held, update, nil))
	})
}
