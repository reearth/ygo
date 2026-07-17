package mobile

import (
	"os"
	"os/exec"
	"testing"
)

// requireNodeYjs skips the test when Node or the yjs package is unavailable,
// UNLESS YGO_REQUIRE_NODE=1 (CI), in which case it hard-fails so a
// misconfigured CI can never silently skip cross-language checks. Returns the
// resolved node path and the testutil dir (which holds node_modules/yjs).
func requireNodeYjs(t *testing.T) (nodePath, testutilDir string) {
	t.Helper()
	require := os.Getenv("YGO_REQUIRE_NODE") == "1"
	node, err := exec.LookPath("node")
	if err != nil {
		if require {
			t.Fatal("YGO_REQUIRE_NODE=1 but `node` not found on PATH")
		}
		t.Skip("node not found; skipping yjs interop (set YGO_REQUIRE_NODE=1 to require)")
	}
	dir := "../testutil"
	cmd := exec.Command(node, "-e", "require('yjs')")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		if require {
			t.Fatalf("YGO_REQUIRE_NODE=1 but `require('yjs')` failed in %s: %v", dir, err)
		}
		t.Skip("yjs not installed in testutil/node_modules; skipping (run `npm ci` in testutil/)")
	}
	return node, dir
}

func TestRequireNodeYjs_Probe(t *testing.T) {
	node, dir := requireNodeYjs(t)
	if node == "" || dir == "" {
		t.Fatal("requireNodeYjs returned empty paths without skipping")
	}
}
