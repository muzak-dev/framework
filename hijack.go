package muzak

import (
	"bufio"
	"net"
	"net/http"
)

// Hijack takes the connection over from net/http, through whatever writer this
// one wraps, and on success tells every wrapper in the chain that the response
// is no longer theirs to write.
//
// [http.ResponseController] calls this rather than unwrapping past it, which
// is the point: the hijack [Context.ResponseWriter] documents used to reach
// net/http's own writer directly, so nothing here learned of it. The handler
// then returned, the framework wrote its response onto a socket that had
// stopped speaking HTTP, and the write that failed was logged as an aborted
// request, with the access log recording a 200 that never went out. The
// WebSocket route marked the chain itself after its own hijack; a handler that
// hijacks is now treated the same way without having to.
//
// A writer underneath that cannot be hijacked, such as one serving HTTP/2,
// reports [http.ErrNotSupported] as the controller would, and nothing is
// marked.
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	markHijacked(w)
	return conn, rw, nil
}
