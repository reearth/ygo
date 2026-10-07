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
 * with the same origins as in Yjs. Fixtures with a second peer are jsonOnly:
 * only the JSON can match, since ygo orders clients, merges structs and
 * encodes collected children differently.
 */
const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const CLIENT_ID = 3735928559 // 0xDEADBEEF, pinned so Go can reproduce it exactly

// A second peer for concurrent-edit fixtures; sorts after CLIENT_ID.
const REMOTE_ID = 4277009102 // 0xFEEDFACE

const toHex = (u8) => Buffer.from(u8).toString('hex')

let twoPeer = false

const remoteDoc = () => {
  twoPeer = true
  const d = new Y.Doc()
  d.clientID = REMOTE_ID
  return d
}

// sync applies from's missing state to `to` under an untracked origin.
const sync = (from, to) =>
  Y.applyUpdate(to, Y.encodeStateAsUpdate(from, Y.encodeStateVector(to)), 'remote')

function authored(name, description, root, kind, build) {
  const doc = new Y.Doc()
  doc.clientID = CLIENT_ID
  const type = kind === 'map' ? doc.getMap(root) : doc.getArray(root)
  const um = new Y.UndoManager(type)
  twoPeer = false
  build(doc, type, um)
  const jsonOnly = twoPeer
  const f = {
    name,
    description,
    clientID: CLIENT_ID,
    root,
    kind,
    updateV1: toHex(Y.encodeStateAsUpdate(doc)),
    expectedJSON: JSON.stringify(type.toJSON()),
  }
  if (jsonOnly) f.jsonOnly = true
  return f
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
  authored(
    'merged_run_undo_delete_then_remote_insert',
    'Two pushes Yjs holds as one struct are restored as one item, so a concurrent insert lands after both.',
    'a', 'array',
    (doc, arr, um) => {
      const r = remoteDoc()
      arr.push(['a'])
      arr.push(['b'])
      um.stopCapturing()
      arr.delete(0, 2)
      sync(doc, r)
      um.undo()
      r.getArray('a').insert(0, ['x'])
      sync(r, doc)
    }
  ),
  authored(
    'merged_run_redo_then_remote_insert',
    'Redoing two merged pushes re-inserts them as one item ahead of a concurrent insert.',
    'a', 'array',
    (doc, arr, um) => {
      const r = remoteDoc()
      arr.push(['a'])
      arr.push(['b'])
      um.undo()
      sync(doc, r)
      um.redo()
      r.getArray('a').insert(0, ['x'])
      sync(r, doc)
    }
  ),
  authored(
    'map_restore_follows_delete_order',
    'Restored values sharing a key are replayed in first-delete order, so the last-deleted client wins.',
    'm', 'map',
    (doc, m, um) => {
      const r = remoteDoc()
      const rm = r.getMap('m')
      rm.set('k', 5)
      rm.set('j', 1)
      sync(r, doc)
      m.delete('k')
      um.undo()
      doc.transact(() => {
        m.delete('j')
        m.set('k', 18)
      })
      rm.set('k', 19)
      sync(r, doc)
      m.set('k', 20)
      um.undo()
    }
  ),
  authored(
    'nested_key_restored_over_collected_remote_set',
    'A remote set inside a map deleted here reaches us collected; undoing the delete restores the old value.',
    'a', 'array',
    (doc, arr, um) => {
      const r = remoteDoc()
      const ra = r.getArray('a')
      const inner = new Y.Map()
      inner.set('k', 6)
      ra.insert(0, [inner])
      sync(r, doc)
      arr.delete(0, 1)
      ra.get(0).set('k', 9)
      sync(doc, r)
      sync(r, doc)
      um.undo()
    }
  ),
  authored(
    'collected_overwritten_remote_value_blocks_restore',
    'A remote value collected because a later remote value overwrote it still blocks undo restoring an older value under its key.',
    'm', 'map',
    (doc, m, um) => {
      const r = remoteDoc()
      const rm = r.getMap('m')
      const inner = new Y.Map()
      inner.set('k', 8)
      rm.set('k0', inner)
      sync(r, doc)
      m.get('k0').set('k', 20)
      um.stopCapturing()
      rm.get('k0').set('k', 23)
      rm.get('k0').set('k', 29)
      sync(r, doc)
      sync(doc, r)
      m.set('k0', 34)
      um.undo()
      um.undo()
    }
  ),
  authored(
    'collected_remote_delete_in_live_map_blocks_restore',
    'A remote set-then-delete of a key in a live map is a remote edit undo must not overwrite, though its value was collected.',
    'm', 'map',
    (doc, m, um) => {
      const r = remoteDoc()
      m.set('k', 1)
      um.stopCapturing()
      m.set('k', 2)
      sync(doc, r)
      r.getMap('m').set('k', 3)
      r.getMap('m').delete('k')
      sync(r, doc)
      um.undo()
    }
  ),
  authored(
    'merged_run_partial_undo_after_restore',
    'Undoing one half of a restored merged run deletes only that half of its copy.',
    'a', 'array',
    (doc, arr, um) => {
      arr.push(['a'])
      um.stopCapturing()
      arr.push(['b'])
      um.stopCapturing()
      arr.delete(0, 2)
      um.undo()
      um.undo()
    }
  ),
  authored(
    'unmerged_neighbours_restore_separately',
    'Adjacent same-client items with different right origins are separate structs in Yjs and are restored separately.',
    'a', 'array',
    (doc, arr, um) => {
      const r = remoteDoc()
      const ra = r.getArray('a')
      arr.push(['x'])
      arr.insert(0, ['a'])
      sync(doc, r)
      ra.insert(1, ['y'])
      sync(r, doc)
      arr.insert(1, ['b'])
      um.stopCapturing()
      arr.delete(0, 2)
      sync(doc, r)
      um.undo()
      ra.insert(0, ['z'])
      sync(r, doc)
    }
  ),
]

const out = path.join(__dirname, '..', 'crdt', 'testdata', 'undo_yjs_fixtures.json')
fs.mkdirSync(path.dirname(out), { recursive: true })
fs.writeFileSync(out, JSON.stringify({ fixtures }, null, 2) + '\n')
console.log(`wrote ${fixtures.length} fixtures to ${path.relative(process.cwd(), out)}`)
