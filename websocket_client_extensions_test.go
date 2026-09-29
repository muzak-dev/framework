package muzak

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"
)

// A 101 that names an extension the client never offered is a server that has
// decided the frames mean something other than what RFC 6455 says they do, and
// the client has no way to read them. The handshake has to fail, whatever the
// frame parser would go on to refuse.
func TestWSDialRefusesAnExtensionItDidNotOffer(t *testing.T) {
	t.Parallel()
	server := newRawHandshakeServer(t, "Sec-WebSocket-Extensions: permessage-deflate\r\n")

	conn, _, err := WSDial(t.Context(), "ws://"+server, WSDialOptions{})
	if err == nil {
		_ = conn.Close(WSStatusNormalClosure, "")
		t.Fatal("WSDial accepted a 101 that negotiated an extension nobody offered")
	}
	if !strings.Contains(err.Error(), "extension") {
		t.Errorf("WSDial() = %v, want an error that names the extension", err)
	}
}

// newRawHandshakeServer answers any handshake with a 101 carrying the given
// extra header lines, and reports its address.
func newRawHandshakeServer(t *testing.T, extra string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				accept, _ := wsAcceptKey(request.Header.Get("Sec-WebSocket-Key"))
				_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
					"Sec-WebSocket-Accept: " + accept + "\r\n" + extra + "\r\n"))
				_, _ = bufio.NewReader(conn).Peek(1)
			}()
		}
	}()
	return listener.Addr().String()
}
