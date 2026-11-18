package crdt

// ApplyOutcome is what one apply of ApplyUpdateV1Outcome did with its update.
type ApplyOutcome struct {
	// Integrated is the commit's own update: the bytes the document's
	// OnUpdate observers are handed, the very slice when there are any, so
	// the caller must not modify it. It carries what the apply integrated
	// and deleted, what the drain of the pending queue landed with it
	// included, and nothing that parked. It is nil when the commit changed
	// nothing: the document already held all of the update, or the update
	// only parked.
	Integrated []byte
	// Parked is whether the apply left some of the update outside the
	// document's store: a struct the update carries, or a clock it deletes,
	// past the clocks the store holds of its client. Such a struct waits in
	// the pending queue, parked or merged into a struct the queue held. What
	// the document already held, and parked content the apply's own drain
	// landed, the store holds. It is false for an apply that returned an
	// error.
	Parked bool
	// Held is whether the document already held all of the update, so the
	// apply changed and parked nothing (ApplyUpdateV1Held).
	Held bool
	// Landed is, for an apply that returned an error, what
	// ApplyUpdateV1Landed returns: what the call left in the document, the
	// structs and deletions it parked included; nil otherwise.
	Landed []byte
}

// ApplyUpdateV1Outcome is ApplyUpdateV1Held that also reports what the
// commit integrated and whether the update was applied with part of it
// parked (ApplyOutcome), read under the lock the apply holds, so a
// concurrent apply cannot change the answer. To report Parked it reads the
// update's structs and delete set once more, without decoding their content,
// after the apply; to report Integrated it encodes the commit's update when
// the commit changed the document, as it does for an OnUpdate observer.
func ApplyUpdateV1Outcome(doc *Doc, update []byte, origin any) (out ApplyOutcome, err error) {
	var failed *[]byte
	doc.Transact(func(txn *Transaction) {
		txn.Local = false // Yjs readUpdate: transact(..., local=false)
		txn.integrated = &out.Integrated
		if err = applyV1Txn(txn, update); err != nil {
			// Encoded at commit, after the GC, as OnUpdate's update is.
			failed = new([]byte)
			txn.landed = failed
			return
		}
		out.Parked = updatePastStore(txn.doc.store, update)
		if !out.Parked && !txn.changesDoc() {
			// An apply that changed nothing and left nothing past the store
			// held all of update, as ApplyUpdateV1Held decides it.
			if structs, ds, decodeErr := decodeStructsV1(newScratchDoc(), update); decodeErr == nil {
				out.Held = storeContains(txn.doc.store, structs, ds)
			}
		}
	}, origin)
	if failed != nil {
		out.Landed = *failed
	}
	return out, err
}

// updatePastStore reports whether the V1 update carries a struct, other than
// a Skip, or deletes a clock, past the clocks store holds of its client. A
// client's structs enter the store in clock order, each once its
// dependencies are there, so every struct the store lacks of a client, and
// every deletion of a clock the store lacks, lies past the store's next clock
// for that client; what lies past it the apply parked or merged into the
// queue. The structs are scanned without decoding their content. An update
// that does not scan reaches past nothing. The caller holds the document
// lock.
func updatePastStore(store *StructStore, update []byte) bool {
	s := newPendingScanner(update, false)
	groups := s.uint()
	for i := uint64(0); i < groups && s.err == nil; i++ {
		n := s.uint()
		c := s.client()
		at := s.uint()
		next := store.NextClock(c)
		for j := uint64(0); j < n && s.err == nil; j++ {
			length, skip, _, _ := s.item()
			if s.err != nil || length > ^uint64(0)-at {
				return false
			}
			end := at + length
			if !skip && end > next {
				return true
			}
			at = end
		}
	}
	clients := s.uint()
	for i := uint64(0); i < clients && s.err == nil; i++ {
		c := ClientID(s.uint())
		ranges := s.uint()
		next := store.NextClock(c)
		for j := uint64(0); j < ranges && s.err == nil; j++ {
			clock, length := s.uint(), s.uint()
			if s.err != nil || length > ^uint64(0)-clock {
				return false
			}
			if length > 0 && clock+length > next {
				return true
			}
		}
	}
	return false
}
