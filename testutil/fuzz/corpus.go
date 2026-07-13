package fuzz

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// LoadCorpus reads every *.json scenario in dir (sorted by filename) into a
// slice of Scenarios. Each corpus file is a frozen reproducer for a bug that
// once diverged; the fuzz suite replays them as fast, node-free regressions.
func LoadCorpus(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	var out []Scenario
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}
