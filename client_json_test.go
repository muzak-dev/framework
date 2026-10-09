package muzak

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type upstreamItem struct {
	Name  string `json:"name"`
	Price int    `json:"price"`
}

func newJSONUpstream(t *testing.T) (*Client, *recordingServer) {
	t.Helper()
	server := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/item":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"name":"plumbus","price":3,"added_later":true}`))
		case "/problem":
			w.Header().Set("Content-Type", "application/problem+json")
			_, _ = w.Write([]byte(`{"name":"problem","price":1}`))
		case "/duplicate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"a","name":"b"}`))
		case "/trailing":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"a"} {"name":"b"}`))
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html>proxy error</html>`))
		case "/missing":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no such item"}` + strings.Repeat(" ", 2*maxRemoteErrorBody)))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/large":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"` + strings.Repeat("x", 4096) + `"}`))
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Content-Type") != "application/json" {
				w.WriteHeader(http.StatusUnsupportedMediaType)
			}
			_, _ = w.Write(body)
		case "/unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	client, network := newTestClient(t, ClientOptions{MaxResponseBytes: 1024})
	network.serve("api.example.com", publicA, "80", server.Server)
	return client, server
}

func TestClientGetJSON(t *testing.T) {
	t.Parallel()
	client, server := newJSONUpstream(t)
	item, err := client.GetJSON[upstreamItem](t.Context(), "http://api.example.com/item")
	if err != nil || item != (upstreamItem{Name: "plumbus", Price: 3}) {
		t.Fatalf("GetJSON = %+v, %v, want the item with the unknown member ignored", item, err)
	}
	if accept := server.headers("api.example.com/item").Get("Accept"); accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}
	if item, err := client.GetJSON[upstreamItem](t.Context(), "http://api.example.com/problem"); err != nil || item.Name != "problem" {
		t.Errorf("GetJSON(+json) = %+v, %v, want a structured suffix accepted", item, err)
	}
	if item, err := client.GetJSON[upstreamItem](t.Context(), "http://api.example.com/empty"); err != nil || item != (upstreamItem{}) {
		t.Errorf("GetJSON(204) = %+v, %v, want the zero value", item, err)
	}

	for path, want := range map[string]string{
		"/duplicate": "not JSON that fits",
		"/trailing":  "not JSON that fits",
		"/html":      "rather than JSON",
	} {
		if _, err := client.GetJSON[upstreamItem](t.Context(), "http://api.example.com"+path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("GetJSON(%s) error = %v, want %q", path, err, want)
		}
	}

	_, err = client.GetJSON[upstreamItem](t.Context(), "http://api.example.com/missing")
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("GetJSON(404) error = %v, want a *RemoteError", err)
	}
	if !bytes.HasPrefix(remote.Body, []byte(`{"error":"no such item"}`)) || len(remote.Body) > maxRemoteErrorBody {
		t.Errorf("RemoteError.Body = %d bytes, want the start of the body and no more than %d", len(remote.Body), maxRemoteErrorBody)
	}
	if strings.Contains(remote.Error(), "no such item") || !strings.Contains(remote.Error(), "404") {
		t.Errorf("RemoteError message = %q, want the status and not the remote server's text", remote)
	}

	if _, err := client.GetJSON[upstreamItem](t.Context(), "http://api.example.com/large"); !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("GetJSON(large) error = %v, want ErrResponseTooLarge", err)
	}
	_, err = client.GetJSON[upstreamItem](t.Context(), "http://api.example.com/\x7f?token=SECRET")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("GetJSON(bad URL) error = %v, want it refused without repeating the URL", err)
	}
	if _, err := client.GetJSON[upstreamItem](t.Context(), "http://127.0.0.1/"); err == nil {
		t.Error("GetJSON reached a refused address")
	}
}

func TestClientPostJSON(t *testing.T) {
	t.Parallel()
	client, server := newJSONUpstream(t)
	item, err := client.PostJSON[upstreamItem](t.Context(), "http://api.example.com/echo", upstreamItem{Name: "sent", Price: 9})
	if err != nil || item != (upstreamItem{Name: "sent", Price: 9}) {
		t.Fatalf("PostJSON = %+v, %v, want the echo", item, err)
	}
	if _, err := client.PostJSON[upstreamItem](t.Context(), "http://api.example.com/echo", make(chan int)); err == nil ||
		!strings.Contains(err.Error(), "could not be encoded") {
		t.Errorf("PostJSON(chan) error = %v, want the encoding refused", err)
	}
	if _, err := client.PostJSON[upstreamItem](t.Context(), "://", nil); err == nil {
		t.Error("PostJSON(bad URL) succeeded")
	}

	// A POST is not retried.
	before := server.hits.Load()
	_, err = client.PostJSON[upstreamItem](t.Context(), "http://api.example.com/unavailable", upstreamItem{})
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("PostJSON(503) error = %v, want a *RemoteError", err)
	}
	if hits := server.hits.Load() - before; hits != 1 {
		t.Errorf("hits = %d, want the POST sent once", hits)
	}
}

func TestClientDoJSONKeepsTheCallersRequest(t *testing.T) {
	t.Parallel()
	client, server := newJSONUpstream(t)
	req := mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/item")
	req.Header = nil
	if _, err := client.DoJSON[upstreamItem](req); err != nil {
		t.Fatalf("DoJSON with a nil header: %v", err)
	}
	if req.Header != nil {
		t.Error("DoJSON wrote into the caller's request")
	}
	custom := mustRequest(t, t.Context(), http.MethodGet, "http://api.example.com/item")
	custom.Header.Set("Accept", "application/vnd.example+json")
	if _, err := client.DoJSON[upstreamItem](custom); err != nil {
		t.Fatalf("DoJSON: %v", err)
	}
	if accept := server.headers("api.example.com/item").Get("Accept"); accept != "application/vnd.example+json" {
		t.Errorf("Accept = %q, want the caller's own kept", accept)
	}
	if _, err := client.DoJSON[upstreamItem](nil); err == nil {
		t.Error("DoJSON(nil) succeeded")
	}
	head := mustRequest(t, t.Context(), http.MethodHead, "http://api.example.com/item")
	if item, err := client.DoJSON[upstreamItem](head); err != nil || item != (upstreamItem{}) {
		t.Errorf("DoJSON(HEAD) = %+v, %v, want the zero value", item, err)
	}
}
