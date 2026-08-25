package validate

import (
	"sync"
	"testing"
)

// TestConcurrentChecks exercises the rules that keep state between calls.
//
// A rule set is built per request, but three things behind the rules are not:
// the time zone names remembered as they are seen, the compiled expressions the
// format rules share, and the code tables. All three are read from every
// goroutine an instance is serving on. Run under the race detector, this is
// what proves none of them is written to while being read.
func TestConcurrentChecks(t *testing.T) {
	t.Parallel()

	zones := []string{"UTC", "Europe/Istanbul", "America/New_York", "Asia/Tokyo", "Mars/Olympus"}
	countries := []string{"TR", "GB", "HK", "XQ", "tr"}

	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 200 {
				zone := zones[(worker+i)%len(zones)]
				if got, want := isTimezone(zone), zone != "Mars/Olympus"; got != want {
					t.Errorf("isTimezone(%q) = %v, want %v", zone, got, want)
					return
				}
				country := countries[(worker+i)%len(countries)]
				if got, want := isCountryCode(country), country == "TR" || country == "GB" || country == "HK"; got != want {
					t.Errorf("isCountryCode(%q) = %v, want %v", country, got, want)
					return
				}
				if !isSemver("1.4.0") || isSemver("1.4") {
					t.Error("the shared expressions disagreed with themselves")
					return
				}
				// A whole rule set, built and evaluated the way a request
				// builds one.
				if got := String().Trim().NotBlank().Email().For(ptr(" nope ")).Evaluate(); len(got) != 1 {
					t.Errorf("a rule set gave %v, want one failure", got)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}

// TestTimezoneDataAvailable checks the start-up probe an application uses to
// refuse to run without a zone database.
func TestTimezoneDataAvailable(t *testing.T) {
	t.Parallel()
	if !TimezoneDataAvailable() {
		t.Skip("this host has no time zone database, which is the case the probe exists to report")
	}
	// It must resolve a real zone rather than UTC, which the standard library
	// answers without reading anything and so would report a database that is
	// not there.
	if !isTimezone("America/New_York") {
		t.Error("the probe passed but a real zone did not resolve")
	}
}
