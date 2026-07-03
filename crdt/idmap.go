package crdt

import (
	"fmt"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// ContentAttribute is one attribution fact attached to a range of item IDs,
// e.g. {"userid","alice"} or {"ts",int64(1719...)}. Value must stay within the
// lib0 "any" domain (see NewContentAttribute); use NewContentAttribute rather
// than a struct literal so invalid values fail at the point of the mistake
// instead of panicking inside EncodeIDMap.
type ContentAttribute struct {
	Name  string
	Value any
}

// ErrUnsupportedAttributeValue is returned by NewContentAttribute for values
// outside the lib0 "any" domain.
var ErrUnsupportedAttributeValue = fmt.Errorf("ygo/crdt: attribute value outside the lib0 any domain")

func validateAnyValue(v any) error {
	switch val := v.(type) {
	case nil, bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, encoding.BigInt, string, []byte:
		return nil
	case []any:
		for _, e := range val {
			if err := validateAnyValue(e); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		for _, e := range val {
			if err := validateAnyValue(e); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrUnsupportedAttributeValue, v)
	}
}

// NewContentAttribute validates value against the encodable lib0 domain and
// returns the attribute. Containers are validated recursively.
func NewContentAttribute(name string, value any) (*ContentAttribute, error) {
	if err := validateAnyValue(value); err != nil {
		return nil, err
	}
	return &ContentAttribute{Name: name, Value: value}, nil
}

// MustContentAttribute is NewContentAttribute that panics on invalid values.
// Intended for tests and compile-time-constant literals.
func MustContentAttribute(name string, value any) *ContentAttribute {
	a, err := NewContentAttribute(name, value)
	if err != nil {
		panic(err)
	}
	return a
}

// attrKey is the interning/equality key: varString(name) ++ writeAny(value)
// bytes. Semantically equivalent to yjs ContentAttribute.hash() (which
// fingerprints the same encoded bytes).
func attrKey(a *ContentAttribute) string {
	enc := encoding.NewEncoder()
	enc.WriteVarString(a.Name)
	enc.WriteAny(a.Value)
	return string(enc.Bytes())
}

// AttrRange is a run of item IDs [Clock, Clock+Len) carrying attribution.
// In IDMap.Slice output, Attrs == nil marks an unattributed gap.
type AttrRange struct {
	Clock uint64
	Len   uint64
	Attrs []*ContentAttribute
}

// attrsHas reports whether attrs contains attr by pointer identity (yjs
// idmapAttrsHas: `a === attr`). Sound because IDMap interns attributes.
func attrsHas(attrs []*ContentAttribute, attr *ContentAttribute) bool {
	for _, a := range attrs {
		if a == attr {
			return true
		}
	}
	return false
}

// attrsEqual is yjs idmapAttrsEqual: same length, same members (pointer identity).
func attrsEqual(a, b []*ContentAttribute) bool {
	if len(a) != len(b) {
		return false
	}
	for _, v := range a {
		if !attrsHas(b, v) {
			return false
		}
	}
	return true
}

// attrsJoin is yjs idmapAttrRangeJoin: a ++ (b minus members already in a).
func attrsJoin(a, b []*ContentAttribute) []*ContentAttribute {
	out := make([]*ContentAttribute, len(a), len(a)+len(b))
	copy(out, a)
	for _, attr := range b {
		if !attrsHas(a, attr) {
			out = append(out, attr)
		}
	}
	return out
}

// attrRanges is the lazily-normalized attributed-range list (yjs AttrRanges).
type attrRanges struct {
	sorted bool
	ids    []AttrRange
}

func (r *attrRanges) add(clock, length uint64, attrs []*ContentAttribute) {
	if length == 0 {
		return
	}
	r.sorted = false
	r.ids = append(r.ids, AttrRange{Clock: clock, Len: length, Attrs: attrs})
}

// getIDs returns sorted ranges with overlaps split and attrs joined
// (verbatim port of yjs AttrRanges.getIds, ids.js).
func (r *attrRanges) getIDs() []AttrRange {
	if r.sorted {
		return r.ids
	}
	r.sorted = true
	ids := r.ids
	sort.SliceStable(ids, func(i, j int) bool { return ids[i].Clock < ids[j].Clock })
	for i := 0; i < len(ids)-1; {
		rng := ids[i]
		next := ids[i+1]
		if rng.Clock < next.Clock { // may need to split rng
			if rng.Clock+rng.Len > next.Clock { // overlapping
				diff := next.Clock - rng.Clock
				ids[i] = AttrRange{Clock: rng.Clock, Len: diff, Attrs: rng.Attrs}
				rest := AttrRange{Clock: next.Clock, Len: rng.Len - diff, Attrs: rng.Attrs}
				ids = append(ids[:i+1], append([]AttrRange{rest}, ids[i+1:]...)...)
			}
			i++
			continue
		}
		// rng.Clock == next.Clock: merge.
		larger := rng
		if next.Len > rng.Len {
			larger = next
		}
		smaller := rng.Len
		if next.Len < rng.Len {
			smaller = next.Len
		}
		ids[i] = AttrRange{Clock: rng.Clock, Len: smaller, Attrs: attrsJoin(rng.Attrs, next.Attrs)}
		if rng.Len == next.Len {
			ids = append(ids[:i+1], ids[i+2:]...)
		} else {
			ids[i+1] = AttrRange{Clock: rng.Clock + smaller, Len: larger.Len - smaller, Attrs: larger.Attrs}
			// bubble ids[i+1] right to keep clock order (yjs array.bubblesortItem).
			for k := i + 1; k+1 < len(ids) && ids[k].Clock > ids[k+1].Clock; k++ {
				ids[k], ids[k+1] = ids[k+1], ids[k]
			}
		}
		if smaller == 0 {
			i++
		}
	}
	for len(ids) > 0 && ids[0].Len == 0 {
		ids = ids[1:]
	}
	// Adjacency merge for equal attrs (same shape as idRanges.getIDs tail).
	var i, j int
	for i, j = 1, 1; i < len(ids); i++ {
		left := ids[j-1]
		right := ids[i]
		if left.Clock+left.Len == right.Clock && attrsEqual(left.Attrs, right.Attrs) {
			ids[j-1] = AttrRange{Clock: left.Clock, Len: left.Len + right.Len, Attrs: left.Attrs}
		} else if right.Len != 0 {
			if j < i {
				ids[j] = right
			}
			j++
		}
	}
	if len(ids) > 0 {
		if ids[j-1].Len == 0 {
			ids = ids[:j-1]
		} else {
			ids = ids[:j]
		}
	}
	r.ids = ids
	return ids
}

// IDMap maps runs of item IDs to attribution metadata. It is the yjs-v14 IdMap
// (src/utils/ids.js). Attributes are interned per map so pointer identity is
// value identity, mirroring yjs object identity after _ensureAttrs.
//
// An IDMap is not goroutine-safe.
type IDMap struct {
	clients  map[ClientID]*attrRanges
	interned map[string]*ContentAttribute // attrKey → canonical instance
}

// NewIDMap returns an empty IDMap.
func NewIDMap() *IDMap {
	return &IDMap{
		clients:  make(map[ClientID]*attrRanges),
		interned: make(map[string]*ContentAttribute),
	}
}

// intern maps each attribute to this map's canonical instance (yjs _ensureAttrs).
func (m *IDMap) intern(attrs []*ContentAttribute) []*ContentAttribute {
	out := make([]*ContentAttribute, len(attrs))
	for i, a := range attrs {
		k := attrKey(a)
		if canon, ok := m.interned[k]; ok {
			out[i] = canon
		} else {
			m.interned[k] = a
			out[i] = a
		}
	}
	return out
}

// Add records attribution attrs for the run [clock, clock+length) of client.
// length 0 is a no-op. attrs are interned; later reads return the canonical
// instances.
func (m *IDMap) Add(client ClientID, clock, length uint64, attrs []*ContentAttribute) {
	if length == 0 {
		return
	}
	attrs = m.intern(attrs)
	r, ok := m.clients[client]
	if !ok {
		m.clients[client] = &attrRanges{ids: []AttrRange{{Clock: clock, Len: length, Attrs: attrs}}}
		return
	}
	r.add(clock, length, attrs)
}

// Has reports whether (client, clock) is attributed.
func (m *IDMap) Has(client ClientID, clock uint64) bool {
	r, ok := m.clients[client]
	if !ok {
		return false
	}
	for _, rg := range r.getIDs() {
		if rg.Clock <= clock && clock < rg.Clock+rg.Len {
			return true
		}
		if rg.Clock > clock {
			break
		}
	}
	return false
}

// IsEmpty reports whether the map contains no ranges.
func (m *IDMap) IsEmpty() bool {
	for _, r := range m.clients {
		if len(r.getIDs()) > 0 {
			return false
		}
	}
	return true
}

// Clients returns clients owning at least one range, ascending.
func (m *IDMap) Clients() []ClientID {
	out := make([]ClientID, 0, len(m.clients))
	for c, r := range m.clients {
		if len(r.getIDs()) > 0 {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Ranges returns the normalized attributed ranges for client (copy), or nil.
func (m *IDMap) Ranges(client ClientID) []AttrRange {
	r, ok := m.clients[client]
	if !ok {
		return nil
	}
	ids := r.getIDs()
	if len(ids) == 0 {
		return nil
	}
	out := make([]AttrRange, len(ids))
	copy(out, ids)
	return out
}

// Slice returns attribution covering [clock, clock+length) for client. The
// result covers the whole span: attributed sub-ranges carry their Attrs, and
// unattributed gaps are returned with Attrs == nil (yjs IdMap.slice).
func (m *IDMap) Slice(client ClientID, clock, length uint64) []AttrRange {
	var res []AttrRange
	if r, ok := m.clients[client]; ok {
		ranges := r.getIDs()
		var prevEnd = clock
		for _, rg := range ranges {
			if rg.Clock+rg.Len <= clock {
				continue
			}
			if rg.Clock >= clock+length {
				break
			}
			cl, ln := rg.Clock, rg.Len
			if cl < clock {
				ln -= clock - cl
				cl = clock
			}
			if cl+ln > clock+length {
				ln = clock + length - cl
			}
			if ln == 0 {
				break
			}
			if prevEnd < cl {
				res = append(res, AttrRange{Clock: prevEnd, Len: cl - prevEnd, Attrs: nil})
			}
			res = append(res, AttrRange{Clock: cl, Len: ln, Attrs: rg.Attrs})
			prevEnd = cl + ln
		}
	}
	if len(res) > 0 {
		last := res[len(res)-1]
		if end := last.Clock + last.Len; end < clock+length {
			res = append(res, AttrRange{Clock: end, Len: clock + length - end, Attrs: nil})
		}
	} else {
		res = append(res, AttrRange{Clock: clock, Len: length, Attrs: nil})
	}
	return res
}
