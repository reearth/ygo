package crdt

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"
)

// The readers here (ToSlice, Entries, ToJSON and observer values) walk nested
// shared types with an explicit stack instead of recursing. A document can
// reach any nesting depth through many small updates from a peer, so a
// recursive read would let that peer grow the reader's goroutine stack until
// the process dies.

// nestedJSONEntry is one value a container renders: a plain value, or the
// owner of a nested shared type (owner != nil) for the walker to descend into.
type nestedJSONEntry struct {
	value any
	owner any
}

func nestedJSONEntryForContent(ct *ContentType) nestedJSONEntry {
	if ct == nil || ct.Type == nil || ct.Type.owner == nil {
		return nestedJSONEntry{}
	}
	return nestedJSONEntry{owner: ct.Type.owner}
}

// nestedJSONEntryForValue reads a staged entry of a detached container, which
// is a plain value or a detached shared type.
func nestedJSONEntryForValue(value any) nestedJSONEntry {
	if st, ok := value.(sharedType); ok {
		return nestedJSONEntry{owner: st.baseType().owner}
	}
	return nestedJSONEntry{value: value}
}

// nestedJSONEntryForMapItem reads a map entry; ok is false for a key the map
// does not render.
func nestedJSONEntryForMapItem(item *Item) (nestedJSONEntry, bool) {
	if item == nil || item.Deleted {
		return nestedJSONEntry{}, false
	}
	// ContentJSON (legacy wire tag 2) reads like ContentAny.
	if value, ok := lastPlainVal(item.Content); ok {
		return nestedJSONEntry{value: value}, true
	}
	switch content := item.Content.(type) {
	case *ContentEmbed:
		return nestedJSONEntry{value: content.Val}, true
	case *ContentType:
		return nestedJSONEntryForContent(content), true
	}
	return nestedJSONEntry{}, false
}

// jsonArrayCursor yields the values an array renders, in order, without
// copying them: a detached array's staged entries, or each live item's values
// with a winning move rendering its target, as renderedStep defines.
type jsonArrayCursor struct {
	t      *abstractType
	item   *Item // next item to read
	vals   []any // unread values of the current item, or the staged entries
	staged bool
}

func newJSONArrayCursor(a *YArray) jsonArrayCursor {
	if a.detached() {
		return jsonArrayCursor{vals: a.prelim, staged: true}
	}
	return jsonArrayCursor{t: &a.abstractType, item: a.start}
}

func (c *jsonArrayCursor) next() (nestedJSONEntry, bool) {
	for {
		if len(c.vals) > 0 {
			value := c.vals[0]
			c.vals = c.vals[1:]
			if c.staged {
				return nestedJSONEntryForValue(value), true
			}
			return nestedJSONEntry{value: value}, true
		}
		item := c.item
		if item == nil {
			return nestedJSONEntry{}, false
		}
		c.item = item.Right
		countable, _, renderAt := c.t.renderedStep(item)
		if !countable {
			continue
		}
		if renderAt != nil {
			item = renderAt
		}
		if vals, ok := plainVals(item.Content); ok {
			c.vals = vals
			continue
		}
		switch content := item.Content.(type) {
		case *ContentEmbed:
			return nestedJSONEntry{value: content.Val}, true
		case *ContentType:
			return nestedJSONEntryForContent(content), true
		}
	}
}

// jsonEntryLocked reads the entry under key, which appendJSONKeysLocked
// reported.
func (m *YMap) jsonEntryLocked(key string) nestedJSONEntry {
	if m.detached() {
		return nestedJSONEntryForValue(m.prelim[key])
	}
	entry, _ := nestedJSONEntryForMapItem(m.itemMap[key])
	return entry
}

// appendJSONKeysLocked appends the keys m renders, sorted bytewise as
// encoding/json sorts map keys.
func (m *YMap) appendJSONKeysLocked(keys []string) []string {
	start := len(keys)
	if m.detached() {
		for key := range m.prelim {
			keys = append(keys, key)
		}
	} else {
		for key, item := range m.itemMap {
			if _, ok := nestedJSONEntryForMapItem(item); ok {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys[start:])
	return keys
}

// nestedJSONSlot is where a converted nested type goes: the result, an element
// of its parent's slice, or a key of its parent's map.
type nestedJSONSlot struct {
	root   *any
	array  []any
	index  int
	object map[string]any
	key    string
}

func (slot nestedJSONSlot) set(value any) {
	switch {
	case slot.root != nil:
		*slot.root = value
	case slot.array != nil:
		slot.array[slot.index] = value
	default:
		slot.object[slot.key] = value
	}
}

type nestedJSONFrame struct {
	owner any
	slot  nestedJSONSlot
}

// nestedJSONValue converts owner to its JSON-shaped value: YArray → []any,
// YMap → map[string]any, YText → string, the XML types → their XML string,
// anything else → nil. A container fills its own slice or map in one pass and
// leaves a frame only for each nested type it holds. Caller must hold the doc
// lock.
func nestedJSONValue(owner any) any {
	var result any
	var inline [16]nestedJSONFrame
	stack := inline[:1]
	stack[0] = nestedJSONFrame{owner: owner, slot: nestedJSONSlot{root: &result}}

	for len(stack) > 0 {
		last := len(stack) - 1
		frame := stack[last]
		stack = stack[:last]

		switch current := frame.owner.(type) {
		case *YArray:
			value := make([]any, 0, current.Len())
			children := len(stack)
			cursor := newJSONArrayCursor(current)
			for entry, ok := cursor.next(); ok; entry, ok = cursor.next() {
				if entry.owner != nil {
					stack = append(stack, nestedJSONFrame{
						owner: entry.owner,
						slot:  nestedJSONSlot{index: len(value)},
					})
				}
				value = append(value, entry.value)
			}
			// The slice's backing array is final only now.
			for i := children; i < len(stack); i++ {
				stack[i].slot.array = value
			}
			frame.slot.set(value)
		case *YMap:
			var value map[string]any
			if current.detached() {
				value = make(map[string]any, len(current.prelim))
				for key, staged := range current.prelim {
					stack = putNestedJSONEntry(stack, value, key, nestedJSONEntryForValue(staged))
				}
			} else {
				value = make(map[string]any, len(current.itemMap))
				for key, item := range current.itemMap {
					if entry, ok := nestedJSONEntryForMapItem(item); ok {
						stack = putNestedJSONEntry(stack, value, key, entry)
					}
				}
			}
			frame.slot.set(value)
		case *YText:
			frame.slot.set(current.toStringLocked())
		case *YXmlElement:
			frame.slot.set(current.toXMLLocked())
		case *YXmlFragment:
			frame.slot.set(current.toXMLLocked())
		case *YXmlText:
			frame.slot.set(current.toXMLLocked())
		default:
			frame.slot.set(nil)
		}
	}
	return result
}

// putNestedJSONEntry stores a plain entry in object, or pushes a frame that
// stores the converted nested type there later.
func putNestedJSONEntry(stack []nestedJSONFrame, object map[string]any, key string, entry nestedJSONEntry) []nestedJSONFrame {
	if entry.owner == nil {
		object[key] = entry.value
		return stack
	}
	return append(stack, nestedJSONFrame{
		owner: entry.owner,
		slot:  nestedJSONSlot{object: object, key: key},
	})
}

// toJSONValue unwraps a ContentType into its JSON-shaped value. Caller must
// hold the doc lock.
func toJSONValue(ct *ContentType) any {
	if ct == nil || ct.Type == nil || ct.Type.owner == nil {
		return nil
	}
	return nestedJSONValue(ct.Type.owner)
}

// jsonWriter is ToJSON's scratch space, pooled so repeated calls reuse its
// buffers. write runs under the document's read lock and renders everything
// except the values only encoding/json can encode, whose positions it
// records; finish runs after the lock is released and marshals those, so no
// MarshalJSON method runs while the lock is held.
type jsonWriter struct {
	out          []byte
	deferredAt   []int // offsets in out where deferredVals go
	deferredVals []any
	frames       []jsonFrame
	keys         []string // sorted keys of every open map, outermost first
	full         bytes.Buffer
	encoder      *json.Encoder // writes to full
}

// jsonFrame is one open container: an array read through its cursor, or a
// map (m != nil) with keys w.keys[next:end] still to write.
type jsonFrame struct {
	array     jsonArrayCursor
	m         *YMap
	keysStart int
	next, end int
	written   bool // an entry was written, so the next one needs a comma
}

var jsonWriterPool = sync.Pool{New: func() any {
	w := &jsonWriter{}
	w.encoder = json.NewEncoder(&w.full)
	return w
}}

// marshalSharedJSON is YArray.ToJSON and YMap.ToJSON.
func marshalSharedJSON(doc *Doc, owner any) ([]byte, error) {
	w := jsonWriterPool.Get().(*jsonWriter)
	defer w.release()
	w.writeLocked(doc, owner)
	return w.finish()
}

func (w *jsonWriter) writeLocked(doc *Doc, owner any) {
	if doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	w.write(owner)
}

// write renders owner with one frame per open container: the Go stack stays
// flat, and the frame stack grows with the nesting depth, not with the number
// of elements.
func (w *jsonWriter) write(owner any) {
	w.open(owner)
	for len(w.frames) > 0 {
		frame := &w.frames[len(w.frames)-1]
		if child := w.writeLeaves(frame); child != nil {
			w.open(child)
			continue
		}
		w.close(frame)
	}
}

// writeLeaves writes frame's entries up to the next nested shared type, which
// it returns, or to the container's end, where it returns nil.
func (w *jsonWriter) writeLeaves(frame *jsonFrame) any {
	for {
		var entry nestedJSONEntry
		if frame.m != nil {
			if frame.next == frame.end {
				return nil
			}
			key := w.keys[frame.next]
			frame.next++
			w.separate(frame)
			w.writeString(key)
			w.out = append(w.out, ':')
			entry = frame.m.jsonEntryLocked(key)
		} else {
			var ok bool
			if entry, ok = frame.array.next(); !ok {
				return nil
			}
			w.separate(frame)
		}
		if entry.owner != nil {
			return entry.owner
		}
		w.writeValue(entry.value)
	}
}

func (w *jsonWriter) separate(frame *jsonFrame) {
	if frame.written {
		w.out = append(w.out, ',')
	}
	frame.written = true
}

// open writes a container's opening bracket and pushes its frame, or writes a
// nested type that reads as a string, or null.
func (w *jsonWriter) open(owner any) {
	switch current := owner.(type) {
	case *YArray:
		w.out = append(w.out, '[')
		w.frames = append(w.frames, jsonFrame{array: newJSONArrayCursor(current)})
	case *YMap:
		w.out = append(w.out, '{')
		start := len(w.keys)
		w.keys = current.appendJSONKeysLocked(w.keys)
		w.frames = append(w.frames, jsonFrame{
			m:         current,
			keysStart: start,
			next:      start,
			end:       len(w.keys),
		})
	case *YText:
		w.writeString(current.toStringLocked())
	case *YXmlElement:
		w.writeString(current.toXMLLocked())
	case *YXmlFragment:
		w.writeString(current.toXMLLocked())
	case *YXmlText:
		w.writeString(current.toXMLLocked())
	default:
		w.out = append(w.out, "null"...)
	}
}

func (w *jsonWriter) close(frame *jsonFrame) {
	if frame.m != nil {
		w.out = append(w.out, '}')
		w.keys = w.keys[:frame.keysStart]
	} else {
		w.out = append(w.out, ']')
	}
	w.frames = w.frames[:len(w.frames)-1]
}

func (w *jsonWriter) writeString(s string) {
	if out, ok := appendJSONString(w.out, s); ok {
		w.out = out
		return
	}
	w.deferredAt = append(w.deferredAt, len(w.out))
	w.deferredVals = append(w.deferredVals, s)
}

func (w *jsonWriter) writeValue(value any) {
	if out, ok := appendJSONScalar(w.out, value); ok {
		w.out = out
		return
	}
	w.deferredAt = append(w.deferredAt, len(w.out))
	w.deferredVals = append(w.deferredVals, value)
}

// finish marshals the deferred values with encoding/json, splices them into
// the rendered output, and returns a copy the caller owns. Errors are
// encoding/json's, for the first failing value in output order.
func (w *jsonWriter) finish() ([]byte, error) {
	if len(w.deferredAt) == 0 {
		return bytes.Clone(w.out), nil
	}
	prev := 0
	for i := 0; i < len(w.deferredAt); {
		// Deferred values with only a comma between them are neighbouring
		// elements of one array, which encoding/json marshals in one call.
		j := i + 1
		for j < len(w.deferredAt) && string(w.out[w.deferredAt[j-1]:w.deferredAt[j]]) == "," {
			j++
		}
		w.full.Write(w.out[prev:w.deferredAt[i]])
		if err := w.encodeRun(w.deferredVals[i:j]); err != nil {
			return nil, err
		}
		prev = w.deferredAt[j-1]
		i = j
	}
	w.full.Write(w.out[prev:])
	return bytes.Clone(w.full.Bytes()), nil
}

// encodeRun appends values to full as comma-separated JSON.
func (w *jsonWriter) encodeRun(values []any) error {
	if len(values) == 1 {
		return w.encode(values[0])
	}
	start := w.full.Len()
	if err := w.encode(values); err != nil {
		return err
	}
	// Drop the brackets encoding/json put around the run.
	encoded := w.full.Bytes()
	copy(encoded[start:], encoded[start+1:len(encoded)-1])
	w.full.Truncate(len(encoded) - 2)
	return nil
}

func (w *jsonWriter) encode(value any) error {
	if err := w.encoder.Encode(value); err != nil {
		return err
	}
	w.full.Truncate(w.full.Len() - 1) // Encode ends each value with a newline
	return nil
}

// release clears every reference into the document before pooling w.
func (w *jsonWriter) release() {
	clear(w.deferredVals[:cap(w.deferredVals)])
	clear(w.frames[:cap(w.frames)])
	clear(w.keys[:cap(w.keys)])
	w.out, w.deferredAt, w.deferredVals = w.out[:0], w.deferredAt[:0], w.deferredVals[:0]
	w.frames, w.keys = w.frames[:0], w.keys[:0]
	w.full.Reset()
	jsonWriterPool.Put(w)
}

// appendJSONScalar appends value as encoding/json encodes it, for the types
// whose encoding runs no user code: nil, bool, valid UTF-8 strings, and the
// integer and finite float types. ok is false for every other value, which
// the caller hands to encoding/json.
func appendJSONScalar(out []byte, value any) ([]byte, bool) {
	switch value := value.(type) {
	case nil:
		return append(out, "null"...), true
	case string:
		return appendJSONString(out, value)
	case bool:
		return strconv.AppendBool(out, value), true
	case int:
		return strconv.AppendInt(out, int64(value), 10), true
	case int8:
		return strconv.AppendInt(out, int64(value), 10), true
	case int16:
		return strconv.AppendInt(out, int64(value), 10), true
	case int32:
		return strconv.AppendInt(out, int64(value), 10), true
	case int64:
		return strconv.AppendInt(out, value, 10), true
	case uint:
		return strconv.AppendUint(out, uint64(value), 10), true
	case uint8:
		return strconv.AppendUint(out, uint64(value), 10), true
	case uint16:
		return strconv.AppendUint(out, uint64(value), 10), true
	case uint32:
		return strconv.AppendUint(out, uint64(value), 10), true
	case uint64:
		return strconv.AppendUint(out, value, 10), true
	case float64:
		return appendJSONFloat(out, value, 64)
	case float32:
		return appendJSONFloat(out, float64(value), 32)
	}
	return out, false
}

// appendJSONFloat is encoding/json's float encoding: ES6 number formatting,
// exponent form below 1e-6 and from 1e21. NaN and ±Inf report !ok so that
// encoding/json returns its own error for them.
func appendJSONFloat(out []byte, f float64, bits int) ([]byte, bool) {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return out, false
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 {
		// A float32 compares as float32 so the cutoffs match encoding/json's.
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	out = strconv.AppendFloat(out, f, format, -1, bits)
	if format == 'e' {
		// e-09 becomes e-9, as in encoding/json.
		n := len(out)
		if n >= 4 && out[n-4] == 'e' && out[n-3] == '-' && out[n-2] == '0' {
			out[n-2] = out[n-1]
			out = out[:n-1]
		}
	}
	return out, true
}

// appendJSONString appends s with encoding/json's HTML-safe string encoding.
// ok is false for invalid UTF-8, which the caller hands to encoding/json so
// the replacement characters match the running Go version's.
func appendJSONString(out []byte, s string) ([]byte, bool) {
	const hex = "0123456789abcdef"

	if !utf8.ValidString(s) {
		return out, false
	}
	out = append(out, '"')
	start := 0
	for index := 0; index < len(s); {
		if b := s[index]; b < utf8.RuneSelf {
			if b >= 0x20 && b != '\\' && b != '"' && b != '<' && b != '>' && b != '&' {
				index++
				continue
			}
			out = append(out, s[start:index]...)
			switch b {
			case '\\', '"':
				out = append(out, '\\', b)
			case '\b':
				out = append(out, '\\', 'b')
			case '\f':
				out = append(out, '\\', 'f')
			case '\n':
				out = append(out, '\\', 'n')
			case '\r':
				out = append(out, '\\', 'r')
			case '\t':
				out = append(out, '\\', 't')
			default:
				out = append(out, '\\', 'u', '0', '0', hex[b>>4], hex[b&0xF])
			}
			index++
			start = index
			continue
		}
		r, size := utf8.DecodeRuneInString(s[index:])
		if r == '\u2028' || r == '\u2029' {
			out = append(out, s[start:index]...)
			out = append(out, '\\', 'u', '2', '0', '2', hex[r&0xF])
			index += size
			start = index
			continue
		}
		index += size
	}
	out = append(out, s[start:]...)
	return append(out, '"'), true
}
