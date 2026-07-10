package fuzz

import (
	"encoding/json"

	"github.com/reearth/ygo/crdt"
)

type peerState struct {
	doc   *crdt.Doc
	inbox [][]byte
}

func newPeers(n int) []*peerState {
	ps := make([]*peerState, n)
	for i := range ps {
		ps[i] = &peerState{doc: crdt.New(crdt.WithClientID(crdt.ClientID(i + 1)))}
	}
	return ps
}

func clampIndex(pos, length int, forInsert bool) int {
	if pos < 0 {
		pos = -pos
	}
	m := length
	if forInsert {
		m = length + 1
	}
	if m <= 0 {
		return 0
	}
	return pos % m
}

func decodeScalar(js string) any {
	if js == "" {
		return nil
	}
	var v any
	_ = json.Unmarshal([]byte(js), &v)
	return v
}

func applyLocalOp(p *peerState, st Step) {
	switch st.TypeKind {
	case KindText:
		txt := p.doc.GetText(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpInsert:
				txt.Insert(txn, clampIndex(st.PosHint, txt.Len(), true), st.StrVal, nil)
			case OpDelete:
				if txt.Len() > 0 {
					txt.Delete(txn, clampIndex(st.PosHint, txt.Len(), false), minInt(st.LenHint, txt.Len()))
				}
			}
		})
	case KindArray:
		arr := p.doc.GetArray(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpInsert:
				arr.Insert(txn, clampIndex(st.PosHint, arr.Len(), true), []any{decodeScalar(st.JSONVal)})
			case OpPush:
				arr.Insert(txn, arr.Len(), []any{decodeScalar(st.JSONVal)})
			case OpDelete:
				if arr.Len() > 0 {
					arr.Delete(txn, clampIndex(st.PosHint, arr.Len(), false), minInt(st.LenHint, arr.Len()))
				}
			}
		})
	case KindMap:
		m := p.doc.GetMap(st.Root)
		p.doc.Transact(func(txn *crdt.Transaction) {
			switch st.Op {
			case OpSetKey:
				m.Set(txn, st.Key, decodeScalar(st.JSONVal))
			case OpDelKey:
				m.Delete(txn, st.Key)
			}
		})
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
