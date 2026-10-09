package main

import (
	"os"
	"path/filepath"
	"testing"

	"muzak.dev/framework"
)

// The fixtures are documents of small applications, generated as each test
// runs, so they are always documents this version of the framework writes.

type itemIn struct {
	ID string `path:"id" doc:"Which item"`
}

type itemOut struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type statusIn struct {
	Status string `json:"status"`
}

type statusOut struct {
	Status string `json:"status"`
}

// newFixtureApp returns an application quiet enough to build in a test, with
// a scheme of each kind declared.
func newFixtureApp() *muzak.App {
	return muzak.New(muzak.AppOptions{
		Title:         "Fixture",
		Version:       "1.0.0",
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
		SecuritySchemes: map[string]muzak.SecurityScheme{
			"bearer": muzak.BearerAuth("JWT"),
			"key":    muzak.APIKeyHeader("X-Key"),
			"oauth": muzak.OAuth2(muzak.OAuthFlows{ClientCredentials: &muzak.OAuthFlow{
				TokenURL: "https://auth.example.com/token",
				Scopes:   map[string]string{"items:read": "Read items.", "items:write": "Change items."},
			}}),
		},
	})
}

func listItems(*muzak.Context, muzak.Empty) ([]itemOut, error)   { return nil, nil }
func createItem(*muzak.Context, statusIn) (itemOut, error)       { return itemOut{}, nil }
func readItem(*muzak.Context, itemIn) (itemOut, error)           { return itemOut{}, nil }
func deleteItem(*muzak.Context, itemIn) (muzak.Empty, error)     { return muzak.Empty{}, nil }
func health(*muzak.Context, muzak.Empty) (statusOut, error)      { return statusOut{}, nil }
func listUsers(*muzak.Context, muzak.Empty) ([]statusOut, error) { return nil, nil }

// routesApp is the application the routes golden file describes: every way
// an operation can declare security, a deprecated operation, and one with no
// summary.
func routesApp() *muzak.App {
	app := newFixtureApp()
	app.Get("/items", listItems, muzak.Summary("List items"), muzak.Public())
	app.Post("/items", createItem, muzak.Summary("Create an item"),
		muzak.WithSecurity(muzak.Require("oauth", "items:write", "items:read")))
	app.Get("/items/{id}", readItem, muzak.Summary("Read an item"),
		muzak.WithSecurity(muzak.Require("bearer"), muzak.SecurityRequirement{"key": nil, "bearer": nil}))
	app.Delete("/items/{id}", deleteItem, muzak.Summary("Delete an item"), muzak.Deprecated(),
		muzak.WithSecurity(muzak.Require("oauth", "items:write")))
	app.Get("/health", health)
	return app
}

// diffApp is the application the diff tests compare versions of. Each
// version leaves out the operations it names and adds GET /users when told.
func diffApp(without map[string]bool, users bool) *muzak.App {
	app := newFixtureApp()
	app.Get("/items", listItems, muzak.Summary("List items"))
	if !without["create"] {
		app.Post("/items", createItem, muzak.Summary("Create an item"))
	}
	if !without["delete"] {
		app.Delete("/items/{id}", deleteItem, muzak.Summary("Delete an item"), muzak.Deprecated())
	}
	if users {
		app.Get("/users", listUsers, muzak.Summary("List users"))
	}
	return app
}

// documentOf returns the document of an application, as JSON.
func documentOf(t *testing.T, app *muzak.App) []byte {
	t.Helper()
	doc, err := app.Document()
	if err != nil {
		t.Fatal(err)
	}
	data, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeDocument writes a document into dir and returns its path.
func writeDocument(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
