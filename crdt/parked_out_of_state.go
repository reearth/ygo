package crdt

// SetParkedOutOfState (off by default) leaves the structs and deletions d
// holds parked - its pending queue and pending deletes, which wait for a
// struct d lacks - out of every encode of d's state: EncodeStateAsUpdateV1
// and EncodeStateAsUpdateV2, and SplitStateAsUpdateV1 (the answer to a sync
// step 1). Each then carries d's integrated structs and deletions only, and a
// reader of it holds nothing that d has not integrated. ApplyReport.StateBytes
// counts no parked content either: a park adds nothing to it, and the drain
// that integrates a parked struct counts it as an append of the struct to the
// store.
//
// What is parked is kept: the queue still holds it, a later apply that fills
// its gap still drains it, and the commit that integrates a struct carries it
// in its update. Doc.PendingStats and ApplyReport.DocStructs still count it,
// since it is held in memory: a caller that weighs the encoded state reads
// ApplyReport.StateBytes, and one that weighs what the document holds reads
// PendingStats as well.
//
// Set it before the document is handed to any reader, as the encodes it
// changes are what a reader already holds.
func (d *Doc) SetParkedOutOfState(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.store
	if s.parkedOutOfState == on {
		return
	}
	s.parkedOutOfState = on
	if s.pending != nil {
		// What an apply report knows of the queue's clocks is kept only while
		// the queue is in the state; built again on the next need.
		s.pending.stateCover = nil
	}
}

// ParkedOutOfState reports SetParkedOutOfState's setting.
func (d *Doc) ParkedOutOfState() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.store.parkedOutOfState
}
