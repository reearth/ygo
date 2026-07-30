package crdt_test

import (
	"math"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// These tests guard a yjs -> ygo interop guarantee: a NESTED shared type (a
// Y.Map stored as the value of another Y.Map's key) must survive every update
// round-trip ygo performs, with all of its entries and their exact values.
//
// Motivation: a downstream app (reearth-flow) observed a workflow node whose
// nested `position` Y.Map arrived EMPTY (keys: []) even though the yjs client
// always writes both x and y. This suite proves ygo is NOT the source of that
// data loss by feeding it the exact bytes a real yjs client emits and asserting
// the nested map round-trips intact through the V1 wire format, the V2 snapshot
// format, and a cross V1<->V2 persist-then-serve cycle. It also pins how ygo
// handles a non-finite float (NaN) inside a nested map.
//
// API-asymmetry note (RESOLVED): ygo used to expose no public API for
// constructing a nested Y.Map/Y.Array and attaching it to a map already in the
// doc — YMap.Set wrapped Go values into ContentAny, and only YXml had public
// prelim constructors. crdt/prelim.go now provides NewMapPrelim, NewTextPrelim,
// NewArrayPrelim and YArray.PushType, so ygo can construct these shapes itself.
//
// The fixtures below stand regardless: feeding ygo genuine yjs bytes remains the
// faithful reproduction of the downstream bug this suite guards, and is a
// stronger interop check than round-tripping our own output.
//
// Fixtures are genuine yjs@13.6.31 output for the doc shape:
//
//	root map "workflows" -> "wf-1" (Y.Map) -> "nodes" (Y.Map)
//	    -> "nodeA" (Y.Map) -> "position" (Y.Map){x:100, y:200}   // sibling
//	    -> "nodeB" (Y.Map) -> "position" (Y.Map){x, y}           // target
//
// patternB = nested map built detached then attached; patternA = attached then
// mutated. (mustHex lives in content_yjs_conformance_test.go in this package.)
const (
	// patternB (detached-then-attach), nodeB.position = {x:100.5, y:-200.25}
	fxNestedPatternBNumbersV1 = "010baed5e49b0500270109776f726b666c6f77730477662d31012700aed5e49b0500056e6f646573012700aed5e49b0501056e6f646541012700aed5e49b050208706f736974696f6e012800aed5e49b05030178017da4012800aed5e49b05030179017d88032700aed5e49b0501056e6f646542012800aed5e49b05060269640177056e6f6465422700aed5e49b050608706f736974696f6e012800aed5e49b05080178017c42c900002800aed5e49b05080179017cc348400000"
	fxNestedPatternBNumbersV2 = "000006eeaac9b70a090900030100440a000400000b27032801270028002700283f32776f726b666c6f777377662d316e6f6465736e6f646541706f736974696f6e78796e6f6465426964706f736974696f6e787909044500084100050208410003010000024104024103010b007da4017d880377056e6f6465427c42c900007cc348400000"

	// patternA (attach-then-mutate), nodeB.position = {x:100.5, y:-200.25}
	fxNestedPatternANumbersV1 = "010bc2d7f4b60500270109776f726b666c6f77730477662d31012700c2d7f4b60500056e6f646573012700c2d7f4b60501056e6f646541012700c2d7f4b6050208706f736974696f6e012800c2d7f4b605030178017da4012800c2d7f4b605030179017d88032700c2d7f4b60501056e6f646542012800c2d7f4b605060269640177056e6f6465422700c2d7f4b6050608706f736974696f6e012800c2d7f4b605080178017c42c900002800c2d7f4b605080179017cc348400000"

	// patternB with x=NaN, y=42 (non-finite float handling)
	fxNestedPatternBNaNV1 = "010be3b5d6e40300270109776f726b666c6f77730477662d31012700e3b5d6e40300056e6f646573012700e3b5d6e40301056e6f646541012700e3b5d6e4030208706f736974696f6e012800e3b5d6e403030178017da4012800e3b5d6e403030179017d88032700e3b5d6e40301056e6f646542012800e3b5d6e403060269640177056e6f6465422700e3b5d6e4030608706f736974696f6e012800e3b5d6e403080178017b7ff80000000000002800e3b5d6e403080179017d2a00"
	fxNestedPatternBNaNV2 = "000006e3ebacc907090900030100440a000400000b27032801270028002700283f32776f726b666c6f777377662d316e6f6465736e6f646541706f736974696f6e78796e6f6465426964706f736974696f6e787909044500084100050208410003010000024104024103010b007da4017d880377056e6f6465427b7ff80000000000007d2a00"
)

// nestedPositionOf navigates workflows -> wf-1 -> nodes -> <node> -> position
// via Entries(), which recursively unwraps nested shared types. (YMap.Get does
// NOT unwrap a nested ContentType; Entries/ToJSON is the only read path that
// surfaces a nested map's contents.)
func nestedPositionOf(t *testing.T, doc *crdt.Doc, node string) (map[string]any, bool) {
	t.Helper()
	root := doc.GetMap("workflows").Entries()
	wf, ok := root["wf-1"].(map[string]any)
	if !ok {
		t.Fatalf("workflows[wf-1] missing or not a map: %#v", root["wf-1"])
	}
	nodes, ok := wf["nodes"].(map[string]any)
	if !ok {
		t.Fatalf("wf-1[nodes] missing or not a map: %#v", wf["nodes"])
	}
	nb, ok := nodes[node].(map[string]any)
	if !ok {
		t.Fatalf("nodes[%s] missing or not a map: %#v", node, nodes[node])
	}
	pos, ok := nb["position"].(map[string]any)
	return pos, ok
}

func nestedToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

func nestedKeysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// assertNestedPositionNumbers checks the target node's position survived intact,
// and that the sibling node's position also survived (baseline sanity).
func assertNestedPositionNumbers(t *testing.T, doc *crdt.Doc, wantX, wantY float64) {
	t.Helper()
	pos, ok := nestedPositionOf(t, doc, "nodeB")
	if !ok {
		t.Fatalf("nodeB.position missing or not a map (the empty-map bug)")
	}
	if len(pos) == 0 {
		t.Fatalf("nodeB.position is an EMPTY map (keys: []) — nested-map data loss")
	}
	x, hasX := pos["x"]
	y, hasY := pos["y"]
	if !hasX || !hasY {
		t.Fatalf("nodeB.position missing x/y: keys=%v value=%#v", nestedKeysOf(pos), pos)
	}
	xf, okx := nestedToFloat(x)
	yf, oky := nestedToFloat(y)
	if !okx || !oky {
		t.Fatalf("nodeB.position x/y not numeric: x=%#v (%T) y=%#v (%T)", x, x, y, y)
	}
	if xf != wantX || yf != wantY {
		t.Fatalf("nodeB.position wrong values: got x=%v y=%v, want x=%v y=%v", xf, yf, wantX, wantY)
	}
	sib, ok := nestedPositionOf(t, doc, "nodeA")
	if !ok || len(sib) == 0 {
		t.Fatalf("sibling nodeA.position empty/missing: %#v", sib)
	}
}

// TestNestedMapYjsInterop_PatternB_Numbers round-trips a nested map built with
// yjs's detached-then-attach ordering through every ygo update path.
func TestNestedMapYjsInterop_PatternB_Numbers(t *testing.T) {
	const wantX, wantY = 100.5, -200.25
	v1 := mustHex(t, fxNestedPatternBNumbersV1)
	v2 := mustHex(t, fxNestedPatternBNumbersV2)

	// Baseline: apply the raw yjs-produced V1 update into a fresh ygo doc.
	base := crdt.New()
	if err := crdt.ApplyUpdateV1(base, v1, nil); err != nil {
		t.Fatalf("ApplyUpdateV1(js v1): %v", err)
	}
	t.Run("baseline_apply_js_v1", func(t *testing.T) {
		assertNestedPositionNumbers(t, base, wantX, wantY)
	})

	// (a) V1 wire round-trip: re-encode V1 (what y-websocket sends clients)
	//     then decode into a fresh doc.
	t.Run("v1_wire_roundtrip", func(t *testing.T) {
		d := crdt.New()
		if err := d.ApplyUpdate(base.EncodeStateAsUpdate()); err != nil {
			t.Fatalf("ApplyUpdate(re-encoded v1): %v", err)
		}
		assertNestedPositionNumbers(t, d, wantX, wantY)
	})

	// (b) V2 persist round-trip: EncodeStateAsUpdateV2 -> ApplyUpdateV2 (the
	//     doc_v2 snapshot format). Also apply the raw yjs V2 bytes directly.
	t.Run("v2_persist_roundtrip", func(t *testing.T) {
		d := crdt.New()
		if err := crdt.ApplyUpdateV2(d, crdt.EncodeStateAsUpdateV2(base, nil), nil); err != nil {
			t.Fatalf("ApplyUpdateV2(re-encoded v2): %v", err)
		}
		assertNestedPositionNumbers(t, d, wantX, wantY)

		djs := crdt.New()
		if err := crdt.ApplyUpdateV2(djs, v2, nil); err != nil {
			t.Fatalf("ApplyUpdateV2(js v2): %v", err)
		}
		assertNestedPositionNumbers(t, djs, wantX, wantY)
	})

	// (c) Cross round-trip persist-then-serve: V1 -> V2 -> V1.
	t.Run("cross_v1_v2_v1", func(t *testing.T) {
		mid := crdt.New()
		if err := crdt.ApplyUpdateV2(mid, crdt.EncodeStateAsUpdateV2(base, nil), nil); err != nil {
			t.Fatalf("ApplyUpdateV2(mid): %v", err)
		}
		final := crdt.New()
		if err := crdt.ApplyUpdateV1(final, crdt.EncodeStateAsUpdateV1(mid, nil), nil); err != nil {
			t.Fatalf("ApplyUpdateV1(final): %v", err)
		}
		assertNestedPositionNumbers(t, final, wantX, wantY)
	})
}

// TestNestedMapYjsInterop_PatternA_Numbers repeats the core round-trips with the
// attach-then-mutate ordering.
func TestNestedMapYjsInterop_PatternA_Numbers(t *testing.T) {
	const wantX, wantY = 100.5, -200.25
	base := crdt.New()
	if err := crdt.ApplyUpdateV1(base, mustHex(t, fxNestedPatternANumbersV1), nil); err != nil {
		t.Fatalf("ApplyUpdateV1(js v1): %v", err)
	}
	assertNestedPositionNumbers(t, base, wantX, wantY)

	d := crdt.New()
	if err := crdt.ApplyUpdateV2(d, crdt.EncodeStateAsUpdateV2(base, nil), nil); err != nil {
		t.Fatalf("ApplyUpdateV2: %v", err)
	}
	assertNestedPositionNumbers(t, d, wantX, wantY)
}

// TestNestedMapYjsInterop_NaN pins how ygo handles a non-finite float (x=NaN)
// inside a nested map. yjs writes NaN as an 8-byte IEEE-754 float64; ygo must
// preserve it (not coerce to 0/null, not drop the key), and must leave the
// sibling y value untouched.
func TestNestedMapYjsInterop_NaN(t *testing.T) {
	v1 := mustHex(t, fxNestedPatternBNaNV1)
	v2 := mustHex(t, fxNestedPatternBNaNV2)

	check := func(t *testing.T, doc *crdt.Doc, label string) {
		pos, ok := nestedPositionOf(t, doc, "nodeB")
		if !ok {
			t.Fatalf("[%s] nodeB.position missing/not a map", label)
		}
		x, hasX := pos["x"]
		y, hasY := pos["y"]
		if !hasX || !hasY {
			t.Fatalf("[%s] nested key dropped: keys=%v", label, nestedKeysOf(pos))
		}
		xf, isF := nestedToFloat(x)
		if !isF || !math.IsNaN(xf) {
			t.Fatalf("[%s] x not preserved as NaN: got %#v (%T)", label, x, x)
		}
		if yf, isF := nestedToFloat(y); !isF || yf != 42 {
			t.Fatalf("[%s] y corrupted: got %#v, want 42", label, y)
		}
	}

	t.Run("v1", func(t *testing.T) {
		d := crdt.New()
		if err := crdt.ApplyUpdateV1(d, v1, nil); err != nil {
			t.Fatalf("ApplyUpdateV1(nan v1): %v", err)
		}
		check(t, d, "js-v1")
		d2 := crdt.New()
		if err := d2.ApplyUpdate(d.EncodeStateAsUpdate()); err != nil {
			t.Fatalf("re-encode v1: %v", err)
		}
		check(t, d2, "reencoded-v1")
	})

	t.Run("v2", func(t *testing.T) {
		d := crdt.New()
		if err := crdt.ApplyUpdateV2(d, v2, nil); err != nil {
			t.Fatalf("ApplyUpdateV2(nan v2): %v", err)
		}
		check(t, d, "js-v2")
	})
}
