# YGO XML wire-conformance — root causes, fix, and verification

> **Status:** GREEN — spike Tests 1/2/3 all PASS; new `yxml_yjs_conformance_test.go` PASS (incl. byte-identity vs yjs@13.6.31); full repo test suite PASS.
> **Branch:** `fix/yxml-wire-conformance` @ `71439ad` in `/tmp/ygo-port` (fork of reearth/ygo v1.31.0). Upstream-PR-ready; not pushed (spike/fork-only constraint, no remote credentials).
> **Date:** 2026-07-10 · Worker: Claude Code (Fable 5)

## Classification — CONFIRMED (and sharpened)

The spike's classification holds: **overlooked wire-conformance corner, not a broken port.** Decode + integrate of genuine yjs XML bytes were already fully correct (evidence: the heading `level` item was present in the element's `itemMap` as `ContentAny{[1]}` after apply). Byte-diffing surfaced **three** defects — one per layer — plus one latent V2 bug the spike never reached:

## Root causes (per file:line, at v1.31.0 = `491d001`)

### 1. Test 1 (JS→Go): attribute getters filtered to string values — `crdt/yxml.go:284,300`

y-prosemirror writes `heading level=1` as a **number** (`ContentAny([1])`, `parentSub="level"`, parent-by-ID → the heading item). ygo decoded and integrated it perfectly; `GetAttribute`/`GetAttributes` then dropped any `ContentAny` whose value wasn't a Go `string` (`yxml.go:284-288`, `300-304`). The wire was innocent; the *getter* lost the data.

**Fix:** getters stringify scalars (JS `String()` semantics); new typed API `SetAttributeValue` / `GetAttributeValue` / `GetAttributeValues` (`crdt/yxml.go:317,392,417`) preserves the typed value end-to-end so Go can author `level=1` as a number.

### 2. Tests 2+3 (Go→JS + convergence): detached subtrees created child items before containers — `crdt/yxml.go:69` (`Insert`), `crdt/ytext.go` mutators

The spike (and any y-prosemirror-style caller) builds bottom-up: fill the `YXmlText`, wrap in `<paragraph>`, attach to the fragment last. ygo materialised items **immediately** with real clocks even inside detached types, so the wire carried, e.g., `string@0 → parent item@7 → parent item@8` — **same-client forward parent references**:

- **yjs crash:** `Item.getMissing` (yjs.mjs:9893) deliberately *skips* the missing-struct check for same-client parents — in yjs the invariant "container clock < child clocks" always holds because detached types only buffer. `find()` then dereferences a struct list that doesn't exist yet → `TypeError ... reading 'length'` in `findIndexSS` (yjs.mjs:2869). Unfixable from the Go side; the bytes must never be produced.
- **ygo self-apply emptied the fragment:** `decodeAndPark` (`crdt/update.go:563`) parks the clock-7/8 items behind a same-client clock gap (watermark 0), while the clock-0 item waits for its clock-7 parent (`update.go:698`, `itemFutureDep`) — a mutual deadlock; `store.retryable` never fires, nothing integrates.

**Fix (yjs parity, not a rewrite):** detached types buffer; attach flushes top-down.
- `crdt/abstract_type.go:115` — `detached()` (`item == nil && name == ""`).
- `crdt/yxml.go:40` `prelimChildren` (+ buffering `Insert`/`Delete`/`Len`), `:267` `prelimAttrs` (+ buffering `SetAttributeValue`/`DeleteAttribute`).
- `crdt/ytext.go:27` `pending` — `Insert`/`InsertEmbed`/`Delete`/`Format`/`ApplyDelta` buffer when detached (Yjs `YText._pending` parity).
- `crdt/item.go:293` — `integrate` flushes the wrapped type's prelim buffers right after setting the container back-pointer (Yjs `ContentType.integrate → type._integrate` position); children before attributes, matching `YXmlElement._integrate` order. Remote-decoded types have empty buffers → no-op on the apply path.

Result: for the same build script and pinned clientID, ygo now emits bytes **byte-identical to yjs in both V1 and V2** (proved by the `author_*` fixtures).

### 3. Latent (found by byte-diff, spike never reached it): V2 `writeKey` deduplicated keys — `crdt/update_v2.go:97`

yjs's `UpdateEncoderV2.writeKey` has dedup **deliberately disabled** upstream — the `keyMap.set(key, keyClock)` line is commented out with: *"I forgot to set the keyclock. So everything was working fine... Older clients won't be able to read updates when we reintroduce this feature."* ygo populated its `keyMap`, so any repeated key (two `strong` format items, repeated `paragraph`/`list_item` node names) made V2 bytes drift from the reference — and risked being unreadable by older yjs clients whose `ContentFormat` decoder used `readString`. Encoder now writes a fresh keyClock+string per occurrence (`update_v2.go:107`); the decoder keeps dedup support, exactly like yjs `readKey`.

## TDD artifacts

- `testutil/gen_fixtures_yxml.js` → `crdt/testdata/yxml_yjs_fixtures.json`: 9 fixtures from yjs@13.6.31 (Burrow's node_modules) — scalar attr types, heading+marks, nested elements, overlapping marks, 3 bottom-up authoring scripts (pinned clientIDs), append-diff vs remote base, concurrent merge.
- `crdt/yxml_yjs_conformance_test.go` (mirrors `ymap_yjs_conformance_test.go`), RED on v1.31.0, now GREEN:
  - `DecodeYjsBytes` — apply V1+V2, canonical tree equality (typed attrs incl. `level=1`).
  - `ReencodeByteIdentical` — re-encode after apply must equal reference bytes, V1 **and** V2 (this is what exposed defect 3).
  - `AuthorBytesMatchYjs` / `AuthorDiffMatchesYjs` — Go replays the yjs build script, same clientID → byte-identical output (V1+V2), incl. diff-vs-state-vector.
  - `AuthorSelfRoundTrip` — the spike's "fragment empties on self-apply" regression guard.
  - `ConcurrentMerge` — both application orders converge to the yjs reference merge.

## Verification (all commands run niced, capped parallelism)

- `go test ./...` — **all packages PASS** (incl. `TestCompat_RoundTrip_JSGoJS`, which drives live node/yjs over ygo's re-encodes of XML shapes; needed `testutil/node_modules` symlink → yjs 13.6.31).
- Spike harness `/tmp/ygo-spike/run.sh` against the fork (`replace` in spike `go.mod`): **Test 1 PASS, Test 2 PASS (v1+v2), Test 3 PASS** — verdict flipped to "ygo IS byte-fidelity-compatible". yjs applies Go-authored updates without error and renders `<paragraph>from go</paragraph>`; concurrent GO-END/JS-MIDDLE edits converge identically on both sides.
- **Profiler/benchstat** (per request): allocs/op identical to base on `YText_Insert`, `ApplyUpdateV1/V2`, `TwoPeerConvergence`; `detached`/`flushPrelim`/`buffer` absent from the CPU profile. An earlier draft cost +1 alloc/op on every attached `YText.Insert` (closure built before the detached check) — caught by benchstat and fixed (closure now built only in the detached branch). Residual ±5% swings on this shared box have no alloc/profile signal (noise).

## Honest notes / disclosures

1. **Spike harness change (comparison-only):** `spike-harness/go/canonical.go` serialised empty mark lists as `null` and omitted empty `attrs`/`children`, while the JS canonicaliser emits `[]`/`{}` — test 3 compares raw JSON strings across languages, so this Go-JSON idiom noise alone failed it even with correct CRDT bytes (it also predates the fix; the original RESULT.md called the attrs half "cosmetic"). I added a `MarshalJSON` that emits the exact JS canonical shape. No ygo behaviour, no expected values, and no comparison logic changed.
2. **Byte-identity scope:** Go-authored = yjs-authored byte-identity is proven for the fixture scripts (same clientID). Distinct client IDs obviously produce different bytes; semantic convergence for that case is covered by the concurrent fixtures + spike test 3.
3. **`detached()` edge:** a root type named `""` would classify as detached; yjs's own prelim handling has the same degenerate corner. Not reachable from Burrow usage.
4. **Not in scope, unchanged:** YMap/YArray nested-type authoring goes through `contentForValue`, which does not accept shared types, so the bottom-up hazard is XML/YText-specific in ygo's current API surface. The `item.integrate` flush hook is generic, so if nested shared-type `Set` is ever added, prelim types will flush correctly there too.

## Acceptance checklist (PROJECT.md)

- [x] `yxml_yjs_conformance_test.go` roundtrips real yjs bytes: attributes, nested elements, text+marks, concurrent merge — PASS.
- [x] Spike Test 1/2/3 PASS against the fork.
- [x] Root cause per failure with `file:line` (this document) + patch on fork branch `fix/yxml-wire-conformance` (`71439ad`). Upstream PR not opened (no push credentials in this environment); the commit message is PR-body-ready.
- [x] Fixability confirmed empirically: bounded wire/API patch (5 library files, +249/−24), NOT a structural rewrite.
- [x] Result reported back (this file + report to opus-manager).
