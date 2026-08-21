package badele_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"badele"
)

// UserOut is the response model used by the examples. Because a handler
// returns this type, it is exactly what the client receives.
type UserOut struct {
	Username string `json:"username"`
}

// Params binds the username from the path template.
type Params struct {
	Username string `path:"username" doc:"The username to look up"`
}

// ExampleNew builds an application, registers a route on it directly, and
// serves one request.
func ExampleNew() {
	app := badele.New(badele.AppOptions{
		Title:         "Bigger Applications Example",
		Version:       "1.0.0",
		Addr:          ":8080",
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})

	app.Get("/users/me", func(ctx *badele.Context, _ badele.Empty) (UserOut, error) {
		return UserOut{Username: "fakecurrentuser"}, nil
	})

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/me", nil))

	fmt.Println(rec.Code)
	fmt.Println(rec.Body.String())
	// Output:
	// 200
	// {"username":"fakecurrentuser"}
}

// ExampleRouter_Get registers a route whose input is bound from the path. The
// type arguments are inferred from the handler literal, so they never appear at
// the call site.
func ExampleRouter_Get() {
	r := badele.NewRouter(badele.WithTags("users"))

	r.Get("/users/{username}", func(ctx *badele.Context, in Params) (UserOut, error) {
		return UserOut{Username: in.Username}, nil
	})

	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})
	app.Include(r)

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/users/rick", nil))

	fmt.Println(rec.Body.String())
	// Output: {"username":"rick"}
}

// ExampleApp_Include composes independently written routers, applying a prefix
// and a guard where the router is mounted rather than where it is defined.
func ExampleApp_Include() {
	admin := badele.NewRouter()
	admin.Post("/", func(ctx *badele.Context, in struct {
		Name string `json:"name"`
	}) (map[string]string, error) {
		return map[string]string{"name": in.Name}, nil
	}, badele.Status(http.StatusCreated))

	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})
	app.Include(admin,
		badele.WithPrefix("/admin"),
		badele.WithTags("admin"),
		badele.WithDependencies(badele.RequireHeaderToken("X-Token", "coneofsilence")),
		badele.WithResponseDoc(http.StatusTeapot, "I'm a teapot"),
	)

	send := func(token string) {
		req := httptest.NewRequest(http.MethodPost, "/admin/", strings.NewReader(`{"name":"schwifty"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("X-Token", token)
		}
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		fmt.Println(rec.Code)
	}

	send("coneofsilence")
	send("hailhydra")
	// Output:
	// 201
	// 401
}

// ExampleStatus shows the two ways a status is chosen: declared once when it
// never changes, and set imperatively when it depends on the request.
func ExampleStatus() {
	type CreateBody struct {
		Name  string `json:"name"`
		Async bool   `json:"async,omitzero"`
	}
	type ItemOut struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})
	app.Post("/items/", func(ctx *badele.Context, in CreateBody) (ItemOut, error) {
		if in.Async {
			ctx.SetStatus(http.StatusAccepted)
		}
		return ItemOut{ID: "42", Name: in.Name}, nil
	}, badele.Status(http.StatusCreated))

	send := func(body string) {
		req := httptest.NewRequest(http.MethodPost, "/items/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, req)
		fmt.Println(rec.Code, rec.Body.String())
	}

	send(`{"name":"Portal Gun"}`)
	send(`{"name":"Portal Gun","async":true}`)
	// Output:
	// 201 {"id":"42","name":"Portal Gun"}
	// 202 {"id":"42","name":"Portal Gun"}
}

// ExampleFrom retrieves a value dependency inside a handler. The type argument
// is checked by the compiler and no cast appears in application code.
func ExampleFrom() {
	type CurrentUser struct{ Username string }
	type ItemOut struct {
		ID    string `json:"id"`
		Owner string `json:"owner"`
	}

	getCurrentUser := func(ctx *badele.Context) (CurrentUser, error) {
		if ctx.Header("Authorization") == "" {
			return CurrentUser{}, badele.NewHTTPError(http.StatusUnauthorized, "unauthorized")
		}
		return CurrentUser{Username: "fakecurrentuser"}, nil
	}

	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})
	app.Get("/items/{id}", func(ctx *badele.Context, in struct {
		ID string `path:"id"`
	}) (ItemOut, error) {
		user := badele.From[CurrentUser](ctx)
		return ItemOut{ID: in.ID, Owner: user.Username}, nil
	}, badele.Needs(getCurrentUser))

	req := httptest.NewRequest(http.MethodGet, "/items/plumbus", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, req)

	fmt.Println(rec.Body.String())
	// Output: {"id":"plumbus","owner":"fakecurrentuser"}
}

// ExampleWithDependencies attaches a guard to the whole application, which
// every route beneath it inherits.
func ExampleWithDependencies() {
	getQueryToken := func(ctx *badele.Context) error {
		if ctx.Query("token") == "" {
			return badele.NewHTTPError(http.StatusBadRequest, "token is required")
		}
		return nil
	}

	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	}, badele.WithDependencies(getQueryToken))

	app.Get("/users/me", func(ctx *badele.Context, _ badele.Empty) (UserOut, error) {
		return UserOut{Username: "fakecurrentuser"}, nil
	})

	for _, target := range []string{"/users/me?token=jessica", "/users/me"} {
		rec := httptest.NewRecorder()
		app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		fmt.Println(rec.Code)
	}
	// Output:
	// 200
	// 400
}

// ExampleNewHTTPError shows the error envelope a rejected request produces.
func ExampleNewHTTPError() {
	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})
	app.Get("/items/{id}", func(ctx *badele.Context, in struct {
		ID string `path:"id"`
	}) (UserOut, error) {
		return UserOut{}, badele.NewHTTPError(http.StatusNotFound, "Item not found")
	})

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/items/missing", nil))

	// The request identifier varies per request, so only the error itself is
	// printed here.
	var envelope badele.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		fmt.Println("decode failed:", err)
		return
	}
	fmt.Println(rec.Code)
	fmt.Printf("%s %s\n", envelope.Error.Code, envelope.Error.Message)
	// Output:
	// 404
	// not_found Item not found
}

// ExampleLoadConfig reads settings from an explicit set of values, which is
// what a test supplies in place of the environment.
func ExampleLoadConfig() {
	type Settings struct {
		AppName      string `env:"APP_NAME" default:"Awesome API"`
		AdminEmail   string `env:"ADMIN_EMAIL" required:"true"`
		ItemsPerUser int    `env:"ITEMS_PER_USER" default:"50"`
	}

	settings, err := badele.LoadConfig[Settings](
		badele.WithoutEnvironment(),
		badele.ConfigValues(map[string]string{"ADMIN_EMAIL": "admin@example.com"}),
	)
	if err != nil {
		fmt.Println("could not load:", err)
		return
	}
	fmt.Println(settings.AppName, settings.AdminEmail, settings.ItemsPerUser)
	// Output: Awesome API admin@example.com 50
}

// ExampleWithSingleton publishes a value to every handler, retrieved by type.
func ExampleWithSingleton() {
	type Settings struct{ AppName string }
	type InfoOut struct {
		AppName string `json:"app_name"`
	}

	app := badele.New(badele.AppOptions{
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	}, badele.WithSingleton(Settings{AppName: "Awesome API"}))

	app.Get("/info", func(ctx *badele.Context, _ badele.Empty) (InfoOut, error) {
		s := badele.From[Settings](ctx)
		return InfoOut{AppName: s.AppName}, nil
	})

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/info", nil))
	fmt.Println(rec.Body.String())
	// Output: {"app_name":"Awesome API"}
}
