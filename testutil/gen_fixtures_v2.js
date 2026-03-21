#!/usr/bin/env node
/**
 * Generates V2 binary fixtures using Y.encodeStateAsUpdateV2.
 *
 * Usage (from repo root):
 *   node testutil/gen_fixtures_v2.js
 *
 * Output: testutil/fixtures/*_v2.bin
 */

const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const outDir = path.join(__dirname, 'fixtures')
fs.mkdirSync(outDir, { recursive: true })

function save(name, doc) {
  const update = Y.encodeStateAsUpdateV2(doc)
  fs.writeFileSync(path.join(outDir, name + '_v2.bin'), update)
  console.log(`wrote ${name}_v2.bin  (${update.byteLength} bytes)`)
}

// empty doc
{
  const doc = new Y.Doc()
  save('empty_doc', doc)
}

// YText: simple insert
{
  const doc = new Y.Doc({ clientID: 1 })
  doc.getText('content').insert(0, 'Hello, world!')
  save('ytext_hello', doc)
}

// YText: bold formatting
{
  const doc = new Y.Doc({ clientID: 1 })
  const text = doc.getText('content')
  text.insert(0, 'Hello, world!')
  text.format(0, 5, { bold: true })
  save('ytext_bold', doc)
}

// YText: insert then delete
{
  const doc = new Y.Doc({ clientID: 1 })
  const text = doc.getText('content')
  text.insert(0, 'Hello, world!')
  text.delete(5, 7)
  save('ytext_delete', doc)
}

// YArray: mixed types
{
  const doc = new Y.Doc({ clientID: 1 })
  doc.getArray('list').push([1, 'two', true, null, { key: 'val' }])
  save('yarray_mixed', doc)
}

// YMap: basic
{
  const doc = new Y.Doc({ clientID: 1 })
  const m = doc.getMap('data')
  m.set('name', 'Alice')
  m.set('age', 30)
  m.set('active', true)
  save('ymap_basic', doc)
}

// Two-client concurrent insert merge — derive V2 from the same V1 fixture to
// ensure consistent client IDs (Yjs ignores the {clientID} constructor option).
{
  const v1 = fs.readFileSync(path.join(outDir, 'merge_concurrent_text.bin'))
  const doc = new Y.Doc()
  Y.applyUpdate(doc, v1)
  save('merge_concurrent_text', doc)
}

console.log('Done.')
