package muzak

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWebSocketHandshakeRefusals(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/ws", wsEcho)
	mustBuild(t, app)

	cases := []struct {
		name    string
		headers map[string]string
		proto   [2]int
		status  int
		message string
	}{
		{
			name:    "an ordinary request is told to upgrade",
			status:  http.StatusUpgradeRequired,
			message: "must ask to upgrade",
		},
		{
			name:    "asking to upgrade to something else",
			headers: map[string]string{"Connection": "Upgrade", "Upgrade": "h2c"},
			status:  http.StatusUpgradeRequired,
			message: "must ask to upgrade",
		},
		{
			name:    "upgrade without the connection token",
			headers: map[string]string{"Upgrade": "websocket"},
			status:  http.StatusUpgradeRequired,
			message: "must ask to upgrade",
		},
		{
			name: "an unsupported protocol version",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket",
				"Sec-WebSocket-Version": "8", "Sec-WebSocket-Key": testWSKey,
			},
			status:  http.StatusUpgradeRequired,
			message: "version 13",
		},
		{
			name: "no version at all",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Key": testWSKey,
			},
			status:  http.StatusUpgradeRequired,
			message: "version 13",
		},
		{
			name: "a missing key",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13",
			},
			status:  http.StatusBadRequest,
			message: "Sec-WebSocket-Key",
		},
		{
			name: "a key that is not base64",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket",
				"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "not base64!",
			},
			status:  http.StatusBadRequest,
			message: "Sec-WebSocket-Key",
		},
		{
			name: "a key of the wrong length",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket",
				"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "c2hvcnQ=",
			},
			status:  http.StatusBadRequest,
			message: "sixteen",
		},
		{
			name: "an older version of HTTP",
			headers: map[string]string{
				"Connection": "Upgrade", "Upgrade": "websocket",
				"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": testWSKey,
			},
			proto:   [2]int{1, 0},
			status:  http.StatusUpgradeRequired,
			message: "HTTP/1.1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", "/ws", nil)
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			if tc.proto != [2]int{} {
				req.Proto = fmt.Sprintf("HTTP/%d.%d", tc.proto[0], tc.proto[1])
				req.ProtoMajor, req.ProtoMinor = tc.proto[0], tc.proto[1]
			}
			rec := doRequest(t, app, req)
			assertStatus(t, rec, tc.status)
			if body := decodeError(t, rec); !strings.Contains(body.Error.Message, tc.message) {
				t.Errorf("message = %q, want it to mention %q", body.Error.Message, tc.message)
			}
			if tc.status == http.StatusUpgradeRequired {
				for header, want := range map[string]string{
					"Upgrade":               "websocket",
					"Connection":            "Upgrade",
					"Sec-WebSocket-Version": "13",
				} {
					if got := rec.Header().Get(header); got != want {
						t.Errorf("%s = %q, want %q, so that a client can correct itself", header, got, want)
					}
				}
			}
		})
	}
}

func TestWebSocketRouteAnswersOtherMethods(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/ws", wsEcho)
	mustBuild(t, app)

	// HEAD is not offered, because there is no way to upgrade one, and the
	// Allow header has to say so rather than promising it.
	head := do(t, app, "HEAD", "/ws")
	assertStatus(t, head, http.StatusMethodNotAllowed)
	if got := head.Header().Get("Allow"); got != "GET, OPTIONS" {
		t.Errorf("Allow = %q, want %q", got, "GET, OPTIONS")
	}
	options := do(t, app, "OPTIONS", "/ws")
	assertStatus(t, options, http.StatusNoContent)
	if got := options.Header().Get("Allow"); got != "GET, OPTIONS" {
		t.Errorf("Allow = %q, want %q", got, "GET, OPTIONS")
	}
	post := do(t, app, "POST", "/ws")
	assertStatus(t, post, http.StatusMethodNotAllowed)
}

func TestWebSocketUpgradeNeedsAConnectionToTakeOver(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/ws", wsEcho)
	mustBuild(t, app)

	// A response recorder has no connection behind it, which is the same
	// position a handler is in on a transport that cannot be hijacked.
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", testWSKey)
	rec := doRequest(t, app, req)
	assertStatus(t, rec, http.StatusInternalServerError)
	if body := decodeError(t, rec); !strings.Contains(body.Error.Message, "cannot be upgraded") {
		t.Errorf("message = %q, want it to say the connection cannot be upgraded", body.Error.Message)
	}
}

// wsItemIn is the input of the example route, with one path parameter and one
// optional query parameter.
type wsItemIn struct {
	ItemID string `path:"item_id"`
	Q      *int   `query:"q" doc:"An optional number echoed back"`
}

// wsSession is the value a dependency hands to the example handler.
type wsSession struct {
	Token string
}

// getWSSession is the dependency the example route declares.
func getWSSession(ctx *Context) (wsSession, error) {
	if cookie, err := ctx.Cookie("session"); err == nil {
		return wsSession{Token: cookie.Value}, nil
	}
	token := ctx.Query("token")
	if token == "" {
		return wsSession{}, NewHTTPError(http.StatusUnauthorized, "a session cookie or a token is required")
	}
	return wsSession{Token: token}, nil
}

func TestWebSocketBindsInputAndDependencies(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/items/{item_id}/ws", func(ctx *Context, in wsItemIn, conn *WSConn) error {
			session := From[wsSession](ctx)
			for {
				message, err := conn.ReadText(ctx.Context())
				if err != nil {
					return nil
				}
				if err := conn.WriteText(ctx.Context(), "token is "+session.Token); err != nil {
					return err
				}
				if in.Q != nil {
					if err := conn.WriteText(ctx.Context(), fmt.Sprintf("q is %d", *in.Q)); err != nil {
						return err
					}
				}
				if err := conn.WriteText(ctx.Context(), fmt.Sprintf("%q for item %s", message, in.ItemID)); err != nil {
					return err
				}
			}
		}, Needs(getWSSession))
	})

	t.Run("with a query token and a query parameter", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/items/plumbus/ws?token=jessica&q=42")
		conn.text("hello")
		conn.expectText("token is jessica")
		conn.expectText("q is 42")
		conn.expectText(`"hello" for item plumbus`)
	})

	t.Run("with a session cookie and no query parameter", func(t *testing.T) {
		t.Parallel()
		conn := dialWS(t, server.URL, "/items/portal-gun/ws", "Cookie", "session=rick")
		conn.text("hi")
		conn.expectText("token is rick")
		conn.expectText(`"hi" for item portal-gun`)
	})

	t.Run("a rejected dependency answers before any upgrade", func(t *testing.T) {
		t.Parallel()
		_, response := dialRaw(t, server.URL, "/items/plumbus/ws")
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("a malformed parameter answers before any upgrade", func(t *testing.T) {
		t.Parallel()
		_, response := dialRaw(t, server.URL, "/items/plumbus/ws?token=jessica&q=lots")
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnprocessableEntity)
		}
	})
}

func TestWebSocketGuardsRunBeforeTheUpgrade(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", wsEcho)
	}, WithDependencies(func(ctx *Context) error {
		if ctx.Header("X-Token") != "coneofsilence" {
			return NewHTTPError(http.StatusForbidden, "the token is wrong")
		}
		return nil
	}))

	_, refused := dialRaw(t, server.URL, "/ws")
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", refused.StatusCode, http.StatusForbidden)
	}
	conn := dialWS(t, server.URL, "/ws", "X-Token", "coneofsilence")
	conn.text("through the guard")
	conn.expectText("through the guard")
}

func TestWebSocketOriginPolicy(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/default", wsEcho)
		app.WS("/allowed", wsEcho, WithWebSocket(WSOptions{
			AllowedOrigins: []string{"https://app.example.com"},
		}))
		app.WS("/any", wsEcho, WithWebSocket(WSOptions{AllowedOrigins: []string{"*"}}))
		app.WS("/dynamic", wsEcho, WithWebSocket(WSOptions{
			AllowOriginFunc: func(_ *http.Request, origin string) bool {
				return strings.HasSuffix(origin, ".trusted.example")
			},
		}))
		app.WS("/open", wsEcho, WithWebSocket(WSOptions{InsecureSkipOriginCheck: true}))
	})
	host := strings.TrimPrefix(server.URL, "http://")

	cases := []struct {
		name   string
		path   string
		origin string
		status int
	}{
		{name: "no origin is not a browser", path: "/default", status: http.StatusSwitchingProtocols},
		{name: "the server's own origin", path: "/default", origin: "http://" + host, status: http.StatusSwitchingProtocols},
		{name: "the same host over TLS", path: "/default", origin: "https://" + host, status: http.StatusSwitchingProtocols},
		{name: "a different origin", path: "/default", origin: "https://evil.example", status: http.StatusForbidden},
		{name: "an origin on the list", path: "/allowed", origin: "https://app.example.com", status: http.StatusSwitchingProtocols},
		{name: "an origin on the list, in another case", path: "/allowed", origin: "https://APP.example.com", status: http.StatusSwitchingProtocols},
		{name: "an origin not on the list", path: "/allowed", origin: "https://other.example.com", status: http.StatusForbidden},
		{name: "the wildcard allows anything", path: "/any", origin: "https://evil.example", status: http.StatusSwitchingProtocols},
		{name: "a dynamic policy that accepts", path: "/dynamic", origin: "https://a.trusted.example", status: http.StatusSwitchingProtocols},
		{name: "a dynamic policy that refuses", path: "/dynamic", origin: "https://a.untrusted.example", status: http.StatusForbidden},
		{name: "an origin that is not a URL", path: "/default", origin: "://", status: http.StatusForbidden},
		{name: "the check turned off", path: "/open", origin: "https://evil.example", status: http.StatusSwitchingProtocols},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			headers := []string{}
			if tc.origin != "" {
				headers = append(headers, "Origin", tc.origin)
			}
			_, response := dialRaw(t, server.URL, tc.path, headers...)
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.status)
			}
		})
	}
}

func TestWebSocketSubprotocolNegotiation(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/chat", func(ctx *Context, _ Empty, conn *WSConn) error {
			return conn.WriteText(ctx.Context(), "speaking "+conn.Subprotocol())
		}, WithWebSocket(WSOptions{Subprotocols: []string{"chat.v2", "chat.v1"}}))
		app.WS("/plain", func(ctx *Context, _ Empty, conn *WSConn) error {
			return conn.WriteText(ctx.Context(), "speaking "+conn.Subprotocol())
		})
	})

	cases := []struct {
		name     string
		path     string
		offered  string
		chosen   string
		announce bool
	}{
		{name: "the client's first choice wins", path: "/chat", offered: "chat.v1, chat.v2", chosen: "chat.v1", announce: true},
		{name: "the first the server knows", path: "/chat", offered: "chat.v3, chat.v2", chosen: "chat.v2", announce: true},
		{name: "nothing in common", path: "/chat", offered: "chat.v9", chosen: ""},
		{name: "the route offers none", path: "/plain", offered: "chat.v1", chosen: ""},
		{name: "the client offers none", path: "/chat", chosen: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			headers := []string{}
			if tc.offered != "" {
				headers = append(headers, "Sec-WebSocket-Protocol", tc.offered)
			}
			conn, response := dialRaw(t, server.URL, tc.path, headers...)
			if response.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status = %d, want 101", response.StatusCode)
			}
			if got := response.Header.Get("Sec-WebSocket-Protocol"); got != tc.chosen {
				t.Errorf("Sec-WebSocket-Protocol = %q, want %q", got, tc.chosen)
			}
			if !tc.announce && response.Header.Get("Sec-WebSocket-Protocol") != "" {
				t.Error("a subprotocol was announced that the client did not offer")
			}
			conn.expectText("speaking " + tc.chosen)
		})
	}
}

func TestWebSocketHandshakeCarriesTheMiddlewareHeaders(t *testing.T) {
	t.Parallel()
	_, server := newWSTestApp(t, func(app *App) {
		app.WS("/ws", func(ctx *Context, _ Empty, conn *WSConn) error {
			return conn.WriteText(ctx.Context(), ctx.RequestID())
		})
	})
	conn, response := dialRaw(t, server.URL, "/ws")
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}
	id := response.Header.Get(HeaderRequestID)
	if id == "" {
		t.Fatal("the handshake response carries no request identifier")
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want the security headers on the handshake too", got)
	}
	if response.Header.Get("Content-Length") != "" {
		t.Error("the handshake response declares a body length, which would confuse the framing")
	}
	conn.expectText(id)
}

func TestWebSocketWorksBehindCompression(t *testing.T) {
	t.Parallel()
	logger, logs := captureLogger(t)
	opts := quietOptions()
	opts.Logger = logger
	app := New(opts)
	app.Use(Compress(CompressionOptions{}))
	app.WS("/ws", wsEcho)
	mustBuild(t, app)
	server := httptest.NewServer(app)
	t.Cleanup(server.Close)

	conn := dialWS(t, server.URL, "/ws", "Accept-Encoding", "gzip")
	conn.text("uncompressed, as a websocket must be")
	conn.expectText("uncompressed, as a websocket must be")
	conn.send(true, opClose, closePayload(1000, ""))
	conn.expectClose(1000)
	conn.expectEOF()

	// A wrapper that still believed it owed a response would write a header
	// onto the hijacked connection, which net/http reports rather than sends.
	if strings.Contains(logs.String(), "hijacked connection") {
		t.Errorf("something wrote to the hijacked connection:\n%s", logs.String())
	}
}

func TestWebSocketRegistrationErrors(t *testing.T) {
	t.Parallel()
	type withBody struct {
		Name string `json:"name"`
	}
	cases := []struct {
		name     string
		register func(*App)
		message  string
	}{
		{
			name:     "an input with a body field",
			register: func(app *App) { app.WS("/ws", func(*Context, withBody, *WSConn) error { return nil }) },
			message:  "carries no request body",
		},
		{
			name:     "a declared status",
			register: func(app *App) { app.WS("/ws", wsEcho, Status(http.StatusOK)) },
			message:  "Status cannot be declared",
		},
		{
			name:     "a nil handler",
			register: func(app *App) { app.WS("/ws", WSHandler[Empty](nil)) },
			message:  "handler is nil",
		},
		{
			name:     "a relative path",
			register: func(app *App) { app.WS("ws", wsEcho) },
			message:  "must begin with",
		},
		{
			name: "a path parameter the template does not declare",
			register: func(app *App) {
				app.WS("/ws", func(*Context, wsItemIn, *WSConn) error { return nil })
			},
			message: "does not declare",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			app := New(quietOptions())
			tc.register(app)
			if got := buildError(t, app); !strings.Contains(got, tc.message) {
				t.Errorf("build error = %q, want it to mention %q", got, tc.message)
			}
		})
	}
}

func TestWebSocketOptionsLayer(t *testing.T) {
	t.Parallel()
	opts := quietOptions()
	opts.WebSocket = WSOptions{ReadLimit: 111, WriteTimeout: time.Second, AllowedOrigins: []string{"https://app.example.com"}}
	app := New(opts)

	inherited := app.WS("/inherited", wsEcho)
	narrowed := app.WS("/narrowed", wsEcho, WithWebSocket(WSOptions{ReadLimit: 222}))

	nested := NewRouter()
	fromRouter := nested.WS("/router", wsEcho)
	fromRoute := nested.WS("/route", wsEcho, WithWebSocket(WSOptions{
		CloseGracePeriod: 5 * time.Millisecond,
		ReadLimit:        444,
	}))
	app.Include(nested, WithWebSocket(WSOptions{ReadLimit: 333, InsecureSkipOriginCheck: true}))
	mustBuild(t, app)

	cases := []struct {
		route            *Route
		readLimit        int64
		skipOrigin       bool
		closeGracePeriod time.Duration
	}{
		{route: inherited, readLimit: 111, closeGracePeriod: DefaultWSCloseGracePeriod},
		{route: narrowed, readLimit: 222, closeGracePeriod: DefaultWSCloseGracePeriod},
		{route: fromRouter, readLimit: 333, skipOrigin: true, closeGracePeriod: DefaultWSCloseGracePeriod},
		{route: fromRoute, readLimit: 444, skipOrigin: true, closeGracePeriod: 5 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.route.Path, func(t *testing.T) {
			got := tc.route.websocket.opts
			if got.ReadLimit != tc.readLimit {
				t.Errorf("ReadLimit = %d, want %d", got.ReadLimit, tc.readLimit)
			}
			if got.InsecureSkipOriginCheck != tc.skipOrigin {
				t.Errorf("InsecureSkipOriginCheck = %v, want %v", got.InsecureSkipOriginCheck, tc.skipOrigin)
			}
			if got.CloseGracePeriod != tc.closeGracePeriod {
				t.Errorf("CloseGracePeriod = %v, want %v", got.CloseGracePeriod, tc.closeGracePeriod)
			}
			// Whatever a narrower scope left alone stays as it was.
			if got.WriteTimeout != time.Second {
				t.Errorf("WriteTimeout = %v, want the application's own %v", got.WriteTimeout, time.Second)
			}
		})
	}
}

func TestWSOptionsWithDefaults(t *testing.T) {
	t.Parallel()
	filled := WSOptions{}.withDefaults()
	if filled.ReadLimit != DefaultWSReadLimit {
		t.Errorf("ReadLimit = %d, want %d", filled.ReadLimit, DefaultWSReadLimit)
	}
	if filled.WriteTimeout != DefaultWSWriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v", filled.WriteTimeout, DefaultWSWriteTimeout)
	}
	if filled.CloseGracePeriod != DefaultWSCloseGracePeriod {
		t.Errorf("CloseGracePeriod = %v, want %v", filled.CloseGracePeriod, DefaultWSCloseGracePeriod)
	}
	if filled.PingInterval != DefaultWSPingInterval {
		t.Errorf("PingInterval = %v, want %v: a peer that goes silent must not hold a slot for as long as it likes",
			filled.PingInterval, DefaultWSPingInterval)
	}
	if filled.PongTimeout != DefaultWSPongTimeout {
		t.Errorf("PongTimeout = %v, want %v", filled.PongTimeout, DefaultWSPongTimeout)
	}

	// A negative duration disables the bound, as it does everywhere else, and
	// there is deliberately no way to remove the read limit.
	disabled := WSOptions{ReadLimit: -1, WriteTimeout: -1, CloseGracePeriod: -1, PingInterval: -1}.withDefaults()
	if disabled.ReadLimit != DefaultWSReadLimit {
		t.Errorf("ReadLimit = %d, want the default rather than no limit", disabled.ReadLimit)
	}
	for name, got := range map[string]time.Duration{
		"WriteTimeout":     disabled.WriteTimeout,
		"CloseGracePeriod": disabled.CloseGracePeriod,
		"PingInterval":     disabled.PingInterval,
	} {
		if got != 0 {
			t.Errorf("%s = %v, want it disabled", name, got)
		}
	}

	if disabled.PongTimeout != 0 {
		t.Errorf("PongTimeout = %v, want none once keepalive is off", disabled.PongTimeout)
	}

	if huge := (WSOptions{ReadLimit: math.MaxInt64}).withDefaults(); huge.ReadLimit > math.MaxInt {
		t.Errorf("ReadLimit = %d, want no more than a slice can hold", huge.ReadLimit)
	}

	keepalive := WSOptions{PingInterval: time.Second}.withDefaults()
	if keepalive.PongTimeout != DefaultWSPongTimeout {
		t.Errorf("PongTimeout = %v, want %v once keepalive is on", keepalive.PongTimeout, DefaultWSPongTimeout)
	}
}

func TestWebSocketOpenAPI(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/items/{item_id}/ws", func(*Context, wsItemIn, *WSConn) error { return nil },
		Summary("Talk to an item"), WithTags("items"))
	mustBuild(t, app)

	rec := do(t, app, "GET", "/openapi.json")
	assertStatus(t, rec, http.StatusOK)
	var doc Document
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the document is not valid JSON: %v", err)
	}
	item, ok := doc.Paths["/items/{item_id}/ws"]
	if !ok || item.Get == nil {
		t.Fatalf("the document does not describe the route: %v", doc.Paths)
	}
	operation := item.Get
	if operation.RequestBody != nil {
		t.Error("the handshake is described as taking a body, which it never does")
	}
	switching, ok := operation.Responses["101"]
	if !ok {
		t.Fatalf("responses = %v, want one for 101", operation.Responses)
	}
	if !strings.Contains(switching.Description, "WebSocket") {
		t.Errorf("the 101 description %q does not say what happens", switching.Description)
	}
	if switching.Content != nil {
		t.Error("the 101 response is described with a body, which it has none of")
	}
	if _, ok := operation.Responses["200"]; ok {
		t.Error("the operation is described as answering 200, which it never does")
	}
	if _, ok := operation.Responses["426"]; !ok {
		t.Errorf("responses = %v, want one for a request that is not a handshake", operation.Responses)
	}
	if _, ok := operation.Responses["422"]; !ok {
		t.Errorf("responses = %v, want one for a handshake whose parameters do not bind", operation.Responses)
	}
	names := make([]string, 0, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		names = append(names, parameter.In+":"+parameter.Name)
	}
	if len(names) != 2 || names[0] != "path:item_id" || names[1] != "query:q" {
		t.Errorf("parameters = %v, want the path and query parameters of the handshake", names)
	}
	if operation.OperationID != "get_items_by_item_id_ws" {
		t.Errorf("operationId = %q", operation.OperationID)
	}
}

func TestWebSocketHiddenFromTheDocument(t *testing.T) {
	t.Parallel()
	app := New(quietOptions())
	app.WS("/internal/ws", wsEcho, Hidden())
	mustBuild(t, app)
	rec := do(t, app, "GET", "/openapi.json")
	if strings.Contains(rec.Body.String(), "/internal/ws") {
		t.Error("a hidden websocket route appears in the document")
	}
}

func TestWSAcceptKeyMatchesTheSpecification(t *testing.T) {
	t.Parallel()
	got, err := wsAcceptKey(testWSKey)
	if err != nil {
		t.Fatalf("wsAcceptKey = %v", err)
	}
	if got != testWSAccept {
		t.Errorf("wsAcceptKey(%q) = %q, want %q", testWSKey, got, testWSAccept)
	}
}

func TestHeaderHasToken(t *testing.T) {
	t.Parallel()
	header := http.Header{}
	header.Add("Connection", "keep-alive, Upgrade")
	header.Add("Connection", "close")
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "a token in a list", token: "upgrade", want: true},
		{name: "the first token", token: "keep-alive", want: true},
		{name: "a token on a repeated line", token: "close", want: true},
		{name: "a token that is not there", token: "trailers"},
		{name: "a prefix of a token", token: "up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := headerHasToken(header, "Connection", tc.token); got != tc.want {
				t.Errorf("headerHasToken(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
	if headerHasToken(header, "Upgrade", "websocket") {
		t.Error("a header that was never sent reported a token")
	}
}

func TestWSStatusAndMessageTypeNames(t *testing.T) {
	t.Parallel()
	for status, want := range map[WSStatus]string{
		WSStatusNormalClosure:       "1000 normal closure",
		WSStatusGoingAway:           "1001 going away",
		WSStatusProtocolError:       "1002 protocol error",
		WSStatusUnsupportedData:     "1003 unsupported data",
		WSStatusNoStatusReceived:    "1005 no status received",
		WSStatusAbnormalClosure:     "1006 abnormal closure",
		WSStatusInvalidFramePayload: "1007 invalid frame payload",
		WSStatusPolicyViolation:     "1008 policy violation",
		WSStatusMessageTooBig:       "1009 message too big",
		WSStatusMandatoryExtension:  "1010 mandatory extension",
		WSStatusInternalError:       "1011 internal error",
		WSStatusServiceRestart:      "1012 service restart",
		WSStatusTryAgainLater:       "1013 try again later",
		WSStatusBadGateway:          "1014 bad gateway",
		WSStatusTLSHandshake:        "1015 TLS handshake",
		WSStatus(4000):              "4000",
	} {
		if got := status.String(); got != want {
			t.Errorf("WSStatus(%d).String() = %q, want %q", uint16(status), got, want)
		}
	}
	for typ, want := range map[WSMessageType]string{
		WSText:            "text",
		WSBinary:          "binary",
		WSMessageType(99): "unknown message type 99",
	} {
		if got := typ.String(); got != want {
			t.Errorf("WSMessageType(%d).String() = %q, want %q", uint8(typ), got, want)
		}
	}
}

func TestWSCloseError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *WSCloseError
		want string
	}{
		{
			name: "a status alone",
			err:  &WSCloseError{Status: WSStatusNormalClosure},
			want: "closed with status 1000 normal closure",
		},
		{
			name: "a status and a reason",
			err:  &WSCloseError{Status: WSStatusPolicyViolation, Reason: "not for you"},
			want: "1008 policy violation: not for you",
		},
		{
			name: "a transport failure underneath",
			err:  &WSCloseError{Status: WSStatusAbnormalClosure, Reason: "the connection was lost", cause: http.ErrBodyNotAllowed},
			want: http.ErrBodyNotAllowed.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("Error() = %q, want it to contain %q", got, tc.want)
			}
			status, ok := WSCloseStatus(fmt.Errorf("wrapped: %w", tc.err))
			if !ok || status != tc.err.Status {
				t.Errorf("WSCloseStatus = %d, %v, want %d, true", status, ok, tc.err.Status)
			}
		})
	}
	if _, ok := WSCloseStatus(http.ErrBodyNotAllowed); ok {
		t.Error("WSCloseStatus reported a closure for an error that describes none")
	}
	lost := &WSCloseError{Status: WSStatusAbnormalClosure, cause: http.ErrBodyNotAllowed}
	if !strings.Contains(fmt.Sprintf("%v", lost.Unwrap()), "body") {
		t.Error("the transport failure is not reachable through Unwrap")
	}
}
