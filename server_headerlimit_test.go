package muzak

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The header block a client may send is bounded by 64 KiB unless the
// application says otherwise, rather than by the megabyte net/http allows: a
// client that never finishes one holds everything it has sent for as long as
// ReadHeaderTimeout lets it.
func TestDefaultHeaderLimitRefusesALargeHeaderBlock(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Get("/ok", func(*Context, Empty) (Empty, error) { return Empty{}, nil })
	built := mustBuild(t, app)

	if got := built.newServer().MaxHeaderBytes; got != DefaultMaxHeaderBytes || DefaultMaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes = %d (default %d), want 64 KiB", got, DefaultMaxHeaderBytes)
	}

	server := httptest.NewUnstartedServer(built)
	server.Config.MaxHeaderBytes = built.newServer().MaxHeaderBytes
	server.Start()
	t.Cleanup(server.Close)

	get := func(padding int) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, server.URL+"/ok", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Padding", strings.Repeat("a", padding))
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(8 << 10); got != http.StatusOK {
		t.Errorf("an 8 KiB header answered %d, want 200", got)
	}
	if got := get(128 << 10); got != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("a 128 KiB header answered %d, want 431", got)
	}
}
