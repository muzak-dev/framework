package muzak

import (
	"bufio"
	"bytes"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A handler behind Compress that takes the connection over through
// http.ResponseController reached net/http through Unwrap, past the wrapper,
// which went on believing it owed a response: when the handler returned it
// sent the held header, and net/http logged "response.WriteHeader on hijacked
// connection". These run a real server, because a recorder cannot be hijacked
// and so never shows it.

// lockedLog collects what a server logs, for a test to read afterwards.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestCompressDoesNotWriteOntoAConnectionTheHandlerTookOver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// before is what the handler does before it takes the connection.
		before func(w http.ResponseWriter)
	}{
		{"with nothing written", func(http.ResponseWriter) {}},
		{"with part of a body held", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("too short to decide on"))
		}},
		{"with the compressor started", func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "5000")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strings.Repeat("compressible ", 100)))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			returned := make(chan struct{})
			compressed := Compress(CompressionOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.before(w)
				conn, rw, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("Hijack() = %v", err)
					return
				}
				_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n")
				_ = rw.Flush()
				_ = conn.Close()
			}))
			logs := &lockedLog{}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(returned)
				compressed.ServeHTTP(w, r)
			}))
			server.Config.ErrorLog = log.New(logs, "", 0)
			server.Start()
			t.Cleanup(server.Close)

			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nAccept-Encoding: gzip\r\n\r\n"))
			_, _ = bufio.NewReader(conn).ReadString('\n')

			select {
			case <-returned:
			case <-time.After(5 * time.Second):
				t.Fatal("the handler did not return")
			}
			if got := logs.String(); strings.Contains(got, "hijacked") {
				t.Errorf("the server logged %q, want nothing written after the hijack", got)
			}
		})
	}
}

func TestCompressHijackReportsAWriterThatCannotBeTakenOver(t *testing.T) {
	t.Parallel()
	// A recorder cannot be hijacked, which is what an HTTP/2 response is like.
	// The failure is reported and the response is left to be written as usual.
	handler := Compress(CompressionOptions{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("Hijack() = %v, want ErrNotSupported", err)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("still answered"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "still answered" {
		t.Errorf("response = %d %q, want the handler's answer", rec.Code, rec.Body.String())
	}
}
