#!/usr/bin/env node
/**
 * reencode_roundtrip.js — the middle hop of a ygo → yjs → ygo round-trip.
 *
 * Reads every `<name>.v1.in` / `<name>.v2.in` file in the directory passed as
 * argv[2] (each an ygo-encoded update), applies it to a fresh Yjs doc, RE-
 * ENCODES with the matching wire version, and writes `<name>.v1.out` /
 * `<name>.v2.out`. The Go side then decodes the `.out` files and asserts the
 * document content survived the full loop — catching anything Yjs normalises
 * or re-splits on re-encode that ygo would then mis-read.
 *
 * Exit 0 on success; non-zero on any apply/encode failure.
 */
const Y = require('yjs')
const fs = require('fs')
const path = require('path')

const dir = process.argv[2]
if (!dir) {
  console.error('usage: reencode_roundtrip.js <dir>')
  process.exit(2)
}

let count = 0
for (const f of fs.readdirSync(dir)) {
  let v2
  if (f.endsWith('.v1.in')) v2 = false
  else if (f.endsWith('.v2.in')) v2 = true
  else continue

  const inBytes = new Uint8Array(fs.readFileSync(path.join(dir, f)))
  const doc = new Y.Doc()
  try {
    if (v2) Y.applyUpdateV2(doc, inBytes)
    else Y.applyUpdate(doc, inBytes)
    const out = v2 ? Y.encodeStateAsUpdateV2(doc) : Y.encodeStateAsUpdate(doc)
    fs.writeFileSync(path.join(dir, f.replace(/\.in$/, '.out')), Buffer.from(out))
    count++
  } catch (e) {
    console.error(`FAIL ${f}: ${e.message}`)
    process.exit(1)
  }
}
console.log(`reencoded ${count} fixtures`)
