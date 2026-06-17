package crdt

import (
	"sort"

	"github.com/reearth/ygo/encoding"
)

// V2 struct-level merge / diff / state-vector extraction (#57). Mirrors the V1
// path in merge.go but drives the columnar V2 codec (newV2Decoder /
// decodeItemV2 / newV2Encoder / encodeItemV2). The merge/dedup/resolveParents
// logic in merge.go is format-agnostic (it operates on *Item), so only the
// decode and encode steps differ.

// decodeStructsV2 decodes every struct in a V2 update into per-client,
// clock-ordered slices (without integrating) plus the delete set.
func decodeStructsV2(scratch *Doc, update []byte) (map[ClientID][]*Item, DeleteSet, error) {
	dec, err := newV2Decoder(update)
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	numClients, err := dec.restDec.ReadVarUint()
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	if numClients > maxV2Items {
		return nil, DeleteSet{}, ErrInvalidUpdate
	}
	// Don't pre-size by the attacker-controlled numClients (see decodeStructsV1).
	out := make(map[ClientID][]*Item)
	total := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		total += numStructs
		if total > maxV2Items {
			return nil, DeleteSet{}, ErrInvalidUpdate
		}
		client, err := dec.readClient()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		clock, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, DeleteSet{}, wrapUpdateErr(err)
		}
		structs := out[client]
		for j := uint64(0); j < numStructs; j++ {
			info, err := dec.readInfo()
			if err != nil {
				return nil, DeleteSet{}, wrapUpdateErr(err)
			}
			switch info & 0x1F {
			case 0: // GC placeholder
				l, err := dec.readLen()
				if err != nil {
					return nil, DeleteSet{}, wrapUpdateErr(err)
				}
				structs = append(structs, &Item{
					ID:      ID{Client: client, Clock: clock},
					Content: NewContentDeleted(l),
					Deleted: true,
				})
				clock += uint64(l)
			case 10: // skip struct: a clock-range placeholder, not content.
				l, err := dec.restDec.ReadVarUint()
				if err != nil {
					return nil, DeleteSet{}, wrapUpdateErr(err)
				}
				// Advance clock but don't carry the skip (encodeItemV2 can't
				// encode it); the gap is re-emitted as a skip by the encoder.
				clock += l
			default:
				item, contentLen, err := decodeItemV2(dec, scratch, client, clock, info)
				if err != nil {
					return nil, DeleteSet{}, wrapUpdateErr(err)
				}
				structs = append(structs, item)
				clock += uint64(contentLen)
			}
		}
		out[client] = structs
	}
	ds, err := decodeDeleteSetV2(dec)
	if err != nil {
		return nil, DeleteSet{}, wrapUpdateErr(err)
	}
	return out, ds, nil
}

// encodeStructStoreV2 encodes per-client struct slices + a delete set into a V2
// update. Mirrors encodeStructStoreV1 but uses the columnar V2 encoder and the
// V2 conventions (clients written in DESCENDING order; skip length on the rest
// stream). store provides origin look-ups for encodeItemV2.
func encodeStructStoreV2(perClient map[ClientID][]*Item, ds DeleteSet, sv StateVector, store *StructStore) []byte {
	clients := make([]ClientID, 0, len(perClient))
	for c, items := range perClient {
		if len(items) > 0 {
			clients = append(clients, c)
		}
	}
	// Yjs V2 writes clients in descending order.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	type entry struct {
		item   *Item // nil => skip struct
		offset int
		skip   uint64
	}
	type group struct {
		client     ClientID
		startClock uint64
		entries    []entry
	}
	var groups []group
	for _, c := range clients {
		svClock := sv.Clock(c)
		var ents []entry
		var startClock, expected uint64
		started := false
		for _, it := range perClient[c] {
			end := it.ID.Clock + uint64(it.Content.Len())
			if end <= svClock {
				continue
			}
			offset := 0
			clk := it.ID.Clock
			if clk < svClock {
				offset = int(svClock - clk)
				clk = svClock
			}
			if !started {
				startClock = clk
				expected = clk
				started = true
			}
			if clk > expected {
				ents = append(ents, entry{skip: clk - expected})
			}
			ents = append(ents, entry{item: it, offset: offset})
			expected = end
		}
		if started {
			groups = append(groups, group{client: c, startClock: startClock, entries: ents})
		}
	}

	enc := newV2Encoder()
	enc.restEnc.WriteVarUint(uint64(len(groups)))
	for _, g := range groups {
		enc.restEnc.WriteVarUint(uint64(len(g.entries)))
		enc.writeClient(g.client)
		enc.restEnc.WriteVarUint(g.startClock)
		for _, e := range g.entries {
			if e.item == nil {
				enc.writeInfo(10) // skip struct
				enc.restEnc.WriteVarUint(e.skip)
				continue
			}
			encodeItemV2(enc, e.item, e.offset, store)
		}
	}
	encodeDeleteSetV2(enc, ds)
	return enc.toBytes()
}

// MergeUpdatesV2 merges several V2 updates into one at the struct level
// (preserving non-integrable structs and clock gaps; unioning delete sets). (#57)
func MergeUpdatesV2(updates ...[]byte) ([]byte, error) {
	perClient, ds, store, err := buildMergeStore(updates, decodeStructsV2)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV2(perClient, ds, StateVector{}, store), nil
}

// DiffUpdateV2 returns the portion of a V2 update missing from sv (structs
// sliced at the sv boundary; full delete set preserved). (#57)
func DiffUpdateV2(update []byte, sv StateVector) ([]byte, error) {
	perClient, ds, store, err := buildMergeStore([][]byte{update}, decodeStructsV2)
	if err != nil {
		return nil, err
	}
	return encodeStructStoreV2(perClient, ds, sv, store), nil
}

// EncodeStateVectorFromUpdateV2 extracts the state vector described by a V2
// update (next clock per client) without integrating it. The state vector wire
// format is identical to V1 (client/clock varuint pairs). (#57)
func EncodeStateVectorFromUpdateV2(update []byte) ([]byte, error) {
	perClient, _, _, err := buildMergeStore([][]byte{update}, decodeStructsV2)
	if err != nil {
		return nil, err
	}
	sv := make(StateVector, len(perClient))
	for c, items := range perClient {
		var maxEnd uint64
		for _, it := range items {
			if end := it.ID.Clock + uint64(it.Content.Len()); end > maxEnd {
				maxEnd = end
			}
		}
		sv[c] = maxEnd
	}
	clients := clientsSorted(sv)
	return encoding.EncodeBytes(func(enc *encoding.Encoder) {
		enc.WriteVarUint(uint64(len(clients)))
		for _, c := range clients {
			enc.WriteVarUint(uint64(c))
			enc.WriteVarUint(sv[c])
		}
	}), nil
}
