//go:build !race

package muzak

// raceDetector reports whether the tests were built with -race; see
// ops_race_test.go.
const raceDetector = false
