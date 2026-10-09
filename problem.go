package muzak

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"uuid"
)

// ProblemContentType is the media type of an RFC 9457 problem details body.
const ProblemContentType = "application/problem+json"

// problemTypeBlank is the problem type RFC 9457 defines for a problem that is
// described by its status code alone.
const problemTypeBlank = "about:blank"

// ProblemOptions configures RFC 9457 problem details as the application's
// error format; see [AppOptions.ProblemDetails] and [ProblemDetails].
type ProblemOptions struct {
	// TypeBase is the absolute URI each problem's "type" is built from, by
	// appending the error code, path-escaped: with
	// "https://errors.example.com/" a 404 is of type
	// "https://errors.example.com/not_found". The code is appended as it is,
	// so the base ends in whatever separates the two, a "/" or a ":". A
	// client compares types as strings, so the base should be one the
	// application keeps; serving documentation at each type URI is what the
	// RFC recommends but not something Muzak does.
	//
	// When it is empty every problem's type is "about:blank", which the RFC
	// defines as a problem described by its status code alone, and a client
	// branches on the "code" member instead.
	//
	// A base that is not an absolute URI, or that carries a query, a
	// fragment, whitespace or a character outside ASCII, is a build error.
	TypeBase string
}

// validate reports a TypeBase that cannot be the start of a problem type.
func (o *ProblemOptions) validate() error {
	if o == nil || o.TypeBase == "" {
		return nil
	}
	base := o.TypeBase
	why := ""
	parsed, err := url.Parse(base)
	switch {
	case !isPrintableASCII(base) || strings.Contains(base, " "):
		why = "holds whitespace, a control character or a character outside ASCII"
	case err != nil || !parsed.IsAbs():
		why = "is not an absolute URI, so a client would resolve it against wherever it read the response"
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(base, "#"):
		why = "carries a query or a fragment, which the code appended to it would end up inside"
	}
	if why == "" {
		return nil
	}
	return fmt.Errorf("muzak: ProblemDetails.TypeBase %q %s; write it as an absolute URI the codes are appended to, such as %q",
		base, why, "https://errors.example.com/")
}

// Problem is the body of an error response in the format RFC 9457 defines,
// "Problem Details for HTTP APIs", sent as [ProblemContentType]:
//
//	{
//	  "type": "about:blank",
//	  "title": "Unprocessable Entity",
//	  "status": 422,
//	  "detail": "The request could not be validated.",
//	  "instance": "urn:uuid:0611f4b2-2f0a-7b57-9c1a-6e6a2e2f9b31",
//	  "code": "validation_error",
//	  "errors": [
//	    { "field": "name", "location": "body", "issue": "is required" }
//	  ],
//	  "request_id": "0611f4b2-2f0a-7b57-9c1a-6e6a2e2f9b31"
//	}
//
// The five members the RFC defines come first. "code", "errors" and
// "request_id" are extension members carrying what the default envelope,
// [ErrorResponse], carries, so a client moving from one to the other loses
// nothing: the stable code to branch on, every field that failed, and the
// identifier that joins the response to the server's log.
type Problem struct {
	// Type is a URI identifying the kind of problem, built from
	// [ProblemOptions.TypeBase] and Code, or "about:blank".
	Type string `json:"type"`
	// Title is the standard reason phrase of the status, such as "Not Found",
	// in the request's locale when a translation of it exists.
	Title string `json:"title,omitzero"`
	// Status repeats the HTTP status code.
	Status int `json:"status"`
	// Detail explains this occurrence of the problem. It is the message the
	// default envelope would carry, and is held to the same rule: safe to
	// show, and never derived from a server-side fault.
	Detail string `json:"detail,omitzero"`
	// Instance identifies this occurrence: the request identifier as a URN,
	// "urn:uuid:" followed by it. It is never the request's path, which can
	// hold personal data a problem report should not repeat.
	Instance string `json:"instance,omitzero"`
	// Code is the machine-readable classifier a client branches on, such as
	// "not_found"; see [CodeForStatus].
	Code string `json:"code"`
	// Errors lists every field that failed, as the default envelope's
	// "details" does. It is omitted when there are none.
	Errors []ErrorDetail `json:"errors,omitzero"`
	// RequestID correlates the response with the server's log entry for the
	// same request.
	RequestID string `json:"request_id,omitzero"`
}

// ProblemDetails returns an [ErrorRenderer] that answers every error with an
// RFC 9457 problem details body, a [Problem], sent as [ProblemContentType].
//
// It decides everything [DefaultErrorRenderer] decides, by asking it, so the
// two make the same promises: the same status and code for every error, the
// same details, the same translated messages, and the same opacity for a
// server-side fault, whose text is replaced with a fixed sentence while the
// error itself is logged and never sent. Only the shape differs. Headers set
// before the error was rendered are kept as they are for the default
// envelope, Retry-After, Allow and WWW-Authenticate among them.
//
// [AppOptions.ProblemDetails] installs it and also tells the OpenAPI document
// to describe errors as problems, which is the usual way to use it. Call it
// directly to compose it with a renderer of your own, and set
// AppOptions.ProblemDetails as well, so the document says what the renderer
// sends.
func ProblemDetails(opts ProblemOptions) ErrorRenderer {
	base := opts.TypeBase
	return func(ctx *Context, err error) (int, any) {
		status, body := DefaultErrorRenderer(ctx, err)
		envelope, _ := body.(ErrorResponse)
		problem := Problem{
			Type:      problemTypeBlank,
			Title:     problemTitle(ctx, status),
			Status:    status,
			Detail:    envelope.Error.Message,
			Instance:  problemInstance(envelope.RequestID),
			Code:      envelope.Error.Code,
			Errors:    envelope.Error.Details,
			RequestID: envelope.RequestID,
		}
		if base != "" && problem.Code != "" {
			problem.Type = base + url.PathEscape(problem.Code)
		}
		if ctx.w != nil {
			// Set rather than left to the writer, which sets JSON's own type
			// only where none was set; the framework clears any type a
			// handler set for its success response before a renderer runs.
			ctx.w.Header().Set("Content-Type", ProblemContentType)
		}
		return status, problem
	}
}

// writeMinimalError writes the fixed 500 that stands in when no [Context] is
// at hand, for a panic outside a route, a renderer whose body could not be
// encoded or an application that did not build, in the format the application
// renders its errors in: the [ErrorResponse] envelope by default, and a
// [Problem] when [AppOptions.ProblemDetails] is set, so that a client parsing
// problems is never handed the envelope for the one error it did not expect.
// Like the envelope it is fixed English, carrying nothing of the failure.
func (a *App) writeMinimalError(w http.ResponseWriter, requestID string) {
	if a.opts.ProblemDetails == nil {
		writeMinimalError(w, requestID)
		return
	}
	if rw, ok := w.(*responseWriter); ok && rw.written {
		abortStartedResponse(rw)
		return
	}
	problem := Problem{
		Type:      problemTypeBlank,
		Title:     http.StatusText(http.StatusInternalServerError),
		Status:    http.StatusInternalServerError,
		Detail:    internalMessage,
		Instance:  problemInstance(requestID),
		Code:      CodeInternalError,
		RequestID: requestID,
	}
	if base := a.opts.ProblemDetails.TypeBase; base != "" {
		problem.Type = base + CodeInternalError
	}
	body, err := json.Marshal(problem)
	if err != nil {
		// coverage: Problem holds strings, an int and a nil slice here, so
		// marshaling it cannot fail; the envelope stands in all the same.
		writeMinimalError(w, requestID)
		return
	}
	resetForError(w.Header(), "")
	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(body)
}

// problemTitle is the reason phrase of a status, in the request's locale when
// the application translates muzak.status.<code>.
func problemTitle(ctx *Context, status int) string {
	return ctx.message("muzak.status."+strconv.Itoa(status), http.StatusText(status))
}

// problemInstance names one occurrence of a problem by its request
// identifier, as a URN of the registered uuid namespace, which is what every
// identifier [RequestID] assigns is. An identifier that is not a UUID, from a
// request identification of the application's own, is left out rather than
// written under a namespace it does not belong to.
func problemInstance(requestID string) string {
	if requestID == "" {
		return ""
	}
	if _, err := uuid.Parse(requestID); err != nil {
		return ""
	}
	return "urn:uuid:" + requestID
}
