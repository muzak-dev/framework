package muzak

import (
	"encoding/json/v2"
	"io"
	"mime"
	"net/http"
	"strings"
)

// maxRemoteDetails bounds how many details a [RemoteError] keeps from an
// envelope, which is as many as a Muzak application ever sends; see
// [MaxValidationDetails]. A body is already bounded at 64 KiB, so this bounds
// the slice, not the reading.
const maxRemoteDetails = MaxValidationDetails

// maxRemoteCode bounds the code a [RemoteError] repeats in its message.
const maxRemoteCode = 64

// newRemoteError reads what is worth keeping of a response whose status is not
// a success: the start of its body, and what that body says when it is an
// error envelope.
func newRemoteError(resp *http.Response, method, origin string) *RemoteError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRemoteErrorBody))
	e := &RemoteError{StatusCode: resp.StatusCode, Header: resp.Header, Body: body, method: method, origin: origin}
	e.readEnvelope(resp.Header.Get("Content-Type"))
	return e
}

// remoteEnvelope is the default error envelope, [ErrorResponse], as it is
// read: only what a RemoteError keeps.
type remoteEnvelope struct {
	Error struct {
		Code    string        `json:"code"`
		Message string        `json:"message"`
		Details []ErrorDetail `json:"details"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

// remoteProblem is an RFC 9457 problem as a Muzak application writes one,
// [Problem], as it is read.
type remoteProblem struct {
	Title     string        `json:"title"`
	Detail    string        `json:"detail"`
	Code      string        `json:"code"`
	Errors    []ErrorDetail `json:"errors"`
	RequestID string        `json:"request_id"`
}

// readEnvelope fills Code, Message, Details and RequestID from the body, when
// it is one of the two envelopes a Muzak application writes.
//
// The body is another server's, so nothing about it is assumed. It is read
// only under a JSON media type, and as the envelope the media type names:
// application/problem+json as a problem, any other JSON as the default
// envelope. It is decoded whole or not at all, so a body that is truncated at
// the 64 KiB a RemoteError keeps, that is not JSON, that gives a member the
// wrong type or that repeats one leaves every field empty rather than half
// filled. Members the envelope does not have are ignored. Reading is linear in
// the body, which is bounded, and at most [maxRemoteDetails] details are kept.
func (e *RemoteError) readEnvelope(contentType string) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || len(e.Body) == 0 {
		return
	}
	switch {
	case mediaType == ProblemContentType:
		var problem remoteProblem
		if json.Unmarshal(e.Body, &problem) != nil {
			return
		}
		e.Code, e.Message, e.Details, e.RequestID = problem.Code, problem.Detail, problem.Errors, problem.RequestID
		if e.Message == "" {
			e.Message = problem.Title
		}
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		var envelope remoteEnvelope
		if json.Unmarshal(e.Body, &envelope) != nil {
			return
		}
		e.Code, e.Message, e.Details, e.RequestID = envelope.Error.Code, envelope.Error.Message, envelope.Error.Details, envelope.RequestID
	}
	if len(e.Details) > maxRemoteDetails {
		e.Details = e.Details[:maxRemoteDetails:maxRemoteDetails]
	}
}

// isPlainCode reports whether a code is a plain identifier, letters, digits
// and "_", "-", "." or ":", of at most 64 bytes, which is the only form
// [RemoteError.Error] repeats. Any other text from the body could write a
// line of its own into a log, or run on for kilobytes.
func isPlainCode(code string) bool {
	if len(code) > maxRemoteCode {
		return false
	}
	for i := range len(code) {
		switch c := code[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}
