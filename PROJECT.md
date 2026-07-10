# Project — ygo XML wire-conformance (port/repair): collapse the live-collab CRDT into the Go binary

> **Status:** SCOPED — ready for build. Worker: **Claude Code (Fable 5)** via claude-proxy.
> **Basis:** spike verdict [link:5de6f156-e6cb-4089-b73e-959debb82e82] · Go-Yjs min-research [link:9f3aaf8d-18b7-473a-806d-f9ac9546e6c0]
> **Type:** project · **Date:** 2026-07-10 · **Owner:** opus-manager (reviews); Fable 5 (builds)

## Goal (one sentence)

Make **`github.com/reearth/ygo`** (pure-Go Yjs) byte-conformant with **`y-prosemirror`** over `Y.XmlFragment("prosemirror")`, so Burrow can host the live-collab CRDT **inside the Go binary** and delete the Node doc-server — back to **one binary + one DB**, with no service macaroon and no 12h token expiry.

## Why now

On 2026-07-10 the Node Yjs doc-server (`burrow-collab`) went down for all cold pages: its **service macaroon expired** (agent credential wrongly used by internal infra) → rooms dropped → pages fell to the raw-markdown "code-box". That reopened: *can the CRDT run natively in Go?* A live fidelity spike ([link:5de6f156-e6cb-4089-b73e-959debb82e82]) proved ygo is NOT yet byte-compatible with y-prosemirror — **but classified the gap as an OVERLOOKED wire-conformance corner, not a broken port.**

## Classification (observed in ygo source, not inferred)

The port's **architecture is faithful**: XML attributes are real CRDT map-items (`yxml.go:208` — `ParentSub = attribute name`), `integrate()` does proper last-writer-wins (`item.go:287`), and the encoder mirrors yjs `Item.write` (`update.go` `encodeItem`/`encodeContent`) already carrying ~4 issue-referenced wire fixes (#125/#140/#YMap-wire/#wire-conformance).

**Smoking gun for "overlooked":** YMap, YText, and Subdoc each have a **JS-fixture conformance test** (`ymap_yjs_conformance_test.go`, `ytext_format_js_compat_test.go`, `subdoc_js_compat_test.go`) that applies real yjs@13.6 bytes; **the XML types have none** (`yxml_test.go` is Go→Go only). The XML wire path was simply never hardened against JavaScript bytes — which is exactly where both failures live.

## The two codebases (drill both)

1. **`reearth/ygo` v1.31.0 — the port to repair.** Reference (read): `/root/go/pkg/mod/github.com/reearth/ygo@v1.31.0`. Work in a **writable clone/fork** (`/tmp/ygo-port`). Key files: `crdt/update.go` (V1/V2 encode+decode, `encodeItem`/`encodeContent`/apply), `crdt/yxml.go` (YXmlElement/Fragment, attributes), `crdt/item.go` (`integrate`, ParentSub map machinery), `crdt/ymap_yjs_conformance_test.go` + `testdata/ymap_yjs_fixtures.json` (the pattern to mirror for XML).
2. **Burrow's editor usage — the reference for the shapes we must support.** `/root/burrow/core/web` (read-only): `@milkdown/plugin-collab` binds `getXmlFragment("prosemirror")`; `y-prosemirror` maps the ProseMirror/Milkdown doc (headings w/ `level` attr, paragraphs, marks, nested elements) into that fragment. The spike harness `/tmp/ygo-spike` already captures the **correct yjs reference bytes** for these shapes.

## Known failures to fix (from the spike, all deterministic/serialization)

| # | Direction | Symptom | Likely area |
|---|---|---|---|
| 1 | JS → Go | heading `level` **attribute dropped** (text + bold survive) | attribute-item decode/attach (`update.go` decode + `item.go` integrate) |
| 2 | Go → JS | ygo-authored update **crashes yjs**, AND ygo can't re-apply its **own** encode (fragment empties) | nested-child **encode ordering / parent bytes** (`encodeItem`/`encodeContent`) |
| 3 | concurrent | **no convergence** (follows from #1/#2) | resolves once #1/#2 are byte-correct |

## Method (TDD, the maintainers' own pattern)

1. **Drill both codebases**; write up the precise defect per failure (ygo `file:line`, what byte/field is wrong vs the yjs reference). Confirm/refine the classification.
2. **Capture yjs fixtures** (yjs@13.6, from Burrow's node_modules): element+attributes, nested elements, text+marks, and a concurrent-merge scenario → `testdata/yxml_yjs_fixtures.json`.
3. **Add `crdt/yxml_yjs_conformance_test.go`** mirroring the ymap one → watch it go **RED** on our exact cases.
4. **Stack the missing wire blocks** in `encodeItem`/`encodeContent` + decode until GREEN. One field at a time, byte-diffed against the reference.
5. **Re-run the spike harness** `/tmp/ygo-spike/run.sh` → **Test 1/2/3 all green**.
6. **Prepare an upstream PR** to `reearth/ygo` (this is a gap they'd obviously take), and a fork branch we can vendor meanwhile.

## Acceptance / done-when

- [ ] `yxml_yjs_conformance_test.go` roundtrips real yjs bytes for: attributes, nested elements, text+marks, concurrent merge — all PASS.
- [ ] Spike `Test 1 / 2 / 3` all PASS against the fork.
- [ ] Writeup: root cause per failure (ygo `file:line`) + the patch (fork branch) + upstream PR link if opened.
- [ ] Fixability confirmed empirically: bounded wire patch, NOT a structural rewrite.
- [ ] Result posted back to this project page.

## Out of scope (explicitly — this is the follow-on integration project, gated on this passing)

Wiring ygo into Burrow / replacing the Node doc-server / removing the macaroon / the unix-socket sealing. Those happen **only after** this proves green. This project is: **make ygo's XML wire conformant + prove it.**

## Constraints

Spike/fork work only — **no Burrow prod, no deploy**; read Burrow for reference. Adversarial + honest: a false "green" is worse than a clean "still red at case X." Every fix byte-verified against the yjs reference.
