package muzak

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// maxRemoteErrorBody bounds how much of a refused response a [RemoteError]
// keeps, which is enough to read what went wrong and no more.
const maxRemoteErrorBody = 64 << 10

// RemoteError reports a response whose status was not a success, from the
// helpers that decode one: [Client.DoJSON], [Client.GetJSON] and
// [Client.PostJSON]. Read the status with [errors.As]:
//
//	var remote *muzak.RemoteError
//	if errors.As(err, &remote) && remote.StatusCode == http.StatusNotFound {
//		return Out{}, muzak.NotFound("no such item upstream")
//	}
//
// Returning it from a handler as it is renders an opaque 500, which is the
// right answer for an upstream failure the handler did not anticipate.
type RemoteError struct {
	// StatusCode is the status the server answered with.
	StatusCode int
	// Header is the response's header, where a Retry-After or a
	// WWW-Authenticate challenge is found.
	Header http.Header
	// Body is the start of the response body, at most 64 KiB of it, for
	// diagnosis. It is another server's account of its own failure: log it
	// with care, and do not pass it on to a client of your own as it is.
	Body []byte

	// Code, Message, Details and RequestID are read from Body when it is an
	// error envelope a Muzak application writes, the default [ErrorResponse]
	// or an RFC 9457 [Problem], and are empty otherwise; see
	// endpoint_remote.go for how a body is judged. Code is what to branch on.
	// Message and the issues in Details are the remote server's text, bounded
	// in size and count but otherwise as it sent them, so the same care
	// applies to them as to Body.
	Code      string
	Message   string
	Details   []ErrorDetail
	RequestID string

	method string
	origin string
}

// Error implements the error interface. It names the method, the origin and
// the status, and leaves the body out, since the body is the remote server's
// text and an error message is read by whoever reads the log.
func (e *RemoteError) Error() string {
	if e.Code != "" && isPlainCode(e.Code) {
		// The code is the one part of the body worth a place in a log line,
		// and it is admitted only as a plain identifier; see isPlainCode.
		return fmt.Sprintf("muzak: %s %s answered with status %d (%s)", e.method, e.origin, e.StatusCode, e.Code)
	}
	return fmt.Sprintf("muzak: %s %s answered with status %d", e.method, e.origin, e.StatusCode)
}

// DoJSON sends a request with [Client.Do] and decodes its JSON response into
// a value of type T, written at the call site:
//
//	item, err := client.DoJSON[ItemOut](req)
//
// A response whose status is not 2xx is returned as a [*RemoteError]. A 204,
// or the answer to a HEAD request, decodes to the zero T. A success that is
// not labelled as JSON is refused rather than guessed at, which is how an
// HTML error page from a proxy in the way announces itself. The body is
// bounded by [ClientOptions.MaxResponseBytes], and decoded by
// encoding/json/v2, which refuses duplicate members and invalid UTF-8 and
// ignores members T does not have, as a client reading another service's
// answers should.
//
// An Accept header is added when the request has none, on a copy.
func (c *Client) DoJSON[T any](req *http.Request) (T, error) {
	var out T
	if req != nil && req.Header.Get("Accept") == "" {
		req = req.Clone(req.Context())
		if req.Header == nil {
			req.Header = make(http.Header)
		}
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	method, origin := clientMethod(resp.Request), clientOrigin(resp.Request.URL)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return out, newRemoteError(resp, method, origin)
	}
	if !responseHasBody(resp) {
		return out, nil
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return out, fmt.Errorf("muzak: %s %s answered with %q rather than JSON; read it with Client.Do instead",
			method, origin, clientShorten(resp.Header.Get("Content-Type")))
	}
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return out, err
		}
		return out, fmt.Errorf("muzak: the response from %s %s is not JSON that fits %T: %w", method, origin, out, err)
	}
	return out, nil
}

// GetJSON fetches a URL and decodes its JSON response into a value of type T,
// as [Client.DoJSON] does:
//
//	item, err := client.GetJSON[ItemOut](ctx.Context(), "https://api.example.com/items/1")
func (c *Client) GetJSON[T any](ctx context.Context, rawURL string) (T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		var zero T
		return zero, clientURLError(err)
	}
	return c.DoJSON[T](req)
}

// PostJSON posts a value encoded as JSON and decodes the JSON response into a
// value of type T, as [Client.DoJSON] does.
//
// A POST is not retried, since sending it twice may do twice what it does.
// Build the request yourself, with an Idempotency-Key header the server
// honours, and send it with DoJSON when a retry is safe.
func (c *Client) PostJSON[T any](ctx context.Context, rawURL string, body any) (T, error) {
	var zero T
	encoded, err := json.Marshal(body)
	if err != nil {
		return zero, fmt.Errorf("muzak: the request body could not be encoded as JSON: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(encoded))
	if err != nil {
		return zero, clientURLError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.DoJSON[T](req)
}

// clientURLError reports a URL that could not be turned into a request,
// without the URL itself, which net/url would otherwise quote whole.
func clientURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return fmt.Errorf("muzak: the request URL could not be parsed: %w", err)
}
