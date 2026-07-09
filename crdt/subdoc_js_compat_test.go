package crdt

import (
	"encoding/base64"
	"testing"
)

// subdocFixtureB64 was captured by running:
//
//	node testutil/gen_fixtures_subdoc.js
//
// against yjs@13.6.30 (see testutil/package.json). The generator builds:
//
//	const parent = new Y.Doc()
//	const sub = new Y.Doc({ guid: 'child-1', autoLoad: true })
//	parent.getMap('root').set('a', sub)
//
// and prints Buffer.from(Y.encodeStateAsUpdate(parent)).toString('base64').
//
// This is a genuine yjs-authored update, not hand-fabricated: it proves ygo
// decodes a real ContentDoc's guid + opts (autoLoad -> shouldLoad) correctly.
const subdocFixtureB64 = "AQHp1/3cAQApAQRyb290AWEHY2hpbGQtMXYBCGF1dG9Mb2FkeAA="

func TestSubdocs_DecodesYjsFixture(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(subdocFixtureB64)
	if err != nil {
		t.Fatal(err)
	}
	d := New()
	if err := ApplyUpdateV1(d, raw, nil); err != nil {
		t.Fatalf("apply yjs fixture: %v", err)
	}
	subs := d.GetSubdocs()
	if len(subs) != 1 || subs[0].GUID() != "child-1" || !subs[0].AutoLoad() || !subs[0].ShouldLoad() {
		t.Fatalf("yjs subdoc mismatch: %+v", subs)
	}
}
