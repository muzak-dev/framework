package muzak

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"io"
	"mime"
	"net"
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

// gzipLevel maps a level onto what compress/gzip and compress/zlib accept.
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
// A compressed response is a different representation from the one the
// handler described, and the validators are adjusted to say so. A strong ETag
// is weakened, to W/"v1" for "v1", on the compressed response and on a 304
// answering a client that negotiated an encoding, so that a cache can use the
// 304 to refresh what it stored. Because RFC 9110 compares If-Match strongly,
// which a weak tag never matches, the W/ is taken off the tags of an incoming
// If-Match before the handler sees it: a client that read a compressed
// response can still make a conditional write with the tag it was given. A
// handler that compares If-Match as RFC 9110 asks is unaffected otherwise; one
// that issues weak tags of its own and compares If-Match by string equality,
// which RFC 9110 does not allow, sees the strong form of them, and should
// compare the opaque tag instead.
//
// A compressed response does not advertise Accept-Ranges, since no range of the
// bytes it carries can be asked for. A Range request is answered from the
// uncompressed content, uncompressed, and one that carries If-Range with a date
// rather than a strong tag is answered with the whole content when an encoding
// was negotiated, because the date cannot say whether the client holds the
// compressed bytes or the uncompressed ones, and a client appending a range of
// one to the other ends up with neither.
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
			encoding := negotiateEncoding(r.Header.Get("Accept-Encoding"))
			r = reconcileConditions(r, encoding)
			cw := &compressWriter{
				ResponseWriter: w,
				policy:         policy,
				pool:           pool,
				encoding:       encoding,
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

// reconcileConditions adjusts the conditional headers of a request for what
// this middleware does to the responses it compresses, and returns the request
// as it arrived when there is nothing to adjust, which is nearly always, or a
// copy when there is, so that what the caller holds is left as it was.
//
// Two headers carry what a compressed response told the client back to the
// handler, which knows nothing of the compression.
//
// If-Match is compared strongly (RFC 9110 section 13.1.1), so the weak tag a
// compressed response carried could never match, and a client that accepts
// gzip had every conditional write refused. Such a tag is the strong tag of the
// same content with W/ in front, so the W/ is taken off before the handler
// compares. That changes no answer a handler comparing as RFC 9110 asks would
// give, except to make one that had to fail match the tag it names: a weak tag
// in If-Match matches nothing at all, and its strong form still matches only a
// resource whose current tag is that same strong tag.
//
// If-Range with a date, sent alongside Range by a client resuming a download,
// asks for the rest of the representation only if it is unchanged. A client
// that holds a compressed response has a weak tag at most, which If-Range may
// not carry, so it sends the date, and the date matched: the handler answered
// with a range of the uncompressed content, which a client that does not look
// at Content-Encoding splices onto the compressed bytes it has. Once an
// encoding is negotiated a date cannot say which of the two representations
// the client holds, so Range and If-Range are dropped and the handler sends the
// whole of it, which is the answer to an If-Range that does not match. Only a
// strong tag, which no compressed response carries, keeps a resume going; a
// weak one never matches If-Range and is dropped with the date, which changes
// nothing a handler comparing as RFC 9110 asks would do. A Range sent without
// If-Range is left alone, as it asks for a range of the uncompressed content
// whatever the client holds, and is answered with one.
func reconcileConditions(r *http.Request, encoding string) *http.Request {
	weakMatch := false
	for _, value := range r.Header.Values("If-Match") {
		if strings.Contains(value, `W/"`) {
			weakMatch = true
			break
		}
	}
	dateRange := false
	if encoding != "" && r.Header.Get("Range") != "" {
		ifRange := r.Header.Get("If-Range")
		dateRange = ifRange != "" && !strings.HasPrefix(strings.TrimSpace(ifRange), `"`)
	}
	if !weakMatch && !dateRange {
		return r
	}

	r = r.Clone(r.Context())
	if weakMatch {
		values := r.Header.Values("If-Match")
		restored := make([]string, len(values))
		for i, value := range values {
			restored[i] = strongEntityTags(value)
		}
		r.Header["If-Match"] = restored
	}
	if dateRange {
		r.Header.Del("Range")
		r.Header.Del("If-Range")
	}
	return r
}

// negotiateEncoding picks the encoding to use for a request, or the empty
// string when the client asked for none this middleware can produce.
//
// gzip is preferred over deflate because every client that accepts deflate
// accepts gzip, while the reverse is not true and deflate has a history of
// being sent in two incompatible framings. The one sent here is the one RFC
// 9110 names, the zlib format around DEFLATE, and not the bare DEFLATE stream
// that some old servers sent and a lenient client had to guess at.
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
	zlib  sync.Pool
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
	if isInformational(status) {
		// An interim response has no body to compress and settles nothing,
		// so it goes straight through and the decision waits for the real
		// status.
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status

	if status == http.StatusNotModified && w.encoding != "" {
		// A 304 refreshes what a cache stored, and RFC 9111 section 4.3.4 lets
		// a strong tag refresh only a stored response with that same strong
		// tag. The 200 this one revalidates was compressed and carried the tag
		// weakened, so a strong tag here could refresh nothing. Weakened, it
		// still corresponds to a stored response that was not compressed and
		// kept the strong one, because entity tags correspond by weak
		// comparison, which is why this does not need to know which of the two
		// the client holds.
		weakenETag(w.Header())
	}

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
	weakenETag(header)
	// A range of the bytes sent here cannot be asked for: a Range request is
	// answered from the uncompressed content, as [compressWriter.worthCompressing]
	// explains. Advertising one would invite a client to resume a download by
	// appending a slice of one representation to the bytes of the other.
	header.Del("Accept-Ranges")

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
		// These are the handler's own bytes, held while the decision was
		// pending and passed on unchanged, as Write passes on b. gosec's G705
		// reaches them only through the receiver: its call graph counts every
		// io.Writer.Write in the program as a caller and reads the bytes that
		// call writes as this receiver, so whether it reports the line depends
		// on which of those callers it happens to visit first.
		_, err = w.ResponseWriter.Write(held) //nolint:gosec // G705: relays the handler's own body unchanged, adding nothing from the request
	}
	return err
}

// FlushError resolves a pending decision and pushes everything buffered to the
// client, so that a handler streaming a response is not held up by either this
// writer or the compressor.
//
// It reports the first thing that went wrong on the way, because a caller that
// streams through [http.ResponseController] learns from it that the client is
// gone. A flush that answered nil whatever became of the bytes would leave a
// stream writing to a dead connection until something else noticed.
func (w *compressWriter) FlushError() error {
	var first error
	if !w.decided {
		// Whatever has arrived so far is all there is to judge by.
		w.reject()
		first = w.flushHeld()
	}
	switch flusher := w.compressor.(type) {
	case *gzip.Writer:
		first = firstError(first, flusher.Flush())
	case *zlib.Writer:
		first = firstError(first, flusher.Flush())
	}
	return firstError(first, http.NewResponseController(w.ResponseWriter).Flush())
}

// Flush is the older spelling of [compressWriter.FlushError], kept because a
// wrapper written before the newer one existed looks for it by name.
func (w *compressWriter) Flush() { _ = w.FlushError() }

// firstError returns first when it is set and next otherwise.
func firstError(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

// markHijacked records that a handler took the connection over, which settles
// the pending decision without writing anything: there is no response left to
// compress, no header left to send, and no trailer left to close a compressed
// body with. A compressor already started is dropped rather than pooled, for
// the reason [compressWriter.abandon] gives.
func (w *compressWriter) markHijacked() {
	w.status = http.StatusSwitchingProtocols
	w.decided = true
	w.headerSent = true
	w.held = nil
	w.compressor = nil
}

// Hijack hands the connection to a handler that takes it over, through
// [http.ResponseController] or by asserting [http.Hijacker], and records that
// it did.
//
// Unwrap alone would let the controller reach the connection past this
// wrapper, which would then go on believing it owed a response: when the
// handler returned it would send the header it was holding, or close the
// compressor onto a socket that had stopped speaking HTTP, and net/http logs
// both. The connection is taken first and recorded only once it has been, so
// a writer that cannot give it up, such as an HTTP/2 one, reports
// [http.ErrNotSupported] and the response carries on as before.
func (w *compressWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	// The whole chain from here down is told, not only this writer, so that
	// none of them finishes a response either.
	markHijacked(w)
	return conn, rw, nil
}

// Unwrap exposes the underlying writer to [http.ResponseController] so that
// deadline control keeps working through this wrapper.
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
	if pooled, ok := p.zlib.Get().(*zlib.Writer); ok {
		pooled.Reset(w)
		return pooled
	}
	// coverage: NewWriterLevel only rejects a level outside the accepted
	// range, and CompressionLevel cannot express one.
	writer, _ := zlib.NewWriterLevel(w, p.level)
	return writer
}

// put returns a finished compressor to the pool.
func (p *compressorPool) put(encoding string, compressor io.WriteCloser) {
	if encoding == "gzip" {
		p.gzip.Put(compressor)
		return
	}
	p.zlib.Put(compressor)
}

// weakenETag marks a strong entity tag as weak, leaving a weak one, and a
// response without one, as they are.
func weakenETag(header http.Header) {
	if tag := header.Get("ETag"); tag != "" && !strings.HasPrefix(tag, "W/") {
		header.Set("ETag", "W/"+tag)
	}
}

// strongEntityTags returns an entity tag list with the weakness indicator
// taken off every tag in it, and everything else as it stands.
//
// It reads the list rather than splitting it on commas, because an opaque tag
// may itself contain a comma or the characters W/, and only a W/ outside a tag
// and directly before its opening quote marks one as weak.
func strongEntityTags(list string) string {
	var out strings.Builder
	out.Grow(len(list))
	inTag := false
	for i := 0; i < len(list); i++ {
		if !inTag && strings.HasPrefix(list[i:], `W/"`) {
			// The indicator is dropped and the quote after it written as the
			// opening of the tag on the next pass.
			i++
			continue
		}
		if list[i] == '"' {
			inTag = !inTag
		}
		out.WriteByte(list[i])
	}
	return out.String()
}

// addVaryAcceptEncoding records that the response depends on Accept-Encoding,
// unless the handler has already said so.
func addVaryAcceptEncoding(header http.Header) { addVary(header, "Accept-Encoding") }

// addVary records that the response depends on the named request header,
// unless the Vary list already says so.
//
// It appends rather than sets, because Vary is a list and a handler or
// middleware that declared its own fields is describing something the caller
// knows nothing about.
func addVary(header http.Header, field string) {
	for _, value := range header.Values("Vary") {
		for listed := range strings.SplitSeq(value, ",") {
			listed = strings.TrimSpace(listed)
			// "*" already says the response varies by everything, and adding to
			// it would only make the header longer.
			if listed == "*" || strings.EqualFold(listed, field) {
				return
			}
		}
	}
	header.Add("Vary", field)
}
