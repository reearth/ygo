package fuzz

// Shrink returns a minimal sub-scenario for which stillFails stays true, using
// ddmin-style removal (contiguous chunks of decreasing size, converging on
// single steps) followed by payload simplification. Deterministic: stillFails
// must be a pure function of the scenario. Because the predicate typically
// wraps the (expensive) cross-impl oracle, chunk removal is preferred over
// one-step-at-a-time so a 200-step scenario collapses in O(log n) passes rather
// than O(n^2) predicate calls.
func Shrink(s Scenario, stillFails func(Scenario) bool) Scenario {
	steps := append([]Step(nil), s.Steps...)
	mk := func(st []Step) Scenario {
		return Scenario{Seed: s.Seed, NumPeers: s.NumPeers, Steps: st}
	}
	remove := func(src []Step, i, size int) []Step {
		out := make([]Step, 0, len(src)-size)
		out = append(out, src[:i]...)
		out = append(out, src[i+size:]...)
		return out
	}

	// Phase 1: remove contiguous chunks. Halve the granularity each sweep and
	// loop the whole thing until a full sweep removes nothing.
	changed := true
	for changed {
		changed = false
		for size := len(steps) / 2; size >= 1; size /= 2 {
			for i := 0; i+size <= len(steps); {
				if cand := remove(steps, i, size); stillFails(mk(cand)) {
					steps = cand
					changed = true
					// Keep i; the window now holds the following steps.
				} else {
					i += size
				}
			}
		}
	}

	// Phase 2: payload simplification — shorten string values while the failure
	// persists (never to empty, which would change the op's semantics). Truncate
	// by rune, not byte: generated StrVal holds multi-byte UTF-8 (Greek/Japanese/
	// emoji), and slicing mid-rune would produce invalid UTF-8 and change the
	// scenario's behaviour rather than shrink it.
	for i := range steps {
		for {
			r := []rune(steps[i].StrVal)
			if len(r) <= 1 {
				break
			}
			cand := append([]Step(nil), steps...)
			cand[i].StrVal = string(r[:len(r)-1])
			if !stillFails(mk(cand)) {
				break
			}
			steps = cand
		}
	}
	return mk(steps)
}
