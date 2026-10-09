package muzak

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recordingServer answers every request with a handler and keeps the headers
// of each one it received, keyed by path.
type recordingServer struct {
	*countingServer
	mu   sync.Mutex
	seen map[string]http.Header
}

func newRecordingServer(t *testing.T, handler http.HandlerFunc) *recordingServer {
	t.Helper()
	server := &recordingServer{seen: map[string]http.Header{}}
	server.countingServer = newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		server.mu.Lock()
		server.seen[r.Host+r.URL.Path] = r.Header.Clone()
		server.mu.Unlock()
		handler(w, r)
	})
	return server
}

func (s *recordingServer) headers(hostPath string) http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[hostPath]
}

// credentialHeaders are what a caller attaches to a request that must not
// follow it to another origin.
func credentialHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer AUTH-SECRET")
	req.Header.Set("Cookie", "session=COOKIE-SECRET")
	req.Header.Set("Proxy-Authorization", "Basic PROXY-SECRET")
	req.Header.Set("X-Api-Key", "APIKEY-SECRET")
	req.Header.Set("X-Trace-Note", "kept")
}

func assertNoCredentials(t *testing.T, where string, h http.Header) {
	t.Helper()
	if h == nil {
		t.Fatalf("%s received no request", where)
	}
	for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key"} {
		if value := h.Get(name); value != "" {
			t.Errorf("%s received %s = %q, want it dropped on leaving the origin", where, name, value)
		}
	}
	if h.Get("X-Trace-Note") != "kept" {
		t.Errorf("%s lost X-Trace-Note, which is not a credential", where)
	}
}

func TestClientStripsCredentialsWhenARedirectLeavesTheOrigin(t *testing.T) {
	t.Parallel()
	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Host + r.URL.Path {
		case "example.com/start":
			http.Redirect(w, r, "http://evil.example.net/steal?from=start", http.StatusFound)
		case "example.com/subdomain":
			// net/http keeps credentials for a subdomain; an origin does not.
			http.Redirect(w, r, "http://api.example.com/sub", http.StatusFound)
		case "example.com/port":
			http.Redirect(w, r, "http://example.com:8080/port", http.StatusFound)
		case "example.com/same":
			http.Redirect(w, r, "/same-target", http.StatusFound)
		case "example.com/bounce":
			http.Redirect(w, r, "http://evil.example.net/bounce", http.StatusFound)
		case "evil.example.net/bounce":
			// Back to the origin, to a path the other host chose.
			http.Redirect(w, r, "http://example.com/admin/delete", http.StatusFound)
		}
	})
	client, network := newTestClient(t, ClientOptions{SensitiveHeaders: []string{"x-api-key"}})
	network.serve("example.com", publicA, "80", server.Server)
	network.serve("evil.example.net", publicB, "80", server.Server)
	network.serve("api.example.com", publicC, "80", server.Server)
	network.route(publicA, "8080", server.Server)

	send := func(path string) {
		t.Helper()
		req := mustRequest(t, t.Context(), http.MethodGet, "http://example.com"+path)
		credentialHeaders(req)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do(%s): %v", path, err)
		}
		drain(t, resp)
		if req.Header.Get("Authorization") == "" {
			t.Errorf("Do(%s) changed the caller's request headers", path)
		}
	}

	send("/start")
	assertNoCredentials(t, "another host", server.headers("evil.example.net/steal"))
	if referer := server.headers("evil.example.net/steal").Get("Referer"); referer != "" {
		t.Errorf("another host received Referer %q, want the previous URL kept from it", referer)
	}

	send("/subdomain")
	assertNoCredentials(t, "a subdomain", server.headers("api.example.com/sub"))

	send("/port")
	assertNoCredentials(t, "another port", server.headers("example.com:8080/port"))

	send("/bounce")
	assertNoCredentials(t, "the origin after a detour", server.headers("example.com/admin/delete"))

	send("/same")
	same := server.headers("example.com/same-target")
	if same.Get("Authorization") != "Bearer AUTH-SECRET" || same.Get("X-Api-Key") != "APIKEY-SECRET" {
		t.Errorf("a same-origin redirect lost the credentials: %v", same)
	}
}

func TestClientRefusesARedirectToARefusedAddress(t *testing.T) {
	t.Parallel()
	internal := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	_, port, _ := strings.Cut(strings.TrimPrefix(internal.URL, "http://"), ":")
	origin := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/literal":
			http.Redirect(w, r, "http://127.0.0.1:"+port+"/", http.StatusFound)
		case "/name":
			http.Redirect(w, r, "http://intranet.example.com/", http.StatusTemporaryRedirect)
		case "/metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/", http.StatusMovedPermanently)
		case "/numeric":
			http.Redirect(w, r, "http://0x7f.1:"+port+"/", http.StatusFound)
		case "/scheme":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		case "/gopher":
			http.Redirect(w, r, "gopher://example.com/", http.StatusFound)
		}
	})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("public.example.com", publicA, "80", origin.Server)
	network.resolve("intranet.example.com", netip.MustParseAddr("192.168.0.10"))
	network.route(netip.MustParseAddr("192.168.0.10"), "80", internal.Server)

	for _, path := range []string{"/literal", "/name", "/metadata", "/numeric"} {
		_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://public.example.com"+path))
		var refused *AddressRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("Do(%s) error = %v, want the redirect target refused", path, err)
		}
	}
	for _, path := range []string{"/scheme", "/gopher"} {
		_, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://public.example.com"+path))
		if err == nil || !strings.Contains(err.Error(), "http and https") {
			t.Errorf("Do(%s) error = %v, want the scheme refused", path, err)
		}
	}
	if hits := internal.hits.Load(); hits != 0 {
		t.Errorf("the internal server received %d requests, want none", hits)
	}
	if hits := origin.hits.Load(); hits != 6 {
		t.Errorf("origin hits = %d, want each refused redirect sent once and not retried", hits)
	}
}

func TestClientRefusesARedirectFromHTTPSDownToHTTP(t *testing.T) {
	t.Parallel()
	plain := newCountingServer(t, func(http.ResponseWriter, *http.Request) {})
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("secure"))
			return
		}
		http.Redirect(w, r, "http://example.com/plain", http.StatusFound)
	}))
	t.Cleanup(secure.Close)
	roots := x509.NewCertPool()
	roots.AddCert(secure.Certificate())
	client, network := newTestClient(t, ClientOptions{TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS10}})
	// The test certificate is issued for example.com, so the name the
	// client checks the certificate against is a real one.
	network.serve("example.com", publicA, "443", secure)
	network.route(publicA, "80", plain.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "https://example.com/ok"))
	if err != nil {
		t.Fatalf("Do over https: %v", err)
	}
	if body := drain(t, resp); body != "secure" {
		t.Errorf("body = %q, want %q", body, "secure")
	}

	req := mustRequest(t, t.Context(), http.MethodGet, "https://example.com/start")
	credentialHeaders(req)
	_, err = client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "from https down to http") {
		t.Fatalf("Do error = %v, want the downgrade refused", err)
	}
	if strings.Contains(err.Error(), "/plain") || strings.Contains(err.Error(), "/start") {
		t.Errorf("error = %q, want it to name origins and not paths", err)
	}
	if hits := plain.hits.Load(); hits != 0 {
		t.Errorf("the plain server received %d requests, want none", hits)
	}
}

func TestClientTLSFloorAndCertificateFailures(t *testing.T) {
	t.Parallel()
	client := NewClient(ClientOptions{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS10}})
	defer client.Close()
	if got := client.transport.TLSClientConfig.MinVersion; got != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want it raised to TLS 1.2", got)
	}
	if NewClient(ClientOptions{}).transport.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Error("the default TLS configuration does not insist on TLS 1.2")
	}

	// A server that only speaks TLS 1.1 is refused, and a certificate that
	// does not verify is not something another attempt will change.
	old := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	old.TLS = &tls.Config{MaxVersion: tls.VersionTLS11, MinVersion: tls.VersionTLS10}
	old.Config.ErrorLog = log.New(io.Discard, "", 0)
	old.StartTLS()
	t.Cleanup(old.Close)
	roots := x509.NewCertPool()
	roots.AddCert(old.Certificate())
	legacy, network := newTestClient(t, ClientOptions{TLSConfig: &tls.Config{RootCAs: roots}})
	network.serve("example.com", publicA, "443", old)
	if _, err := legacy.Do(mustRequest(t, t.Context(), http.MethodGet, "https://example.com/")); err == nil {
		t.Error("Do against a TLS 1.1 server succeeded, want it refused")
	}
	if attempts := len(network.connections()); attempts != 1 {
		t.Errorf("connections = %d, want a handshake failure not retried", attempts)
	}

	current := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	current.Config.ErrorLog = log.New(io.Discard, "", 0)
	current.StartTLS()
	t.Cleanup(current.Close)
	untrusted, network := newTestClient(t, ClientOptions{})
	network.serve("example.com", publicA, "443", current)
	_, err := untrusted.Do(mustRequest(t, t.Context(), http.MethodGet, "https://example.com/"))
	var verification *tls.CertificateVerificationError
	if !errors.As(err, &verification) {
		t.Errorf("Do error = %v, want a certificate verification failure", err)
	}
	if attempts := len(network.connections()); attempts != 1 {
		t.Errorf("connections = %d, want a certificate failure not retried", attempts)
	}
}

func TestClientRedirectLimit(t *testing.T) {
	t.Parallel()
	server := newCountingServer(t, func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if n == 0 {
			_, _ = w.Write([]byte("arrived"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/%d", n-1), http.StatusFound)
	})
	client, network := newTestClient(t, ClientOptions{})
	network.serve("chain.example.com", publicA, "80", server.Server)

	resp, err := client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://chain.example.com/5"))
	if err != nil {
		t.Fatalf("five redirects: %v", err)
	}
	if body := drain(t, resp); body != "arrived" {
		t.Errorf("body = %q, want the end of the chain", body)
	}
	_, err = client.Do(mustRequest(t, t.Context(), http.MethodGet, "http://chain.example.com/6"))
	if err == nil || !strings.Contains(err.Error(), "redirected more than 5 times") {
		t.Errorf("six redirects: error = %v, want the limit reported", err)
	}

	unfollowed, network := newTestClient(t, ClientOptions{MaxRedirects: -1})
	network.serve("chain.example.com", publicA, "80", server.Server)
	resp, err = unfollowed.Do(mustRequest(t, t.Context(), http.MethodGet, "http://chain.example.com/3"))
	if err != nil {
		t.Fatalf("MaxRedirects -1: %v", err)
	}
	drain(t, resp)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/2" {
		t.Errorf("response = %d to %q, want the redirect itself returned", resp.StatusCode, resp.Header.Get("Location"))
	}

	one, network := newTestClient(t, ClientOptions{MaxRedirects: 1})
	network.serve("chain.example.com", publicA, "80", server.Server)
	if _, err := one.Do(mustRequest(t, t.Context(), http.MethodGet, "http://chain.example.com/2")); err == nil {
		t.Error("two redirects with MaxRedirects 1 succeeded")
	}
}
