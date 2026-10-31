package crdt

// checkBeforeApply refuses an update that would park past the pending cap
// before the apply integrates any of it, so a refused update changes nothing.
func (b *pendingBudget) checkBeforeApply(size int) error {
	if !b.mayParkPastRoom(size) {
		return nil
	}
	if b.v2 && b.structs == 0 {
		n, err := b.walkStructsV2()
		if err != nil {
			// The apply would fail in this section after earlier structs integrated.
			return wrapUpdateErr(scanReason(err))
		}
		if n <= b.remaining {
			return nil
		}
		b.structs = n
	}
	return b.check()
}

// walkStructsV2 counts the V2 update's structs, skips included, or returns why
// it cannot.
func (b *pendingBudget) walkStructsV2() (int, error) {
	v2 := *b.v2Start
	n, err := v2StructCount(&v2, true)
	*v2.restDec = b.v2Rest
	return n, err
}

// firstPassV2 runs scanPendingGroupsV2 on copies of the update's cursors.
func (b *pendingBudget) firstPassV2(known StateVector, clients *pendingClientRange) (pendingPass, error) {
	v2 := *b.v2Start
	rest := b.v2Rest
	v2.restDec = &rest
	return scanPendingGroupsV2(&v2, known, b.remaining, clients, b.superseded, b.structs)
}

// mayParkPastRoom is whether the update may park more items than the room left,
// by a bound that costs nothing to read.
func (b *pendingBudget) mayParkPastRoom(size int) bool {
	if !b.v2 && size/2 <= b.remaining {
		return false
	}
	if b.structs > 0 {
		return b.structs > b.remaining
	}
	if !b.v2 {
		return true
	}
	// On stack copies of the cursors, so that it allocates nothing.
	v2 := *b.v2Start
	n, err := v2StructCount(&v2, false)
	*v2.restDec = b.v2Rest
	return err != nil || n > b.remaining
}

// v2StructCount is the V2 update's struct count from its group headers, or why
// it cannot be read: an unknown tag is a contentTagError.
func v2StructCount(d *v2Decoder, toEnd bool) (int, error) {
	s := pendingScanner{v2: d, rest: d.restDec}
	clients := s.uint()
	if s.err != nil {
		return 0, scanReason(s.err)
	}
	if clients > maxV2Items {
		return 0, ErrInvalidUpdate
	}
	var total uint64
	lens := 0 // length-column entries passed and not yet read
	for i := range clients {
		structs := s.uint()
		total += structs
		if s.err != nil {
			return 0, scanReason(s.err)
		}
		if total > maxV2Items {
			return 0, ErrInvalidUpdate
		}
		if i == clients-1 && !toEnd {
			break
		}
		s.uint() // the group's first clock; its client is on the client column
		if s.err != nil {
			return 0, scanReason(s.err)
		}
		for left := int(structs); left > 0; {
			info, k, err := d.infoDec.ReadRun(left)
			if err != nil {
				return 0, scanReason(err)
			}
			left -= k
			switch info & 0x1f {
			case 0, wireDeleted, wireJSON:
				lens += k
			case wireString, wireType:
			case wireAny:
				for range k {
					for ; lens > 0; lens-- {
						s.length()
					}
					values := s.length()
					if values > maxV2Items {
						return 0, ErrInvalidUpdate
					}
					for range values {
						s.any()
					}
				}
			case 10:
				for range k {
					s.uint()
				}
			case wireBinary:
				for range k {
					s.bytes()
				}
			case wireEmbed, wireFormat, wireDoc:
				for range k {
					s.any()
				}
			case wireMove:
				for range 3 * k {
					s.uint()
				}
			default:
				return 0, tagError(info&0x1f, true)
			}
			if s.err != nil {
				return 0, scanReason(s.err)
			}
		}
	}
	return int(total), nil
}
