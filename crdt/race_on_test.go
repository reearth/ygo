//go:build race

package crdt

const raceEnabled = true

// raceSeeds scales a fuzz test's default seed count down under the race
// detector, which runs these loops several times slower; env overrides still
// set the exact count.
func raceSeeds(n int) int { return max(n/5, 1) }
