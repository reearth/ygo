package crdt

import (
	"encoding/binary"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// v1Frame reads a V1 update's structs as a lenient pendingScanner does, without
// allocating, so the check stays cheap on a large update.
type v1Frame struct {
	buf []byte
	p   int
	// err is why the frame went bad, as the decoder names it, or nil.
	err error
}

// fail stops the frame for err. It keeps scanReason's value, never err, so a
// decoder copy that err comes from stays on the stack.
func (f *v1Frame) fail(err error) {
	if f.err == nil {
		f.err = scanReason(err)
	}
	f.p = len(f.buf)
}

// eof stops the frame for a read past the end; it is small enough for the
// readers that call it to inline.
func (f *v1Frame) eof() {
	if f.err == nil {
		f.err = encoding.ErrUnexpectedEOF
	}
	f.p = len(f.buf)
}

func (f *v1Frame) uint() uint64 {
	if p := f.p; p < len(f.buf) && f.buf[p] < 0x80 {
		f.p = p + 1
		return uint64(f.buf[p])
	}
	return f.uintSlow()
}

func (f *v1Frame) uintSlow() uint64 {
	v, n := binary.Uvarint(f.buf[f.p:])
	if n == 0 {
		f.eof()
		return 0
	}
	if n < 0 {
		f.fail(encoding.ErrOverflow)
		return 0
	}
	f.p += n
	return v
}

func (f *v1Frame) byte() byte {
	if f.p >= len(f.buf) {
		f.eof()
		return 0
	}
	b := f.buf[f.p]
	f.p++
	return b
}

// bytes steps over a length-prefixed string and returns it.
func (f *v1Frame) bytes() []byte {
	l := f.uint()
	if l > uint64(len(f.buf)-f.p) {
		f.eof()
		return nil
	}
	b := f.buf[f.p : f.p+int(l)]
	f.p += int(l)
	return b
}

func (f *v1Frame) any() {
	if f.err != nil {
		return
	}
	n, err := anycodec.Skip(f.buf[f.p:])
	if err != nil {
		f.fail(err)
		return
	}
	f.p += n
}

// item reads one struct as pendingScanner.item reads it.
func (f *v1Frame) item() (length uint64, skip bool, deps [3]ID, numDeps int) {
	info := f.byte()
	tag := info & 0x1f
	if tag == 0 {
		return f.uint(), false, deps, 0
	}
	if tag == 10 {
		return f.uint(), true, deps, 0
	}
	if info&flagHasOrigin != 0 {
		deps[numDeps] = ID{Client: ClientID(f.uint()), Clock: f.uint()}
		numDeps++
	}
	if info&flagHasRightOrigin != 0 {
		deps[numDeps] = ID{Client: ClientID(f.uint()), Clock: f.uint()}
		numDeps++
	}
	if numDeps == 0 {
		if f.byte() == 1 {
			f.bytes()
		} else {
			deps[0] = ID{Client: ClientID(f.uint()), Clock: f.uint()}
			numDeps++
		}
		if info&flagHasParentSub != 0 {
			f.bytes()
		}
	}
	return f.content(tag), false, deps, numDeps
}

// content reads a struct's content as decodeContent does, and is its length.
func (f *v1Frame) content(tag byte) uint64 {
	switch tag {
	case wireDeleted:
		return f.uint()
	case wireString:
		return utf16Units(f.bytes())
	case wireJSON:
		return f.frameJSON()
	case wireAny:
		n := f.uint()
		if n > uint64(len(f.buf)-f.p) {
			f.fail(ErrInvalidUpdate)
			return 0
		}
		for i := uint64(0); i < n && f.err == nil; i++ {
			f.any()
		}
		return n
	case wireBinary, wireEmbed:
		f.bytes()
	case wireFormat:
		f.bytes()
		f.bytes()
	case wireType:
		if ref := f.byte(); ref == 3 || ref == 5 {
			f.bytes()
		}
	case wireDoc:
		f.bytes()
		f.any()
	case wireMove:
		f.uint()
		f.uint()
		f.uint()
	default:
		f.fail(tagError(tag, false))
	}
	return 1
}

// frameJSON reads JSON content as decodeContent does, since only that decides
// where the values end.
func (f *v1Frame) frameJSON() uint64 {
	start := f.p
	n := f.uint()
	if n > uint64(len(f.buf)-f.p) {
		f.fail(ErrInvalidUpdate)
		return 0
	}
	if n == 0 || !isAnyTag(f.buf[f.p]) {
		for i := uint64(0); i < n && f.err == nil; i++ {
			f.bytes()
		}
		return n
	}
	d := encoding.NewDecoder(f.buf[start:])
	js := pendingScanner{rest: d}
	js.content(wireJSON)
	if js.err != nil {
		f.fail(js.err)
		return 0
	}
	f.p = len(f.buf) - d.Remaining()
	return n
}

// scanPendingGroupsV1 is the pending budget's first pass over a V1 update,
// stopping once the unread structs cannot change its answer.
func scanPendingGroupsV1(update []byte, known StateVector, remaining int, clientRange *pendingClientRange, superseded groupSet, structs int) (pendingPass, error) {
	var result pendingPass
	read := 0
	f := v1Frame{buf: update}
	clients := f.uint()
	if f.err != nil {
		return result, f.err
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
		client, clock := ClientID(f.uint()), f.uint()
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
			if f.err != nil {
				// A struct the frame cannot read refuses the update for that
				// reason, before any count of it.
				break
			}
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
			if unread := pendingUnread(structs, read, i+1 == clients, n-j-1); unread >= 0 && result.pending+unread <= remaining {
				return result, nil
			}
		}
		if knownClock != startClock {
			known[client] = knownClock
		}
	}
	if f.err != nil {
		return result, f.err
	}
	return result, nil
}

// pendingUnread bounds the structs the first pass has not read, each adding one
// pending at most, or is -1 when it cannot.
func pendingUnread(structs, read int, lastGroup bool, inGroup uint64) int {
	if structs > 0 {
		return structs - read
	}
	if lastGroup {
		return int(inGroup)
	}
	return -1
}
