package crdt

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"
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
	run, entry, ok := c.nextRun()
	if len(run) > 0 {
		c.vals = run[1:]
		return nestedJSONEntry{value: run[0]}, true
	}
	return entry, ok
}

// nextRun returns the next plain values of one item as a run, which aliases
// the item's content, or else the next single entry.
func (c *jsonArrayCursor) nextRun() ([]any, nestedJSONEntry, bool) {
	if c.staged {
		if len(c.vals) == 0 {
			return nil, nestedJSONEntry{}, false
		}
		value := c.vals[0]
		c.vals = c.vals[1:]
		return nil, nestedJSONEntryForValue(value), true
	}
	if len(c.vals) > 0 {
		run := c.vals
		c.vals = nil
		return run, nestedJSONEntry{}, true
	}
	for item := c.item; item != nil; item = c.item {
		c.item = item.Right
		countable, _, renderAt := c.t.renderedStep(item)
		if !countable {
			continue
		}
		if renderAt != nil {
			item = renderAt
		}
		if vals, ok := plainVals(item.Content); ok {
			if len(vals) > 0 {
				return vals, nestedJSONEntry{}, true
			}
			continue
		}
		switch content := item.Content.(type) {
		case *ContentEmbed:
			return nil, nestedJSONEntry{value: content.Val}, true
		case *ContentType:
			return nil, nestedJSONEntryForContent(content), true
		}
	}
	return nil, nestedJSONEntry{}, false
}

// nestedJSONFrame is a YArray or YMap being converted. An array reads its
// cursor up to the next nested container; a map has filled its plain entries
// and converts the nested ones in pending[next:end] one at a time.
type nestedJSONFrame struct {
	array     jsonArrayCursor
	slice     []any
	object    map[string]any // nil for an array
	start     int            // the map's first entry in pending
	next, end int
}

// nestedJSONPending is a map entry holding a nested container.
type nestedJSONPending struct {
	key   string
	owner any
}

// leafJSONValue converts a nested type that holds no nested JSON values: YText
// → string, the XML types → their XML string, anything else but YArray and
// YMap → nil. ok is false for YArray and YMap. Caller must hold the doc lock.
func leafJSONValue(owner any) (value any, ok bool) {
	switch current := owner.(type) {
	case *YArray, *YMap:
		return nil, false
	case *YText:
		return current.toStringLocked(), true
	case *YXmlElement:
		return current.toXMLLocked(), true
	case *YXmlFragment:
		return current.toXMLLocked(), true
	case *YXmlText:
		return current.toXMLLocked(), true
	}
	return nil, true
}

// nestedJSONValue converts owner to its JSON-shaped value: YArray → []any,
// YMap → map[string]any, and leafJSONValue for the rest. It keeps one frame
// per open container, and a container's value goes to its parent once it is
// complete. Caller must hold the doc lock.
func nestedJSONValue(owner any) any {
	if value, ok := leafJSONValue(owner); ok {
		return value
	}
	var inline [8]nestedJSONFrame
	stack := inline[:0]
	var pending []nestedJSONPending
	stack, pending = openNestedJSON(stack, pending, owner)

	for {
		frame := &stack[len(stack)-1]
		var child any
		if frame.object == nil {
			child = frame.nextArrayChild()
		} else if frame.next < frame.end {
			child = pending[frame.next].owner
		}
		if child != nil {
			stack, pending = openNestedJSON(stack, pending, child)
			continue
		}

		var value any = frame.object
		if frame.object == nil {
			value = frame.slice
		} else {
			pending = pending[:frame.start]
		}
		stack = stack[:len(stack)-1]
		if len(stack) == 0 {
			return value
		}
		parent := &stack[len(stack)-1]
		if parent.object == nil {
			parent.slice = append(parent.slice, value)
		} else {
			parent.object[pending[parent.next].key] = value
			pending[parent.next] = nestedJSONPending{}
			parent.next++
		}
	}
}

// openNestedJSON pushes a frame for the YArray or YMap owner.
func openNestedJSON(stack []nestedJSONFrame, pending []nestedJSONPending, owner any) ([]nestedJSONFrame, []nestedJSONPending) {
	if a, ok := owner.(*YArray); ok {
		return append(stack, nestedJSONFrame{
			array: newJSONArrayCursor(a),
			slice: make([]any, 0, a.Len()),
		}), pending
	}
	m := owner.(*YMap)
	start := len(pending)
	var object map[string]any
	if m.detached() {
		object = make(map[string]any, len(m.prelim))
		for key, staged := range m.prelim {
			pending = putNestedJSONEntry(pending, object, key, nestedJSONEntryForValue(staged))
		}
	} else {
		object = make(map[string]any, len(m.itemMap))
		for key, item := range m.itemMap {
			if entry, ok := nestedJSONEntryForMapItem(item); ok {
				pending = putNestedJSONEntry(pending, object, key, entry)
			}
		}
	}
	return append(stack, nestedJSONFrame{object: object, start: start, next: start, end: len(pending)}), pending
}

// nextArrayChild appends the array's values up to its next nested YArray or
// YMap, which it returns, or to its end, where it returns nil.
func (frame *nestedJSONFrame) nextArrayChild() any {
	for entry, ok := frame.array.next(); ok; entry, ok = frame.array.next() {
		if entry.owner != nil {
			leaf, isLeaf := leafJSONValue(entry.owner)
			if !isLeaf {
				return entry.owner
			}
			entry.value = leaf
		}
		frame.slice = append(frame.slice, entry.value)
	}
	return nil
}

// putNestedJSONEntry stores entry in object, or queues it in pending when it
// holds a nested YArray or YMap.
func putNestedJSONEntry(pending []nestedJSONPending, object map[string]any, key string, entry nestedJSONEntry) []nestedJSONPending {
	if entry.owner == nil {
		object[key] = entry.value
		return pending
	}
	if leaf, ok := leafJSONValue(entry.owner); ok {
		object[key] = leaf
		return pending
	}
	return append(pending, nestedJSONPending{key: key, owner: entry.owner})
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
// buffers. collect runs under the document's read lock and only copies the
// document out as tokens; render sorts and formats them after the lock is
// released, so writers wait for a copy rather than for formatting, and no
// MarshalJSON method runs while the lock is held.
type jsonWriter struct {
	tokens       []jsonToken
	vals         []any // the values of every jsonValues token
	frames       []jsonFrame
	order        []int // entry tokens of every open object, sorted by key
	out          []byte
	deferredAt   []int // offsets in out where deferredVals go
	deferredVals []any
	encoded      bytes.Buffer
	encoder      *json.Encoder // writes to encoded
}

type jsonTokenKind uint8

const (
	jsonValue  jsonTokenKind = iota // value
	jsonValues                      // vals[start:end], array elements
	jsonArray                       // tokens[start:end], or owner in value until collected
	jsonObject                      // tokens[start:end], or owner in value until collected
)

// jsonToken is one value. Inside an object it carries its key.
type jsonToken struct {
	kind       jsonTokenKind
	start, end int32
	key        string
	value      any
}

// jsonFrame is one open container during render: tokens[next:end] of an
// array, or order[next:end] of an object.
type jsonFrame struct {
	object           bool
	written          bool // an entry was written, so the next one needs a comma
	start, next, end int
}

func newJSONWriter() *jsonWriter {
	w := &jsonWriter{}
	w.encoder = json.NewEncoder(&w.encoded)
	return w
}

var jsonWriterPool = sync.Pool{New: func() any { return newJSONWriter() }}

// marshalSharedJSON is YArray.ToJSON and YMap.ToJSON.
func marshalSharedJSON(doc *Doc, owner any) ([]byte, error) {
	w := jsonWriterPool.Get().(*jsonWriter)
	defer w.release()
	w.collectLocked(doc, owner)
	w.render()
	return w.finish()
}

func (w *jsonWriter) collectLocked(doc *Doc, owner any) {
	if doc != nil {
		doc.mu.RLock()
		defer doc.mu.RUnlock()
	}
	w.collect(owner)
}

// collect records owner as tokens. Each container's direct children are one
// contiguous range, appended when the scan reaches the container's token, so
// one pass over the growing slice collects every level without recursion.
func (w *jsonWriter) collect(owner any) {
	w.appendEntry("", nestedJSONEntry{owner: owner})
	for i := 0; i < len(w.tokens); i++ {
		switch current := w.tokens[i].value.(type) {
		case *YArray:
			start := len(w.tokens)
			cursor := newJSONArrayCursor(current)
			for run, entry, ok := cursor.nextRun(); ok; run, entry, ok = cursor.nextRun() {
				if run == nil {
					w.appendEntry("", entry)
					continue
				}
				// Copied: a later merge may append into the content's spare capacity.
				w.tokens = append(w.tokens, jsonToken{
					kind:  jsonValues,
					start: int32(len(w.vals)),
					end:   int32(len(w.vals) + len(run)),
				})
				w.vals = append(w.vals, run...)
			}
			w.setChildren(i, start)
		case *YMap:
			start := len(w.tokens)
			if current.detached() {
				for key, staged := range current.prelim {
					w.appendEntry(key, nestedJSONEntryForValue(staged))
				}
			} else {
				for key, item := range current.itemMap {
					if entry, ok := nestedJSONEntryForMapItem(item); ok {
						w.appendEntry(key, entry)
					}
				}
			}
			w.setChildren(i, start)
		}
	}
}

// appendEntry records a plain value, a nested type that reads as a value, or
// a nested YArray or YMap whose children collect appends later.
func (w *jsonWriter) appendEntry(key string, entry nestedJSONEntry) {
	token := jsonToken{key: key, value: entry.value}
	if entry.owner != nil {
		switch entry.owner.(type) {
		case *YArray:
			token.kind, token.value = jsonArray, entry.owner
		case *YMap:
			token.kind, token.value = jsonObject, entry.owner
		default:
			token.value, _ = leafJSONValue(entry.owner)
		}
	}
	w.tokens = append(w.tokens, token)
}

func (w *jsonWriter) setChildren(i, start int) {
	token := &w.tokens[i]
	token.start, token.end, token.value = int32(start), int32(len(w.tokens)), nil
}

// render writes the tokens, objects in encoding/json's bytewise key order,
// except the values only encoding/json can encode, whose positions it records
// for finish.
func (w *jsonWriter) render() {
	w.writeToken(0)
	for len(w.frames) > 0 {
		frame := &w.frames[len(w.frames)-1]
		if frame.next == frame.end {
			if frame.object {
				w.out = append(w.out, '}')
				w.order = w.order[:frame.start]
			} else {
				w.out = append(w.out, ']')
			}
			w.frames = w.frames[:len(w.frames)-1]
			continue
		}
		i := frame.next
		if frame.object {
			i = w.order[i]
		}
		frame.next++
		token := &w.tokens[i]
		if token.kind == jsonValues {
			for _, value := range w.vals[token.start:token.end] {
				w.separate(frame)
				w.writeValue(value)
			}
			continue
		}
		w.separate(frame)
		if frame.object {
			w.writeString(token.key)
			w.out = append(w.out, ':')
		}
		w.writeToken(i)
	}
}

func (w *jsonWriter) separate(frame *jsonFrame) {
	if frame.written {
		w.out = append(w.out, ',')
	}
	frame.written = true
}

// writeToken writes a value, or opens a container and pushes its frame.
func (w *jsonWriter) writeToken(i int) {
	token := &w.tokens[i]
	switch token.kind {
	case jsonArray:
		w.out = append(w.out, '[')
		w.frames = append(w.frames, jsonFrame{next: int(token.start), end: int(token.end)})
	case jsonObject:
		w.out = append(w.out, '{')
		start := len(w.order)
		for j := token.start; j < token.end; j++ {
			w.order = append(w.order, int(j))
		}
		entries := w.order[start:]
		slices.SortFunc(entries, func(a, b int) int {
			return strings.Compare(w.tokens[a].key, w.tokens[b].key)
		})
		w.frames = append(w.frames, jsonFrame{object: true, start: start, next: start, end: len(w.order)})
	default:
		w.writeValue(token.value)
	}
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

// finish marshals the deferred values with encoding/json in one call, splices
// them into the rendered output, and returns a copy the caller owns. Errors
// are encoding/json's, for the first failing value in output order.
func (w *jsonWriter) finish() ([]byte, error) {
	if len(w.deferredAt) == 0 {
		return bytes.Clone(w.out), nil
	}
	var encode any = w.deferredVals
	if len(w.deferredVals) == 1 {
		encode = w.deferredVals[0]
	}
	if err := w.encoder.Encode(encode); err != nil {
		return nil, err
	}
	encoded := w.encoded.Bytes()
	encoded = encoded[:len(encoded)-1] // Encode ends with a newline
	if len(w.deferredVals) > 1 {
		encoded = encoded[1 : len(encoded)-1] // the brackets around the values
	}

	result := make([]byte, 0, len(w.out)+len(encoded))
	prev := 0
	for i := 0; i < len(w.deferredAt); {
		// Deferred values with only a comma between them are neighbouring
		// array elements, encoded the same way here, so they copy as one run.
		j := i + 1
		for j < len(w.deferredAt) && string(w.out[w.deferredAt[j-1]:w.deferredAt[j]]) == "," {
			j++
		}
		result = append(result, w.out[prev:w.deferredAt[i]]...)
		prev = w.deferredAt[j-1]
		n := len(encoded)
		if j < len(w.deferredAt) {
			n = -1
			for range j - i {
				n++ // past the previous value's comma
				n += nextJSONValueEnd(encoded[n:])
			}
		}
		result = append(result, encoded[:n]...)
		if n < len(encoded) {
			n++ // the comma before the next run
		}
		encoded = encoded[n:]
		i = j
	}
	return append(result, w.out[prev:]...), nil
}

// nextJSONValueEnd returns the length of the first value in compact JSON:
// the offset of the first comma outside strings and nested containers.
func nextJSONValueEnd(data []byte) int {
	depth := 0
	inString := false
	for i := 0; i < len(data); i++ {
		b := data[i]
		if inString {
			switch b {
			case '\\':
				i++
			case '"':
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		}
	}
	return len(data)
}

// release clears every reference into the document before pooling w.
func (w *jsonWriter) release() {
	clear(w.tokens[:cap(w.tokens)])
	clear(w.vals[:cap(w.vals)])
	clear(w.deferredVals[:cap(w.deferredVals)])
	w.tokens, w.vals, w.out, w.deferredAt, w.deferredVals = w.tokens[:0], w.vals[:0], w.out[:0], w.deferredAt[:0], w.deferredVals[:0]
	w.frames, w.order = w.frames[:0], w.order[:0]
	w.encoded.Reset()
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
