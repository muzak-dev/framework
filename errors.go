package muzak

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"slices"
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

	// Kind names the rule that failed, as "blank" or "too_short". It is not
	// serialized: it is what an [ErrorRenderer] branches on, and what
	// [DefaultErrorRenderer] renders Issue from when the application has a
	// translation store configured.
	Kind string `json:"-"`
	// Key names a translation to render Issue from, set by MessageKey or
	// [Validation.RejectKey]. It wins over Kind. It is not serialized.
	Key string `json:"-"`
	// Args carries the values the message interpolates, as alternating names
	// and values. It is not serialized.
	Args []any `json:"-"`
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
	// CodePaymentRequired classifies a request refused until an account is in
	// good standing.
	CodePaymentRequired = "payment_required"
	// CodeForbidden classifies a request whose credentials were understood
	// but insufficient.
	CodeForbidden = "forbidden"
	// CodeNotFound classifies a request for a path or resource that does not
	// exist.
	CodeNotFound = "not_found"
	// CodeMethodNotAllowed classifies a request whose method is not served at
	// an otherwise valid path.
	CodeMethodNotAllowed = "method_not_allowed"
	// CodeNotAcceptable classifies a request whose Accept header rules out
	// every representation the route can produce.
	CodeNotAcceptable = "not_acceptable"
	// CodeRequestTimeout classifies a request that arrived too slowly to be
	// waited for.
	CodeRequestTimeout = "request_timeout"
	// CodeConflict classifies a request that collides with the current state
	// of the resource.
	CodeConflict = "conflict"
	// CodeGone classifies a resource that existed and was deliberately
	// removed.
	CodeGone = "gone"
	// CodePreconditionFailed classifies a conditional request whose
	// precondition did not hold.
	CodePreconditionFailed = "precondition_failed"
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
	// CodeNotImplemented classifies a route that exists but does nothing yet.
	CodeNotImplemented = "not_implemented"
	// CodeBadGateway classifies a dependency answering with something
	// unusable.
	CodeBadGateway = "bad_gateway"
	// CodeServiceUnavailable classifies a dependency that is down or an
	// application shedding load.
	CodeServiceUnavailable = "service_unavailable"
	// CodeGatewayTimeout classifies a dependency that did not answer in time.
	CodeGatewayTimeout = "gateway_timeout"
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
	http.StatusPaymentRequired:       CodePaymentRequired,
	http.StatusForbidden:             CodeForbidden,
	http.StatusNotFound:              CodeNotFound,
	http.StatusMethodNotAllowed:      CodeMethodNotAllowed,
	http.StatusNotAcceptable:         CodeNotAcceptable,
	http.StatusRequestTimeout:        CodeRequestTimeout,
	http.StatusConflict:              CodeConflict,
	http.StatusGone:                  CodeGone,
	http.StatusPreconditionFailed:    CodePreconditionFailed,
	http.StatusRequestEntityTooLarge: CodePayloadTooLarge,
	http.StatusUnsupportedMediaType:  CodeUnsupportedMediaType,
	http.StatusUnprocessableEntity:   CodeValidationError,
	http.StatusTooManyRequests:       CodeTooManyRequests,
	http.StatusNotImplemented:        CodeNotImplemented,
	http.StatusBadGateway:            CodeBadGateway,
	http.StatusServiceUnavailable:    CodeServiceUnavailable,
	http.StatusGatewayTimeout:        CodeGatewayTimeout,
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
// keeps its status, while any other error is treated as an unexpected fault and
// reported as a bare 500 with the real cause logged but never transmitted.
// Implement it on your own error types to make them first-class citizens of
// the error pipeline without depending on [*HTTPError].
//
// What reaches the client depends on the status. Below 500, the client reads
// the Error() text of the StatusCoder itself, the value [errors.As] finds in
// the chain, and never the text of anything wrapping it: Go idiom is to add
// context with fmt.Errorf("...: %w", err) on the way up, and that context is
// exactly where a query, a connection string or a token ends up, so the outer
// layers are treated as internal. Keep the StatusCoder's own Error() free of
// internal detail, and do not fold a wrapped cause into it. For a 5xx the
// message is replaced with the same opaque sentence a plain error produces and
// the whole error is logged instead, because a server-side failure is exactly
// the error whose text is most likely to describe internals. An [*HTTPError]
// is the exception to both rules: its Message is a field written for the
// client, so it is sent as given at any status.
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

	// MessageKey names a translation the message is rendered from when the
	// application has a translation store configured. Message remains the
	// fallback, so an error carrying both reads correctly either way.
	MessageKey string
	// MessageArgs carries the values MessageKey interpolates, as alternating
	// names and values.
	MessageArgs []any

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

// with returns a copy of e that the caller may edit freely.
//
// Every builder below goes through it rather than editing e, because the
// natural way to write a reusable error is a package-level value that each
// request decorates with its own detail or cause. Editing that value in place
// would hand one request's details to the next response, grow them with every
// request, and race between concurrent ones. Details is cloned so that two
// errors built from the same one never share a backing array.
func (e *HTTPError) with(edit func(*HTTPError)) *HTTPError {
	c := *e
	c.Details = slices.Clone(e.Details)
	edit(&c)
	return &c
}

// WithMessageKey names a translation to render the message from, so that an
// error raised by an application reads in the caller's language:
//
//	return muzak.Forbidden("").WithMessageKey("errors.access.denied")
//
// The message already set is kept as the fallback, for a locale that has no
// translation of the key and for an application with no store configured.
//
// Like the other builders it returns a modified copy and leaves e untouched,
// so it is safe to call on a shared error value.
func (e *HTTPError) WithMessageKey(key string, args ...any) *HTTPError {
	return e.with(func(c *HTTPError) {
		c.MessageKey = key
		c.MessageArgs = args
	})
}

// Wrap attaches an underlying cause that is recorded in logs and made visible
// to [errors.Is] and [errors.As] but never sent to the client. It returns a
// copy of e carrying the cause, so it can be used inline in a return statement
// and on a shared error value; e itself is not changed.
func (e *HTTPError) Wrap(err error) *HTTPError {
	return e.with(func(c *HTTPError) { c.cause = err })
}

// WithCode overrides the machine-readable classifier, for cases where the
// status alone is too coarse, such as telling "card_declined" from
// "insufficient_funds" behind the same 402. It returns a modified copy for
// inline use and leaves e untouched.
func (e *HTTPError) WithCode(code string) *HTTPError {
	return e.with(func(c *HTTPError) { c.Code = code })
}

// WithDetails attaches per-field details to the error and returns a copy for
// inline use, leaving e untouched. Details appear in the "details" member of
// the response, so keep their text free of internal state.
func (e *HTTPError) WithDetails(details ...ErrorDetail) *HTTPError {
	return e.with(func(c *HTTPError) { c.Details = append(c.Details, details...) })
}

// ValidationError reports one or more fields that could not be bound from the
// request. Muzak renders it as a 422 classified "validation_error", with one
// entry in "details" per offending field, so a client learns about every
// mistake at once instead of one per round trip.
type ValidationError struct {
	// Details lists every problem found, in the order the fields are declared
	// on the input type.
	Details []ErrorDetail
	// Model names the input type the failures came from, in snake case, which
	// is the narrowest scope a translated message is looked up under. It is
	// empty when the failures belong to the request rather than to one model.
	Model string
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

// addKeyed appends one field failure that names the rule behind it, so that its
// message can be rendered in the request's locale rather than only in English.
//
// The rule is a Kind, which is looked up through the four scopes an application
// may phrase a message under. Use [ValidationError.addKey] for a failure whose
// message lives at one fixed key instead.
func (e *ValidationError) addKeyed(location, field, issue, kind string, args ...any) {
	e.Details = append(e.Details, ErrorDetail{
		Field: field, Location: location, Issue: issue, Kind: kind, Args: args,
	})
}

// addKey appends one field failure whose message lives at a fixed key.
//
// The binder's failures are keyed this way rather than by rule, because a value
// that could not be read at all is not a rule an application would want to
// phrase per field.
func (e *ValidationError) addKey(location, field, issue, key string, args ...any) {
	e.Details = append(e.Details, ErrorDetail{
		Field: field, Location: location, Issue: issue, Key: key, Args: args,
	})
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
// one detail per field. An [*HTTPError] keeps its status, code, message and
// details. Any other error implementing [StatusCoder] keeps its status and
// the code for that status; below 500 its message is the Error() text of the
// StatusCoder value found in the chain, never that of an error wrapping it,
// and from 500 up its message is the same fixed sentence as for a plain error.
// Everything else becomes a 500 whose message and code are fixed constants, so
// that an unexpected fault (a nil dereference, a database driver error, a
// wrapped file path) cannot leak internal state through the response. The
// request identifier is copied from the context in every case.
func DefaultErrorRenderer(ctx *Context, err error) (int, any) {
	body := ErrorResponse{RequestID: ctx.RequestID()}

	var ve *ValidationError
	if errors.As(err, &ve) {
		body.Error = ErrorBody{
			Code:    CodeValidationError,
			Message: ctx.message("muzak.validation.summary", validationMessage),
			Status:  http.StatusUnprocessableEntity,
			Details: translateDetails(ctx, ve.Model, ve.Details),
		}
		return body.Error.Status, body
	}

	var he *HTTPError
	if errors.As(err, &he) {
		status := clampStatus(he.Status)
		body.Error = ErrorBody{
			Code:    he.ErrorCode(),
			Message: ctx.httpMessage(he),
			Status:  status,
			Details: translateDetails(ctx, "", he.Details),
		}
		return status, body
	}

	var sc statusCoderError
	if errors.As(err, &sc) {
		status := clampStatus(sc.HTTPStatus())
		// The message is the matched error's own text rather than err.Error(),
		// because err is the outermost layer of the chain and every layer the
		// application wrapped around the StatusCoder on its way up is internal
		// context. A server-side status gets no text of its own at all: the
		// error is logged through logCause, and the client reads the same
		// sentence as for any other fault.
		message := sc.Error()
		if status >= http.StatusInternalServerError {
			message = ctx.message("muzak.validation.internal", internalMessage)
		}
		body.Error = ErrorBody{
			Code:    CodeForStatus(status),
			Message: message,
			Status:  status,
		}
		return status, body
	}

	body.Error = ErrorBody{
		Code:    CodeInternalError,
		Message: ctx.message("muzak.validation.internal", internalMessage),
		Status:  http.StatusInternalServerError,
	}
	return http.StatusInternalServerError, body
}

// statusCoderError is the target DefaultErrorRenderer hands to [errors.As].
//
// Asking for a [StatusCoder] alone would find the right value but return it as
// something with no Error method, and the renderer needs that value's own text
// rather than the text of the chain around it. Every value errors.As can match
// is an error already, so the combined interface matches exactly what a
// StatusCoder target would.
type statusCoderError interface {
	error
	StatusCoder
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
