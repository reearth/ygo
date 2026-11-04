// Generate deleted-item integration expectations from the installed Yjs.
const fs = require('fs')
const path = require('path')
const Y = require('yjs')
const encoding = require('lib0/encoding')
const hex = bytes => Buffer.from(bytes).toString('hex')
const fixtures = []
for (const shape of ['rooted', 'origin', 'right_origin']) {
  for (const length of [1000, 2 ** 32 + 17]) {
    for (const gc of [false, true]) {
      const doc = new Y.Doc({ gc })
      doc.clientID = 99
      const base = new Y.Doc({ gc: false })
      base.clientID = 10
      if (shape !== 'rooted') base.getText('deleted-items').insert(0, 'x')
      const prior = Y.encodeStateAsUpdate(base)
      Y.applyUpdate(doc, prior)
      const e = encoding.createEncoder()
      for (const n of [1, 1, 123456789, 0]) encoding.writeVarUint(e, n)
      encoding.writeUint8(e, shape === 'rooted' ? 1 : shape === 'origin' ? 0x81 : 0x41)
      if (shape === 'rooted') {
        encoding.writeVarUint(e, 1)
        encoding.writeVarString(e, 'deleted-items')
      } else {
        encoding.writeVarUint(e, 10)
        encoding.writeVarUint(e, 0)
      }
      encoding.writeVarUint(e, length)
      encoding.writeVarUint(e, 0)
      const input = encoding.toUint8Array(e)
      let event
      doc.on('update', update => { event = update })
      Y.applyUpdate(doc, input)
      fixtures.push({
        name: `${shape}/${length}/gc=${gc}`, gc, length,
        prior: hex(prior), input: hex(input), inputV2: hex(Y.convertUpdateFormatV1ToV2(input)),
        state: hex(Y.encodeStateAsUpdate(doc)), stateV2: hex(Y.encodeStateAsUpdateV2(doc)),
        event: hex(event), snapshot: hex(Y.encodeSnapshot(Y.snapshot(doc))),
        text: doc.getText('deleted-items').toString()
      })
      doc.destroy()
      base.destroy()
    }
  }
}
const output = process.argv[2] || path.join(__dirname, '../crdt/testdata/deleted_yjs_fixtures.json')
fs.writeFileSync(output, JSON.stringify(fixtures, null, 2) + '\n')
console.log(`Generated ${fixtures.length} deleted-item fixtures at ${output}`)
