#!/usr/bin/env node
/**
 * Generates cross-language conformance fixtures for Y.UndoManager restoring
 * DELETED NESTED TYPES, from the Yjs reference implementation (yjs@13.6.x).
 *
 * Usage:
 *   npm install            (in testutil/)
 *   node testutil/gen_fixtures_undo.js
 *
 * Output: crdt/testdata/undo_yjs_fixtures.json
 * Loaded by crdt/undo_yjs_conformance_test.go.
 *
 * Fixture kind — "author" (see gen_fixtures_prelim.js): a scripted sequence of
 * edits and undo/redo calls executed by Yjs with a PINNED clientID. The Go test
 * replays the identical sequence and must produce byte-identical V1 bytes, so
 * the redone container AND its re-inserted children land at the same clocks
 * with the same origins as in Yjs.
 */
const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const CLIENT_ID = 3735928559 // 0xDEADBEEF, pinned so Go can reproduce it exactly

const toHex = (u8) => Buffer.from(u8).toString('hex')

function authored(name, description, root, kind, build) {
  const doc = new Y.Doc()
  doc.clientID = CLIENT_ID
  const type = kind === 'map' ? doc.getMap(root) : doc.getArray(root)
  const um = new Y.UndoManager(type)
  build(doc, type, um)
  return {
    name,
    description,
    clientID: CLIENT_ID,
    root,
    kind,
    updateV1: toHex(Y.encodeStateAsUpdate(doc)),
    expectedJSON: JSON.stringify(type.toJSON()),
  }
}

const setText = (m) => {
  const t = new Y.Text()
  t.insert(0, 'Hello')
  m.set('t', t)
}

const fixtures = [
  authored(
    'map_key_text_undo_delete',
    'Undo the delete of a map key holding a Y.Text restores the text content.',
    'm', 'map',
    (doc, m, um) => {
      setText(m)
      um.stopCapturing()
      m.delete('t')
      um.undo()
    }
  ),
  authored(
    'array_nested_map_children_undo_delete',
    'Undo the delete of an array element holding a Y.Map restores its entries, including a nested Y.Text.',
    'a', 'array',
    (doc, arr, um) => {
      doc.transact(() => {
        arr.push(['x'])
        const inner = new Y.Map()
        inner.set('k', 'v')
        const src = new Y.Text()
        src.insert(0, 'deep')
        inner.set('src', src)
        arr.push([inner])
        arr.push(['y'])
      })
      um.stopCapturing()
      arr.delete(1, 1)
      um.undo()
    }
  ),
  authored(
    'nested_in_nested_undo_delete',
    'Undo the delete of a map that holds a map that holds a Y.Text and a Y.Array.',
    'a', 'array',
    (doc, arr, um) => {
      doc.transact(() => {
        const outer = new Y.Map()
        const inner = new Y.Map()
        const txt = new Y.Text()
        txt.insert(0, 'leaf')
        inner.set('text', txt)
        const list = new Y.Array()
        list.push(['one', 'two'])
        inner.set('list', list)
        outer.set('inner', inner)
        outer.set('tag', 'o')
        arr.push([outer])
      })
      um.stopCapturing()
      arr.delete(0, 1)
      um.undo()
    }
  ),
  authored(
    'map_key_text_undo_redo_undo',
    'Undo, redo, then undo again the delete of a map key holding a Y.Text.',
    'm', 'map',
    (doc, m, um) => {
      setText(m)
      um.stopCapturing()
      m.delete('t')
      um.undo()
      um.redo()
      um.undo()
    }
  ),
  authored(
    'map_key_text_full_unwind',
    'After undo/redo/undo of the delete, undoing the original set removes the twice-restored text.',
    'm', 'map',
    (doc, m, um) => {
      setText(m)
      um.stopCapturing()
      m.delete('t')
      um.undo()
      um.redo()
      um.undo()
      um.undo()
    }
  ),
  authored(
    'nested_edit_merged_undo',
    'A nested map edit merged between two captured pushes is undone with them, restoring the overwritten value.',
    'a', 'array',
    (doc, arr, um) => {
      const m = new Y.Map()
      m.set('k', 1)
      arr.push([m])
      um.stopCapturing()
      arr.push([5])
      m.set('k', 2)
      arr.push([6])
      um.undo()
    }
  ),
  authored(
    'fresh_doc_merged_pushes_undo',
    'Two merged pushes on a fresh doc are both undone.',
    'a', 'array',
    (doc, arr, um) => {
      arr.push(['x'])
      arr.insert(0, ['y'])
      um.undo()
    }
  ),
  authored(
    'insert_then_delete_merged_undo',
    'An element inserted and deleted within one capture interval stays deleted on undo.',
    'a', 'array',
    (doc, arr, um) => {
      arr.push(['base'])
      um.stopCapturing()
      arr.insert(0, ['x'])
      arr.delete(0, 1)
      um.undo()
    }
  ),
  authored(
    'edit_after_undo_starts_new_item',
    'An edit right after an undo is not merged into the next-older stack item (undo stops capturing).',
    'm', 'map',
    (doc, m, um) => {
      m.set('a', 1)
      um.stopCapturing()
      m.set('b', 2)
      um.undo()
      m.set('c', 3)
      um.undo()
    }
  ),
]

const out = path.join(__dirname, '..', 'crdt', 'testdata', 'undo_yjs_fixtures.json')
fs.mkdirSync(path.dirname(out), { recursive: true })
fs.writeFileSync(out, JSON.stringify({ fixtures }, null, 2) + '\n')
console.log(`wrote ${fixtures.length} fixtures to ${path.relative(process.cwd(), out)}`)
