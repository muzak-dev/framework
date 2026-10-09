package testclient_test

import (
	"net/http"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// hostApp answers with the host each request arrived for, over each kind of
// request the client sends.
func hostApp() *muzak.App {
	app := echoApp()
	app.Get("/hop", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.SetHeader("Location", "/echo")
		ctx.SetStatus(http.StatusFound)
		return muzak.Empty{}, nil
	})
	app.SSE("/host/stream", func(ctx *muzak.Context, _ muzak.Empty, stream *muzak.SSEStream[muzak.Empty]) error {
		return stream.SendEvent(muzak.SSEEvent[muzak.Empty]{Text: ctx.Request().Host})
	})
	return app
}

// TestHostHeaderSetsTheRequestHost is the regression test for a Host header
// that never left the client. net/http takes the host a request is sent for
// from Request.Host and ignores a Host header, so Header("Host", ...) did
// nothing, and a test of host-based routing or of a WebSocket's allowed hosts
// tested the loopback address instead.
func TestHostHeaderSetsTheRequestHost(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, hostApp(), testclient.WithoutCookies())

	client.Get("/echo", testclient.Header("Host", "api.example.com")).
		AssertJSON(`{"host": "api.example.com", "cookies": {}}`)
	// A redirect within the same server keeps the host it was asked for, as
	// net/http keeps a Host set on the request itself.
	client.Get("/hop", testclient.Header("Host", "api.example.com")).
		AssertJSON(`{"host": "api.example.com", "cookies": {}}`)
	// An event stream is requested by the framework's own dialer, which is
	// handed the headers rather than a request, and is reached all the same.
	stream := client.SSE("/host/stream", testclient.Header("Host", "stream.example.com"))
	if got := stream.Next().Data; got != "stream.example.com" {
		t.Errorf("stream host = %q, want stream.example.com", got)
	}

	withDefault := testclient.New(t, hostApp(), testclient.WithHeader("Host", "tenant.example.com"))
	withDefault.Get("/echo").AssertJSON(`{"host": "tenant.example.com", "cookies": {}}`)
	// A request's own Host replaces the client's, as any header does.
	withDefault.Get("/echo", testclient.Header("Host", "other.example.com")).
		AssertJSON(`{"host": "other.example.com", "cookies": {}}`)
}
