package crdt

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// checkBase is the state every document and peer of the apply-check tests
// starts from, written by client 1: the text "t" with "world" in bold, the map
// "m" holding k = "v", and the XML fragment "x" holding one paragraph with a
// "level" attribute and the text "abc".
func checkBase(t *testing.T) []byte {
	t.Helper()
	w := New(WithClientID(1))
	text, m, x := w.GetText("t"), w.GetMap("m"), w.GetXmlFragment("x")
	w.Transact(func(txn *Transaction) {
		text.Insert(txn, 0, "hello world", nil)
		text.Format(txn, 6, 5, Attributes{"bold": true})
		m.Set(txn, "k", "v")
		p := NewYXmlElement("paragraph")
		x.InsertElement(txn, 0, p)
		p.SetAttribute(txn, "level", "1")
		inner := NewYXmlText()
		p.InsertText(txn, 0, inner)
		inner.Insert(txn, 0, "abc", nil)
	})
	return EncodeStateAsUpdateV1(w, nil)
}

// checkedDoc is a document of client 7 holding state, with its roots resolved
// as an owner resolves the handles its check reads, and no check yet: a test
// sets one once the document holds the state it trusts.
func checkedDoc(t *testing.T, state []byte) *Doc {
	t.Helper()
	doc := New(WithClientID(7))
	if err := ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatalf("the base state was refused: %v", err)
	}
	doc.GetText("t")
	doc.GetMap("m")
	doc.GetXmlFragment("x")
	return doc
}

// peerEdit is the V1 update of what edit writes as client on a peer holding
// state, so a document holding state takes it as a remote update of that edit
// alone.
func peerEdit(t *testing.T, state []byte, client ClientID, edit func(txn *Transaction)) []byte {
	t.Helper()
	peer := New(WithClientID(client))
	if err := ApplyUpdateV1(peer, state, nil); err != nil {
		t.Fatalf("the peer refused the state: %v", err)
	}
	sv := peer.StateVector()
	peer.Transact(edit)
	return EncodeStateAsUpdateV1(peer, sv)
}

// paragraph is the first element of the fragment "x" in txn's document.
func paragraph(txn *Transaction) *YXmlElement {
	return txn.GetXmlFragment("x").Children()[0].(*YXmlElement)
}

// byteState is what a refused apply must leave as it was: the document's
// whole state in both encodings, which holds its parked structs and parked
// deletes too (withParked), its state vector and its pending queue.
type byteState struct {
	v1, v2  []byte
	sv      string
	pending string
}

func byteStateOf(doc *Doc) byteState {
	return byteState{
		v1:      EncodeStateAsUpdateV1(doc, nil),
		v2:      EncodeStateAsUpdateV2(doc, nil),
		sv:      fmt.Sprint(doc.StateVector()),
		pending: fmt.Sprint(doc.PendingStats()),
	}
}

func (s byteState) equal(o byteState) bool {
	return bytes.Equal(s.v1, o.v1) && bytes.Equal(s.v2, o.v2) && s.sv == o.sv && s.pending == o.pending
}

// requireByteForByte requires doc to encode as before did, byte for byte.
func requireByteForByte(t *testing.T, doc *Doc, before byteState) {
	t.Helper()
	if after := byteStateOf(doc); !after.equal(before) {
		t.Fatalf("the document changed:\nbefore sv %s pending %s, %d/%d bytes\nafter  sv %s pending %s, %d/%d bytes",
			before.sv, before.pending, len(before.v1), len(before.v2), after.sv, after.pending, len(after.v1), len(after.v2))
	}
}

// firedOn counts every observer call doc makes from here on: OnUpdate,
// OnAfterTransaction, and the observers of its three roots.
func firedOn(doc *Doc) *int {
	n := new(int)
	doc.OnUpdate(func([]byte, any) { *n++ })
	doc.OnAfterTransaction(func(*Transaction) { *n++ })
	doc.GetText("t").Observe(func(YTextEvent) { *n++ })
	doc.GetMap("m").Observe(func(YMapEvent) { *n++ })
	doc.GetXmlFragment("x").Observe(func(YXmlEvent) { *n++ })
	return n
}

var errCheckRefused = errors.New("the test's check refuses it")

func refuseEverything(any, *ApplyView) error { return errCheckRefused }

func passEverything(any, *ApplyView) error { return nil }

// requireCheckRefused requires err to be the refusal of an update by the
// document's apply check: a RefusedUpdateError wrapping ErrApplyCheck,
// ErrInvalidUpdate and the check's own error.
func requireCheckRefused(t *testing.T, err error, checkErr error) {
	t.Helper()
	var refused *RefusedUpdateError
	if !errors.As(err, &refused) {
		t.Fatalf("the apply returned %v, want a *RefusedUpdateError", err)
	}
	for _, want := range []error{ErrApplyCheck, ErrInvalidUpdate, checkErr} {
		if !errors.Is(err, want) {
			t.Fatalf("the refusal %v does not wrap %v", err, want)
		}
	}
	if errors.Is(err, ErrApplyCheckDrain) {
		t.Fatalf("the refusal of the update %v reads as a refused drain", err)
	}
}

// checkEdits are the remote updates the byte-for-byte tests refuse: each
// changes the document a different way, and together they integrate, split,
// delete, format, take a format away, set an attribute and set a map key.
var checkEdits = []struct {
	name string
	edit func(txn *Transaction)
}{
	{"integrates", func(txn *Transaction) {
		txn.GetText("t").Insert(txn, 0, "X", nil)
		txn.GetText("t").Insert(txn, 8, "Y", nil) // inside a run: splits it
	}},
	{"deletes", func(txn *Transaction) { txn.GetText("t").Delete(txn, 2, 6) }},
	{"formats", func(txn *Transaction) { txn.GetText("t").Format(txn, 1, 3, Attributes{"italic": true}) }},
	{"takes a format away", func(txn *Transaction) { txn.GetText("t").Format(txn, 6, 5, Attributes{"bold": nil}) }},
	{"deletes formatted text and its markers", func(txn *Transaction) { txn.GetText("t").Delete(txn, 5, 6) }},
	{"sets an attribute", func(txn *Transaction) { paragraph(txn).SetAttribute(txn, "level", "2") }},
	{"edits an element's text", func(txn *Transaction) {
		inner := paragraph(txn).Children()[0].(*YXmlText)
		inner.Insert(txn, 1, "Z", Attributes{"code": true})
	}},
	{"sets a map key", func(txn *Transaction) { txn.GetMap("m").Set(txn, "k", "w") }},
}

// An apply the check refuses leaves the document byte for byte as it was, in
// both encodings, with its state vector and pending queue, and fires no
// observer, for an update that integrates, deletes, formats or sets an
// attribute, in either wire format; the same update applies under a check that
// passes it, so each refusal is the check's.
func TestUnit_ApplyCheck_ARefusedApplyLeavesTheDocumentByteForByte(t *testing.T) {
	base := checkBase(t)
	for _, tc := range checkEdits {
		for _, v2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/v2=%v", tc.name, v2), func(t *testing.T) {
				peer := New(WithClientID(2))
				if err := ApplyUpdateV1(peer, base, nil); err != nil {
					t.Fatal(err)
				}
				sv := peer.StateVector()
				peer.Transact(tc.edit)
				apply := func(doc *Doc) error { return ApplyUpdateV1(doc, EncodeStateAsUpdateV1(peer, sv), "peer") }
				if v2 {
					apply = func(doc *Doc) error { return ApplyUpdateV2(doc, EncodeStateAsUpdateV2(peer, sv), "peer") }
				}

				doc := checkedDoc(t, base)
				doc.SetApplyCheck(refuseEverything)
				before := byteStateOf(doc)
				fired := firedOn(doc)
				requireCheckRefused(t, apply(doc), errCheckRefused)
				requireByteForByte(t, doc, before)
				if *fired != 0 {
					t.Fatalf("the refused apply fired %d observer calls", *fired)
				}

				control := checkedDoc(t, base)
				control.SetApplyCheck(passEverything)
				controlBefore := byteStateOf(control)
				if err := apply(control); err != nil {
					t.Fatalf("the check that passes everything refused it: %v", err)
				}
				if byteStateOf(control).equal(controlBefore) {
					t.Fatal("the update changes nothing, so its refusal proves nothing")
				}
			})
		}
	}
}

// parkedPair is two updates of client 3, each typing one character at the
// end of "t": the second carries clock 1 and parks on a document without the
// first, which fills its gap.
func parkedPair(t *testing.T, base []byte) (first, second []byte) {
	t.Helper()
	peer := New(WithClientID(3))
	if err := ApplyUpdateV1(peer, base, nil); err != nil {
		t.Fatal(err)
	}
	text := peer.GetText("t")
	sv0 := peer.StateVector()
	peer.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "A", nil) })
	first = EncodeStateAsUpdateV1(peer, sv0)
	sv1 := peer.StateVector()
	peer.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), "B", nil) })
	return first, EncodeStateAsUpdateV1(peer, sv1)
}

// An update the check refuses whose apply would have drained the pending
// queue leaves the queue as it found it: the refusal comes before the drain,
// so the parked struct stays parked, byte for byte, and drains under the next
// apply that fills its gap.
func TestUnit_ApplyCheck_ARefusedApplyThatWouldDrainLeavesTheQueueByteForByte(t *testing.T) {
	base := checkBase(t)
	first, second := parkedPair(t, base)
	doc := checkedDoc(t, base)
	doc.SetApplyCheck(passEverything)
	if err := ApplyUpdateV1(doc, second, nil); err != nil {
		t.Fatal(err)
	}
	if got := doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked, want the 1 behind the gap", got)
	}

	doc.SetApplyCheck(refuseEverything)
	before := byteStateOf(doc)
	fired := firedOn(doc)
	requireCheckRefused(t, ApplyUpdateV1(doc, first, nil), errCheckRefused)
	requireByteForByte(t, doc, before)
	if *fired != 0 {
		t.Fatalf("the refused apply fired %d observer calls", *fired)
	}

	doc.SetApplyCheck(passEverything)
	if err := ApplyUpdateV1(doc, first, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := doc.GetText("t").ToString(), "hello worldAB"; got != want {
		t.Fatalf("the text is %q, want %q once the gap is filled", got, want)
	}
}

// An apply the check passes commits exactly as it does on a document with no
// check: after each update - inserts, deletes, formats, attributes, map keys,
// and a pair whose second parks and drains - the two documents encode alike
// in both formats, and their observers were handed the same updates.
func TestUnit_ApplyCheck_APassedApplyCommitsAsWithoutTheOption(t *testing.T) {
	base := checkBase(t)
	plain, checked := checkedDoc(t, base), checkedDoc(t, base)
	calls := 0
	checked.SetApplyCheck(func(any, *ApplyView) error { calls++; return nil })
	var plainUpdates, checkedUpdates [][]byte
	plain.OnUpdate(func(u []byte, _ any) { plainUpdates = append(plainUpdates, u) })
	checked.OnUpdate(func(u []byte, _ any) { checkedUpdates = append(checkedUpdates, u) })
	plainFired, checkedFired := firedOn(plain), firedOn(checked)

	updates := make([][]byte, 0, len(checkEdits)+2)
	state := base
	for i, tc := range checkEdits {
		u := peerEdit(t, state, ClientID(10+i), tc.edit)
		updates = append(updates, u)
		merged, err := MergeUpdatesV1(state, u)
		if err != nil {
			t.Fatal(err)
		}
		state = merged
	}
	first, second := parkedPair(t, state)
	updates = append(updates, second, first)

	for i, u := range updates {
		if err := ApplyUpdateV1(plain, u, "peer"); err != nil {
			t.Fatalf("update %d: the plain document refused it: %v", i, err)
		}
		if err := ApplyUpdateV1(checked, u, "peer"); err != nil {
			t.Fatalf("update %d: the checked document refused it: %v", i, err)
		}
		if !byteStateOf(checked).equal(byteStateOf(plain)) {
			t.Fatalf("update %d: the checked document encodes differently from the plain one", i)
		}
	}
	if len(plainUpdates) != len(checkedUpdates) {
		t.Fatalf("OnUpdate was handed %d updates on the checked document, %d on the plain one", len(checkedUpdates), len(plainUpdates))
	}
	for i := range plainUpdates {
		if !bytes.Equal(plainUpdates[i], checkedUpdates[i]) {
			t.Fatalf("OnUpdate update %d differs", i)
		}
	}
	if *plainFired != *checkedFired {
		t.Fatalf("observers fired %d times on the checked document, %d on the plain one", *checkedFired, *plainFired)
	}
	// One check for each update that changed the document, the parked one
	// excepted, and one more for the drain its gap's filling landed.
	if want := len(checkEdits) + 2; calls != want {
		t.Fatalf("the check ran %d times, want %d", calls, want)
	}
}

// The check is called with the apply's origin and reads, through its view,
// what the update left: the text, its delta with the update's formats, an
// element's attribute, a map's value, and the types the update changed.
func TestUnit_ApplyCheck_TheCheckSeesTheAppliedState(t *testing.T) {
	base := checkBase(t)
	update := peerEdit(t, base, 2, func(txn *Transaction) {
		txn.GetText("t").Insert(txn, 0, "> ", Attributes{"code": true})
		txn.GetMap("m").Set(txn, "k", "w")
		paragraph(txn).SetAttribute(txn, "level", "3")
	})
	doc := checkedDoc(t, base)
	text, m, x := doc.GetText("t"), doc.GetMap("m"), doc.GetXmlFragment("x")
	var (
		origin  any
		seen    string
		delta   []Delta
		value   any
		level   string
		changed []SharedType
	)
	doc.SetApplyCheck(func(o any, v *ApplyView) error {
		origin = o
		seen = v.TextString(text)
		delta = v.TextDelta(text)
		value, _ = v.MapGet(m, "k")
		level, _ = x.Children()[0].(*YXmlElement).GetAttribute("level")
		changed = v.ChangedTypes()
		return errCheckRefused
	})
	before := byteStateOf(doc)
	requireCheckRefused(t, ApplyUpdateV1(doc, update, "from-peer-2"), errCheckRefused)
	requireByteForByte(t, doc, before)

	if origin != "from-peer-2" {
		t.Fatalf("the check was called with origin %v, want the apply's", origin)
	}
	if seen != "> hello world" {
		t.Fatalf("the check read the text %q, want the update's %q", seen, "> hello world")
	}
	if len(delta) == 0 || delta[0].Insert != "> " || delta[0].Attributes["code"] != true {
		t.Fatalf("the check read the delta %+v, want the update's code span first", delta)
	}
	if value != "w" {
		t.Fatalf("the check read k = %v, want the update's w", value)
	}
	if level != "3" {
		t.Fatalf("the check read level %q, want the update's 3", level)
	}
	names := map[SharedType]bool{}
	for _, c := range changed {
		names[c] = true
	}
	if !names[text] || !names[m] {
		t.Fatalf("the check was told the update changed %v, want the text and the map among them", changed)
	}
	// The refused apply is gone: the document reads as before.
	if got := text.ToString(); got != "hello world" {
		t.Fatalf("after the refusal the text reads %q", got)
	}
}

// A check that panics refuses nothing: the apply is rolled back first, byte
// for byte and with no observer, and the panic then reaches the caller of the
// apply with its own value, once the document's lock is released, so the
// document takes the next apply.
func TestUnit_ApplyCheck_APanicIsRaisedAfterTheApplyIsRolledBack(t *testing.T) {
	base := checkBase(t)
	for _, tc := range checkEdits {
		t.Run(tc.name, func(t *testing.T) {
			update := peerEdit(t, base, 2, tc.edit)
			doc := checkedDoc(t, base)
			boom := &struct{ why string }{"the check's own panic"}
			doc.SetApplyCheck(func(any, *ApplyView) error { panic(boom) })
			before := byteStateOf(doc)
			fired := firedOn(doc)
			var applyErr error
			got := func() (r any) {
				defer func() { r = recover() }()
				applyErr = ApplyUpdateV1(doc, update, nil)
				return nil
			}()
			if got != boom {
				t.Fatalf("the apply raised %v (returned %v), want the check's panic", got, applyErr)
			}
			requireByteForByte(t, doc, before)
			if *fired != 0 {
				t.Fatalf("the rolled-back apply fired %d observer calls", *fired)
			}
			doc.SetApplyCheck(passEverything)
			if err := ApplyUpdateV1(doc, update, nil); err != nil {
				t.Fatalf("the document refused the update once the check passed it: %v", err)
			}
			if byteStateOf(doc).equal(before) {
				t.Fatal("the update changed nothing once the check passed it")
			}
		})
	}
}

// drainScenario is a document whose pending queue holds client 4's "EVIL",
// written after client 5's first character, which the document does not hold
// yet, and the three updates client 5 then sends: "y", "z" and "w", each typed
// at the end of the text.
type drainScenario struct {
	base, evil   []byte
	y, z, w      []byte
	doc, control *Doc
}

func newDrainScenario(t *testing.T) *drainScenario {
	t.Helper()
	s := &drainScenario{base: checkBase(t)}
	honest := New(WithClientID(5))
	if err := ApplyUpdateV1(honest, s.base, nil); err != nil {
		t.Fatal(err)
	}
	text := honest.GetText("t")
	typed := func(ch string) []byte {
		sv := honest.StateVector()
		honest.Transact(func(txn *Transaction) { text.Insert(txn, text.Len(), ch, nil) })
		return EncodeStateAsUpdateV1(honest, sv)
	}
	s.y = typed("y")
	attacker := New(WithClientID(4))
	for _, u := range [][]byte{s.base, s.y} {
		if err := ApplyUpdateV1(attacker, u, nil); err != nil {
			t.Fatal(err)
		}
	}
	sv := attacker.StateVector()
	evilText := attacker.GetText("t")
	attacker.Transact(func(txn *Transaction) { evilText.Insert(txn, evilText.Len(), "EVIL", nil) })
	s.evil = EncodeStateAsUpdateV1(attacker, sv)
	s.z, s.w = typed("z"), typed("w")

	s.doc = checkedDoc(t, s.base)
	if err := ApplyUpdateV1(s.doc, s.evil, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked, want EVIL behind client 5's gap", got)
	}
	// The control is the document that never heard from the attacker.
	s.control = checkedDoc(t, s.base)
	return s
}

// refuseEvil refuses a document whose text holds EVIL.
func refuseEvil(text *YText) func(any, *ApplyView) error {
	return func(_ any, v *ApplyView) error {
		if strings.Contains(v.TextString(text), "EVIL") {
			return errCheckRefused
		}
		return nil
	}
}

// requireDrainRefused requires err to report a drain the check refused while
// the update stood: ErrApplyCheckDrain with the check's error, and neither a
// RefusedUpdateError nor ErrInvalidUpdate, so a caller can tell it from a
// refusal of the update itself.
func requireDrainRefused(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrApplyCheckDrain) || !errors.Is(err, errCheckRefused) {
		t.Fatalf("the apply returned %v, want ErrApplyCheckDrain with the check's error", err)
	}
	var refused *RefusedUpdateError
	if errors.As(err, &refused) || errors.Is(err, ErrInvalidUpdate) || errors.Is(err, ErrApplyCheck) {
		t.Fatalf("the refused drain %v reads as a refusal of the update", err)
	}
}

// A drain is its own unit: an honest update whose apply would drain what
// another client parked behind its clock is checked alone and stands, and the
// drain, which the check refuses, is rolled back. The apply returns
// ErrApplyCheckDrain, which no caller mistakes for a refusal of the update;
// the document and its observers hold exactly what the control, which never
// had the parked struct, holds; and the struct stays parked.
func TestUnit_ApplyCheck_ARefusedDrainIsRolledBackAndTheUpdateStands(t *testing.T) {
	s := newDrainScenario(t)
	var docUpdates, controlUpdates [][]byte
	s.doc.OnUpdate(func(u []byte, _ any) { docUpdates = append(docUpdates, u) })
	s.control.OnUpdate(func(u []byte, _ any) { controlUpdates = append(controlUpdates, u) })
	s.doc.SetApplyCheck(refuseEvil(s.doc.GetText("t")))

	requireDrainRefused(t, ApplyUpdateV1(s.doc, s.y, nil))
	if err := ApplyUpdateV1(s.control, s.y, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := s.doc.GetText("t").ToString(), "hello worldy"; got != want {
		t.Fatalf("the text is %q, want the honest update's %q and nothing parked", got, want)
	}
	if got, want := fmt.Sprint(s.doc.StateVector()), fmt.Sprint(s.control.StateVector()); got != want {
		t.Fatalf("the state vector is %s, want the control's %s", got, want)
	}
	if got := s.doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked after the rollback, want EVIL parked again", got)
	}
	if len(docUpdates) != 1 || !bytes.Equal(docUpdates[0], controlUpdates[0]) {
		t.Fatalf("OnUpdate was handed %d updates, want the honest update's alone, as the control's", len(docUpdates))
	}
	// What the document holds beside its queue is the control's, byte for
	// byte: the queue's struct merged into the control's state is the
	// document's state.
	want, err := MergeUpdatesV1(EncodeStateAsUpdateV1(s.control, nil), s.evil)
	if err != nil {
		t.Fatal(err)
	}
	if got := EncodeStateAsUpdateV1(s.doc, nil); !bytes.Equal(got, want) {
		t.Fatalf("the document's state is not the control's with the parked struct beside it (%d bytes, want %d)", len(got), len(want))
	}
}

// The rollback parks the struct again, so each later apply drains it again
// and has it refused again: the honest update stands every time, returns
// ErrApplyCheckDrain every time, and is never refused itself. Once the check
// takes what the queue holds, the queue drains and the document is the
// control's that took the attacker's struct in the first place.
func TestUnit_ApplyCheck_ARefusedDrainRepeatsWithEachApplyThatRetriesIt(t *testing.T) {
	s := newDrainScenario(t)
	text := s.doc.GetText("t")
	calls := 0
	evil := refuseEvil(text)
	s.doc.SetApplyCheck(func(o any, v *ApplyView) error { calls++; return evil(o, v) })

	for i, step := range []struct {
		update []byte
		text   string
	}{{s.y, "hello worldy"}, {s.z, "hello worldyz"}} {
		calls = 0
		requireDrainRefused(t, ApplyUpdateV1(s.doc, step.update, nil))
		if got := text.ToString(); got != step.text {
			t.Fatalf("apply %d: the text is %q, want %q", i, got, step.text)
		}
		if got := s.doc.PendingStats().Items; got != 1 {
			t.Fatalf("apply %d: %d structs parked, want EVIL parked again", i, got)
		}
		if calls != 2 {
			t.Fatalf("apply %d: the check ran %d times, want 2: the update, then the drain", i, calls)
		}
	}

	s.doc.SetApplyCheck(passEverything)
	if err := ApplyUpdateV1(s.doc, s.w, nil); err != nil {
		t.Fatalf("the drain the check takes was refused: %v", err)
	}
	if got := s.doc.PendingStats().Items; got != 0 {
		t.Fatalf("%d structs still parked once the check took the drain", got)
	}
	took := checkedDoc(t, s.base)
	for _, u := range [][]byte{s.evil, s.y, s.z, s.w} {
		if err := ApplyUpdateV1(took, u, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := text.ToString(), took.GetText("t").ToString(); got != want {
		t.Fatalf("the text is %q, want %q as on a document that took every update", got, want)
	}
	for _, pair := range [][2]*Doc{{s.doc, took}, {took, s.doc}} {
		if held, err := DocContainsUpdateV1(pair[0], EncodeStateAsUpdateV1(pair[1], nil)); err != nil || !held {
			t.Fatalf("the documents disagree once the queue drained (held %v, err %v)", held, err)
		}
	}
}

// A check that panics on what the drain lands rolls back the drain alone:
// the update that filled the gap stands and commits, with its observers, and
// the panic then reaches the caller of the apply.
func TestUnit_ApplyCheck_APanicOnTheDrainRollsTheDrainBackThenIsRaised(t *testing.T) {
	s := newDrainScenario(t)
	text := s.doc.GetText("t")
	boom := &struct{ why string }{"the drain's panic"}
	s.doc.SetApplyCheck(func(_ any, v *ApplyView) error {
		if strings.Contains(v.TextString(text), "EVIL") {
			panic(boom)
		}
		return nil
	})
	updates := observed(s.doc)
	got := func() (r any) {
		defer func() { r = recover() }()
		_ = ApplyUpdateV1(s.doc, s.y, nil)
		return nil
	}()
	if got != boom {
		t.Fatalf("the apply raised %v, want the check's panic", got)
	}
	if got, want := text.ToString(), "hello worldy"; got != want {
		t.Fatalf("the text is %q, want the update's %q without the drain", got, want)
	}
	if got := s.doc.PendingStats().Items; got != 1 {
		t.Fatalf("%d structs parked, want EVIL parked again", got)
	}
	if *updates != 1 {
		t.Fatalf("OnUpdate was handed %d updates, want the update that stood", *updates)
	}
}

// The view reads only while its check runs and only the document the check
// runs on: a view kept past the check, and a read of another document's type
// in the check, panic. The second panics in the check, so the apply is
// rolled back and the panic reaches the caller.
func TestUnit_ApplyCheck_TheViewReadsOnlyItsDocumentWhileTheCheckRuns(t *testing.T) {
	base := checkBase(t)
	update := peerEdit(t, base, 2, func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "X", nil) })

	doc := checkedDoc(t, base)
	text := doc.GetText("t")
	var kept *ApplyView
	doc.SetApplyCheck(func(_ any, v *ApplyView) error { kept = v; return nil })
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatal(err)
	}
	requirePanics(t, "read after the apply check", func() { kept.TextString(text) })
	requirePanics(t, "read after the apply check", func() { kept.ChangedTypes() })

	other := checkedDoc(t, base)
	foreign := New(WithClientID(9)).GetText("t")
	other.SetApplyCheck(func(_ any, v *ApplyView) error { v.TextString(foreign); return nil })
	before := byteStateOf(other)
	requirePanics(t, "reads only the document", func() { _ = ApplyUpdateV1(other, update, nil) })
	requireByteForByte(t, other, before)
}

func requirePanics(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("got panic %v, want one saying %q", r, want)
		}
	}()
	f()
}

// A read after a refused apply finds the document as it was, through every
// cache the document keeps: the text, its delta, a map value, an array index
// read through the search markers, and local writes at an index and with
// formats, which use the markers and the attribute cache, encode exactly as
// on a document that never saw the refused update. The check read the
// refused state through its view first.
func TestUnit_ApplyCheck_AReadAfterARefusalSeesTheOldState(t *testing.T) {
	base := checkBase(t)
	refused := peerEdit(t, base, 2, func(txn *Transaction) {
		text := txn.GetText("t")
		text.Insert(txn, 3, "QQQ", Attributes{"bold": true})
		text.Format(txn, 0, 9, Attributes{"italic": true})
		text.Delete(txn, 10, 2)
		txn.GetMap("m").Set(txn, "k", "refused")
		txn.GetArray("a").Insert(txn, 0, []any{"r0", "r1"})
	})
	// Both documents warm their caches with the same local writes first.
	warm := func(doc *Doc) {
		text, arr := doc.GetText("t"), doc.GetArray("a")
		doc.Transact(func(txn *Transaction) {
			arr.Insert(txn, 0, []any{"a", "b", "c", "d", "e", "f"})
			text.Insert(txn, 4, "-", Attributes{"italic": true})
			text.Format(txn, 2, 6, Attributes{"code": true})
		})
		_ = arr.Get(4)
		_ = text.ToDelta()
	}
	doc, control := checkedDoc(t, base), checkedDoc(t, base)
	warm(doc)
	warm(control)
	text, m, arr := doc.GetText("t"), doc.GetMap("m"), doc.GetArray("a")
	var seen string
	doc.SetApplyCheck(func(_ any, v *ApplyView) error {
		seen = v.TextString(text)
		_ = v.TextDelta(text)
		_, _ = v.MapGet(m, "k")
		_ = v.MapEntries(m)
		_ = v.ArraySlice(arr)
		return errCheckRefused
	})
	before := byteStateOf(doc)
	requireCheckRefused(t, ApplyUpdateV1(doc, refused, nil), errCheckRefused)
	requireByteForByte(t, doc, before)
	if !strings.Contains(seen, "QQQ") {
		t.Fatalf("the check read %q, not the refused update", seen)
	}

	if got, want := text.ToString(), control.GetText("t").ToString(); got != want {
		t.Fatalf("the text reads %q, want %q", got, want)
	}
	if got, want := fmt.Sprint(text.ToDelta()), fmt.Sprint(control.GetText("t").ToDelta()); got != want {
		t.Fatalf("the delta reads %s, want %s", got, want)
	}
	if got, _ := m.Get("k"); got != "v" {
		t.Fatalf("k reads %v, want v", got)
	}
	if got, want := arr.Get(4), control.GetArray("a").Get(4); got != want {
		t.Fatalf("index 4 reads %v, want %v", got, want)
	}
	write := func(doc *Doc) {
		text, arr := doc.GetText("t"), doc.GetArray("a")
		doc.Transact(func(txn *Transaction) {
			text.Insert(txn, 5, "#", Attributes{"code": true})
			text.Format(txn, 1, 4, Attributes{"italic": nil})
			text.Insert(txn, 8, "+", nil)
			arr.Insert(txn, 3, []any{"x"})
		})
	}
	write(doc)
	write(control)
	if !byteStateOf(doc).equal(byteStateOf(control)) {
		t.Fatalf("local writes after the refusal encode differently from the control's:\n%v\n%v",
			text.ToDelta(), control.GetText("t").ToDelta())
	}
}

// The check runs only for an apply that changes the document: an update the
// document already holds and one that only parks are not checked, and the
// check reads its document once per apply as the apply begins.
func TestUnit_ApplyCheck_ItIsNotCalledForAnApplyThatChangedNothing(t *testing.T) {
	base := checkBase(t)
	first, second := parkedPair(t, base)
	doc := checkedDoc(t, base)
	calls := 0
	doc.SetApplyCheck(func(any, *ApplyView) error { calls++; return nil })
	if err := ApplyUpdateV1(doc, base, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("an update the document held was checked %d times", calls)
	}
	if err := ApplyUpdateV1(doc, second, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || doc.PendingStats().Items != 1 {
		t.Fatalf("an update that only parked was checked %d times (%d parked)", calls, doc.PendingStats().Items)
	}
	if err := ApplyUpdateV1(doc, first, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("the update that filled the gap and its drain were checked %d times, want 2", calls)
	}
	// A local transaction is never checked.
	doc.Transact(func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "L", nil) })
	if calls != 2 {
		t.Fatalf("a local transaction was checked (%d calls)", calls)
	}
}

// A document with a check applies remote updates whole or not at all with no
// other option: it refuses a remote update carrying a subdocument, which a
// rollback could not undo, and applies it once the check is removed.
func TestUnit_ApplyCheck_ADocumentWithACheckAppliesWholeOrNothing(t *testing.T) {
	doc := New(WithClientID(7), WithApplyCheck(passEverything))
	peer := New(WithClientID(2))
	peer.Transact(func(txn *Transaction) { txn.GetMap("m").Set(txn, "sub", New(WithGUID("sub"))) })
	update := EncodeStateAsUpdateV1(peer, nil)
	err := ApplyUpdateV1(doc, update, nil)
	var refused *RefusedUpdateError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "takes no remote move or subdocument") {
		t.Fatalf("a remote subdocument was applied with err %v, want it refused", err)
	}
	doc.SetApplyCheck(nil)
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatalf("with the check removed, the subdocument was refused: %v", err)
	}
}

// Over generated updates - inserts with and without formats, deletes, format
// changes, attribute and map writes, in both wire formats, and pairs whose
// second parks until the first drains it - a refused apply leaves the
// document byte for byte as it was and tells no observer, and a passed apply
// leaves it byte for byte as a document with no check.
func TestUnit_ApplyCheck_GeneratedUpdatesRefuseByteForByte(t *testing.T) {
	const iterations = 240
	rng := rand.New(rand.NewPCG(179, 2544))
	base := checkBase(t)
	doc, control := checkedDoc(t, base), checkedDoc(t, base)
	fired := firedOn(doc)
	calls := 0
	refuse := func(any, *ApplyView) error { calls++; return errCheckRefused }
	pass := func(any, *ApplyView) error { return nil }

	letters := []string{"a", "b", "cd", "efg", "\n", "é"}
	formats := []Attributes{nil, {"bold": true}, {"italic": true}, {"bold": nil}, {"code": true, "bold": true}}
	edit := func(txn *Transaction) {
		for range 1 + rng.IntN(3) {
			text := txn.GetText("t")
			n := text.Len()
			switch rng.IntN(7) {
			case 0, 1:
				text.Insert(txn, rng.IntN(n+1), letters[rng.IntN(len(letters))], formats[rng.IntN(len(formats))])
			case 2:
				if n > 0 {
					i := rng.IntN(n)
					text.Delete(txn, i, 1+rng.IntN(n-i))
				}
			case 3:
				if n > 0 {
					i := rng.IntN(n)
					text.Format(txn, i, 1+rng.IntN(n-i), formats[1+rng.IntN(len(formats)-1)])
				}
			case 4:
				txn.GetMap("m").Set(txn, []string{"k", "j"}[rng.IntN(2)], rng.IntN(100))
			case 5:
				paragraph(txn).SetAttribute(txn, "level", fmt.Sprint(rng.IntN(6)))
			case 6:
				inner := paragraph(txn).Children()[0].(*YXmlText)
				inner.Insert(txn, rng.IntN(inner.Len()+1), letters[rng.IntN(len(letters))], formats[rng.IntN(len(formats))])
			}
		}
	}
	encode := func(peer *Doc, sv StateVector, v2 bool) func(*Doc) error {
		if v2 {
			u := EncodeStateAsUpdateV2(peer, sv)
			return func(d *Doc) error { return ApplyUpdateV2(d, u, nil) }
		}
		u := EncodeStateAsUpdateV1(peer, sv)
		return func(d *Doc) error { return ApplyUpdateV1(d, u, nil) }
	}
	// refuseThenPass applies update to doc under a check that refuses it,
	// requiring the refusal to change nothing, then under one that passes
	// it, beside the control.
	refuseThenPass := func(i int, apply func(*Doc) error) {
		t.Helper()
		doc.SetApplyCheck(refuse)
		before := byteStateOf(doc)
		firedBefore, callsBefore := *fired, calls
		err := apply(doc)
		if calls == callsBefore {
			// An update that changes nothing is not checked, and leaves
			// nothing to refuse; its transaction still ends, as any does.
			if err != nil {
				t.Fatalf("iteration %d: an unchecked apply failed: %v", i, err)
			}
		} else {
			requireCheckRefused(t, err, errCheckRefused)
			if *fired != firedBefore {
				t.Fatalf("iteration %d: the refused apply fired %d observer calls", i, *fired-firedBefore)
			}
		}
		requireByteForByte(t, doc, before)
		doc.SetApplyCheck(pass)
		if err := apply(doc); err != nil {
			t.Fatalf("iteration %d: the passed apply failed: %v", i, err)
		}
		if err := apply(control); err != nil {
			t.Fatalf("iteration %d: the control refused the update: %v", i, err)
		}
		if !byteStateOf(doc).equal(byteStateOf(control)) {
			t.Fatalf("iteration %d: the passed apply left the document unlike the control", i)
		}
	}

	refusals := 0
	for i := range iterations {
		peer := New(WithClientID(ClientID(100 + i)))
		if err := ApplyUpdateV1(peer, EncodeStateAsUpdateV1(control, nil), nil); err != nil {
			t.Fatal(err)
		}
		v2 := rng.IntN(2) == 0
		sv0 := peer.StateVector()
		peer.Transact(edit)
		if rng.IntN(3) > 0 {
			callsBefore := calls
			refuseThenPass(i, encode(peer, sv0, v2))
			if calls > callsBefore {
				refusals++
			}
			continue
		}
		// A pair: the second update parks on both documents, and the first,
		// which fills its gap and drains it, is refused and then passed.
		first := encode(peer, sv0, v2)
		sv1 := peer.StateVector()
		peer.Transact(edit)
		second := encode(peer, sv1, v2)
		doc.SetApplyCheck(pass)
		if err := second(doc); err != nil {
			t.Fatalf("iteration %d: the parked half failed: %v", i, err)
		}
		if err := second(control); err != nil {
			t.Fatalf("iteration %d: the control's parked half failed: %v", i, err)
		}
		callsBefore := calls
		refuseThenPass(i, first)
		if calls > callsBefore {
			refusals++
		}
	}
	if refusals < iterations/2 {
		t.Fatalf("only %d of %d generated updates reached the check", refusals, iterations)
	}
}

// The check is called for every apply that changes the document, whichever
// of the document's many clients holds the change, and sees the change: an
// update of the whole state whose one new struct is a held client's, one
// whose only struct is a GC struct, which changes no type, one from a client
// the document never saw, a delete alone, and one that lands for three
// clients at once, in both wire formats. Only the delete deletes anything,
// so the others show only in the clocks of the clients they touched, and
// whether an apply changed anything is asked only of the clients it added
// structs for (updateLanded): each must be among them. An update the
// document holds whole, across all its clients, is still not checked.
func TestUnit_ApplyCheck_ItSeesEveryClientAnApplyTouches(t *testing.T) {
	state := manyClientsState(t, checkBase(t), 100, 40)
	// asWriter is the update of what a peer holding state writes as client,
	// a client state may already hold: the whole state then, or only what
	// edit wrote.
	asWriter := func(client ClientID, whole bool, edit func(peer *Doc, txn *Transaction)) []byte {
		t.Helper()
		peer := New()
		if err := ApplyUpdateV1(peer, state, nil); err != nil {
			t.Fatal(err)
		}
		sv := peer.StateVector()
		peer.setClientID(client)
		peer.Transact(func(txn *Transaction) { edit(peer, txn) })
		if whole {
			sv = nil
		}
		return EncodeStateAsUpdateV1(peer, sv)
	}
	cases := []struct {
		name   string
		update []byte
		sees   func(v *ApplyView, text *YText, m *YMap) bool
	}{
		{
			"the one new struct of a held client, in the whole state",
			asWriter(120, true, func(_ *Doc, txn *Transaction) { txn.GetMap("m").Set(txn, "k120", "again") }),
			func(v *ApplyView, _ *YText, m *YMap) bool { got, _ := v.MapGet(m, "k120"); return got == "again" },
		},
		{
			"a GC struct alone, which changes no type",
			// Client 99, clock 0: one GC struct of length 1, and no deletes.
			[]byte{1, 1, 99, 0, 0, 1, 0},
			func(v *ApplyView, _ *YText, _ *YMap) bool { return len(v.ChangedTypes()) == 0 },
		},
		{
			"a client the document never saw",
			asWriter(300, false, func(_ *Doc, txn *Transaction) { txn.GetText("t").Insert(txn, 0, "Q", nil) }),
			func(v *ApplyView, text *YText, _ *YMap) bool { return strings.HasPrefix(v.TextString(text), "Q") },
		},
		{
			"a delete alone",
			asWriter(301, false, func(_ *Doc, txn *Transaction) { txn.GetText("t").Delete(txn, 0, 1) }),
			func(v *ApplyView, text *YText, _ *YMap) bool { return v.TextString(text) == "ello world" },
		},
		{
			"three clients at once",
			asWriter(105, false, func(peer *Doc, txn *Transaction) {
				m := txn.GetMap("m")
				m.Set(txn, "k105", "x")
				peer.setClientID(139)
				m.Set(txn, "k139", "y")
				peer.setClientID(302)
				txn.GetText("t").Insert(txn, 0, "R", nil)
			}),
			func(v *ApplyView, text *YText, m *YMap) bool {
				x, _ := v.MapGet(m, "k105")
				y, _ := v.MapGet(m, "k139")
				return x == "x" && y == "y" && strings.HasPrefix(v.TextString(text), "R")
			},
		},
	}
	for _, c := range cases {
		for _, v2 := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/v2=%t", c.name, v2), func(t *testing.T) {
				doc := checkedDoc(t, state)
				text, m := doc.GetText("t"), doc.GetMap("m")
				calls, saw := 0, false
				doc.SetApplyCheck(func(_ any, v *ApplyView) error {
					calls++
					saw = c.sees(v, text, m)
					return nil
				})
				apply := func() error { return ApplyUpdateV1(doc, c.update, nil) }
				if v2 {
					u, err := UpdateV1ToV2(c.update)
					if err != nil {
						t.Fatal(err)
					}
					apply = func() error { return ApplyUpdateV2(doc, u, nil) }
				}
				if err := apply(); err != nil {
					t.Fatal(err)
				}
				if calls != 1 || !saw {
					t.Fatalf("the check was called %d times and saw the change: %t; want once, seeing it", calls, saw)
				}
				// Held now, the update and the whole state change nothing.
				if err := apply(); err != nil {
					t.Fatal(err)
				}
				if err := ApplyUpdateV1(doc, state, nil); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("an update the document held whole was checked (%d calls)", calls)
				}
			})
		}
	}
}

// A drain that finds something parked is still judged by what it landed: a
// parked struct the update releases, and a parked delete alone, which the
// update's struct lets land, each have the drain checked and, refused,
// rolled back while the update stands. A drain that finds the queue and the
// pending deletes empty, or holding only what the update does not release,
// lands nothing, and the apply is checked once, for the update.
func TestUnit_ApplyCheck_ADrainThatFindsSomethingParkedIsStillChecked(t *testing.T) {
	t.Run("a parked struct the update releases", func(t *testing.T) {
		s := newDrainScenario(t)
		calls, evil := 0, refuseEvil(s.doc.GetText("t"))
		s.doc.SetApplyCheck(func(origin any, v *ApplyView) error { calls++; return evil(origin, v) })
		requireDrainRefused(t, ApplyUpdateV1(s.doc, s.y, nil))
		if calls != 2 {
			t.Fatalf("the update and its drain were checked %d times, want 2", calls)
		}
		if got, want := s.doc.GetText("t").ToString(), "hello worldy"; got != want {
			t.Fatalf("the text is %q, want %q with the drain rolled back", got, want)
		}
		if got := s.doc.PendingStats().Items; got != 1 {
			t.Fatalf("%d structs parked, want EVIL parked again", got)
		}
	})
	t.Run("a parked delete the update lets land", func(t *testing.T) {
		base := checkBase(t)
		y := peerEdit(t, base, 5, func(txn *Transaction) { txn.GetText("t").Insert(txn, 11, "y", nil) })
		holder := New(WithClientID(6))
		for _, u := range [][]byte{base, y} {
			if err := ApplyUpdateV1(holder, u, nil); err != nil {
				t.Fatal(err)
			}
		}
		sv := holder.StateVector()
		holder.Transact(func(txn *Transaction) { txn.GetText("t").Delete(txn, 11, 1) })
		deleteY := EncodeStateAsUpdateV1(holder, sv)

		doc := checkedDoc(t, base)
		text := doc.GetText("t")
		calls := 0
		doc.SetApplyCheck(func(_ any, v *ApplyView) error {
			calls++
			if !strings.Contains(v.TextString(text), "y") {
				return errCheckRefused
			}
			return nil
		})
		if err := ApplyUpdateV1(doc, deleteY, nil); err != nil {
			t.Fatal(err)
		}
		if stats := doc.PendingStats(); calls != 0 || stats.Items != 0 || stats.DeleteRanges != 1 {
			t.Fatalf("a delete of what the document lacks was checked %d times and left %+v parked, want no check and one range", calls, stats)
		}
		requireDrainRefused(t, ApplyUpdateV1(doc, y, nil))
		if calls != 2 {
			t.Fatalf("the update and its drain were checked %d times, want 2", calls)
		}
		if got, want := text.ToString(), "hello worldy"; got != want {
			t.Fatalf("the text is %q, want %q with the drain's delete rolled back", got, want)
		}
		if got := doc.PendingStats().DeleteRanges; got != 1 {
			t.Fatalf("%d delete ranges parked, want the one parked again", got)
		}
	})
	t.Run("an empty queue", func(t *testing.T) {
		base := checkBase(t)
		doc := checkedDoc(t, base)
		calls := 0
		doc.SetApplyCheck(func(any, *ApplyView) error { calls++; return nil })
		typed := peerEdit(t, base, 5, func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "y", nil) })
		if err := ApplyUpdateV1(doc, typed, nil); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("an apply with nothing parked was checked %d times, want once", calls)
		}
	})
	t.Run("a parked struct the update does not release", func(t *testing.T) {
		s := newDrainScenario(t)
		calls := 0
		s.doc.SetApplyCheck(func(any, *ApplyView) error { calls++; return nil })
		typed := peerEdit(t, s.base, 6, func(txn *Transaction) { txn.GetText("t").Insert(txn, 0, "Q", nil) })
		if err := ApplyUpdateV1(s.doc, typed, nil); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || s.doc.PendingStats().Items != 1 {
			t.Fatalf("the apply was checked %d times with %d structs parked, want once and EVIL still parked", calls, s.doc.PendingStats().Items)
		}
	})
}
