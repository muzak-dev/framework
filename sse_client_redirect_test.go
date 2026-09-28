package muzak

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// headerSink is a server that records the headers of the last request it
// received, standing in for whatever host a redirect points at.
type headerSink struct {
	mu      sync.Mutex
	headers http.Header
}

func (s *headerSink) seen() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headers
}

func newHeaderSink(t *testing.T) (*headerSink, *httptest.Server) {
	t.Helper()
	sink := &headerSink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.mu.Lock()
		sink.headers = r.Header.Clone()
		sink.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	return sink, server
}

func TestSSEDialDoesNotCarryACredentialToAnotherHost(t *testing.T) {
	t.Parallel()
	sink, server := newHeaderSink(t)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting %s: %v", server.URL, err)
	}
	// The same listener under another host name is another host as far as
	// net/http is concerned, which is the case where it drops Authorization
	// but forwards a credential header it does not recognise.
	elsewhere := "http://localhost:" + port + "/steal"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere, http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	reader, response, err := SSEDial(t.Context(), origin.URL+"/events", SSEDialOptions{
		Header: http.Header{
			"Authorization": {"Bearer AUTH-SECRET"},
			"X-Api-Key":     {"APIKEY-SECRET"},
		},
	})
	if err == nil || reader != nil {
		t.Fatalf("SSEDial = %v, %v, want the redirect refused", reader, err)
	}
	if !strings.Contains(err.Error(), "redirect") || !strings.Contains(err.Error(), elsewhere) {
		t.Errorf("error = %q, want it to name the redirect and where it pointed", err)
	}
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != elsewhere {
		t.Errorf("response = %d to %q, want the redirect itself", response.StatusCode, response.Header.Get("Location"))
	}
	if seen := sink.seen(); seen != nil {
		t.Errorf("the other host received a request carrying X-Api-Key=%q", seen.Get("X-Api-Key"))
	}
}

func TestSSEDialDoesNotFollowAnHTTPSOriginDownToPlainHTTP(t *testing.T) {
	t.Parallel()
	sink, plain := newHeaderSink(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/cleartext", http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	reader, _, err := SSEDial(t.Context(), origin.URL+"/events", SSEDialOptions{
		HTTPClient: origin.Client(),
		Header:     http.Header{"Authorization": {"Bearer AUTH-SECRET"}},
	})
	if err == nil || reader != nil {
		t.Fatalf("SSEDial = %v, %v, want the redirect refused", reader, err)
	}
	if seen := sink.seen(); seen != nil {
		t.Errorf("the cleartext server received Authorization=%q", seen.Get("Authorization"))
	}
}

func TestSSEDialClientRefusesRedirectsOnACopy(t *testing.T) {
	t.Parallel()
	followed := func(*http.Request, []*http.Request) error { return nil }
	caller := &http.Client{Timeout: time.Second, CheckRedirect: followed}
	dialer := sseDialClient(caller)
	if dialer == caller {
		t.Fatal("the caller's own client was handed back, so changing it would change theirs")
	}
	if err := dialer.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect = %v, want the redirect refused", err)
	}
	if caller.Timeout != time.Second || caller.CheckRedirect(nil, nil) != nil {
		t.Error("the caller's own client was modified")
	}
}
