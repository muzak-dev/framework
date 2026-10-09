package muzak

import (
	"strconv"
	"strings"
	"testing"
)

// TestWebSocketRefusesAnOriginListEntryThatCanNeverMatch is the regression
// test for WSOptions.AllowedOrigins and AllowedHosts taking any string. An
// origin entry with a trailing slash, a path, a pattern or a default port
// never equals the Origin a browser sends, so the browser it was meant for was
// refused with nothing to say why, and "null" let every page that arranged to
// send it open a connection with the visitor's cookies. CORS has refused all
// of these since it learned to; the WebSocket list now does too, except for
// case, which it compares without regard to.
func TestWebSocketRefusesAnOriginListEntryThatCanNeverMatch(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"https://app.example.com/":    `write "https://app.example.com"`,
		"https://app.example.com/ws":  `write "https://app.example.com"`,
		"https://app.example.com:443": `write "https://app.example.com"`,
		"https://*.example.com":       "AllowOriginFunc",
		"null":                        "sandboxed",
		"app.example.com":             "scheme://host",
	}
	for entry, want := range refused {
		app := New(quietOptions())
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{AllowedOrigins: []string{"https://ok.example", entry}}))
		err := app.Build()
		if err == nil {
			t.Errorf("the AllowedOrigins entry %q was accepted", entry)
			continue
		}
		if msg := err.Error(); !strings.Contains(msg, "WS /ws: AllowedOrigins entry "+strconv.Quote(entry)) || !strings.Contains(msg, want) {
			t.Errorf("entry %q: error = %q, want it named with %q", entry, msg, want)
		}
	}

	// A list set for the whole application is held to the same rules on every
	// route it reaches, and every problem is reported at once.
	opts := quietOptions()
	opts.WebSocket = WSOptions{AllowedOrigins: []string{"null", "https://app.example.com/"}, AllowedHosts: []string{"https://app.example.com"}}
	app := New(opts)
	app.WS("/ws", wsEcho)
	err := app.Build()
	if err == nil {
		t.Fatal("an application-wide list with three mistakes built")
	}
	for _, want := range []string{`"null"`, `"https://app.example.com/"`, `AllowedHosts entry "https://app.example.com"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %s", err, want)
		}
	}

	for _, hosts := range [][]string{{"app.example.com/"}, {"*.example.com"}, {"user@app.example.com"}, {""}} {
		app := New(quietOptions())
		app.WS("/ws", wsEcho, WithWebSocket(WSOptions{AllowedHosts: hosts}))
		if err := app.Build(); err == nil || !strings.Contains(err.Error(), "AllowedHosts entry") {
			t.Errorf("AllowedHosts %q: error = %v, want it refused", hosts, err)
		}
	}

	// What can match still builds: the documented "*", any case, a port that
	// is not the default, a scheme a browser does not normalize, and hosts.
	app = New(quietOptions())
	app.WS("/ws", wsEcho, WithWebSocket(WSOptions{
		AllowedOrigins: []string{"*", "HTTPS://App.Example.com", "https://app.example.com:8443", "chrome-extension://abcdef"},
		AllowedHosts:   []string{"app.example.com", "APP.example.com:8443", "[::1]:8080"},
	}))
	if err := app.Build(); err != nil {
		t.Errorf("valid lists were refused: %v", err)
	}
}
