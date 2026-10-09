package otlp

import (
	"net/http"
	"testing"
	"time"
)

// FuzzRetryAfter checks the Retry-After a collector, which may be anyone the
// endpoint names, can send: no input panics, none yields a negative wait, and
// whatever it yields is held to the cap once the exporter uses it.
func FuzzRetryAfter(f *testing.F) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, seed := range []string{
		"", "0", "5", " 120 ", "99999999999999999999999", "9223372037", "-1", "1.5", "soon",
		now.Add(time.Minute).Format(http.TimeFormat), "Wed, 21 Oct 2015 07:28:00 GMT",
	} {
		f.Add(seed)
	}
	e, err := New(Options{Endpoint: "http://c", RetryMaxInterval: time.Second, RetryInitialInterval: time.Millisecond})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, header string) {
		wait, ok := parseRetryAfter(header, now)
		if wait < 0 || !ok && wait != 0 {
			t.Fatalf("parseRetryAfter(%q) = %v, %v", header, wait, ok)
		}
		capped := e.retryWait(3, attempt{retryAfter: wait, hasRetryAfter: ok})
		if capped < 0 || capped > time.Second {
			t.Fatalf("a Retry-After of %q waits %v, past the cap", header, capped)
		}
	})
}

// FuzzPartialSuccess checks the body of a collector's successful answer: no
// input panics, the rejected count is never negative, and the message quoted
// in the log stays within its bound.
func FuzzPartialSuccess(f *testing.F) {
	for _, seed := range []string{
		``, `{}`, `not json`, `{"partialSuccess":{}}`,
		`{"partialSuccess":{"rejectedSpans":"2","errorMessage":"two"}}`,
		`{"partialSuccess":{"rejectedSpans":2}}`,
		`{"partialSuccess":{"rejectedSpans":"-1"}}`,
		`{"partialSuccess":{"rejectedSpans":"9223372036854775808"}}`,
		`{"partialSuccess":{"rejectedSpans":[1]}}`,
		`{"partialSuccess":{"errorMessage":"` + string(make([]byte, 10)) + `"}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		rejected, message := parsePartialSuccess(body)
		if rejected < 0 || len(message) > maxLoggedResponse {
			t.Fatalf("parsePartialSuccess(%q) = %d, %d bytes of message", body, rejected, len(message))
		}
	})
}
