package fuzz

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
)

// yrsOracleDir returns the absolute path to testutil/yrs-oracle, resolved
// relative to this source file so it doesn't depend on the test binary's
// working directory.
func yrsOracleDir() (dir string, ok bool) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	dir = filepath.Join(filepath.Dir(filepath.Dir(self)), "yrs-oracle") // .../testutil/fuzz -> .../testutil/yrs-oracle
	if _, err := os.Stat(filepath.Join(dir, "Cargo.toml")); err != nil {
		return "", false
	}
	return dir, true
}

// yrsBinaryPath builds the yrs-oracle crate in release mode and resolves the
// resulting binary's absolute path via `cargo metadata`. It never assumes
// the target directory lives under the crate (it can be redirected, e.g. to
// a shared ~/.cargo/shared-target on this machine).
func yrsBinaryPath(manifest string) (string, error) {
	build := exec.Command("cargo", "build", "--release", "--manifest-path", manifest)
	var buildErr bytes.Buffer
	build.Stdout = &buildErr
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		return "", fmt.Errorf("cargo build: %w (output: %s)", err, buildErr.String())
	}

	meta := exec.Command("cargo", "metadata", "--format-version", "1", "--manifest-path", manifest)
	metaOut, err := meta.Output()
	if err != nil {
		return "", fmt.Errorf("cargo metadata: %w", err)
	}
	var parsed struct {
		TargetDirectory string `json:"target_directory"`
	}
	if err := json.Unmarshal(metaOut, &parsed); err != nil {
		return "", fmt.Errorf("decode cargo metadata: %w", err)
	}
	if parsed.TargetDirectory == "" {
		return "", errors.New("cargo metadata: empty target_directory")
	}
	return filepath.Join(parsed.TargetDirectory, "release", "yrs-oracle"), nil
}

// RunYrs replays every scenario against real yrs (the Rust port of Yjs) by
// piping one NDJSON line per scenario into a single persistent yrs-oracle
// worker process and reading one NDJSON reply per scenario back — the same
// transport discipline as RunNode. It returns ok=false — signalling the
// caller should skip — when cargo is not on PATH, the crate is missing, or
// the release build fails, UNLESS YGO_REQUIRE_YRS=1 is set, in which case a
// build failure is surfaced as a per-scenario Err (ok=true) so CI fails
// loudly instead of silently skipping. It returns one NodeResult per
// scenario in order.
func RunYrs(scenarios []Scenario) ([]NodeResult, bool) {
	requireYrs := os.Getenv("YGO_REQUIRE_YRS") == "1"

	fail := func(err error) ([]NodeResult, bool) {
		rs := make([]NodeResult, len(scenarios))
		for i := range rs {
			rs[i].Err = err
		}
		return rs, true
	}

	if _, err := exec.LookPath("cargo"); err != nil {
		if requireYrs {
			return fail(fmt.Errorf("cargo not found on PATH: %w", err))
		}
		return nil, false
	}
	dir, ok := yrsOracleDir()
	if !ok {
		if requireYrs {
			return fail(errors.New("testutil/yrs-oracle crate not found"))
		}
		return nil, false
	}
	manifest := filepath.Join(dir, "Cargo.toml")

	binPath, err := yrsBinaryPath(manifest)
	if err != nil {
		if requireYrs {
			return fail(err)
		}
		return nil, false
	}

	cmd := exec.Command(binPath)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fail(fmt.Errorf("yrs-oracle worker StdinPipe: %w", err))
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(fmt.Errorf("yrs-oracle worker StdoutPipe: %w", err))
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("yrs-oracle worker start: %w", err))
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
		results[i].Err = errors.New("yrs-oracle worker produced no output for this scenario")
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
				results[i].Err = fmt.Errorf("%w (yrs-oracle stderr: %s)", results[i].Err, stderr.String())
			}
		}
	}
	return results, true
}

// CrossImplEqualYrs replays s with the Go interpreter and asserts ygo agrees
// with yrs (nr) LOGICALLY on root "a" ONLY — no byte round-trip, because
// yrs's move wire format differs from ygo's, so decoding yrs bytes in ygo
// would fail even when both sides logically agree.
func CrossImplEqualYrs(s Scenario, nr NodeResult) error {
	if nr.Err != nil {
		return nr.Err
	}
	peers, err := RunGo(s)
	if err != nil {
		return fmt.Errorf("RunGo: %w", err)
	}
	if len(nr.PeerJSON) != len(peers) {
		return fmt.Errorf("peer count mismatch: go=%d yrs=%d", len(peers), len(nr.PeerJSON))
	}
	arrayRoot := []struct {
		name string
		kind TypeKind
	}{{"a", KindArray}}
	for i, p := range peers {
		goState, err := stateJSON(p, arrayRoot)
		if err != nil {
			return fmt.Errorf("go stateJSON peer %d: %w", i, err)
		}
		jsArr := map[string]any{"a": nr.PeerJSON[i]["a"]}
		if g, j := normalize(goState), normalize(jsArr); !reflect.DeepEqual(g, j) {
			return fmt.Errorf("yrs logical mismatch at peer %d (array):\n  go=%v\n  yrs=%v", i, g, j)
		}
	}
	return nil
}
