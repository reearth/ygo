package crdt

// Transaction batches a set of insertions and deletions into a single atomic
// operation. Observers fire once per transaction, not once per operation,
// which keeps event handler overhead proportional to transactions not edits.
type Transaction struct {
	doc         *Doc
	Origin      any   // user-supplied tag forwarded to update observers
	Local       bool  // true when the change originated on this peer
	deleteSet   DeleteSet
	beforeState StateVector
	afterState  StateVector
	// changed tracks which types (and which map keys within them) were modified.
	changed map[*abstractType]map[string]struct{}
}

// addChanged records that a type was modified, optionally under a specific key.
func (txn *Transaction) addChanged(t *abstractType, key string) {
	keys, ok := txn.changed[t]
	if !ok {
		keys = make(map[string]struct{})
		txn.changed[t] = keys
	}
	keys[key] = struct{}{}
}
