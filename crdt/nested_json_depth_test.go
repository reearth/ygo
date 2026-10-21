package crdt

import (
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
)

const deepJSONTestDepth = 20_000

// TestUnit_ContentType_ToJSON_DeepRemoteNestingIsStackSafe proves that reading
// a remote CRDT tree (ToJSON, Entries, ToSlice) cannot overflow the process
// stack while unwrapping nested shared types.
func TestUnit_ContentType_ToJSON_DeepRemoteNestingIsStackSafe(t *testing.T) {
	if os.Getenv("YGO_DEEP_JSON_HELPER") == "1" {
		debug.SetMaxStack(256 * 1024)
		applyAndReadDeepTree()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestUnit_ContentType_ToJSON_DeepRemoteNestingIsStackSafe$")
	cmd.Env = append(os.Environ(), "YGO_DEEP_JSON_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("reading a %d-level remote nested tree failed: %v\n%s", deepJSONTestDepth, err, output)
	}
}

// applyAndReadDeepTree builds a chain alternating YMap and YArray levels, so
// every reader descends through both container kinds.
func applyAndReadDeepTree() {
	source := newTestDoc(1)
	root := source.GetMap("root")
	source.Transact(func(txn *Transaction) {
		current := root
		for i := 0; i < deepJSONTestDepth; i += 2 {
			list := NewArrayPrelim()
			current.Set(txn, "c", list)
			current = NewMapPrelim()
			list.PushType(txn, current)
		}
	})

	target := newTestDoc(2)
	result := target.GetMap("root")
	if err := ApplyUpdateV1(target, EncodeStateAsUpdateV1(source, nil), nil); err != nil {
		panic(err)
	}
	encoded, err := result.ToJSON()
	if err != nil {
		panic(err)
	}
	if len(encoded) == 0 {
		panic("empty JSON result")
	}
	if len(result.Entries()) != 1 {
		panic("Entries lost the chain")
	}
	list, _ := result.Get("c")
	if len(list.(*YArray).ToSlice()) != 1 {
		panic("ToSlice lost the chain")
	}
}
