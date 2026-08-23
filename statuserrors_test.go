package muzak

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// namedErrors lists every constructor with the status and classifier it is
// expected to produce, so that adding one without wiring up its code fails
// here rather than in an application's response.
var namedErrors = []struct {
	name   string
	build  func(string) *HTTPError
	status int
	code   string
}{
	{"BadRequest", BadRequest, http.StatusBadRequest, CodeBadRequest},
	{"Unauthorized", Unauthorized, http.StatusUnauthorized, CodeUnauthorized},
	{"PaymentRequired", PaymentRequired, http.StatusPaymentRequired, CodePaymentRequired},
	{"Forbidden", Forbidden, http.StatusForbidden, CodeForbidden},
	{"NotFound", NotFound, http.StatusNotFound, CodeNotFound},
	{"MethodNotAllowed", MethodNotAllowed, http.StatusMethodNotAllowed, CodeMethodNotAllowed},
	{"NotAcceptable", NotAcceptable, http.StatusNotAcceptable, CodeNotAcceptable},
	{"RequestTimeout", RequestTimeout, http.StatusRequestTimeout, CodeRequestTimeout},
	{"Conflict", Conflict, http.StatusConflict, CodeConflict},
	{"Gone", Gone, http.StatusGone, CodeGone},
	{"PreconditionFailed", PreconditionFailed, http.StatusPreconditionFailed, CodePreconditionFailed},
	{"PayloadTooLarge", PayloadTooLarge, http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
	{"UnsupportedMediaType", UnsupportedMediaType, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType},
	{"UnprocessableEntity", UnprocessableEntity, http.StatusUnprocessableEntity, CodeValidationError},
	{"TooManyRequests", TooManyRequests, http.StatusTooManyRequests, CodeTooManyRequests},
	{"InternalServerError", InternalServerError, http.StatusInternalServerError, CodeInternalError},
	{"NotImplemented", NotImplemented, http.StatusNotImplemented, CodeNotImplemented},
	{"BadGateway", BadGateway, http.StatusBadGateway, CodeBadGateway},
	{"ServiceUnavailable", ServiceUnavailable, http.StatusServiceUnavailable, CodeServiceUnavailable},
	{"GatewayTimeout", GatewayTimeout, http.StatusGatewayTimeout, CodeGatewayTimeout},
}

func TestNamedErrorsCarryTheirStatusAndCode(t *testing.T) {
	t.Parallel()
	for _, tc := range namedErrors {
		err := tc.build("something went wrong")
		if err.Status != tc.status {
			t.Errorf("%s status = %d, want %d", tc.name, err.Status, tc.status)
		}
		if got := err.ErrorCode(); got != tc.code {
			t.Errorf("%s code = %q, want %q", tc.name, got, tc.code)
		}
		if err.Message != "something went wrong" {
			t.Errorf("%s message = %q, want the one it was given", tc.name, err.Message)
		}
	}
}

// TestNamedErrorsFallBackToAStandardSentence covers the empty message, which
// is how an application asks for the standard wording for a status.
func TestNamedErrorsFallBackToAStandardSentence(t *testing.T) {
	t.Parallel()
	for _, tc := range namedErrors {
		err := tc.build("")
		if err.Message == "" {
			t.Errorf("%s has no standard message", tc.name)
			continue
		}
		if !strings.HasSuffix(err.Message, ".") {
			t.Errorf("%s standard message is not a sentence: %q", tc.name, err.Message)
		}
		// The wording is sent to clients, so it must not name anything
		// server-side.
		for _, leaked := range []string{"muzak", "Muzak", "handler", "goroutine"} {
			if strings.Contains(err.Message, leaked) {
				t.Errorf("%s standard message mentions %q: %q", tc.name, leaked, err.Message)
			}
		}
	}
}

// TestNamedErrorStatusesAreClassified checks that no constructor falls back to
// the generic classifier, which would make its code useless to a client
// switching on it.
func TestNamedErrorStatusesAreClassified(t *testing.T) {
	t.Parallel()
	for status := range statusMessages {
		if code := CodeForStatus(status); code == CodeClientError {
			t.Errorf("status %d has no classifier of its own, only %q", status, code)
		}
	}
	for _, tc := range namedErrors {
		if _, described := statusMessages[tc.status]; !described {
			t.Errorf("%s has no standard message registered for status %d", tc.name, tc.status)
		}
	}
}

// TestNamedErrorsCompose checks that what the constructors return is an
// ordinary *HTTPError, so everything already built for one applies.
func TestNamedErrorsCompose(t *testing.T) {
	t.Parallel()
	cause := errors.New("no rows in result set")
	err := NotFound("no user goes by that name").
		WithCode("user_unknown").
		WithDetails(ErrorDetail{Field: "username", Location: "path", Issue: "does not exist"}).
		Wrap(cause)

	if got := err.ErrorCode(); got != "user_unknown" {
		t.Errorf("code = %q, want the override", got)
	}
	if !errors.Is(err, cause) {
		t.Error("the wrapped cause is not visible to errors.Is")
	}
	if len(err.Details) != 1 || err.Details[0].Field != "username" {
		t.Errorf("details = %+v", err.Details)
	}
	if !strings.Contains(err.Error(), "no rows in result set") {
		t.Errorf("the server-side rendering hides the cause: %s", err.Error())
	}

	var status StatusCoder
	if !errors.As(err, &status) || status.HTTPStatus() != http.StatusNotFound {
		t.Error("a named error does not carry its status through errors.As")
	}
}

// TestNamedErrorsRenderThroughAHandler covers the whole path: a handler
// returns one and the client reads the envelope it produces.
func TestNamedErrorsRenderThroughAHandler(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/gone", func(ctx *Context, in struct{}) (Empty, error) {
		return Empty{}, Gone("that photo was deleted in 2019")
	})
	app.Get("/teapot", func(ctx *Context, in struct{}) (Empty, error) {
		return Empty{}, NewHTTPError(http.StatusTeapot, "I'm a teapot")
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/gone")
	assertStatus(t, rec, http.StatusGone)
	body := decodeError(t, rec)
	if body.Error.Code != CodeGone {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeGone)
	}
	if body.Error.Message != "that photo was deleted in 2019" {
		t.Errorf("message = %q", body.Error.Message)
	}
	if body.Error.Status != http.StatusGone {
		t.Errorf("status in the envelope = %d", body.Error.Status)
	}
	if body.RequestID == "" {
		t.Error("the envelope carries no request identifier")
	}

	// A status with no constructor of its own still works through
	// NewHTTPError, and is classified as a client error.
	teapot := do(t, app, "GET", "/teapot")
	assertStatus(t, teapot, http.StatusTeapot)
	if got := decodeError(t, teapot).Error.Code; got != CodeClientError {
		t.Errorf("code = %q, want %q", got, CodeClientError)
	}
}

// TestNamedServerErrorsDiscloseTheirMessageButNotTheirCause checks the one
// place these differ from an unexpected fault: returning a 5xx by name is
// deliberate, so its message reaches the client while the cause stays in the
// log.
func TestNamedServerErrorsDiscloseTheirMessageButNotTheirCause(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.Get("/upstream", func(ctx *Context, in struct{}) (Empty, error) {
		return Empty{}, BadGateway("the pricing service is not answering").
			Wrap(errors.New("dial tcp 10.0.0.7:5432: connect: connection refused"))
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/upstream")
	assertStatus(t, rec, http.StatusBadGateway)

	body := decodeError(t, rec)
	if body.Error.Message != "the pricing service is not answering" {
		t.Errorf("message = %q, want the one the handler wrote", body.Error.Message)
	}
	if body.Error.Code != CodeBadGateway {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeBadGateway)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.7") {
		t.Errorf("the wrapped cause reached the client:\n%s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "10.0.0.7") {
		t.Errorf("the wrapped cause was not logged:\n%s", logs.String())
	}
}
