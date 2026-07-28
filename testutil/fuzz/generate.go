package fuzz

import (
	"encoding/json"
	"math/rand"
)

var textAlphabets = []string{"abcdef", "αβγδ日本語", "🙂🎉ABC"} // ascii, multi-byte, emoji

// GenOpts controls opt-in fuzz-generation features that are unsafe for the
// yjs cross-impl oracle (e.g. moves, which ygo encodes as a wire extension
// the yjs oracle cannot decode) and so must never appear in default
// generation.
type GenOpts struct {
	Moves       bool // allow array move ops (ygo-only; breaks yjs cross-impl)
	ArrayOnly   bool // restrict generation to the single {"a", KindArray} root
	SingleMover bool // only peer 0 may emit a move op (others stay move-free)
	NoPush      bool // never emit OpPush for array ops (emit OpInsert instead); yrs's push_back is less Yjs-conformant than ygo's OpPush (see testutil/yrs-oracle), so the yrs cross-impl oracle excludes it
}

// Generate builds a deterministic Scenario from seed with unchanged, move-free
// behaviour (equivalent to GenerateWith(seed, GenOpts{})). Kept byte-identical
// so TestFuzzConvergence and TestFuzzCrossImpl are unaffected.
func Generate(seed uint64) Scenario {
	return GenerateWith(seed, GenOpts{})
}

// GenerateArrayMoves builds a deterministic array-only Scenario from seed
// with move ops enabled on peer 0 only (SingleMover) — the shape the yrs
// oracle expects: array-root scenarios that may include OpMove.
func GenerateArrayMoves(seed uint64) Scenario {
	return GenerateWith(seed, GenOpts{Moves: true, ArrayOnly: true, SingleMover: true, NoPush: true})
}

// GenerateWith builds a deterministic Scenario from seed, honoring opts.
func GenerateWith(seed uint64, opts GenOpts) Scenario {
	r := rand.New(rand.NewSource(int64(seed))) //nolint:gosec // deterministic fuzz seed, not crypto
	numPeers := 3 + r.Intn(3)                  // 3..5
	numSteps := 60 + r.Intn(141)               // 60..200
	roots := []struct {
		name string
		kind TypeKind
	}{
		{"t", KindText}, {"a", KindArray}, {"m", KindMap}, {"x", KindXmlFragment},
	}
	if opts.ArrayOnly {
		roots = []struct {
			name string
			kind TypeKind
		}{
			{"a", KindArray},
		}
	}
	s := Scenario{Seed: seed, NumPeers: numPeers}
	for i := 0; i < numSteps; i++ {
		switch {
		case r.Intn(100) < 15: // 15% sync
			s.Steps = append(s.Steps, genSync(r, numPeers))
		case r.Intn(100) < 5: // ~4% GC
			s.Steps = append(s.Steps, Step{Kind: StepGC, Peer: r.Intn(numPeers)})
		default:
			root := roots[r.Intn(len(roots))]
			s.Steps = append(s.Steps, genLocalOp(r, numPeers, root.name, root.kind, opts))
		}
	}
	return s
}

func genSync(r *rand.Rand, n int) Step {
	from := r.Intn(n)
	to := (from + 1 + r.Intn(n-1)) % n // distinct peer
	// Biased: 60% merge/diff, 40% apply.
	methods := []SyncMethod{MergeV1, MergeV2, DiffV1, DiffV2, MergeV1, MergeV2, ApplyV1, ApplyV2, ApplyV1, ApplyV2}
	return Step{Kind: StepSync, From: from, To: to, Method: methods[r.Intn(len(methods))]}
}

func genLocalOp(r *rand.Rand, n int, root string, kind TypeKind, opts GenOpts) Step {
	st := Step{Kind: StepLocalOp, Peer: r.Intn(n), Root: root, TypeKind: kind}
	switch kind {
	case KindText:
		if r.Intn(100) < 70 {
			st.Op, st.PosHint = OpInsert, r.Intn(50)
			ab := textAlphabets[r.Intn(len(textAlphabets))]
			st.StrVal = randRunes(r, ab, 1+r.Intn(5))
		} else {
			st.Op, st.PosHint, st.LenHint = OpDelete, r.Intn(50), 1+r.Intn(3)
		}
	case KindArray:
		moveOK := opts.Moves && (!opts.SingleMover || st.Peer == 0)
		pick := r.Intn(3)
		if moveOK {
			pick = r.Intn(4) // 0..3, add the move branch
		}
		switch pick {
		case 0:
			st.Op, st.PosHint, st.JSONVal = OpInsert, r.Intn(50), randScalarJSON(r)
		case 1:
			if opts.NoPush {
				st.Op, st.PosHint, st.JSONVal = OpInsert, r.Intn(50), randScalarJSON(r)
			} else {
				st.Op, st.JSONVal = OpPush, randScalarJSON(r)
			}
		case 2:
			st.Op, st.PosHint, st.LenHint = OpDelete, r.Intn(50), 1+r.Intn(3)
		default: // move
			st.Op, st.PosHint, st.ToHint = OpMove, r.Intn(50), r.Intn(50)
		}
	case KindMap:
		if r.Intn(100) < 70 {
			st.Op, st.Key, st.JSONVal = OpSetKey, randKey(r), randScalarJSON(r)
		} else {
			st.Op, st.Key = OpDelKey, randKey(r)
		}
	case KindXmlFragment:
		genXmlOp(r, &st)
	}
	return st
}

var xmlTags = []string{"div", "p", "span"}
var xmlAttrKeys = []string{"class", "id", "style"}

// genXmlOp populates an XML op on st: insert an element/text child, delete a
// child, or set/delete an attribute on a (possibly nested, up to depth 3)
// element addressed by Target.
func genXmlOp(r *rand.Rand, st *Step) {
	switch pct := r.Intn(100); {
	case pct < 50: // 50%: add child (element or text)
		st.Op, st.PosHint = OpAddChild, r.Intn(20)
		if r.Intn(3) == 0 {
			st.ChildXml = "text"
		} else {
			st.ChildXml = "elem:" + xmlTags[r.Intn(len(xmlTags))]
		}
	case pct < 70: // 20%: delete a child
		st.Op, st.PosHint = OpDelete, r.Intn(20)
	default: // 30%: set/del attribute on a nested element (Target path)
		if r.Intn(5) == 0 {
			st.Op = OpDelAttr
		} else {
			st.Op = OpSetAttr
			st.StrVal = randRunes(r, "abcXYZ-", 1+r.Intn(6))
		}
		st.Key = xmlAttrKeys[r.Intn(len(xmlAttrKeys))]
		depth := r.Intn(4) // 0..3
		st.Target = make([]int, depth)
		for i := range st.Target {
			st.Target[i] = r.Intn(20)
		}
	}
}

func randRunes(r *rand.Rand, alphabet string, n int) string {
	rs := []rune(alphabet)
	out := make([]rune, n)
	for i := range out {
		out[i] = rs[r.Intn(len(rs))]
	}
	return string(out)
}

func randKey(r *rand.Rand) string {
	keys := []string{"k0", "k1", "k2", "k3"} // small keyspace → forces set/delete conflicts
	return keys[r.Intn(len(keys))]
}

func randScalarJSON(r *rand.Rand) string {
	var v any
	switch r.Intn(4) {
	case 0:
		v = r.Intn(1000)
	case 1:
		v = r.Float64()
	case 2:
		v = r.Intn(2) == 1
	default:
		v = randRunes(r, "abcXYZ", 1+r.Intn(4))
	}
	b, _ := json.Marshal(v)
	return string(b)
}
