//go:build race

package ai

// raceEnabled is whether the race detector, which slows code many times
// over, is on: wall-clock bounds do not hold under it.
const raceEnabled = true
