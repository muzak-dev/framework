package muzak

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sharedSentinel is the idiom the builders must survive: one package-level
// error that every request decorates with its own details.
var sharedSentinel = NewHTTPError(http.StatusUnprocessableEntity, "invalid")

// TestHTTPErrorBuildersDoNotMutateTheirReceiver is the regression test for a
// shared sentinel that accumulated state. Each builder used to edit the error
// in place, so one request's details reached the next request's response,
// grew without bound, and raced under concurrent use.
func TestHTTPErrorBuildersDoNotMutateTheirReceiver(t *testing.T) {
	t.Parallel()
	base := NewHTTPError(http.StatusConflict, "taken")
	cause := errors.New("driver: duplicate key")

	details := base.WithDetails(ErrorDetail{Field: "name", Location: "body", Issue: "is taken"})
	coded := base.WithCode("name_taken")
	wrapped := base.Wrap(cause)
	keyed := base.WithMessageKey("errors.taken", "name", "x")

	if len(base.Details) != 0 || base.Code != "" || base.cause != nil || base.MessageKey != "" || base.MessageArgs != nil {
		t.Fatalf("a builder edited the receiver: %+v", base)
	}
	if len(details.Details) != 1 || coded.Code != "name_taken" || !errors.Is(wrapped, cause) || keyed.MessageKey != "errors.taken" {
		t.Errorf("a builder dropped what it was given: %+v %+v %+v %+v", details, coded, wrapped, keyed)
	}
	if got := details.WithDetails(ErrorDetail{Field: "b"}); len(got.Details) != 2 || len(details.Details) != 1 {
		t.Errorf("chained WithDetails shared a backing array: %d and %d entries", len(got.Details), len(details.Details))
	}
	// A chain keeps everything set earlier on it.
	chained := base.WithCode("c").Wrap(cause).WithDetails(ErrorDetail{Field: "f"})
	if chained.Code != "c" || !errors.Is(chained, cause) || len(chained.Details) != 1 || chained.Status != http.StatusConflict {
		t.Errorf("a chained call lost earlier state: %+v", chained)
	}
}

// TestSharedHTTPErrorSentinelDoesNotLeakBetweenRequests drives the sentinel
// from many requests at once, under -race, and then checks that the next
// caller sees none of theirs.
func TestSharedHTTPErrorSentinelDoesNotLeakBetweenRequests(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/u/{id}", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, sharedSentinel.WithDetails(ErrorDetail{Field: "user", Location: "path", Issue: "secret-of-" + ctx.PathValue("id")})
	})
	mustBuild(t, app)

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/u/alice", nil))
		}()
	}
	wg.Wait()

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/u/bob", nil))
	if strings.Contains(rec.Body.String(), "secret-of-alice") {
		t.Errorf("bob's response contains alice's detail (%d bytes)", rec.Body.Len())
	}
	if !strings.Contains(rec.Body.String(), "secret-of-bob") {
		t.Errorf("bob's own detail is missing: %s", rec.Body.String())
	}
}
