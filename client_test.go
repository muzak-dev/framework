package muzak

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientDefaults(t *testing.T) {
	t.Parallel()
	client := NewClient(ClientOptions{})
	defer client.Close()
	checks := map[string]bool{
		"timeout":           client.timeout == DefaultClientTimeout,
		"body limit":        client.maxResponseBytes == DefaultClientMaxResponseBytes,
		"redirects":         client.maxRedirects == DefaultClientMaxRedirects,
		"attempts":          client.retry.maxAttempts == DefaultClientMaxAttempts,
		"retry after":       client.retry.maxRetryAfter == DefaultClientMaxRetryAfter,
		"dial timeout":      client.dialer.timeout == DefaultClientDialTimeout,
		"tls timeout":       client.transport.TLSHandshakeTimeout == DefaultClientTLSHandshakeTimeout,
		"header timeout":    client.transport.ResponseHeaderTimeout == DefaultClientResponseHeaderTimeout,
		"header bytes":      client.transport.MaxResponseHeaderBytes == clientMaxResponseHeaderBytes,
		"no proxy":          client.transport.Proxy == nil,
		"no breaker":        client.breakers == nil && client.client.Transport == client.transport,
		"no cookie jar":     client.client.Jar == nil,
		"no client timeout": client.client.Timeout == 0,
		"private refused":   !client.policy.allowPrivate,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("default %s is not what ClientOptions documents", name)
		}
	}
	unbounded := NewClient(ClientOptions{Timeout: -1, DialTimeout: -1, ResponseHeaderTimeout: -1, TLSHandshakeTimeout: -1})
	defer unbounded.Close()
	if unbounded.timeout != 0 || unbounded.dialer.timeout != 0 || unbounded.transport.ResponseHeaderTimeout != 0 {
		t.Error("a negative timeout did not remove the bound")
	}
}

func TestClientRefusesWhatItCannotSend(t *testing.T) {
	t.Parallel()
	client, network := newTestClient(t, ClientOptions{})
	if _, err := client.Do(nil); err == nil || !strings.HasPrefix(err.Error(), "muzak: ") {
		t.Errorf("Do(nil) = %v, want a muzak: error", err)
	}
	if _, err := client.Do(&http.Request{}); err == nil {
		t.Error("Do(no URL) succeeded")
	}
	body := &closeRecorder{Reader: strings.NewReader("x")}
	req := mustRequest(t, t.Context(), http.MethodPost, "ftp://example.com/")
	req.Body = body
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "http and https") {
		t.Errorf("Do(ftp) = %v, want the scheme refused", err)
	}
	if !body.closed {
		t.Error("a refused request's body was not closed, which net/http's contract promises")
	}
	if _, err := client.Do(&http.Request{URL: &url.URL{Scheme: "http", Path: "/x"}}); err == nil || !strings.Contains(err.Error(), "no host") {
		t.Errorf("Do(no host) = %v, want it refused", err)
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("connections = %v, want none", dialled)
	}
}

func TestClientHandlesABareRequest(t *testing.T) {
	t.Parallel()
	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.Method)) })
	// No method, no header and no overall timeout: the zero values net/http
	// accepts are accepted here too.
	client, network := newTestClient(t, ClientOptions{Timeout: -1})
	network.serve("api.example.com", publicA, "80", server.Server)
	target, _ := url.Parse("http://api.example.com/bare")
	resp, err := client.Do((&http.Request{URL: target}).WithContext(contextWithRequestID(t.Context(), "bare-id")))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if body := drain(t, resp); body != http.MethodGet {
		t.Errorf("method = %q, want GET", body)
	}
	if got := server.headers("api.example.com/bare").Get(HeaderRequestID); got != "bare-id" {
		t.Errorf("X-Request-Id = %q, want it propagated onto a request that had no header", got)
	}

	// A request refused for its header still has its body closed.
	body := &closeRecorder{Reader: strings.NewReader("x")}
	req := mustRequest(t, t.Context(), http.MethodPost, "http://api.example.com/")
	req.Header.Set("X-Bad", "a\r\nb")
	req.Body = body
	if _, err := client.Do(req); err == nil || !body.closed {
		t.Errorf("Do = %v, body closed %v, want the header refused and the body closed", err, body.closed)
	}
}

func TestClientProductionResolver(t *testing.T) {
	t.Parallel()
	// The resolver a client uses outside of these tests is the system's, and
	// localhost is the one name it answers without a network.
	client := NewClient(ClientOptions{})
	defer client.Close()
	addrs, err := client.dialer.lookup(t.Context(), "localhost")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("lookup(localhost) = %v, %v, want an address", addrs, err)
	}
	for _, addr := range addrs {
		if !addr.Unmap().IsLoopback() {
			t.Errorf("lookup(localhost) answered %s", addr)
		}
	}
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error {
	c.closed = true
	return nil
}

func TestClientRefusesHeaderInjection(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)
	// net/http refuses these too, once it has a request in hand; the client
	// refuses them first, with a message that says what is wrong and why.
	for name, tc := range map[string]struct {
		header http.Header
		reason string
	}{
		"carriage return": {http.Header{"X-Token": {"SECRET\r\nX-Admin: true"}}, "carriage return, line feed or NUL"},
		"line feed":       {http.Header{"X-Token": {"SECRET\nHost: internal"}}, "carriage return, line feed or NUL"},
		"nul":             {http.Header{"X-Token": {"SECRET\x00"}}, "carriage return, line feed or NUL"},
		"second value":    {http.Header{"X-Token": {"fine", "SECRET\r\n"}}, "carriage return, line feed or NUL"},
		"bad name":        {http.Header{"X Token": {"value"}}, "not a valid token"},
		"colon in name":   {http.Header{"X-Token:": {"value"}}, "not a valid token"},
		"empty name":      {http.Header{"": {"value"}}, "not a valid token"},
	} {
		req := mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/")
		req.Header = tc.header
		_, err := client.Do(req)
		if err == nil || !strings.HasPrefix(err.Error(), "muzak: ") || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: Do error = %v, want the header refused as %q", name, err, tc.reason)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: error = %q, want the header's value left out of it", name, err)
		}
	}
	if hits := server.hits.Load(); hits != 0 {
		t.Errorf("server hits = %d, want no refused header sent", hits)
	}
	if dialled := network.connections(); len(dialled) != 0 {
		t.Errorf("connections = %v, want none", dialled)
	}
}

func TestClientPropagatesTheRequestID(t *testing.T) {
	t.Parallel()
	server := newRecordingServer(t, func(http.ResponseWriter, *http.Request) {})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)
	send := func(path string, ctx context.Context, header http.Header) {
		t.Helper()
		req := mustRequest(t, ctx, http.MethodGet, "http://api.example.com"+path)
		for name, values := range header {
			req.Header[name] = values
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do(%s): %v", path, err)
		}
		drain(t, resp)
		if path == "/caller" && req.Header.Get(HeaderRequestID) != "caller-chosen" {
			t.Error("Do changed the caller's header")
		}
	}

	id := "0192f3a4-5b6c-7d8e-9f00-112233445566"
	send("/served", contextWithRequestID(t.Context(), id), nil)
	if got := server.headers("api.example.com/served").Get(HeaderRequestID); got != id {
		t.Errorf("X-Request-Id = %q, want the served request's %q", got, id)
	}
	send("/caller", contextWithRequestID(t.Context(), id), http.Header{HeaderRequestID: {"caller-chosen"}})
	if got := server.headers("api.example.com/caller").Get(HeaderRequestID); got != "caller-chosen" {
		t.Errorf("X-Request-Id = %q, want the caller's own left alone", got)
	}
	send("/none", t.Context(), nil)
	if got := server.headers("api.example.com/none").Values(HeaderRequestID); len(got) != 0 {
		t.Errorf("X-Request-Id = %q, want none outside a served request", got)
	}
	send("/forged", contextWithRequestID(t.Context(), "id\r\nX-Admin: true"), nil)
	send("/long", contextWithRequestID(t.Context(), strings.Repeat("a", maxPropagatedID+1)), nil)
	for _, path := range []string{"/forged", "/long"} {
		if got := server.headers("api.example.com" + path).Values(HeaderRequestID); len(got) != 0 {
			t.Errorf("%s: X-Request-Id = %q, want an unfit identifier left out rather than sent", path, got)
		}
	}
}

func TestClientCustomPropagation(t *testing.T) {
	t.Parallel()
	server := newRecordingServer(t, func(http.ResponseWriter, *http.Request) {})
	type tenantKey struct{}
	client, network := newTestClient(t, ClientOptions{Propagate: func(ctx context.Context, h http.Header) {
		DefaultPropagate(ctx, h)
		if tenant, ok := ctx.Value(tenantKey{}).(string); ok {
			h.Set("X-Tenant", tenant)
		}
	}})
	network.serve("api.example.com", publicA, "80", server.Server)
	ctx := context.WithValue(contextWithRequestID(t.Context(), "abc"), tenantKey{}, "acme")
	resp, err := client.Do(mustRequest(t, ctx, http.MethodGet, "http://api.example.com/x"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	seen := server.headers("api.example.com/x")
	if seen.Get("X-Tenant") != "acme" || seen.Get(HeaderRequestID) != "abc" {
		t.Errorf("headers = %v, want both the tenant and the request id", seen)
	}

	// What a propagator adds is checked like anything else.
	forging, _ := newTestClient(t, ClientOptions{Propagate: func(_ context.Context, h http.Header) {
		h.Set("X-Tenant", "acme\r\nX-Admin: true")
	}})
	if _, err := forging.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/")); err == nil {
		t.Error("a propagated header holding CRLF was sent")
	}

	// A propagator that sends nothing turns propagation off.
	silent, network := newTestClient(t, ClientOptions{Propagate: func(context.Context, http.Header) {}})
	network.serve("api.example.com", publicA, "80", server.Server)
	resp, err = silent.Do(mustRequest(t, contextWithRequestID(t.Context(), "abc"), http.MethodGet, "http://api.example.com/silent"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	drain(t, resp)
	if got := server.headers("api.example.com/silent").Get(HeaderRequestID); got != "" {
		t.Errorf("X-Request-Id = %q, want none", got)
	}

	// A propagator that panics panics in the caller's goroutine, where the
	// application's recovery middleware can see it, and leaves nothing held.
	panicking, _ := newTestClient(t, ClientOptions{Propagate: func(context.Context, http.Header) { panic("propagator bug") }})
	if recovered := catchPanic(func() {
		_, _ = panicking.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	}); recovered != "propagator bug" {
		t.Errorf("recovered %v, want the propagator's panic", recovered)
	}
}

func TestClientResponseBodyLimit(t *testing.T) {
	t.Parallel()
	const limit = 1024
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		switch r.URL.Path {
		case "/declared":
			w.Header().Set("Content-Length", strconv.Itoa(size))
		case "/chunked":
			// Flushing first makes the body chunked, with no length declared.
			http.NewResponseController(w).Flush()
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(w)
			_, _ = writer.Write(bytes.Repeat([]byte{0}, size))
			_ = writer.Close()
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), size))
	})
	client, network := newTestClient(t, ClientOptions{MaxResponseBytes: limit})
	network.serve("api.example.com", publicA, "80", server.Server)
	read := func(path string, size int) (int, error) {
		t.Helper()
		resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com"+path+"?size="+strconv.Itoa(size)))
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return len(body), err
	}

	for _, path := range []string{"/declared", "/chunked", "/gzip"} {
		if n, err := read(path, limit); err != nil || n != limit {
			t.Errorf("%s at the limit: read %d, %v, want the whole body", path, n, err)
		}
		n, err := read(path, limit+1)
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Errorf("%s one past the limit: read %d, %v, want ErrResponseTooLarge", path, n, err)
		}
		if n > limit {
			t.Errorf("%s: read %d bytes, want no more than the limit handed over", path, n)
		}
	}
	// A declared length past the limit is refused before the body is read.
	if _, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/declared?size=1048576")); !errors.Is(err, ErrResponseTooLarge) ||
		!strings.Contains(err.Error(), "declares 1048576 bytes") {
		t.Errorf("Do error = %v, want the declared length refused", err)
	}
	// A compressed body that expands far past the limit is cut off at it.
	if n, err := read("/gzip", 64<<20); !errors.Is(err, ErrResponseTooLarge) || n > limit {
		t.Errorf("gzip bomb: read %d, %v, want it stopped at the limit", n, err)
	}
	// A HEAD response declares a length it does not send.
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodHead, "http://api.example.com/declared?size=1048576"))
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	drain(t, resp)
}

func TestClientBodyLimitAtTheLargestValue(t *testing.T) {
	t.Parallel()
	// A limit of the largest int64 has no "one byte past it" to read, and must
	// not overflow working that out.
	var released bool
	body := &clientBody{body: io.NopCloser(strings.NewReader("abc")), remaining: math.MaxInt64, limit: math.MaxInt64,
		release: func() { released = true }}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != "abc" || !released {
		t.Errorf("ReadAll = %q, %v, released %v, want the body and the context released at its end", got, err, released)
	}
}

func TestClientReadsOnlyTheDeclaredLengthOfALyingBody(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		switch r.URL.Path {
		case "/short":
			// Declares five bytes and sends a megabyte. The connection is
			// marked to close, so the excess is not read as the start of
			// the next response on a connection put back in the pool.
			_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 5\r\n\r\n" + strings.Repeat("y", 1<<20))
		case "/long":
			// Declares a hundred bytes, sends ten and hangs up.
			_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789")
		case "/headers":
			// A header block larger than the client accepts.
			_, _ = buffered.WriteString("HTTP/1.1 200 OK\r\nX-Big: " + strings.Repeat("h", clientMaxResponseHeaderBytes+1) + "\r\n\r\n")
		}
		_ = buffered.Flush()
	})
	client, network := newTestClient(t, ClientOptions{MaxAttempts: 1})
	network.serve("api.example.com", publicA, "80", server.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/short"))
	if err != nil {
		t.Fatalf("Do(short): %v", err)
	}
	if body := drain(t, resp); body != "yyyyy" {
		t.Errorf("body = %d bytes, want exactly the five declared", len(body))
	}

	resp, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/long"))
	if err != nil {
		t.Fatalf("Do(long): %v", err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("reading a truncated body = %v, want io.ErrUnexpectedEOF", err)
	}

	if _, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/headers")); err == nil {
		t.Error("a response header past the client's limit was accepted")
	}
}

func TestClientTimeoutCoversTheBody(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		http.NewResponseController(w).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	client, network := newTestClient(t, ClientOptions{Timeout: 200 * time.Millisecond})
	network.serve("api.example.com", publicA, "80", server.Server)
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	if err == nil {
		t.Fatal("reading a body that never ends succeeded")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the read took %v, want it ended by the timeout", elapsed)
	}
}

func TestClientResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	client, network := newTestClient(t, ClientOptions{ResponseHeaderTimeout: 50 * time.Millisecond})
	network.serve("api.example.com", publicA, "80", server.Server)
	start := time.Now()
	_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
	if err == nil || !strings.Contains(err.Error(), "timeout awaiting response headers") {
		t.Fatalf("Do error = %v, want the header timeout", err)
	}
	// A GET is retried after a timeout, which is a connection-level failure.
	if hits := server.hits.Load(); hits != DefaultClientMaxAttempts {
		t.Errorf("hits = %d, want %d attempts", hits, DefaultClientMaxAttempts)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Do took %v, want each attempt bounded", elapsed)
	}
}

func TestClientIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	server := statusServer(t, nil, 503, 200)
	client, network := newTestClient(t, ClientOptions{CircuitBreaker: CircuitBreakerOptions{Threshold: 1000}})
	network.serve("api.example.com", publicA, "80", server.Server)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			ctx := contextWithRequestID(t.Context(), strconv.Itoa(i))
			resp, err := client.Do(mustRequest(t, ctx, http.MethodGet, "http://api.example.com/"))
			if err != nil {
				t.Errorf("Do: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		})
	}
	wg.Wait()
	_ = client.Close()
	_ = client.Close()
}

func TestClientCloseLeavesItUsable(t *testing.T) {
	t.Parallel()
	server := statusServer(t, nil, 200)
	client, network := newTestClient(t, ClientOptions{})
	network.serve("api.example.com", publicA, "80", server.Server)
	for range 2 {
		resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/"))
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		drain(t, resp)
		if err := client.Close(); err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	}
	if dialled := len(network.connections()); dialled != 2 {
		t.Errorf("connections = %d, want a fresh one after Close released the idle one", dialled)
	}
}

func TestClientLeavesNoGoroutinesBehind(t *testing.T) {
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/unavailable":
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		case "/large":
			_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
		case "/drop":
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
		default:
			_, _ = w.Write([]byte("ok"))
		}
	})
	client, network := newTestClient(t, ClientOptions{
		Timeout:          300 * time.Millisecond,
		MaxResponseBytes: 1024,
		CircuitBreaker:   CircuitBreakerOptions{Threshold: 2},
	})
	network.serve("api.example.com", publicA, "80", server.Server)
	for _, path := range []string{"/ok", "/redirect", "/unavailable", "/slow", "/large", "/drop", "/unavailable"} {
		resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com"+path))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = client.Do(mustRequest(t, ctx, http.MethodGet, "http://api.example.com/ok"))
	_, _ = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://127.0.0.1/"))
	// A response whose body is never read or closed still has its context
	// released once the timeout passes.
	if resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/ok")); err == nil {
		_ = resp
	}
	time.Sleep(400 * time.Millisecond)
	_ = client.Close()
	server.Close()
	assertNoGoroutineLeaks(t)
}

func TestClientWorksOverARealListenerWhenAllowed(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		scanner := bufio.NewScanner(r.Body)
		scanner.Scan()
		_, _ = w.Write([]byte("echo " + scanner.Text()))
	})
	client := NewClient(ClientOptions{AllowedNetworks: nil, AllowPrivateNetworks: true})
	defer client.Close()
	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodPost, server.URL, "hello"))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if body := drain(t, resp); body != "echo hello" {
		t.Errorf("body = %q, want the echo", body)
	}
}
