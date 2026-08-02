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
    'two_cells_pushed_in_sequence',
    'Two notebook cells pushed into the same array — the second push must anchor after the first cell, not at the list head.',
    (doc) => {
      const cells = doc.getArray('cells')
      doc.transact(() => {
        for (const text of ['first cell', 'second cell']) {
          const cell = new Y.Map()
          const src = new Y.Text()
          src.insert(0, text)
          cell.set('source', src)
          cells.push([cell])
        }
      })
    }
  ),
  authored(
    'pushtype_after_plain_values',
    'A nested map pushed after plain values — PushType anchors after the ContentAny item.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        root.push(['a'])
        const m = new Y.Map()
        m.set('k', 'v')
        root.push([m])
      })
    }
  ),
  authored(
    'pushtype_after_tombstone',
    'A nested map pushed after a deleted trailing element — PushType anchors past the tombstone, the trailing-tombstone rule from #70.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        root.push(['a'])
        root.delete(0, 1)
        const m = new Y.Map()
        m.set('k', 'v')
        root.push([m])
      })
    }
  ),
  // The shapes below distinguish STAGED prelim content from replayed calls.
  // Yjs stages detached content (_prelimContent) and materialises the net
  // result once at integrate, so repeated calls collapse rather than each
  // emitting their own item.
  authored(
    'map_key_set_twice',
    'A detached key set twice — only the surviving value reaches the wire.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const m = new Y.Map()
        m.set('k', 'v1')
        m.set('k', 'v2')
        root.push([m])
      })
    }
  ),
  authored(
    'map_set_then_delete',
    'A detached key set then deleted before attach — nothing reaches the wire for it.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const m = new Y.Map()
        m.set('a', '1')
        m.set('keep', '2')
        m.delete('a')
        root.push([m])
      })
    }
  ),
  authored(
    'array_two_pushes_coalesce',
    'Two pushes onto a detached array — Yjs emits one item carrying both values.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const inner = new Y.Array()
        inner.push(['one'])
        inner.push(['two'])
        root.push([inner])
      })
    }
  ),
  authored(
    'array_detached_delete',
    'Push then delete on a detached array — the deleted value never materialises, so no tombstone.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const inner = new Y.Array()
        inner.push(['a', 'b', 'c'])
        inner.delete(1, 1)
        root.push([inner])
      })
    }
  ),
  authored(
    'array_values_interleaved_with_type',
    'Plain values either side of a nested type — the runs coalesce around it.',
    (doc) => {
      const root = doc.getArray('root')
      doc.transact(() => {
        const inner = new Y.Array()
        inner.push(['a', 'b'])
        const child = new Y.Map()
        child.set('k', 'v')
        inner.push([child])
        inner.push(['c'])
        root.push([inner])
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
