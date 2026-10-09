// Package testclient exercises a Muzak application over a real HTTP
// connection from inside a Go test.
//
// The application is served in-process, requests go through the whole stack
// (middleware, routing, binding, dependencies, error rendering), and the test
// reads back a response it can assert on directly.
//
//	func TestReadItem(t *testing.T) {
//		client := testclient.New(t, buildApp())
//
//		res := client.Get("/items/foo", testclient.Header("X-Token", "coneofsilence"))
//
//		res.AssertStatus(http.StatusOK)
//		res.AssertJSON(`{"id":"foo","title":"Foo","description":"There goes my hero"}`)
//	}
//
// Responses can also be decoded into a typed value, with the type checked by
// the compiler rather than asserted at run time:
//
//	item := testclient.Decoded[ItemOut](client.Get("/items/foo"))
//	if item.Title != "Foo" {
//		t.Errorf("title = %q, want %q", item.Title, "Foo")
//	}
//
// The server, its listener and the application's lifecycle components are
// started when the client is created and released through the test's cleanup,
// so a test never has to remember to close anything. The server is the one
// App.Run would start, with the application's own ServerOptions, and the
// cleanup shuts it down as App.Shutdown does. A cookie jar is enabled
// by default, which lets a login followed by an authenticated call work the
// way it would in a browser.
package testclient
