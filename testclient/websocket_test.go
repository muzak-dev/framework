package testclient_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"badele"
	"badele/testclient"
)

// wsEchoIn is the input of the echoing route, so that the test covers a
// handshake that binds something as well as one that binds nothing.
type wsEchoIn struct {
	Room  string `path:"room"`
	Token string `query:"token" required:"true"`
}

// newWSApp builds an application with the WebSocket routes these tests drive.
func newWSApp() *badele.App {
	app := badele.New(badele.AppOptions{
		Title:         "WebSocket Test API",
		Version:       "1.0.0",
		LoggerOptions: badele.LoggerOptions{Format: badele.LogFormatNone},
	})
	app.WS("/rooms/{room}/ws", func(ctx *badele.Context, in wsEchoIn, conn *badele.WSConn) error {
		if err := conn.WriteText(ctx.Context(), "welcome to "+in.Room+" as "+in.Token); err != nil {
			return err
		}
		for {
			message, err := conn.ReadText(ctx.Context())
			if err != nil {
				return nil
			}
			if err := conn.WriteText(ctx.Context(), strings.ToUpper(message)); err != nil {
				return err
			}
		}
	}, badele.WithWebSocket(badele.WSOptions{Subprotocols: []string{"chat.v1"}}))

	app.WS("/session/ws", func(ctx *badele.Context, _ badele.Empty, conn *badele.WSConn) error {
		cookie, err := ctx.Cookie("session")
		if err != nil {
			return &badele.WSCloseError{Status: badele.WSStatusPolicyViolation, Reason: "no session"}
		}
		return conn.WriteText(ctx.Context(), "session is "+cookie.Value)
	})
	return app
}

func TestClientWS(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newWSApp())
	conn := client.WS("/rooms/lobby/ws",
		testclient.Query("token", "jessica"),
		testclient.Subprotocols("chat.v1"))

	if got := conn.Subprotocol(); got != "chat.v1" {
		t.Errorf("Subprotocol() = %q, want %q", got, "chat.v1")
	}
	greeting, err := conn.ReadText(t.Context())
	if err != nil {
		t.Fatalf("ReadText = %v", err)
	}
	if greeting != "welcome to lobby as jessica" {
		t.Errorf("greeting = %q", greeting)
	}
	if err := conn.WriteText(t.Context(), "hello"); err != nil {
		t.Fatalf("WriteText = %v", err)
	}
	reply, err := conn.ReadText(t.Context())
	if err != nil || reply != "HELLO" {
		t.Fatalf("ReadText = %q, %v, want %q", reply, err, "HELLO")
	}
}

func TestClientWSRefusedHandshake(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newWSApp())
	conn, response := client.TryWS("/rooms/lobby/ws")
	if conn != nil {
		t.Fatal("a connection was opened for a handshake that should have been refused")
	}
	response.AssertStatus(http.StatusUnprocessableEntity)
	response.AssertErrorCode(badele.CodeValidationError)
}

func TestClientWSCarriesTheCookieJar(t *testing.T) {
	t.Parallel()
	app := newWSApp()
	app.Get("/login", func(ctx *badele.Context, _ badele.Empty) (badele.Empty, error) {
		ctx.SetCookie(&http.Cookie{Name: "session", Value: "rick", Path: "/"})
		return badele.Empty{}, nil
	})
	client := testclient.New(t, app)
	client.Get("/login").AssertStatus(http.StatusOK)

	conn := client.WS("/session/ws")
	got, err := conn.ReadText(t.Context())
	if err != nil {
		t.Fatalf("ReadText = %v", err)
	}
	if got != "session is rick" {
		t.Errorf("the handler saw %q, want the cookie the jar was holding", got)
	}
}

func TestClientWSHandlerCloseReachesTheTest(t *testing.T) {
	t.Parallel()
	client := testclient.New(t, newWSApp())
	conn := client.WS("/session/ws")
	// No cookie was set, so the handler refuses on its own terms and the
	// status it chose is what the read reports.
	_, err := conn.ReadText(t.Context())
	status, ok := badele.WSCloseStatus(err)
	if !ok || status != badele.WSStatusPolicyViolation {
		t.Fatalf("read error = %v, want the policy violation the handler chose", err)
	}
	var closed *badele.WSCloseError
	if !errors.As(err, &closed) || closed.Reason != "no session" {
		t.Errorf("close reason = %v, want the handler's own", err)
	}
}
