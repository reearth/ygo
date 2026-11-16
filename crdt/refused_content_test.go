package crdt

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/reearth/ygo/encoding"
)

// Tests for WithRefusedContent, the kinds of content a remote update may not
// carry.

// kindUpdate is an update of one struct for client 9 under the root name,
// carrying the content tag writes. It is hand-encoded because the kinds this
// guards are ones no writer in this package needs to produce: a browser sends
// them.
func kindUpdate(root string, tag byte, writes func(enc *encoding.Encoder)) []byte {
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(1) // one struct
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(tag)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString(root)
	writes(enc)
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// embedUpdate is an update of one embed under the root, the shape a browser
// sends for a node its editor allows and another owner's schema has none of.
func embedUpdate(root string) []byte {
	return kindUpdate(root, wireEmbed, func(enc *encoding.Encoder) {
		enc.WriteVarString(`{"image":"x.png"}`) // V1 writes an embed as JSON text
	})
}

// binaryUpdate is an update of one binary struct under the root.
func binaryUpdate(root string) []byte {
	return kindUpdate(root, wireBinary, func(enc *encoding.Encoder) {
		enc.WriteVarBytes([]byte{1, 2, 3, 4})
	})
}

// stringUpdate is an update of one string struct under the root: a kind the
// answers below do not name.
func stringUpdate(root string) []byte {
	return kindUpdate(root, wireString, func(enc *encoding.Encoder) {
		enc.WriteVarString("plain")
	})
}

// stringThenEmbedUpdate is an update of two structs for client 9 under the
// root name: the string "plain", and an embed written after it, whose left
// origin is the string's last clock.
func stringThenEmbedUpdate(root string) []byte {
	const plain = "plain"
	enc := encoding.NewEncoder()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(2) // two structs
	enc.WriteVarUint(9) // client
	enc.WriteVarUint(0) // first clock
	enc.WriteUint8(wireString)
	enc.WriteUint8(1) // parent info: a named root follows
	enc.WriteVarString(root)
	enc.WriteVarString(plain)
	enc.WriteUint8(wireEmbed | flagHasOrigin)
	enc.WriteVarUint(9)                      // the left origin's client
	enc.WriteVarUint(uint64(len(plain) - 1)) // its clock: the string's last
	enc.WriteVarString(`{"image":"x.png"}`)
	enc.WriteVarUint(0) // delete set: no clients
	return bytes.Clone(enc.Bytes())
}

// contentDoc is a document holding base as the text of root "t", with
// whole-or-nothing applies or without them.
func contentDoc(t *testing.T, base string, wholeOrNothing bool) *Doc {
	t.Helper()
	doc := New(WithClientID(7), WithWholeOrNothingApplies(wholeOrNothing))
	if err := ApplyUpdateV1(doc, baseState(t, base), nil); err != nil {
		t.Fatalf("the base state was refused: %v", err)
	}
	return doc
}

// requireContentRefused requires err to be ErrContentNotAllowed in a
// RefusedUpdateError, and doc to read as before had it and to have told
// none of its update observers, counted in updates.
func requireContentRefused(t *testing.T, doc *Doc, before docState, updates *int, err error) {
	t.Helper()
	var refused *RefusedUpdateError
	if !errors.Is(err, ErrContentNotAllowed) || !errors.Is(err, ErrInvalidUpdate) || !errors.As(err, &refused) {
		t.Fatalf("the update was applied with err %v, want ErrContentNotAllowed in a RefusedUpdateError", err)
	}
	if after := stateOf(doc); after != before {
		t.Fatalf("the refused update changed the document:\nbefore %+v\nafter  %+v", before, after)
	}
	if *updates != 0 {
		t.Fatalf("the refused update was handed to %d observers", *updates)
	}
}

// A document refuses a remote update carrying a kind of content its owner
// names, and the refusal leaves the document as it was whether or not it has
// whole-or-nothing applies: each struct's kind is read as the update is
// decoded, and every struct decodes before one integrates, so nothing of a
// refused update has entered the document. That holds for a refused struct
// behind one the answer allows, and for the V2 form of each update. A kind
// the answer does not name applies, and with no answer every update here
// adds its structs.
func TestUnit_RefusedContent_RefusesTheKindsTheOwnerNames(t *testing.T) {
	const base = "hello world"
	noEmbedOrBinary := func(any) []ContentKind {
		return []ContentKind{ContentKindEmbed, ContentKindBinary}
	}
	updates := map[string]struct {
		update  []byte
		structs int
	}{
		"an embed":                     {embedUpdate("t"), 1},
		"binary":                       {binaryUpdate("t"), 1},
		"an embed behind a string":     {stringThenEmbedUpdate("t"), 2},
		"a string, which is not named": {stringUpdate("t"), 1},
	}

	for name, u := range updates {
		v2, err := UpdateV1ToV2(u.update)
		if err != nil {
			t.Fatal(err)
		}
		for _, wholeOrNothing := range []bool{false, true} {
			for form, update := range map[string][]byte{"V1": u.update, "V2": v2} {
				t.Run(name+"/"+form+"/"+map[bool]string{false: "keeps what it integrates", true: "whole or nothing"}[wholeOrNothing], func(t *testing.T) {
					apply := func(doc *Doc) error {
						if form == "V2" {
							return ApplyUpdateV2(doc, update, nil)
						}
						return ApplyUpdateV1(doc, update, nil)
					}

					doc := contentDoc(t, base, wholeOrNothing)
					doc.SetRefusedContent(noEmbedOrBinary)
					before := stateOf(doc)
					seen := observed(doc)
					err := apply(doc)
					if name == "a string, which is not named" {
						requireApplied(t, doc, before, err)
					} else {
						requireContentRefused(t, doc, before, seen, err)
					}

					// The control: with no answer the document adds every
					// struct of the update, which is what the refusal kept out.
					open := contentDoc(t, base, wholeOrNothing)
					openBefore := stateOf(open)
					requireApplied(t, open, openBefore, apply(open))
					if got, want := storeStructs(open), openBefore.structs+u.structs; got != want {
						t.Fatalf("the document with no answer holds %d structs, want %d", got, want)
					}
				})
			}
		}
	}
}

// A document with no refused kinds takes every kind, exactly as one without
// the option does: a nil callback, a nil answer and an empty one are each
// that.
func TestUnit_RefusedContent_ADocumentWithNoKindsTakesEveryKind(t *testing.T) {
	const base = "hello world"
	for name, kinds := range map[string]func(any) []ContentKind{
		"a nil callback":  nil,
		"a nil answer":    func(any) []ContentKind { return nil },
		"an empty answer": func(any) []ContentKind { return []ContentKind{} },
	} {
		t.Run(name, func(t *testing.T) {
			for what, update := range map[string][]byte{
				"an embed": embedUpdate("t"),
				"binary":   binaryUpdate("t"),
			} {
				doc := contentDoc(t, base, false)
				doc.SetRefusedContent(kinds)
				if err := ApplyUpdateV1(doc, update, nil); err != nil {
					t.Fatalf("%s refused %s: %v", name, what, err)
				}

				open := contentDoc(t, base, false)
				if err := ApplyUpdateV1(open, update, nil); err != nil {
					t.Fatalf("the document without the option refused %s: %v", what, err)
				}
				if got, want := stateOf(doc), stateOf(open); got != want {
					t.Fatalf("%s left\n%+v\nand a document without the option left\n%+v", name, got, want)
				}
			}
		})
	}
}

// The option refuses no subdocument by itself: an update that sets a plain
// key and a subdocument applies whole under an answer naming another kind,
// and only an answer naming ContentKindDoc refuses it, leaving the document
// as it was.
func TestUnit_RefusedContent_RefusesASubdocumentOnlyWhenTheAnswerNamesIt(t *testing.T) {
	peer := New(WithClientID(2))
	m := peer.GetMap("m")
	peer.Transact(func(txn *Transaction) {
		m.Set(txn, "k", "v")
		m.Set(txn, "sub", New(WithGUID("sub")))
	})
	update := EncodeStateAsUpdateV1(peer, nil)

	doc := New(WithRefusedContent(func(any) []ContentKind { return []ContentKind{ContentKindEmbed} }))
	if err := ApplyUpdateV1(doc, update, nil); err != nil {
		t.Fatalf("the update was refused: %v", err)
	}
	got := doc.GetMap("m")
	if v, _ := got.Get("k"); v != "v" {
		t.Fatalf("the key holds %v, want \"v\"", v)
	}
	if sub, _ := got.Get("sub"); sub == nil {
		t.Fatal("the subdocument is missing")
	} else if _, isDoc := sub.(*Doc); !isDoc {
		t.Fatalf("the subdocument's key holds %T, want *Doc", sub)
	}

	refusing := New(WithRefusedContent(func(any) []ContentKind { return []ContentKind{ContentKindDoc} }))
	before := stateOf(refusing)
	requireContentRefused(t, refusing, before, observed(refusing), ApplyUpdateV1(refusing, update, nil))
}

// A callback that panics refuses the update before anything of it is read,
// with an invalid update naming the callback and its panic, and leaves the
// document as it was; once the callback is removed the same update applies.
func TestUnit_RefusedContent_APanickingCallbackRefusesTheUpdate(t *testing.T) {
	const base = "hello world"
	doc := contentDoc(t, base, false)
	doc.SetRefusedContent(func(any) []ContentKind { panic("boom in the kinds") })
	before := stateOf(doc)
	err := ApplyUpdateV1(doc, stringUpdate("t"), nil)
	var refused *RefusedUpdateError
	if !errors.Is(err, ErrInvalidUpdate) || !errors.As(err, &refused) {
		t.Fatalf("the update was applied with err %v, want ErrInvalidUpdate in a RefusedUpdateError", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "WithRefusedContent callback") || !strings.Contains(msg, "boom in the kinds") {
		t.Fatalf("the error does not name the callback and its panic: %v", err)
	}
	if after := stateOf(doc); after != before {
		t.Fatalf("the refused update changed the document:\nbefore %+v\nafter  %+v", before, after)
	}

	doc.SetRefusedContent(nil)
	requireApplied(t, doc, before, ApplyUpdateV1(doc, stringUpdate("t"), nil))
}
