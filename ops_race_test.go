//go:build race

package muzak

// raceDetector reports whether the tests were built with -race, under which
// sync.Pool drops a share of what it is given on purpose, so a count of
// allocations on a path that pools is no longer exact.
const raceDetector = true
