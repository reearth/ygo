package crdt

// shrunk is s, copied into a slice made at its length once it holds under a
// quarter of the array it was compacted or cut in, and s itself otherwise;
// an empty s gives nil. A slice compacted or cut in place keeps the array of
// the most it held, which no figure of the queue or the pending deletes
// counts, so a list that grew to a cap and fell to one entry would hold that
// array for as long as the entry stays. The check is one comparison, and a
// copy of under a quarter of the array follows more than three quarters of
// its entries going, so it costs less than one copied entry for each entry
// that went. The caller holds the document lock.
func shrunk[E any](s []E) []E {
	switch {
	case len(s) == 0:
		return nil
	case len(s) < cap(s)/4:
		fresh := make([]E, len(s))
		copy(fresh, s)
		return fresh
	}
	return s
}
