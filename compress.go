package muzak

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// CompressionLevel selects how hard the compressor works.
//
// It is a named type with three values rather than an integer, because the
// underlying levels are a range with invalid values in it and there is nothing
// useful to do with a level of 42 at run time. Choosing between the three is a
// judgement about CPU against bytes; there is no level that is wrong.
type CompressionLevel uint8

const (
	// CompressionDefault balances speed against size, which is the right
	// choice until a measurement says otherwise.
	CompressionDefault CompressionLevel = iota
	// CompressionFastest spends the least CPU per response, for a service that
	// is CPU bound or serving large bodies to a fast network.
	CompressionFastest
	// CompressionBest spends the most CPU for the smallest body, for a service
	// whose clients are on slow or metered connections.
	CompressionBest
)

// gzipLevel maps a level onto what compress/gzip and compress/flate accept.
func (l CompressionLevel) gzipLevel() int {
	switch l {
	case CompressionFastest:
		return gzip.BestSpeed
	case CompressionBest:
		return gzip.BestCompression
	default:
		return gzip.DefaultCompression
	}
}

// DefaultCompressionMinSize is the smallest body [Compress] will compress
// unless told otherwise.
//
// It is a little under one ethernet MTU: a response that already fits in a
// single packet cannot be made to arrive sooner by shrinking it, and
// compressing it spends CPU on both ends to save nothing.
const DefaultCompressionMinSize = 1400

// defaultCompressibleTypes are the media types compressed when a policy names
// none of its own.
//
// The list is deliberately short and made of types that are text underneath.
// Anything already compressed, which is most images, audio, video and archive
// formats, only grows when it is compressed again.
var defaultCompressibleTypes = []string{
	"text/",
	"application/json",
	"application/javascript",
	"application/xml",
	"application/wasm",
	"application/x-ndjson",
	"image/svg+xml",
	"+json",
	"+xml",
}

// CompressionOptions configures [Compress].
//
// The zero value is usable and compresses the common text types above
// [DefaultCompressionMinSize] at the default level.
type CompressionOptions struct {
	// Level selects how hard the compressor works. It defaults to
	// [CompressionDefault].
	Level CompressionLevel

	// MinSize is the smallest body worth compressing, in bytes. It defaults to
	// [DefaultCompressionMinSize]. A response whose length is not known in
	// advance is buffered up to this size before the decision is made.
	MinSize int

	// ContentTypes limits compression to responses whose media type matches
	// one of these entries. An entry is matched as a prefix, so "text/" covers
	// every text type, and an entry beginning with "+" matches a structured
	// syntax suffix, so "+json" covers "application/problem+json". When empty,
	// a built-in list of text-like types is used.
	ContentTypes []string
}

// withDefaults fills in the unset fields so the middleware can read the policy
// without repeating fallbacks per request.
func (o CompressionOptions) withDefaults() CompressionOptions {
	if o.MinSize <= 0 {
		o.MinSize = DefaultCompressionMinSize
	}
	if len(o.ContentTypes) == 0 {
		o.ContentTypes = defaultCompressibleTypes
	}
	return o
}

// compressible reports whether a response of this media type is worth
// compressing under the policy.
func (o CompressionOptions) compressible(contentType string) bool {
	if contentType == "" {
		// Nothing has declared what this is. Compressing an unknown body risks
		// spending CPU on something already compressed, so it is left alone.
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	// A server-sent event stream is text, and compressing it would hold each
	// event in the compressor's window until something forced it out, which is
	// the one thing a stream must not do.
	if mediaType == "text/event-stream" {
		return false
	}
	for _, candidate := range o.ContentTypes {
		if strings.HasPrefix(candidate, "+") {
			if strings.HasSuffix(mediaType, candidate) {
				return true
			}
			continue
		}
		if strings.HasPrefix(mediaType, candidate) {
			return true
		}
	}
	return false
}

// Compress returns middleware that compresses response bodies the client has
// said it can decode.
//
// Encoding is negotiated from Accept-Encoding, preferring gzip over deflate
// and honouring an explicit refusal such as "gzip;q=0". A response is left
// alone when the client asked for neither encoding, when the handler encoded
// it already, when its media type is not one the policy compresses, when it
// carries no body, and when it is smaller than [CompressionOptions.MinSize].
// Vary is set on every response either way, so a cache cannot hand a
// compressed body to a client that cannot read it.
//
// Install it with [App.Use]:
//
//	app.Use(muzak.Compress(muzak.CompressionOptions{}))
//
// Compression and secrecy interact badly. When a response mixes a secret with
// something the client controls, its compressed length leaks how much the two
// have in common, which is what the BREACH attack recovers a token from over
// many requests. Muzak's own responses do not mix the two, but a handler that
// reflects a query parameter back alongside a CSRF token does. Where that is
// possible, leave compression off for the route or stop reflecting the input.
func Compress(opts CompressionOptions) Middleware {
	policy := opts.withDefaults()
	pool := &compressorPool{level: policy.Level.gzipLevel()}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Every request is wrapped, including one from a client that
			// accepts no encoding this can produce. The wrapper is what
			// guarantees Vary, and setting it here instead would let a handler
			// that writes its own Vary replace it and leave a cache free to
			// hand a compressed body to a client that cannot read it.
			cw := &compressWriter{
				ResponseWriter: w,
				policy:         policy,
				pool:           pool,
				encoding:       negotiateEncoding(r.Header.Get("Accept-Encoding")),
			}
			// finish commits whatever is pending, which is right when the
			// handler returned and wrong when it panicked: sending a held
			// header then would put a 200 on the wire before Recovery could
			// answer 500, and closing the compressor would write the trailer
			// that makes a truncated body look complete. The flag is set only
			// on a normal return, so a panic (or runtime.Goexit) abandons the
			// response instead and continues on its way.
			returned := false
			defer func() {
				if returned {
					cw.finish()
					return
				}
				cw.abandon()
			}()
			next.ServeHTTP(cw, r)
			returned = true
		})
	}
}

// negotiateEncoding picks the encoding to use for a request, or the empty
// string when the client asked for none this middleware can produce.
//
// gzip is preferred over deflate because every client that accepts deflate
// accepts gzip, while the reverse is not true and deflate has a history of
// being sent in two incompatible framings.
func negotiateEncoding(header string) string {
	if header == "" {
		return ""
	}
	var deflate bool
	for entry := range strings.SplitSeq(header, ",") {
		name, quality, _ := strings.Cut(strings.TrimSpace(entry), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if refused(quality) {
			continue
		}
		switch name {
		case "gzip":
			return "gzip"
		case "deflate":
			deflate = true
		}
	}
	if deflate {
		return "deflate"
	}
	return ""
}

// refused reports whether an Accept-Encoding parameter list rejects the
// encoding it belongs to with a quality of zero.
//
// It is the yes-or-no form of [quality], which locale negotiation needs graded.
// Both read the same header syntax, so they share one parser rather than
// growing two that drift apart.
func refused(parameters string) bool {
	return quality(parameters) == 0
}

// compressorPool recycles the compressors for one policy, which each hold a
// window buffer far larger than the response they compress.
//
// The pool belongs to the policy rather than to the package because a pooled
// compressor keeps the level it was built with. Two applications in one
// process, or one application with a second policy on a subtree, would
// otherwise hand each other compressors set to the wrong level.
type compressorPool struct {
	level int
	gzip  sync.Pool
	flate sync.Pool
}

// compressWriter compresses what a handler writes, deciding whether to do so
// as late as it has to.
//
// The decision needs the status, the media type and the length. The first two
// are known when the header is written; the length is too when the handler
// declared Content-Length, which every response Muzak encodes does. A handler
// that writes the body itself declares no length, so the first writes are held
// until either the minimum size is passed or the handler finishes.
type compressWriter struct {
	http.ResponseWriter
	policy   CompressionOptions
	pool     *compressorPool
	encoding string

	status     int
	headerSent bool
	decided    bool
	// compressor is non-nil once the decision was to compress.
	compressor io.WriteCloser
	// held carries the body written before the decision could be made.
	held []byte
}

// WriteHeader decides from the response header whether to compress, and holds
// the header back when the body's length is not yet known.
func (w *compressWriter) WriteHeader(status int) {
	// A second call is ignored, the way net/http ignores one. The test for it
	// is the recorded status rather than headerSent, because the header can be
	// held back while the decision waits on the body's length, and a later
	// call would otherwise overwrite a status already chosen.
	if w.status != 0 {
		return
	}
	w.status = status

	if w.encoding == "" || !w.worthCompressing(status) {
		w.reject()
		return
	}

	// A declared length settles it immediately, which is the common case: the
	// framework encodes a response in full before writing a byte of it.
	if declared := w.Header().Get("Content-Length"); declared != "" {
		length, err := strconv.Atoi(declared)
		if err == nil && length < w.policy.MinSize {
			w.reject()
			return
		}
		w.accept()
		return
	}
	// The length is unknown, so the header waits until the body says.
}

// worthCompressing reports whether the response header describes a body worth
// compressing at all.
func (w *compressWriter) worthCompressing(status int) bool {
	switch {
	case status < http.StatusOK, status == http.StatusNoContent, status == http.StatusNotModified:
		// These carry no body to compress.
		return false
	case status == http.StatusPartialContent:
		// The range was computed against the uncompressed body, so compressing
		// it now would answer with bytes the client did not ask for.
		return false
	case w.Header().Get("Content-Encoding") != "":
		// The handler encoded this itself.
		return false
	}
	return w.policy.compressible(w.Header().Get("Content-Type"))
}

// accept commits to compressing and sends the header.
func (w *compressWriter) accept() {
	header := w.Header()
	header.Set("Content-Encoding", w.encoding)
	// The declared length described the uncompressed body, and the compressed
	// one is not known until it has been written.
	header.Del("Content-Length")
	// A strong validator has to change when the representation does.
	if tag := header.Get("ETag"); tag != "" && !strings.HasPrefix(tag, "W/") {
		header.Set("ETag", "W/"+tag)
	}

	w.decided = true
	w.compressor = w.pool.get(w.encoding, w.ResponseWriter)
	w.sendHeader()
}

// reject commits to passing the body through untouched and sends the header.
func (w *compressWriter) reject() {
	w.decided = true
	w.sendHeader()
}

// sendHeader forwards the status, defaulting it the way net/http does for a
// handler that wrote a body without one.
//
// It carries no guard against being called twice, because it cannot be: both
// callers commit the decision before calling it, and every entry point returns
// early once that decision is made.
func (w *compressWriter) sendHeader() {
	w.headerSent = true
	// This is the last moment the handler could have replaced the header, so
	// it is the only safe place to make this promise.
	addVaryAcceptEncoding(w.Header())
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
}

// Write compresses the body, or holds it until there is enough of it to judge.
func (w *compressWriter) Write(b []byte) (int, error) {
	if !w.decided {
		if w.status == 0 {
			// A handler that wrote without setting a status implies 200, and
			// the header it set is final by now.
			w.WriteHeader(http.StatusOK)
		}
		if !w.decided {
			w.held = append(w.held, b...)
			if len(w.held) < w.policy.MinSize {
				return len(b), nil
			}
			// There is enough to be worth compressing after all.
			w.accept()
			return len(b), w.flushHeld()
		}
	}
	if w.compressor != nil {
		return w.compressor.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// flushHeld writes out the body held while the decision was pending.
func (w *compressWriter) flushHeld() error {
	if len(w.held) == 0 {
		return nil
	}
	held := w.held
	w.held = nil
	var err error
	if w.compressor != nil {
		_, err = w.compressor.Write(held)
	} else {
		_, err = w.ResponseWriter.Write(held)
	}
	return err
}

// Flush resolves a pending decision and pushes everything buffered to the
// client, so that a handler streaming a response is not held up by either this
// writer or the compressor.
func (w *compressWriter) Flush() {
	if !w.decided {
		// Whatever has arrived so far is all there is to judge by.
		w.reject()
		_ = w.flushHeld()
	}
	if flusher, ok := w.compressor.(*gzip.Writer); ok {
		_ = flusher.Flush()
	}
	if flusher, ok := w.compressor.(*flate.Writer); ok {
		_ = flusher.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// markHijacked records that a handler took the connection over, which settles
// the pending decision without writing anything: there is no response left to
// compress, and no header left to send.
func (w *compressWriter) markHijacked() {
	w.status = http.StatusSwitchingProtocols
	w.decided = true
	w.headerSent = true
	w.held = nil
}

// Unwrap exposes the underlying writer to [http.ResponseController] so that
// deadline control and hijacking keep working through this wrapper.
func (w *compressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish completes the response: a body too small to be worth compressing is
// written as it stands, and a compressed one has its trailer written.
func (w *compressWriter) finish() {
	if !w.decided {
		w.reject()
	}
	_ = w.flushHeld()
	if w.compressor != nil {
		_ = w.compressor.Close()
		w.pool.put(w.encoding, w.compressor)
		w.compressor = nil
	}
}

// abandon gives up on a response whose handler did not return, committing
// nothing: a header still held back stays unsent, a body still held is
// dropped, and a compressor is released without writing its trailer. Whoever
// recovers the panic then decides what the client sees, which is a 500 if the
// header never left and an aborted connection if it did.
//
// The compressor is not returned to the pool, because a goroutine the handler
// started may still hold this writer and write through it.
func (w *compressWriter) abandon() {
	w.held = nil
	w.compressor = nil
}

// get takes a compressor from the pool, or builds one when the pool is empty,
// and points it at the response.
func (p *compressorPool) get(encoding string, w io.Writer) io.WriteCloser {
	if encoding == "gzip" {
		if pooled, ok := p.gzip.Get().(*gzip.Writer); ok {
			pooled.Reset(w)
			return pooled
		}
		// coverage: NewWriterLevel only rejects a level outside the accepted
		// range, and CompressionLevel cannot express one.
		writer, _ := gzip.NewWriterLevel(w, p.level)
		return writer
	}
	if pooled, ok := p.flate.Get().(*flate.Writer); ok {
		pooled.Reset(w)
		return pooled
	}
	writer, _ := flate.NewWriter(w, p.level)
	return writer
}

// put returns a finished compressor to the pool.
func (p *compressorPool) put(encoding string, compressor io.WriteCloser) {
	if encoding == "gzip" {
		p.gzip.Put(compressor)
		return
	}
	p.flate.Put(compressor)
}

// addVaryAcceptEncoding records that the response depends on Accept-Encoding,
// unless the handler has already said so.
//
// It appends rather than sets, because Vary is a list and a handler that
// declared its own fields is describing something this middleware knows
// nothing about.
func addVaryAcceptEncoding(header http.Header) {
	for _, value := range header.Values("Vary") {
		for field := range strings.SplitSeq(value, ",") {
			field = strings.TrimSpace(field)
			// "*" already says the response varies by everything, and adding to
			// it would only make the header longer.
			if field == "*" || strings.EqualFold(field, "Accept-Encoding") {
				return
			}
		}
	}
	header.Add("Vary", "Accept-Encoding")
}
