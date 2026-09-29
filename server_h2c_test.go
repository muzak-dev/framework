package muzak

import (
	"context"
	"net/http"
	"testing"
)

// get sends one request to the running application, over HTTP/2 with prior
// knowledge when h2c is set and over HTTP/1 otherwise, and reports the protocol
// the server answered in.
func get(t *testing.T, addr string, h2c bool) (string, error) {
	t.Helper()
	var protocols http.Protocols
	if h2c {
		protocols.SetUnencryptedHTTP2(true)
	} else {
		protocols.SetHTTP1(true)
	}
	client := &http.Client{Transport: &http.Transport{Protocols: &protocols}}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Get("http://" + addr + "/x")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.Proto, nil
}

// A platform that speaks HTTP/2 to the container in the clear, such as Cloud
// Run on an h2c port, reaches the application only if the server accepts it.
func TestUnencryptedHTTP2IsServedOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		on   bool
	}{{"off", false}, {"on", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := quietOptions()
			opts.Addr = "127.0.0.1:0"
			opts.UnencryptedHTTP2 = tc.on
			app := New(opts)
			app.Get("/x", okHandler)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- app.RunContext(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			addr := waitForAddr(t, app)

			// HTTP/1 is served either way.
			if proto, err := get(t, addr, false); err != nil || proto != "HTTP/1.1" {
				t.Fatalf("an HTTP/1 request answered %q, %v; want HTTP/1.1", proto, err)
			}
			proto, err := get(t, addr, true)
			if tc.on && (err != nil || proto != "HTTP/2.0") {
				t.Errorf("an h2c request answered %q, %v; want HTTP/2.0", proto, err)
			}
			if !tc.on && err == nil {
				t.Errorf("an h2c request was served as %q with the option off", proto)
			}
		})
	}
}
