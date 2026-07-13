#!/usr/bin/env node
// fuzz_oracle.js — replays fuzz Scenarios against Yjs, one NDJSON line in/out.
//
// This worker MUST mirror testutil/fuzz/interpret.go semantics EXACTLY so that
// any difference between its output and ygo's replay is a genuine ygo↔Yjs CRDT
// divergence, not a harness artifact. In particular the transport discipline
// matches RunGo/applySync/drainInboxes/finalSync:
//
//   * Diff*/Merge* updates are staged in per-peer inboxes keyed by wire version
//     (inboxV1/inboxV2). V1 and V2 are distinct, non-interchangeable encodings,
//     so an update is only ever merged/applied through its own decoder — never
//     guessed (mirrors peerState.inboxV1/inboxV2, see interpret.go).
//   * Apply* syncs land immediately using an sv-diff of the source against the
//     destination's state vector.
//   * Merge* pushes the source's FULL state into the version bucket, merges the
//     WHOLE bucket, applies it, and clears the bucket (so it also drains any
//     Diff* entries already staged in that bucket) — exactly like Go's MergeV1/
//     MergeV2 cases.
//   * After the step list: drainInboxes applies every staged update one at a
//     time through its own decoder, then finalSync forces a full-mesh V1 sync
//     (encode each peer's full state, mergeUpdates all, apply the merge to every
//     peer). Without this final full-mesh sync the peers would be left in a
//     partially-connected state and disagree with Go's converged peers for
//     reasons unrelated to CRDT correctness.
//
// clamp mirrors clampIndex(pos,length,forInsert). ygo's YText.Len() is UTF-16
// code-unit length, matching Yjs's .length, so index math agrees on emoji/
// multi-byte text.
const Y = require('yjs')
const readline = require('readline')

const clamp = (pos, len, ins) => {
  if (pos < 0) pos = -pos
  const m = ins ? len + 1 : len
  return m <= 0 ? 0 : pos % m
}

// resolveElem mirrors resolveXmlElem: walk the child-index path from the
// fragment's top-level children down through nested elements. Returns null if
// target is empty, the fragment has no children, or the first hop is not an
// element; stops and returns the current element if a deeper hop steps into a
// non-element (e.g. an XmlText) or an empty element.
const resolveElem = (f, target) => {
  const cs = f.toArray()
  if (!cs.length || !target.length) return null
  let el = cs[target[0] % cs.length]
  if (!(el instanceof Y.XmlElement)) return null
  for (const idx of target.slice(1)) {
    const cc = el.toArray()
    if (!cc.length) return el
    const nx = cc[idx % cc.length]
    if (!(nx instanceof Y.XmlElement)) return el
    el = nx
  }
  return el
}

// delCount mirrors ygo's deleteRange: from `idx` it removes up to `want`
// elements OR stops at the end of the list, never past it. Yjs's delete throws
// "Length exceeded" when idx+want > length, so we clamp want to length-idx —
// the exact number of elements ygo would have tombstoned.
const delCount = (want, len, idx) => Math.min(want || 1, len - idx)

const applyLocal = (d, st) => {
  if (st.typeKind === 'text') {
    const t = d.getText(st.root)
    if (st.op === 'insert') t.insert(clamp(st.posHint || 0, t.length, true), st.strVal)
    else if (st.op === 'delete' && t.length > 0) {
      const idx = clamp(st.posHint || 0, t.length, false)
      t.delete(idx, delCount(st.lenHint, t.length, idx))
    }
  } else if (st.typeKind === 'array') {
    const a = d.getArray(st.root)
    if (st.op === 'insert') a.insert(clamp(st.posHint || 0, a.length, true), [JSON.parse(st.jsonVal)])
    else if (st.op === 'push') a.push([JSON.parse(st.jsonVal)])
    else if (st.op === 'delete' && a.length > 0) {
      const idx = clamp(st.posHint || 0, a.length, false)
      a.delete(idx, delCount(st.lenHint, a.length, idx))
    }
  } else if (st.typeKind === 'map') {
    const m = d.getMap(st.root)
    if (st.op === 'setkey') m.set(st.key, JSON.parse(st.jsonVal))
    else if (st.op === 'delkey') m.delete(st.key)
  } else if (st.typeKind === 'xmlfrag') {
    const f = d.getXmlFragment(st.root)
    if (st.op === 'addchild') {
      const idx = clamp(st.posHint || 0, f.length, true)
      if (st.childXml === 'text') f.insert(idx, [new Y.XmlText()])
      else f.insert(idx, [new Y.XmlElement((st.childXml || 'elem:div').slice(5) || 'div')])
    } else if (st.op === 'delete') {
      if (f.length > 0) f.delete(clamp(st.posHint || 0, f.length, false), 1)
    } else if (st.op === 'setattr' || st.op === 'delattr') {
      const el = resolveElem(f, st.target || [])
      if (el) {
        if (st.op === 'setattr') el.setAttribute(st.key, st.strVal)
        else el.removeAttribute(st.key)
      }
    }
  }
}

// svApply mirrors the Apply* cases: sv-diff of `from` against `to`'s state
// vector, applied immediately.
const svApply = (from, to, ver) => {
  if (ver === 1) Y.applyUpdate(to, Y.encodeStateAsUpdate(from, Y.encodeStateVector(to)))
  else Y.applyUpdateV2(to, Y.encodeStateAsUpdateV2(from, Y.encodeStateVector(to)))
}

const runScenario = (s) => {
  const n = s.numPeers
  const docs = []
  const inboxV1 = []
  const inboxV2 = []
  for (let i = 0; i < n; i++) {
    // NB: the Y.Doc constructor has NO clientID option — passing one is
    // silently ignored and Yjs assigns a RANDOM clientID (generateNewClientId).
    // clientID is YATA's concurrent-insert tie-breaker, so a random one both
    // makes replays non-deterministic and stops them matching ygo's
    // WithClientID(i+1). Assign it explicitly, before any struct is created.
    const d = new Y.Doc()
    d.clientID = i + 1
    docs.push(d)
    inboxV1.push([])
    inboxV2.push([])
  }

  for (const st of s.steps) {
    if (st.kind === 'op') {
      applyLocal(docs[(st.peer || 0) % n], st)
    } else if (st.kind === 'gc') {
      // ygo forces a commit pass; Yjs GCs at transaction end. No observable
      // logical effect — no-op here.
    } else if (st.kind === 'sync') {
      const fi = (st.from || 0) % n
      const ti = (st.to || 0) % n
      const from = docs[fi]
      const to = docs[ti]
      switch (st.method) {
        case 'applyv1':
          svApply(from, to, 1)
          break
        case 'applyv2':
          svApply(from, to, 2)
          break
        case 'diffv1':
          inboxV1[ti].push(Y.diffUpdate(Y.encodeStateAsUpdate(from), Y.encodeStateVector(to)))
          break
        case 'diffv2':
          inboxV2[ti].push(Y.diffUpdateV2(Y.encodeStateAsUpdateV2(from), Y.encodeStateVector(to)))
          break
        case 'mergev1':
          inboxV1[ti].push(Y.encodeStateAsUpdate(from))
          Y.applyUpdate(to, Y.mergeUpdates(inboxV1[ti]))
          inboxV1[ti] = []
          break
        case 'mergev2':
          inboxV2[ti].push(Y.encodeStateAsUpdateV2(from))
          Y.applyUpdateV2(to, Y.mergeUpdatesV2(inboxV2[ti]))
          inboxV2[ti] = []
          break
      }
    }
  }

  // drainInboxes: apply every staged update one at a time through its own
  // decoder, then clear.
  for (let i = 0; i < n; i++) {
    for (const u of inboxV1[i]) Y.applyUpdate(docs[i], u)
    inboxV1[i] = []
    for (const u of inboxV2[i]) Y.applyUpdateV2(docs[i], u)
    inboxV2[i] = []
  }

  // finalSync: full-mesh V1 sync so every peer holds identical state.
  const fullStates = docs.map((d) => Y.encodeStateAsUpdate(d))
  const merged = Y.mergeUpdates(fullStates)
  for (const d of docs) Y.applyUpdate(d, merged)

  const roots = [['t', 'text'], ['a', 'array'], ['m', 'map'], ['x', 'xmlfrag']]
  const out = { peerJSON: [], peerUpdateB64: [] }
  for (const d of docs) {
    const j = {}
    for (const [name, kind] of roots) {
      if (kind === 'text') j[name] = d.getText(name).toString()
      else if (kind === 'array') j[name] = d.getArray(name).toJSON()
      else if (kind === 'map') j[name] = d.getMap(name).toJSON()
      else if (kind === 'xmlfrag') j[name] = d.getXmlFragment(name).toString()
    }
    out.peerJSON.push(j)
    out.peerUpdateB64.push(Buffer.from(Y.encodeStateAsUpdate(d)).toString('base64'))
  }
  return out
}

const rl = readline.createInterface({ input: process.stdin })
rl.on('line', (line) => {
  if (!line.trim()) return
  const s = JSON.parse(line)
  process.stdout.write(JSON.stringify(runScenario(s)) + '\n')
})
