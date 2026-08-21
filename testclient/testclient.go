package testclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"badele"
)

// Client issues requests against an application served in-process.
//
// Create one with [New]; the zero Client is not usable. A Client is safe for
// concurrent use, which lets a test fire parallel requests to check that
// request-scoped state stays isolated.
type Client struct {
	tb      testing.TB
	server  *httptest.Server
	http    *http.Client
	headers http.Header
}

// Option configures a [Client] at creation.
type Option func(*config)

// config accumulates client-level settings.
type config struct {
	headers   http.Header
	timeout   time.Duration
	noCookies bool
	noRedirect
}

// noRedirect is a named bool so that the option that sets it reads clearly at
// the point of use.
type noRedirect bool

// WithHeader adds a header sent with every request, which is how a test avoids
// repeating an authentication token on each call.
func WithHeader(name, value string) Option {
	return func(c *config) { c.headers.Add(name, value) }
}

// WithTimeout bounds how long a single request may take, defaulting to ten
// seconds. A test that hangs is far less useful than one that fails.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// WithoutCookies disables the cookie jar, so that each request is independent.
func WithoutCookies() Option {
	return func(c *config) { c.noCookies = true }
}

// WithoutRedirects stops the client from following redirects, so that a test
// can assert on the 3xx response itself.
func WithoutRedirects() Option {
	return func(c *config) { c.noRedirect = true }
}

// New starts app on a local listener and returns a client for it.
//
// The application is built, its lifecycle components are started, and both the
// server and those components are released through tb.Cleanup when the test
// finishes. A build failure or a component that refuses to start fails the
// test immediately, because every later assertion would be meaningless.
func New(tb testing.TB, app *badele.App, opts ...Option) *Client {
	tb.Helper()
	cfg := config{headers: http.Header{}, timeout: 10 * time.Second}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := app.Build(); err != nil {
		tb.Fatalf("testclient: the application could not be built: %v", err)
	}
	if err := app.StartLifecycle(context.Background()); err != nil {
		tb.Fatalf("testclient: the lifecycle components could not be started: %v", err)
	}
	tb.Cleanup(func() {
		if err := app.StopLifecycle(context.Background()); err != nil {
			tb.Errorf("testclient: the lifecycle components could not be stopped: %v", err)
		}
	})

	server := httptest.NewTestServer(tb, app)
	client := &Client{
		tb:      tb,
		server:  server,
		headers: cfg.headers,
		http:    &http.Client{Timeout: cfg.timeout},
	}
	if !cfg.noCookies {
		jar, err := cookiejar.New(nil)
		if err != nil {
			// coverage: cookiejar.New only fails on a malformed public suffix
			// list, which the nil options value never supplies.
			tb.Fatalf("testclient: the cookie jar could not be created: %v", err)
		}
		client.http.Jar = jar
	}
	if bool(cfg.noRedirect) {
		client.http.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

// URL returns the base URL the application is served from, for the rare test
// that needs to build a request by hand.
func (c *Client) URL() string { return c.server.URL }

// HTTPClient returns the underlying *http.Client, whose cookie jar carries the
// session established by earlier requests.
func (c *Client) HTTPClient() *http.Client { return c.http }

// RequestOption adjusts a single request.
type RequestOption func(*request)

// request accumulates what one call should send.
type request struct {
	header http.Header
	query  url.Values
	body   io.Reader
	err    error
}

// Header sets a header on this request, replacing any client-level value of
// the same name.
func Header(name, value string) RequestOption {
	return func(r *request) { r.header.Set(name, value) }
}

// Query adds a query parameter, which may be repeated to send several values
// under one name.
func Query(name, value string) RequestOption {
	return func(r *request) { r.query.Add(name, value) }
}

// Cookie sends a cookie with this request, in addition to whatever the jar
// already holds.
func Cookie(cookie *http.Cookie) RequestOption {
	return func(r *request) { r.header.Add("Cookie", cookie.String()) }
}

// JSON sends value as a JSON request body and sets the Content-Type header.
// A value that cannot be encoded fails the test when the request is issued.
func JSON(value any) RequestOption {
	return func(r *request) {
		encoded, err := json.Marshal(value)
		if err != nil {
			r.err = err
			return
		}
		r.header.Set("Content-Type", "application/json")
		r.body = bytes.NewReader(encoded)
	}
}

// RawJSON sends body verbatim as a JSON request body, for testing what the
// server does with JSON a Go value could not produce, such as a duplicate
// member or a malformed document.
func RawJSON(body string) RequestOption {
	return func(r *request) {
		r.header.Set("Content-Type", "application/json")
		r.body = strings.NewReader(body)
	}
}

// Body sends an arbitrary body under the given content type.
func Body(contentType string, body io.Reader) RequestOption {
	return func(r *request) {
		if contentType != "" {
			r.header.Set("Content-Type", contentType)
		}
		r.body = body
	}
}

// Get issues a GET request.
func (c *Client) Get(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodGet, path, opts...)
}

// Post issues a POST request. Pass [JSON] to send a body.
func (c *Client) Post(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodPost, path, opts...)
}

// Put issues a PUT request.
func (c *Client) Put(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodPut, path, opts...)
}

// Patch issues a PATCH request.
func (c *Client) Patch(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodPatch, path, opts...)
}

// Delete issues a DELETE request.
func (c *Client) Delete(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodDelete, path, opts...)
}

// Head issues a HEAD request.
func (c *Client) Head(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodHead, path, opts...)
}

// Options issues an OPTIONS request.
func (c *Client) Options(path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	return c.Do(http.MethodOptions, path, opts...)
}

// Do issues a request with an arbitrary method.
//
// The path is relative to the served application and may carry its own query
// string, which [Query] adds to. Any transport-level failure fails the test,
// because it means the request never reached the application.
func (c *Client) Do(method, path string, opts ...RequestOption) *Response {
	c.tb.Helper()
	req := &request{header: c.headers.Clone(), query: url.Values{}}
	if req.header == nil {
		req.header = http.Header{}
	}
	for _, opt := range opts {
		opt(req)
	}
	if req.err != nil {
		c.tb.Fatalf("testclient: %s %s: the request body could not be encoded: %v", method, path, req.err)
	}

	target := c.server.URL + path
	if len(req.query) > 0 {
		separator := "?"
		if strings.Contains(path, "?") {
			separator = "&"
		}
		target += separator + req.query.Encode()
	}

	httpReq, err := http.NewRequestWithContext(c.tb.Context(), method, target, req.body)
	if err != nil {
		c.tb.Fatalf("testclient: %s %s could not be built: %v", method, path, err)
	}
	httpReq.Header = req.header

	res, err := c.http.Do(httpReq)
	if err != nil {
		c.tb.Fatalf("testclient: %s %s failed: %v", method, path, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		c.tb.Fatalf("testclient: %s %s: the response body could not be read: %v", method, path, err)
	}
	return &Response{
		tb:      c.tb,
		method:  method,
		path:    path,
		Status:  res.StatusCode,
		Header:  res.Header,
		Body:    body,
		Cookies: res.Cookies(),
	}
}

// Response is a completed request, read fully into memory so that it can be
// inspected more than once.
type Response struct {
	// Status is the HTTP status code.
	Status int
	// Header holds the response headers.
	Header http.Header
	// Body is the raw response body.
	Body []byte
	// Cookies lists the cookies the response set.
	Cookies []*http.Cookie

	tb     testing.TB
	method string
	path   string
}

// String returns the response body as text, which is what a failure message
// wants to show.
func (r *Response) String() string { return string(r.Body) }

// RequestID returns the identifier the server assigned to the request, taken
// from the X-Request-Id response header.
func (r *Response) RequestID() string { return r.Header.Get(badele.HeaderRequestID) }

// JSON decodes the response body into target, failing the test if it is not
// valid JSON or does not fit.
func (r *Response) JSON(target any) {
	r.tb.Helper()
	if err := json.Unmarshal(r.Body, target); err != nil {
		r.tb.Fatalf("%s %s: the response body is not JSON that fits %T: %v\nbody: %s",
			r.method, r.path, target, err, r.Body)
	}
}

// Decode decodes the response body into a value of type T and returns it.
//
// The type argument is written at the call site, which keeps the expected
// shape visible in the test and checked by the compiler:
//
//	item := client.Get("/items/foo").Decode[ItemOut]()
func (r *Response) Decode[T any]() T {
	r.tb.Helper()
	var out T
	r.JSON(&out)
	return out
}

// Decoded decodes a response into a value of type T. It is the free-function
// form of [Response.Decode], for chaining directly off a request:
//
//	item := testclient.Decoded[ItemOut](client.Get("/items/foo"))
func Decoded[T any](r *Response) T {
	r.tb.Helper()
	return r.Decode[T]()
}

// Error decodes the response body as Badele's standard error envelope, failing
// the test if it does not fit.
func (r *Response) Error() badele.ErrorResponse {
	r.tb.Helper()
	return r.Decode[badele.ErrorResponse]()
}

// AssertStatus fails the test unless the response carried the wanted status.
// It returns the response so assertions can be chained.
func (r *Response) AssertStatus(want int) *Response {
	r.tb.Helper()
	if r.Status != want {
		r.tb.Errorf("%s %s: status = %d, want %d\nbody: %s", r.method, r.path, r.Status, want, r.Body)
	}
	return r
}

// AssertHeader fails the test unless the named response header has the wanted
// value.
func (r *Response) AssertHeader(name, want string) *Response {
	r.tb.Helper()
	if got := r.Header.Get(name); got != want {
		r.tb.Errorf("%s %s: header %s = %q, want %q", r.method, r.path, name, got, want)
	}
	return r
}

// AssertJSON fails the test unless the response body is JSON equal to want.
//
// Comparison is semantic rather than textual: member order and insignificant
// whitespace are ignored, so the expectation can be written readably.
func (r *Response) AssertJSON(want string) *Response {
	r.tb.Helper()
	var got, expected any
	if err := json.Unmarshal(r.Body, &got); err != nil {
		r.tb.Errorf("%s %s: the response body is not valid JSON: %v\nbody: %s", r.method, r.path, err, r.Body)
		return r
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		r.tb.Errorf("%s %s: the expected value is not valid JSON: %v", r.method, r.path, err)
		return r
	}
	if !reflect.DeepEqual(got, expected) {
		r.tb.Errorf("%s %s: response body mismatch\n got: %s\nwant: %s", r.method, r.path, r.Body, want)
	}
	return r
}

// AssertErrorCode fails the test unless the response is an error envelope
// carrying the wanted machine-readable code, such as "validation_error".
func (r *Response) AssertErrorCode(want string) *Response {
	r.tb.Helper()
	envelope := r.Error()
	if envelope.Error.Code != want {
		r.tb.Errorf("%s %s: error code = %q, want %q\nbody: %s",
			r.method, r.path, envelope.Error.Code, want, r.Body)
	}
	return r
}
