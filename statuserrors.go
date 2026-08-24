package muzak

import (
	"net/http"
	"strconv"
)

// This file holds one constructor per outcome an application reaches for by
// name, so that returning the right response does not mean remembering the
// right number. Each returns an [*HTTPError], so each composes with
// [HTTPError.Wrap], [HTTPError.WithCode] and [HTTPError.WithDetails]:
//
//	user, err := store.Find(ctx, name)
//	if errors.Is(err, sql.ErrNoRows) {
//		return schemas.UserOut{}, muzak.NotFound("no user goes by that name").Wrap(err)
//	}
//
// Every one of them takes the message the client will read. Passing "" uses
// the standard sentence for that status, which is the equivalent of raising
// the exception with no argument in a framework that has one per status:
//
//	muzak.Forbidden("")  // "You do not have access to this resource."
//
// For a message built from values, use [NewHTTPErrorf], and keep internal
// state out of it: the message is transmitted verbatim. That matters most for
// the 5xx constructors, whose message is also disclosed because returning one
// is a deliberate act; the cause belongs in [HTTPError.Wrap], which is logged
// and never sent.
//
// A status with no constructor here is still one [NewHTTPError] can produce.

// statusMessages holds the sentence each constructor falls back to when it is
// given no message. They describe the outcome without naming anything
// server-side, because they are sent as they are.
var statusMessages = map[int]string{
	http.StatusBadRequest:            "The request could not be understood.",
	http.StatusUnauthorized:          "Authentication is required.",
	http.StatusPaymentRequired:       "Payment is required.",
	http.StatusForbidden:             "You do not have access to this resource.",
	http.StatusNotFound:              "The requested resource was not found.",
	http.StatusMethodNotAllowed:      "That method is not allowed here.",
	http.StatusNotAcceptable:         "No representation acceptable to this client is available.",
	http.StatusRequestTimeout:        "The request took too long to arrive.",
	http.StatusConflict:              "The request conflicts with the current state of the resource.",
	http.StatusGone:                  "The requested resource is no longer available.",
	http.StatusPreconditionFailed:    "A precondition given in the request failed.",
	http.StatusRequestEntityTooLarge: "The request body is larger than this route accepts.",
	http.StatusUnsupportedMediaType:  "The request body is in a media type this route cannot read.",
	http.StatusUnprocessableEntity:   "The request was understood but could not be processed.",
	http.StatusTooManyRequests:       "Too many requests have been sent.",
	http.StatusInternalServerError:   internalMessage,
	http.StatusNotImplemented:        "That is not implemented.",
	http.StatusBadGateway:            "A service this request depends on answered with something unusable.",
	http.StatusServiceUnavailable:    "The service is temporarily unavailable.",
	http.StatusGatewayTimeout:        "A service this request depends on did not answer in time.",
}

// statusError builds the error one of the constructors below returns, falling
// back to the standard sentence for the status when no message is given.
func statusError(status int, message string) *HTTPError {
	e := &HTTPError{Status: status, Message: orDefault(message, statusMessages[status])}
	if message == "" {
		// Only the standard sentence is translated. A message written by the
		// caller is the caller's own words, and replacing them with a
		// translation of something else would be wrong.
		e.MessageKey = "muzak.http." + strconv.Itoa(status)
	}
	return e
}

// BadRequest returns a 400 classified [CodeBadRequest], for a request that
// could not be understood at all.
//
// A request that was understood but asks for something the application will
// not do is better answered with [UnprocessableEntity], and one whose fields
// failed validation is answered by Muzak itself before a handler runs.
func BadRequest(message string) *HTTPError {
	return statusError(http.StatusBadRequest, message)
}

// Unauthorized returns a 401 classified [CodeUnauthorized], for a request that
// carried no usable credentials.
//
// It is the answer to "I do not know who you are". When the caller is known
// but not allowed, use [Forbidden].
func Unauthorized(message string) *HTTPError {
	return statusError(http.StatusUnauthorized, message)
}

// PaymentRequired returns a 402 classified [CodePaymentRequired], for a
// request refused until an account is in good standing.
func PaymentRequired(message string) *HTTPError {
	return statusError(http.StatusPaymentRequired, message)
}

// Forbidden returns a 403 classified [CodeForbidden], for a request whose
// credentials were understood but are not enough.
//
// Answering 404 instead is the right call when admitting the resource exists
// would itself disclose something; that is a decision to make deliberately.
func Forbidden(message string) *HTTPError {
	return statusError(http.StatusForbidden, message)
}

// NotFound returns a 404 classified [CodeNotFound], for a resource that does
// not exist.
func NotFound(message string) *HTTPError {
	return statusError(http.StatusNotFound, message)
}

// MethodNotAllowed returns a 405 classified [CodeMethodNotAllowed].
//
// Muzak already answers an unrouted method with a 405 and an accurate Allow
// header, so reach for this only when a handler decides that for itself.
func MethodNotAllowed(message string) *HTTPError {
	return statusError(http.StatusMethodNotAllowed, message)
}

// NotAcceptable returns a 406 classified [CodeNotAcceptable], for a request
// whose Accept header rules out every representation available.
func NotAcceptable(message string) *HTTPError {
	return statusError(http.StatusNotAcceptable, message)
}

// RequestTimeout returns a 408 classified [CodeRequestTimeout], for a client
// that took too long to send its request.
func RequestTimeout(message string) *HTTPError {
	return statusError(http.StatusRequestTimeout, message)
}

// Conflict returns a 409 classified [CodeConflict], for a request that
// collides with the current state of the resource, such as a name already
// taken or an edit made against a version that has since moved.
func Conflict(message string) *HTTPError {
	return statusError(http.StatusConflict, message)
}

// Gone returns a 410 classified [CodeGone], for a resource that existed and
// was deliberately removed. It differs from [NotFound] in saying so on
// purpose, which lets a client stop asking.
func Gone(message string) *HTTPError {
	return statusError(http.StatusGone, message)
}

// PreconditionFailed returns a 412 classified [CodePreconditionFailed], for a
// conditional request whose precondition did not hold, as when an
// If-Match header names an entity tag the resource no longer carries.
func PreconditionFailed(message string) *HTTPError {
	return statusError(http.StatusPreconditionFailed, message)
}

// PayloadTooLarge returns a 413 classified [CodePayloadTooLarge].
//
// Muzak enforces [AppOptions.MaxBodySize] and the per-route limits itself, so
// this is for a bound a handler applies of its own.
func PayloadTooLarge(message string) *HTTPError {
	return statusError(http.StatusRequestEntityTooLarge, message)
}

// UnsupportedMediaType returns a 415 classified [CodeUnsupportedMediaType],
// for a body sent under a media type the route cannot read.
func UnsupportedMediaType(message string) *HTTPError {
	return statusError(http.StatusUnsupportedMediaType, message)
}

// UnprocessableEntity returns a 422 classified [CodeValidationError], for a
// request that was well-formed and understood but asks for something that
// cannot be done.
//
// Muzak produces this status itself for a request whose fields failed
// validation, with one entry per field. Attach [HTTPError.WithDetails] to say
// which field is at fault here too, so that a client sees the same shape
// whichever produced it.
func UnprocessableEntity(message string) *HTTPError {
	return statusError(http.StatusUnprocessableEntity, message)
}

// TooManyRequests returns a 429 classified [CodeTooManyRequests].
//
// Muzak's own rate limiting answers with this status and the accompanying
// headers; this is for a quota an application counts for itself.
func TooManyRequests(message string) *HTTPError {
	return statusError(http.StatusTooManyRequests, message)
}

// InternalServerError returns a 500 classified [CodeInternalError].
//
// Returning it is a deliberate act, so unlike an unexpected fault its message
// reaches the client. Keep that message free of internal state and put the
// real cause in [HTTPError.Wrap], which is logged and never transmitted.
func InternalServerError(message string) *HTTPError {
	return statusError(http.StatusInternalServerError, message)
}

// NotImplemented returns a 501 classified [CodeNotImplemented], for a route
// that exists but does not do anything yet.
func NotImplemented(message string) *HTTPError {
	return statusError(http.StatusNotImplemented, message)
}

// BadGateway returns a 502 classified [CodeBadGateway], for a service this
// request depends on answering with something that cannot be used.
func BadGateway(message string) *HTTPError {
	return statusError(http.StatusBadGateway, message)
}

// ServiceUnavailable returns a 503 classified [CodeServiceUnavailable], for a
// dependency that is down or an application that is shedding load.
func ServiceUnavailable(message string) *HTTPError {
	return statusError(http.StatusServiceUnavailable, message)
}

// GatewayTimeout returns a 504 classified [CodeGatewayTimeout], for a service
// this request depends on not answering in time.
func GatewayTimeout(message string) *HTTPError {
	return statusError(http.StatusGatewayTimeout, message)
}
