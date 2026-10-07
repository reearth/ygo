//go:build !race

package crdt

const raceEnabled = false

// raceSeeds scales a fuzz test's default seed count down under the race
// detector, which runs these loops several times slower; env overrides still
// set the exact count.
func raceSeeds(n int) int { return n }
