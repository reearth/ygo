package crdt

// reportApplyRecovered makes the report of txn's remote apply (reportApply)
// with a panic of the callback returned rather than raised, for the commit to
// raise once the document is unlocked.
func reportApplyRecovered(txn *Transaction, t *applyTally) (panicked any) {
	defer func() { panicked = recover() }()
	reportApply(txn, t)
	return nil
}

// reportApply makes the report of txn's remote apply, whose tally is t
// (WithApplyReport), under the document lock at the end of the commit, after
// its merges have run: the structs the document holds, read here as the
// store's and the parked queue's, and the apply's own counts.
func reportApply(txn *Transaction, t *applyTally) {
	d := txn.doc
	held := d.store.pending.heldStructs()
	for _, items := range d.store.clients {
		held += len(items)
	}
	d.applyReport(txn.Origin, ApplyReport{
		DocStructs: held,
		Integrated: t.structs,
		Bytes:      t.bytes,
		Split:      t.split,
		Decoded:    t.decoded,
	})
}
