package mobile

import (
	"encoding/json"
	"strconv"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/crdt"
)

// idiomaticOp is one operation in an idiomatic Yjs delta. A single op carries
// exactly one of insert/retain/delete, plus optional attributes, matching the
// shape a Yjs/Quill consumer expects (e.g. `{"insert":"hi","attributes":{...}}`).
//
// Insert is typed `any` so a text run marshals as a JSON string while an embed
// marshals as its JSON object. omitempty on an interface field is a nil-interface
// check (encoding/json's isEmptyValue tests IsNil for Kind Interface, not the
// underlying value), so it drops Insert only on retain/delete ops (where it is
// left nil) and preserves every genuine insert value — including the empty
// string or 0 — should ToDelta ever emit one. Retain/Delete are *int so a
// zero-count op still serialises the key; ToDelta only ever emits positive
// counts, but this keeps the mapping faithful.
type idiomaticOp struct {
	Insert     any            `json:"insert,omitempty"`
	Retain     *int           `json:"retain,omitempty"`
	Delete     *int           `json:"delete,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// deltaToIdiomaticJSON converts ygo's internal []crdt.Delta into the idiomatic
// Yjs delta JSON shape. A nil or empty slice marshals to `[]` (never `null`), so
// a JS consumer can iterate the result unconditionally.
func deltaToIdiomaticJSON(delta []crdt.Delta) ([]byte, error) {
	ops := make([]idiomaticOp, 0, len(delta))
	for _, d := range delta {
		var op idiomaticOp
		switch d.Op {
		case crdt.DeltaOpInsert:
			op.Insert = normalizeEmbed(d.Insert)
		case crdt.DeltaOpRetain:
			r := d.Retain
			op.Retain = &r
		case crdt.DeltaOpDelete:
			del := d.Delete
			op.Delete = &del
		}
		if len(d.Attributes) > 0 {
			op.Attributes = map[string]any(d.Attributes)
		}
		ops = append(ops, op)
	}
	return json.Marshal(ops)
}

// normalizeEmbed maps an insert payload to the JSON a JS consumer expects.
//
// This is a passthrough. For a text run d.Insert is a string; for an embed it is
// the value that was handed to InsertEmbed (e.g. a map[string]any). ygo encodes
// an embed on the V1 wire as a JSON-text varstring (Yjs writeJSON), so both ygo's
// ToDelta and a Yjs consumer that decoded the same update see the identical JSON
// object — no shape adaptation is needed. The yjs-delta interop test
// (TestGetTextJSON_YjsInterop) locks this in; if a future embed type ever
// diverges, flatten it here.
func normalizeEmbed(v any) any {
	return v
}

// statesToIdiomaticJSON converts awareness.GetStates() into a JSON object keyed
// by stringy client ID whose value is each client's raw state object — dropping
// the internal Clock. GetStates returns active clients only (State non-nil), so
// every value is a present state object. An empty map marshals to `{}`.
func statesToIdiomaticJSON(states map[uint64]awareness.ClientState) ([]byte, error) {
	out := make(map[string]any, len(states))
	for id, cs := range states {
		out[strconv.FormatUint(id, 10)] = cs.State
	}
	return json.Marshal(out)
}
