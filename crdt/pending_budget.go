package crdt

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"

	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/internal/anycodec"
)

// pendingBudget checks whether dependencies can resolve within this update
// before charging its unresolved structs to the persistent pending limit.
// Preflight retains a compact record per blocked wire group and its blocked
// structs as varints, never a decoder copy or a Go value per struct.
type pendingBudget struct {
	initial     StateVector
	update      []byte
	remaining   int
	v2          bool
	checked     bool
	v2Start     *v2Decoder
	v2Rest      encoding.Decoder
	superseded  groupSet
	groupsKnown bool
}

func newPendingBudget(doc *Doc, initial StateVector, update []byte, v2 bool) pendingBudget {
	remaining := doc.maxPendingItemsLimit()
	if doc.store.pending != nil {
		remaining -= len(doc.store.pending.items)
	}
	return pendingBudget{initial: initial, update: update, remaining: remaining, v2: v2}
}

// setSuperseded records the groups the decoder drops, so preflight counts
// only the groups that are integrated.
func (b *pendingBudget) setSuperseded(s groupSet) {
	b.superseded, b.groupsKnown = s, true
}

func (b *pendingBudget) scanner() *pendingScanner {
	if b.v2Start == nil {
		return newPendingScanner(b.update, b.v2)
	}
	// Column cursors copy by value; the immutable columns and string pool are
	// shared with the real decoder. Only the raw rest cursor needs rebinding.
	v2 := *b.v2Start
	rest := b.v2Rest
	v2.restDec = &rest
	return &pendingScanner{v2: &v2, rest: &rest}
}

func (b *pendingBudget) check(count int) error {
	if b.checked || count < b.remaining {
		return nil
	}
	if b.remaining < 0 {
		return ErrInvalidUpdate
	}
	if !b.groupsKnown {
		v2, rest := b.v2Start, b.v2Rest
		if b.v2 && v2 == nil {
			dec, err := newV2Decoder(b.update)
			if err != nil {
				return wrapUpdateErr(err)
			}
			v2, rest = dec, *dec.restDec
		}
		superseded, err := supersededGroups(b.update, v2, rest)
		if err != nil {
			return wrapUpdateErr(err)
		}
		b.setSuperseded(superseded)
	}
	known := make(StateVector, len(b.initial))
	for client, clock := range b.initial {
		known[client] = clock
	}
	var clients pendingClientRange
	// Two allocation-light passes handle permanently missing heads, cycles,
	// and unrelated progress without retaining a head for every wire group.
	// This bound is independent of chain length: remaining dependencies use
	// the worklist, never an unbounded sequence of full-message scans.
	for pass := 0; pass < 2; pass++ {
		var result pendingPass
		var err error
		if pass == 0 {
			result, err = scanPendingGroups(b.scanner(), known, b.remaining, &clients, b.superseded)
		} else {
			var clientRange *pendingClientRange
			if clients.ordered {
				clientRange = &clients
			}
			var bounded pendingBoundedPass
			bounded, err = scanPendingGroupsBounded(b.scanner(), known, b.remaining, nil, nil, clientRange, b.superseded)
			result = bounded.pendingPass
			result.pending += bounded.permanent
		}
		if err != nil {
			if err == ErrInvalidUpdate {
				return err
			}
			return wrapUpdateErr(err)
		}
		if result.pending <= b.remaining {
			b.checked = true
			return nil
		}
		if !result.progressed {
			return ErrInvalidUpdate
		}
	}
	// An absent dependency cannot be supplied by a later scan. Record compact
	// wire bounds before retaining any per-group state.
	var bounds []pendingWireBound
	if !b.wireContainsDependencies(known) {
		var err error
		bounds, err = pendingWireBounds(b.scanner(), known, b.superseded)
		if err != nil {
			return wrapUpdateErr(err)
		}
	}
	// A sizing pass makes the retained records and summary exact.
	sizing := pendingCollector{}
	if _, err := scanPendingGroupsBounded(b.scanner(), known, b.remaining, &sizing, bounds, nil, b.superseded); err != nil {
		return wrapUpdateErr(err)
	}
	collect := pendingCollector{fill: true, groups: make([]pendingGroup, 0, sizing.heads), sum: make([]byte, 0, sizing.size)}
	result, err := scanPendingGroupsBounded(b.scanner(), known, b.remaining, &collect, bounds, nil, b.superseded)
	if err != nil {
		return wrapUpdateErr(err)
	}
	groups := collect.groups
	if len(groups) > 0 {
		resolvePendingGroups(groups, collect.sum, known)
		pending := result.permanent
		for i := range groups {
			g := &groups[i]
			for !g.done {
				if g.end > known.Clock(g.client) {
					pending++
					if pending > b.remaining {
						return ErrInvalidUpdate
					}
				}
				g.next(collect.sum)
			}
		}
	}
	b.checked = true
	return nil
}

// Strict wire ordering proves that each client occurs in only one group.
// Its endpoints and first gap rule out absent clients without an index.
// Non-monotonic or repeated clients require the full bounds check instead.
type pendingClientRange struct {
	min, max, previous ClientID
	gapMin, gapMax     ClientID
	descending         bool
	ordered            bool
}

func (r *pendingClientRange) add(client ClientID, first bool) {
	if first {
		r.min, r.max, r.previous, r.ordered = client, client, client, true
		return
	}
	if !r.ordered {
		return
	}
	if r.min == r.max {
		r.descending = client < r.previous
	}
	if r.descending {
		if client >= r.previous {
			r.ordered = false
			return
		}
		if r.gapMax == 0 && r.previous-client > 1 {
			r.gapMin, r.gapMax = client+1, r.previous-1
		}
		r.min = client
	} else {
		if client <= r.previous {
			r.ordered = false
			return
		}
		if r.gapMax == 0 && client-r.previous > 1 {
			r.gapMin, r.gapMax = r.previous+1, client-1
		}
		r.max = client
	}
	r.previous = client
}

func (r *pendingClientRange) headImpossible(known StateVector, client ClientID, clock uint64, deps []ID) bool {
	if clock > known.Clock(client) {
		return true // A unique group cannot supply its preceding clock gap.
	}
	for _, dep := range deps {
		if (dep.Client < r.min || dep.Client > r.max || r.gapMax != 0 && dep.Client >= r.gapMin && dep.Client <= r.gapMax) && dep.Clock >= known.Clock(dep.Client) {
			return true
		}
	}
	return false
}

// Bounds are sorted by client. Each surviving group is its client's only
// group, so end is the furthest clock that client can reach in this update.
type pendingWireBound struct {
	client ClientID
	end    uint64
}

func pendingWireBounds(s *pendingScanner, known StateVector, skip groupSet) ([]pendingWireBound, error) {
	n := s.uint()
	if n > maxV2Items {
		return nil, ErrInvalidUpdate
	}
	bounds := make([]pendingWireBound, 0, int(n))
	var total uint64
	for i := uint64(0); i < n && s.err == nil; i++ {
		items := s.uint()
		total += items
		if total > maxV2Items {
			return nil, ErrInvalidUpdate
		}
		client, clock := s.client(), s.uint()
		// Only this group advances its client, so a clock gap or a skip over
		// unknown clocks ends what it can ever supply.
		reach, open := known.Clock(client), true
		for j := uint64(0); j < items && s.err == nil; j++ {
			length, isSkip, _, _ := s.item()
			end := clock + length
			if end < clock {
				return nil, ErrInvalidUpdate
			}
			if open && end > reach {
				if isSkip || clock > reach {
					open = false
				} else {
					reach = end
				}
			}
			clock = end
		}
		if !skip.has(i) {
			bounds = append(bounds, pendingWireBound{client: client, end: reach})
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].client < bounds[j].client })
	return bounds, nil
}

func pendingHeadImpossible(bounds []pendingWireBound, known StateVector, client ClientID, clock uint64, deps []ID) bool {
	if clock > known.Clock(client) {
		return true // The only group cannot fill its own preceding clock gap.
	}
	for _, dep := range deps {
		if dep.Clock < known.Clock(dep.Client) {
			continue
		}
		i := sort.Search(len(bounds), func(i int) bool { return bounds[i].client >= dep.Client })
		if i == len(bounds) || bounds[i].client != dep.Client || dep.Clock >= bounds[i].end {
			return true
		}
	}
	return false
}

// A contiguous, strictly ordered client range with contiguous structs and
// references below every group's end needs no per-client bound index. This
// covers ordinary checkpoints and reverse chains without an extra allocation.
type pendingWireSummary struct {
	clients, minClient, maxClient uint64
	minDep, maxDep, maxDepClock   uint64
	minEnd                        uint64
	possibleMin, possibleMax      uint64
	previous                      ClientID
	direction                     int
	contiguous                    bool
}

func (w pendingWireSummary) containsDependencies() bool {
	return w.contiguous && w.clients > 0 && w.maxClient-w.minClient == w.clients-1 &&
		w.minDep >= w.minClient && w.maxDep <= w.maxClient && w.maxDepClock < w.minEnd
}

// The census never retains cursors. Keep its decoder copies on the stack so
// the extra wire pass does not add allocations to complete checkpoints.
func (b *pendingBudget) wireContainsDependencies(known StateVector) bool {
	if b.v2Start == nil {
		if b.v2 {
			return pendingWireContainsDependencies(newPendingScanner(b.update, true), known)
		}
		rest := encoding.NewDecoder(b.update)
		s := pendingScanner{rest: rest}
		return pendingWireContainsDependencies(&s, known)
	}
	v2, rest := *b.v2Start, b.v2Rest
	// The census uses only V2 column methods. Raw fields go through s.rest;
	// no full V2 decoder rest cursor is needed or retained here.
	v2.restDec = nil
	s := pendingScanner{v2: &v2, rest: &rest}
	return pendingWireContainsDependencies(&s, known)
}

// This census runs only after both plain scans made progress. Impossible
// dense-range proofs stop immediately; rejected tails keep the plain scan cost.
// An unproven census falls back to the validating bounds scan. Returning only
// the proof keeps decoder copies from escaping through an error interface.
func pendingWireContainsDependencies(s *pendingScanner, known StateVector) bool {
	clients := s.uint()
	if clients == 0 {
		return false
	}
	if clients > maxV2Items {
		return false
	}
	w := pendingWireSummary{clients: clients, minClient: ^uint64(0), minDep: ^uint64(0), minEnd: ^uint64(0), contiguous: true}
	var total uint64
	for i := uint64(0); i < clients; i++ {
		n := s.uint()
		if n > maxV2Items-total {
			return false
		}
		total += n
		client, clock := s.client(), s.uint()
		if s.err != nil {
			return false
		}
		w.minClient = min(w.minClient, uint64(client))
		w.maxClient = max(w.maxClient, uint64(client))
		if i == 0 {
			distance := clients - 1
			if uint64(client) >= distance {
				w.possibleMin = uint64(client) - distance
			}
			w.possibleMax = ^uint64(0)
			if uint64(client) <= ^uint64(0)-distance {
				w.possibleMax = uint64(client) + distance
			}
		}
		if clock > known.Clock(client) {
			return false
		}
		if i > 0 {
			direction := -1
			if client > w.previous {
				direction = 1
			}
			if client == w.previous || (i > 1 && direction != w.direction) ||
				(client > w.previous && client-w.previous != 1) ||
				(client < w.previous && w.previous-client != 1) {
				return false
			}
			w.direction = direction
		}
		w.previous = client
		for j := uint64(0); j < n; j++ {
			length, skip, deps, numDeps := s.item()
			if s.err != nil {
				return false
			}
			end := clock + length
			if end < clock {
				return false
			}
			if skip {
				return false
			}
			for _, dep := range deps[:numDeps] {
				if uint64(dep.Client) < w.possibleMin || uint64(dep.Client) > w.possibleMax {
					return false
				}
				w.minDep = min(w.minDep, uint64(dep.Client))
				w.maxDep = max(w.maxDep, uint64(dep.Client))
				w.maxDepClock = max(w.maxDepClock, dep.Clock)
			}
			clock = end
		}
		w.minEnd = min(w.minEnd, clock)
		if w.maxDepClock >= w.minEnd {
			return false
		}
	}
	return w.containsDependencies()
}

type pendingPass struct {
	heads, pending int
	progressed     bool
}

type pendingBoundedPass struct {
	pendingPass
	permanent int
}

func scanPendingGroups(s *pendingScanner, known StateVector, remaining int, clientRange *pendingClientRange, superseded groupSet) (pendingPass, error) {
	var result pendingPass
	clients := s.uint()
	if clients > maxV2Items {
		return result, ErrInvalidUpdate
	}
	var total uint64
	first := true
	for i := uint64(0); i < clients && s.err == nil; i++ {
		n := s.uint()
		total += n
		if total > maxV2Items {
			return result, ErrInvalidUpdate
		}
		client, clock := s.client(), s.uint()
		drop := superseded.has(i)
		if clientRange != nil && !drop {
			clientRange.add(client, first)
			first = false
		}
		knownClock := known.Clock(client)
		for j := uint64(0); j < n && s.err == nil; j++ {
			length, skip, deps, numDeps := s.item()
			end := clock + length
			if end < clock {
				return result, ErrInvalidUpdate
			}
			if !drop && !skip && end > knownClock {
				ready := clock <= knownClock
				if ready {
					for _, dep := range deps[:numDeps] {
						if dep.Clock >= known.Clock(dep.Client) {
							ready = false
							break
						}
					}
				}
				if ready {
					known[client] = end
					knownClock = end
					result.progressed = true
				} else {
					result.pending++
					if clients == 1 && result.pending > remaining {
						// No other group can fill this group's first blocked head. All
						// following uncovered non-Skip structs remain permanently pending.
						return result, ErrInvalidUpdate
					}
				}
			}
			clock = end
		}
	}
	return result, s.err
}

func scanPendingGroupsBounded(s *pendingScanner, known StateVector, remaining int, collect *pendingCollector, bounds []pendingWireBound, clientRange *pendingClientRange, superseded groupSet) (pendingBoundedPass, error) {
	var result pendingBoundedPass
	clients := s.uint()
	if clients > maxV2Items {
		return result, ErrInvalidUpdate
	}
	var total uint64
	for i := uint64(0); i < clients && s.err == nil; i++ {
		n := s.uint()
		total += n
		if total > maxV2Items {
			return result, ErrInvalidUpdate
		}
		client, clock := s.client(), s.uint()
		drop := superseded.has(i)
		blocked, permanent, collected := false, false, false
		knownClock := known.Clock(client)
		for j := uint64(0); j < n && s.err == nil; j++ {
			length, skip, deps, numDeps := s.item()
			end := clock + length
			if end < clock {
				return result, ErrInvalidUpdate
			}
			if collected {
				collect.put(skip, length, deps[:numDeps])
			}
			if !drop && !skip && end > knownClock {
				if !blocked {
					if clientRange != nil {
						permanent = clientRange.headImpossible(known, client, clock, deps[:numDeps])
					} else if bounds != nil {
						permanent = pendingHeadImpossible(bounds, known, client, clock, deps[:numDeps])
					}
				}
				if permanent {
					result.permanent++
					if result.permanent > remaining {
						return result, ErrInvalidUpdate
					}
					blocked = true
					clock = end
					continue
				}
				ready := clock <= knownClock
				if ready {
					for _, dep := range deps[:numDeps] {
						if dep.Clock >= known.Clock(dep.Client) {
							ready = false
							break
						}
					}
				}
				if ready {
					known[client] = end
					knownClock = end
					result.progressed = true
				} else {
					result.pending++
					if clients == 1 && result.pending > remaining {
						// No other group can fill this group's first blocked head. All
						// following uncovered non-Skip structs remain permanently pending.
						return result, ErrInvalidUpdate
					}
					if !blocked {
						result.heads++
						if collect != nil {
							collect.head(client, clock, end, n-j-1)
							collect.put(false, length, deps[:numDeps])
							collected = true
						}
						blocked = true
					}
				}
			}
			clock = end
		}
	}
	return result, s.err
}

// The scanner retains nothing. V1 reads the caller's buffer; V2 uses its
// normal column decoder without building a key dictionary or decoding
// Any/JSON values into object trees. A framing scanner only finds struct
// boundaries: it skips text and JSON without validating or counting them, and
// in V2 never reads the clock, string, key or type columns. The decoder still
// validates everything.
type pendingScanner struct {
	rest  *encoding.Decoder
	v2    *v2Decoder
	keys  int
	err   error
	frame bool
}

func newPendingScanner(update []byte, v2 bool) *pendingScanner {
	s := &pendingScanner{}
	if v2 {
		s.v2, s.err = newV2Decoder(update)
		if s.err == nil {
			s.rest = s.v2.restDec
		}
	} else {
		s.rest = encoding.NewDecoder(update)
	}
	return s
}

func (s *pendingScanner) uint() uint64 {
	if s.err != nil {
		return 0
	}
	v, err := s.rest.ReadVarUint()
	s.err = err
	return v
}
func (s *pendingScanner) byte() byte {
	if s.err != nil {
		return 0
	}
	v, err := s.rest.ReadUint8()
	s.err = err
	return v
}
func (s *pendingScanner) client() ClientID {
	if s.v2 == nil {
		return ClientID(s.uint())
	}
	if s.err != nil {
		return 0
	}
	v, err := s.v2.readClient()
	s.err = err
	return v
}
func (s *pendingScanner) id(right bool) ID {
	if s.v2 == nil {
		return ID{Client: s.client(), Clock: s.uint()}
	}
	if s.err != nil {
		return ID{}
	}
	if s.frame {
		var client uint64
		client, s.err = s.v2.clientDec.Read()
		return ID{Client: ClientID(client)}
	}
	var id ID
	if right {
		id, s.err = s.v2.readRightID()
	} else {
		id, s.err = s.v2.readLeftID()
	}
	return id
}
func (s *pendingScanner) length() uint64 {
	if s.v2 == nil {
		return s.uint()
	}
	if s.err != nil {
		return 0
	}
	v, err := s.v2.readLen()
	s.err = err
	return uint64(v)
}
func (s *pendingScanner) bytes() {
	if s.err == nil {
		_, s.err = s.rest.ReadVarBytes()
	}
}
func (s *pendingScanner) any() {
	if s.err == nil {
		rest := s.rest.RemainingBytes()
		consumed, err := anycodec.Skip(rest)
		// Keep the existing pointer (also v2.restDec), resetting its buffer to
		// the unread suffix. This advances without re-reading every skipped byte.
		*s.rest = *encoding.NewDecoder(rest[consumed:])
		s.err = err
	}
}

func (s *pendingScanner) text() uint64 {
	if s.err != nil {
		return 0
	}
	if s.frame {
		if s.v2 == nil {
			_, s.err = s.rest.ReadVarBytes()
		}
		return 0
	}
	var n uint64
	if s.v2 != nil {
		var str string
		str, s.err = s.v2.readString()
		for _, r := range str {
			n++
			if r > 0xffff {
				n++
			}
		}
	} else {
		var raw []byte
		raw, s.err = s.rest.ReadVarBytes()
		if s.err == nil && !utf8.Valid(raw) {
			s.err = encoding.ErrInvalidUTF8
		}
		for len(raw) > 0 {
			r, size := utf8.DecodeRune(raw)
			raw = raw[size:]
			n++
			if r > 0xffff {
				n++
			}
		}
	}
	return n
}
func (s *pendingScanner) key() {
	if s.v2 == nil {
		s.text()
		return
	}
	if s.err != nil || s.frame {
		return
	}
	var index int64
	index, s.err = s.v2.keyClockDec.Read()
	if index < 0 {
		s.err = ErrInvalidUpdate
	}
	if s.err == nil && index >= int64(s.keys) {
		s.text()
		s.keys++
	}
}
func (s *pendingScanner) item() (length uint64, skip bool, deps [3]ID, numDeps int) {
	var info byte
	if s.err != nil {
		return
	}
	if s.v2 == nil {
		info = s.byte()
	} else {
		info, s.err = s.v2.readInfo()
	}
	tag := info & 0x1f
	if tag == 0 {
		return s.length(), false, deps, 0
	}
	if tag == 10 {
		return s.uint(), true, deps, 0
	}
	if info&flagHasOrigin != 0 {
		deps[numDeps] = s.id(false)
		numDeps++
	}
	if info&flagHasRightOrigin != 0 {
		deps[numDeps] = s.id(true)
		numDeps++
	}
	if numDeps == 0 {
		var named bool
		if s.v2 == nil {
			named = s.byte() == 1
		} else if s.err == nil {
			named, s.err = s.v2.readParentInfo()
		}
		if named {
			s.text()
		} else {
			deps[0] = s.id(false)
			numDeps++
		}
		if info&flagHasParentSub != 0 {
			s.text()
		}
	}
	return s.content(tag), false, deps, numDeps
}
func (s *pendingScanner) content(tag byte) uint64 {
	if s.err != nil {
		return 0
	}
	switch tag {
	case wireDeleted:
		return s.length()
	case wireJSON, wireAny:
		n := s.length()
		if (s.v2 != nil && n > maxV2Items) || (s.v2 == nil && n > uint64(s.rest.Remaining())) {
			s.err = ErrInvalidUpdate
			return 0
		}
		if tag == wireJSON && s.v2 != nil && s.frame {
			// V2 JSON values live in the string column, which framing never reads.
			return n
		}
		if tag == wireJSON && s.v2 == nil {
			// Match decodeContent's whole-item JSON-first legacy fallback,
			// without constructing the values or their nested object trees.
			probe := *s.rest
			jsonErr := skipJSONVals(&probe, n)
			if jsonErr == nil {
				*s.rest = probe
			} else if n > 0 && isAnyTag(s.rest.RemainingBytes()[0]) {
				for i := uint64(0); i < n && s.err == nil; i++ {
					s.any()
				}
				if s.err != nil {
					s.err = jsonErr
				}
			} else {
				s.err = jsonErr
			}
		} else {
			for i := uint64(0); i < n && s.err == nil; i++ {
				if tag == wireJSON {
					s.json()
				} else {
					s.any()
				}
			}
		}
		return n
	case wireBinary:
		s.bytes()
	case wireString:
		return s.text()
	case wireEmbed:
		if s.v2 == nil {
			s.json()
		} else {
			s.any()
		}
	case wireFormat:
		s.key()
		if s.v2 == nil {
			s.json()
		} else {
			s.any()
		}
	case wireType:
		var ref byte
		if s.v2 == nil {
			ref = s.byte()
		} else if !s.frame {
			ref, s.err = s.v2.readTypeRef()
		}
		if ref == 3 || ref == 5 {
			s.key()
		}
	case wireDoc:
		if s.v2 == nil {
			s.bytes()
		} else {
			s.text()
		}
		s.any()
	case wireMove:
		s.uint()
		s.uint()
		s.uint()
	default:
		s.err = ErrInvalidUpdate
	}
	return 1
}

// json validates JSON-bearing content without materializing its values.
func (s *pendingScanner) json() {
	if s.err != nil {
		return
	}
	if s.frame {
		s.text()
		return
	}
	if s.v2 == nil {
		s.err = skipJSONVals(s.rest, 1)
		return
	}
	var value string
	value, s.err = s.v2.readString()
	if s.err == nil {
		s.err = validatePendingJSON([]byte(value))
	}
}

// skipJSONVals validates V1 JSON text without allocating decoded values.
func skipJSONVals(dec *encoding.Decoder, n uint64) error {
	for i := uint64(0); i < n; i++ {
		raw, err := dec.ReadVarBytes()
		if err != nil {
			return err
		}
		if err := validatePendingJSON(raw); err != nil {
			return err
		}
	}
	return nil
}

// Match json.Unmarshal's syntax and float-range validation. This also keeps
// the V1 legacy Any fallback's choice consistent for ambiguous 116–127 bytes.
func validatePendingJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return encoding.ErrInvalidUTF8
	}
	if bytes.Equal(raw, []byte("undefined")) {
		return nil
	}
	if !json.Valid(raw) {
		return ErrInvalidUpdate
	}
	for j := 0; j < len(raw); j++ {
		if raw[j] == '"' {
			for j++; raw[j] != '"'; j++ {
				if raw[j] == '\\' {
					j++
				}
			}
		} else if raw[j] == '-' || raw[j] >= '0' && raw[j] <= '9' {
			start := j
			for j < len(raw) && (raw[j] >= '0' && raw[j] <= '9' || raw[j] == '-' || raw[j] == '+' || raw[j] == '.' || raw[j] == 'e' || raw[j] == 'E') {
				j++
			}
			if _, err := strconv.ParseFloat(string(raw[start:j]), 64); err != nil {
				return ErrInvalidUpdate
			}
			j--
		}
	}
	return nil
}
