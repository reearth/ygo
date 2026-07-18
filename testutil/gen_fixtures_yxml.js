#!/usr/bin/env node
/**
 * Generates YXml cross-language conformance fixtures from the Yjs reference
 * implementation (yjs@13.6.x).
 *
 * Usage:
 *   npm install yjs   (or NODE_PATH=<dir with yjs> node testutil/gen_fixtures_yxml.js)
 *   node testutil/gen_fixtures_yxml.js
 *
 * Output: crdt/testdata/yxml_yjs_fixtures.json
 * Loaded by crdt/yxml_yjs_conformance_test.go.
 *
 * Three fixture kinds:
 *
 *  - "decode":     full-state V1+V2 updates authored by Yjs, plus the expected
 *                  canonical tree. Go must apply them and match, then re-encode
 *                  V1 byte-identically (foreign-struct preservation).
 *  - "author":     a scripted build sequence executed by Yjs with a pinned
 *                  clientID. The Go test replays the IDENTICAL sequence with
 *                  the same clientID and must produce byte-identical V1 bytes.
 *                  All build sequences use the y-prosemirror pattern: subtrees
 *                  are built DETACHED (bottom-up, prelim content/attrs) and
 *                  attached last. Yjs only materialises items at attach time,
 *                  top-down — the exact wire-ordering corner ygo got wrong.
 *  - "concurrent": a base doc plus two concurrent diff updates (one of them
 *                  authored via the detached/prelim path). Go must converge to
 *                  the expected canonical tree in either application order.
 *
 *  - "decode_xmlstring": the original XML scenario from
 *                  gen_conformance_fixtures.js (root "x", attach-first build,
 *                  expected = the ToXML string). Consumed by
 *                  crdt/conformance_fixtures_test.go, NOT by
 *                  yxml_yjs_conformance_test.go. It lives here because both
 *                  suites share this output file and the drift gate
 *                  regenerates it as a whole.
 *
 * For all other kinds the shared root type is always
 * Y.XmlFragment("prosemirror") — the type y-prosemirror binds (matches
 * y-prosemirror / @milkdown/plugin-collab usage).
 */

const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const outFile = path.join(__dirname, '..', 'crdt', 'testdata', 'yxml_yjs_fixtures.json')

const hex = (u8) => Buffer.from(u8).toString('hex')

// Canonical tree: elements → {tag, attrs, children}; text → {text: [{insert, attrs}]}
function canonNode(n) {
  if (n instanceof Y.XmlText) {
    return { text: n.toDelta().map((d) => ({ insert: d.insert, attrs: d.attributes ?? {} })) }
  }
  if (n instanceof Y.XmlElement) {
    return { tag: n.nodeName, attrs: n.getAttributes(), children: n.toArray().map(canonNode) }
  }
  throw new Error(`unexpected node: ${n?.constructor?.name}`)
}
const canonFragment = (frag) => frag.toArray().map(canonNode)

function newDoc(clientID) {
  const doc = new Y.Doc()
  doc.clientID = clientID
  if (doc.clientID !== clientID) throw new Error('clientID pin failed')
  return doc
}

const fixtures = []

// ── ToXML string fixture (kind "decode_xmlstring") ──────────────────────────
// Replicates gen_conformance_fixtures.js's original yxml scenario exactly
// (clientID 1, root "x", attach-FIRST build — the complement of the
// y-prosemirror detached pattern below). Consumed by
// crdt/conformance_fixtures_test.go's TestConformance_YXml_DecodeYjsBytes.
{
  const doc = newDoc(1)
  const f = doc.getXmlFragment('x')
  const el = new Y.XmlElement('div')
  f.insert(0, [el])
  el.setAttribute('class', 'a')
  el.insert(0, [new Y.XmlText('hi')])
  fixtures.push({
    name: 'div_attr_text',
    kind: 'decode_xmlstring',
    clientID: 1,
    v1: hex(Y.encodeStateAsUpdate(doc)),
    v2: hex(Y.encodeStateAsUpdateV2(doc)),
    expected: f.toString(),
  })
}

function decodeFixture(name, clientID, build) {
  const doc = newDoc(clientID)
  build(doc.getXmlFragment('prosemirror'))
  fixtures.push({
    name,
    kind: 'decode',
    clientID,
    v1: hex(Y.encodeStateAsUpdate(doc)),
    v2: hex(Y.encodeStateAsUpdateV2(doc)),
    expected: canonFragment(doc.getXmlFragment('prosemirror')),
  })
}

function authorFixture(name, clientID, build) {
  const doc = newDoc(clientID)
  build(doc.getXmlFragment('prosemirror'))
  fixtures.push({
    name,
    kind: 'author',
    clientID,
    v1: hex(Y.encodeStateAsUpdate(doc)),
    v2: hex(Y.encodeStateAsUpdateV2(doc)),
    expected: canonFragment(doc.getXmlFragment('prosemirror')),
  })
}

// ── decode fixtures (JS → Go) ────────────────────────────────────────────────
// Built the y-prosemirror way: detached subtrees (prelim), attached last, so
// the wire layout (children items before attribute items, parent clocks below
// child clocks) matches what a y-prosemirror editor actually sends.

decodeFixture('attrs_scalar_types', 101, (frag) => {
  const h = new Y.XmlElement('heading')
  h.setAttribute('level', 1) // number — the y-prosemirror failure case
  h.setAttribute('id', 'h1') // string
  h.setAttribute('collapsed', true) // bool
  h.setAttribute('indent', 1.5) // float
  const t = new Y.XmlText()
  t.insert(0, 'Governance')
  h.insert(0, [t])
  frag.insert(0, [h])
})

decodeFixture('heading_para_marks', 102, (frag) => {
  const h = new Y.XmlElement('heading')
  h.setAttribute('level', 1)
  const ht = new Y.XmlText()
  ht.insert(0, 'Governance')
  h.insert(0, [ht])
  const p = new Y.XmlElement('paragraph')
  const pt = new Y.XmlText()
  pt.insert(0, 'hello ')
  pt.insert(6, 'world', { strong: {} })
  p.insert(0, [pt])
  frag.insert(0, [h, p])
})

decodeFixture('nested_elements', 103, (frag) => {
  const items = ['first', 'second'].map((word) => {
    const t = new Y.XmlText()
    t.insert(0, word)
    const p = new Y.XmlElement('paragraph')
    p.insert(0, [t])
    const li = new Y.XmlElement('list_item')
    li.insert(0, [p])
    return li
  })
  const ul = new Y.XmlElement('bullet_list')
  ul.setAttribute('tight', true)
  ul.insert(0, items)
  frag.insert(0, [ul])
})

decodeFixture('overlapping_marks', 104, (frag) => {
  const t = new Y.XmlText()
  t.insert(0, 'plain bold bolditalic italic tail')
  t.format(6, 15, { strong: {} }) // "bold bolditalic"
  t.format(11, 17, { em: {} }) // "bolditalic italic"
  const p = new Y.XmlElement('paragraph')
  p.insert(0, [t])
  frag.insert(0, [p])
})

// ── author fixtures (Go must byte-match these) ──────────────────────────────

// The exact spike-test2 sequence: text filled first, wrapped, attached last.
authorFixture('author_bottom_up', 1000001, (frag) => {
  const para = new Y.XmlElement('paragraph')
  const txt = new Y.XmlText()
  txt.insert(0, 'from go')
  para.insert(0, [txt])
  frag.insert(0, [para])
})

// Attributes set while detached (prelim attrs flush AFTER children) + marks.
authorFixture('author_attrs_marks', 1000002, (frag) => {
  const h = new Y.XmlElement('heading')
  h.setAttribute('level', 1)
  const t = new Y.XmlText()
  t.insert(0, 'hello ')
  t.insert(6, 'world', { strong: {} })
  h.insert(0, [t])
  frag.insert(0, [h])
})

// Three levels built leaf-first, all detached, attached last.
authorFixture('author_nested_bottom_up', 1000003, (frag) => {
  const txt = new Y.XmlText()
  txt.insert(0, 'item')
  const p = new Y.XmlElement('paragraph')
  p.insert(0, [txt])
  const li = new Y.XmlElement('list_item')
  li.insert(0, [p])
  const ul = new Y.XmlElement('bullet_list')
  ul.setAttribute('tight', true)
  ul.insert(0, [li])
  frag.insert(0, [ul])
})

// ── author-diff fixture: append to a remote base, encode diff vs base SV ────
// Mirrors spike test3's Go role: base authored by another client, local append
// built detached/bottom-up, diff update encoded against the base state vector.
{
  const baseDoc = newDoc(2000004)
  {
    const frag = baseDoc.getXmlFragment('prosemirror')
    const h = new Y.XmlElement('heading')
    h.setAttribute('level', 1)
    const ht = new Y.XmlText()
    ht.insert(0, 'Base')
    h.insert(0, [ht])
    const p = new Y.XmlElement('paragraph')
    const pt = new Y.XmlText()
    pt.insert(0, 'alpha beta gamma')
    p.insert(0, [pt])
    frag.insert(0, [h, p])
  }
  const baseV1 = Y.encodeStateAsUpdate(baseDoc)
  const baseSV = Y.encodeStateVector(baseDoc)

  const doc = newDoc(1000004)
  Y.applyUpdate(doc, baseV1)
  const frag = doc.getXmlFragment('prosemirror')
  const endPara = new Y.XmlElement('paragraph')
  const endTxt = new Y.XmlText()
  endTxt.insert(0, 'GO-END')
  endPara.insert(0, [endTxt])
  frag.insert(frag.length, [endPara])
  const diffV1 = Y.encodeStateAsUpdate(doc, baseSV)

  fixtures.push({
    name: 'author_append_diff',
    kind: 'author_diff',
    clientID: 1000004,
    baseV1: hex(baseV1),
    v1: hex(diffV1),
    expected: canonFragment(frag),
  })
}

// ── concurrent merge fixture ─────────────────────────────────────────────────
{
  const baseDoc = newDoc(3000001)
  {
    const frag = baseDoc.getXmlFragment('prosemirror')
    const h = new Y.XmlElement('heading')
    h.setAttribute('level', 1)
    const ht = new Y.XmlText()
    ht.insert(0, 'Base')
    h.insert(0, [ht])
    const p = new Y.XmlElement('paragraph')
    const pt = new Y.XmlText()
    pt.insert(0, 'alpha beta gamma')
    p.insert(0, [pt])
    frag.insert(0, [h, p])
  }
  const baseV1 = Y.encodeStateAsUpdate(baseDoc)
  const baseSV = Y.encodeStateVector(baseDoc)

  // Side A: append a paragraph at the end (detached/prelim build).
  const docA = newDoc(3000002)
  Y.applyUpdate(docA, baseV1)
  {
    const frag = docA.getXmlFragment('prosemirror')
    const endPara = new Y.XmlElement('paragraph')
    const endTxt = new Y.XmlText()
    endTxt.insert(0, 'GO-END')
    endPara.insert(0, [endTxt])
    frag.insert(frag.length, [endPara])
  }
  const updA = Y.encodeStateAsUpdate(docA, baseSV)

  // Side B: insert text mid-paragraph.
  const docB = newDoc(3000003)
  Y.applyUpdate(docB, baseV1)
  {
    const frag = docB.getXmlFragment('prosemirror')
    const para = frag.get(1)
    const txt = para.get(0)
    txt.insert(6, 'JS-MIDDLE ')
  }
  const updB = Y.encodeStateAsUpdate(docB, baseSV)

  // Reference merge — both orders must agree.
  const m1 = new Y.Doc()
  Y.applyUpdate(m1, baseV1)
  Y.applyUpdate(m1, updA)
  Y.applyUpdate(m1, updB)
  const m2 = new Y.Doc()
  Y.applyUpdate(m2, baseV1)
  Y.applyUpdate(m2, updB)
  Y.applyUpdate(m2, updA)
  const c1 = JSON.stringify(canonFragment(m1.getXmlFragment('prosemirror')))
  const c2 = JSON.stringify(canonFragment(m2.getXmlFragment('prosemirror')))
  if (c1 !== c2) throw new Error('yjs reference merge is order-dependent?!')

  fixtures.push({
    name: 'concurrent_merge',
    kind: 'concurrent',
    baseV1: hex(baseV1),
    updA: hex(updA),
    updB: hex(updB),
    expected: canonFragment(m1.getXmlFragment('prosemirror')),
  })
}

fs.mkdirSync(path.dirname(outFile), { recursive: true })
fs.writeFileSync(outFile, JSON.stringify(fixtures, null, 1) + '\n')
console.log(`yjs version: ${require('yjs/package.json').version}`)
console.log(`wrote ${outFile} (${fixtures.length} fixtures)`)
