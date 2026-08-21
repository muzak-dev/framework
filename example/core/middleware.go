package core

import (
	"net/http"
	"strconv"
	"time"

	"badele"
)

// ProcessTime reports how long the service spent on a request in an
// X-Process-Time header, in seconds.
//
// It is the Go counterpart of FastAPI's middleware example, and it is written
// differently for a reason worth knowing. FastAPI can set a header after
// call_next returns:
//
//	@app.middleware("http")
//	async def add_process_time_header(request, call_next):
//	    start = time.perf_counter()
//	    response = await call_next(request)
//	    response.headers["X-Process-Time"] = str(time.perf_counter() - start)
//	    return response
//
// That works because the response is an object in memory until it is handed
// back. In Go the header block goes out the moment WriteHeader is called, so
// the same shape here would set a header nobody ever receives, and it would do
// it silently. The duration is therefore filled in by a wrapper, at the last
// moment before the status leaves.
//
// The framework's own access log already records a duration. This exists to
// report it to the client rather than to the operator.
func ProcessTime() badele.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&processTimer{ResponseWriter: w, start: time.Now()}, r)
		})
	}
}

// processTimer stamps the elapsed time onto the response as it starts.
type processTimer struct {
	http.ResponseWriter
	start   time.Time
	stamped bool
}

// WriteHeader records the duration and forwards the status. time.Since reads
// the monotonic clock, so a clock adjustment mid-request cannot produce a
// negative duration.
func (w *processTimer) WriteHeader(status int) {
	if !w.stamped {
		w.stamped = true
		elapsed := time.Since(w.start).Seconds()
		w.Header().Set("X-Process-Time", strconv.FormatFloat(elapsed, 'f', 6, 64))
	}
	w.ResponseWriter.WriteHeader(status)
}

// Write stamps a response whose handler never set a status, which net/http
// treats as a 200.
func (w *processTimer) Write(b []byte) (int, error) {
	if !w.stamped {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush passes a flush through to whatever is underneath, so that installing
// this middleware cannot stop a handler from streaming.
func (w *processTimer) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the underlying writer to [http.ResponseController].
func (w *processTimer) Unwrap() http.ResponseWriter { return w.ResponseWriter }
