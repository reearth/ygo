package fuzz

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"

	"github.com/reearth/ygo/crdt"
)

// NodeResult is the per-scenario reply from the Node/Yjs oracle worker
// (testutil/fuzz_oracle.js). PeerJSON[i] is peer i's logical view across the
// well-known roots ("t"/"a"/"m"/"x"); PeerUpdateB64[i] is peer i's full
// since-genesis state as a base64-encoded V1 update. Err is set (non-JSON)
// when the worker failed to produce a result for this scenario.
type NodeResult struct {
	PeerJSON      []map[string]any `json:"peerJSON"`
	PeerUpdateB64 []string         `json:"peerUpdateB64"`
	Err           error            `json:"-"`
}

// oracleScriptDir returns the directory holding fuzz_oracle.js (testutil/) and
// the absolute path to the script. The directory is used as the worker's cwd
// so that require('yjs') resolves testutil/node_modules regardless of the test
// binary's own working directory.
func oracleScriptDir() (dir, script string, ok bool) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", "", false
	}
	dir = filepath.Dir(filepath.Dir(self)) // .../testutil/fuzz -> .../testutil
	script = filepath.Join(dir, "fuzz_oracle.js")
	if _, err := os.Stat(script); err != nil {
		return "", "", false
	}
	return dir, script, true
}

// RunNode replays every scenario against real Yjs by piping one NDJSON line per
// scenario into a single persistent node worker and reading one NDJSON reply
// per scenario back. It returns ok=false — signalling the caller should skip —
// ONLY when the environment is genuinely absent: node is not on PATH, the
// worker script is missing, or the yjs dependency is not installed. Once those
// checks pass, node+yjs are present, so any later worker setup/spawn or runtime
// failure is returned with ok=true and a non-nil per-scenario Err, which the
// caller surfaces as a test failure (not a skip) — otherwise a broken oracle
// would pass silently in CI. It returns one NodeResult per scenario in order.
func RunNode(scenarios []Scenario) ([]NodeResult, bool) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		return nil, false
	}
	dir, script, ok := oracleScriptDir()
	if !ok {
		return nil, false
	}
	// Probe that yjs resolves from the worker's cwd; a missing dependency is a
	// skip, not a failure, so headless environments without it stay green.
	probe := exec.Command(nodePath, "-e", "require('yjs')")
	probe.Dir = dir
	if err := probe.Run(); err != nil {
		return nil, false
	}

	// Past this point node AND yjs are confirmed present, so a worker
	// setup/spawn failure is a genuine error — not an "environment absent" skip.
	// Surface it through per-scenario Err (with ok=true) so the caller reports a
	// failure instead of silently skipping (which would hide a broken oracle in
	// CI). fail returns one errored result per scenario.
	fail := func(err error) ([]NodeResult, bool) {
		rs := make([]NodeResult, len(scenarios))
		for i := range rs {
			rs[i].Err = err
		}
		return rs, true
	}

	cmd := exec.Command(nodePath, script)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(fmt.Errorf("node worker StdinPipe: %w", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(fmt.Errorf("node worker StdoutPipe: %w", err))
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("node worker start: %w", err))
	}

	// Feed scenarios from a goroutine so a full OS pipe buffer can't deadlock
	// us against the worker's output we haven't read yet.
	go func() {
		w := bufio.NewWriter(stdin)
		for _, s := range scenarios {
			b, mErr := json.Marshal(s)
			if mErr != nil {
				break
			}
			if _, wErr := w.Write(append(b, '\n')); wErr != nil {
				break
			}
		}
		_ = w.Flush()
		_ = stdin.Close()
	}()

	results := make([]NodeResult, len(scenarios))
	for i := range results {
		results[i].Err = errors.New("node worker produced no output for this scenario")
	}
	reader := bufio.NewReader(stdout)
	for i := range scenarios {
		line, rErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var nr NodeResult
			if uErr := json.Unmarshal(line, &nr); uErr == nil {
				results[i] = nr // clears the sentinel Err
			} else {
				results[i].Err = fmt.Errorf("decode worker output: %w", uErr)
			}
		}
		if rErr != nil {
			break // EOF or read error: remaining results keep their sentinel Err
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	// If the worker exited badly, attach its stderr to any still-unfilled
	// results so the caller reports a meaningful failure.
	if waitErr != nil || stderr.Len() > 0 {
		for i := range results {
			if results[i].Err != nil {
				results[i].Err = fmt.Errorf("%w (node stderr: %s)", results[i].Err, stderr.String())
			}
		}
	}
	return results, true
}

// crossImplRoots are the well-known roots the generator drives, in the same
// order used by Converged/stateJSON.
var crossImplRoots = []struct {
	name string
	kind TypeKind
}{{"t", KindText}, {"a", KindArray}, {"m", KindMap}, {"x", KindXmlFragment}}

// CrossImplEqual replays s with the Go interpreter and asserts ygo agrees with
// Yjs (nr) two ways:
//
//  1. Logical: each ygo peer's per-root view (stateJSON) equals the
//     corresponding Yjs peer's view.
//  2. Round-trip: a fresh ygo doc that decodes each Yjs peer's encoded V1
//     update produces the same view Yjs reports for that peer — i.e. ygo can
//     read Yjs's own bytes and see what Yjs sees.
//
// Both comparisons run through normalize so number/collection representations
// are compared by value, not by Go/JS encoding quirks.
func CrossImplEqual(s Scenario, nr NodeResult) error {
	if nr.Err != nil {
		return nr.Err
	}
	peers, err := RunGo(s)
	if err != nil {
		return fmt.Errorf("RunGo: %w", err)
	}
	if len(nr.PeerJSON) != len(peers) {
		return fmt.Errorf("peer count mismatch: go=%d yjs=%d", len(peers), len(nr.PeerJSON))
	}
	if len(nr.PeerUpdateB64) != len(peers) {
		return fmt.Errorf("peer update count mismatch: go=%d yjs=%d", len(peers), len(nr.PeerUpdateB64))
	}

	// (1) logical parity, per peer.
	for i, p := range peers {
		goState, err := stateJSON(p, crossImplRoots)
		if err != nil {
			return fmt.Errorf("go stateJSON peer %d: %w", i, err)
		}
		if g, j := normalize(goState), normalize(nr.PeerJSON[i]); !reflect.DeepEqual(g, j) {
			return fmt.Errorf("logical mismatch at peer %d:\n  go=%v\n  js=%v", i, g, j)
		}
	}

	// (2) round-trip parity: ygo decodes Yjs's bytes, must see what Yjs sees.
	for i, b64 := range nr.PeerUpdateB64 {
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("peer %d update b64 decode: %w", i, err)
		}
		fresh := crdt.New()
		if err := crdt.ApplyUpdateV1(fresh, raw, nil); err != nil {
			return fmt.Errorf("peer %d round-trip apply: %w", i, err)
		}
		rtState, err := stateJSON(&peerState{doc: fresh}, crossImplRoots)
		if err != nil {
			return fmt.Errorf("round-trip stateJSON peer %d: %w", i, err)
		}
		if g, j := normalize(rtState), normalize(nr.PeerJSON[i]); !reflect.DeepEqual(g, j) {
			return fmt.Errorf("round-trip mismatch at peer %d (ygo decode of yjs update):\n  ygo=%v\n  js=%v", i, g, j)
		}
	}
	return nil
}

// normalize canonicalises a decoded-JSON value by round-tripping it through
// encoding/json, so a Go-built map[string]any and a worker-emitted one compare
// equal by value (all numbers become float64, empty vs nil collections align)
// under reflect.DeepEqual.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}
