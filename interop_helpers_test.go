package muzak

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"muzak.dev/framework/i18n"
)

// mountCall is what a mounted handler saw of one request.
type mountCall struct {
	method     string
	path       string
	rawPath    string
	escaped    string
	requestURI string
	requestID  string
	route      string
	request    *http.Request
}

// mountRecorder is a plain net/http handler that records every request it is
// handed and answers with the method and path it saw, which is what a test of
// a mount asserts against.
type mountRecorder struct {
	mu    sync.Mutex
	calls []mountCall
}

func (m *mountRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, _ := RequestIDFromContext(r.Context())
	route, _ := RouteFromContext(r.Context())
	m.mu.Lock()
	m.calls = append(m.calls, mountCall{
		method:     r.Method,
		path:       r.URL.Path,
		rawPath:    r.URL.RawPath,
		escaped:    r.URL.EscapedPath(),
		requestURI: r.RequestURI,
		requestID:  id,
		route:      route,
		request:    r,
	})
	m.mu.Unlock()
	w.Header().Set("X-Mounted", "yes")
	// What the request said is recorded above rather than echoed, so the
	// handler writes nothing a client chose.
	_, _ = io.WriteString(w, "mounted")
}

// count reports how many requests reached the handler.
func (m *mountRecorder) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// last returns the most recent request the handler saw.
func (m *mountRecorder) last(t *testing.T) mountCall {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		t.Fatal("the mounted handler was never called")
	}
	return m.calls[len(m.calls)-1]
}

// serveOnWire serves an application on a loopback socket through the
// framework's own run path, so that what net/http does to a request before
// any handler sees it is part of the test, and returns the address.
func serveOnWire(t *testing.T, app *App) string {
	t.Helper()
	base, stop, err := app.serveInProcessAt("127.0.0.1:0")
	if err != nil {
		t.Fatalf("serving the application: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(context.Background()); err != nil {
			t.Errorf("stopping the application: %v", err)
		}
	})
	return strings.TrimPrefix(base, "http://")
}

// rawExchange writes a request exactly as given onto a fresh connection and
// reads one response, which is how a test sends what no well-behaved client
// would: a request line with "//", "/./" or an absolute URL, two Host
// headers, or none.
func rawExchange(t *testing.T, addr, request string) (*http.Response, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("writing the request: %v", err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the response to %q: %v", request, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

// interopSpanish translates the sentences and titles the interop features
// produce, written with escapes as every locale fixture in this repository's
// source is.
const interopSpanish = `
es:
  muzak:
    http:
      404: "No se encontr\u00f3 el recurso solicitado."
      421: "Este servidor no atiende el host solicitado."
    status:
      404: "No encontrado"
      421: "Solicitud mal dirigida"
      422: "Entidad no procesable"
    validation:
      summary: "La solicitud no pudo ser validada."
`

// interopStore chains interopSpanish in front of the locale the framework
// ships.
func interopStore(t *testing.T) *i18n.Store {
	t.Helper()
	own, err := i18n.Load(fstest.MapFS{"locales/es.yml": &fstest.MapFile{Data: []byte(interopSpanish)}}, "locales")
	if err != nil {
		t.Fatalf("loading the Spanish locale: %v", err)
	}
	store, err := i18n.New(i18n.StoreOptions{Backend: i18n.NewChain(own.Backend(), i18n.Builtin().Backend())})
	if err != nil {
		t.Fatalf("chaining the stores: %v", err)
	}
	return store
}

// rawGet is rawExchange for a GET of target with the given Host and extra
// header lines, each ending in CRLF.
func rawGet(t *testing.T, addr, target, host, extra string) (*http.Response, string) {
	t.Helper()
	return rawExchange(t, addr, "GET "+target+" HTTP/1.1\r\nHost: "+host+"\r\n"+extra+"Connection: close\r\n\r\n")
}
