package crdt

import (
	"errors"
	"fmt"
)

// ErrApplyCheck is returned, wrapped in a RefusedUpdateError with the check's
// own error, by a remote apply whose update the document's apply check
// (WithApplyCheck) refused. It wraps ErrInvalidUpdate. The document is left
// as it was.
var ErrApplyCheck = fmt.Errorf("%w: the document's apply check refused what it leaves", ErrInvalidUpdate)

// ErrApplyCheckDrain is returned, wrapped with the check's own error, by a
// remote apply whose update the apply check passed and whose drain of the
// parked queue it refused: the drain was rolled back and the update was
// applied. It is no RefusedUpdateError and does not wrap ErrInvalidUpdate.
var ErrApplyCheckDrain = errors.New("crdt: the document's apply check refused what the drain of the parked queue lands; the update was applied")

// errApplyCheckPanicked is the apply's error for a check that panicked, which
// rolls the apply back; the panic itself is raised after the commit.
var errApplyCheckPanicked = errors.New("crdt: the document's apply check panicked")

// ApplyView is the document as a remote apply has left it, for the apply
// check (WithApplyCheck) to read while the apply holds the document's lock,
// which the read methods of its types take. A view takes no lock, holds no
// transaction to write with, and panics when it reads another document's type
// or reads after the check has returned.
type ApplyView struct {
	txn *Transaction
}

// ChangedTypes is the types the apply changed so far, each the direct parent
// of an item it integrated or deleted, in no particular order: in the check of
// a drain, the update's and the drain's.
func (v *ApplyView) ChangedTypes() []SharedType {
	txn := v.live()
	types := make([]SharedType, 0, len(txn.changed))
	for t := range txn.changed {
		switch t.owner.(type) {
		case nil, *rawType:
			continue
		}
		types = append(types, t.owner)
	}
	return types
}

// TextString is t's text as the apply left it, as YText.ToString returns it.
func (v *ApplyView) TextString(t *YText) string {
	v.reads(&t.abstractType)
	return t.toStringLocked()
}

// TextDelta is t's content as the apply left it, as YText.ToDelta returns it.
func (v *ApplyView) TextDelta(t *YText) []Delta {
	v.reads(&t.abstractType)
	return t.toDeltaLocked()
}

// MapGet is m's value for key as the apply left it, as YMap.Get returns it.
func (v *ApplyView) MapGet(m *YMap, key string) (any, bool) {
	v.reads(&m.abstractType)
	return m.getLocked(key)
}

// MapEntries is m's live entries as the apply left them, as YMap.Entries
// returns them.
func (v *ApplyView) MapEntries(m *YMap) map[string]any {
	v.reads(&m.abstractType)
	return m.entriesLocked()
}

// ArraySlice is a's elements as the apply left them, as YArray.ToSlice
// returns them.
func (v *ApplyView) ArraySlice(a *YArray) []any {
	v.reads(&a.abstractType)
	return a.toSliceLocked()
}

// live is the apply's transaction, while the check that got v runs.
func (v *ApplyView) live() *Transaction {
	if v.txn == nil {
		panic("crdt: ApplyView read after the apply check it was made for returned")
	}
	return v.txn
}

// reads panics unless t is a type of the document whose check runs.
func (v *ApplyView) reads(t *abstractType) {
	if t.doc != v.live().doc {
		panic("crdt: ApplyView reads only the document its apply check runs on")
	}
}

// checkApply runs the apply check on what the update left, before the drain,
// where its refusal can still roll the whole apply back (endRemoteApply). An
// apply that changed nothing is not checked (updateLanded).
func checkApply(txn *Transaction) error {
	j := txn.doc.applying
	if j == nil || j.check == nil || !j.updateLanded(txn) {
		return nil
	}
	switch panicked, err := runCheck(txn, j.check); {
	case panicked:
		return errApplyCheckPanicked
	case err != nil:
		return fmt.Errorf("%w: %w", ErrApplyCheck, err)
	}
	return nil
}

// checkDrain runs the apply check on what the running drain landed. A refusal,
// or a panic, stops the drain, so drainParked rolls it back and parks the
// rest again as for a drain the apply budget stops, and the update stands: the
// parked structs are not its sender's. The refusal is kept for the apply to
// return once the update has committed.
func checkDrain(txn *Transaction, j *applyJournal) {
	if j.check == nil || !landedSince(txn, j.drain) {
		return
	}
	panicked, err := runCheck(txn, j.check)
	if !panicked && err == nil {
		return
	}
	if err != nil {
		j.drainRefused = fmt.Errorf("%w: %w", ErrApplyCheckDrain, err)
	}
	panic(drainStopped{})
}

// landedSince is whether the apply integrated a struct or deleted an item
// since point.
func landedSince(txn *Transaction, point *applyPoint) bool {
	if deletedSpan(txn.deleteSet) != deletedSpan(point.deleted) {
		return true
	}
	s := txn.doc.store
	for client := range s.clients {
		if s.NextClock(client) != point.sv.Clock(client) {
			return true
		}
	}
	return false
}

// updateLanded is landedSince the apply's start, asked only of the clients
// the update added a struct for (touched): no other client's clock moves
// before the drain, so it costs what the update carries, not the document.
func (j *applyJournal) updateLanded(txn *Transaction) bool {
	if deletedSpan(txn.deleteSet) != deletedSpan(j.start.deleted) {
		return true
	}
	s := txn.doc.store
	for _, client := range j.touched {
		if s.NextClock(client) != j.start.sv.Clock(client) {
			return true
		}
	}
	return false
}

// deletedSpan is the clocks ds's ranges cover.
func deletedSpan(ds DeleteSet) uint64 {
	var n uint64
	for _, ranges := range ds.clients {
		for _, r := range ranges {
			n += r.Len
		}
	}
	return n
}

// runCheck calls check with txn's origin and a view of its document. A panic
// is kept on the document, to be raised once the transaction has committed,
// and reported as panicked.
func runCheck(txn *Transaction, check func(origin any, view *ApplyView) error) (panicked bool, err error) {
	view := &ApplyView{txn: txn}
	defer func() {
		view.txn = nil
		if r := recover(); r != nil {
			txn.doc.checkPanic = r
			panicked, err = true, nil
		}
	}()
	return false, check(txn.Origin, view)
}
