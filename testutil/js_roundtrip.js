#!/usr/bin/env node
/**
 * js_roundtrip.js — drives a yjs → ygo → yjs round-trip for scenarios that
 * ygo cannot build through its own public API (nested types, XML elements,
 * XML attributes, deep nesting). The Go side is the middle hop: it decodes
 * each `<name>.<ver>.in` and re-encodes it to `<name>.<ver>.out`.
 *
 *   node js_roundtrip.js gen    <dir>   # build scenarios → .in files + manifest.json
 *   node js_roundtrip.js verify <dir>   # apply ygo's .out files, compare to manifest
 *
 * Each scenario declares one or more (root, kind) checks; gen records the
 * expected content (toJSON / toDelta / XML string) and verify re-extracts it
 * after applying ygo's re-encoding. Any divergence ygo introduces on decode or
 * re-encode — a dropped attribute, a lost nested child, a reordered map — makes
 * verify fail. Exit non-zero on any mismatch.
 */
const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const mode = process.argv[2]
const dir = process.argv[3]
if (!mode || !dir) {
  console.error('usage: js_roundtrip.js <gen|verify> <dir>')
  process.exit(2)
}

// ── Scenarios ────────────────────────────────────────────────────────────────
// Each: { name, build(doc), checks: [{root, kind}] } where kind ∈
// 'map' | 'array' | 'text' | 'xml'.
const scenarios = [
  {
    name: 'nested_map_in_map',
    build: (d) => {
      const m = d.getMap('m')
      const inner = new Y.Map()
      m.set('inner', inner)
      inner.set('x', 1)
      inner.set('y', 'two')
    },
    checks: [{ root: 'm', kind: 'map' }],
  },
  {
    name: 'nested_array_in_map',
    build: (d) => {
      const m = d.getMap('m')
      const arr = new Y.Array()
      m.set('list', arr)
      arr.push([1, 2, 3, 'four'])
    },
    checks: [{ root: 'm', kind: 'map' }],
  },
  {
    name: 'nested_map_in_array',
    build: (d) => {
      const a = d.getArray('a')
      const m = new Y.Map()
      a.push([m, 'sibling'])
      m.set('k', 'v')
    },
    checks: [{ root: 'a', kind: 'array' }],
  },
  {
    name: 'deep_nest',
    build: (d) => {
      const m = d.getMap('m')
      const a = new Y.Array()
      m.set('arr', a)
      const inner = new Y.Map()
      a.push([inner])
      inner.set('leaf', 42)
    },
    checks: [{ root: 'm', kind: 'map' }],
  },
  {
    name: 'nested_overwrite',
    build: (d) => {
      const m = d.getMap('m')
      m.set('n', 'scalar')
      const arr = new Y.Array()
      m.set('n', arr) // overwrite scalar with a nested type (origin-bearing LWW)
      arr.push([7, 8, 9])
    },
    checks: [{ root: 'm', kind: 'map' }],
  },
  {
    name: 'xml_basic',
    build: (d) => {
      const f = d.getXmlFragment('x')
      const div = new Y.XmlElement('div')
      f.insert(0, [div])
      div.insert(0, [new Y.XmlText('hello')])
    },
    checks: [{ root: 'x', kind: 'xml' }],
  },
  {
    name: 'xml_attrs',
    build: (d) => {
      const f = d.getXmlFragment('x')
      const div = new Y.XmlElement('div')
      f.insert(0, [div])
      div.setAttribute('class', 'box')
      div.setAttribute('id', 'main')
    },
    checks: [{ root: 'x', kind: 'xml' }],
  },
  {
    name: 'xml_attr_overwrite',
    build: (d) => {
      // XML attributes are map-keyed (parentSub) — overwriting one is the
      // same LWW/origin path as a YMap key, exercised through XML.
      const f = d.getXmlFragment('x')
      const div = new Y.XmlElement('div')
      f.insert(0, [div])
      div.setAttribute('class', 'first')
      div.setAttribute('class', 'second')
    },
    checks: [{ root: 'x', kind: 'xml' }],
  },
  {
    name: 'xml_nested',
    build: (d) => {
      const f = d.getXmlFragment('x')
      const div = new Y.XmlElement('div')
      const span = new Y.XmlElement('span')
      f.insert(0, [div])
      div.insert(0, [span])
      span.insert(0, [new Y.XmlText('inner')])
    },
    checks: [{ root: 'x', kind: 'xml' }],
  },
  {
    name: 'multi_root',
    build: (d) => {
      d.getText('t').insert(0, 'hi')
      d.getMap('m').set('a', 1)
      d.getArray('a').push([true, null])
    },
    checks: [
      { root: 't', kind: 'text' },
      { root: 'm', kind: 'map' },
      { root: 'a', kind: 'array' },
    ],
  },
]

function extract(doc, check) {
  switch (check.kind) {
    case 'map':
      return JSON.stringify(doc.getMap(check.root).toJSON())
    case 'array':
      return JSON.stringify(doc.getArray(check.root).toJSON())
    case 'text':
      return JSON.stringify(doc.getText(check.root).toDelta())
    case 'xml':
      return doc.getXmlFragment(check.root).toString()
    default:
      throw new Error(`unknown kind ${check.kind}`)
  }
}

if (mode === 'gen') {
  const manifest = []
  for (const sc of scenarios) {
    const doc = new Y.Doc()
    sc.build(doc)
    fs.writeFileSync(path.join(dir, `${sc.name}.v1.in`), Buffer.from(Y.encodeStateAsUpdate(doc)))
    fs.writeFileSync(path.join(dir, `${sc.name}.v2.in`), Buffer.from(Y.encodeStateAsUpdateV2(doc)))
    manifest.push({
      name: sc.name,
      checks: sc.checks.map((c) => ({ ...c, expected: extract(doc, c) })),
    })
  }
  fs.writeFileSync(path.join(dir, 'manifest.json'), JSON.stringify(manifest))
  console.log(`gen: wrote ${scenarios.length} scenarios`)
  process.exit(0)
}

if (mode === 'verify') {
  const manifest = JSON.parse(fs.readFileSync(path.join(dir, 'manifest.json'), 'utf8'))
  let failed = 0
  for (const entry of manifest) {
    for (const ver of ['v1', 'v2']) {
      const outPath = path.join(dir, `${entry.name}.${ver}.out`)
      if (!fs.existsSync(outPath)) {
        console.error(`FAIL ${entry.name}/${ver}: ygo produced no .out`)
        failed++
        continue
      }
      const doc = new Y.Doc()
      try {
        const bytes = new Uint8Array(fs.readFileSync(outPath))
        if (ver === 'v1') Y.applyUpdate(doc, bytes)
        else Y.applyUpdateV2(doc, bytes)
      } catch (e) {
        console.error(`FAIL ${entry.name}/${ver}: Yjs could not apply ygo's re-encoding: ${e.message}`)
        failed++
        continue
      }
      for (const c of entry.checks) {
        const got = extract(doc, c)
        if (got !== c.expected) {
          console.error(`FAIL ${entry.name}/${ver} [${c.kind} ${c.root}]:\n  expected ${c.expected}\n  got      ${got}`)
          failed++
        }
      }
    }
  }
  if (failed > 0) {
    console.error(`${failed} round-trip mismatch(es)`)
    process.exit(1)
  }
  console.log(`verify: all ${manifest.length} scenarios round-tripped`)
  process.exit(0)
}

console.error(`unknown mode ${mode}`)
process.exit(2)
