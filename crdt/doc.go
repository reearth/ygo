// Package crdt implements the Yjs CRDT algorithm in pure Go.
//
// The central concept is the Item: a node in a per-type doubly-linked list
// that carries content and origin pointers enabling conflict-free merging (YATA).
//
// Start with Doc, which is the root of a collaborative document:
//
//	doc := crdt.New()
//	doc.Transact(func(txn *crdt.Transaction) {
//	    doc.GetText("content").Insert(txn, 0, "Hello")
//	})
//	update := doc.EncodeStateAsUpdate()
//
// Reference algorithm: https://github.com/yjs/yjs/blob/main/INTERNALS.md
package crdt
