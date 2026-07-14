package fuzz

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/reearth/ygo/crdt"
)

type peerState struct {
	doc *crdt.Doc
	// inboxV1/inboxV2 stage Diff*-produced update bytes until a Merge* step
	// (or final drain) folds them into doc. Kept as two separate queues
	// because V1 and V2 are distinct, non-interchangeable wire encodings: a
	// real transport tags each message with its encoding (or a peer pair
	// negotiates one encoding up front), so a receiver never needs to guess.
	// A single untyped queue would let a DiffV2 blob and a MergeV1 step land
	// in the same MergeUpdatesV1 call, corrupting the byte stream — this bit
	// the first implementation (see interpret_test.go's TestApplySync_
	// DoesNotMixV1AndV2Inboxes, added after triaging that exact crash on
	// fuzz seed 0).
	inboxV1 [][]byte
	inboxV2 [][]byte
}

func newPeers(n int) []*peerState {
	ps := make([]*peerState, n)
	for i := range ps {
		ps[i] = &peerState{doc: crdt.New(crdt.WithClientID(crdt.ClientID(i + 1)))}
	}
	return ps
}

func clampIndex(pos, length int, forInsert bool) int {
	if pos < 0 {
		pos = -pos
	}
	m := length
	if forInsert {
		m = length + 1
	}
	if m <= 0 {
		return 0
	}
	return pos % m
}

func decodeScalar(js string) any {
	if js == "" {
		return nil
	}
	var v any
	_ = json.Unmarshal([]byte(js), &v)
	return v
}

func applyLocalOp(p *peerState, st Step) {
	switch st.TypeKind {
	case KindText:
		txt := p.doc.GetText(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpInsert:
				txt.Insert(txn, clampIndex(st.PosHint, txt.Len(), true), st.StrVal, nil)
			case OpDelete:
				if txt.Len() > 0 {
					txt.Delete(txn, clampIndex(st.PosHint, txt.Len(), false), minInt(st.LenHint, txt.Len()))
				}
			}
		})
	case KindArray:
		arr := p.doc.GetArray(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpInsert:
				arr.Insert(txn, clampIndex(st.PosHint, arr.Len(), true), []any{decodeScalar(st.JSONVal)})
			case OpPush:
				arr.Push(txn, []any{decodeScalar(st.JSONVal)})
			case OpDelete:
				if arr.Len() > 0 {
					arr.Delete(txn, clampIndex(st.PosHint, arr.Len(), false), minInt(st.LenHint, arr.Len()))
				}
			}
		})
	case KindMap:
		m := p.doc.GetMap(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpSetKey:
				m.Set(txn, st.Key, decodeScalar(st.JSONVal))
			case OpDelKey:
				m.Delete(txn, st.Key)
			}
		})
	case KindXmlFragment:
		frag := p.doc.GetXmlFragment(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpAddChild:
				idx := clampIndex(st.PosHint, frag.Len(), true)
				if st.ChildXml == "text" {
					frag.InsertText(txn, idx, crdt.NewYXmlText())
				} else { // "elem:<tag>"
					tag := "div"
					if len(st.ChildXml) > 5 {
						tag = st.ChildXml[5:]
					}
					frag.InsertElement(txn, idx, crdt.NewYXmlElement(tag))
				}
			case OpDelete:
				if frag.Len() > 0 {
					frag.Delete(txn, clampIndex(st.PosHint, frag.Len(), false), 1)
				}
			case OpSetAttr, OpDelAttr:
				if el := resolveXmlElem(frag, st.Target); el != nil {
					if st.Op == OpSetAttr {
						el.SetAttribute(txn, st.Key, st.StrVal)
					} else {
						el.DeleteAttribute(txn, st.Key)
					}
				}
			}
		})
	}
}

// resolveXmlElem walks target (a 0-3-deep child-index path) from frag's
// top-level children down through nested elements, returning the element at
// the end of the path. Returns nil if target is empty, frag has no children,
// or the path steps into a non-element (e.g. a YXmlText) child — in which
// case the caller treats the op as a no-op rather than panicking.
//
// YXmlElement embeds YXmlFragment (verified in crdt/yxml.go), so
// (*crdt.YXmlElement).Children() is promoted from YXmlFragment.Children() —
// nesting is walked uniformly at every depth, capped at len(target) by the
// generator (genXmlOp caps depth at 3).
func resolveXmlElem(frag *crdt.YXmlFragment, target []int) *crdt.YXmlElement {
	children := frag.Children()
	if len(children) == 0 || len(target) == 0 {
		return nil
	}
	el, ok := children[target[0]%len(children)].(*crdt.YXmlElement)
	if !ok {
		return nil
	}
	for _, idx := range target[1:] {
		cc := el.Children()
		if len(cc) == 0 {
			return el
		}
		next, ok := cc[idx%len(cc)].(*crdt.YXmlElement)
		if !ok {
			return el
		}
		el = next
	}
	return el
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RunGo builds NumPeers peers, replays every step of the scenario against
// them, drains any pending inbox updates (from Diff/Merge sync steps), forces
// a final full-mesh sync, and returns the resulting peers for convergence
// checking.
func RunGo(s Scenario) ([]*peerState, error) {
	peers := newPeers(s.NumPeers)
	for _, st := range s.Steps {
		switch st.Kind {
		case StepLocalOp:
			applyLocalOp(peers[st.Peer%s.NumPeers], st)
		case StepSync:
			if err := applySync(peers, st, s.NumPeers); err != nil {
				return nil, err
			}
		case StepGC:
			// GC is triggered by re-encoding; ygo auto-GCs at commit. A no-op
			// local transaction forces a commit pass. (If a public GC() exists,
			// call it here instead — grep crdt for "func (d *Doc) GC".)
			peers[st.Peer%s.NumPeers].doc.Transact(func(*crdt.Transaction) {})
		}
	}
	if err := drainInboxes(peers); err != nil {
		return nil, err
	}
	if err := finalSync(peers); err != nil {
		return nil, err
	}
	return peers, nil
}

// finalSync forces full transitive connectivity across every peer,
// regardless of which sync edges the scenario's random walk happened to
// exercise. Because Step.Kind==StepSync picks a random directed (from, to)
// pair each time, a peer's local edit can easily go un-routed to some other
// peer by the time the step list ends (observed on fuzz seed 0: peer 1's
// step-116 array push never reached peer 0 because no sync edge — direct or
// transitive — existed from peer 1 to peer 0 after that step). That is an
// artifact of the random walk's connectivity, not a CRDT defect, and
// asserting equality across a possibly-disconnected peer graph would make
// Converged flag false positives unrelated to ygo's correctness.
//
// Mirrors Yjs's own fuzz-test harness, which reconnects and syncs every user
// before comparing (see yjs/testHelper.js's compareUsers, always invoked
// after a full reconnect). Each peer's complete since-genesis state is
// merged into one update and applied to every peer, so whatever Converged
// later compares reflects "did Merge/Apply/Diff compute the right result
// once all information was exchanged" — the property this fuzzer exists to
// check — rather than "did the random graph happen to be connected."
func finalSync(peers []*peerState) error {
	updates := make([][]byte, len(peers))
	for i, p := range peers {
		updates[i] = crdt.EncodeStateAsUpdateV1(p.doc, nil)
	}
	merged, err := crdt.MergeUpdatesV1(updates...)
	if err != nil {
		return err
	}
	for _, p := range peers {
		if err := crdt.ApplyUpdateV1(p.doc, merged, nil); err != nil {
			return err
		}
	}
	return nil
}

// applySync drives one sync step across the from->to peer pair over the
// wire path named by st.Method. Apply* steps sync immediately; Diff*/Merge*
// steps stage the update in the destination's version-tagged inbox (drained
// later by drainInboxes), modeling store-and-forward transports. DiffV1/
// MergeV1 only ever touch inboxV1, and DiffV2/MergeV2 only ever touch
// inboxV2 — V1 and V2 are distinct byte encodings, so entries must never be
// merged or applied across the wrong decoder (see the peerState doc comment).
func applySync(peers []*peerState, st Step, n int) error {
	from, to := peers[st.From%n], peers[st.To%n]
	switch st.Method {
	case ApplyV1:
		return crdt.ApplyUpdateV1(to.doc, crdt.EncodeStateAsUpdateV1(from.doc, to.doc.StateVector()), nil)
	case ApplyV2:
		return crdt.ApplyUpdateV2(to.doc, crdt.EncodeStateAsUpdateV2(from.doc, to.doc.StateVector()), nil)
	case DiffV1:
		u, err := crdt.DiffUpdateV1(crdt.EncodeStateAsUpdateV1(from.doc, nil), to.doc.StateVector())
		if err != nil {
			return err
		}
		to.inboxV1 = append(to.inboxV1, u)
		return nil
	case DiffV2:
		u, err := crdt.DiffUpdateV2(crdt.EncodeStateAsUpdateV2(from.doc, nil), to.doc.StateVector())
		if err != nil {
			return err
		}
		to.inboxV2 = append(to.inboxV2, u)
		return nil
	case MergeV1:
		to.inboxV1 = append(to.inboxV1, crdt.EncodeStateAsUpdateV1(from.doc, nil))
		merged, err := crdt.MergeUpdatesV1(to.inboxV1...)
		if err != nil {
			return err
		}
		to.inboxV1 = nil
		return crdt.ApplyUpdateV1(to.doc, merged, nil)
	case MergeV2:
		to.inboxV2 = append(to.inboxV2, crdt.EncodeStateAsUpdateV2(from.doc, nil))
		merged, err := crdt.MergeUpdatesV2(to.inboxV2...)
		if err != nil {
			return err
		}
		to.inboxV2 = nil
		return crdt.ApplyUpdateV2(to.doc, merged, nil)
	}
	return nil
}

// drainInboxes applies every pending update left in each peer's inboxes
// after the step list has been replayed, so Diff-staged updates that were
// never followed by another sync step still land before convergence is
// checked. Each queue is drained through its own decoder — never guessed —
// since a V1 blob fed to the V2 decoder (or vice versa) is a format error,
// not a recoverable one.
func drainInboxes(peers []*peerState) error {
	for _, p := range peers {
		for _, u := range p.inboxV1 {
			if err := crdt.ApplyUpdateV1(p.doc, u, nil); err != nil {
				return fmt.Errorf("drain v1: %w", err)
			}
		}
		p.inboxV1 = nil
		for _, u := range p.inboxV2 {
			if err := crdt.ApplyUpdateV2(p.doc, u, nil); err != nil {
				return fmt.Errorf("drain v2: %w", err)
			}
		}
		p.inboxV2 = nil
	}
	return nil
}

// stateJSON normalizes peer p's state across roots into a JSON-comparable
// map, keyed by root name.
func stateJSON(p *peerState, roots []struct {
	name string
	kind TypeKind
}) (map[string]any, error) {
	out := map[string]any{}
	for _, r := range roots {
		var raw []byte
		var err error
		switch r.kind {
		case KindText:
			raw, err = p.doc.GetText(r.name).ToJSON()
		case KindArray:
			raw, err = p.doc.GetArray(r.name).ToJSON()
		case KindMap:
			raw, err = p.doc.GetMap(r.name).ToJSON()
		case KindXmlFragment:
			raw = []byte(fmt.Sprintf("%q", p.doc.GetXmlFragment(r.name).ToXML()))
		}
		if err != nil {
			return nil, err
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out[r.name] = v
	}
	return out, nil
}

// Converged asserts every peer holds structurally identical state across the
// well-known roots the generator drives ("t"/"a"/"m"/"x"). Verified: YText/
// YArray/YMap.ToJSON never error on an empty/untouched root — each is a bare
// json.Marshal of ToString()/ToSlice()/Entries(), which give "", a non-nil
// []any{}, and a non-nil map[string]any{} respectively, i.e. "", "[]", "{}" —
// all valid JSON — and YXmlFragment.ToXML() returns "" for an empty fragment.
func Converged(peers []*peerState) error {
	if len(peers) < 2 {
		return nil
	}
	roots := []struct {
		name string
		kind TypeKind
	}{{"t", KindText}, {"a", KindArray}, {"m", KindMap}, {"x", KindXmlFragment}}
	ref, err := stateJSON(peers[0], roots)
	if err != nil {
		return err
	}
	for i := 1; i < len(peers); i++ {
		got, err := stateJSON(peers[i], roots)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(ref, got) {
			return fmt.Errorf("peer 0 vs peer %d diverged:\n 0=%v\n %d=%v", i, ref, i, got)
		}
	}
	return nil
}
