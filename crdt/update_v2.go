package crdt

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// maxV2Items caps the total number of structs decoded from a single V2 update to
// prevent RLE-compressed pathological inputs from causing unbounded loops or OOM.
const maxV2Items = uint64(1 << 20) // 1 million items

// ── V2 encoder state ──────────────────────────────────────────────────────────

type v2Encoder struct {
	keyClock int

	keyClockEnc   encoding.IntDiffOptRleEncoder
	clientEnc     encoding.UintOptRleEncoder
	leftClockEnc  encoding.IntDiffOptRleEncoder
	rightClockEnc encoding.IntDiffOptRleEncoder
	infoEnc       encoding.RleByteEncoder
	stringEnc     encoding.StringEncoder
	parentInfoEnc encoding.RleByteEncoder
	typeRefEnc    encoding.UintOptRleEncoder
	lenEnc        encoding.UintOptRleEncoder
	restEnc       *encoding.Encoder

	dsCurrVal uint64
}

func newV2Encoder() *v2Encoder {
	return &v2Encoder{
		restEnc: encoding.NewEncoder(),
	}
}

func (e *v2Encoder) toBytes() []byte {
	out := encoding.NewEncoder()
	out.WriteVarUint(0) // feature flag
	out.WriteVarBytes(e.keyClockEnc.Bytes())
	out.WriteVarBytes(e.clientEnc.Bytes())
	out.WriteVarBytes(e.leftClockEnc.Bytes())
	out.WriteVarBytes(e.rightClockEnc.Bytes())
	out.WriteVarBytes(e.infoEnc.Bytes())
	out.WriteVarBytes(e.stringEnc.Bytes())
	out.WriteVarBytes(e.parentInfoEnc.Bytes())
	out.WriteVarBytes(e.typeRefEnc.Bytes())
	out.WriteVarBytes(e.lenEnc.Bytes())
	// restEncoder is appended raw (no length prefix) — matches Yjs UpdateEncoderV2.toUint8Array
	out.WriteRaw(e.restEnc.Bytes())
	return out.Bytes()
}

func (e *v2Encoder) writeClient(client ClientID) {
	e.clientEnc.Write(uint64(client))
}

func (e *v2Encoder) writeInfo(info byte) {
	e.infoEnc.Write(info)
}

func (e *v2Encoder) writeLeftID(id ID) {
	e.clientEnc.Write(uint64(id.Client))
	e.leftClockEnc.Write(int64(id.Clock))
}

func (e *v2Encoder) writeRightID(id ID) {
	e.clientEnc.Write(uint64(id.Client))
	e.rightClockEnc.Write(int64(id.Clock))
}

func (e *v2Encoder) writeParentInfo(isYKey bool) {
	if isYKey {
		e.parentInfoEnc.Write(1)
	} else {
		e.parentInfoEnc.Write(0)
	}
}

func (e *v2Encoder) writeString(s string) {
	e.stringEnc.Write(s)
}

func (e *v2Encoder) writeTypeRef(ref byte) {
	e.typeRefEnc.Write(uint64(ref))
}

func (e *v2Encoder) writeLen(l uint64) {
	e.lenEnc.Write(l)
}

// writeKey mirrors Yjs UpdateEncoderV2.writeKey EXACTLY — including its
// deliberately-disabled key deduplication. In yjs the `keyMap.set(key,
// keyClock)` line is commented out ("I forgot to set the keyclock. So
// everything was working fine."), so the reference encoder writes a fresh
// keyClock + string for EVERY occurrence of a key (ContentFormat keys,
// XmlElement node names, parentSub). Populating the map here made ygo emit
// back-references the yjs encoder never produces: V2 bytes drifted from the
// reference for any repeated key (e.g. two `strong` format items, repeated
// `paragraph` node names), and — per the yjs source comment — older yjs
// clients decode ContentFormat keys with readString and cannot read deduped
// updates at all. The DECODER (readKey) keeps dedup support, exactly like
// Yjs's readKey. (#yxml-wire)
func (e *v2Encoder) writeKey(key string) {
	e.keyClockEnc.Write(int64(e.keyClock))
	e.keyClock++
	e.stringEnc.Write(key)
}

func (e *v2Encoder) resetDsCurVal() { e.dsCurrVal = 0 }

func (e *v2Encoder) writeDsClock(clock uint64) {
	diff := clock - e.dsCurrVal
	e.dsCurrVal = clock
	e.restEnc.WriteVarUint(diff)
}

func (e *v2Encoder) writeDsLen(l uint64) {
	e.restEnc.WriteVarUint(l - 1)
	e.dsCurrVal += l
}

// ── V2 decoder state ──────────────────────────────────────────────────────────

type v2Decoder struct {
	keyClockDec   anycodec.IntDiffOptRleDecoder
	clientDec     anycodec.UintOptRleDecoder
	leftClockDec  anycodec.IntDiffOptRleDecoder
	rightClockDec anycodec.IntDiffOptRleDecoder
	infoDec       anycodec.RleByteDecoder
	stringDec     anycodec.StringDecoder
	parentInfoDec anycodec.RleByteDecoder
	typeRefDec    anycodec.UintOptRleDecoder
	lenDec        anycodec.UintOptRleDecoder
	restDec       *encoding.Decoder

	keys      []string
	dsCurrVal uint64
}

func newV2Decoder(data []byte) (*v2Decoder, error) {
	dec := encoding.NewDecoder(data)

	// Feature flag (currently unused, always 0)
	if _, err := dec.ReadVarUint(); err != nil {
		return nil, fmt.Errorf("%w: V2 feature flag: %v", ErrInvalidUpdate, err)
	}

	read := func(name string) ([]byte, error) {
		b, err := dec.ReadVarBytes()
		if err != nil {
			return nil, fmt.Errorf("%w: V2 %s: %v", ErrInvalidUpdate, name, err)
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		return cp, nil
	}

	keyClockBytes, err := read("keyClockEncoder")
	if err != nil {
		return nil, err
	}
	clientBytes, err := read("clientEncoder")
	if err != nil {
		return nil, err
	}
	leftClockBytes, err := read("leftClockEncoder")
	if err != nil {
		return nil, err
	}
	rightClockBytes, err := read("rightClockEncoder")
	if err != nil {
		return nil, err
	}
	infoBytes, err := read("infoEncoder")
	if err != nil {
		return nil, err
	}
	stringBytes, err := read("stringEncoder")
	if err != nil {
		return nil, err
	}
	parentInfoBytes, err := read("parentInfoEncoder")
	if err != nil {
		return nil, err
	}
	typeRefBytes, err := read("typeRefEncoder")
	if err != nil {
		return nil, err
	}
	lenBytes, err := read("lenEncoder")
	if err != nil {
		return nil, err
	}

	// Remaining bytes = restDecoder (raw, no length prefix)
	remaining := dec.RemainingBytes()

	stringDec, err := anycodec.NewStringDecoder(stringBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: V2 stringDecoder: %v", ErrInvalidUpdate, err)
	}

	return &v2Decoder{
		keyClockDec:   anycodec.NewIntDiffOptRleDecoder(keyClockBytes),
		clientDec:     anycodec.NewUintOptRleDecoder(clientBytes),
		leftClockDec:  anycodec.NewIntDiffOptRleDecoder(leftClockBytes),
		rightClockDec: anycodec.NewIntDiffOptRleDecoder(rightClockBytes),
		infoDec:       anycodec.NewRleByteDecoder(infoBytes),
		stringDec:     stringDec,
		parentInfoDec: anycodec.NewRleByteDecoder(parentInfoBytes),
		typeRefDec:    anycodec.NewUintOptRleDecoder(typeRefBytes),
		lenDec:        anycodec.NewUintOptRleDecoder(lenBytes),
		restDec:       encoding.NewDecoder(remaining),
	}, nil
}

func (d *v2Decoder) readClient() (ClientID, error) {
	v, err := d.clientDec.Read()
	return ClientID(v), err
}

func (d *v2Decoder) readInfo() (byte, error) {
	return d.infoDec.Read()
}

func (d *v2Decoder) readLeftID() (ID, error) {
	client, err := d.clientDec.Read()
	if err != nil {
		return ID{}, err
	}
	clock, err := d.leftClockDec.Read()
	if err != nil {
		return ID{}, err
	}
	return v2ID(client, clock)
}

func (d *v2Decoder) readRightID() (ID, error) {
	client, err := d.clientDec.Read()
	if err != nil {
		return ID{}, err
	}
	clock, err := d.rightClockDec.Read()
	if err != nil {
		return ID{}, err
	}
	return v2ID(client, clock)
}

// v2ID is the ID the client and clock columns give, refused when the clock,
// a running sum of diffs, is below 0 (as uint64 it would be past 2^63) or
// past maxSafeInteger.
func v2ID(client uint64, clock int64) (ID, error) {
	if clock < 0 || clock > maxSafeInteger {
		return ID{}, ErrInvalidUpdate
	}
	return ID{Client: ClientID(client), Clock: uint64(clock)}, nil
}

func (d *v2Decoder) readParentInfo() (bool, error) {
	b, err := d.parentInfoDec.Read()
	return b == 1, err
}

func (d *v2Decoder) readString() (string, error) {
	return d.stringDec.Read()
}

func (d *v2Decoder) readTypeRef() (byte, error) {
	v, err := d.typeRefDec.Read()
	if v > math.MaxUint8 {
		// A ref past a byte names no type; truncating it could alias a known
		// ref, such as 261 to the hook's 5, and read a name it does not have.
		return math.MaxUint8, err
	}
	return byte(v), err
}

func (d *v2Decoder) readLen() (uint64, error) {
	return d.lenDec.Read()
}

func (d *v2Decoder) readKey() (string, error) {
	keyClock, err := d.keyClockDec.Read()
	if err != nil {
		return "", err
	}
	if keyClock < 0 {
		return "", ErrInvalidUpdate
	}
	if uint64(keyClock) < uint64(len(d.keys)) {
		return d.keys[keyClock], nil
	}
	key, err := d.stringDec.Read()
	if err != nil {
		return "", err
	}
	d.keys = append(d.keys, key)
	return key, nil
}

func (d *v2Decoder) readAny() (any, error) {
	return d.restDec.ReadAny()
}

func (d *v2Decoder) readBuf() ([]byte, error) {
	b, err := d.restDec.ReadVarBytes()
	if err != nil {
		return nil, err
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func (d *v2Decoder) resetDsCurVal() { d.dsCurrVal = 0 }

// readDsClock reads a delete range's clock, a diff from the end of the range
// before, refused past maxSafeInteger.
func (d *v2Decoder) readDsClock() (uint64, error) {
	diff, err := d.restDec.ReadVarUint()
	if err != nil {
		return 0, err
	}
	if d.dsCurrVal, err = structEnd(d.dsCurrVal, diff); err != nil {
		return 0, err
	}
	if d.dsCurrVal > maxSafeInteger {
		return 0, ErrInvalidUpdate
	}
	return d.dsCurrVal, nil
}

// readDsLen reads a delete range's length, refused when the range would reach
// a clock past maxSafeInteger.
func (d *v2Decoder) readDsLen() (uint64, error) {
	v, err := d.restDec.ReadVarUint()
	if err != nil {
		return 0, err
	}
	l := v + 1
	if d.dsCurrVal, err = structEnd(d.dsCurrVal, l); err != nil {
		return 0, err
	}
	return l, nil
}

// ── V2 encoding ───────────────────────────────────────────────────────────────

func encodeV2Locked(doc *Doc, sv StateVector) []byte {
	enc := newV2Encoder()

	type clientGroup struct {
		client     ClientID
		items      []*Item
		startClock uint64
	}

	var groups []clientGroup
	for client, items := range doc.store.clients {
		svClock := sv.Clock(client)
		var relevant []*Item
		for _, item := range items {
			if item.ID.Clock+item.Content.Len() > svClock {
				relevant = append(relevant, item)
			}
		}
		if len(relevant) > 0 {
			groups = append(groups, clientGroup{client, relevant, svClock})
		}
	}
	// Yjs V2 writes clients in DESCENDING order.
	sort.Slice(groups, func(i, j int) bool { return groups[i].client > groups[j].client })

	enc.restEnc.WriteVarUint(uint64(len(groups)))
	for _, g := range groups {
		enc.restEnc.WriteVarUint(uint64(len(g.items)))
		enc.writeClient(g.client)
		enc.restEnc.WriteVarUint(g.startClock)
		for i, item := range g.items {
			offset := uint64(0)
			if i == 0 && g.startClock > item.ID.Clock {
				offset = g.startClock - item.ID.Clock
			}
			encodeItemV2(enc, item, offset)
		}
	}

	encodeDeleteSetV2(enc, buildDeleteSet(doc.store))
	return enc.toBytes()
}

func encodeItemV2(enc *v2Encoder, item *Item, offset uint64) {
	// Orphaned items (no parent) came from GC wire format where the parent
	// type name is lost. Encode as GC struct for valid clock accounting.
	//
	// Require no origins too: a struct-level-decoded item (MergeUpdatesV2 /
	// DiffUpdateV2) that infers its parent from an origin legitimately has a nil
	// Parent but a valid Origin/OriginRight — encode it as a normal item, not a
	// GC placeholder, or its content is lost. Parent info is only written in the
	// no-origin branch below. (#125 / #57)
	//
	// Require no deferred parent-by-ID either: a struct-level-decoded item whose
	// container is in a later client group has a nil Parent but a valid parentID.
	// Treat it as a normal item and re-emit the parent-by-ID in the no-origin
	// branch below; GC-orphaning it here would drop its content. Mirrors the V1
	// encodeItem fix. (#140)
	if item.Parent == nil && item.Origin == nil && item.OriginRight == nil && item.parentID == nil {
		length := item.Content.Len()
		if offset > 0 {
			length -= offset
		}
		enc.writeInfo(0) // GC struct
		enc.writeLen(length)
		return
	}

	var origin, originRight *ID
	if offset > 0 {
		oc := ID{Client: item.ID.Client, Clock: item.ID.Clock + offset - 1}
		origin = &oc
		originRight = item.OriginRight
	} else {
		origin = item.Origin
		originRight = item.OriginRight
	}

	contentTag := contentTagOf(item.Content)
	info := contentTag
	if origin != nil {
		info |= flagHasOrigin
	}
	if originRight != nil {
		info |= flagHasRightOrigin
	}
	cantCopyParentInfo := origin == nil && originRight == nil
	// BIT6 reflects "has a key" whenever ParentSub is present — matching Yjs,
	// which sets it regardless of origin presence. The string itself is still
	// written only inside the no-origin block below. (#YMap-wire)
	if item.ParentSub != nil {
		info |= flagHasParentSub
	}
	enc.writeInfo(info)

	if origin != nil {
		enc.writeLeftID(*origin)
	}
	if originRight != nil {
		enc.writeRightID(*originRight)
	}

	if cantCopyParentInfo {
		if item.Parent != nil && item.Parent.item != nil {
			// Nested type: parent identified by its item's ID.
			enc.writeParentInfo(false)
			enc.writeLeftID(item.Parent.item.ID)
		} else if item.parentID != nil {
			// Deferred parent-by-ID: decoded but not yet integrated (its container
			// is in a later client group), so Parent is still nil while parentID
			// holds the container's ID. Re-emit the explicit parent-by-ID so the
			// receiver can defer and resolve it too. Mirrors encodeItem.
			enc.writeParentInfo(false)
			enc.writeLeftID(*item.parentID)
		} else {
			// Root named type; an item with no parent is a GC struct, written above.
			enc.writeParentInfo(true)
			enc.writeString(item.Parent.name)
		}
		if item.ParentSub != nil {
			enc.writeString(*item.ParentSub)
		}
	}

	encodeContentV2(enc, item.Content, offset)
}

func contentTagOf(c Content) byte {
	switch c.(type) {
	case *ContentDeleted:
		return wireDeleted
	case *ContentJSON:
		return wireJSON
	case *ContentBinary:
		return wireBinary
	case *ContentString:
		return wireString
	case *ContentEmbed:
		return wireEmbed
	case *ContentFormat:
		return wireFormat
	case *ContentType:
		return wireType
	case *ContentAny:
		return wireAny
	case *ContentDoc:
		return wireDoc
	case *ContentMove:
		return wireMove
	default:
		return wireAny
	}
}

func encodeContentV2(enc *v2Encoder, c Content, offset uint64) {
	switch ct := c.(type) {
	case *ContentDeleted:
		enc.writeLen(ct.length - offset)
	case *ContentJSON:
		vals := ct.Vals[offset:]
		enc.writeLen(uint64(len(vals)))
		for _, v := range vals {
			enc.writeString(fmtValToJSON(v))
		}
	case *ContentBinary:
		enc.restEnc.WriteVarBytes(ct.Data)
	case *ContentString:
		// Emit only the tail from `offset`. splitUTF16 emits a leading U+FFFD
		// when offset bisects a surrogate pair, matching Yjs's mid-surrogate slice.
		_, tail := splitUTF16(ct.Str, int(min(offset, uint64(ct.utf16Len))))
		enc.writeString(tail)
	case *ContentEmbed:
		enc.restEnc.WriteAny(ct.Val)
	case *ContentFormat:
		enc.writeKey(ct.Key)
		enc.restEnc.WriteAny(ct.Val)
	case *ContentType:
		tc, name := typeClassOf(ct)
		enc.writeTypeRef(tc)
		if typeRefHasName(tc) {
			enc.writeKey(name)
		}
	case *ContentAny:
		vals := ct.Vals[offset:]
		enc.writeLen(uint64(len(vals)))
		for _, v := range vals {
			enc.restEnc.WriteAny(v)
		}
	case *ContentDoc:
		guid := ""
		if ct.Doc != nil {
			guid = ct.Doc.GUID()
		}
		enc.writeString(guid)
		writeSubdocOpts(enc.restEnc, ct.Doc)
	case *ContentMove:
		enc.restEnc.WriteVarUint(uint64(ct.Target.Client))
		enc.restEnc.WriteVarUint(ct.Target.Clock)
		enc.restEnc.WriteVarUint(ct.TargetLen)
	}
}

func encodeDeleteSetV2(enc *v2Encoder, ds DeleteSet) {
	clients := make([]ClientID, 0, len(ds.clients))
	for c, ranges := range ds.clients {
		if len(nonEmptyRanges(ranges)) > 0 {
			clients = append(clients, c)
		}
	}
	// Yjs V2 writes delete set clients in DESCENDING order.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	enc.restEnc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		enc.resetDsCurVal()
		enc.restEnc.WriteVarUint(uint64(c))
		ranges := nonEmptyRanges(ds.clients[c])
		enc.restEnc.WriteVarUint(uint64(len(ranges)))
		for _, r := range ranges {
			enc.writeDsClock(r.Clock)
			enc.writeDsLen(r.Len)
		}
	}
}

// nonEmptyRanges returns ranges without zero-length entries, which delete
// nothing and have no V2 encoding (lengths are written as len-1). It returns
// ranges itself when there are none to drop.
func nonEmptyRanges(ranges []DeleteRange) []DeleteRange {
	for i, r := range ranges {
		if r.Len > 0 {
			continue
		}
		kept := append(make([]DeleteRange, 0, len(ranges)-1), ranges[:i]...)
		for _, r := range ranges[i+1:] {
			if r.Len > 0 {
				kept = append(kept, r)
			}
		}
		return kept
	}
	return ranges
}

// ── V2 decoding ───────────────────────────────────────────────────────────────

func applyV2Txn(txn *Transaction, update []byte) (retErr error) {
	// Same panic recovery as applyV1Txn — guard against crafted V2 updates
	// that force a Splice on non-splittable content types.
	applying := false
	defer func() {
		if r := recover(); r != nil {
			retErr = recoveredApplyErr(r)
		}
		// An apply that fails before it began to integrate must tell no observer.
		txn.refused = retErr != nil && !applying
		if applying {
			retErr = drainAfterApply(txn, retErr)
		}
		endRemoteApply(txn, retErr != nil)
		retErr = endApplyTally(txn, retErr)
	}()

	sv := txn.doc.store.StateVector()
	beginRemoteApply(txn, sv)
	beginApplyTally(txn)
	dec, err := newV2Decoder(update)
	if err != nil {
		return err
	}

	initialV2 := *dec
	budget := newPendingBudget(txn.doc, sv, update, true)
	budget.v2Start = &initialV2
	budget.v2Rest = *dec.restDec
	// Peek on a copy, since the pending-cap check reads from the apply's start.
	peek := budget.v2Rest
	numClients, err := peek.ReadVarUint()
	if err != nil {
		return wrapUpdateErr(err)
	}
	// Guard against pathological inputs that encode huge counts via RLE compression.
	// No realistic update has more client groups than its byte count.
	if numClients > maxV2Items {
		return ErrInvalidUpdate
	}

	var pending []*Item
	if numClients > 1 {
		// Fail before integrating anything: a later malformed group must not
		// leave earlier, superseded groups applied; the walk also counts the
		// structs, bounding the cap check.
		if err := budget.knowSuperseded(); err != nil {
			return wrapUpdateErr(err)
		}
	} else {
		budget.setSuperseded(nil)
	}
	superseded := budget.superseded
	// The pending cap refuses an update before the apply changes anything, for
	// its items (checkBeforeApply) or its delete ranges (checkParkedDeletes),
	// both reading the update through the apply's one budget.
	if err := budget.checkBeforeApply(len(update)); err != nil {
		return err
	}
	if err := checkParkedDeletes(txn.doc, &budget); err != nil {
		return err
	}
	*dec.restDec = peek

	// Every struct decodes before one integrates, as Yjs reads them all first,
	// so a struct that does not decode leaves the document unchanged; a
	// keystroke's few structs stay on the stack (see decodeStructs).
	var buf [8]*Item
	group := buf[:0]
	var groupClient ClientID
	totalStructs := uint64(0)
	for i := uint64(0); i < numClients; i++ {
		numStructs, err := dec.restDec.ReadVarUint()
		if err != nil {
			return wrapUpdateErr(err)
		}
		totalStructs += numStructs
		if totalStructs > maxV2Items {
			return ErrInvalidUpdate
		}
		client, err := dec.readClient()
		if err != nil {
			return wrapUpdateErr(err)
		}
		clock, err := dec.restDec.ReadVarUint()
		if err != nil {
			return wrapUpdateErr(err)
		}
		groupClient = client

		existingEnd := sv.Clock(client)
		drop := superseded.has(i)
		if numClients == 1 {
			// Reserve the list up front instead of regrowing it each time it fills,
			// with the requested capacity bounded by the input length for a false count.
			group = slices.Grow(group, int(min(numStructs, uint64(len(update)))))
		}

		kept := false // Yjs's rest takes a client's structs from one that waits
		for j := uint64(0); j < numStructs; j++ {
			// Each struct is a value the apply reads, counted before it is read.
			txn.doc.tally().decode(1)
			info, err := dec.readInfo()
			if err != nil {
				return wrapUpdateErr(err)
			}

			var item *Item
			var contentLen uint64

			switch info & 0x1F {
			case 0: // GC (garbage collected)
				l, err := dec.readLen()
				if err != nil {
					return wrapUpdateErr(err)
				}
				// Decode the GC struct into the same placeholder Item the V1
				// decoder builds (update.go decodeItem, tag 0), so it flows
				// through the identical clock-accounting, park and offset logic
				// instead of short-circuiting past it, which left every later
				// struct of the client falsely gapped. As in Yjs, a GC struct
				// adds nothing to the transaction's delete set.
				item = &Item{
					ID:      ID{Client: client, Clock: clock},
					Content: NewContentDeleted(l),
					Deleted: true,
				}
				contentLen = l

			case 10: // Skip struct: withheld clocks; advance the cursor only (see V1 decodeStructs)
				l, err := dec.restDec.ReadVarUint()
				if err != nil {
					return wrapUpdateErr(err)
				}
				if clock, err = structEnd(clock, l); err != nil {
					return err
				}
				continue

			default:
				var derr error
				item, contentLen, derr = decodeItemV2(dec, txn.doc, client, clock, info)
				if derr != nil {
					return wrapUpdateErr(derr)
				}
			}

			// Every struct is bounded, a replaced group's too, so apply refuses
			// what the merge decoders refuse.
			itemEnd, err := structEnd(clock, contentLen)
			if err != nil {
				return err
			}
			if itemEnd == clock {
				return wrapUpdateErr(errZeroLengthStruct)
			}
			switch {
			case drop:
			case numClients > 1:
				// Several clients' structs integrate in Yjs's order once all
				// are decoded, as in the V1 decoder.
				if kept || itemEnd > existingEnd || hasMissingDep(txn.doc.store, item) || namesFutureOwn(item, existingEnd) {
					pending, kept = append(pending, item), true
				}
			default:
				group = append(group, item)
			}
			clock = itemEnd
		}
	}
	applying = true
	if numClients == 1 {
		if pending, err = integrateGroup(txn, group, sv.Clock(groupClient), pending); err != nil {
			return wrapUpdateErr(err)
		}
	}

	if err := resolveWithinUpdatePending(txn, pending); err != nil {
		return err
	}

	// Decode delete set, unless the parked-range check read it whole.
	ds, dsRanges, dsErr := budget.deleteSet, budget.deleteSetRanges, error(nil)
	if !budget.deleteSetRead {
		ds, dsRanges, dsErr = decodeDeleteSetV2Ranges(dec)
	}
	countDecodedRanges(txn, dsRanges)
	if err := applyDecodedDeleteSet(txn, ds, dsErr); err != nil {
		return err
	}

	return nil
}

func decodeItemV2(dec *v2Decoder, doc *Doc, client ClientID, clock uint64, info byte) (*Item, uint64, error) {
	hasOrigin := info&flagHasOrigin != 0
	hasRightOrigin := info&flagHasRightOrigin != 0
	hasParentSub := info&flagHasParentSub != 0

	var origin, originRight *ID

	if hasOrigin {
		id, err := dec.readLeftID()
		if err != nil {
			return nil, 0, err
		}
		origin = &id
	}

	if hasRightOrigin {
		id, err := dec.readRightID()
		if err != nil {
			return nil, 0, err
		}
		originRight = &id
	}

	var parent *abstractType
	var parentSub *string
	var parentByID *ID // deferred container ref when the parent isn't integrated yet

	cantCopyParentInfo := !hasOrigin && !hasRightOrigin
	if cantCopyParentInfo {
		isYKey, err := dec.readParentInfo()
		if err != nil {
			return nil, 0, err
		}
		if isYKey {
			// Named root type.
			name, err := dec.readString()
			if err != nil {
				return nil, 0, err
			}
			if err := refuseRoot(doc, name); err != nil {
				return nil, 0, err
			}
			parent = doc.getOrCreateType(name)
		} else {
			// Parent by item ID.
			pid, err := dec.readLeftID()
			if err != nil {
				return nil, 0, err
			}
			parentItem := doc.store.Find(pid)
			if parentItem == nil {
				// Container not integrated yet: V2 sorts client groups descending,
				// so a higher-clientID child decodes before its lower-clientID
				// parent. Defer instead of hard-failing; the resolve/drain pass
				// integrates it once the container arrives (C-3 parity with V1). (#140)
				parentByID = &pid
			} else if ct, ok := parentItem.Content.(*ContentType); ok {
				parent = ct.Type
			} else {
				// Tombstoned/GC'd container: defer as an orphan reference instead
				// of hard-failing (Yjs parent=nil). See update.go decodeItem.
				parentByID = &pid
			}
		}

		if hasParentSub {
			sub, err := dec.readString()
			if err != nil {
				return nil, 0, err
			}
			parentSub = &sub
		}
	}

	content, err := decodeContentV2(dec, doc, info&0x1F)
	if err != nil {
		return nil, 0, err
	}

	item := &Item{
		ID:          ID{Client: client, Clock: clock},
		Origin:      origin,
		OriginRight: originRight,
		Parent:      parent,
		ParentSub:   parentSub,
		parentID:    parentByID,
		Content:     content,
	}

	// Infer parent (and parentSub) from origin when not explicitly encoded.
	// A keyed item written after the same key (LWW) has an origin and so no
	// on-wire parentSub; it inherits the key from its left/origin neighbour.
	// Without this the item integrates with an empty key and is dropped from
	// the parent's itemMap → silent loss on V2. (#YMap-wire)
	if item.Parent == nil {
		if origin != nil {
			if oi := doc.store.Find(*origin); oi != nil {
				item.Parent = oi.Parent
				if item.ParentSub == nil {
					item.ParentSub = oi.ParentSub
				}
			}
		} else if originRight != nil {
			if ori := doc.store.Find(*originRight); ori != nil {
				item.Parent = ori.Parent
				if item.ParentSub == nil {
					item.ParentSub = ori.ParentSub
				}
			}
		}
	}

	// item.Parent may be nil when origin items belong to a client group not
	// yet decoded in this update. The caller retries these after all groups.
	return item, content.Len(), nil
}

func decodeContentV2(dec *v2Decoder, doc *Doc, tag byte) (Content, error) {
	switch tag {
	case wireDeleted:
		l, err := dec.readLen()
		if err != nil {
			return nil, err
		}
		return NewContentDeleted(l), nil

	case wireJSON:
		l, err := dec.readLen()
		if err != nil {
			return nil, err
		}
		if l > maxV2Items {
			return nil, ErrInvalidUpdate
		}
		// The declared count is charged before the list is made; what each
		// value holds is counted off the value its text decoded to.
		tally := doc.tally()
		tally.decode(int(l))
		vals := make([]any, l)
		for i := range vals {
			s, err := dec.readString()
			if err != nil {
				return nil, err
			}
			if vals[i], err = fmtValFromJSON(s); err != nil {
				return nil, err
			}
			tally.nested(vals[i])
		}
		return NewContentJSON(vals...), nil

	case wireBinary:
		b, err := dec.readBuf()
		if err != nil {
			return nil, err
		}
		return NewContentBinary(b), nil

	case wireString:
		s, err := dec.readString()
		if err != nil {
			return nil, err
		}
		return NewContentString(s), nil

	case wireEmbed:
		countAnyRun(doc.tally(), dec.restDec, 1)
		v, err := dec.readAny()
		if err != nil {
			return nil, err
		}
		return NewContentEmbed(v), nil

	case wireFormat:
		key, err := dec.readKey()
		if err != nil {
			return nil, err
		}
		countAnyRun(doc.tally(), dec.restDec, 1)
		val, err := dec.readAny()
		if err != nil {
			return nil, err
		}
		return NewContentFormat(key, val), nil

	case wireType:
		typeRef, err := dec.readTypeRef()
		if err != nil {
			return nil, err
		}
		at, err := decodeTypeContentV2(dec, doc, typeRef)
		if err != nil {
			return nil, err
		}
		return NewContentType(at), nil

	case wireAny:
		l, err := dec.readLen()
		if err != nil {
			return nil, err
		}
		if l > maxV2Items {
			return nil, ErrInvalidUpdate
		}
		// Every value is counted off the frame before the list is made.
		countAnyRun(doc.tally(), dec.restDec, l)
		vals := make([]any, l)
		for i := range vals {
			v, err := dec.readAny()
			if err != nil {
				return nil, err
			}
			vals[i] = v
		}
		return NewContentAny(vals...), nil

	case wireDoc:
		if doc.wholeOrNothing {
			return nil, errWholeOrNothingContent
		}
		guid, err := dec.readString()
		if err != nil {
			return nil, err
		}
		countAnyRun(doc.tally(), dec.restDec, 1)
		optsAny, err := dec.readAny()
		if err != nil {
			return nil, err
		}
		return NewContentDoc(newSubdocFromOpts(guid, optsAny)), nil

	case wireMove:
		if doc.wholeOrNothing {
			return nil, errWholeOrNothingContent
		}
		clientU, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := readClock(dec.restDec)
		if err != nil {
			return nil, err
		}
		targetLen, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		target := &ID{Client: ClientID(clientU), Clock: clock}
		return NewContentMove(target, targetLen), nil

	default:
		return nil, unknownTagErr(true, tag)
	}
}

func decodeTypeContentV2(dec *v2Decoder, doc *Doc, typeRef byte) (*abstractType, error) {
	switch typeRef {
	case 0: // YArray
		arr := &YArray{}
		arr.doc = doc
		arr.itemMap = make(map[string]*Item)
		arr.owner = arr
		return &arr.abstractType, nil

	case 1: // YMap
		m := &YMap{}
		m.doc = doc
		m.itemMap = make(map[string]*Item)
		m.owner = m
		return &m.abstractType, nil

	case 2: // YText
		txt := &YText{}
		txt.doc = doc
		txt.itemMap = make(map[string]*Item)
		txt.owner = txt
		return &txt.abstractType, nil

	case 3: // YXmlElement — reads node name via readKey
		nodeName, err := dec.readKey()
		if err != nil {
			return nil, err
		}
		elem := NewYXmlElement(nodeName)
		elem.doc = doc
		return &elem.abstractType, nil

	case 4: // YXmlFragment
		frag := &YXmlFragment{}
		frag.doc = doc
		frag.itemMap = make(map[string]*Item)
		frag.owner = frag
		return &frag.abstractType, nil

	case 5: // YXmlHook — reads hook name via readKey
		hookName, err := dec.readKey()
		if err != nil {
			return nil, err
		}
		h := newYXmlHook(hookName)
		h.doc = doc
		return &h.abstractType, nil

	case 6: // YXmlText
		xt := NewYXmlText()
		xt.doc = doc
		return &xt.abstractType, nil

	default:
		return nil, typeRefError(typeRef)
	}
}

// decodeDeleteSetV2 is decodeDeleteSet for V2, with the same result after an error.
func decodeDeleteSetV2(dec *v2Decoder) (DeleteSet, error) {
	ds, _, err := decodeDeleteSetV2Ranges(dec)
	return ds, err
}

// decodeDeleteSetV2Ranges is decodeDeleteSetRanges for V2.
func decodeDeleteSetV2Ranges(dec *v2Decoder) (ds DeleteSet, ranges int, err error) {
	defer func() {
		for c := range ds.clients {
			ds.sortAndCompact(c)
		}
	}()
	n, err := dec.restDec.ReadVarUint()
	if err != nil {
		return newOrderedDeleteSet(), 0, err
	}
	// A counting first pass sizes every list once, as in decodeDeleteSet; it reads
	// the rest decoder, which is restored after it unless the pass added nothing.
	rest, cur := *dec.restDec, dec.dsCurrVal
	tally := rangeTally{room: int(min(n, uint64(dec.restDec.Remaining()/4)))}
	scanErr := readDeleteSetV2(dec, n, tally.count)
	if len(tally.clients) == 0 {
		return newOrderedDeleteSet(), 0, scanErr
	}
	*dec.restDec, dec.dsCurrVal = rest, cur
	tally.merge()
	ds = tally.deleteSet()
	err = readDeleteSetV2(dec, n, func(client ClientID, r DeleteRange, _ bool) {
		ds.appendDecoded(client, r, &tally)
	})
	return ds, tally.total(), err
}

// readDeleteSetV2 is readDeleteSet for V2.
func readDeleteSetV2(dec *v2Decoder, n uint64, add func(client ClientID, r DeleteRange, first bool)) error {
	for range n {
		dec.resetDsCurVal()
		clientU, err := dec.restDec.ReadVarUint()
		if err != nil {
			return err
		}
		numRanges, err := dec.restDec.ReadVarUint()
		if err != nil {
			return err
		}
		for j := range numRanges {
			clock, err := dec.readDsClock()
			if err != nil {
				return err
			}
			length, err := dec.readDsLen()
			if err != nil {
				return err
			}
			add(ClientID(clientU), DeleteRange{Clock: clock, Len: length}, j == 0)
		}
	}
	return nil
}
