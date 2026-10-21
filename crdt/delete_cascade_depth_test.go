package crdt

import (
	"os"
	"os/exec"
	"runtime/debug"
	"strconv"
	"testing"

	"github.com/reearth/ygo/encoding"
)

const (
	deepRemoteDeleteDepth = 20_000
	deepRemoteDeleteBatch = 1_000
)

// TestUnit_Item_Delete_DeepRemoteCascadeIsStackSafe proves that a small
// incoming delete update cannot overflow the process stack after a document has
// accumulated deeply nested shared types through many ordinary transactions.
// The helper process turns Go's unrecoverable stack-overflow fatal error into a
// normal test failure in the parent process.
func TestUnit_Item_Delete_DeepRemoteCascadeIsStackSafe(t *testing.T) {
	if os.Getenv("YGO_DEEP_DELETE_HELPER") == "1" {
		debug.SetMaxStack(deepRemoteDeleteStackLimit())
		applyDeepRemoteDelete()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestUnit_Item_Delete_DeepRemoteCascadeIsStackSafe$")
	cmd.Env = append(os.Environ(), "YGO_DEEP_DELETE_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("incoming root delete over a %d-level nested map chain failed: %v\n%s", deepRemoteDeleteDepth, err, output)
	}
}

func deepRemoteDeleteStackLimit() int {
	if raw := os.Getenv("YGO_DEEP_DELETE_MAX_STACK"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			panic(err)
		}
		return limit
	}
	return 256 * 1024
}

func deepRemoteDeleteShape() (int, int) {
	depth, batch := deepRemoteDeleteDepth, deepRemoteDeleteBatch
	if raw := os.Getenv("YGO_DEEP_DELETE_DEPTH"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			panic(err)
		}
		depth = parsed
	}
	if raw := os.Getenv("YGO_DEEP_DELETE_BATCH"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			panic(err)
		}
		batch = parsed
	}
	if depth < 1 || batch < 1 {
		panic("deep-delete depth and batch must be positive")
	}
	return depth, batch
}

func applyDeepRemoteDelete() {
	source := newTestDoc(1)
	target := newTestDoc(2)
	root := source.GetMap("root")
	current := root
	depth, batch := deepRemoteDeleteShape()
	for offset := 0; offset < depth; offset += batch {
		source.Transact(func(txn *Transaction) {
			for range min(batch, depth-offset) {
				child := NewMapPrelim()
				current.Set(txn, "c", child)
				current = child
			}
		})
		if err := ApplyUpdateV1(target, EncodeStateAsUpdateV1(source, target.StateVector()), nil); err != nil {
			panic(err)
		}
	}
	rootItem := root.itemMap["c"]

	deleteSet := newDeleteSet()
	deleteSet.add(rootItem.ID, 1)
	encoder := encoding.NewEncoder()
	encoder.WriteVarUint(0)
	encodeDeleteSet(encoder, deleteSet)
	if err := ApplyUpdateV1(target, encoder.Bytes(), nil); err != nil {
		panic(err)
	}
}
