package muzak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sseTestTimeout bounds every read a test makes, so that a broken engine fails
// a test rather than hanging one.
const sseTestTimeout = 5 * time.Second

// itemOut is the model the streams in these tests carry.
type itemOut struct {
	Name string `json:"name"`
}

// newSSETestApp builds an application with the given routes and serves it over
// a real socket, which is what a stream needs: a response recorder is read
// only once the handler has returned, so nothing arrives while it runs.
func newSSETestApp(t *testing.T, register func(app *App), opts ...RouterOption) (*App, *httptest.Server) {
	t.Helper()
	return newSSETestAppWith(t, quietOptions(), register, opts...)
}

// newSSETestAppWith is newSSETestApp with the application options spelled out.
func newSSETestAppWith(t *testing.T, options AppOptions, register func(app *App), opts ...RouterOption) (*App, *httptest.Server) {
	t.Helper()
	app := New(options, opts...)
	register(app)
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)
	return app, server
}

// openStream opens a stream against a test server and fails the test if it
// could not be opened.
func openStream(t *testing.T, serverURL, path string, opts ...func(*SSEDialOptions)) *SSEReader {
	t.Helper()
	reader, response := tryStream(t, serverURL, path, opts...)
	if reader == nil {
		t.Fatalf("the stream was refused with status %d", response.StatusCode)
	}
	return reader
}

// tryStream opens a stream and returns whatever came back, for a test that
// expects a refusal.
func tryStream(t *testing.T, serverURL, path string, opts ...func(*SSEDialOptions)) (*SSEReader, *http.Response) {
	t.Helper()
	options := SSEDialOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	t.Cleanup(cancel)
	reader, response, err := SSEDial(ctx, serverURL+path, options)
	if response == nil {
		t.Fatalf("SSEDial(%s) = %v", path, err)
	}
	if reader != nil {
		t.Cleanup(func() { _ = reader.Close() })
	}
	return reader, response
}

// nextEvent reads one event, failing the test if the stream ends first.
func nextEvent(t *testing.T, reader *SSEReader) SSEMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	message, err := reader.Next(ctx)
	if err != nil {
		t.Fatalf("Next() = %v, want an event", err)
	}
	return message
}

// assertStreamEnded fails the test unless the stream has finished.
func assertStreamEnded(t *testing.T, reader *SSEReader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
	defer cancel()
	message, err := reader.Next(ctx)
	if err == nil {
		t.Fatalf("Next() = %+v, want the stream to have ended", message)
	}
	if !errors.Is(err, ErrSSEStreamEnded) {
		t.Errorf("Next() = %v, want it to report ErrSSEStreamEnded", err)
	}
}

// streamItems sends one event per name and returns.
func streamItems(names ...string) func(*Context, Empty, *SSEStream[itemOut]) error {
	return func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		for _, name := range names {
			if err := stream.Send(itemOut{Name: name}); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestSSEStreamsEvents(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/items/stream", streamItems("Plumbus", "Portal Gun"))
	})

	reader := openStream(t, server.URL, "/items/stream")
	for _, want := range []string{"Plumbus", "Portal Gun"} {
		item, err := nextEvent(t, reader).Decode[itemOut]()
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		if item.Name != want {
			t.Errorf("name = %q, want %q", item.Name, want)
		}
	}
	// The handler returned, so the stream is over rather than merely quiet.
	assertStreamEnded(t, reader)
}

func TestSSEResponseHeaders(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/items/stream", streamItems("Plumbus"))
	})

	reader, response := tryStream(t, server.URL, "/items/stream")
	if reader == nil {
		t.Fatalf("the stream was refused with status %d", response.StatusCode)
	}
	for name, want := range map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8",
		"Cache-Control":     "no-cache, no-transform",
		"X-Accel-Buffering": "no",
	} {
		if got := response.Header.Get(name); got != want {
			t.Errorf("header %s = %q, want %q", name, got, want)
		}
	}
	// The request identifier reaches a stream like it reaches any response.
	if response.Header.Get(HeaderRequestID) == "" {
		t.Error("the stream carries no request identifier")
	}
}

// sseWire runs one handler through a response recorder and returns exactly
// what it wrote, which is how the wire format itself is asserted on.
func sseWire(t *testing.T, register func(app *App)) string {
	t.Helper()
	app := New(quietOptions())
	register(app)
	mustBuild(t, app)
	rec := do(t, app, http.MethodGet, "/stream")
	assertStatus(t, rec, http.StatusOK)
	return rec.Body.String()
}

func TestSSEWireFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		send  func(*SSEStream[itemOut]) error
		wants string
	}{
		{
			name:  "data alone",
			send:  func(s *SSEStream[itemOut]) error { return s.Send(itemOut{Name: "Plumbus"}) },
			wants: "data: {\"name\":\"Plumbus\"}\n\n",
		},
		{
			name: "every field",
			send: func(s *SSEStream[itemOut]) error {
				item := itemOut{Name: "Plumbus"}
				return s.SendEvent(SSEEvent[itemOut]{
					Comment: "the first one",
					Name:    "item_update",
					ID:      "1",
					Retry:   5 * time.Second,
					Data:    &item,
				})
			},
			wants: ": the first one\nevent: item_update\nid: 1\nretry: 5000\ndata: {\"name\":\"Plumbus\"}\n\n",
		},
		{
			name: "text instead of JSON",
			send: func(s *SSEStream[itemOut]) error {
				return s.SendEvent(SSEEvent[itemOut]{Name: "done", Text: "[DONE]"})
			},
			wants: "event: done\ndata: [DONE]\n\n",
		},
		{
			name:  "a comment of its own",
			send:  func(s *SSEStream[itemOut]) error { return s.Comment("still here") },
			wants: ": still here\n\n",
		},
		{
			name: "an event carrying nothing but an id",
			send: func(s *SSEStream[itemOut]) error {
				return s.SendEvent(SSEEvent[itemOut]{ID: "42"})
			},
			wants: "id: 42\n\n",
		},
		{
			name: "text spanning several lines",
			send: func(s *SSEStream[itemOut]) error {
				return s.SendEvent(SSEEvent[itemOut]{Text: "first\nsecond\r\nthird\rfourth"})
			},
			wants: "data: first\ndata: second\ndata: third\ndata: fourth\n\n",
		},
		{
			name: "text ending in a newline keeps it",
			send: func(s *SSEStream[itemOut]) error {
				return s.SendEvent(SSEEvent[itemOut]{Text: "line\n"})
			},
			// The empty data line is what carries the trailing newline: a
			// client joins the lines and drops one, leaving what was sent.
			wants: "data: line\ndata: \n\n",
		},
		{
			name: "a comment spanning several lines",
			send: func(s *SSEStream[itemOut]) error {
				return s.Comment("one\ntwo")
			},
			wants: ": one\n: two\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := sseWire(t, func(app *App) {
				app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
					return tc.send(stream)
				})
			})
			if body != tc.wants {
				t.Errorf("stream body =\n%q\nwant\n%q", body, tc.wants)
			}
		})
	}
}

func TestSSERetryIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	body := sseWire(t, func(app *App) {
		app.SSE("/stream", streamItems("Plumbus"),
			WithSSE(SSEOptions{Retry: 2 * time.Second}))
	})

	want := "retry: 2000\n\ndata: {\"name\":\"Plumbus\"}\n\n"
	if body != want {
		t.Errorf("stream body = %q, want %q", body, want)
	}
}

func TestSSEBindsInputAndDependencies(t *testing.T) {
	t.Parallel()
	type streamIn struct {
		Room  string `path:"room"`
		Since *int   `query:"since"`
	}

	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/rooms/{room}/stream", func(ctx *Context, in streamIn, stream *SSEStream[itemOut]) error {
			token := From[string](ctx)
			since := -1
			if in.Since != nil {
				since = *in.Since
			}
			return stream.Send(itemOut{Name: fmt.Sprintf("%s/%d/%s/%s", in.Room, since, token, stream.LastEventID())})
		}, Needs(func(ctx *Context) (string, error) { return ctx.Query("token"), nil }))
	})

	reader := openStream(t, server.URL, "/rooms/hall/stream?since=7&token=jessica",
		func(o *SSEDialOptions) { o.LastEventID = "3" })

	item, err := nextEvent(t, reader).Decode[itemOut]()
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if item.Name != "hall/7/jessica/3" {
		t.Errorf("event = %q, want %q", item.Name, "hall/7/jessica/3")
	}
}

func TestSSEBindingFailureNeverBecomesAStream(t *testing.T) {
	t.Parallel()
	type streamIn struct {
		Limit int `query:"limit"`
	}

	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ streamIn, stream *SSEStream[itemOut]) error {
		return stream.Send(itemOut{Name: "never"})
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/stream?limit=nonsense")
	assertStatus(t, rec, http.StatusUnprocessableEntity)
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want a JSON error rather than a stream", got)
	}
	if code := decodeError(t, rec).Error.Code; code != CodeValidationError {
		t.Errorf("error code = %q, want %q", code, CodeValidationError)
	}
}

func TestSSEGuardsRunBeforeTheStreamOpens(t *testing.T) {
	t.Parallel()
	app := New(quietOptions(), WithDependencies(func(ctx *Context) error {
		if ctx.Query("token") == "" {
			return NewHTTPError(http.StatusUnauthorized, "a token is required")
		}
		return nil
	}))
	app.SSE("/stream", streamItems("Plumbus"))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/stream")
	assertStatus(t, rec, http.StatusUnauthorized)
	if body := rec.Body.String(); strings.Contains(body, "data:") {
		t.Errorf("the refusal carried stream data: %s", body)
	}
}

func TestSSERouteAnswersOtherMethods(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.SSE("/stream", streamItems("Plumbus"))
	mustBuild(t, app)

	// A HEAD is not offered: answering one would run the handler with every
	// write discarded until it gave up.
	head := do(t, app, http.MethodHead, "/stream")
	assertStatus(t, head, http.StatusMethodNotAllowed)
	if allow := head.Header().Get("Allow"); allow != "GET, OPTIONS" {
		t.Errorf("Allow = %q, want %q", allow, "GET, OPTIONS")
	}

	options := do(t, app, http.MethodOptions, "/stream")
	assertStatus(t, options, http.StatusNoContent)
	if allow := options.Header().Get("Allow"); allow != "GET, OPTIONS" {
		t.Errorf("Allow = %q, want %q", allow, "GET, OPTIONS")
	}
}

func TestSSEOverPost(t *testing.T) {
	t.Parallel()
	type promptIn struct {
		Text string `json:"text"`
	}

	_, server := newSSETestApp(t, func(app *App) {
		app.SSEHandle("post", "/chat/stream", func(_ *Context, in promptIn, stream *SSEStream[Empty]) error {
			for word := range strings.SplitSeq(in.Text, " ") {
				if err := stream.SendEvent(SSEEvent[Empty]{Name: "token", Text: word}); err != nil {
					return err
				}
			}
			return stream.SendEvent(SSEEvent[Empty]{Name: "done", Text: "[DONE]"})
		})
	})

	reader := openStream(t, server.URL, "/chat/stream", func(o *SSEDialOptions) {
		o.Method = http.MethodPost
		o.Body = strings.NewReader(`{"text":"a b"}`)
		o.Header = http.Header{"Content-Type": []string{"application/json"}}
	})

	for _, want := range []struct{ name, data string }{
		{"token", "a"},
		{"token", "b"},
		{"done", "[DONE]"},
	} {
		message := nextEvent(t, reader)
		if message.Name != want.name || message.Data != want.data {
			t.Errorf("event = %q/%q, want %q/%q", message.Name, message.Data, want.name, want.data)
		}
	}
}

func TestSSEKeepsAnIdleStreamOpen(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			<-release
			return stream.Send(itemOut{Name: "at last"})
		}, WithSSE(SSEOptions{KeepAlive: 10 * time.Millisecond}))
	})
	defer close(release)

	reader := openStream(t, server.URL, "/stream", func(o *SSEDialOptions) { o.KeepComments = true })
	message := nextEvent(t, reader)
	if message.Comment != sseKeepAliveComment {
		t.Errorf("first message = %+v, want a keepalive comment", message)
	}
}

func TestSSEKeepAliveSaysNothingOnABusyStream(t *testing.T) {
	t.Parallel()
	stop := make(chan struct{})
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			for {
				select {
				case <-stop:
					return nil
				default:
				}
				if err := stream.Send(itemOut{Name: "busy"}); err != nil {
					return err
				}
				time.Sleep(time.Millisecond)
			}
		}, WithSSE(SSEOptions{KeepAlive: 50 * time.Millisecond}))
	})
	defer close(stop)

	reader := openStream(t, server.URL, "/stream", func(o *SSEDialOptions) { o.KeepComments = true })
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if message := nextEvent(t, reader); message.Comment != "" {
			t.Fatalf("a busy stream was sent a keepalive: %+v", message)
		}
	}
}

func TestSSEEndsWhenTheClientGoesAway(t *testing.T) {
	t.Parallel()
	ended := make(chan error, 1)
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "first"}); err != nil {
				return err
			}
			<-stream.Context().Done()
			err := stream.Err()
			ended <- err
			return err
		}, WithSSE(SSEOptions{KeepAlive: -1}))
	})

	reader := openStream(t, server.URL, "/stream")
	nextEvent(t, reader)
	_ = reader.Close()

	select {
	case err := <-ended:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the handler ended with %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the handler never noticed the client going away")
	}
}

func TestSSEShutdownEndsOpenStreams(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.Addr = "127.0.0.1:0"
	logger, logs := captureLogger(t)
	opts.Logger = logger

	app := New(opts)
	sending := make(chan struct{})
	returned := make(chan error, 1)
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		if err := stream.Send(itemOut{Name: "first"}); err != nil {
			return err
		}
		close(sending)
		for {
			if err := stream.Send(itemOut{Name: "more"}); err != nil {
				returned <- err
				return err
			}
			time.Sleep(time.Millisecond)
		}
	}, WithSSE(SSEOptions{KeepAlive: -1}))
	mustBuild(t, app)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunContext(ctx) }()
	addr := waitForAddr(t, app)

	reader := openStream(t, "http://"+addr, "/stream")
	nextEvent(t, reader)
	<-sending

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
	select {
	case err := <-returned:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the handler ended with %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the handler was never told the server was shutting down")
	}
	if !strings.Contains(logs.String(), "Ended 1 event stream") {
		t.Errorf("the shutdown never reported the stream it ended; the log holds:\n%s", logs.String())
	}
}

func TestSSERefusesAStreamWhileDraining(t *testing.T) {
	t.Parallel()
	app, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", streamItems("Plumbus"))
	})

	app.streams.shutdown(time.Second, (*sseStream).shuttingDown)
	_, response := tryStream(t, server.URL, "/stream")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestSSEStreamLimit(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.SSE = SSEOptions{MaxStreams: 1, KeepAlive: -1}
	release := make(chan struct{})
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "held"}); err != nil {
				return err
			}
			<-release
			return nil
		})
	})
	defer close(release)

	first := openStream(t, server.URL, "/stream")
	nextEvent(t, first)

	_, response := tryStream(t, server.URL, "/stream")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("the second stream got status %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
	if retry := response.Header.Get("Retry-After"); retry == "" {
		t.Error("a refused stream carried no Retry-After header")
	}
}

func TestSSEStreamLimitResolution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ configured, want int }{
		{0, DefaultSSEMaxStreams},
		{-1, 0},
		{7, 7},
	} {
		if got := sseStreamLimit(tc.configured); got != tc.want {
			t.Errorf("sseStreamLimit(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

func TestSSEStreamLimitPerIP(t *testing.T) {
	t.Parallel()
	// MaxStreams alone bounds the process, not one client within it: without
	// MaxStreamsPerIP, a single address could hold every one of MaxStreams'
	// slots itself and leave 503 for everyone else.
	opts := quietOptions()
	opts.SSE = SSEOptions{MaxStreams: 10, MaxStreamsPerIP: 1, KeepAlive: -1}
	opts.ClientIP = ClientIPOptions{TrustedProxies: []string{"127.0.0.1/32"}}
	release := make(chan struct{})
	_, server := newSSETestAppWith(t, opts, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "held"}); err != nil {
				return err
			}
			<-release
			return nil
		})
	})
	defer close(release)

	withAddress := func(ip string) func(*SSEDialOptions) {
		return func(o *SSEDialOptions) { o.Header = http.Header{"X-Forwarded-For": []string{ip}} }
	}

	first := openStream(t, server.URL, "/stream", withAddress("203.0.113.9"))
	nextEvent(t, first)

	// A second stream from the same address is refused even though the
	// process-wide MaxStreams has plenty of room left.
	_, refused := tryStream(t, server.URL, "/stream", withAddress("203.0.113.9"))
	if refused.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a second stream from the same address got status %d, want %d", refused.StatusCode, http.StatusServiceUnavailable)
	}
	if retry := refused.Header.Get("Retry-After"); retry == "" {
		t.Error("a refused stream carried no Retry-After header")
	}

	// A different address is unaffected: the limit is per client, not global.
	second := openStream(t, server.URL, "/stream", withAddress("198.51.100.4"))
	nextEvent(t, second)
}

func TestSSEStreamsPerIPLimitResolution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ configured, want int }{
		{0, DefaultSSEMaxStreamsPerIP},
		{-1, 0},
		{7, 7},
	} {
		if got := sseStreamsPerIPLimit(tc.configured); got != tc.want {
			t.Errorf("sseStreamsPerIPLimit(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

func TestSSERegistrationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		register func(app *App)
		wants    string
	}{
		{
			name:     "a nil handler",
			register: func(app *App) { app.SSE("/stream", SSEHandler[Empty, itemOut](nil)) },
			wants:    "handler is nil",
		},
		{
			name: "a path that is not one",
			register: func(app *App) {
				app.SSE("stream", streamItems("Plumbus"))
			},
			wants: "path must begin with",
		},
		{
			name: "a declared status",
			register: func(app *App) {
				app.SSE("/stream", streamItems("Plumbus"), Status(http.StatusCreated))
			},
			wants: "Status cannot be declared on an event stream route",
		},
		{
			name: "a stream limit on the route",
			register: func(app *App) {
				app.SSE("/stream", streamItems("Plumbus"), WithSSE(SSEOptions{MaxStreams: 2}))
			},
			wants: "MaxStreams may only be set on the application",
		},
		{
			name: "a stream limit on a router",
			register: func(app *App) {
				live := NewRouter(WithSSE(SSEOptions{MaxStreams: 2}))
				live.SSE("/stream", streamItems("Plumbus"))
				app.Include(live)
			},
			wants: "MaxStreams may only be set on the application",
		},
		{
			name: "a path parameter the template does not declare",
			register: func(app *App) {
				app.SSE("/stream", func(_ *Context, _ struct {
					Room string `path:"room"`
				}, stream *SSEStream[itemOut]) error {
					return stream.Send(itemOut{Name: "x"})
				})
			},
			wants: "does not declare",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.register(app)
			if got := buildError(t, app); !strings.Contains(got, tc.wants) {
				t.Errorf("Build() = %q, want it to mention %q", got, tc.wants)
			}
		})
	}
}

func TestSSEOptionsLayer(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.SSE = SSEOptions{KeepAlive: time.Minute, WriteTimeout: 3 * time.Second}

	app := New(opts)
	live := NewRouter(WithSSE(SSEOptions{KeepAlive: 30 * time.Second}))
	narrow := live.SSE("/narrow", streamItems("x"), WithSSE(SSEOptions{Retry: time.Second}))
	wide := live.SSE("/wide", streamItems("x"))
	app.Include(live)
	mustBuild(t, app)

	// The route raises one field; everything else comes from the router and
	// the application in turn.
	if got := narrow.sse.opts; got.KeepAlive != 30*time.Second || got.WriteTimeout != 3*time.Second || got.Retry != time.Second {
		t.Errorf("the narrowed route resolved to %+v", got)
	}
	if got := wide.sse.opts; got.KeepAlive != 30*time.Second || got.Retry != 0 {
		t.Errorf("the inheriting route resolved to %+v", got)
	}
}

func TestSSEOptionsWithDefaults(t *testing.T) {
	t.Parallel()
	filled := SSEOptions{}.withDefaults()
	if filled.KeepAlive != DefaultSSEKeepAlive {
		t.Errorf("KeepAlive = %v, want %v", filled.KeepAlive, DefaultSSEKeepAlive)
	}
	if filled.WriteTimeout != DefaultSSEWriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", filled.WriteTimeout, DefaultSSEWriteTimeout)
	}

	// A negative value is how each bound is removed, and a negative retry is
	// simply no retry at all.
	off := SSEOptions{KeepAlive: -1, WriteTimeout: -1, Retry: -1}.withDefaults()
	if off.KeepAlive != 0 || off.WriteTimeout != 0 || off.Retry != 0 {
		t.Errorf("the disabled options resolved to %+v", off)
	}
}

func TestSSEOpenAPI(t *testing.T) {
	t.Parallel()
	type streamIn struct {
		Room string `path:"room" doc:"The room to follow"`
	}

	app := New(quietOptions())
	app.SSE("/rooms/{room}/stream", func(_ *Context, _ streamIn, stream *SSEStream[itemOut]) error {
		return stream.Send(itemOut{Name: "x"})
	}, Summary("Follow a room"))
	app.SSE("/logs/stream", func(_ *Context, _ Empty, stream *SSEStream[Empty]) error {
		return stream.SendEvent(SSEEvent[Empty]{Text: "a log line"})
	})
	mustBuild(t, app)

	doc, err := app.Document()
	if err != nil {
		t.Fatalf("Document() = %v", err)
	}

	op := doc.Paths["/rooms/{room}/stream"].Get
	if op == nil {
		t.Fatal("the stream route is not in the document")
	}
	ok, described := op.Responses["200"]
	if !described {
		t.Fatalf("the stream has no 200 response: %+v", op.Responses)
	}
	media, streamed := ok.Content["text/event-stream"]
	if !streamed {
		t.Fatalf("the 200 response is not an event stream: %+v", ok.Content)
	}
	if media.Schema.Ref != componentPrefix+"itemOut" {
		t.Errorf("the event schema is %+v, want a reference to the item model", media.Schema)
	}
	if _, upgraded := op.Responses["101"]; upgraded {
		t.Error("a stream route documents an upgrade it never performs")
	}
	if len(op.Parameters) != 1 || op.Parameters[0].Name != "room" {
		t.Errorf("parameters = %+v, want the room path parameter", op.Parameters)
	}
	if op.RequestBody != nil {
		t.Errorf("a GET stream documents a request body: %+v", op.RequestBody)
	}

	// A stream of text that is not JSON has no schema to describe, exactly as
	// a route returning Empty has no response body to describe.
	raw := doc.Paths["/logs/stream"].Get.Responses["200"]
	if len(raw.Content) != 0 {
		t.Errorf("the raw stream documents content: %+v", raw.Content)
	}
}

func TestSSENeedsAResponseItCanFlush(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.SSE("/stream", streamItems("Plumbus"))
	mustBuild(t, app)

	// A writer with no Flush of its own cannot carry a stream, and refusing
	// before the header is written is what turns that into an error a caller
	// can read rather than an empty 200.
	recorder := httptest.NewRecorder()
	app.ServeHTTP(unflushable{recorder}, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "cannot be flushed") {
		t.Errorf("the refusal does not say why: %s", body)
	}
	if strings.Contains(body, "data:") {
		t.Errorf("events were written to a response that cannot be flushed: %q", body)
	}
}

// unflushable is a response writer that carries neither a flush nor a
// deadline, which is what a middleware that wraps without forwarding leaves a
// handler with.
type unflushable struct{ recorder *httptest.ResponseRecorder }

func (u unflushable) Header() http.Header         { return u.recorder.Header() }
func (u unflushable) Write(b []byte) (int, error) { return u.recorder.Write(b) }
func (u unflushable) WriteHeader(status int)      { u.recorder.WriteHeader(status) }

func TestSSEHandlerFailureIsNotDisclosed(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		if err := stream.Send(itemOut{Name: "first"}); err != nil {
			return err
		}
		return errors.New("the mainframe database at 10.0.0.3 refused the query")
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/stream")
	assertStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); strings.Contains(body, "mainframe") {
		t.Errorf("the failure reached the client: %q", body)
	}
	if !strings.Contains(logs.String(), "mainframe") {
		t.Errorf("the failure was not logged; the log holds:\n%s", logs.String())
	}
}

func TestSSEEndedIsNotReportedAsAFailure(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.SSE("/stream", func(_ *Context, _ Empty, _ *SSEStream[itemOut]) error {
		return ended("the client went away")
	})
	mustBuild(t, app)

	do(t, app, http.MethodGet, "/stream")
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Errorf("a stream that merely ended was logged as a failure:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "an event stream ended") {
		t.Errorf("the stream ending was not recorded; the log holds:\n%s", logs.String())
	}
}

func TestSSEHandlerPanicIsRecovered(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger

	app := New(opts)
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		if err := stream.Send(itemOut{Name: "first"}); err != nil {
			return err
		}
		panic("the handler fell over")
	})
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/stream")
	// The response began long ago, so the panic cannot change the status; what
	// it must not do is escape or leave the stream registered.
	assertStatus(t, rec, http.StatusOK)
	if !strings.Contains(logs.String(), "the handler fell over") {
		t.Errorf("the panic was not logged; the log holds:\n%s", logs.String())
	}
	if held := app.streams.count(); held != 0 {
		t.Errorf("the register still holds %d streams after a panic", held)
	}
}

func TestSSEWorksBehindCompression(t *testing.T) {
	t.Parallel()
	// The handler does not return until the test has read the event, so the
	// event can only have arrived because it was flushed on the way. A wrapper
	// that swallowed the flush would hold it until the response ended, which
	// is a stream that is not a stream.
	release := make(chan struct{})
	_, server := newSSETestApp(t, func(app *App) {
		app.Use(Compress(CompressionOptions{MinSize: 1}))
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "Plumbus"}); err != nil {
				return err
			}
			<-release
			return nil
		}, WithSSE(SSEOptions{KeepAlive: -1}))
	})
	defer close(release)

	reader, response := tryStream(t, server.URL, "/stream")
	if reader == nil {
		t.Fatalf("the stream was refused with status %d", response.StatusCode)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" {
		t.Errorf("Content-Encoding = %q, want an event stream to be left alone", encoding)
	}
	if item, err := nextEvent(t, reader).Decode[itemOut](); err != nil || item.Name != "Plumbus" {
		t.Errorf("event = %+v, %v", item, err)
	}
}

func TestSSEEventsArriveWhileTheHandlerRuns(t *testing.T) {
	t.Parallel()
	// The same assertion without any middleware at all, so that a flush lost
	// in the framework's own wrapper is caught here rather than blamed on a
	// wrapper somebody installed.
	release := make(chan struct{})
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: "first"}); err != nil {
				return err
			}
			<-release
			return nil
		}, WithSSE(SSEOptions{KeepAlive: -1}))
	})
	defer close(release)

	reader := openStream(t, server.URL, "/stream")
	if item, err := nextEvent(t, reader).Decode[itemOut](); err != nil || item.Name != "first" {
		t.Errorf("event = %+v, %v, want it before the handler returned", item, err)
	}
}

func TestSSEConcurrentSendersDoNotInterleave(t *testing.T) {
	t.Parallel()
	const senders, each = 8, 25

	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			var wg sync.WaitGroup
			for sender := range senders {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range each {
						name := strings.Repeat(strconv.Itoa(sender), 40)
						if err := stream.Send(itemOut{Name: fmt.Sprintf("%d:%d:%s", sender, i, name)}); err != nil {
							return
						}
					}
				}()
			}
			wg.Wait()
			return nil
		}, WithSSE(SSEOptions{KeepAlive: 5 * time.Millisecond}))
	})

	reader := openStream(t, server.URL, "/stream")
	counts := make(map[int]int)
	for range senders * each {
		item, err := nextEvent(t, reader).Decode[itemOut]()
		if err != nil {
			t.Fatalf("Decode() = %v", err)
		}
		var sender, index int
		var name string
		if _, err := fmt.Sscanf(item.Name, "%d:%d:%s", &sender, &index, &name); err != nil {
			t.Fatalf("an event arrived mangled: %q", item.Name)
		}
		if index != counts[sender] {
			t.Fatalf("sender %d sent event %d, want %d: the stream reordered them", sender, index, counts[sender])
		}
		counts[sender]++
	}
}

func TestSSESendAfterTheHandlerReturned(t *testing.T) {
	t.Parallel()
	sent := make(chan error, 1)
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			// A goroutine that outlives its handler must not write into a
			// response net/http has already handed to the next request.
			go func() {
				<-stream.Context().Done()
				sent <- stream.Send(itemOut{Name: "too late"})
			}()
			return stream.Send(itemOut{Name: "in time"})
		})
	})

	reader := openStream(t, server.URL, "/stream")
	nextEvent(t, reader)
	assertStreamEnded(t, reader)

	select {
	case err := <-sent:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the late send returned %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("the late send never returned")
	}
}

func TestSSEShrinksItsBuffersAfterALargeEvent(t *testing.T) {
	t.Parallel()
	measured := make(chan [2]int, 1)
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			if err := stream.Send(itemOut{Name: strings.Repeat("x", maxSSEBuffer+1)}); err != nil {
				return err
			}
			measured <- [2]int{cap(stream.core.buf), cap(stream.core.payload)}
			return nil
		})
	})

	reader := openStream(t, server.URL, "/stream")
	nextEvent(t, reader)
	sizes := <-measured
	if sizes[0] > maxSSEBuffer || sizes[1] > maxSSEBuffer {
		t.Errorf("the stream kept buffers of %d and %d bytes, want them released above %d",
			sizes[0], sizes[1], maxSSEBuffer)
	}
}

func TestSSEUnserializableEventLeavesTheStreamUsable(t *testing.T) {
	t.Parallel()
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[chan int]) error {
			if err := stream.Send(make(chan int)); err == nil {
				return errors.New("a channel was encoded as JSON")
			}
			// The event never reached the wire, so the stream is still good.
			return stream.SendEvent(SSEEvent[chan int]{Text: "carried on"})
		})
	})

	reader := openStream(t, server.URL, "/stream")
	if message := nextEvent(t, reader); message.Data != "carried on" {
		t.Errorf("event = %+v, want the stream to have carried on", message)
	}
}

func TestSSEStreamsAreForgottenWhenTheirHandlersReturn(t *testing.T) {
	t.Parallel()
	app, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", streamItems("Plumbus"))
	})

	for range 5 {
		reader := openStream(t, server.URL, "/stream")
		nextEvent(t, reader)
		assertStreamEnded(t, reader)
	}
	waitFor(t, func() bool { return app.streams.count() == 0 }, "the register to forget every finished stream")
}

func TestSSENoGoroutineLeaks(t *testing.T) {
	_, server := newSSETestApp(t, func(app *App) {
		app.SSE("/stream", streamItems("one", "two"))
		app.SSE("/keepalive", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
			for range 3 {
				if err := stream.Send(itemOut{Name: "tick"}); err != nil {
					return err
				}
			}
			return nil
		}, WithSSE(SSEOptions{KeepAlive: time.Millisecond}))
	})

	var wg sync.WaitGroup
	var opened atomic.Int64
	for range 20 {
		for _, path := range []string{"/stream", "/keepalive"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(t.Context(), sseTestTimeout)
				defer cancel()
				reader, _, err := SSEDial(ctx, server.URL+path, SSEDialOptions{})
				if err != nil {
					return
				}
				opened.Add(1)
				for {
					if _, err := reader.Next(ctx); err != nil {
						break
					}
				}
				_ = reader.Close()
			}()
		}
	}
	wg.Wait()
	if opened.Load() == 0 {
		t.Fatal("no stream was opened at all")
	}

	server.Close()
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	assertNoGoroutineLeaks(t)
}

func TestSSEBindsAFormBody(t *testing.T) {
	t.Parallel()
	type promptIn struct {
		Text string `form:"text"`
	}

	_, server := newSSETestApp(t, func(app *App) {
		app.SSEHandle(http.MethodPost, "/chat/stream", func(_ *Context, in promptIn, stream *SSEStream[itemOut]) error {
			return stream.Send(itemOut{Name: in.Text})
		})
	})

	reader := openStream(t, server.URL, "/chat/stream", func(o *SSEDialOptions) {
		o.Method = http.MethodPost
		o.Body = strings.NewReader("text=hello")
		o.Header = http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}}
	})
	if item, err := nextEvent(t, reader).Decode[itemOut](); err != nil || item.Name != "hello" {
		t.Errorf("event = %+v, %v, want the form value back", item, err)
	}
}

func TestSSERefusedAfterTheResponseStarted(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Something in the chain answered already, so there is no response
			// left to stream into.
			w.WriteHeader(http.StatusAccepted)
			next.ServeHTTP(w, r)
		})
	})
	app.SSE("/stream", streamItems("Plumbus"))
	mustBuild(t, app)

	rec := do(t, app, http.MethodGet, "/stream")
	assertStatus(t, rec, http.StatusAccepted)
	if body := rec.Body.String(); strings.Contains(body, "data:") {
		t.Errorf("events were written after the response had started: %q", body)
	}
}

// newTestStream builds a stream over a response recorder, for the few parts of
// the engine that a served stream cannot be made to reach on demand.
func newTestStream(t *testing.T, opts SSEOptions) *sseStream {
	t.Helper()
	c := &Context{
		w: asResponseWriter(httptest.NewRecorder()),
		r: httptest.NewRequest(http.MethodGet, "/stream", nil),
	}
	stream := newSSEStream(c, opts.withDefaults())
	t.Cleanup(stream.cancel)
	return stream
}

func TestSSEStreamGivesUpWaitingWhenItEnds(t *testing.T) {
	t.Parallel()
	stream := newTestStream(t, SSEOptions{})
	if err := stream.acquire(); err != nil {
		t.Fatalf("acquire() = %v", err)
	}

	waiting := make(chan error, 1)
	go func() { waiting <- stream.acquire() }()
	// The second writer is queued behind the first when the stream ends, and
	// must be released rather than left holding a semaphore nobody will return.
	time.Sleep(10 * time.Millisecond)
	stream.shuttingDown()

	select {
	case err := <-waiting:
		if !errors.Is(err, ErrSSEStreamEnded) {
			t.Errorf("the waiting writer got %v, want ErrSSEStreamEnded", err)
		}
	case <-time.After(sseTestTimeout):
		t.Fatal("a writer waiting for its turn was never released")
	}
	stream.release()
}

func TestSSEStreamFinishReportsAWriteItCouldNotWaitOut(t *testing.T) {
	t.Parallel()
	stream := newTestStream(t, SSEOptions{WriteTimeout: 20 * time.Millisecond})
	if err := stream.acquire(); err != nil {
		t.Fatalf("acquire() = %v", err)
	}
	// The semaphore is never given back, which is what a write stuck on a
	// client that has stopped reading looks like from here.
	if stream.finish() {
		t.Error("finish() reported a response handed back while a write still held it")
	}
	// Once the response has gone back to net/http, nothing here may touch its
	// deadlines again.
	stream.interrupt()
}

func TestSSEFinalWaitAlwaysBoundsTheLastWrite(t *testing.T) {
	t.Parallel()
	if got := (&sseStream{}).finalWait(); got != DefaultSSEWriteTimeout {
		t.Errorf("finalWait() = %v, want %v for a stream with no write timeout", got, DefaultSSEWriteTimeout)
	}
	if got := (&sseStream{writeTimeout: time.Second}).finalWait(); got != time.Second {
		t.Errorf("finalWait() = %v, want the configured write timeout", got)
	}
}

func TestSSEKeepAliveStopsWhenItCannotWrite(t *testing.T) {
	t.Parallel()
	stream := newTestStream(t, SSEOptions{})
	// The stream has failed but its context has not been cancelled, which is
	// the moment a keepalive discovers the stream through its own write.
	stream.record(ended("the connection was lost"))

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		stream.keepalive(time.Millisecond)
	}()
	select {
	case <-stopped:
	case <-time.After(sseTestTimeout):
		t.Fatal("the keepalive carried on writing to a stream that had ended")
	}
}

// errBrokenWriter is what a response that has stopped working reports.
var errBrokenWriter = errors.New("the connection went")

// brokenWriter is a response writer that fails at a chosen point, so that the
// failures a real connection only produces by accident can be produced on
// purpose.
type brokenWriter struct {
	recorder  *httptest.ResponseRecorder
	failWrite bool
	// failFlush is the flush that fails, counting from one; zero never fails.
	failFlush int
	flushes   int
}

func (b *brokenWriter) Header() http.Header    { return b.recorder.Header() }
func (b *brokenWriter) WriteHeader(status int) { b.recorder.WriteHeader(status) }

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.failWrite {
		return 0, errBrokenWriter
	}
	return b.recorder.Write(p)
}

func (b *brokenWriter) FlushError() error {
	b.flushes++
	if b.failFlush == b.flushes {
		return errBrokenWriter
	}
	return nil
}

func TestSSEStreamThatCannotBeStarted(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		writer *brokenWriter
		opts   SSEOptions
	}{
		{
			name:   "a connection that goes before the header is out",
			writer: &brokenWriter{failFlush: 1},
		},
		{
			name:   "a connection that goes before the first event",
			writer: &brokenWriter{failWrite: true},
			opts:   SSEOptions{Retry: time.Second},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran atomic.Bool
			app := New(quietOptions())
			app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
				ran.Store(true)
				return stream.Send(itemOut{Name: "never"})
			}, WithSSE(tc.opts))
			mustBuild(t, app)

			tc.writer.recorder = httptest.NewRecorder()
			app.ServeHTTP(tc.writer, httptest.NewRequest(http.MethodGet, "/stream", nil))

			if ran.Load() {
				t.Error("the handler ran on a stream that could not be started")
			}
		})
	}
}

func TestSSEEndsWhenAnEventCannotBeFlushed(t *testing.T) {
	t.Parallel()
	ended := make(chan error, 1)
	app := New(quietOptions())
	app.SSE("/stream", func(_ *Context, _ Empty, stream *SSEStream[itemOut]) error {
		err := stream.Send(itemOut{Name: "first"})
		ended <- err
		return err
	}, WithSSE(SSEOptions{KeepAlive: -1}))
	mustBuild(t, app)

	// The header goes out, and the flush after the first event is the one that
	// finds the connection gone.
	writer := &brokenWriter{recorder: httptest.NewRecorder(), failFlush: 2}
	app.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if err := <-ended; !errors.Is(err, ErrSSEStreamEnded) || !errors.Is(err, errBrokenWriter) {
		t.Errorf("Send() = %v, want the stream ended by the failure underneath it", err)
	}
}

func TestSSEFinishWaitsOutAWriteItInterrupted(t *testing.T) {
	t.Parallel()
	stream := newTestStream(t, SSEOptions{WriteTimeout: time.Second})
	if err := stream.acquire(); err != nil {
		t.Fatalf("acquire() = %v", err)
	}
	// The write gives up shortly after being interrupted, which is the case
	// finish is waiting for rather than the one it gives up on.
	go func() {
		time.Sleep(10 * time.Millisecond)
		stream.release()
	}()
	if !stream.finish() {
		t.Error("finish() gave up on a write that finished within the timeout")
	}
}

// deadlineWriter is a response writer that carries deadlines and counts the
// ones it is given, which is how a test sees the stream reaching for a
// response after it has handed it back.
type deadlineWriter struct {
	recorder  *httptest.ResponseRecorder
	failWrite bool
	deadlines atomic.Int64
}

func (d *deadlineWriter) Header() http.Header    { return d.recorder.Header() }
func (d *deadlineWriter) WriteHeader(status int) { d.recorder.WriteHeader(status) }
func (d *deadlineWriter) FlushError() error      { return nil }

func (d *deadlineWriter) Write(p []byte) (int, error) {
	if d.failWrite {
		return 0, errBrokenWriter
	}
	return d.recorder.Write(p)
}

func (d *deadlineWriter) SetWriteDeadline(time.Time) error {
	d.deadlines.Add(1)
	return nil
}

func (d *deadlineWriter) SetReadDeadline(time.Time) error { return nil }

func TestSSEStopsWatchingARequestItNeverStreamed(t *testing.T) {
	t.Parallel()
	// This stream got as far as watching its request and then failed on its
	// first event. What it arranged has to be undone before the response goes
	// back, or cancelling the request later would set a deadline on a
	// connection that by then belongs to somebody else.
	app := New(quietOptions())
	app.SSE("/stream", streamItems("never"), WithSSE(SSEOptions{Retry: time.Second}))
	mustBuild(t, app)

	writer := &deadlineWriter{recorder: httptest.NewRecorder(), failWrite: true}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	app.ServeHTTP(writer, request)

	handedBack := writer.deadlines.Load()
	cancel()
	// A moment for the arrangement to fire, if it is still there to fire.
	time.Sleep(20 * time.Millisecond)
	if reached := writer.deadlines.Load(); reached != handedBack {
		t.Errorf("the stream touched the response %d times after handing it back", reached-handedBack)
	}
	if held := app.streams.count(); held != 0 {
		t.Errorf("the register still holds %d streams", held)
	}
}
