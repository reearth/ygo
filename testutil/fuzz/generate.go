package fuzz

import (
	"encoding/json"
	"math/rand"
)

var textAlphabets = []string{"abcdef", "αβγδ日本語", "🙂🎉ABC"} // ascii, multi-byte, emoji

// Generate builds a deterministic Scenario from seed.
func Generate(seed uint64) Scenario {
	r := rand.New(rand.NewSource(int64(seed))) //nolint:gosec // deterministic fuzz seed, not crypto
	numPeers := 3 + r.Intn(3)                  // 3..5
	numSteps := 60 + r.Intn(141)               // 60..200
	roots := []struct {
		name string
		kind TypeKind
	}{
		{"t", KindText}, {"a", KindArray}, {"m", KindMap},
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
			s.Steps = append(s.Steps, genLocalOp(r, numPeers, root.name, root.kind))
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

func genLocalOp(r *rand.Rand, n int, root string, kind TypeKind) Step {
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
		switch r.Intn(3) {
		case 0:
			st.Op, st.PosHint, st.JSONVal = OpInsert, r.Intn(50), randScalarJSON(r)
		case 1:
			st.Op, st.JSONVal = OpPush, randScalarJSON(r)
		default:
			st.Op, st.PosHint, st.LenHint = OpDelete, r.Intn(50), 1+r.Intn(3)
		}
	case KindMap:
		if r.Intn(100) < 70 {
			st.Op, st.Key, st.JSONVal = OpSetKey, randKey(r), randScalarJSON(r)
		} else {
			st.Op, st.Key = OpDelKey, randKey(r)
		}
	}
	return st
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
