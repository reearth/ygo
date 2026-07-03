package crdt

import (
	"errors"
	"fmt"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// ErrAttributionDecode wraps every malformed-input error returned by the
// attribution decoders (DecodeIDSet, DecodeIDMap, DecodeContentIDs,
// DecodeContentMap).
var ErrAttributionDecode = errors.New("ygo/crdt: malformed attribution data")

func attrDecodeErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAttributionDecode, fmt.Sprintf(format, args...))
}

// maxPreallocAttrs caps the up-front capacity of the per-range attribute slice
// in readIDMap. attrCount is bounded only by the remaining input, so a crafted
// payload could otherwise force an ~8*Remaining()-byte pointer-slice allocation
// (make([]*ContentAttribute, 0, attrCount)) before decode fails on the first
// missing attr byte — the pointer-slice analogue of encoding's maxAnyElements
// guard (N-C2). append still grows the slice for a genuinely large, valid range,
// and each real attribute consumes input, so total work stays bounded.
const maxPreallocAttrs = 1024

// idSetRLEWriter is the clock/len RLE state of yjs IdSetEncoderV2: clocks are
// written as diffs against a per-client cursor; lens as len-1 (len 0 is
// forbidden on the wire — normalization removes zero-length ranges).
type idSetRLEWriter struct {
	enc *encoding.Encoder
	cur uint64
}

func (w *idSetRLEWriter) reset() { w.cur = 0 }
func (w *idSetRLEWriter) writeClock(clock uint64) {
	w.enc.WriteVarUint(clock - w.cur)
	w.cur = clock
}
func (w *idSetRLEWriter) writeLen(l uint64) { // caller guarantees l > 0
	w.enc.WriteVarUint(l - 1)
	w.cur += l
}

// idSetRLEReader mirrors idSetRLEWriter.
type idSetRLEReader struct {
	dec *encoding.Decoder
	cur uint64
}

func (r *idSetRLEReader) reset() { r.cur = 0 }
func (r *idSetRLEReader) readClock() (uint64, error) {
	diff, err := r.dec.ReadVarUint()
	if err != nil {
		return 0, err
	}
	r.cur += diff
	return r.cur, nil
}
func (r *idSetRLEReader) readLen() (uint64, error) {
	v, err := r.dec.ReadVarUint()
	if err != nil {
		return 0, err
	}
	l := v + 1
	r.cur += l
	return l, nil
}

// writeIDSet appends the canonical encoding of s to enc (yjs writeIdSet:
// clients DESCENDING, zero-range clients omitted). A nil s encodes as an
// empty IDSet, matching how the algebra ops (MergeIDSets et al.) already
// treat a nil input as empty.
func writeIDSet(enc *encoding.Encoder, s *IDSet) {
	if s == nil {
		enc.WriteVarUint(0)
		return
	}
	type entry struct {
		client ClientID
		ids    []IDRange
	}
	entries := make([]entry, 0, len(s.clients))
	for c, r := range s.clients {
		if ids := r.getIDs(); len(ids) > 0 {
			entries = append(entries, entry{c, ids})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].client > entries[j].client })
	enc.WriteVarUint(uint64(len(entries)))
	w := idSetRLEWriter{enc: enc}
	for _, e := range entries {
		w.reset()
		enc.WriteVarUint(uint64(e.client))
		enc.WriteVarUint(uint64(len(e.ids)))
		for _, rg := range e.ids {
			w.writeClock(rg.Clock)
			w.writeLen(rg.Len)
		}
	}
}

// readIDSet consumes one IDSet from dec.
func readIDSet(dec *encoding.Decoder) (*IDSet, error) {
	numClients, err := dec.ReadVarUint()
	if err != nil {
		return nil, attrDecodeErr("idset client count: %v", err)
	}
	// Ceiling (N-12): each client entry needs ≥2 bytes (client + range count).
	if numClients > uint64(dec.Remaining()) {
		return nil, attrDecodeErr("idset client count %d exceeds remaining input %d", numClients, dec.Remaining())
	}
	s := NewIDSet()
	r := idSetRLEReader{dec: dec}
	for i := uint64(0); i < numClients; i++ {
		r.reset()
		client, err := dec.ReadVarUint()
		if err != nil {
			return nil, attrDecodeErr("idset client: %v", err)
		}
		numRanges, err := dec.ReadVarUint()
		if err != nil {
			return nil, attrDecodeErr("idset range count: %v", err)
		}
		if numRanges > uint64(dec.Remaining())+1 { // each range needs ≥2 bytes; +1 tolerates final 1-byte pairs
			return nil, attrDecodeErr("idset range count %d exceeds remaining input %d", numRanges, dec.Remaining())
		}
		for j := uint64(0); j < numRanges; j++ {
			clock, err := r.readClock()
			if err != nil {
				return nil, attrDecodeErr("idset clock: %v", err)
			}
			length, err := r.readLen()
			if err != nil {
				return nil, attrDecodeErr("idset len: %v", err)
			}
			s.Add(ClientID(client), clock, length)
		}
	}
	return s, nil
}

// EncodeIDSet encodes s in the yjs-v14 IdSet binary format (IdSetEncoderV2).
func EncodeIDSet(s *IDSet) []byte {
	enc := encoding.NewEncoder()
	writeIDSet(enc, s)
	return enc.Bytes()
}

// DecodeIDSet decodes data produced by EncodeIDSet / yjs encodeIdSet.
func DecodeIDSet(data []byte) (*IDSet, error) {
	dec := encoding.NewDecoder(data)
	s, err := readIDSet(dec)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// writeIDMap appends the canonical encoding of m to enc (yjs writeIdMap). A
// nil m encodes as an empty IDMap, matching how the algebra ops
// (MergeIDMaps et al.) already treat a nil input as empty.
func writeIDMap(enc *encoding.Encoder, m *IDMap) {
	if m == nil {
		enc.WriteVarUint(0)
		return
	}
	type entry struct {
		client ClientID
		ids    []AttrRange
	}
	entries := make([]entry, 0, len(m.clients))
	for c, r := range m.clients {
		if ids := r.getIDs(); len(ids) > 0 {
			entries = append(entries, entry{c, ids})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].client < entries[j].client })
	enc.WriteVarUint(uint64(len(entries)))
	w := idSetRLEWriter{enc: enc}
	var lastClient uint64
	visitedAttrs := make(map[*ContentAttribute]uint64) // instance -> attrID (encounter order)
	visitedNames := make(map[string]uint64)            // name -> attrNameID (encounter order)
	for _, e := range entries {
		w.reset()
		enc.WriteVarUint(uint64(e.client) - lastClient)
		lastClient = uint64(e.client)
		enc.WriteVarUint(uint64(len(e.ids)))
		for _, rg := range e.ids {
			w.writeClock(rg.Clock)
			w.writeLen(rg.Len)
			enc.WriteVarUint(uint64(len(rg.Attrs)))
			for _, attr := range rg.Attrs {
				if id, ok := visitedAttrs[attr]; ok {
					enc.WriteVarUint(id)
					continue
				}
				id := uint64(len(visitedAttrs))
				visitedAttrs[attr] = id
				enc.WriteVarUint(id)
				if nameID, ok := visitedNames[attr.Name]; ok {
					enc.WriteVarUint(nameID)
				} else {
					nameID := uint64(len(visitedNames))
					visitedNames[attr.Name] = nameID
					enc.WriteVarUint(nameID)
					enc.WriteVarString(attr.Name)
				}
				enc.WriteAny(attr.Value)
			}
		}
	}
}

// readIDMap consumes one IDMap from dec.
func readIDMap(dec *encoding.Decoder) (*IDMap, error) {
	numClients, err := dec.ReadVarUint()
	if err != nil {
		return nil, attrDecodeErr("idmap client count: %v", err)
	}
	if numClients > uint64(dec.Remaining()) {
		return nil, attrDecodeErr("idmap client count %d exceeds remaining input %d", numClients, dec.Remaining())
	}
	m := NewIDMap()
	r := idSetRLEReader{dec: dec}
	var visitedAttrs []*ContentAttribute
	var visitedNames []string
	var lastClient uint64
	for i := uint64(0); i < numClients; i++ {
		r.reset()
		delta, err := dec.ReadVarUint()
		if err != nil {
			return nil, attrDecodeErr("idmap client delta: %v", err)
		}
		client := lastClient + delta
		lastClient = client
		numRanges, err := dec.ReadVarUint()
		if err != nil {
			return nil, attrDecodeErr("idmap range count: %v", err)
		}
		if numRanges > uint64(dec.Remaining())+1 {
			return nil, attrDecodeErr("idmap range count %d exceeds remaining input %d", numRanges, dec.Remaining())
		}
		for j := uint64(0); j < numRanges; j++ {
			clock, err := r.readClock()
			if err != nil {
				return nil, attrDecodeErr("idmap clock: %v", err)
			}
			length, err := r.readLen()
			if err != nil {
				return nil, attrDecodeErr("idmap len: %v", err)
			}
			attrCount, err := dec.ReadVarUint()
			if err != nil {
				return nil, attrDecodeErr("idmap attr count: %v", err)
			}
			if attrCount > uint64(dec.Remaining())+1 {
				return nil, attrDecodeErr("idmap attr count %d exceeds remaining input %d", attrCount, dec.Remaining())
			}
			rangeAttrs := make([]*ContentAttribute, 0, min(attrCount, maxPreallocAttrs))
			for k := uint64(0); k < attrCount; k++ {
				attrID, err := dec.ReadVarUint()
				if err != nil {
					return nil, attrDecodeErr("idmap attr id: %v", err)
				}
				if attrID < uint64(len(visitedAttrs)) {
					rangeAttrs = append(rangeAttrs, visitedAttrs[attrID])
					continue
				}
				if attrID != uint64(len(visitedAttrs)) {
					return nil, attrDecodeErr("idmap attr id %d skips table size %d", attrID, len(visitedAttrs))
				}
				nameID, err := dec.ReadVarUint()
				if err != nil {
					return nil, attrDecodeErr("idmap attr name id: %v", err)
				}
				var name string
				switch {
				case nameID < uint64(len(visitedNames)):
					name = visitedNames[nameID]
				case nameID == uint64(len(visitedNames)):
					name, err = dec.ReadVarString()
					if err != nil {
						return nil, attrDecodeErr("idmap attr name: %v", err)
					}
					visitedNames = append(visitedNames, name)
				default:
					return nil, attrDecodeErr("idmap attr name id %d skips table size %d", nameID, len(visitedNames))
				}
				value, err := dec.ReadAny()
				if err != nil {
					return nil, attrDecodeErr("idmap attr value: %v", err)
				}
				attr := &ContentAttribute{Name: name, Value: value}
				visitedAttrs = append(visitedAttrs, attr)
				rangeAttrs = append(rangeAttrs, attr)
			}
			m.Add(ClientID(client), clock, length, rangeAttrs)
		}
	}
	return m, nil
}

// EncodeIDMap encodes m in the yjs-v14 IdMap binary format. All attributes must
// come from NewContentAttribute (or otherwise stay within the lib0 any domain);
// hand-constructed literals with unsupported values panic in WriteAny.
func EncodeIDMap(m *IDMap) []byte {
	enc := encoding.NewEncoder()
	writeIDMap(enc, m)
	return enc.Bytes()
}

// DecodeIDMap decodes data produced by EncodeIDMap / yjs encodeIdMap.
func DecodeIDMap(data []byte) (*IDMap, error) {
	dec := encoding.NewDecoder(data)
	return readIDMap(dec)
}

// EncodeContentIDs encodes c as inserts then deletes, following yjs-main's
// writeContentIds composition (src/utils/meta.js): two IdSets concatenated.
// Each half's wire format is byte-verified against published yjs v14; the
// published yjs v14 rc line has no top-level writeContentIds/ContentIds
// export to pin the wrapper itself against (see attribution_js_compat_test.go).
func EncodeContentIDs(c ContentIDs) []byte {
	enc := encoding.NewEncoder()
	writeIDSet(enc, c.Inserts)
	writeIDSet(enc, c.Deletes)
	return enc.Bytes()
}

// DecodeContentIDs decodes data produced by EncodeContentIDs.
func DecodeContentIDs(data []byte) (ContentIDs, error) {
	dec := encoding.NewDecoder(data)
	inserts, err := readIDSet(dec)
	if err != nil {
		return ContentIDs{}, err
	}
	deletes, err := readIDSet(dec)
	if err != nil {
		return ContentIDs{}, err
	}
	return ContentIDs{Inserts: inserts, Deletes: deletes}, nil
}

// EncodeContentMap encodes c as inserts then deletes, following yjs-main's
// writeContentMap composition (src/utils/meta.js): two IdMaps concatenated.
// Each half's wire format is byte-verified against published yjs v14. The
// published yjs v14 rc line has no top-level writeContentMap/ContentMap
// export to pin the wrapper itself against — that API exists only on yjs's
// unreleased main branch (see attribution_js_compat_test.go and the
// follow-up issue to re-verify once yjs v14.0.0 final ships).
func EncodeContentMap(c ContentMap) []byte {
	enc := encoding.NewEncoder()
	writeIDMap(enc, c.Inserts)
	writeIDMap(enc, c.Deletes)
	return enc.Bytes()
}

// DecodeContentMap decodes data produced by EncodeContentMap.
func DecodeContentMap(data []byte) (ContentMap, error) {
	dec := encoding.NewDecoder(data)
	inserts, err := readIDMap(dec)
	if err != nil {
		return ContentMap{}, err
	}
	deletes, err := readIDMap(dec)
	if err != nil {
		return ContentMap{}, err
	}
	return ContentMap{Inserts: inserts, Deletes: deletes}, nil
}
