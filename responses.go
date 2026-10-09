package muzak

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Bytes is a response body written exactly as given, under a media type the
// handler names, instead of being encoded as JSON.
//
//	r.Get("/reports/{id}.csv", func(ctx *muzak.Context, in ReportIn) (muzak.Bytes, error) {
//		csv, err := reports.Render(ctx.Context(), in.ID)
//		if err != nil {
//			return muzak.Bytes{}, err
//		}
//		return muzak.Bytes{ContentType: "text/csv; charset=utf-8", Data: csv,
//			Filename: "report-" + in.ID + ".csv", Download: true}, nil
//	}, muzak.Produces("text/csv"))
//
// The body is already in memory, so it is sent with a Content-Length, a route
// declared with [AutoETag] tags it as it does a JSON body, and the status
// follows [Status] and [Context.SetStatus] as it does for JSON; 204 and 304
// send no body. Every response carries "X-Content-Type-Options: nosniff", so
// a browser believes the type it is given rather than guessing from the
// bytes.
//
// Use [Stream] for a body too large to hold, [FileResponse] for a file, and
// [Produces] to document the media types in the generated document, which
// otherwise describes the body as application/octet-stream.
type Bytes struct {
	// ContentType is the media type the body is sent as, such as
	// "text/csv; charset=utf-8" or "image/png". Empty means
	// application/octet-stream, which a browser saves rather than renders.
	// A value that is not a media type as RFC 9110 spells it, one holding a
	// line break or any other control character included, fails the request
	// with a 500 rather than reaching the header.
	ContentType string

	// Data is the body.
	Data []byte

	// Filename is the name a client is offered to save the body under, sent
	// in a Content-Disposition header built by Muzak; see [FileResponse.Filename]
	// for how it is cleaned. Empty sends none unless Download is set.
	Filename string

	// Download asks the browser to save the body rather than display it, with
	// "Content-Disposition: attachment".
	Download bool
}

// Stream is a response body copied to the client as it is read, for a body
// too large to hold in memory or one that is produced as it is sent, such as
// an export read from a database cursor or a download proxied from another
// service.
//
//	r.Get("/exports/{id}", func(ctx *muzak.Context, in ExportIn) (muzak.Stream, error) {
//		object, err := bucket.Open(ctx.Context(), in.ID)
//		if err != nil {
//			return muzak.Stream{}, muzak.NotFound("").Wrap(err)
//		}
//		return muzak.Stream{ContentType: "application/zip", Body: object, Length: object.Size()}, nil
//	})
//
// Muzak owns Body from the moment the handler returns it. When Body is an
// [io.Closer] it is closed exactly once on every path: after it has been
// copied, when the copy fails, when the client goes away, for a HEAD request
// (which never reads it), when the handler returns an error beside it, and
// when reading it panics. A handler must not close it itself.
//
// The status follows [Status] and [Context.SetStatus]; 204 and 304 send no
// body. Once the header is sent the response cannot be turned into an error,
// so a Body that fails part way, or that ends before the Length it declared,
// is logged and the connection aborted: the client sees a failed transfer
// rather than a body that ends cleanly and looks complete.
//
// Muzak cannot interrupt a Read that blocks. A Body whose Read can wait, an
// [io.Pipe] fed by a goroutine for instance, should end when the request's
// [Context.Context] is done; a body from an outbound request made with that
// context already does. A goroutine feeding the body keeps that context, and
// never the *Context, which is reused once the handler returns.
type Stream struct {
	// ContentType is the media type the body is sent as, checked as
	// [Bytes.ContentType] is. Empty means application/octet-stream.
	ContentType string

	// Body is read to its end and copied to the client.
	Body io.Reader

	// Length is the size of the body in bytes, sent as Content-Length so a
	// client can show progress and tell a truncated body from a complete one.
	// Zero or less means the size is not known, and the body is sent chunked
	// over HTTP/1.1.
	Length int64

	// Filename and Download offer the body as a file to save, as
	// [Bytes.Filename] and [Bytes.Download] do.
	Filename string
	Download bool
}

// Produces documents the media types a route answering with [Bytes], [Stream]
// or [FileResponse] sends, which the generated document otherwise describes as
// application/octet-stream:
//
//	r.Get("/avatars/{id}", handlers.Avatar, muzak.Produces("image/png", "image/webp"))
//
// Each type is described as binary content. It documents and decides nothing
// at run time: the media type a response is sent with is the one the value
// names. Each entry must be a media type as RFC 9110 spells it, and a range
// such as "image/*" is accepted. Using it on a route whose output is anything
// else, JSON included, is a build error, because the document already knows
// what that route sends.
func Produces(contentTypes ...string) RouteOption {
	return routeOptionFunc(func(c *routeConfig) {
		c.output.produces = append(c.output.produces, contentTypes...)
		c.output.producesSet = true
	})
}

// The output types the router recognises, which every handler's Out is
// matched against once, when the route is registered.
var (
	bytesOutType    = reflect.TypeFor[Bytes]()
	streamOutType   = reflect.TypeFor[Stream]()
	redirectOutType = reflect.TypeFor[Redirect]()
	fileOutType     = reflect.TypeFor[FileResponse]()
)

// outputKind is what a route's Out type tells the router about how its
// response is written.
type outputKind uint8

const (
	// outputEncoded is every other type: JSON, [HTML] or [Empty].
	outputEncoded outputKind = iota
	outputBytes
	outputStream
	outputRedirect
	outputFile
)

// outputKindOf classifies an Out type.
func outputKindOf(t reflect.Type) outputKind {
	switch t {
	case bytesOutType:
		return outputBytes
	case streamOutType:
		return outputStream
	case redirectOutType:
		return outputRedirect
	case fileOutType:
		return outputFile
	}
	return outputEncoded
}

// binary reports whether a route of this kind sends a body whose media type
// the value names, which is what [Produces] describes.
func (k outputKind) binary() bool {
	return k == outputBytes || k == outputStream || k == outputFile
}

// outputOptions is what a router or a route declares about its responses.
type outputOptions struct {
	autoETag      bool
	redirectHosts []string
	produces      []string
	producesSet   bool
}

// merge layers what a router declares over what it inherited. Nothing here
// can be switched back off below where it was switched on, which is the
// direction that is safe for each: a tag only answers a request the body has
// not changed for, and a host list only grows by being named.
func (o outputOptions) merge(declared outputOptions) outputOptions {
	return outputOptions{
		autoETag:      o.autoETag || declared.autoETag,
		redirectHosts: concat(o.redirectHosts, declared.redirectHosts),
	}
}

// routeOutput is a route's resolved response configuration, read on every
// request and so computed once when the application is built.
type routeOutput struct {
	kind     outputKind
	autoETag bool
	// mayStream is set when the handler can return a [Stream], whose Body
	// must be closed even when an error is returned beside it. Out may be the
	// type itself or an interface it satisfies.
	mayStream     bool
	produces      []string
	redirectHosts []redirectHost
}

// resolveOutput completes a route's response configuration from what it
// inherited and what it declared, and reports every mistake in it at once.
// It runs after the route's status is resolved, because a redirecting route
// replaces the default of 200 with a redirect status.
func (rt *Route) resolveOutput(in outputOptions) error {
	declared := rt.cfg.output
	kind := outputKindOf(rt.outType)
	out := routeOutput{
		kind:     kind,
		autoETag: in.autoETag || declared.autoETag,
		mayStream: rt.outType == streamOutType ||
			(rt.outType.Kind() == reflect.Interface && streamOutType.Implements(rt.outType)),
	}
	where := rt.Method + " " + rt.Path
	var errs []error
	if rt.outType.Kind() == reflect.Pointer {
		if elem := rt.outType.Elem(); outputKindOf(elem) != outputEncoded {
			errs = append(errs, fmt.Errorf("muzak: %s: the handler returns *muzak.%s, which would be encoded as JSON; return muzak.%s by value",
				where, elem.Name(), elem.Name()))
		}
	}
	// The hosts are read only for a route that can answer with a Redirect,
	// which is the only one that consults them. A list declared on the
	// application would otherwise be parsed, and a mistake in it reported,
	// once for every route beneath it.
	mayRedirect := rt.outType == redirectOutType ||
		(rt.outType.Kind() == reflect.Interface && redirectOutType.Implements(rt.outType))
	for _, entry := range concat(in.redirectHosts, declared.redirectHosts) {
		if !mayRedirect {
			break
		}
		host, err := parseRedirectHost(entry)
		if err != nil {
			errs = append(errs, fmt.Errorf("muzak: %s: %w", where, err))
			continue
		}
		out.redirectHosts = append(out.redirectHosts, host)
	}
	if declared.producesSet {
		switch {
		case !kind.binary() || rt.websocket != nil || rt.sse != nil:
			errs = append(errs, fmt.Errorf("muzak: %s: Produces describes a Bytes, Stream or FileResponse body, and this route returns %s; leave it out",
				where, rt.outType))
		case len(declared.produces) == 0:
			errs = append(errs, fmt.Errorf("muzak: %s: Produces was given no media types; name at least one or leave it out", where))
		}
		for _, contentType := range declared.produces {
			if !validMediaType(contentType) {
				errs = append(errs, fmt.Errorf("muzak: %s: Produces was given %q, which is not a media type such as \"text/csv\"",
					where, truncateForMessage(contentType)))
			}
		}
		out.produces = dedupeStrings(declared.produces)
	}
	if kind == outputRedirect && rt.websocket == nil && rt.sse == nil {
		switch {
		case rt.cfg.status == 0:
			rt.Status = defaultRedirectStatus(rt.Method)
		case !isRedirectStatus(rt.cfg.status):
			errs = append(errs, fmt.Errorf("muzak: %s: the route returns muzak.Redirect but declares status %d; declare 301, 302, 303, 307 or 308, or leave Status out",
				where, rt.cfg.status))
		}
	}
	rt.output = out
	return errors.Join(errs...)
}

// closeStreamOutput closes the body of a [Stream] a handler returned beside an
// error, which is the one path where nothing else would ever read or close it.
func closeStreamOutput(v any) {
	if stream, ok := v.(Stream); ok {
		closeBody(stream.Body)
	}
}

// closeBody closes a body that is an [io.Closer]. Its error is not reported:
// the response has been decided by the time a body is closed, and a reader's
// close failing says nothing the client or the log could act on.
func closeBody(body io.Reader) {
	if closer, ok := body.(io.Closer); ok {
		_ = closer.Close()
	}
}

// writeTyped writes the output types that are not encoded, reporting false for
// every other value. It runs before anything else looks at a response, because
// a [Stream] has a body to close whether or not anything is written.
func (c *Context) writeTyped(v any) (bool, error) {
	switch out := v.(type) {
	case Bytes:
		return true, c.writeBytes(out)
	case Stream:
		return true, c.writeStream(out)
	case Redirect:
		return true, c.writeRedirect(out)
	case FileResponse:
		return true, c.writeFile(out)
	}
	return false, nil
}

// bodiless reports whether a status is one whose response carries no body at
// all, which is how the encoded responses treat 204 and 304.
func bodiless(status int) bool {
	return status == http.StatusNoContent || status == http.StatusNotModified
}

// writeBytes writes a [Bytes] value.
func (c *Context) writeBytes(out Bytes) error {
	if c.w.written {
		return nil
	}
	contentType, err := c.responseContentType(out.ContentType)
	if err != nil {
		return err
	}
	status := clampStatus(c.status)
	if bodiless(status) {
		c.w.WriteHeader(status)
		return nil
	}
	c.describeBody(contentType, out.Download, out.Filename)
	if c.notModifiedBytes(status, out.Data) {
		return nil
	}
	c.w.Header().Set("Content-Length", strconv.Itoa(len(out.Data)))
	c.w.WriteHeader(status)
	_, err = c.w.Write(out.Data)
	return err
}

// writeStream writes a [Stream] value, closing its body on every way out,
// a panic while reading it included.
func (c *Context) writeStream(out Stream) error {
	defer closeBody(out.Body)
	if c.w.written {
		return nil
	}
	if out.Body == nil {
		return fmt.Errorf("muzak: %s %s returned a muzak.Stream with no Body; return muzak.Bytes for a body that is empty",
			c.r.Method, c.route.pathOrRequest(c.r))
	}
	contentType, err := c.responseContentType(out.ContentType)
	if err != nil {
		return err
	}
	status := clampStatus(c.status)
	if bodiless(status) {
		c.w.WriteHeader(status)
		return nil
	}
	c.describeBody(contentType, out.Download, out.Filename)
	return c.sendBody(status, out.Body, out.Length)
}

// responseContentType returns the media type a response is sent with, or the
// error that fails the request when the value names one that is not.
func (c *Context) responseContentType(contentType string) (string, error) {
	if contentType == "" {
		return "application/octet-stream", nil
	}
	if !validMediaType(contentType) {
		return "", fmt.Errorf("muzak: %s %s returned the content type %q, which is not a media type such as \"text/csv; charset=utf-8\"",
			c.r.Method, c.route.pathOrRequest(c.r), truncateForMessage(contentType))
	}
	return contentType, nil
}

// describeBody sets the headers that describe a body whose type the handler
// named rather than Muzak.
func (c *Context) describeBody(contentType string, download bool, filename string) {
	header := c.w.Header()
	header.Set("Content-Type", contentType)
	// Set here as well as by the security headers, which an application may
	// turn off: a body whose bytes a client may have written is exactly what a
	// browser's sniffing would turn into a page.
	header.Set("X-Content-Type-Options", "nosniff")
	if disposition := contentDisposition(download, filename); disposition != "" {
		header.Set("Content-Disposition", disposition)
	}
}

// streamBufferSize is the size of the buffers bodies are copied through,
// which is the size [io.Copy] would allocate for each copy.
const streamBufferSize = 32 << 10

// streamBuffers recycles the copy buffers, so a server streaming many bodies
// does not allocate one per response. Nothing is left in a buffer that a
// later response could send, because a copy only sends what it read into the
// buffer itself.
var streamBuffers = sync.Pool{
	New: func() any {
		buf := make([]byte, streamBufferSize)
		return &buf
	},
}

// readerOnly hides every method of a reader but Read, so that a copy goes
// through the pooled buffer. A reader with a WriteTo of its own, *os.File for
// one, would otherwise copy through a fresh buffer of its own for a writer
// such as this one that is not a socket.
type readerOnly struct{ io.Reader }

// sendBody writes the header with status and then the body, which is the
// part a [Stream] and a file that cannot be served by [http.ServeContent]
// share. length is sent as Content-Length when it is positive, and the body
// is then held to it.
//
// The copy is linear in the body and holds one pooled buffer. A HEAD request
// is answered from the header alone, without reading the body at all.
func (c *Context) sendBody(status int, body io.Reader, length int64) error {
	if length > 0 {
		c.w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	}
	c.w.WriteHeader(status)
	if c.r.Method == http.MethodHead {
		return nil
	}
	buf := streamBuffers.Get().(*[]byte)
	defer streamBuffers.Put(buf)
	n, err := io.CopyBuffer(c.w, readerOnly{body}, *buf)
	if err != nil {
		return fmt.Errorf("muzak: the body of %s %s failed after %d bytes: %w",
			c.r.Method, c.route.pathOrRequest(c.r), n, err)
	}
	if length > 0 && n < length {
		return fmt.Errorf("muzak: the body of %s %s ended after %d of the %d bytes its Length declared",
			c.r.Method, c.route.pathOrRequest(c.r), n, length)
	}
	return nil
}

// maxMediaTypeLength bounds a media type a response or a [Produces]
// declaration names. A real one, parameters and a multipart boundary
// included, is a small fraction of it.
const maxMediaTypeLength = 1024

// validMediaType reports whether s is a media type as RFC 9110 section 8.3.1
// spells it: a type and a subtype, each a token, then any number of
// parameters, each a token, "=", and a token or a quoted string, separated by
// ";" and optional spaces or tabs.
//
// It is stricter than [mime.ParseMediaType], on purpose: it admits nothing
// but ASCII, no control character other than a tab in the places the grammar
// allows one, no empty parameter, and nothing past [maxMediaTypeLength], so a
// value it accepts is safe to put in a header as it is. It reads s once, left
// to right, and allocates nothing.
func validMediaType(s string) bool {
	if s == "" || len(s) > maxMediaTypeLength {
		return false
	}
	i := scanToken(s, 0)
	if i == 0 || i == len(s) || s[i] != '/' {
		return false
	}
	end := scanToken(s, i+1)
	if end == i+1 {
		return false
	}
	i = end
	for {
		i = skipSpace(s, i)
		if i == len(s) {
			return true
		}
		if s[i] != ';' {
			return false
		}
		i = skipSpace(s, i+1)
		end = scanToken(s, i)
		if end == i || end == len(s) || s[end] != '=' {
			return false
		}
		i = end + 1
		if i < len(s) && s[i] == '"' {
			end = scanQuoted(s, i)
		} else {
			end = scanToken(s, i)
		}
		if end <= i {
			return false
		}
		i = end
	}
}

// scanToken returns the index just past the run of token characters starting
// at i, which is i itself when there is none.
func scanToken(s string, i int) int {
	for i < len(s) && isTokenChar(s[i]) {
		i++
	}
	return i
}

// isTokenChar reports whether c is a tchar of RFC 9110 section 5.6.2.
func isTokenChar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// skipSpace returns the index of the first character at or after i that is
// neither a space nor a tab.
func skipSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// scanQuoted returns the index just past the quoted string starting at i,
// which holds a double quote, or i when it is not one: unterminated, or
// holding a control character or a byte outside ASCII.
func scanQuoted(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '"':
			return j + 1
		case c == '\\':
			j++
			if j == len(s) || !isQuotedText(s[j]) {
				return i
			}
		case !isQuotedText(c):
			return i
		}
	}
	return i
}

// isQuotedText reports whether c may appear inside a quoted string, either as
// itself or after a backslash: a tab, a space or a visible ASCII character.
func isQuotedText(c byte) bool {
	return c == '\t' || (c >= ' ' && c <= '~')
}
