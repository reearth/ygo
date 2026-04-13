package crdt

import (
	"fmt"
	"math"
	"sort"

	"github.com/reearth/ygo/encoding"
)

// maxV2Items caps the total number of structs decoded from a single V2 update to
// prevent RLE-compressed pathological inputs from causing unbounded loops or OOM.
const maxV2Items = uint64(1 << 20) // 1 million items

// ── V2 encoder state ──────────────────────────────────────────────────────────

type v2Encoder struct {
	keyClock int
	keyMap   map[string]int

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
		keyMap:  make(map[string]int),
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

func (e *v2Encoder) writeLen(l int) {
	e.lenEnc.Write(uint64(l))
}

func (e *v2Encoder) writeKey(key string) {
	if clock, ok := e.keyMap[key]; ok {
		e.keyClockEnc.Write(int64(clock))
	} else {
		e.keyClockEnc.Write(int64(e.keyClock))
		e.stringEnc.Write(key)
		e.keyMap[key] = e.keyClock
		e.keyClock++
	}
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
	keyClockDec   *encoding.IntDiffOptRleDecoder
	clientDec     *encoding.UintOptRleDecoder
	leftClockDec  *encoding.IntDiffOptRleDecoder
	rightClockDec *encoding.IntDiffOptRleDecoder
	infoDec       *encoding.RleByteDecoder
	stringDec     *encoding.StringDecoder
	parentInfoDec *encoding.RleByteDecoder
	typeRefDec    *encoding.UintOptRleDecoder
	lenDec        *encoding.UintOptRleDecoder
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

	stringDec, err := encoding.NewStringDecoder(stringBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: V2 stringDecoder: %v", ErrInvalidUpdate, err)
	}

	return &v2Decoder{
		keyClockDec:   encoding.NewIntDiffOptRleDecoder(keyClockBytes),
		clientDec:     encoding.NewUintOptRleDecoder(clientBytes),
		leftClockDec:  encoding.NewIntDiffOptRleDecoder(leftClockBytes),
		rightClockDec: encoding.NewIntDiffOptRleDecoder(rightClockBytes),
		infoDec:       encoding.NewRleByteDecoder(infoBytes),
		stringDec:     stringDec,
		parentInfoDec: encoding.NewRleByteDecoder(parentInfoBytes),
		typeRefDec:    encoding.NewUintOptRleDecoder(typeRefBytes),
		lenDec:        encoding.NewUintOptRleDecoder(lenBytes),
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
	return ID{Client: ClientID(client), Clock: uint64(clock)}, nil
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
	return byte(v), err
}

func (d *v2Decoder) readLen() (int, error) {
	v, err := d.lenDec.Read()
	if err != nil {
		return 0, err
	}
	// Guard against silent wrap: uint64 values larger than MaxInt32 would
	// produce a negative int on 32-bit platforms and a misleading huge value
	// on 64-bit platforms that bypasses downstream bounds checks.
	if v > math.MaxInt32 {
		return 0, ErrInvalidUpdate
	}
	return int(v), nil
}

func (d *v2Decoder) readKey() (string, error) {
	keyClock, err := d.keyClockDec.Read()
	if err != nil {
		return "", err
	}
	idx := int(keyClock)
	if idx < 0 {
		return "", ErrInvalidUpdate
	}
	if idx < len(d.keys) {
		return d.keys[idx], nil
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

func (d *v2Decoder) readDsClock() (uint64, error) {
	diff, err := d.restDec.ReadVarUint()
	if err != nil {
		return 0, err
	}
	d.dsCurrVal += diff
	return d.dsCurrVal, nil
}

func (d *v2Decoder) readDsLen() (uint64, error) {
	v, err := d.restDec.ReadVarUint()
	if err != nil {
		return 0, err
	}
	l := v + 1
	d.dsCurrVal += l
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
			if item.ID.Clock+uint64(item.Content.Len()) > svClock {
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
			offset := 0
			if i == 0 && g.startClock > item.ID.Clock {
				offset = int(g.startClock - item.ID.Clock)
			}
			encodeItemV2(enc, item, offset, doc.store)
		}
	}

	encodeDeleteSetV2(enc, buildDeleteSet(doc.store))
	return enc.toBytes()
}

func encodeItemV2(enc *v2Encoder, item *Item, offset int, store *StructStore) {
	// Orphaned items (no parent) came from GC wire format where the parent
	// type name is lost. Encode as GC struct for valid clock accounting.
	if item.Parent == nil {
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
		oc := ID{Client: item.ID.Client, Clock: item.ID.Clock + uint64(offset) - 1}
		origin = &oc
		originRight = item.OriginRight
	} else {
		origin = item.Origin
		originRight = item.OriginRight
	}

	// If the origin item is a GC placeholder (no Parent), the receiver can't
	// infer this item's parent from it. Clear the origin so explicit parent
	// info is encoded instead.
	if origin != nil {
		if oi := store.Find(*origin); oi != nil && oi.Parent == nil {
			origin = nil
		}
	}
	if originRight != nil {
		if ori := store.Find(*originRight); ori != nil && ori.Parent == nil {
			originRight = nil
		}
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
	if cantCopyParentInfo && item.ParentSub != "" {
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
		} else {
			// Root named type.
			enc.writeParentInfo(true)
			name := ""
			if item.Parent != nil {
				name = item.Parent.name
			}
			enc.writeString(name)
		}
		if item.ParentSub != "" {
			enc.writeString(item.ParentSub)
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

func encodeContentV2(enc *v2Encoder, c Content, offset int) {
	switch ct := c.(type) {
	case *ContentDeleted:
		enc.writeLen(ct.length - offset)
	case *ContentJSON:
		vals := ct.Vals[offset:]
		enc.writeLen(len(vals))
		for _, v := range vals {
			enc.writeString(fmtValToJSON(v))
		}
	case *ContentBinary:
		enc.restEnc.WriteVarBytes(ct.Data)
	case *ContentString:
		byteOff := utf16ByteOffset(ct.Str, offset)
		enc.writeString(ct.Str[byteOff:])
	case *ContentEmbed:
		enc.restEnc.WriteAny(ct.Val)
	case *ContentFormat:
		enc.writeKey(ct.Key)
		enc.restEnc.WriteAny(ct.Val)
	case *ContentType:
		tc, nodeName := typeClassOf(ct)
		enc.writeTypeRef(tc)
		if tc == 3 { // YXmlElement
			enc.writeKey(nodeName)
		}
	case *ContentAny:
		vals := ct.Vals[offset:]
		enc.writeLen(len(vals))
		for _, v := range vals {
			enc.restEnc.WriteAny(v)
		}
	case *ContentDoc:
		guid := ""
		if ct.Doc != nil {
			guid = ct.Doc.GUID()
		}
		enc.writeString(guid)
		enc.restEnc.WriteAny(nil)
	case *ContentMove:
		enc.restEnc.WriteVarUint(uint64(ct.Target.Client))
		enc.restEnc.WriteVarUint(ct.Target.Clock)
		enc.restEnc.WriteVarUint(uint64(ct.TargetLen))
	}
}

func encodeDeleteSetV2(enc *v2Encoder, ds DeleteSet) {
	clients := make([]ClientID, 0, len(ds.clients))
	for c := range ds.clients {
		clients = append(clients, c)
	}
	// Yjs V2 writes delete set clients in DESCENDING order.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	enc.restEnc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		enc.resetDsCurVal()
		enc.restEnc.WriteVarUint(uint64(c))
		ranges := ds.clients[c]
		enc.restEnc.WriteVarUint(uint64(len(ranges)))
		for _, r := range ranges {
			enc.writeDsClock(r.Clock)
			enc.writeDsLen(r.Len)
		}
	}
}

// ── V2 decoding ───────────────────────────────────────────────────────────────

func applyV2Txn(txn *Transaction, update []byte) (retErr error) {
	// Same panic recovery as applyV1Txn — guard against crafted V2 updates
	// that force a Splice on non-splittable content types.
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("%w: panic during item integration: %v", ErrInvalidUpdate, r)
		}
	}()

	dec, err := newV2Decoder(update)
	if err != nil {
		return err
	}

	sv := txn.doc.store.StateVector()

	numClients, err := dec.restDec.ReadVarUint()
	if err != nil {
		return wrapUpdateErr(err)
	}
	// Guard against pathological inputs that encode huge counts via RLE compression.
	// No realistic update has more client groups than its byte count.
	if numClients > maxV2Items {
		return ErrInvalidUpdate
	}

	var pending []*Item

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

		existingEnd := sv.Clock(client)

		for j := uint64(0); j < numStructs; j++ {
			info, err := dec.readInfo()
			if err != nil {
				return wrapUpdateErr(err)
			}

			contentType := info & 0x1F

			switch contentType {
			case 0: // GC (garbage collected)
				l, err := dec.readLen()
				if err != nil {
					return wrapUpdateErr(err)
				}
				length := uint64(l)
				itemEnd := clock + length
				if itemEnd > existingEnd {
					// GC structs carry no parent info in the V2 wire format, so
					// we cannot integrate them as live items. Instead we add
					// their clock range to the delete set: items we already hold
					// in this range will be tombstoned, and items we've never
					// seen (GC'd before they reached us) are benignly ignored
					// because deleteSet.applyTo silently skips missing items.
					effectiveStart := clock
					if effectiveStart < existingEnd {
						effectiveStart = existingEnd
					}
					gcLen := int(itemEnd - effectiveStart)
					if gcLen > 0 {
						txn.deleteSet.add(ID{Client: client, Clock: effectiveStart}, gcLen)
					}
				}
				clock = itemEnd
				continue

			case 10: // Skip struct
				l, err := dec.restDec.ReadVarUint()
				if err != nil {
					return wrapUpdateErr(err)
				}
				clock += l
				continue
			}

			// Regular item
			item, contentLen, err := decodeItemV2(dec, txn.doc, client, clock, info)
			if err != nil {
				return wrapUpdateErr(err)
			}

			itemEnd := clock + uint64(contentLen)
			if itemEnd <= existingEnd {
				clock = itemEnd
				continue
			}

			offset := 0
			if clock < existingEnd {
				offset = int(existingEnd - clock)
			}

			// Items whose parent can't be resolved yet (cross-client
			// reference to a group not yet decoded) are deferred.
			if item.Parent == nil {
				pending = append(pending, item)
				clock = itemEnd
				continue
			}

			if offset == 0 && item.Origin != nil {
				item.Left = txn.doc.store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
			}

			item.integrate(txn, offset)
			clock = itemEnd
		}
	}

	// Retry items whose parent couldn't be resolved during the first pass
	// because their origin items were in a later client group.
	for len(pending) > 0 {
		var remaining []*Item
		for _, item := range pending {
			if item.Origin != nil {
				if oi := txn.doc.store.Find(*item.Origin); oi != nil {
					item.Parent = oi.Parent
				}
			}
			if item.Parent == nil && item.OriginRight != nil {
				if ori := txn.doc.store.Find(*item.OriginRight); ori != nil {
					item.Parent = ori.Parent
				}
			}
			if item.Parent == nil && item.ParentSub != "" {
				item.Parent = findParentForMapEntry(txn.doc.store)
			}
			if item.Parent != nil {
				if item.Origin != nil {
					item.Left = txn.doc.store.getItemCleanEnd(txn, item.Origin.Client, item.Origin.Clock)
				}
				item.integrate(txn, 0)
			} else {
				remaining = append(remaining, item)
			}
		}
		if len(remaining) == len(pending) {
			for _, item := range remaining {
				txn.doc.store.Append(item)
			}
			break
		}
		pending = remaining
	}

	// Decode delete set
	ds, err := decodeDeleteSetV2(dec)
	if err != nil {
		return wrapUpdateErr(err)
	}
	ds.applyTo(txn)
	return nil
}

func decodeItemV2(dec *v2Decoder, doc *Doc, client ClientID, clock uint64, info byte) (*Item, int, error) {
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
	var parentSub string

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
			parent = doc.getOrCreateType(name)
		} else {
			// Parent by item ID.
			parentID, err := dec.readLeftID()
			if err != nil {
				return nil, 0, err
			}
			parentItem := doc.store.Find(parentID)
			if parentItem == nil {
				return nil, 0, fmt.Errorf("parent item {%d,%d} not found", parentID.Client, parentID.Clock)
			}
			ct, ok := parentItem.Content.(*ContentType)
			if !ok {
				return nil, 0, fmt.Errorf("parent item {%d,%d} is not a ContentType", parentID.Client, parentID.Clock)
			}
			parent = ct.Type
		}

		if hasParentSub {
			sub, err := dec.readString()
			if err != nil {
				return nil, 0, err
			}
			parentSub = sub
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
		Content:     content,
	}

	// Infer parent from origin when not explicitly encoded.
	if item.Parent == nil {
		if origin != nil {
			if oi := doc.store.Find(*origin); oi != nil {
				item.Parent = oi.Parent
			}
		} else if originRight != nil {
			if ori := doc.store.Find(*originRight); ori != nil {
				item.Parent = ori.Parent
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
		if l < 0 || l > int(maxV2Items) {
			return nil, ErrInvalidUpdate
		}
		vals := make([]any, l)
		for i := range vals {
			s, err := dec.readString()
			if err != nil {
				return nil, err
			}
			if s == "undefined" {
				vals[i] = nil
			} else {
				v, err := fmtValFromJSON(s)
				if err != nil {
					return nil, err
				}
				vals[i] = v
			}
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
		if l < 0 || l > int(maxV2Items) {
			return nil, ErrInvalidUpdate
		}
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
		guid, err := dec.readString()
		if err != nil {
			return nil, err
		}
		if _, err := dec.readAny(); err != nil { // opts — not yet used
			return nil, err
		}
		return NewContentDoc(New(WithGUID(guid))), nil

	case wireMove:
		clientU, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		targetLen, err := dec.restDec.ReadVarUint()
		if err != nil {
			return nil, err
		}
		target := &ID{Client: ClientID(clientU), Clock: clock}
		return NewContentMove(target, int(targetLen)), nil

	default:
		return nil, fmt.Errorf("unknown V2 content tag: %d", tag)
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

	case 5: // YXmlHook — reads hook name via readKey (discard)
		if _, err := dec.readKey(); err != nil {
			return nil, err
		}
		r := &rawType{}
		r.doc = doc
		r.itemMap = make(map[string]*Item)
		r.owner = r
		return &r.abstractType, nil

	case 6: // YXmlText
		xt := NewYXmlText()
		xt.doc = doc
		return &xt.abstractType, nil

	default:
		r := &rawType{}
		r.doc = doc
		r.itemMap = make(map[string]*Item)
		r.owner = r
		return &r.abstractType, nil
	}
}

func decodeDeleteSetV2(dec *v2Decoder) (DeleteSet, error) {
	ds := newDeleteSet()
	n, err := dec.restDec.ReadVarUint()
	if err != nil {
		return ds, err
	}
	if n > maxV2Items {
		return ds, ErrInvalidUpdate
	}
	for i := uint64(0); i < n; i++ {
		dec.resetDsCurVal()
		clientU, err := dec.restDec.ReadVarUint()
		if err != nil {
			return ds, err
		}
		numRanges, err := dec.restDec.ReadVarUint()
		if err != nil {
			return ds, err
		}
		if numRanges > maxV2Items {
			return ds, ErrInvalidUpdate
		}
		client := ClientID(clientU)
		for j := uint64(0); j < numRanges; j++ {
			clock, err := dec.readDsClock()
			if err != nil {
				return ds, err
			}
			length, err := dec.readDsLen()
			if err != nil {
				return ds, err
			}
			ds.clients[client] = append(ds.clients[client], DeleteRange{Clock: clock, Len: length})
		}
	}
	for c := range ds.clients {
		ds.sortAndCompact(c)
	}
	return ds, nil
}
