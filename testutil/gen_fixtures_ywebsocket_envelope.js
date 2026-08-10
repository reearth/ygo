#!/usr/bin/env node
/**
 * Generates y-websocket outer-envelope conformance fixtures from the Yjs
 * reference implementation (#165 Task 2).
 *
 * Usage:
 *   npm install            (in testutil/)
 *   node testutil/gen_fixtures_ywebsocket_envelope.js
 *
 * Output: provider/client/testdata/ywebsocket_envelope_fixtures.json
 * Loaded by provider/client/wire_test.go.
 *
 * WHY y-protocols + lib0/encoding INSTEAD OF importing y-websocket itself:
 * y-websocket's own module (src/y-websocket.js) is written for a browser/
 * WebSocket transport and pulls in ws/lib0 observable plumbing that doesn't
 * run cleanly headless from a plain `node script.js`. But the *envelope*
 * y-websocket puts on the wire is not something y-websocket invents — it is
 * exactly this, verbatim, from y-websocket/src/utils.js:
 *
 *   const encoder = encoding.createEncoder()
 *   encoding.writeVarUint(encoder, messageSync)       // or messageAwareness=1
 *   syncProtocol.writeSyncStep1(encoder, doc)         // sync payload, RAW
 *   // ...
 *   encoding.writeVarUint(encoder, messageAwareness)
 *   encoding.writeVarUint8Array(encoder, awarenessProtocol.encodeAwarenessUpdate(...)) // VarBytes-wrapped
 *
 * i.e. `writeVarUint(outerTag)` followed directly by whatever
 * `y-protocols/{sync,awareness}` already produces (sync raw, awareness
 * length-prefixed) — outerTag and payload shape are the ONLY things
 * y-websocket's utils.js contributes; the payload bytes themselves come
 * from y-protocols. Building that same two lines directly with
 * `lib0/encoding` + `y-protocols` reproduces y-websocket's wire bytes
 * exactly, without needing y-websocket's transport code at all. This keeps
 * the fixture generator dependency-light (no ws, no jsdom) while still
 * pinning against the real upstream protocol implementation, not our own
 * understanding of it.
 *
 * queryAwareness (outer tag 3) carries NO payload at all in y-websocket —
 * see provider/websocket/peer.go's `case msgQueryAwareness` handler, which
 * reads no further bytes after the outer tag.
 *
 * encodeEnvelope/decodeEnvelope (provider/client/wire.go) only handle the
 * OUTER layer: VarUint(msgType) followed by the payload appended RAW, with
 * no further structure imposed. That is why the awareness fixture's
 * `innerHex` is itself already VarBytes-wrapped (VarUint(len) + raw
 * awareness bytes) — matching peer.go's `sendAwareness`, which calls
 * `enc.WriteVarBytes(awMsg)` for the *whole* payload after the outer tag.
 * decodeEnvelope has no opinion on what's inside; unwrapping the awareness
 * VarBytes layer is the sync-protocol layer's job, not this codec's.
 */
const Y = require('yjs')
const syncProtocol = require('y-protocols/sync')
const awarenessProtocol = require('y-protocols/awareness')
const encoding = require('lib0/encoding')
const fs = require('fs')
const path = require('path')

// Outer message type tags, matching y-websocket/src/utils.js and
// provider/websocket/server.go's msgSync/msgAwareness/msgAuth/msgQueryAwareness.
const messageSync = 0
const messageAwareness = 1
const messageQueryAwareness = 3

const hex = (u8) => Buffer.from(u8).toString('hex')

// envelope wraps innerBytes (already the exact bytes y-protocols/lib0 wrote
// for the payload, RAW or VarBytes as the caller chooses) behind an outer
// VarUint(msgType), and records both layers so the Go test can assert
// decodeEnvelope splits them apart identically.
function envelope(name, direction, msgType, innerBytes) {
  const enc = encoding.createEncoder()
  encoding.writeVarUint(enc, msgType)
  encoding.writeUint8Array(enc, innerBytes)
  return {
    name,
    direction,
    msgType,
    innerHex: hex(innerBytes),
    hex: hex(encoding.toUint8Array(enc)),
  }
}

// ---- step1: client asks a peer for its state vector ----
const docA = new Y.Doc()
docA.clientID = 1
docA.getText('t').insert(0, 'hello')

const step1Enc = encoding.createEncoder()
syncProtocol.writeSyncStep1(step1Enc, docA)
const step1Inner = encoding.toUint8Array(step1Enc)
const fxStep1 = envelope('sync_step1', 'client->server', messageSync, step1Inner)

// ---- step2: server replies with the missing state relative to the state
// vector docA sent, from a second doc with divergent content (client ID 2)
// so the payload is non-trivial. ----
const docB = new Y.Doc()
docB.clientID = 2
docB.getText('t').insert(0, 'hello')
docB.getArray('a').push(['x', 'y'])

const step2Enc = encoding.createEncoder()
syncProtocol.writeSyncStep2(step2Enc, docB, Y.encodeStateVector(docA))
const step2Inner = encoding.toUint8Array(step2Enc)
const fxStep2 = envelope('sync_step2', 'server->client', messageSync, step2Inner)

// ---- update: an incremental update broadcast after the initial sync ----
const docC = new Y.Doc()
docC.clientID = 3
const beforeUpdate = Y.encodeStateVector(docC)
docC.getArray('a').push([1, 2, 3])
const incrementalUpdate = Y.encodeStateAsUpdate(docC, beforeUpdate)

const updateEnc = encoding.createEncoder()
syncProtocol.writeUpdate(updateEnc, incrementalUpdate)
const updateInner = encoding.toUint8Array(updateEnc)
const fxUpdate = envelope('sync_update', 'client->server', messageSync, updateInner)

// ---- awareness: a client publishes its local presence state. The outer
// envelope's payload for awareness is the VarBytes-wrapped update (see the
// module comment above), so wrap it here before handing it to envelope(),
// which always appends its payload argument raw. ----
const docD = new Y.Doc()
docD.clientID = 42
const aw = new awarenessProtocol.Awareness(docD)
aw.setLocalState({ user: { name: 'ana', color: '#ff0000' } })
const awarenessRaw = awarenessProtocol.encodeAwarenessUpdate(aw, [docD.clientID])
const awarenessWrapEnc = encoding.createEncoder()
encoding.writeVarUint8Array(awarenessWrapEnc, awarenessRaw)
const awarenessInner = encoding.toUint8Array(awarenessWrapEnc)
// Awareness starts a setInterval outdated-timeout check (dist/awareness.cjs)
// that would otherwise keep the node process alive after the script's work
// is done; destroy() clears it so this generator exits on its own.
aw.destroy()
const fxAwareness = envelope('awareness_update', 'client->server', messageAwareness, awarenessInner)

// ---- queryAwareness: no payload at all, just the outer tag ----
const fxQueryAwareness = envelope('query_awareness', 'client->server', messageQueryAwareness, new Uint8Array(0))

const fixtures = [fxStep1, fxStep2, fxUpdate, fxAwareness, fxQueryAwareness]

const out = path.join(__dirname, '..', 'provider', 'client', 'testdata', 'ywebsocket_envelope_fixtures.json')
fs.mkdirSync(path.dirname(out), { recursive: true })
fs.writeFileSync(out, JSON.stringify({ fixtures }, null, 2) + '\n')
console.log(`wrote ${fixtures.length} fixtures to ${path.relative(process.cwd(), out)}`)
