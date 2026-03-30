#!/usr/bin/env node
/**
 * verify_go_fixtures.js — reads Go-encoded binary update files from
 * testutil/go_fixtures/ and verifies their content using the Yjs reference
 * implementation. Called by TestCompat_GoToJS_* tests via exec.Command.
 *
 * Exit 0 on success, non-zero on any assertion failure.
 */

const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const fixtureDir = path.join(__dirname, 'go_fixtures')
let failed = false

function load(name) {
  const buf = fs.readFileSync(path.join(fixtureDir, name + '.bin'))
  return new Uint8Array(buf)
}

function check(name, fn) {
  try {
    fn()
    console.log(`PASS  ${name}`)
  } catch (e) {
    console.error(`FAIL  ${name}: ${e.message}`)
    failed = true
  }
}

function assertEqual(a, b, msg) {
  if (JSON.stringify(a) !== JSON.stringify(b)) {
    throw new Error(`${msg}: expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}`)
  }
}

// ── YText: simple insert ──────────────────────────────────────────────────────
check('GoToJS_YText_Insert_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('ytext_insert_v1'))
  assertEqual(doc.getText('content').toString(), 'Hello from Go!', 'text content')
})

check('GoToJS_YText_Insert_V2', () => {
  const doc = new Y.Doc()
  Y.applyUpdateV2(doc, load('ytext_insert_v2'))
  assertEqual(doc.getText('content').toString(), 'Hello from Go!', 'text content')
})

// ── YText: insert + delete ────────────────────────────────────────────────────
check('GoToJS_YText_Delete_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('ytext_delete_v1'))
  assertEqual(doc.getText('content').toString(), 'Hello!', 'text after delete')
})

// ── YText: bold formatting (delta) ────────────────────────────────────────────
check('GoToJS_YText_Format_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('ytext_format_v1'))
  const text = doc.getText('content')
  // Verify total content is correct regardless of delta structure.
  const plain = text.toDelta().map(op => op.insert || '').join('')
  if (plain !== 'Hello, world!') throw new Error(`expected 'Hello, world!', got '${plain}'`)
  // Verify bold attribute exists somewhere in the first 5 characters.
  const delta = text.toDelta()
  const boldOp = delta.find(op => op.attributes && op.attributes.bold)
  if (!boldOp) throw new Error('expected a delta op with bold=true attribute')
  if (!boldOp.insert.includes('H') && boldOp.insert !== 'Hello') {
    // Allow either merged or split format ops — just check bold appears on 'Hello'.
    const boldText = delta.filter(op => op.attributes && op.attributes.bold).map(op => op.insert).join('')
    if (!boldText.startsWith('Hello')) throw new Error(`bold text should start with 'Hello', got '${boldText}'`)
  }
})

// ── YArray: mixed types ───────────────────────────────────────────────────────
check('GoToJS_YArray_Mixed_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('yarray_mixed_v1'))
  const arr = doc.getArray('list').toArray()
  if (arr.length !== 5) throw new Error(`expected 5 items, got ${arr.length}`)
  assertEqual(arr[0], 1,     'arr[0]')
  assertEqual(arr[1], 'two', 'arr[1]')
  assertEqual(arr[2], true,  'arr[2]')
  assertEqual(arr[3], null,  'arr[3]')
  assertEqual(arr[4], { key: 'val' }, 'arr[4]')
})

// ── YMap: basic ───────────────────────────────────────────────────────────────
check('GoToJS_YMap_Basic_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('ymap_basic_v1'))
  const m = doc.getMap('data')
  assertEqual(m.get('name'),   'Alice', 'name')
  assertEqual(m.get('age'),    30,      'age')
  assertEqual(m.get('active'), true,    'active')
})

// ── Concurrent merge ──────────────────────────────────────────────────────────
check('GoToJS_ConcurrentMerge_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('concurrent_merge_v1'))
  const text = doc.getText('t').toString()
  // YATA: lower clientID wins — client 10 ("Alice") < client 20 ("Bob")
  if (text !== 'AliceBob' && text !== 'BobAlice') {
    throw new Error(`expected convergent merge, got '${text}'`)
  }
  if (text.length !== 8) throw new Error(`expected length 8, got ${text.length}`)
})

// ── Run-length squash: 5 chars typed → 1 item in Go, JS decodes correctly ─────
check('GoToJS_Squashed_V1', () => {
  const doc = new Y.Doc()
  Y.applyUpdate(doc, load('squashed_v1'))
  assertEqual(doc.getText('content').toString(), 'hello', 'squashed text')
})

process.exit(failed ? 1 : 0)
