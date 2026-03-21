// Package types implements the Yjs shared types: YArray, YMap, YText,
// YXmlFragment, YXmlElement, and YXmlText.
//
// All types are backed by a doubly-linked list of Items stored in the
// parent Doc's StructStore. Access them through crdt.Doc:
//
//	text := doc.GetText("name")
//	arr  := doc.GetArray("name")
//	m    := doc.GetMap("name")
package types
