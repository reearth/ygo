// Package fuzz implements a randomized convergence fuzzer for ygo's CRDT
// types (#70). A Scenario is a seed-reproducible, JSON-serializable script
// replayed identically by the Go interpreter and the Node/Yjs oracle.
package fuzz

// StepKind tags the Step union.
type StepKind string

const (
	StepLocalOp StepKind = "op"
	StepSync    StepKind = "sync"
	StepGC      StepKind = "gc"
)

// TypeKind identifies a shared-type root.
type TypeKind string

const (
	KindText        TypeKind = "text"
	KindArray       TypeKind = "array"
	KindMap         TypeKind = "map"
	KindXmlFragment TypeKind = "xmlfrag"
	KindXmlElement  TypeKind = "xmlelem"
	KindXmlText     TypeKind = "xmltext"
)

// OpCode is a per-type mutation.
type OpCode string

const (
	OpInsert   OpCode = "insert"   // text/array/xmltext
	OpDelete   OpCode = "delete"   // text/array/xml children
	OpFormat   OpCode = "format"   // text
	OpPush     OpCode = "push"     // array
	OpSetKey   OpCode = "setkey"   // map
	OpDelKey   OpCode = "delkey"   // map
	OpSetAttr  OpCode = "setattr"  // xmlelem
	OpDelAttr  OpCode = "delattr"  // xmlelem
	OpAddChild OpCode = "addchild" // xmlfrag/xmlelem: insert element or text child
)

// SyncMethod is the wire path a Sync step drives.
type SyncMethod string

const (
	ApplyV1 SyncMethod = "applyv1"
	ApplyV2 SyncMethod = "applyv2"
	MergeV1 SyncMethod = "mergev1"
	MergeV2 SyncMethod = "mergev2"
	DiffV1  SyncMethod = "diffv1"
	DiffV2  SyncMethod = "diffv2"
)

// Step is one action. Only the fields relevant to Kind are populated; the
// rest stay zero. Flat (not an interface) so it JSON-encodes as one object.
type Step struct {
	Kind StepKind `json:"kind"`

	// StepLocalOp
	Peer     int      `json:"peer,omitempty"`
	Root     string   `json:"root,omitempty"`
	TypeKind TypeKind `json:"typeKind,omitempty"`
	Op       OpCode   `json:"op,omitempty"`
	PosHint  int      `json:"posHint,omitempty"`
	LenHint  int      `json:"lenHint,omitempty"`  // delete length
	StrVal   string   `json:"strVal,omitempty"`   // text/xmltext content
	Key      string   `json:"key,omitempty"`      // map key / attr name
	JSONVal  string   `json:"jsonVal,omitempty"`  // JSON-encoded scalar value for map-set/array-insert
	ChildXml string   `json:"childXml,omitempty"` // "elem:<tag>" or "text" for OpAddChild
	Target   []int    `json:"target,omitempty"`   // path to nested container ([] = root)

	// StepSync
	From   int        `json:"from,omitempty"`
	To     int        `json:"to,omitempty"`
	Method SyncMethod `json:"method,omitempty"`
}

// Scenario is a full fuzz run: a seed, a peer count, and an ordered step list.
type Scenario struct {
	Seed     uint64 `json:"seed"`
	NumPeers int    `json:"numPeers"`
	Steps    []Step `json:"steps"`
}
