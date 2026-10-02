package crdt

import (
	"os"
	"os/exec"
	"runtime/debug"
	"testing"
)

const deepJSONTestDepth = 20_000

// TestUnit_ContentType_ToJSON_DeepRemoteNestingIsStackSafe proves that reading
// a remote CRDT tree cannot overflow the process stack while recursively
// unwrapping nested shared types.
func TestUnit_ContentType_ToJSON_DeepRemoteNestingIsStackSafe(t *testing.T) {
	if os.Getenv("YGO_DEEP_JSON_HELPER") == "1" {
		debug.SetMaxStack(256 * 1024)
		applyAndEncodeDeepMap()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestUnit_ContentType_ToJSON_DeepRemoteNestingIsStackSafe$")
	cmd.Env = append(os.Environ(), "YGO_DEEP_JSON_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ToJSON on a %d-level remote nested map failed: %v\n%s", deepJSONTestDepth, err, output)
	}
}

func applyAndEncodeDeepMap() {
	source := newTestDoc(1)
	root := source.GetMap("root")
	current := root
	source.Transact(func(txn *Transaction) {
		for range deepJSONTestDepth {
			child := NewMapPrelim()
			current.Set(txn, "c", child)
			current = child
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
}
