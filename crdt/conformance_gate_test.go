package crdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDecideConformance(t *testing.T) {
	cases := []struct {
		name                      string
		nodeOK, depOK, requireEnv bool
		wantSkip, wantFatal       bool
	}{
		{"all present", true, true, false, false, false},
		{"all present + require", true, true, true, false, false},
		{"node missing, no require", false, true, false, true, false},
		{"node missing + require", false, true, true, false, true},
		{"dep missing, no require", true, false, false, true, false},
		{"dep missing + require", true, false, true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skip, fatal := decideConformance(c.nodeOK, c.depOK, c.requireEnv)
			if skip != c.wantSkip || fatal != c.wantFatal {
				t.Fatalf("decideConformance(%v,%v,%v) = (skip=%v,fatal=%v), want (skip=%v,fatal=%v)",
					c.nodeOK, c.depOK, c.requireEnv, skip, fatal, c.wantSkip, c.wantFatal)
			}
		})
	}
}

// decideConformance is the skip-vs-fail policy for cross-impl tests. When the
// environment can't run them (Node or the needed npm package missing), CI
// (YGO_REQUIRE_NODE=1) fails so coverage can never silently vanish; locally it
// skips so contributors without Node aren't blocked.
func decideConformance(nodeOK, depOK, requireEnv bool) (skip, fatal bool) {
	if nodeOK && depOK {
		return false, false
	}
	if requireEnv {
		return false, true
	}
	return true, false
}

// requireConformance gates a cross-impl test on Node + the given npm package
// (pkg is "yjs" for stable, "yjs14" for the prerelease alias). It probes with a
// real `node -e "require(pkg)"` load, run from testutil/ so require resolves
// node_modules there. On a missing environment it t.Skip()s, unless
// YGO_REQUIRE_NODE=1, in which case it t.Fatal()s.
func requireConformance(t *testing.T, pkg string) (nodePath, testutilDir string) {
	t.Helper()
	requireEnv := os.Getenv("YGO_REQUIRE_NODE") == "1"
	node, lookErr := exec.LookPath("node")
	testutilDir, _ = filepath.Abs(filepath.Join("..", "testutil"))
	nodeOK := lookErr == nil
	depOK := false
	if nodeOK {
		probe := exec.Command(node, "-e", "require('"+pkg+"')")
		probe.Dir = testutilDir
		depOK = probe.Run() == nil
	}
	skip, fatal := decideConformance(nodeOK, depOK, requireEnv)
	if fatal {
		t.Fatalf("YGO_REQUIRE_NODE=1 but cross-impl env unavailable (node=%v, %s loadable=%v)", nodeOK, pkg, depOK)
	}
	if skip {
		t.Skipf("node/%s unavailable — skipping cross-impl test", pkg)
	}
	return node, testutilDir
}
