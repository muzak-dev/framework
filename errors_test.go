package muzak

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPError(t *testing.T) {
	t.Parallel()

	t.Run("message and status", func(t *testing.T) {
		t.Parallel()
		err := NewHTTPError(http.StatusNotFound, "missing")
		if err.HTTPStatus() != http.StatusNotFound {
			t.Errorf("HTTPStatus = %d, want 404", err.HTTPStatus())
		}
		if got := err.Error(); got != "404 Not Found: missing" {
			t.Errorf("Error() = %q", got)
		}
		if err.Unwrap() != nil {
			t.Error("Unwrap on an error with no cause returned something")
		}
	})

	t.Run("formatted message", func(t *testing.T) {
		t.Parallel()
		err := NewHTTPErrorf(http.StatusBadRequest, "expected %d, got %d", 1, 2)
		if err.Message != "expected 1, got 2" {
			t.Errorf("Message = %q", err.Message)
		}
	})

	t.Run("wrapped cause is visible to errors.Is but not to the message", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("driver: connection refused")
		err := NewHTTPError(http.StatusBadGateway, "upstream unavailable").Wrap(sentinel)
		if !errors.Is(err, sentinel) {
			t.Error("errors.Is could not see the wrapped cause")
		}
		if !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("the log-facing Error() lost the cause: %q", err)
		}
		if err.Message != "upstream unavailable" {
			t.Errorf("the client-facing Message changed: %q", err.Message)
		}
	})

	t.Run("code overrides and details", func(t *testing.T) {
		t.Parallel()
		err := NewHTTPError(http.StatusPaymentRequired, "declined").
			WithCode("card_declined").
			WithDetails(ErrorDetail{Field: "card", Location: "body", Issue: "was declined"})
		if err.ErrorCode() != "card_declined" {
			t.Errorf("ErrorCode = %q", err.ErrorCode())
		}
		if len(err.Details) != 1 {
			t.Fatalf("Details = %d entries, want 1", len(err.Details))
		}
	})

	t.Run("code defaults to the one implied by the status", func(t *testing.T) {
		t.Parallel()
		if got := NewHTTPError(http.StatusConflict, "clash").ErrorCode(); got != CodeConflict {
			t.Errorf("ErrorCode = %q, want %q", got, CodeConflict)
		}
	})
}

func TestCodeForStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, CodeBadRequest},
		{http.StatusUnauthorized, CodeUnauthorized},
		{http.StatusForbidden, CodeForbidden},
		{http.StatusNotFound, CodeNotFound},
		{http.StatusMethodNotAllowed, CodeMethodNotAllowed},
		{http.StatusConflict, CodeConflict},
		{http.StatusRequestEntityTooLarge, CodePayloadTooLarge},
		{http.StatusUnsupportedMediaType, CodeUnsupportedMediaType},
		{http.StatusUnprocessableEntity, CodeValidationError},
		{http.StatusTooManyRequests, CodeTooManyRequests},
		{http.StatusTeapot, CodeClientError},
		{http.StatusBadGateway, CodeInternalError},
		{http.StatusOK, CodeInternalError},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			if got := CodeForStatus(tc.status); got != tc.want {
				t.Errorf("CodeForStatus(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

func TestClampStatus(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want int }{
		{200, 200}, {599, 599}, {100, 100},
		{99, 500}, {600, 500}, {0, 500}, {-1, 500},
	}
	for _, tc := range tests {
		if got := clampStatus(tc.in); got != tc.want {
			t.Errorf("clampStatus(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestValidationErrorMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		details []ErrorDetail
		want    string
	}{
		{
			name:    "single named field",
			details: []ErrorDetail{{Field: "limit", Location: "query", Issue: "must be a valid integer"}},
			want:    `validation failed: query "limit" must be a valid integer`,
		},
		{
			name:    "single unnamed field",
			details: []ErrorDetail{{Field: "", Location: "body", Issue: "is required"}},
			want:    "validation failed: body content is required",
		},
		{
			name: "several fields",
			details: []ErrorDetail{
				{Field: "a", Location: "query", Issue: "is required"},
				{Field: "b", Location: "query", Issue: "is required"},
			},
			want: `validation failed for 2 fields (first: query "a" is required)`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := &ValidationError{Details: tc.details}
			if got := err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			if err.HTTPStatus() != http.StatusUnprocessableEntity {
				t.Errorf("HTTPStatus = %d, want 422", err.HTTPStatus())
			}
		})
	}
}

// customStatusError implements StatusCoder without being an HTTPError, which
// is the third branch of the renderer.
type customStatusError struct {
	status int
	text   string
}

func (e *customStatusError) Error() string   { return e.text }
func (e *customStatusError) HTTPStatus() int { return e.status }

func TestDefaultErrorRenderer(t *testing.T) {
	t.Parallel()
	ctx := &Context{requestID: "req-1"}

	tests := []struct {
		name     string
		err      error
		status   int
		code     string
		message  string
		details  int
		notInMsg string
	}{
		{
			name:   "validation error",
			err:    &ValidationError{Details: []ErrorDetail{{Field: "a", Location: "query", Issue: "is required"}}},
			status: 422, code: CodeValidationError, message: validationMessage, details: 1,
		},
		{
			name:   "http error",
			err:    NewHTTPError(http.StatusNotFound, "no such item"),
			status: 404, code: CodeNotFound, message: "no such item",
		},
		{
			name:   "http error with a wrapped secret",
			err:    NewHTTPError(http.StatusBadGateway, "upstream unavailable").Wrap(errors.New("dial tcp 10.0.0.5:5432: refused")),
			status: 502, code: CodeInternalError, message: "upstream unavailable",
			notInMsg: "10.0.0.5",
		},
		{
			name:   "http error with an impossible status",
			err:    NewHTTPError(9999, "nonsense"),
			status: 500, code: CodeInternalError, message: "nonsense",
		},
		{
			name:   "custom status coder",
			err:    &customStatusError{status: http.StatusTeapot, text: "short and stout"},
			status: 418, code: CodeClientError, message: "short and stout",
		},
		{
			name:   "custom status coder out of range",
			err:    &customStatusError{status: 7, text: "nonsense"},
			status: 500, code: CodeInternalError, message: "nonsense",
		},
		{
			name:   "plain error is opaque",
			err:    errors.New("table users does not exist"),
			status: 500, code: CodeInternalError, message: internalMessage,
			notInMsg: "users",
		},
		{
			name:   "wrapped plain error is opaque",
			err:    fmt.Errorf("loading config: %w", errors.New("/etc/secrets/key.pem: permission denied")),
			status: 500, code: CodeInternalError, message: internalMessage,
			notInMsg: "secrets",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, body := DefaultErrorRenderer(ctx, tc.err)
			if status != tc.status {
				t.Errorf("status = %d, want %d", status, tc.status)
			}
			envelope, ok := body.(ErrorResponse)
			if !ok {
				t.Fatalf("body is %T, want ErrorResponse", body)
			}
			if envelope.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", envelope.Error.Code, tc.code)
			}
			if envelope.Error.Message != tc.message {
				t.Errorf("message = %q, want %q", envelope.Error.Message, tc.message)
			}
			if envelope.Error.Status != status {
				t.Errorf("the envelope reports status %d but the renderer returned %d", envelope.Error.Status, status)
			}
			if len(envelope.Error.Details) != tc.details {
				t.Errorf("details = %d entries, want %d", len(envelope.Error.Details), tc.details)
			}
			if envelope.RequestID != "req-1" {
				t.Errorf("request id = %q, want %q", envelope.RequestID, "req-1")
			}
			if tc.notInMsg != "" && strings.Contains(envelope.Error.Message, tc.notInMsg) {
				t.Errorf("the message leaked %q: %s", tc.notInMsg, envelope.Error.Message)
			}
		})
	}
}

func TestLogCause(t *testing.T) {
	t.Parallel()
	cause := errors.New("underlying")

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"validation errors are not server faults", &ValidationError{Details: []ErrorDetail{{}}}, nil},
		{"a deliberate 4xx needs no log line", NewHTTPError(400, "bad"), nil},
		{"an http error's cause is logged", NewHTTPError(502, "upstream").Wrap(cause), cause},
		{"a 4xx status coder needs no log line", &customStatusError{status: 404, text: "x"}, nil},
		{"a 5xx status coder is logged", &customStatusError{status: 503, text: "x"}, &customStatusError{status: 503, text: "x"}},
		{"an unexpected error is always logged", cause, cause},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := logCause(tc.err)
			if tc.want == nil {
				if got != nil {
					t.Errorf("logCause = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("logCause = nil, want %v", tc.want)
			}
			if got.Error() != tc.want.Error() {
				t.Errorf("logCause = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDescribeJSONKind(t *testing.T) {
	t.Parallel()
	tests := map[byte]string{
		'n': "null", 'f': "a boolean", 't': "a boolean", '"': "a string",
		'0': "a number", '{': "an object", '[': "an array", 'x': "that value",
	}
	for kind, want := range tests {
		if got := describeJSONKind(jsontext.Kind(kind)); got != want {
			t.Errorf("describeJSONKind(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestSanitizeSyntaxError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"package prefix removed", errors.New("jsontext: unexpected EOF"), "is not valid JSON: unexpected EOF"},
		{"json prefix removed", errors.New("json: bad thing"), "is not valid JSON: bad thing"},
		{"within clause trimmed", errors.New("jsontext: invalid character within object"), "is not valid JSON: invalid character"},
		{"offset trimmed", errors.New("jsontext: invalid character after offset 12"), "is not valid JSON: invalid character"},
		{"at offset trimmed", errors.New("jsontext: broken at offset 3"), "is not valid JSON: broken"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sanitizeSyntaxError(tc.err); got != tc.want {
				t.Errorf("sanitizeSyntaxError = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeIssueFallsBackForSyntaxErrors(t *testing.T) {
	t.Parallel()
	field, issue := decodeIssue(errors.New("jsontext: unexpected EOF"))
	if field != "" {
		t.Errorf("field = %q, want empty for a syntax error", field)
	}
	if !strings.Contains(issue, "is not valid JSON") {
		t.Errorf("issue = %q", issue)
	}
}

func TestCustomErrorRenderer(t *testing.T) {
	t.Parallel()
	type customEnvelope struct {
		Problem string `json:"problem"`
		Where   string `json:"where"`
	}

	opts := quietOptions()
	opts.ErrorRenderer = func(ctx *Context, err error) (int, any) {
		return http.StatusIMUsed, customEnvelope{Problem: err.Error(), Where: ctx.Request().URL.Path}
	}
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, NewHTTPError(http.StatusNotFound, "gone fishing")
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusIMUsed)
	assertJSON(t, rec, `{"problem":"404 Not Found: gone fishing","where":"/x"}`)
}

// TestErrorRendererMayReturnNoBody covers a renderer that chooses to answer
// with a bare status line.
func TestErrorRendererMayReturnNoBody(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.ErrorRenderer = func(ctx *Context, err error) (int, any) {
		return http.StatusNoContent, nil
	}
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, errors.New("anything")
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusNoContent)
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

// TestErrorRendererProducingUnserializableBody covers the fallback that keeps a
// broken renderer from producing a body-less 500.
func TestErrorRendererProducingUnserializableBody(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	opts.ErrorRenderer = func(ctx *Context, err error) (int, any) {
		// A channel cannot be encoded as JSON.
		return http.StatusInternalServerError, make(chan int)
	}
	app := New(opts)
	app.Get("/x", func(ctx *Context, _ Empty) (rtOut, error) {
		return rtOut{}, errors.New("anything")
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusInternalServerError)
	if code := decodeError(t, rec).Error.Code; code != CodeInternalError {
		t.Errorf("code = %q, want the fixed fallback envelope", code)
	}
	if !strings.Contains(logs.String(), "unserializable") {
		t.Errorf("the broken renderer was not reported:\n%s", logs.String())
	}
}

// TestUnserializableResponseBecomesAnOpaque500 covers a handler whose return
// value cannot be encoded, which must not produce a truncated body.
func TestUnserializableResponseBecomesAnOpaque500(t *testing.T) {
	t.Parallel()
	type bad struct {
		Ch chan int `json:"ch"`
	}
	app := New(quietOptions())
	app.Get("/x", func(ctx *Context, _ Empty) (bad, error) {
		return bad{Ch: make(chan int)}, nil
	})
	mustBuild(t, app)

	rec := do(t, app, "GET", "/x")
	assertStatus(t, rec, http.StatusInternalServerError)
	if code := decodeError(t, rec).Error.Code; code != CodeInternalError {
		t.Errorf("code = %q, want %q", code, CodeInternalError)
	}
}
