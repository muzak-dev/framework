package muzak

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// endpointServer serves an application built by register over a real
// listener, so that what a call writes goes through net/http on both sides,
// and returns a client whose BaseURL points at it.
func endpointServer(t testing.TB, register func(app *App), opts ...func(*ClientOptions)) *Client {
	t.Helper()
	app := New(quietOptions())
	register(app)
	if err := app.Build(); err != nil {
		t.Fatalf("build: %v", err)
	}
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	options := ClientOptions{AllowPrivateNetworks: true, BaseURL: srv.URL, MaxAttempts: 1}
	for _, opt := range opts {
		opt(&options)
	}
	client := NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// capture records the inputs a handler received, so a test can compare what
// arrived with what was sent.
type capture[In any] struct {
	mu   sync.Mutex
	seen []In
}

func (c *capture[In]) record(in In) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, in)
}

func (c *capture[In]) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.seen)
}

func (c *capture[In]) last(t testing.TB) In {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		t.Fatal("the handler was never called")
	}
	return c.seen[len(c.seen)-1]
}

// assertCallRefused fails unless err is a refusal to send, whose message mentions
// every one of the fragments given.
func assertCallRefused(t testing.TB, err error, fragments ...string) {
	t.Helper()
	if !errors.Is(err, ErrCallRefused) {
		t.Fatalf("want a refusal wrapping ErrCallRefused, got %v", err)
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("refusal %q does not mention %q", err, fragment)
		}
	}
	if !strings.HasPrefix(err.Error(), "muzak: ") {
		t.Errorf("refusal %q does not start with muzak:", err)
	}
}

// assertErrorMentions fails unless err is non-nil and mentions each fragment.
func assertErrorMentions(t testing.TB, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error %q does not mention %q", err, fragment)
		}
	}
}

// assertEqualValue fails when got and want differ, showing both.
func assertEqualValue[T any](t testing.TB, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the value:\n got  %#v\n want %#v", got, want)
	}
}
