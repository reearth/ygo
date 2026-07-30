#!/usr/bin/env node
/**
 * Generates cross-language conformance fixtures for PRELIM construction of the
 * core types (Y.Map / Y.Text / Y.Array), from the Yjs reference implementation
 * (yjs@13.6.x).
 *
 * Usage:
 *   npm install            (in testutil/)
 *   node testutil/gen_fixtures_prelim.js
 *
 * Output: crdt/testdata/prelim_yjs_fixtures.json
 * Loaded by crdt/prelim_yjs_conformance_test.go.
 *
 * Fixture kind — "author", the same shape gen_fixtures_yxml.js uses:
 *   a scripted build sequence executed by Yjs with a PINNED clientID. The Go
 *   test replays the identical sequence with the same clientID and must produce
 *   byte-identical V1 bytes.
 *
 * Every sequence builds its subtree DETACHED (bottom-up, prelim content) and
 * attaches it last. Yjs only materialises items at attach time, top-down, so the
 * container's clock precedes its children's. That is the wire-ordering corner
 * this fixture exists to pin for Y.Map/Y.Text — gen_fixtures_yxml.js already
 * pins it for YXml.
 *
 * The "notebook cell" case is the motivating downstream shape: a Y.Map holding a
 * Y.Text, which is how jupyter_ydoc represents one notebook cell.
 */
const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const CLIENT_ID = 3735928559 // 0xDEADBEEF, pinned so Go can reproduce it exactly

const toHex = (u8) => Buffer.from(u8).toString('hex')

function authored(name, description, build) {
  const doc = new Y.Doc()
  doc.clientID = CLIENT_ID
  build(doc)
  return {
    name,
    description,
    clientID: CLIENT_ID,
    updateV1: toHex(Y.encodeStateAsUpdate(doc)),
    expectedJSON: JSON.stringify(doc.toJSON()),
  }
}

const fixtures = [
  authored(
    'map_with_text_child',
    'Y.Map holding a Y.Text, built detached and attached last (a notebook cell).',
    (doc) => {
      const cells = doc.getArray('cells')
      doc.transact(() => {
        const cell = new Y.Map()
        const src = new Y.Text()
        src.insert(0, 'coach note')
        cell.set('cell_type', 'markdown')
        cell.set('source', src)
        cell.set('metadata', new Y.Map())
        cells.push([cell])
      })
    }
  ),
  authored(
    'text_child_only',
    'A bare Y.Text pushed into an array, built detached.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const t = new Y.Text()
        t.insert(0, 'hello')
        root.push([t])
      })
    }
  ),
  authored(
    'array_nested_in_map',
    'Y.Map holding a Y.Array of scalars, built detached.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const outer = new Y.Map()
        const inner = new Y.Array()
        inner.push(['one', 'two'])
        outer.set('outputs', inner)
        root.push([outer])
      })
    }
  ),
]

const out = path.join(__dirname, '..', 'crdt', 'testdata', 'prelim_yjs_fixtures.json')
fs.mkdirSync(path.dirname(out), { recursive: true })
fs.writeFileSync(out, JSON.stringify({ fixtures }, null, 2) + '\n')
console.log(`wrote ${fixtures.length} fixtures to ${path.relative(process.cwd(), out)}`)
