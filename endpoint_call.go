package muzak

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strings"
)

// CallOption adjusts one call made with [Endpoint.Call].
type CallOption interface {
	applyCall(*callConfig)
}

// callOptionFunc adapts a function into a [CallOption].
type callOptionFunc func(*callConfig)

func (f callOptionFunc) applyCall(c *callConfig) { f(c) }

// CallHeader adds a header the endpoint's input does not bind, such as the
// Authorization a guard reads or an Idempotency-Key that lets a POST be
// retried:
//
//	item, err := api.CreateItem.Call(ctx, client, in,
//		muzak.CallHeader("Authorization", "Bearer "+token),
//		muzak.CallHeader("Idempotency-Key", key))
//
// A header the input binds is set through its field, and naming it here is
// refused, as are Host, Content-Length, Content-Type and the other headers a
// call writes itself, so that two values never compete. A Cookie given here
// is sent after the cookies the input binds. The value is held to the rule a
// bound header's is, so one holding a line break or a space at either end is
// refused rather than sent altered. Headers that apply to every call, such as
// a tenant derived from the context, belong in [ClientOptions.Propagate].
func CallHeader(name, value string) CallOption {
	return callOptionFunc(func(c *callConfig) { c.headers = append(c.headers, [2]string{name, value}) })
}

// ValidateFirst checks the input before it is sent, as the server checks it
// once it is bound: a required parameter, form field or file with no value,
// and the rules of an input that implements [Validatable]. A failure is
// returned as the [*ValidationError] the server would have answered 422 with,
// and nothing is sent.
//
// It is off unless asked for, because the server is the authority on what it
// accepts, and the two can disagree. A client built from an older version of
// the shared package holds the rules of that version, and would refuse what
// the server now accepts until it is rebuilt. A rule that reads a [Dep]
// reads its zero value here, since dependencies are resolved on the server.
// A rule that rewrites the value, such as Trim, rewrites what is sent, as the
// server would rewrite what it received. A route declared with
// [SkipValidation] skips the rules here too. Turn it on where a round trip is
// worth saving and both sides ship together.
func ValidateFirst() CallOption {
	return callOptionFunc(func(c *callConfig) { c.validate = true })
}

// Call sends in to the endpoint at the client's [ClientOptions.BaseURL] and
// returns what the handler answered, decoded into Out:
//
//	users := muzak.NewClient(muzak.ClientOptions{
//		AllowPrivateNetworks: true,
//		BaseURL:              "http://users.internal:8080",
//	})
//	user, err := userapi.GetUser.Call(ctx.Context(), users, userapi.GetUserIn{ID: "42"})
//
// The request is the exact inverse of binding: every field of in goes where
// its tag says the server reads it from, written so that the server's binder
// reads back a value equal to in. Path parameters are escaped segment by
// segment; a query parameter is sent once, or once per entry for a list; a
// header list is one comma-separated line; a cookie is quoted where the
// cookie parser needs it; the JSON body holds the body members alone, never
// a located field or a [Dep]; and an input with form or file fields is sent as
// multipart/form-data. Numbers are written in the decimal grammar the binder
// reads, and a time, a UUID or any type that reads itself with UnmarshalText
// writes itself with MarshalText.
//
// Some values have no request that carries them unchanged, and are refused
// before anything is sent with an error wrapping [ErrCallRefused] that names
// the field and the reason, never the value: a path parameter that is empty,
// "." or "..", or that holds a "/" outside a trailing {name...} parameter; a
// header with a line break or another control character in it, or a space at
// either end; a header list entry holding a comma; a cookie outside the
// characters a cookie may carry; a number that is not finite; a body string
// that is not valid UTF-8; a body member with a default that the omitzero or
// omitempty option of its json tag leaves out, which would arrive as the
// default. A nil pointer or an empty list is not sent at all,
// so the server reads it as absent, which is its default if it has one. The
// input type itself is checked on the first call, and a type the server binds
// but a call cannot write, such as one with UnmarshalText and no MarshalText
// or a [File] field, fails every call with an error saying what to change.
//
// The answer is decoded as follows. A JSON output is decoded with
// encoding/json/v2, members Out does not have ignored, as a client reading
// another service should; a 204 decodes to the zero Out. [Bytes] returns the
// body with its media type and the file name of its Content-Disposition, and
// [Stream] returns the body unread, as an io.ReadCloser the caller must
// close, which a handler can return as its own [Stream] to pass it on. A
// [Redirect] output returns the redirect itself, To and Status, rather than
// following it, since following it would answer with another endpoint's
// output. [HTML] returns the document and [Empty] nothing. Every body is
// bounded by [ClientOptions.MaxResponseBytes]. An endpoint answering with a
// [FileResponse] cannot be called: declare it with [Stream].
//
// A status outside 2xx is returned as a [*RemoteError] whose Code, Message,
// Details and RequestID are read from the error envelope the service sent,
// the default one or problem details. A failure to reach the service is
// returned as [Client.Do] returns it. The call is retried, redirected and
// bounded as every request the client sends; a POST is retried only with an
// Idempotency-Key, which [CallHeader] adds. The headers
// [ClientOptions.Propagate] adds are added after the input is written, so a
// field bound to one of them, such as X-Request-Id, and left out receives
// the propagated value.
func (ep Endpoint[In, Out]) Call(ctx context.Context, c *Client, in In, opts ...CallOption) (Out, error) {
	var zero Out
	def := ep.def
	switch {
	case def == nil:
		return zero, errNoEndpoint
	case def.err != nil:
		return zero, def.err
	case c == nil:
		return zero, fmt.Errorf("muzak: %s %s was called with a nil *Client; build one with NewClient", def.method, def.path)
	case c.baseURL == nil:
		return zero, fmt.Errorf("muzak: %s %s was called with a client that has no BaseURL; set ClientOptions.BaseURL to the root the service is served under", def.method, def.path)
	}
	plan, err := def.compile()
	if err != nil {
		return zero, err
	}
	var cfg callConfig
	for _, opt := range opts {
		opt.applyCall(&cfg)
	}
	encoded, err := plan.encode(reflect.ValueOf(&in).Elem(), &cfg)
	if err != nil {
		return zero, err
	}
	req, err := plan.request(ctx, c.baseURL, encoded)
	if err != nil {
		return zero, err
	}
	resp, err := c.Do(req) //nolint:bodyclose // decodeOutput closes it, or hands it to the caller inside a Stream
	if err != nil {
		return zero, err
	}
	return decodeOutput[Out](resp, plan)
}

// request turns an encoded input into the request sent to base.
func (p *callPlan) request(ctx context.Context, base *url.URL, encoded *encodedRequest) (*http.Request, error) {
	target := *base
	// The base path is escaped already, and so is the endpoint's, so they
	// are joined as escaped text and both forms of the path set from it.
	// net/http then writes RawPath, which keeps an escaped character in a
	// parameter escaped on the wire.
	target.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + encoded.path
	path, err := url.PathUnescape(target.RawPath)
	if err != nil {
		// coverage: both halves were escaped by net/url, and PathUnescape
		// reads back whatever PathEscape and EscapedPath write.
		return nil, fmt.Errorf("muzak: %s %s: the path could not be built: %w", p.method, p.path, err)
	}
	target.Path = path
	target.RawQuery = encoded.query
	var body io.Reader
	if encoded.body != nil {
		body = bytes.NewReader(encoded.body)
	}
	req, err := http.NewRequestWithContext(ctx, p.method, target.String(), body)
	if err != nil {
		// The URL was built from parts already checked, so what is left to
		// fail is what the caller passed, a nil context.
		return nil, fmt.Errorf("muzak: %s %s: the request could not be built: %w", p.method, p.path, err)
	}
	req.Header = encoded.header
	if encoded.contentType != "" {
		req.Header.Set("Content-Type", encoded.contentType)
	}
	// Not when the input binds Accept itself: one it left out must arrive
	// absent, as it was sent.
	if !p.headers["Accept"] && req.Header.Get("Accept") == "" && p.output == outputEncoded && !p.html && !p.empty {
		req.Header.Set("Accept", "application/json")
	}
	if p.output == outputRedirect {
		req = req.WithContext(context.WithValue(req.Context(), keepRedirectKey{}, true))
	}
	return req, nil
}

// keepRedirectKey marks the context of a call whose output is a [Redirect],
// which wants the redirect the server answered with rather than whatever it
// leads to.
type keepRedirectKey struct{}

// keepsRedirect reports whether a redirect of req is to be returned rather
// than followed; see [Client.checkRedirect].
func keepsRedirect(req *http.Request) bool {
	return req.Context().Value(keepRedirectKey{}) != nil
}

// clientBaseURL parses [ClientOptions.BaseURL], panicking on one that cannot
// be a base, as [NewClient] does for its other options.
func clientBaseURL(raw string) *url.URL {
	if raw == "" {
		return nil
	}
	base, err := url.Parse(raw)
	why := ""
	switch {
	case err != nil:
		why = "is not a URL"
	case base.Scheme != "http" && base.Scheme != "https":
		why = "is not an absolute http or https URL"
	case base.Host == "":
		why = "names no host"
	case base.User != nil:
		why = "carries user information, which would be sent with every call; send credentials in a header with CallHeader or Propagate"
	case base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || strings.Contains(raw, "#"):
		why = "carries a query or a fragment, which a call's own would collide with"
	}
	if why != "" {
		panic(fmt.Sprintf("muzak: ClientOptions.BaseURL %q %s; write it as the root the service is served under, such as %q",
			clientShorten(raw), why, "http://users.internal:8080/api"))
	}
	return base
}

// decodeOutput reads the answer to a call into a value of the endpoint's
// output type, closing the response unless it is handed on inside a [Stream].
func decodeOutput[Out any](resp *http.Response, plan *callPlan) (out Out, err error) {
	method, origin := clientMethod(resp.Request), clientOrigin(resp.Request.URL)
	if plan.output != outputStream {
		defer func() { _ = resp.Body.Close() }()
	}
	if plan.output == outputRedirect && resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		location := resp.Header.Get("Location")
		if location == "" {
			return out, fmt.Errorf("muzak: %s %s answered with status %d and no Location to redirect to", method, origin, resp.StatusCode)
		}
		*any(&out).(*Redirect) = Redirect{To: location, Status: resp.StatusCode}
		return out, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if plan.output == outputStream {
			defer func() { _ = resp.Body.Close() }()
		}
		return out, newRemoteError(resp, method, origin)
	}
	switch plan.output {
	case outputRedirect:
		return out, fmt.Errorf("muzak: %s %s answered with status %d where the endpoint redirects", method, origin, resp.StatusCode)
	case outputStream:
		stream := Stream{ContentType: resp.Header.Get("Content-Type"), Body: resp.Body}
		if resp.ContentLength > 0 {
			stream.Length = resp.ContentLength
		}
		stream.Filename, stream.Download = readDisposition(resp.Header)
		*any(&out).(*Stream) = stream
		return out, nil
	}
	if plan.empty || !responseHasBody(resp) {
		return out, nil
	}
	if plan.output == outputBytes || plan.html {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return out, err
		}
		if plan.html {
			*any(&out).(*HTML) = HTML(data)
			return out, nil
		}
		value := Bytes{ContentType: resp.Header.Get("Content-Type"), Data: data}
		value.Filename, value.Download = readDisposition(resp.Header)
		*any(&out).(*Bytes) = value
		return out, nil
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
		return out, fmt.Errorf("muzak: %s %s answered with %q rather than JSON", method, origin, clientShorten(resp.Header.Get("Content-Type")))
	}
	if err := json.UnmarshalRead(resp.Body, &out, durationJSON); err != nil {
		if errors.Is(err, ErrResponseTooLarge) {
			return out, err
		}
		var zero Out
		return zero, fmt.Errorf("muzak: the response from %s %s is not JSON that fits %T: %w", method, origin, out, err)
	}
	return out, nil
}

// readDisposition reads the file name a Content-Disposition offers and
// whether it asks for the body to be saved, as [Bytes] and [Stream] carry
// them. mime reads the RFC 8187 filename* a name outside ASCII is sent in, and
// prefers it to the ASCII filename beside it, as RFC 6266 asks.
func readDisposition(h http.Header) (filename string, download bool) {
	disposition, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil {
		return "", false
	}
	return params["filename"], disposition == "attachment"
}
