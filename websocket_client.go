package muzak

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// WSDialOptions configures [WSDial].
//
// The zero value dials with the default HTTP client, offers no subprotocol and
// applies the same connection defaults a served connection gets.
type WSDialOptions struct {
	// HTTPClient issues the handshake request, which is how a dialer reaches a
	// server on a network of its own, such as the in-process one a test client
	// serves over. It defaults to a fresh [net/http.Client].
	//
	// A client with a Timeout is used with that timeout removed, because it
	// would otherwise apply to the whole life of the connection rather than to
	// the handshake and cut the conversation short.
	HTTPClient *http.Client

	// Header carries extra request headers, which is where an Authorization
	// header or a cookie belongs. The headers the handshake defines are set
	// afterwards and cannot be overridden.
	Header http.Header

	// Subprotocols lists the subprotocols to offer, in order of preference.
	// The server picks one of them or none at all, and a server that picks
	// something else is refused.
	Subprotocols []string

	// ReadLimit, ReadTimeout, WriteTimeout and CloseGracePeriod configure the
	// connection exactly as the matching fields of [WSOptions] do for a served
	// one.
	ReadLimit        int64
	ReadTimeout      time.Duration
	WriteTimeout     time.Duration
	CloseGracePeriod time.Duration
}

// WSDial opens a WebSocket connection to a server.
//
// It is the client side of the same engine that serves connections, which is
// what makes a WebSocket route testable end to end without a second
// implementation to disagree with the first. The URL may be written with
// either the ws and wss schemes or the http and https ones.
//
//	conn, _, err := muzak.WSDial(ctx, "ws://"+app.Addr()+"/items/plumbus/ws", muzak.WSDialOptions{})
//	if err != nil {
//		return err
//	}
//	defer conn.Close(muzak.WSStatusNormalClosure, "")
//
// The response is returned alongside the connection so that a caller can read
// the headers of the handshake, and on failure so that it can read the status
// and body the server refused with; its body has already been read into memory
// and may be read again. The connection is nil unless the handshake succeeded.
//
// Cancelling ctx aborts the handshake. Afterwards ctx has no further bearing on
// the connection, which is driven by the contexts passed to its own reads and
// writes.
func WSDial(ctx context.Context, rawURL string, opts WSDialOptions) (*WSConn, *http.Response, error) {
	target, err := wsRequestURL(rawURL)
	if err != nil {
		return nil, nil, err
	}
	var keyBytes [16]byte
	if _, err := crand.Read(keyBytes[:]); err != nil {
		// coverage: crypto/rand does not fail on any supported platform.
		return nil, nil, fmt.Errorf("muzak: generating a websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		// coverage: the URL was already parsed above, so building the request
		// cannot fail for any reason a caller could produce.
		return nil, nil, fmt.Errorf("muzak: building the websocket handshake: %w", err)
	}
	for name, values := range opts.Header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", key)
	if len(opts.Subprotocols) > 0 {
		request.Header.Set("Sec-WebSocket-Protocol", strings.Join(opts.Subprotocols, ", "))
	}

	response, err := wsDialClient(opts.HTTPClient).Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("muzak: the websocket handshake failed: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, wsBufferBody(response), fmt.Errorf(
			"muzak: the websocket handshake was refused with status %d", response.StatusCode)
	}

	conn, err := wsClientConn(response, key, opts)
	if err != nil {
		_ = response.Body.Close()
		return nil, response, err
	}
	return conn, response, nil
}

// wsClientConn verifies a 101 response and wraps the switched connection.
func wsClientConn(response *http.Response, key string, opts WSDialOptions) (*WSConn, error) {
	if !headerHasToken(response.Header, "Upgrade", "websocket") ||
		!headerHasToken(response.Header, "Connection", "upgrade") {
		return nil, fmt.Errorf("muzak: the server answered 101 without upgrading to websocket")
	}
	expected, err := wsAcceptKey(key)
	if err != nil {
		// coverage: the key was produced here from sixteen random bytes, so it
		// always decodes.
		return nil, err
	}
	if response.Header.Get("Sec-WebSocket-Accept") != expected {
		return nil, fmt.Errorf("muzak: the server's Sec-WebSocket-Accept header does not match the key that was sent")
	}
	if extensions := response.Header.Values("Sec-WebSocket-Extensions"); len(extensions) > 0 {
		// RFC 6455 section 4.1: a server may answer with an extension only if
		// the client asked for it, and this one never does. An extension
		// changes what a frame's bits mean, and a client that read on as if it
		// had not would be reading a conversation it does not understand.
		return nil, fmt.Errorf("muzak: the server negotiated the extension %q, which was not offered",
			wsShorten(strings.Join(extensions, ", ")))
	}
	subprotocol := response.Header.Get("Sec-WebSocket-Protocol")
	if subprotocol != "" && !slices.Contains(opts.Subprotocols, subprotocol) {
		// A server may only choose from what the client offered. Accepting
		// anything else would let it pick the protocol the conversation is
		// interpreted under.
		return nil, fmt.Errorf("muzak: the server chose the subprotocol %q, which was not offered", subprotocol)
	}
	transport, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		// coverage: net/http hands back a read-write body for every 101 that
		// carries the upgrade headers checked above.
		return nil, fmt.Errorf("muzak: the switched connection cannot be written to")
	}

	settings := WSOptions{
		ReadLimit:        opts.ReadLimit,
		ReadTimeout:      opts.ReadTimeout,
		WriteTimeout:     opts.WriteTimeout,
		CloseGracePeriod: opts.CloseGracePeriod,
	}.withDefaults()
	return newWSConn(transport, bufio.NewReader(transport), true, subprotocol, settings), nil
}

// wsDialClient returns the client to run the handshake with.
//
// Two things are changed about whatever the caller supplied, on a copy so that
// the caller's own client is left as it was. The timeout goes, because it
// would otherwise bound the whole life of the connection rather than the
// handshake and cut the conversation short. Redirects are refused, because
// following one would send the headers of the handshake, an Authorization
// header among them, to whatever host the answer named.
func wsDialClient(client *http.Client) *http.Client {
	dialer := &http.Client{}
	if client != nil {
		copied := *client
		dialer = &copied
	}
	dialer.Timeout = 0
	dialer.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return dialer
}

// wsRequestURL turns a WebSocket URL into the HTTP one the handshake is sent
// to, accepting either spelling.
func wsRequestURL(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("muzak: the websocket address could not be parsed: %w", err)
	}
	switch parsed.Scheme {
	case "ws", "http":
		parsed.Scheme = "http"
	case "wss", "https":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("muzak: %q is not a websocket address; use ws, wss, http or https", rawURL)
	}
	return parsed.String(), nil
}

// wsBufferBody reads a refused handshake's body into memory so that the caller
// can read it after the connection has been released.
func wsBufferBody(response *http.Response) *http.Response {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
	response.Body = io.NopCloser(strings.NewReader(string(body)))
	return response
}
