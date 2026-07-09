#!/usr/bin/env node
/**
 * Generates a cross-implementation byte-parity fixture for subdocument opts.
 *
 * Builds a parent Y.Doc with a subdocument (guid: 'child-1', autoLoad: true)
 * embedded in a YMap, then prints the base64-encoded V1 update. Used by
 * crdt/subdoc_js_compat_test.go to prove ygo decodes a real yjs-authored
 * ContentDoc's opts (guid + autoLoad -> shouldLoad) byte-for-byte.
 *
 * Usage (from repo root, so it resolves the installed yjs in testutil/):
 *   node testutil/gen_fixtures_subdoc.js
 */

const Y = require('yjs')

const parent = new Y.Doc()
const sub = new Y.Doc({ guid: 'child-1', autoLoad: true })
parent.getMap('root').set('a', sub)

console.log(Buffer.from(Y.encodeStateAsUpdate(parent)).toString('base64'))
