package testclient_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// The application below mirrors the FastAPI testing tutorial, so the tests read
// the way the Python ones do.

type Item struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type itemParams struct {
	ID string `path:"item_id"`
}

type store struct {
	mu    sync.Mutex
	items map[string]Item
}

func newStore() *store {
	return &store{items: map[string]Item{
		"foo": {ID: "foo", Title: "Foo", Description: "There goes my hero"},
	}}
}

// buildApp returns an application with a shared secret guard and a small
// in-memory store.
func buildApp() *muzak.App {
	data := newStore()

	app := muzak.New(muzak.AppOptions{
		Title:         "Test Client Example",
		Version:       "1.0.0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	}, muzak.WithDependencies(muzak.RequireHeaderToken("X-Token", "coneofsilence")))

	app.Get("/items/{item_id}", func(ctx *muzak.Context, in itemParams) (Item, error) {
		data.mu.Lock()
		defer data.mu.Unlock()
		item, found := data.items[in.ID]
		if !found {
			return Item{}, muzak.NewHTTPError(http.StatusNotFound, "Item not found")
		}
		return item, nil
	})

	app.Post("/items/", func(ctx *muzak.Context, in Item) (Item, error) {
		data.mu.Lock()
		defer data.mu.Unlock()
		if _, exists := data.items[in.ID]; exists {
			return Item{}, muzak.NewHTTPError(http.StatusConflict, "Item already exists")
		}
		data.items[in.ID] = in
		return in, nil
	})

	app.Put("/items/{item_id}", func(ctx *muzak.Context, in struct {
		ID    string `path:"item_id"`
		Title string `json:"title"`
	}) (Item, error) {
		data.mu.Lock()
		defer data.mu.Unlock()
		item, found := data.items[in.ID]
		if !found {
			return Item{}, muzak.NewHTTPError(http.StatusNotFound, "Item not found")
		}
		item.Title = in.Title
		data.items[in.ID] = item
		return item, nil
	})

	app.Delete("/items/{item_id}", func(ctx *muzak.Context, in itemParams) (muzak.Empty, error) {
		data.mu.Lock()
		defer data.mu.Unlock()
		delete(data.items, in.ID)
		return muzak.Empty{}, nil
	}, muzak.Status(http.StatusNoContent))

	app.Patch("/items/{item_id}", func(ctx *muzak.Context, in itemParams) (Item, error) {
		return Item{ID: in.ID, Title: "patched"}, nil
	})

	app.Handle(http.MethodOptions, "/items/", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.SetHeader("X-Options", "yes")
		return muzak.Empty{}, nil
	}, muzak.Status(http.StatusNoContent))

	app.Get("/whoami", func(ctx *muzak.Context, in struct {
		Name string `query:"name"`
	}) (map[string]string, error) {
		return map[string]string{"name": in.Name}, nil
	})

	app.Get("/session", func(ctx *muzak.Context, _ muzak.Empty) (map[string]string, error) {
		if cookie, err := ctx.Cookie("session"); err == nil {
			return map[string]string{"session": cookie.Value}, nil
		}
		ctx.SetCookie(&http.Cookie{Name: "session", Value: "issued", Path: "/"})
		return map[string]string{"session": ""}, nil
	})

	app.Get("/plain", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		w := ctx.ResponseWriter()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("not json at all"))
		return muzak.Empty{}, nil
	})

	app.Get("/redirect", func(ctx *muzak.Context, _ muzak.Empty) (muzak.Empty, error) {
		ctx.SetHeader("Location", "/items/foo")
		ctx.SetStatus(http.StatusFound)
		return muzak.Empty{}, nil
	})

	return app
}

// authorized is the header every request in this suite needs.
func authorized() testclient.Option {
	return testclient.WithHeader("X-Token", "coneofsilence")
}

func TestReadItem(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	res := client.Get("/items/foo")

	res.AssertStatus(http.StatusOK)
	res.AssertJSON(`{"id":"foo","title":"Foo","description":"There goes my hero"}`)
}

func TestReadItemBadToken(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp())

	res := client.Get("/items/foo", testclient.Header("X-Token", "hailhydra"))

	res.AssertStatus(http.StatusUnauthorized)
	res.AssertErrorCode(muzak.CodeUnauthorized)
}

func TestReadNonexistentItem(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	res := client.Get("/items/baz")

	res.AssertStatus(http.StatusNotFound)
	if got := res.Error().Error.Message; got != "Item not found" {
		t.Errorf("message = %q", got)
	}
}

func TestCreateItem(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	res := client.Post("/items/", testclient.JSON(Item{
		ID: "foobar", Title: "Foo Bar", Description: "The Foo Barters",
	}))

	res.AssertStatus(http.StatusOK)
	res.AssertJSON(`{"id":"foobar","title":"Foo Bar","description":"The Foo Barters"}`)
}

func TestCreateExistingItem(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	res := client.Post("/items/", testclient.RawJSON(
		`{"id":"foo","title":"The Foo ID Stealers","description":"There goes my stealer"}`))

	res.AssertStatus(http.StatusConflict)
	res.AssertErrorCode(muzak.CodeConflict)
}

// TestDecodeIntoATypedValue exercises the generic method that keeps the
// expected shape visible and compiler-checked.
func TestDecodeIntoATypedValue(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	item := client.Get("/items/foo").Decode[Item]()
	if item.Title != "Foo" {
		t.Errorf("title = %q, want %q", item.Title, "Foo")
	}

	// The free-function form chains directly off a request.
	same := testclient.Decoded[Item](client.Get("/items/foo"))
	if same != item {
		t.Errorf("Decoded returned %+v, want %+v", same, item)
	}

	var into Item
	client.Get("/items/foo").JSON(&into)
	if into != item {
		t.Errorf("JSON decoded %+v, want %+v", into, item)
	}
}

func TestEveryMethodHelper(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	client.Put("/items/foo", testclient.RawJSON(`{"title":"Renamed"}`)).
		AssertStatus(http.StatusOK).
		AssertJSON(`{"id":"foo","title":"Renamed","description":"There goes my hero"}`)

	client.Patch("/items/foo").AssertStatus(http.StatusOK)
	client.Head("/items/foo").AssertStatus(http.StatusOK)
	client.Options("/items/").AssertStatus(http.StatusNoContent).AssertHeader("X-Options", "yes")
	client.Delete("/items/foo").AssertStatus(http.StatusNoContent)
	client.Get("/items/foo").AssertStatus(http.StatusNotFound)
	client.Do(http.MethodGet, "/items/baz").AssertStatus(http.StatusNotFound)
}

func TestQueryAndBodyOptions(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	client.Get("/whoami", testclient.Query("name", "rick")).
		AssertJSON(`{"name":"rick"}`)

	// A path that already carries a query string is extended, not replaced.
	client.Get("/whoami?name=morty").AssertJSON(`{"name":"morty"}`)

	// The separator is chosen to suit the path, so an option added to a path
	// that already has a query string joins it rather than starting a new one.
	client.Get("/whoami?name=summer", testclient.Query("ignored", "1")).
		AssertJSON(`{"name":"summer"}`)

	// A value the encoder cannot handle is reported rather than sent.
	if err := testclient.ApplyJSON(make(chan int)); err == nil {
		t.Error("an unencodable body was accepted")
	}

	client.Post("/items/", testclient.Body("application/json",
		strings.NewReader(`{"id":"raw","title":"Raw","description":"d"}`))).
		AssertStatus(http.StatusOK)
}

func TestResponseHelpers(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	res := client.Get("/items/foo")
	if res.RequestID() == "" {
		t.Error("no request identifier was reported")
	}
	if !strings.Contains(res.String(), `"title":"Foo"`) {
		t.Errorf("String() = %q", res.String())
	}
	res.AssertHeader("Content-Type", "application/json; charset=utf-8")
	if client.URL() == "" {
		t.Error("URL() returned nothing")
	}
	if client.HTTPClient() == nil {
		t.Error("HTTPClient() returned nil")
	}
}

func TestCookieJarCarriesASession(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())

	client.Get("/session").AssertJSON(`{"session":""}`)
	// The jar replays the cookie the first response set.
	client.Get("/session").AssertJSON(`{"session":"issued"}`)
}

func TestWithoutCookies(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized(), testclient.WithoutCookies())

	client.Get("/session").AssertJSON(`{"session":""}`)
	client.Get("/session").AssertJSON(`{"session":""}`)
}

func TestExplicitCookie(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized(), testclient.WithoutCookies())

	client.Get("/session", testclient.Cookie(&http.Cookie{Name: "session", Value: "supplied"})).
		AssertJSON(`{"session":"supplied"}`)
}

func TestWithoutRedirects(t *testing.T) {
	t.Parallel()
	following := testclient.New(t, buildApp(), authorized())
	following.Get("/redirect").AssertStatus(http.StatusOK)

	stopping := testclient.New(t, buildApp(), authorized(), testclient.WithoutRedirects())
	stopping.Get("/redirect").
		AssertStatus(http.StatusFound).
		AssertHeader("Location", "/items/foo")
}

func TestWithTimeout(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized(), testclient.WithTimeout(5*time.Second))
	client.Get("/items/foo").AssertStatus(http.StatusOK)
}

// TestLifecycleComponentsRunForTheClient checks that the client starts and
// stops the application's components around the test.
func TestLifecycleComponentsRunForTheClient(t *testing.T) {
	t.Parallel()
	var started, stopped bool
	var mu sync.Mutex

	app := muzak.New(muzak.AppOptions{
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	}, muzak.WithLifecycle(muzak.NewLifecycle("probe",
		func(context.Context) error { mu.Lock(); started = true; mu.Unlock(); return nil },
		func(context.Context) error { mu.Lock(); stopped = true; mu.Unlock(); return nil },
	)))
	app.Get("/x", func(ctx *muzak.Context, _ muzak.Empty) (map[string]bool, error) {
		return map[string]bool{"ok": true}, nil
	})

	t.Run("inner", func(t *testing.T) {
		client := testclient.New(t, app)
		client.Get("/x").AssertStatus(http.StatusOK)

		mu.Lock()
		defer mu.Unlock()
		if !started {
			t.Error("the component was not started for the client")
		}
	})

	// The subtest's cleanup has run by now, so the component is released.
	mu.Lock()
	defer mu.Unlock()
	if !stopped {
		t.Error("the component was not stopped when the test finished")
	}
}

// TestParallelRequests drives the client from several goroutines, which is
// worth running under the race detector.
func TestParallelRequests(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized(), testclient.WithoutCookies())

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Get("/items/foo").AssertStatus(http.StatusOK)
		}()
	}
	wg.Wait()
}

// TestAssertionCheckers exercises the decision behind each assertion directly.
//
// The assertions themselves report through testing.TB, so a test that drove a
// failing assertion would fail itself. The checkers carry the logic and return
// the message instead, which is what makes the behaviour testable.
func TestAssertionCheckers(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, buildApp(), authorized())
	res := client.Get("/items/foo")

	t.Run("status", func(t *testing.T) {
		if got := testclient.CheckStatus(res, http.StatusOK); got != "" {
			t.Errorf("a matching status reported %q", got)
		}
		got := testclient.CheckStatus(res, http.StatusTeapot)
		if !strings.Contains(got, "status = 200, want 418") {
			t.Errorf("message = %q", got)
		}
	})

	t.Run("header", func(t *testing.T) {
		if got := testclient.CheckHeader(res, "Content-Type", "application/json; charset=utf-8"); got != "" {
			t.Errorf("a matching header reported %q", got)
		}
		got := testclient.CheckHeader(res, "Content-Type", "text/plain")
		if !strings.Contains(got, "want \"text/plain\"") {
			t.Errorf("message = %q", got)
		}
	})

	t.Run("json", func(t *testing.T) {
		// Member order and whitespace are ignored.
		if got := testclient.CheckJSON(res, `{"description":"There goes my hero","title":"Foo","id":"foo"}`); got != "" {
			t.Errorf("a semantically equal body reported %q", got)
		}
		if got := testclient.CheckJSON(res, `{"id":"other"}`); !strings.Contains(got, "body mismatch") {
			t.Errorf("message = %q", got)
		}
		if got := testclient.CheckJSON(res, `not json`); !strings.Contains(got, "expected value is not valid JSON") {
			t.Errorf("message = %q", got)
		}
	})

	t.Run("error code", func(t *testing.T) {
		failing := client.Get("/items/missing")
		if got := testclient.CheckErrorCode(failing, muzak.CodeNotFound); got != "" {
			t.Errorf("a matching error code reported %q", got)
		}
		got := testclient.CheckErrorCode(failing, muzak.CodeConflict)
		if !strings.Contains(got, "error code =") {
			t.Errorf("message = %q", got)
		}
	})

	t.Run("json checker on a non-JSON body", func(t *testing.T) {
		text := client.Get("/plain")
		if got := testclient.CheckJSON(text, `{}`); !strings.Contains(got, "not valid JSON") {
			t.Errorf("message = %q", got)
		}
		if got := testclient.CheckErrorCode(text, "anything"); !strings.Contains(got, "not an error envelope") {
			t.Errorf("message = %q", got)
		}
	})
}
