package muzak

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrorDetail describes one specific thing that was wrong with a request.
//
// Details are what turn a bare "the request could not be validated" into
// something a client can act on, naming the offending field, where it was read
// from and what it should have been instead.
type ErrorDetail struct {
	// Field is the name of the parameter or JSON member at fault, such as
	// "limit". It is empty when the problem concerns the request as a whole
	// rather than one field.
	Field string `json:"field"`
	// Location is where the field was read from: "path", "query", "header",
	// "cookie" or "body".
	Location string `json:"location"`
	// Issue explains what was wrong, phrased to read after the field name, as
	// in "is required" or "must be a valid integer".
	Issue string `json:"issue"`
}

// ErrorBody is the "error" member of an error response.
type ErrorBody struct {
	// Code is a stable, machine-readable classifier such as
	// "validation_error" or "not_found". Clients should branch on it rather
	// than on the status code or the message text.
	Code string `json:"code"`
	// Message is a human-readable summary, safe to display and safe to
	// disclose. It never contains internal state.
	Message string `json:"message"`
	// Status repeats the HTTP status code, so that a response body which has
	// been logged or forwarded remains self-describing.
	Status int `json:"status"`
	// Details lists the individual problems found. It is omitted when there
	// are none.
	Details []ErrorDetail `json:"details,omitzero"`
}

// ErrorResponse is the JSON body Muzak writes for every unsuccessful request.
//
// The shape is deliberately fixed and self-describing:
//
//	{
//	  "error": {
//	    "code": "validation_error",
//	    "message": "The request could not be validated.",
//	    "status": 422,
//	    "details": [
//	      { "field": "name", "location": "body", "issue": "is required" },
//	      { "field": "limit", "location": "query", "issue": "must be a valid integer" }
//	    ]
//	  },
//	  "request_id": "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"
//	}
//
// Set [AppOptions.ErrorRenderer] to replace it with a shape of your own.
type ErrorResponse struct {
	// Error carries the classification, summary and per-field details.
	Error ErrorBody `json:"error"`
	// RequestID correlates the response with the server-side log entry
	// written for the same request.
	RequestID string `json:"request_id,omitzero"`
}

// Error codes Muzak uses by default. An [HTTPError] that does not set a code
// of its own is given the one that matches its status.
const (
	// CodeBadRequest classifies a malformed request that could not be read at
	// all.
	CodeBadRequest = "bad_request"
	// CodeUnauthorized classifies a request that carried no usable
	// credentials.
	CodeUnauthorized = "unauthorized"
	// CodeForbidden classifies a request whose credentials were understood
	// but insufficient.
	CodeForbidden = "forbidden"
	// CodeNotFound classifies a request for a path or resource that does not
	// exist.
	CodeNotFound = "not_found"
	// CodeMethodNotAllowed classifies a request whose method is not served at
	// an otherwise valid path.
	CodeMethodNotAllowed = "method_not_allowed"
	// CodeConflict classifies a request that collides with the current state
	// of the resource.
	CodeConflict = "conflict"
	// CodePayloadTooLarge classifies a request body over the route's limit.
	CodePayloadTooLarge = "payload_too_large"
	// CodeUnsupportedMediaType classifies a body sent under a media type the
	// route cannot decode.
	CodeUnsupportedMediaType = "unsupported_media_type"
	// CodeValidationError classifies a request whose fields failed binding or
	// validation. Responses carrying it always populate Details.
	CodeValidationError = "validation_error"
	// CodeTooManyRequests classifies a rate-limited request.
	CodeTooManyRequests = "too_many_requests"
	// CodeInternalError classifies an unexpected server-side fault. It is the
	// code used for every error that does not describe itself, and the
	// response never carries anything derived from the underlying cause.
	CodeInternalError = "internal_error"
	// CodeClientError classifies a 4xx that Muzak has no more specific code
	// for.
	CodeClientError = "client_error"
)

// Default messages for the outcomes Muzak produces itself.
const (
	validationMessage = "The request could not be validated."
	internalMessage   = "The server could not complete the request."
)

// statusCodes maps the statuses Muzak produces onto their default machine
// readable classifiers.
var statusCodes = map[int]string{
	http.StatusBadRequest:            CodeBadRequest,
	http.StatusUnauthorized:          CodeUnauthorized,
	http.StatusForbidden:             CodeForbidden,
	http.StatusNotFound:              CodeNotFound,
	http.StatusMethodNotAllowed:      CodeMethodNotAllowed,
	http.StatusConflict:              CodeConflict,
	http.StatusRequestEntityTooLarge: CodePayloadTooLarge,
	http.StatusUnsupportedMediaType:  CodeUnsupportedMediaType,
	http.StatusUnprocessableEntity:   CodeValidationError,
	http.StatusTooManyRequests:       CodeTooManyRequests,
}

// CodeForStatus returns the default error code for an HTTP status.
//
// Statuses Muzak recognises get a specific classifier such as "not_found";
// any other 4xx becomes [CodeClientError] and everything else becomes
// [CodeInternalError]. Use it when writing a custom [ErrorRenderer] that
// should stay consistent with the built-in codes.
func CodeForStatus(status int) string {
	if code, ok := statusCodes[status]; ok {
		return code
	}
	if status >= 400 && status < 500 {
		return CodeClientError
	}
	return CodeInternalError
}

// StatusCoder is implemented by errors that carry their own HTTP status code.
//
// Muzak consults it when turning a handler or dependency error into a
// response: an error that implements StatusCoder is considered deliberate and
// its message is sent to the client, while any other error is treated as an
// unexpected fault and reported as a bare 500 with the real cause logged but
// never transmitted. Implement it on your own error types to make them
// first-class citizens of the error pipeline without depending on
// [*HTTPError].
type StatusCoder interface {
	// HTTPStatus returns the status code that should be written for this
	// error. Values outside the 100 to 599 range are clamped to 500.
	HTTPStatus() int
}

// HTTPError is an error that carries an HTTP status code and a message that is
// safe to disclose to the client.
//
// Returning an *HTTPError from a handler, a guard dependency or a value
// dependency short-circuits the request: nothing further down the chain runs,
// and the status, code and message become the response. Any other error type
// is deliberately opaque to the client; see [StatusCoder].
type HTTPError struct {
	// Status is the HTTP status code to write.
	Status int
	// Code is the machine-readable classifier. When empty, the code matching
	// Status is used; see [CodeForStatus].
	Code string
	// Message is the client-visible explanation. Keep it free of internal
	// detail, because it is transmitted verbatim.
	Message string
	// Details lists the individual problems behind the error, if any.
	Details []ErrorDetail

	cause error
}

// NewHTTPError returns an *HTTPError with the given status code and
// client-visible message, as in NewHTTPError(401, "unauthorized"). The error
// code is derived from the status unless [HTTPError.WithCode] overrides it.
func NewHTTPError(status int, message string) *HTTPError {
	return &HTTPError{Status: status, Message: message}
}

// NewHTTPErrorf returns an *HTTPError whose message is built with
// [fmt.Sprintf]. Because the formatted result is sent to the client, avoid
// interpolating internal state such as file paths or driver errors; use
// [HTTPError.Wrap] for those instead, which keeps them server-side.
func NewHTTPErrorf(status int, format string, args ...any) *HTTPError {
	return &HTTPError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// Error implements the error interface, rendering the status, message and
// cause in a form suited to server-side logs rather than to clients.
func (e *HTTPError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, http.StatusText(e.Status), e.Message, e.cause)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// HTTPStatus implements [StatusCoder].
func (e *HTTPError) HTTPStatus() int { return e.Status }

// ErrorCode returns the machine-readable classifier for the error, falling
// back to the one implied by its status.
func (e *HTTPError) ErrorCode() string {
	if e.Code != "" {
		return e.Code
	}
	return CodeForStatus(e.Status)
}

// Unwrap returns the error passed to [HTTPError.Wrap], letting [errors.Is] and
// [errors.As] see through to the underlying cause. It returns nil when no
// cause was attached.
func (e *HTTPError) Unwrap() error { return e.cause }

// Wrap attaches an underlying cause that is recorded in logs and made visible
// to [errors.Is] and [errors.As] but never sent to the client. It returns e so
// it can be used inline in a return statement.
func (e *HTTPError) Wrap(err error) *HTTPError {
	e.cause = err
	return e
}

// WithCode overrides the machine-readable classifier, for cases where the
// status alone is too coarse, such as telling "card_declined" from
// "insufficient_funds" behind the same 402. It returns e for inline use.
func (e *HTTPError) WithCode(code string) *HTTPError {
	e.Code = code
	return e
}

// WithDetails attaches per-field details to the error and returns e for inline
// use. Details appear in the "details" member of the response, so keep their
// text free of internal state.
func (e *HTTPError) WithDetails(details ...ErrorDetail) *HTTPError {
	e.Details = append(e.Details, details...)
	return e
}

// ValidationError reports one or more fields that could not be bound from the
// request. Muzak renders it as a 422 classified "validation_error", with one
// entry in "details" per offending field, so a client learns about every
// mistake at once instead of one per round trip.
type ValidationError struct {
	// Details lists every problem found, in the order the fields are declared
	// on the input type.
	Details []ErrorDetail
}

// Error implements the error interface, summarizing how many fields failed and
// naming the first of them.
func (e *ValidationError) Error() string {
	first := e.Details[0]
	if len(e.Details) == 1 {
		return fmt.Sprintf("validation failed: %s %s %s", first.Location, describeField(first.Field), first.Issue)
	}
	return fmt.Sprintf("validation failed for %d fields (first: %s %s %s)",
		len(e.Details), first.Location, describeField(first.Field), first.Issue)
}

// describeField renders a field name for a log message, naming the request
// part itself when the problem was not attributable to one field.
func describeField(name string) string {
	if name == "" {
		return "content"
	}
	return `"` + name + `"`
}

// HTTPStatus implements [StatusCoder], reporting 422 Unprocessable Content.
func (e *ValidationError) HTTPStatus() int { return http.StatusUnprocessableEntity }

// add appends one field failure.
func (e *ValidationError) add(location, field, issue string) {
	e.Details = append(e.Details, ErrorDetail{Field: field, Location: location, Issue: issue})
}

// ErrorRenderer converts an error into the response written for it.
//
// It receives the request [Context], whose route, request identifier and
// headers are all available, and the error the request failed with. It returns
// the status code to write and the value to serialize as the body; returning a
// nil body writes the status with no content.
//
// A renderer must not leak internal state. The error it receives may be
// anything a handler returned, including a wrapped database or filesystem
// error, so a custom renderer should classify the errors it recognises and
// fall back to an opaque response for the rest, exactly as
// [DefaultErrorRenderer] does. Install one with [AppOptions.ErrorRenderer].
type ErrorRenderer func(ctx *Context, err error) (status int, body any)

// DefaultErrorRenderer produces Muzak's standard [ErrorResponse] envelope.
//
// A [*ValidationError] becomes a 422 classified "validation_error" carrying
// one detail per field. An error implementing [StatusCoder], including
// [*HTTPError], keeps its status, code, message and details. Everything else
// becomes a 500 whose message and code are fixed constants, so that an
// unexpected fault (a nil dereference, a database driver error, a wrapped file
// path) cannot leak internal state through the response. The request
// identifier is copied from the context in every case.
func DefaultErrorRenderer(ctx *Context, err error) (int, any) {
	body := ErrorResponse{RequestID: ctx.RequestID()}

	var ve *ValidationError
	if errors.As(err, &ve) {
		body.Error = ErrorBody{
			Code:    CodeValidationError,
			Message: validationMessage,
			Status:  http.StatusUnprocessableEntity,
			Details: ve.Details,
		}
		return body.Error.Status, body
	}

	var he *HTTPError
	if errors.As(err, &he) {
		status := clampStatus(he.Status)
		body.Error = ErrorBody{
			Code:    he.ErrorCode(),
			Message: he.Message,
			Status:  status,
			Details: he.Details,
		}
		return status, body
	}

	var sc StatusCoder
	if errors.As(err, &sc) {
		status := clampStatus(sc.HTTPStatus())
		body.Error = ErrorBody{
			Code:    CodeForStatus(status),
			Message: err.Error(),
			Status:  status,
		}
		return status, body
	}

	body.Error = ErrorBody{
		Code:    CodeInternalError,
		Message: internalMessage,
		Status:  http.StatusInternalServerError,
	}
	return http.StatusInternalServerError, body
}

// logCause returns the part of an error worth recording server-side but not
// worth sending to the client, or nil when the error is fully described by the
// response it produces. A deliberate 4xx is not a server fault and does not
// need a log line of its own; an unexpected error always does.
func logCause(err error) error {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return nil
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.cause
	}
	var sc StatusCoder
	if errors.As(err, &sc) {
		if clampStatus(sc.HTTPStatus()) >= 500 {
			return err
		}
		return nil
	}
	return err
}

// clampStatus rejects status codes outside the range HTTP defines, falling
// back to 500 so that a miscomputed code cannot produce a malformed response
// line.
func clampStatus(code int) int {
	if code < 100 || code > 599 {
		return http.StatusInternalServerError
	}
	return code
}

// decodeIssue turns a decoder error into a field name and an issue phrase safe
// to return to the client.
//
// encoding/json/v2 reports a JSON pointer to the offending member, which gives
// the detail its field name. The accompanying Go type is deliberately dropped,
// because naming server-side types in a response tells a client more about the
// implementation than it needs to know.
func decodeIssue(err error) (field, issue string) {
	var se *json.SemanticError
	if errors.As(err, &se) {
		field = se.JSONPointer.LastToken()
		switch {
		case errors.Is(se.Err, json.ErrUnknownName):
			return field, "is not a field this endpoint accepts"
		case se.JSONKind != 0:
			return field, fmt.Sprintf("has the wrong type, %s is not accepted here", describeJSONKind(se.JSONKind))
		default:
			return field, "could not be decoded"
		}
	}
	return "", sanitizeSyntaxError(err)
}

// describeJSONKind names a JSON kind in words a client will recognise.
func describeJSONKind(k jsontext.Kind) string {
	switch k {
	case 'n':
		return "null"
	case 'f', 't':
		return "a boolean"
	case '"':
		return "a string"
	case '0':
		return "a number"
	case '{':
		return "an object"
	case '[':
		return "an array"
	default:
		return "that value"
	}
}

// sanitizeSyntaxError reduces a syntactic decoding failure to a short phrase.
// The decoder's own messages describe the offending JSON rather than server
// internals, but they carry package prefixes and byte offsets that are noise
// to a client.
func sanitizeSyntaxError(err error) string {
	msg := err.Error()
	for _, cut := range []string{" within ", " after offset ", " at offset "} {
		if i := strings.Index(msg, cut); i >= 0 {
			msg = msg[:i]
		}
	}
	msg = strings.TrimPrefix(msg, "jsontext: ")
	msg = strings.TrimPrefix(msg, "json: ")
	return "is not valid JSON: " + msg
}
