package crdt

import (
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// v2Frame reads a V2 update's structs on its columns as a lenient
// pendingScanner does.
type v2Frame struct {
	d    *v2Decoder
	keys int
	err  error
	// gc is whether the struct item last read is a GC struct, which only
	// parked GC structs hold.
	gc bool
}

func (f *v2Frame) uint() uint64 {
	if f.err != nil {
		return 0
	}
	v, err := f.d.restDec.ReadVarUint()
	f.err = err
	return v
}

func (f *v2Frame) client() ClientID { return ClientID(uintInRun(&f.d.clientDec, f)) }

func clockInRun(d *anycodec.IntDiffOptRleDecoder, f *v2Frame) uint64 {
	if v, ok := d.ReadInRun(); ok {
		return uint64(v)
	}
	if f.err != nil {
		return 0
	}
	v, err := d.Read()
	f.err = err
	return uint64(v)
}

func uintInRun(d *anycodec.UintOptRleDecoder, f *v2Frame) uint64 {
	if v, ok := d.ReadInRun(); ok {
		return v
	}
	if f.err != nil {
		return 0
	}
	v, err := d.Read()
	f.err = err
	return v
}

func byteInRun(d *anycodec.RleByteDecoder, f *v2Frame) byte {
	if v, ok := d.ReadInRun(); ok {
		return v
	}
	if f.err != nil {
		return 0
	}
	v, err := d.Read()
	f.err = err
	return v
}

// text steps over a string of the string column and returns its length.
func (f *v2Frame) text() uint64 {
	if f.err != nil {
		return 0
	}
	v, err := f.d.stringDec.ReadUnits()
	f.err = err
	return v
}

func (f *v2Frame) any() {
	if f.err != nil {
		return
	}
	rest := f.d.restDec.RemainingBytes()
	n, err := anycodec.Skip(rest)
	*f.d.restDec = *encoding.NewDecoder(rest[n:])
	f.err = err
}

func (f *v2Frame) key() {
	if f.err != nil {
		return
	}
	index, err := f.d.keyClockDec.Read()
	if err == nil && index < 0 {
		err = ErrInvalidUpdate
	}
	f.err = err
	if f.err == nil && index >= int64(f.keys) {
		f.text()
		f.keys++
	}
}

// item reads one struct as pendingScanner.item reads it.
func (f *v2Frame) item() (length uint64, skip bool, deps [3]ID, numDeps int) {
	info := byteInRun(&f.d.infoDec, f)
	tag := info & 0x1f
	f.gc = tag == 0
	if tag == 0 {
		return uintInRun(&f.d.lenDec, f), false, deps, 0
	}
	if tag == 10 {
		return f.uint(), true, deps, 0
	}
	if info&flagHasOrigin != 0 {
		deps[numDeps] = ID{Client: f.client(), Clock: clockInRun(&f.d.leftClockDec, f)}
		numDeps++
	}
	if info&flagHasRightOrigin != 0 {
		deps[numDeps] = ID{Client: f.client(), Clock: clockInRun(&f.d.rightClockDec, f)}
		numDeps++
	}
	if numDeps == 0 {
		if byteInRun(&f.d.parentInfoDec, f) == 1 {
			f.text()
		} else {
			deps[0] = ID{Client: f.client(), Clock: clockInRun(&f.d.leftClockDec, f)}
			numDeps++
		}
		if info&flagHasParentSub != 0 {
			f.text()
		}
	}
	return f.content(tag), false, deps, numDeps
}

// content reads a struct's content as the lenient pendingScanner does, and is
// its length.
func (f *v2Frame) content(tag byte) uint64 {
	switch tag {
	case wireDeleted:
		return uintInRun(&f.d.lenDec, f)
	case wireString:
		return f.text()
	case wireJSON, wireAny:
		n := uintInRun(&f.d.lenDec, f)
		if n > maxV2Items {
			f.fail(ErrInvalidUpdate)
			return 0
		}
		for i := uint64(0); i < n && f.err == nil; i++ {
			if tag == wireJSON {
				f.text()
			} else {
				f.any()
			}
		}
		return n
	case wireBinary:
		if f.err == nil {
			_, f.err = f.d.restDec.ReadVarBytes()
		}
	case wireEmbed:
		f.any()
	case wireFormat:
		f.key()
		f.any()
	case wireType:
		// Checked before the byte conversion, so that 261 cannot read as 5.
		switch ref := uintInRun(&f.d.typeRefDec, f); {
		case ref > maxTypeRef:
			f.fail(typeRefError(ref))
		case typeRefHasName(byte(ref)):
			f.key()
		}
	case wireDoc:
		f.text()
		f.any()
	case wireMove:
		f.uint()
		f.uint()
		f.uint()
	default:
		f.fail(tagError(tag, true))
	}
	return 1
}

// fail stops the frame for err, unless it has stopped already.
func (f *v2Frame) fail(err error) {
	if f.err == nil {
		f.err = err
	}
}

// scanPendingGroupsV2 is scanPendingGroupsV1 for a V2 update.
func scanPendingGroupsV2(d *v2Decoder, known StateVector, remaining int, clientRange *pendingClientRange, superseded groupSet, structs int, parked *pendingUpdate, whole bool) (pendingPass, error) {
	var result pendingPass
	read := 0
	f := v2Frame{d: d}
	clients := f.uint()
	if f.err != nil {
		return result, scanReason(f.err)
	}
	if clients > maxV2Items {
		return result, ErrInvalidUpdate
	}
	var total uint64
	first := true
	for i := uint64(0); i < clients && f.err == nil; i++ {
		n := f.uint()
		total += n
		if total > maxV2Items {
			return result, ErrInvalidUpdate
		}
		client, clock := f.client(), f.uint()
		drop := superseded.has(i)
		if clientRange != nil && !drop {
			clientRange.add(client, first)
			first = false
		}
		knownClock := known.Clock(client)
		startClock := knownClock
		stalled := false // a covered struct waits, so the rest of the group does
		var depClient ClientID
		var depClock uint64
		depCached := false
		for j := uint64(0); j < n && f.err == nil; j++ {
			length, skip, deps, numDeps := f.item()
			if f.err != nil {
				// A struct the frame cannot read refuses the update for that
				// reason, before any count of it.
				break
			}
			end := clock + length
			if end < clock {
				return result, ErrInvalidUpdate
			}
			if !drop && !skip {
				// Yjs checks a struct's dependencies before its clocks, so a
				// covered struct can wait too, and every struct of the client
				// after one that waits parks with it.
				waits := stalled || clock > knownClock
				if !waits {
					for _, dep := range deps[:numDeps] {
						at := knownClock
						if dep.Client != client {
							if !depCached || dep.Client != depClient {
								depClient, depClock, depCached = dep.Client, known.Clock(dep.Client), true
							}
							at = depClock
						}
						if dep.Clock >= at {
							waits = true
							break
						}
					}
				}
				if !waits {
					if end > knownClock {
						knownClock = end
						result.progressed = true
					}
				} else {
					stalled = stalled || end <= knownClock
					if parked.merges(client, clock, end, f.gc) {
						result.overlapping++
					} else {
						result.pending++
						if clients == 1 && pastRoom(result.pending, remaining, parked.heldStructs()) {
							return result, ErrPendingCap
						}
					}
				}
			}
			clock = end
			read++
			if unread := pendingUnread(structs, read, i+1 == clients, n-j-1); unread >= 0 && result.charged()+unread <= remaining {
				if !whole {
					return result, nil
				}
				result.settled = true
			}
		}
		if knownClock != startClock {
			known[client] = knownClock
		}
	}
	if f.err != nil {
		return result, scanReason(f.err)
	}
	result.rest, result.exact = d.restDec.RemainingBytes(), clients <= 1
	return result, nil
}
