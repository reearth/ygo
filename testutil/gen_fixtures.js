#!/usr/bin/env node
/**
 * Generates binary golden-file fixtures from the Yjs reference implementation.
 *
 * Usage:
 *   npm install yjs
 *   node testutil/gen_fixtures.js
 *
 * Output: testutil/fixtures/*.bin
 * These files are committed and loaded by TestCompat_* tests in Go.
 */

const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const outDir = path.join(__dirname, 'fixtures')
fs.mkdirSync(outDir, { recursive: true })

function save(name, doc) {
  const update = Y.encodeStateAsUpdate(doc)
  fs.writeFileSync(path.join(outDir, name + '.bin'), update)
  console.log(`wrote ${name}.bin  (${update.byteLength} bytes)`)
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

// Two-client concurrent insert merge
{
  const doc1 = new Y.Doc({ clientID: 1 })
  const doc2 = new Y.Doc({ clientID: 2 })
  doc1.getText('t').insert(0, 'Alice')
  doc2.getText('t').insert(0, 'Bob')
  Y.applyUpdate(doc1, Y.encodeStateAsUpdate(doc2))
  Y.applyUpdate(doc2, Y.encodeStateAsUpdate(doc1))
  // both converge; save doc1's state
  save('merge_concurrent_text', doc1)
}

console.log('Done.')
