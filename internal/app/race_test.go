//go:build race

package app

// raceEnabled shrinks the slowest data-volume tests: the race detector makes
// the pure-Go SQLite many times slower.
const raceEnabled = true
