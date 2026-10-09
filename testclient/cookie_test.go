package testclient_test

import (
	"net/http"
	"testing"

	"muzak.dev/framework"
	"muzak.dev/framework/testclient"
)

// echoApp answers with what the request carried that the options below set.
func echoApp() *muzak.App {
	app := muzak.New(muzak.AppOptions{
		LoggerOptions: muzak.LoggerOptions{Format: muzak.LogFormatNone},
	})
	app.Get("/session", func(ctx *muzak.Context, _ muzak.Empty) (map[string]string, error) {
		ctx.SetCookie(&http.Cookie{Name: "session", Value: "from-the-jar", Path: "/"})
		return map[string]string{}, nil
	})
	app.Get("/echo", func(ctx *muzak.Context, _ muzak.Empty) (map[string]any, error) {
		cookies := map[string]string{}
		for _, cookie := range ctx.Request().Cookies() {
			cookies[cookie.Name] = cookie.Value
		}
		return map[string]any{"host": ctx.Request().Host, "cookies": cookies}, nil
	})
	return app
}

// TestCookieSendsOnlyTheNameAndValue is the regression test for a cookie sent
// in the syntax a server sets one with. Cookie wrote cookie.String() into the
// request, so a cookie built as the response of a login would have built it,
// with a Path or HttpOnly, reached the server as "a=b; Path=/; HttpOnly", and
// the server read a second cookie named Path. Two Cookie options were also
// sent as two header fields, the second of which the client dropped when the
// jar added a cookie of its own.
func TestCookieSendsOnlyTheNameAndValue(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, echoApp())
	client.Get("/session").AssertStatus(http.StatusOK)

	client.Get("/echo",
		testclient.Cookie(&http.Cookie{Name: "a", Value: "b", Path: "/", Domain: "example.com", HttpOnly: true, Secure: true}),
		testclient.Cookie(&http.Cookie{Name: "c", Value: "d", MaxAge: 60}),
	).AssertStatus(http.StatusOK).AssertJSON(`{
		"host": "` + client.URL()[len("http://"):] + `",
		"cookies": {"a": "b", "c": "d", "session": "from-the-jar"}
	}`)
}
