package crdt

import (
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// v2Frame reads a V2 update's struct section on its columns as a lenient
// pendingScanner does (see pendingScanner): each struct's length, clock and
// dependencies, with strings, keys and names read for their length only,
// from the string column's lengths, and Any values stepped over by their
// framing; it validates nothing the decoder validates. A value that repeats
// its column's current run is taken without a call into the column's
// decoder. The first failed read sets err and stops every later read.
type v2Frame struct {
	d    *v2Decoder
	keys int
	err  error
}

func (f *v2Frame) uint() uint64 {
	if f.err != nil {
		return 0
	}
	v, err := f.d.restDec.ReadVarUint()
	f.err = err
	return v
}

func (f *v2Frame) client() ClientID {
	if v, ok := f.d.clientDec.ReadInRun(); ok {
		return ClientID(v)
	}
	if f.err != nil {
		return 0
	}
	v, err := f.d.clientDec.Read()
	f.err = err
	return ClientID(v)
}

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

// text steps over a string of the string column, reading its length only.
func (f *v2Frame) text() uint64 {
	if v, ok := f.d.stringDec.ReadLenInRun(); ok {
		return v
	}
	if f.err != nil {
		return 0
	}
	v, err := f.d.stringDec.ReadLen()
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
			f.fail()
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
		if ref := byte(uintInRun(&f.d.typeRefDec, f)); ref == 3 || ref == 5 {
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
		f.fail()
	}
	return 1
}

func (f *v2Frame) fail() {
	if f.err == nil {
		f.err = ErrInvalidUpdate
	}
}

// scanPendingGroupsV2 is scanPendingGroupsV1 for a V2 update, on its columns
// through a v2Frame.
func scanPendingGroupsV2(d *v2Decoder, known StateVector, remaining int, clientRange *pendingClientRange, superseded groupSet, structs int) (pendingPass, error) {
	var result pendingPass
	read := 0
	f := v2Frame{d: d}
	clients := f.uint()
	// A failed read is ErrInvalidUpdate: the decoders' own errors are not
	// returned, so nothing of the decoder copy escapes through them.
	if f.err != nil {
		return result, ErrInvalidUpdate
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
		var depClient ClientID
		var depClock uint64
		depCached := false
		for j := uint64(0); j < n && f.err == nil; j++ {
			length, skip, deps, numDeps := f.item()
			end := clock + length
			if end < clock {
				return result, ErrInvalidUpdate
			}
			if !drop && !skip && end > knownClock {
				ready := clock <= knownClock
				if ready {
					for _, dep := range deps[:numDeps] {
						at := knownClock
						if dep.Client != client {
							if !depCached || dep.Client != depClient {
								depClient, depClock, depCached = dep.Client, known.Clock(dep.Client), true
							}
							at = depClock
						}
						if dep.Clock >= at {
							ready = false
							break
						}
					}
				}
				if ready {
					knownClock = end
					result.progressed = true
				} else {
					result.pending++
					if clients == 1 && result.pending > remaining {
						return result, ErrInvalidUpdate
					}
				}
			}
			clock = end
			read++
			if unread := pendingUnread(structs, read, i+1 == clients, n-j-1); unread >= 0 && f.err == nil && result.pending+unread <= remaining {
				return result, nil
			}
		}
		if knownClock != startClock {
			known[client] = knownClock
		}
	}
	if f.err != nil {
		return result, ErrInvalidUpdate
	}
	return result, nil
}
