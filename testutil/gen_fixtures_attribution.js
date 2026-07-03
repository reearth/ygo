#!/usr/bin/env node
/**
 * Generates attribution fixtures from the pinned yjs v14 rc ("yjs14" alias,
 * exact pin yjs@14.0.0-16 -- see package.json / package-lock.json).
 *
 * Run: node gen_fixtures_attribution.js   (from testutil/)
 * Writes fixtures/attribution_fixtures.json and, if go_fixtures/attribution/
 * exists (written by the Go test with YGO_WRITE_GO_FIXTURES=1), verifies yjs
 * can decode Go-encoded bytes (the Go->JS direction, generator-time only).
 *
 * API-ADAPTATION NOTES (checked against
 * node_modules/yjs14/src/utils/{IdSet,IdMap,AttributionManager}.js and the
 * package's public export list in src/index.js -- there is no meta.js /
 * ContentIds / ContentMap / ContentAttribute anywhere in yjs 14.0.0-0 through
 * 14.0.0-16, the full published rc range as of 2026-07-03):
 *
 *  - Y.createContentAttribute       -> does not exist. The real primitive is
 *    Y.createAttributionItem(name, val) (class AttributionItem, aliased on
 *    the public surface as `Attribution`).
 *  - Y.encodeIdSet / Y.decodeIdSet  -> NOT exported publicly (only
 *    `readIdSet` is, and it needs a decoder instance; there's no idset
 *    encoder wrapper at all). We reimplement thin encode/decode wrappers
 *    here using the internal `writeIdSet`/`readIdSet` + `IdSetEncoderV2`/
 *    `DSDecoderV2`, imported from 'yjs14/internals' (a real subpath export
 *    per yjs's package.json `exports` map). This exercises the exact same
 *    writeIdSet/readIdSet code paths Y.encodeIdMap/decodeIdMap use.
 *  - Y.createContentIds / Y.encodeContentIds / Y.createContentMapFromContentIds
 *    / Y.encodeContentMap / Y.decodeContentMap -> none of these exist in any
 *    published yjs14 rc. ygo's ContentIDs/ContentMap types (crdt/contentmap.go)
 *    are a pair-of-{IDSet,IDMap} abstraction the Go port defined itself, wire
 *    encoded as inserts-then-deletes concatenation of the already-verified
 *    writeIdSet/writeIdMap format. There is nothing further to pin against on
 *    the JS side beyond IdSet/IdMap encoding, so this generator constructs
 *    the "contentmap_stamp" fixture by hand from IdSet/IdMap primitives that
 *    DO exist, in the same shape ygo's ContentIDs/ContentMap serialize to
 *    (writeIdSet(inserts) then writeIdSet(deletes); writeIdMap(inserts) then
 *    writeIdMap(deletes)). See the "encodeContentIds"/"encodeContentMap"
 *    local helpers below.
 *  - Y.createInsertSetFromStructStore -> exported under a different name:
 *    `createInsertionSetFromStructStore` (note "Insertion", not "Insert").
 *  - IdSet.add(client, clock, len) / IdMap.add(client, clock, len, attrs) do
 *    exist as instance methods exactly as the brief assumed.
 */
import * as Y from 'yjs14'
import { writeIdSet, readIdSet, IdSetEncoderV2, DSDecoderV2 } from 'yjs14/internals'
import * as decoding from 'lib0/decoding'
import * as fs from 'fs'

const b64 = (u8) => Buffer.from(u8).toString('base64')

// -- local encode/decode helpers for IdSet (not exported publicly) ----------
const encodeIdSet = (idset) => {
  const encoder = new IdSetEncoderV2()
  writeIdSet(encoder, idset)
  return encoder.toUint8Array()
}
const decodeIdSet = (data) => readIdSet(new DSDecoderV2(decoding.createDecoder(data)))

// -- ContentIds / ContentMap: ygo-defined pairing, not a yjs14 concept.
// Encoded as inserts-then-deletes concatenation of IdSet/IdMap encodings,
// matching crdt/attribution_codec.go's EncodeContentIDs/EncodeContentMap.
const encodeContentIds = (c) => Buffer.concat([Buffer.from(encodeIdSet(c.inserts)), Buffer.from(encodeIdSet(c.deletes))])
const encodeContentMap = (c) => Buffer.concat([Buffer.from(Y.encodeIdMap(c.inserts)), Buffer.from(Y.encodeIdMap(c.deletes))])

const fixtures = {}

// 1. idset_basic: multi-client, multi-range (canonical: writeIdSet sorts
//    clients DESCENDING internally, so JS construction order doesn't matter
//    for byte output -- constructed in a natural ascending order here).
{
  const s = Y.createIdSet()
  s.add(1, 0, 5); s.add(1, 10, 8); s.add(7, 3, 1); s.add(42, 100, 1000)
  fixtures.idset_basic = { bytes: b64(encodeIdSet(s)) }
}

// 2. idmap_basic: dedup across ranges + shared names + int/bool/string values.
//    yjs interns AttributionItem instances by content-hash (attrsH map keyed
//    by hash()), so passing fresh Y.createAttributionItem(...) calls with the
//    same (name, val) pair dedups exactly like reusing one instance would --
//    matching ygo's IDMap interning (crdt/idmap.go attrKey/interned map).
{
  const m = Y.createIdMap()
  const alice = Y.createAttributionItem('user', 'alice')
  const ts = Y.createAttributionItem('ts', 1000)
  m.add(3, 0, 4, [alice, ts])
  m.add(3, 10, 2, [alice, ts])
  m.add(1, 5, 5, [Y.createAttributionItem('user', 'bob')])
  m.add(1, 20, 1, [Y.createAttributionItem('reviewed', true)])
  fixtures.idmap_basic = { bytes: b64(Y.encodeIdMap(m)) }
}

// 3. idmap_overlap: the overlap-split/join semantics (gap #3 acceptance).
{
  const m = Y.createIdMap()
  const a = Y.createAttributionItem('u', 'a')
  const b = Y.createAttributionItem('u', 'b')
  m.add(1, 0, 10, [a])   // then overlap:
  m.add(1, 5, 10, [b])   // [5,10) gets both
  m.add(1, 20, 5, [a])   // same-range different attrs:
  m.add(1, 20, 5, [b])
  fixtures.idmap_overlap = { bytes: b64(Y.encodeIdMap(m)) }
}

// 4. contentmap_stamp: the y/hub write path -- update -> contentIds -> stamp.
//    Includes GC'd + deleted content (gap #6).
{
  const doc = new Y.Doc()
  doc.getText('t').insert(0, 'hello world')
  doc.getText('t').delete(0, 6)
  const update = Y.encodeStateAsUpdate(doc)
  const contentIds = {
    inserts: Y.createInsertionSetFromStructStore(doc.store, false),
    deletes: Y.createDeleteSetFromStructStore(doc.store)
  }
  const cm = {
    inserts: Y.createIdMapFromIdSet(contentIds.inserts, [Y.createAttributionItem('userid', 'alice'), Y.createAttributionItem('ts', 1000)]),
    deletes: Y.createIdMapFromIdSet(contentIds.deletes, [Y.createAttributionItem('userid', 'alice'), Y.createAttributionItem('ts', 1000)])
  }
  fixtures.contentmap_stamp = {
    update: b64(update),
    contentIdsBytes: b64(encodeContentIds(contentIds)),
    bytes: b64(encodeContentMap(cm))
  }
}

// 5. empty containers.
{
  fixtures.idset_empty = { bytes: b64(encodeIdSet(Y.createIdSet())) }
  fixtures.idmap_empty = { bytes: b64(Y.encodeIdMap(Y.createIdMap())) }
}

fs.mkdirSync('fixtures', { recursive: true })
fs.writeFileSync('fixtures/attribution_fixtures.json', JSON.stringify(fixtures, null, 2))
console.log('wrote fixtures/attribution_fixtures.json')

// Go->JS verification (generator-time): decode any Go-emitted fixture bytes.
const goDir = 'go_fixtures/attribution'
if (fs.existsSync(goDir)) {
  for (const f of fs.readdirSync(goDir)) {
    const raw = new Uint8Array(fs.readFileSync(`${goDir}/${f}`))
    if (f.startsWith('idset')) decodeIdSet(raw)
    else if (f.startsWith('idmap')) Y.decodeIdMap(raw)
    else if (f.startsWith('contentmap')) {
      // ContentMap = IdMap(inserts) ++ IdMap(deletes); decode both halves in
      // sequence from one decoder to mirror crdt.DecodeContentMap.
      const dec = new DSDecoderV2(decoding.createDecoder(raw))
      Y.readIdMap(dec)
      Y.readIdMap(dec)
    }
    console.log(`yjs decoded ${f} OK`)
  }
}
