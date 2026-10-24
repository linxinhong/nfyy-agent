//go:build race

package main_test

// raceEnabled marks race-instrumented test builds. The concurrency gate
// skips its timing assertions here: the race detector's overhead skews
// latency far past any budget. Race cleanliness is still asserted (any
// detected race fails the run).
const raceEnabled = true
