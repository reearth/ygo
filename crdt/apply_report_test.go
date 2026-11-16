package crdt

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// Tests for the report a committed apply makes (WithApplyReport): the structs
// the document is left holding, and what the apply added of its own.

// reports is what a document's report callback was called with, in order.
type reports struct {
	origins []any
	made    []ApplyReport
}

func (r *reports) take(origin any, report ApplyReport) {
	r.origins = append(r.origins, origin)
	r.made = append(r.made, report)
}

// only is the one report the applies made, and fails for any other number.
func (r *reports) only(t *testing.T) ApplyReport {
	t.Helper()
	if len(r.made) != 1 {
		t.Fatalf("%d reports were made, want one: %+v", len(r.made), r.made)
	}
	return r.made[0]
}

// structsHeld is the structs doc holds and has parked, read from outside the
// apply as an owner reads them.
func structsHeld(doc *Doc) int {
	return storeStructs(doc) + doc.PendingStats().Items
}

// parkedStructs is an update of n one-character structs for client 2 at
// clocks 1 to n, each naming the one before it as its left origin. Clock 0 is
// never sent, so a document that has not seen it parks every one of them.
func parkedStructs(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(2) // client
	enc.WriteVarUint(1) // first clock: clock 0 is the gap
	for i := range n {
		enc.WriteUint8(wireString | flagHasOrigin)
		enc.WriteVarUint(2)         // the left origin's client
		enc.WriteVarUint(uint64(i)) // its clock, the struct before this one
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// parkedSplitters is an update of n one-character structs for client 2 at
// clocks 1 to n, each between two neighbouring clocks of the run client 1
// wrote: clock 0 of client 2 is never sent, so every one of them parks, and
// the drain that lands them cuts that run into n+1 pieces that stay cut.
func parkedSplitters(n int) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteVarUint(2) // client
	enc.WriteVarUint(1) // first clock: clock 0 is the gap
	for i := range n {
		enc.WriteUint8(wireString | flagHasOrigin | flagHasRightOrigin)
		enc.WriteVarUint(1)         // the left origin's client: the run's writer
		enc.WriteVarUint(uint64(i)) // a different clock inside the run for each
		enc.WriteVarUint(1)         // the right origin's client
		enc.WriteVarUint(uint64(i + 1))
		enc.WriteVarString("x")
	}
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// applyEither applies update to doc as V1 or as its V2 re-encoding.
func applyEither(t *testing.T, doc *Doc, update []byte, v2 bool) error {
	t.Helper()
	if !v2 {
		return ApplyUpdateV1(doc, update, nil)
	}
	asV2, err := UpdateV1ToV2(update)
	if err != nil {
		t.Fatalf("re-encode the update as V2: %v", err)
	}
	return ApplyUpdateV2(doc, asV2, nil)
}

// requireReportMatches requires report to carry the counts of refused, the
// error of the same apply.
func requireReportMatches(t *testing.T, report ApplyReport, refused *RefusedUpdateError) {
	t.Helper()
	if report.Integrated != refused.Structs || report.Bytes != refused.Bytes ||
		report.Split != refused.Split || report.Decoded != refused.Decoded {
		t.Fatalf("the report adds %+v, want the counts of the apply's error: "+
			"%d structs, %d bytes, %d splits, %d decoded",
			report, refused.Structs, refused.Bytes, refused.Split, refused.Decoded)
	}
}

// An apply that commits reports the structs the document holds, the ones
// parked in the pending queue included, and what it added: five structs the
// apply parked leave the store at the one struct of the base text and the
// report at six. The same update under a budget one struct short keeps the
// four structs that fit, and the apply reports those four, with the counts
// its error carries.
func TestUnit_ApplyReport_ReportsTheStructsHeldParkedIncludedAndTheApplysCounts(t *testing.T) {
	const base, n = "hello world", 5
	update := parkedStructs(n)

	var made reports
	doc := contentDoc(t, base, false)
	doc.SetApplyReport(made.take)
	if err := ApplyUpdateV1(doc, update, "peer"); err != nil {
		t.Fatalf("the update was refused: %v", err)
	}
	report := made.only(t)
	if made.origins[0] != "peer" {
		t.Fatalf("the report names origin %v, want the apply's \"peer\"", made.origins[0])
	}
	if got := storeStructs(doc); got != 1 {
		t.Fatalf("the store holds %d structs, want the one run of the base text", got)
	}
	if got := doc.PendingStats().Items; got != n {
		t.Fatalf("%d structs are parked, want the %d the update carried", got, n)
	}
	if report.DocStructs != 1+n || report.DocStructs != structsHeld(doc) {
		t.Fatalf("the report counts %d structs, want the one in the store and the %d parked", report.DocStructs, n)
	}
	if report.Integrated != n || report.Bytes != n || report.Decoded != n || report.Split != 0 {
		t.Fatalf("the report adds %+v, want the %d one-byte structs the update parked, read as %d values and splitting nothing",
			report, n, n)
	}

	var shortMade reports
	short := contentDoc(t, base, false)
	short.SetApplyReport(shortMade.take)
	short.SetApplyBudget(func(any) ApplyBudget {
		return ApplyBudget{Structs: n - 1}
	})
	err := ApplyUpdateV1(short, update, "peer")
	var refused *RefusedUpdateError
	if !errors.Is(err, ErrApplyBudget) || !errors.As(err, &refused) {
		t.Fatalf("the update was applied with err %v, want ErrApplyBudget in a RefusedUpdateError", err)
	}
	kept := shortMade.only(t)
	requireReportMatches(t, kept, refused)
	if kept.Integrated != n-1 || kept.Bytes != n-1 {
		t.Fatalf("the refused apply reports %+v, want the %d structs it kept", kept, n-1)
	}
	if got := short.PendingStats().Items; got != n-1 || kept.DocStructs != 1+n-1 || kept.DocStructs != structsHeld(short) {
		t.Fatalf("%d structs are parked and the report counts %d held, want %d parked and %d held",
			got, kept.DocStructs, n-1, 1+n-1)
	}
}

// The struct total is what the document holds once the apply has committed,
// not the structs the apply linked: the apply carries one character, its
// drain lands the five its sender parked earlier, and the commit squashes all
// six into one run beside the base text.
func TestUnit_ApplyReport_TheTotalIsWhatTheCommitLeavesTheDocumentHolding(t *testing.T) {
	const base, n = "hello world", 5
	doc := contentDoc(t, base, false)
	if err := ApplyUpdateV1(doc, parkedStructs(n), "parker"); err != nil {
		t.Fatalf("the parking update was refused: %v", err)
	}

	var made reports
	doc.SetApplyReport(made.take)
	if err := ApplyUpdateV1(doc, gapStringUpdate(0, "A", nil), "filler"); err != nil {
		t.Fatalf("the update that fills the gap was refused: %v", err)
	}
	report := made.only(t)
	if got := doc.PendingStats().Items; got != 0 {
		t.Fatalf("%d structs are still parked, want the drain to have taken them all", got)
	}
	if got := storeStructs(doc); got != 2 {
		t.Fatalf("the store holds %d structs, want the base run and the one run the commit squashed "+
			"the fill and the %d structs its drain landed into", got, n)
	}
	if report.DocStructs != structsHeld(doc) {
		t.Fatalf("the report counts %d structs, want the %d the document is left holding", report.DocStructs, structsHeld(doc))
	}
	if report.Integrated != 1 || report.Bytes != 1 {
		t.Fatalf("the report adds %d structs of %d bytes, want the one character the update carried: "+
			"what its drain landed was counted by the apply that parked it",
			report.Integrated, report.Bytes)
	}
}

// A refused apply reports what the document kept of it. Without
// whole-or-nothing applies, three inserts under Structs: 2 keep the first two,
// and the apply reports them with the counts its error carries. With
// whole-or-nothing applies the same refusal leaves the document as it was
// and makes no report, and neither does an update refused before anything of
// it is read. The same update under a budget that holds it reports the three.
func TestUnit_ApplyReport_ARefusedApplyReportsWhatItKept(t *testing.T) {
	const base = "hello world"
	update := threeInserts(t, base)
	twoStructs := func(any) ApplyBudget { return ApplyBudget{Structs: 2} }

	var keptMade reports
	keeps := contentDoc(t, base, false)
	keeps.SetApplyReport(keptMade.take)
	keeps.SetApplyBudget(twoStructs)
	err := ApplyUpdateV1(keeps, update, "peer")
	var refused *RefusedUpdateError
	if !errors.Is(err, ErrApplyBudget) || !errors.As(err, &refused) {
		t.Fatalf("the update was applied with err %v, want ErrApplyBudget in a RefusedUpdateError", err)
	}
	kept := keptMade.only(t)
	requireReportMatches(t, kept, refused)
	if kept.Integrated != 2 || kept.Bytes != 2 {
		t.Fatalf("the refused apply reports %+v, want the two one-character inserts it kept", kept)
	}
	if got, want := keeps.GetText("t").ToString(), "ahelblo world"; got != want {
		t.Fatalf("the refused apply left %q, want %q: its first two inserts", got, want)
	}
	if kept.DocStructs != structsHeld(keeps) {
		t.Fatalf("the report counts %d structs, want the %d the document holds", kept.DocStructs, structsHeld(keeps))
	}

	var made reports
	doc := budgetDoc(t, base)
	doc.SetApplyReport(made.take)
	doc.SetApplyBudget(twoStructs)
	before := stateOf(doc)
	requireRefused(t, doc, before, ApplyUpdateV1(doc, update, "peer"), ErrApplyBudget)
	if len(made.made) != 0 {
		t.Fatalf("the apply whole-or-nothing rolled back reported %+v, want nothing", made.made)
	}

	for _, d := range []*Doc{keeps, doc} {
		if err := ApplyUpdateV1(d, []byte{0xff, 0xff, 0xff}, "peer"); err == nil {
			t.Fatal("unreadable bytes were applied")
		}
	}
	if len(made.made) != 0 || len(keptMade.made) != 1 {
		t.Fatalf("an unreadable update reported %+v and %+v, want nothing", made.made, keptMade.made[1:])
	}

	doc.SetApplyBudget(func(any) ApplyBudget {
		return ApplyBudget{Structs: 3}
	})
	requireApplied(t, doc, before, ApplyUpdateV1(doc, update, "peer"))
	report := made.only(t)
	if report.Integrated != 3 || report.Bytes != 3 {
		t.Fatalf("the applied update reports %+v, want its three one-character inserts", report)
	}
	if report.DocStructs != structsHeld(doc) {
		t.Fatalf("the report counts %d structs, want the %d the document holds", report.DocStructs, structsHeld(doc))
	}
}

// The splits a drain of the pending queue makes are the landing apply's, so
// they are in its report: ten parked characters, each between two clocks of
// the run the base text is held in, cut that run into eleven pieces when the
// gap is filled. A drain the split budget stops keeps what it landed, and
// the report counts the splits the document kept: nine of them, or none
// where whole-or-nothing applies take the drain back.
func TestUnit_ApplyReport_ADrainsSplitsAreInTheReport(t *testing.T) {
	const base, n = "hello world", 10 // ten interior clocks of the run
	fill := gapStringUpdate(0, "A", nil)

	for name, v2 := range map[string]bool{"V1": false, "V2": true} {
		t.Run(name, func(t *testing.T) {
			doc := contentDoc(t, base, false)
			if err := ApplyUpdateV1(doc, parkedSplitters(n), nil); err != nil {
				t.Fatalf("the parking update was refused: %v", err)
			}
			var made reports
			doc.SetApplyReport(made.take)
			if err := applyEither(t, doc, fill, v2); err != nil {
				t.Fatalf("the update that fills the gap was refused: %v", err)
			}
			report := made.only(t)
			if report.Split != n {
				t.Fatalf("the report counts %d splits, want the %d the drain cut the run into pieces by", report.Split, n)
			}
			if report.Integrated != 1 || report.Bytes != 1 {
				t.Fatalf("the report adds %d structs of %d bytes, want the one character the fill carried",
					report.Integrated, report.Bytes)
			}
			// The run in n+1 pieces, the fill, and the n structs the drain
			// landed, none of which merges back.
			if want := (n + 1) + 1 + n; report.DocStructs != want || structsHeld(doc) != want {
				t.Fatalf("the report counts %d structs and the document holds %d, want %d",
					report.DocStructs, structsHeld(doc), want)
			}
		})
	}

	splitShort := func(any) ApplyBudget { return ApplyBudget{Structs: 1, Bytes: 1, Split: n - 1} }
	for _, c := range []struct {
		name           string
		wholeOrNothing bool
		split, parked  int
	}{
		{"keeps what it landed", false, n - 1, 1},
		{"whole or nothing", true, 0, n},
	} {
		t.Run("a drain the budget stops/"+c.name, func(t *testing.T) {
			doc := contentDoc(t, base, c.wholeOrNothing)
			if err := ApplyUpdateV1(doc, parkedSplitters(n), nil); err != nil {
				t.Fatalf("the parking update was refused: %v", err)
			}
			var made reports
			doc.SetApplyReport(made.take)
			doc.SetApplyBudget(splitShort)
			if err := ApplyUpdateV1(doc, fill, nil); err != nil {
				t.Fatalf("the fill was refused for a drain that is not its own: %v", err)
			}
			report := made.only(t)
			if got := doc.PendingStats().Items; got != c.parked {
				t.Fatalf("%d structs are parked, want %d", got, c.parked)
			}
			if report.Split != c.split {
				t.Fatalf("the report counts %d splits, want the %d the document kept", report.Split, c.split)
			}
			if report.DocStructs != structsHeld(doc) {
				t.Fatalf("the report counts %d structs, want the %d the document holds", report.DocStructs, structsHeld(doc))
			}
		})
	}
}

// A callback that only watches changes nothing an apply does: an update that
// sets a plain key and a subdocument applies whole, as on a document without
// the option, and is reported once.
func TestUnit_ApplyReport_ADocumentThatOnlyWatchesAppliesEveryUpdate(t *testing.T) {
	peer := New(WithClientID(2))
	m := peer.GetMap("m")
	peer.Transact(func(txn *Transaction) {
		m.Set(txn, "k", "v")
		m.Set(txn, "sub", New(WithGUID("sub")))
	})
	update := EncodeStateAsUpdateV1(peer, nil)

	var made reports
	doc := New(WithApplyReport(made.take))
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
	if report := made.only(t); report.Integrated != 2 || report.DocStructs != structsHeld(doc) {
		t.Fatalf("the report is %+v, want the two structs the update added and the %d the document holds",
			report, structsHeld(doc))
	}
}

// A report callback that panics must not skip the commit's formatting cleanup.
// The panic reaches the caller, the lock is released, and a later plain
// insert renders without the format of a run the apply deleted: B holds a
// live format and applies A's delete of the run it formats, which is an
// apply the cleanup reconciles.
func TestUnit_ApplyReport_APanickingCallbackDoesNotSkipCommitCleanup(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "callback returns", true: "callback panics"}[panics], func(t *testing.T) {
			a, b := newTestDoc(1), newTestDoc(2)
			at, bt := a.GetText("t"), b.GetText("t")
			a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "abc", nil) })
			syncText(t, a, b, nil)
			a.Transact(func(txn *Transaction) { at.Delete(txn, 0, 3) })
			b.Transact(func(txn *Transaction) { bt.Format(txn, 2, 1, Attributes{"bold": true}) })

			b.SetApplyReport(func(any, ApplyReport) {
				if panics {
					panic("report")
				}
			})
			got := func() (p any) {
				defer func() { p = recover() }()
				syncText(t, a, b, "remote")
				return nil
			}()
			if panics && got != "report" {
				t.Fatalf("a panicking callback did not propagate: got %v", got)
			}
			if !panics && got != nil {
				t.Fatalf("a returning callback panicked: %v", got)
			}
			if !b.mu.TryLock() {
				t.Fatal("the document lock was held after the report")
			}
			b.mu.Unlock()

			b.SetApplyReport(nil)
			a.Transact(func(txn *Transaction) { at.Insert(txn, 0, "x", nil) })
			syncText(t, a, b, nil)
			want := []Delta{{Op: DeltaOpInsert, Insert: "x"}}
			if !reflect.DeepEqual(bt.ToDelta(), want) {
				t.Fatalf("a callback panic corrupted later formatting: delta=%+v want=%+v", bt.ToDelta(), want)
			}
		})
	}
}
