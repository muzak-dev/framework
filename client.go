package muzak

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Defaults applied by [NewClient] when [ClientOptions] leaves them unset.
const (
	// DefaultClientTimeout bounds one call to [Client.Do] from start to
	// finish, at thirty seconds: every attempt, every redirect, every wait
	// between attempts and the reading of the response body.
	DefaultClientTimeout = 30 * time.Second
	// DefaultClientDialTimeout bounds resolving a name and opening a
	// connection to it, at ten seconds.
	DefaultClientDialTimeout = 10 * time.Second
	// DefaultClientTLSHandshakeTimeout bounds a TLS handshake, at ten seconds.
	DefaultClientTLSHandshakeTimeout = 10 * time.Second
	// DefaultClientResponseHeaderTimeout bounds the wait for a response's
	// header once the request has been written, at twenty seconds.
	DefaultClientResponseHeaderTimeout = 20 * time.Second
	// DefaultClientMaxResponseBytes is the largest response body a client
	// reads, at ten mebibytes, counted after any decompression.
	DefaultClientMaxResponseBytes int64 = 10 << 20
	// DefaultClientMaxRedirects is how many redirects one request follows.
	DefaultClientMaxRedirects = 5
	// DefaultClientMaxAttempts is how many times one request is sent at most,
	// the first time included, so two retries.
	DefaultClientMaxAttempts = 3
	// DefaultClientRetryBaseDelay is the ceiling of the first wait between
	// attempts, which doubles with each attempt after it.
	DefaultClientRetryBaseDelay = 100 * time.Millisecond
	// DefaultClientRetryMaxDelay is the most the doubling ceiling grows to.
	DefaultClientRetryMaxDelay = 2 * time.Second
	// DefaultClientMaxRetryAfter is the longest Retry-After a client waits
	// out before trying again, at ten seconds.
	DefaultClientMaxRetryAfter = 10 * time.Second
)

// clientMaxResponseHeaderBytes bounds a response's header, which net/http
// would otherwise let grow to ten mebibytes of whatever the server sends.
const clientMaxResponseHeaderBytes = 1 << 20

// clientDrainBytes is how much of a response that is being retried is read
// before it is closed, which lets its connection be used again. A server that
// sends more than this costs a new connection rather than a long read.
const clientDrainBytes = 64 << 10

// ClientOptions configures [NewClient].
//
// The zero value is a client fit to fetch a URL someone else chose: it refuses
// private, loopback, link-local and metadata addresses, follows at most five
// redirects and never from https down to http, reads at most ten mebibytes of
// a response, gives up after thirty seconds, and retries a request only when
// sending it twice is harmless.
type ClientOptions struct {
	// AllowPrivateNetworks lets the client connect to private, loopback,
	// link-local IPv6 and the other special-purpose ranges, which is what a
	// call from one service to another inside the same network needs.
	//
	// It is off by default because a client that can reach them is one a URL
	// supplied from outside can point at the database, the admin port on
	// localhost or the cache with no password, and that is the whole of a
	// request forgery. Leave it off for any URL that did not come from your
	// own configuration.
	//
	// It never opens a cloud metadata service, which holds the credentials of
	// the machine the client runs on: the IPv4 link-local block and the
	// metadata addresses of AWS, Azure, Google Cloud, Alibaba Cloud and Oracle
	// Cloud stay refused unless AllowedNetworks names them.
	AllowPrivateNetworks bool

	// AllowedNetworks lists networks the client may connect to whatever
	// else would refuse them, such as the one private subnet a service calls,
	// which is narrower than AllowPrivateNetworks. A prefix wide enough to
	// contain a metadata address opens that too.
	//
	// A prefix that is not valid, such as the zero netip.Prefix, panics in
	// NewClient, because a list with a hole in it does not mean what it says.
	AllowedNetworks []netip.Prefix

	// DeniedNetworks lists networks the client never connects to, whatever
	// AllowPrivateNetworks and AllowedNetworks say, such as a public range
	// that belongs to your own infrastructure.
	DeniedNetworks []netip.Prefix

	// Proxy sends every request through this proxy, an http, https or socks5
	// URL. Without it the client connects directly, and it never reads a
	// proxy from the environment: HTTP_PROXY is a setting anything that can
	// set a variable controls, and a proxy decides where requests go.
	//
	// The proxy itself is connected to without the address checks, and is
	// trusted to enforce them for the names it is asked to reach, since it is
	// the proxy that resolves them. The client still refuses a URL that names
	// a refused address literally, a localhost name, a name ending in a number
	// and a name outside ASCII, which are the forms it can judge without
	// resolving them. A proxy that does not enforce an egress policy of its
	// own reopens every request forgery this client otherwise closes.
	Proxy *url.URL

	// TLSConfig configures TLS for https requests, such as a private root of
	// trust or a client certificate. It is cloned, and a MinVersion below
	// TLS 1.2 is raised to it: 1.0 and 1.1 are deprecated by RFC 8996.
	TLSConfig *tls.Config

	// Timeout bounds one call to [Client.Do] from start to finish, defaulting
	// to [DefaultClientTimeout]: every attempt, every redirect, every wait
	// between attempts, and the reading of the response body, which runs on
	// after Do returns until the body is closed. The request's own context
	// bounds it as well. A negative value removes this bound, leaving only
	// the context's.
	Timeout time.Duration

	// DialTimeout bounds resolving a host and connecting to it, defaulting to
	// [DefaultClientDialTimeout]. A negative value removes the bound.
	DialTimeout time.Duration

	// TLSHandshakeTimeout bounds a TLS handshake, defaulting to
	// [DefaultClientTLSHandshakeTimeout]. A negative value removes the bound.
	TLSHandshakeTimeout time.Duration

	// ResponseHeaderTimeout bounds the wait for a response's header once the
	// request has been written, defaulting to
	// [DefaultClientResponseHeaderTimeout]. A server that accepts a request
	// and never answers costs this rather than the whole Timeout. A negative
	// value removes the bound.
	ResponseHeaderTimeout time.Duration

	// MaxResponseBytes is the largest response body the client reads,
	// defaulting to [DefaultClientMaxResponseBytes]. A response that declares
	// a larger Content-Length is refused before its body is read, and one that
	// does not declare one, or that is compressed, fails with
	// [ErrResponseTooLarge] on the read that passes the limit. It is counted
	// after decompression, so a small compressed body cannot expand into a
	// large one.
	MaxResponseBytes int64

	// MaxRedirects is how many redirects one request follows, defaulting to
	// [DefaultClientMaxRedirects]; one more is an error. A negative value
	// follows none and returns the redirect response itself, Location and
	// all, for the caller to judge.
	//
	// Every redirect is checked as the first request was, so one cannot lead
	// to a refused address, and none is followed from https to plain http,
	// which would send the request in the clear.
	MaxRedirects int

	// SensitiveHeaders lists headers, beyond Authorization, Cookie and
	// Proxy-Authorization, that are removed from a redirected request once
	// any redirect in its chain has left the origin the request was sent to,
	// such as an API key header. An origin is the scheme, host and port
	// together, so a redirect to a subdomain or from http to https leaves it
	// too.
	SensitiveHeaders []string

	// MaxAttempts is how many times one request is sent at most, the first
	// time included, defaulting to [DefaultClientMaxAttempts]. One turns
	// retrying off.
	//
	// A request is retried only when sending it twice is harmless: a GET,
	// HEAD, OPTIONS, PUT or DELETE, or any request that carries an
	// Idempotency-Key header. It is retried after a connection error, and
	// after a 429, 502, 503 or 504; the last such response is returned as it
	// is. A request whose body cannot be read a second time, because it has
	// no GetBody, is sent once.
	MaxAttempts int

	// RetryBaseDelay and RetryMaxDelay shape the wait between attempts,
	// defaulting to [DefaultClientRetryBaseDelay] and
	// [DefaultClientRetryMaxDelay]. The wait is a random duration between zero
	// and a ceiling that starts at RetryBaseDelay and doubles with each
	// attempt up to RetryMaxDelay, which keeps many clients that failed
	// together from retrying together.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration

	// MaxRetryAfter is the longest wait a Retry-After header is obeyed for,
	// defaulting to [DefaultClientMaxRetryAfter]. A response asking for
	// longer is returned to the caller rather than retried, since retrying
	// sooner than the server asked is what it asked not to have, and a
	// response whose wait would outlast the request's deadline is returned at
	// once rather than slept on.
	MaxRetryAfter time.Duration

	// RetryBudget bounds the retries the client spends in proportion to the
	// requests that succeed, so that an upstream that is down is not sent
	// three times the traffic it was failing to serve. See [RetryBudget].
	RetryBudget RetryBudget

	// CircuitBreaker, once given a Threshold, stops sending requests to a
	// host that keeps failing, for a while. It is off by default. See
	// [CircuitBreakerOptions].
	CircuitBreaker CircuitBreakerOptions

	// Propagate adds headers to every request from its context, which is how
	// a call made while serving a request carries that request's identity
	// along. It defaults to [DefaultPropagate], which copies the request
	// identifier the [RequestID] middleware assigned. Set a function that
	// does nothing to send no such header.
	//
	// It is called once per call to Do, with the request's context and a copy
	// of its header, and what it adds is checked like any other header.
	Propagate func(ctx context.Context, h http.Header)
}

// RetryBudget bounds how many retries a [Client] spends in proportion to the
// requests that succeed.
//
// Retries are what turn an upstream's bad minute into its bad hour: every
// failed request becomes three, and the load that made it fail triples. The
// budget is a bucket of retry tokens. Every response that is not a server
// error earns Ratio of a token, every retry spends one, and a retry with no
// token to spend is not made. It starts full, and holds at most Burst.
type RetryBudget struct {
	// Ratio is the share of a retry each successful response earns,
	// defaulting to 0.1: at most one retry for every ten requests that
	// succeed, once the burst is spent.
	Ratio float64
	// Burst is how many retries can be made before any request has
	// succeeded, and the most that can be saved up, defaulting to 10.
	Burst int
}

// Defaults for [RetryBudget].
const (
	defaultRetryRatio = 0.1
	defaultRetryBurst = 10
)

// ErrResponseTooLarge reports a response body larger than
// [ClientOptions.MaxResponseBytes]. It is returned by [Client.Do] for a
// response that declares as much, and by a read of the body for one that
// does not declare it and sends it anyway. Test for it with [errors.Is].
var ErrResponseTooLarge = errors.New("muzak: the response body exceeds the client's limit")

// Client sends HTTP requests to other services, and is safe to point at a URL
// that came from somewhere else.
//
// It is a [net/http.Client] with the decisions that make that safe already
// made: addresses that do not belong to the public internet are refused at
// the moment of connecting, so neither a name that resolves to one, nor one
// that resolves to one the second time it is asked, nor a redirect to one,
// gets through; every wait has a bound; a response body has a size; and a
// request is retried only when sending it twice is harmless, and never so
// often that retries become the outage. See [ClientOptions] for each.
//
// A Client is safe for concurrent use and should be reused rather than built
// per request, since it holds the pool of open connections:
//
//	client := muzak.NewClient(muzak.ClientOptions{})
//	defer client.Close()
//
//	req, err := http.NewRequestWithContext(ctx.Context(), http.MethodGet, in.URL, nil)
//	if err != nil {
//		return Out{}, muzak.BadRequest("that is not a URL")
//	}
//	resp, err := client.Do(req)
type Client struct {
	client    *http.Client
	transport *http.Transport
	dialer    *clientDialer
	policy    *addressPolicy
	proxied   bool

	timeout          time.Duration
	maxResponseBytes int64
	maxRedirects     int
	sensitive        []string

	retry     *retryPolicy
	breakers  *breakerSet
	propagate func(ctx context.Context, h http.Header)
}

// NewClient builds a [Client].
//
// It panics on options that cannot mean what they say, which are an invalid
// prefix in AllowedNetworks or DeniedNetworks and a Proxy with no host or a
// scheme no proxy speaks, because a policy with a hole in it is not one to
// run with.
func NewClient(opts ClientOptions) *Client {
	policy := newAddressPolicy(opts)
	proxyAddr := clientProxyAddr(opts.Proxy)
	dialer := newClientDialer(policy, orDefaultDuration(opts.DialTimeout, DefaultClientDialTimeout), proxyAddr)

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSConfig != nil {
		tlsConfig = opts.TLSConfig.Clone()
		tlsConfig.MinVersion = max(tlsConfig.MinVersion, tls.VersionTLS12)
	}
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    orDefaultDuration(opts.TLSHandshakeTimeout, DefaultClientTLSHandshakeTimeout),
		ResponseHeaderTimeout:  orDefaultDuration(opts.ResponseHeaderTimeout, DefaultClientResponseHeaderTimeout),
		ExpectContinueTimeout:  time.Second,
		MaxIdleConns:           100,
		MaxIdleConnsPerHost:    16,
		IdleConnTimeout:        90 * time.Second,
		MaxResponseHeaderBytes: clientMaxResponseHeaderBytes,
	}
	if opts.Proxy != nil {
		transport.Proxy = http.ProxyURL(opts.Proxy)
	}

	c := &Client{
		transport:        transport,
		dialer:           dialer,
		policy:           policy,
		proxied:          opts.Proxy != nil,
		timeout:          orDefaultDuration(opts.Timeout, DefaultClientTimeout),
		maxResponseBytes: opts.MaxResponseBytes,
		maxRedirects:     opts.MaxRedirects,
		sensitive:        clientSensitiveHeaders(opts.SensitiveHeaders),
		retry:            newRetryPolicy(opts),
		propagate:        opts.Propagate,
	}
	if c.maxResponseBytes <= 0 {
		c.maxResponseBytes = DefaultClientMaxResponseBytes
	}
	if c.maxRedirects == 0 {
		c.maxRedirects = DefaultClientMaxRedirects
	}
	if c.propagate == nil {
		c.propagate = DefaultPropagate
	}
	var roundTripper http.RoundTripper = transport
	if opts.CircuitBreaker.Threshold > 0 {
		c.breakers = newBreakerSet(opts.CircuitBreaker)
		roundTripper = &breakerTransport{next: transport, breakers: c.breakers}
	}
	c.client = &http.Client{Transport: roundTripper, CheckRedirect: c.checkRedirect}
	return c
}

// clientProxyAddr returns the address the transport dials to reach a proxy,
// which is the one address the dialer connects to without the policy. It is
// computed the way net/http computes it, the scheme's port filled in.
func clientProxyAddr(proxy *url.URL) string {
	if proxy == nil {
		return ""
	}
	var defaultPort string
	switch proxy.Scheme {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	case "socks5", "socks5h":
		defaultPort = "1080"
	}
	if defaultPort == "" || proxy.Hostname() == "" || !isASCII(proxy.Hostname()) {
		// A name outside ASCII is mapped by net/http before it is dialled, and
		// would then not be recognised as the proxy.
		panic(fmt.Sprintf("muzak: ClientOptions.Proxy %q is not a proxy URL; give it an http, https or socks5 scheme and a host written in ASCII",
			clientShorten(proxy.Redacted())))
	}
	return net.JoinHostPort(proxy.Hostname(), cmp.Or(proxy.Port(), defaultPort))
}

// clientSensitiveHeaders returns the headers dropped on a redirect that leaves
// the origin, in canonical form so that a configured "x-api-key" removes the
// header however the request spelled it.
func clientSensitiveHeaders(extra []string) []string {
	names := []string{"Authorization", "Cookie", "Proxy-Authorization"}
	for _, name := range extra {
		names = append(names, http.CanonicalHeaderKey(strings.TrimSpace(name)))
	}
	return names
}

// Close releases the connections the client holds open while they are idle,
// which is what an application's shutdown wants of it. It does not stop the
// client: a request made afterwards opens a connection again, which is what
// lets a [Lifecycle] stop call it while a late request is still finishing.
// It always returns nil, and is safe to call more than once:
//
//	muzak.WithLifecycle(muzak.NewLifecycle("payments-client", nil,
//		func(context.Context) error { return client.Close() }))
func (c *Client) Close() error {
	c.transport.CloseIdleConnections()
	return nil
}

// Do sends a request and returns its response, following redirects and
// retrying as [ClientOptions] describes.
//
// It behaves as [net/http.Client.Do] does, except where safety calls for
// something else. A request to a refused address fails with an
// [*AddressRefusedError] and sends nothing. A header holding a carriage
// return, a line feed or a NUL is refused rather than sent. The response's
// body is bounded by MaxResponseBytes and stays under Timeout until it is
// closed, so the caller must close it, as with net/http. The error messages
// name the method and the origin of the request, never its path or query,
// which is where URLs carry their secrets.
//
// The request is not modified: headers [ClientOptions.Propagate] adds go on
// a copy. Its body is closed, even on an error, as net/http closes it.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if err := c.checkRequest(req); err != nil {
		if req != nil && req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	ctx, cancel := c.bound(req)
	// The context outlives this call only inside a response body that is
	// handed back, so every other way out, a panic in Propagate included,
	// releases it here.
	handedOff := false
	defer func() {
		if !handedOff {
			cancel()
		}
	}()
	out := req.Clone(ctx)
	if out.Header == nil {
		out.Header = make(http.Header)
	}
	c.propagate(ctx, out.Header)
	if err := checkRequestHeaders(out.Header); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, attempts, err := c.send(ctx, out)
	if err != nil {
		return nil, c.explain(ctx, out, attempts, err)
	}
	resp, err = c.bodyBounded(resp, cancel)
	handedOff = err == nil
	return resp, err
}

// checkRequest refuses a request the client cannot or will not send, before
// anything else is done with it.
func (c *Client) checkRequest(req *http.Request) error {
	if req == nil || req.URL == nil {
		return errors.New("muzak: Client.Do was given no request, or one with no URL")
	}
	return c.checkTarget(req.URL)
}

// checkTarget refuses a URL the client will not send a request to, for the
// first request and for every redirect.
func (c *Client) checkTarget(target *url.URL) error {
	if target.Scheme != "http" && target.Scheme != "https" {
		return fmt.Errorf("muzak: the client sends http and https requests, and %q is neither", clientShorten(target.Scheme))
	}
	host := target.Hostname()
	if host == "" {
		return errors.New("muzak: the request URL names no host")
	}
	return c.policy.checkHost(host, c.proxied)
}

// clientTimeout is the cause a call's context is cancelled with when
// [ClientOptions.Timeout] runs out, which is what lets the error say so rather
// than report a bare deadline.
type clientTimeout struct {
	method string
	origin string
	after  time.Duration
}

func (e *clientTimeout) Error() string {
	return fmt.Sprintf("muzak: %s %s did not complete within %v; raise ClientOptions.Timeout if it is expected to take longer",
		e.method, e.origin, e.after)
}

// Unwrap lets errors.Is find context.DeadlineExceeded, which is what code
// that handles timeouts in general tests for.
func (e *clientTimeout) Unwrap() error { return context.DeadlineExceeded }

// bound returns the context one call runs under: the request's own, limited by
// the client's Timeout.
func (c *Client) bound(req *http.Request) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(req.Context())
	}
	cause := &clientTimeout{method: clientMethod(req), origin: clientOrigin(req.URL), after: c.timeout}
	return context.WithTimeoutCause(req.Context(), c.timeout, cause)
}

// clientMethod returns a request's method, which net/http lets be empty for
// GET.
func clientMethod(req *http.Request) string {
	if req.Method == "" {
		return http.MethodGet
	}
	return req.Method
}

// clientOrigin renders the scheme and host of a URL, which is as much of it as
// an error message carries: a path or a query string is where a URL keeps its
// secrets, and an error message is read by whoever reads the log.
func clientOrigin(target *url.URL) string {
	return target.Scheme + "://" + clientShorten(target.Host)
}

// explain turns a failure into the error Do returns: a refusal, a timeout or an
// open circuit as itself, and anything else wrapped with the method and
// origin and without net/http's rendering of the whole URL.
func (c *Client) explain(ctx context.Context, req *http.Request, attempts int, err error) error {
	var (
		timeout  *clientTimeout
		refused  *AddressRefusedError
		redirect *redirectRefusal
		open     *circuitOpenError
	)
	switch {
	case errors.As(context.Cause(ctx), &timeout):
		return timeout
	case errors.As(err, &refused):
		return refused
	case errors.As(err, &redirect):
		return redirect
	case errors.As(err, &open):
		return open
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if attempts > 1 {
		return fmt.Errorf("muzak: %s %s failed after %d attempts: %w", clientMethod(req), clientOrigin(req.URL), attempts, err)
	}
	return fmt.Errorf("muzak: %s %s failed: %w", clientMethod(req), clientOrigin(req.URL), err)
}

// send makes the attempts one call to Do allows, and returns the response to
// hand back or the error that ended them, with how many were made.
func (c *Client) send(ctx context.Context, first *http.Request) (*http.Response, int, error) {
	replayable := c.retry.maxAttempts > 1 && replayableRequest(first)
	req := first
	for attempt := 1; ; attempt++ {
		resp, err := c.client.Do(req)
		if err == nil && !retryableStatus(resp.StatusCode) {
			if resp.StatusCode < http.StatusInternalServerError {
				c.retry.budget.earn()
			}
			return resp, attempt, nil
		}
		if err != nil && !retryableError(ctx, err) {
			return nil, attempt, err
		}
		delay, again := c.retry.next(ctx, attempt, replayable, resp)
		if !again {
			return resp, attempt, err
		}
		if resp != nil {
			_, _ = io.CopyN(io.Discard, resp.Body, clientDrainBytes)
			_ = resp.Body.Close()
		}
		if err := c.retry.wait(ctx, delay); err != nil {
			return nil, attempt, err
		}
		if req, err = replay(ctx, first); err != nil {
			return nil, attempt, err
		}
	}
}

// replay returns a copy of the first attempt to send again, with a fresh body.
func replay(ctx context.Context, first *http.Request) (*http.Request, error) {
	req := first.WithContext(ctx)
	if first.Body == nil || first.Body == http.NoBody {
		return req, nil
	}
	body, err := first.GetBody()
	if err != nil {
		// No prefix of its own: explain puts this behind the method and
		// origin, which is where it reads as a sentence.
		return nil, fmt.Errorf("the request body could not be read again for another attempt: %w", err)
	}
	req.Body = body
	return req, nil
}

// replayableRequest reports whether sending a request twice is harmless and
// possible: its method is idempotent, or it carries an Idempotency-Key, and
// its body, if it has one, can be produced again.
//
// The X-Idempotency-Key spelling is accepted too, because net/http's own
// transport accepts it when it decides whether a request may be resent on a
// fresh connection, and the two should not disagree.
func replayableRequest(req *http.Request) bool {
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	switch clientMethod(req) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return req.Header.Get("Idempotency-Key") != "" || req.Header.Get("X-Idempotency-Key") != ""
}

// retryableStatus reports a status that says the server could not serve the
// request now and might a moment later.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryableError reports whether a failure is a connection that broke or never
// formed, which another attempt may not meet, rather than one that would fail
// the same way every time.
//
// What is never retried: a cancelled or expired call, a refused address, an
// open circuit, a refused redirect, a certificate that does not verify, an
// alert the server sent during the handshake, and a name that does not exist.
func retryableError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var (
		refused     *AddressRefusedError
		redirect    *redirectRefusal
		open        *circuitOpenError
		certificate *tls.CertificateVerificationError
		dns         *net.DNSError
		op          *net.OpError
	)
	switch {
	case errors.As(err, &refused), errors.As(err, &redirect), errors.As(err, &open), errors.As(err, &certificate):
		return false
	case errors.As(err, &dns):
		return dns.IsTimeout || dns.IsTemporary
	case errors.As(err, &op):
		// A TLS alert from the server arrives as an OpError of its own, and
		// says the two ends cannot agree, which another attempt will not
		// change.
		return op.Op != "remote error"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	var urlErr *url.Error
	var timeout interface{ Timeout() bool }
	return errors.As(err, &urlErr) && errors.As(urlErr.Err, &timeout) && timeout.Timeout()
}

// bodyBounded hands a response back with its body limited to MaxResponseBytes
// and tied to the call's context, which is released when the body is closed
// or read to its end. A response that declares more than the limit is closed
// unread and refused.
func (c *Client) bodyBounded(resp *http.Response, release context.CancelFunc) (*http.Response, error) {
	if resp.ContentLength > c.maxResponseBytes && responseHasBody(resp) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: the response declares %d bytes and the limit is %d; raise ClientOptions.MaxResponseBytes if a body that large is expected",
			ErrResponseTooLarge, resp.ContentLength, c.maxResponseBytes)
	}
	resp.Body = &clientBody{body: resp.Body, remaining: c.maxResponseBytes, limit: c.maxResponseBytes, release: release}
	return resp, nil
}

// responseHasBody reports whether a response can carry a body at all, which a
// response to HEAD, a 204 and a 304 cannot whatever their Content-Length says.
func responseHasBody(resp *http.Response) bool {
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		return false
	}
	return resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified
}

// clientBody limits a response body and releases the call's context when the
// body is done with.
type clientBody struct {
	body      io.ReadCloser
	remaining int64
	limit     int64
	release   context.CancelFunc
	err       error
}

// Read reads at most one byte past what is left of the limit, which is how a
// body that ends exactly at the limit is told apart from one that goes past
// it without reading the rest.
func (b *clientBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if b.remaining < math.MaxInt64 && int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.body.Read(p)
	if int64(n) > b.remaining {
		n = int(b.remaining)
		b.remaining = 0
		b.err = fmt.Errorf("%w: more than %d bytes arrived; raise ClientOptions.MaxResponseBytes if a body that large is expected",
			ErrResponseTooLarge, b.limit)
		b.release()
		return n, b.err
	}
	b.remaining -= int64(n)
	if err != nil {
		b.release()
	}
	return n, err
}

// Close closes the body and releases the call's context.
func (b *clientBody) Close() error {
	err := b.body.Close()
	b.release()
	return err
}

// redirectRefusal is a redirect the client would not follow. It is a type of
// its own so that it is never retried: the same request would be redirected
// the same way again.
type redirectRefusal struct {
	message string
}

func (e *redirectRefusal) Error() string { return e.message }

// checkRedirect decides whether a redirect is followed, and takes the
// credentials off one that leaves the request's origin.
//
// net/http has already copied the first request's headers onto the next one
// when this runs, dropping Authorization and Cookie only for a different
// domain, and keeping them for a subdomain, another port or another scheme.
// This drops them, and SensitiveHeaders, for any change of origin, and keeps
// dropping them for the rest of the chain once one hop has left it, so that a
// host the chain passed through cannot send it back with the credentials to a
// path of its choosing.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if c.maxRedirects < 0 {
		return http.ErrUseLastResponse
	}
	if len(via) > c.maxRedirects {
		return &redirectRefusal{fmt.Sprintf(
			"muzak: %s was redirected more than %d times; raise ClientOptions.MaxRedirects if a chain that long is expected",
			clientOrigin(via[0].URL), c.maxRedirects)}
	}
	previous := via[len(via)-1].URL
	if previous.Scheme == "https" && req.URL.Scheme != "https" {
		return &redirectRefusal{fmt.Sprintf(
			"muzak: %s redirected to %s, from https down to %s, which would send the request in the clear; it is not followed",
			clientOrigin(previous), clientOrigin(req.URL), clientShorten(req.URL.Scheme))}
	}
	if err := c.checkTarget(req.URL); err != nil {
		return err
	}
	if leftOrigin(via, req.URL) {
		for _, name := range c.sensitive {
			req.Header.Del(name)
		}
		if via[0].Header.Get("Referer") == "" {
			// net/http adds the previous URL as the Referer, query string
			// and all, which is not something to hand to another origin
			// the caller never chose.
			req.Header.Del("Referer")
		}
	}
	return nil
}

// leftOrigin reports whether any hop of a redirect chain, the next one
// included, is on a different origin from the first request. The chain is
// bounded by MaxRedirects, so this is too.
func leftOrigin(via []*http.Request, next *url.URL) bool {
	first := via[0].URL
	if !sameOrigin(first, next) {
		return true
	}
	for _, hop := range via[1:] {
		if !sameOrigin(first, hop.URL) {
			return true
		}
	}
	return false
}

// sameOrigin reports whether two URLs share a scheme, a host and a port, with
// the scheme's default port standing in for one left out.
func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && originPort(a) == originPort(b)
}

// originPort returns a URL's port, or its scheme's default.
func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// checkRequestHeaders refuses a header that would change the shape of the
// request it is written into, before anything is sent.
//
// net/http refuses these too, but with a message that names neither the rule
// nor the reason, and only once a connection is open. A value holding a line
// break is how a header that came from outside, such as a forwarded token or
// an identifier, writes a header or a whole request of its own. The value
// itself is never repeated in the error, since a header is where credentials
// travel. The cost is linear in the size of the header.
func checkRequestHeaders(h http.Header) error {
	for name, values := range h {
		if !isHTTPToken(name) {
			return fmt.Errorf("muzak: the request header name %q is not a valid token, so it cannot be sent", clientShorten(name))
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return fmt.Errorf("muzak: the value of the request header %q holds a carriage return, line feed or NUL, "+
					"which could end the header early and write another; it is refused rather than sent", name)
			}
		}
	}
	return nil
}

// maxPropagatedID bounds the request identifier [DefaultPropagate] copies onto
// an outgoing request. The identifiers Muzak assigns are UUIDs; one this long
// came from somewhere else.
const maxPropagatedID = 256

// DefaultPropagate is the [ClientOptions.Propagate] a client uses unless told
// otherwise. It copies the identifier [RequestID] assigned to the request
// being served into the X-Request-Id header of the outgoing one, so that the
// logs of both services can be joined on it, and it leaves a header the caller
// set alone.
//
// An identifier that is not something to put in a header, because it holds a
// control character or is implausibly long, is left out rather than failing
// the call, since the call matters more than the correlation.
//
// Compose it with your own when you need both:
//
//	Propagate: func(ctx context.Context, h http.Header) {
//		muzak.DefaultPropagate(ctx, h)
//		h.Set("X-Tenant", tenantFrom(ctx))
//	},
func DefaultPropagate(ctx context.Context, h http.Header) {
	if _, set := h[HeaderRequestID]; !set {
		if id, ok := RequestIDFromContext(ctx); ok && id != "" && len(id) <= maxPropagatedID && isHeaderSafe(id) {
			h.Set(HeaderRequestID, id)
		}
	}
	// Trace context is propagated here as well once the application traces
	// requests, which is the one place every outgoing call passes through.
}

// isHeaderSafe reports whether a value can be written into a header as it is:
// printable ASCII and spaces, nothing that ends a line or a header.
func isHeaderSafe(value string) bool {
	for i := range len(value) {
		if c := value[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
