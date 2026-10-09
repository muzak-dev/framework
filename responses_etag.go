package muzak

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
)

// AutoETag tags the responses of a route, or of every route beneath a router,
// with an entity tag computed from the body, and answers a request that
// already holds that body with 304 Not Modified instead of sending it again.
// Declared on [New], it applies to the whole application:
//
//	app := muzak.New(opts, muzak.AutoETag())
//
// It applies to a GET or HEAD request answered 200 with a body Muzak holds in
// full before sending it: JSON, [HTML] and [Bytes]. The tag is strong, the
// first 128 bits of the body's SHA-256 in unpadded base64url, quoted, so it
// changes whenever a byte of the body does and only then; JSON is encoded with
// map keys sorted on such a route, so that the same value always encodes to
// the same bytes. A handler that sets an ETag of its own keeps it, and the
// request is answered against that one instead.
//
// A request whose If-None-Match names the tag, compared weakly as RFC 9110
// section 13.1.2 requires so that the weakened tag a compressed response
// carried still matches, or is "*", is answered 304 with no body. The 304
// keeps the headers RFC 9110 section 15.4.5 asks for (ETag, Cache-Control,
// Content-Location, Date, Expires and Vary) and drops those describing the
// body it does not send, Content-Type, Content-Length and Last-Modified among
// them. Headers about the exchange rather than the body, such as Set-Cookie
// and the request identifier, are kept. An If-None-Match that is not a
// well-formed list is ignored, and the full response sent.
//
// It composes with what surrounds it. [Compress] weakens the tag of a body it
// compresses, and of the 304 it answers for one. A guarded route's
// "Cache-Control: private, no-cache" is kept on its 304, so the response is
// still never stored by a shared cache. A [Stream] has no body to hash until
// it is sent, and a [FileResponse] is answered by [http.ServeContent], which
// reads the conditional headers itself, so neither is tagged.
//
// The body is hashed on every such request, which costs time linear in its
// size; the 304 saves the bytes and the client's work, not the handler's. A
// route that does not declare it pays nothing at all.
func AutoETag() SharedOption {
	return sharedOption{
		route:  func(c *routeConfig) { c.output.autoETag = true },
		router: func(c *routerConfig) { c.output.autoETag = true },
	}
}

// taggedJSON encodes the body of a tagged route. Map keys are sorted so that
// one value always produces one body, and so one tag.
var taggedJSON = json.JoinOptions(durationJSON, json.Deterministic(true))

// jsonOptions returns the options a response body is encoded with.
func (c *Context) jsonOptions(status int) json.Options {
	if c.tagsResponse(status) {
		return taggedJSON
	}
	return durationJSON
}

// tagsResponse reports whether a response with this status is one [AutoETag]
// tags. It reads nothing but fields already in hand, so a route that has not
// declared it pays for a comparison and no more.
func (c *Context) tagsResponse(status int) bool {
	return c.route != nil && c.route.output.autoETag && status == http.StatusOK &&
		(c.r.Method == http.MethodGet || c.r.Method == http.MethodHead)
}

// notModifiedBytes tags a body held as bytes and answers the request with 304
// when it already holds it, reporting whether it did.
func (c *Context) notModifiedBytes(status int, body []byte) bool {
	if !c.tagsResponse(status) {
		return false
	}
	return c.revalidate(func() string { return digestTag(sha256.Sum256(body)) })
}

// notModifiedString is [Context.notModifiedBytes] for a body held as text.
func (c *Context) notModifiedString(status int, body string) bool {
	if !c.tagsResponse(status) {
		return false
	}
	return c.revalidate(func() string { return digestTag(sha256.Sum256([]byte(body))) })
}

// digestTag renders a digest as a strong entity tag: its first 128 bits,
// which are as unlikely to collide by chance as any response needs, in
// unpadded base64url between quotes.
func digestTag(sum [sha256.Size]byte) string {
	const encoded = 22 // base64.RawURLEncoding.EncodedLen(16)
	var tag [encoded + 2]byte
	tag[0], tag[len(tag)-1] = '"', '"'
	base64.RawURLEncoding.Encode(tag[1:len(tag)-1], sum[:16])
	return string(tag[:])
}

// revalidate sets the response's entity tag, computing it only when the
// handler set none, and answers 304 when the request's If-None-Match names it.
func (c *Context) revalidate(compute func() string) bool {
	header := c.w.Header()
	tag := header.Get("ETag")
	if tag == "" {
		tag = compute()
		header.Set("ETag", tag)
	}
	if !ifNoneMatch(c.r.Header.Values("If-None-Match"), tag) {
		return false
	}
	for _, name := range notModifiedOmits {
		header.Del(name)
	}
	c.w.WriteHeader(http.StatusNotModified)
	return true
}

// notModifiedOmits are the headers a 304 does not carry. RFC 9110 section
// 15.4.5 asks a 304 for the headers a 200 would have had that a cache uses to
// update what it stored, and says a sender should not generate representation
// metadata beyond them, since the representation is not sent: these describe
// the body, its encoding, its language, its ranges, its integrity or how it is
// framed. Last-Modified is among them because the tag already does its job.
var notModifiedOmits = [...]string{
	"Content-Type",
	"Content-Length",
	"Content-Encoding",
	"Content-Language",
	"Content-Range",
	"Content-Disposition",
	"Content-Digest",
	"Repr-Digest",
	"Digest",
	"Content-MD5",
	"Accept-Ranges",
	"Last-Modified",
	"Trailer",
	"Transfer-Encoding",
}

// ifNoneMatch reports whether an If-None-Match field, given as its lines,
// names tag by the weak comparison of RFC 9110 section 13.1.2, or is "*".
//
// The field is a list of entity tags, and a tag is quoted text that may hold
// a comma, so the list is read rather than split. A field that is not a
// well-formed list is answered as though it were absent, by reporting no
// match: sending the full response is always correct, and a 304 sent on a
// misreading is not. The read is a single pass over every line, so it is
// linear in the field, which net/http bounds by the server's header limit.
func ifNoneMatch(lines []string, tag string) bool {
	if len(lines) == 0 {
		return false
	}
	opaque, ok := opaqueTag(tag)
	if !ok {
		return false
	}
	matched := false
	for _, line := range lines {
		lineMatched, valid := matchTagList(line, opaque)
		if !valid {
			return false
		}
		matched = matched || lineMatched
	}
	return matched
}

// matchTagList reads one line of an If-None-Match field, reporting whether it
// names the opaque tag and whether it is well formed at all. "*" is accepted
// only as the whole of a line, since it stands for any tag and a list holding
// it means nothing more.
func matchTagList(line, opaque string) (matched, valid bool) {
	start, end := skipSpace(line, 0), len(line)
	for end > start && (line[end-1] == ' ' || line[end-1] == '\t') {
		end--
	}
	if line[start:end] == "*" {
		return true, true
	}
	i := start
	for i < end {
		// RFC 9110 section 5.6.1 has a recipient accept empty list elements,
		// so commas and the spaces around them are skipped.
		if c := line[i]; c == ',' || c == ' ' || c == '\t' {
			i++
			continue
		}
		if line[i] == 'W' && i+1 < end && line[i+1] == '/' {
			i += 2
		}
		if i == end || line[i] != '"' {
			return false, false
		}
		open := i + 1
		i = open
		for i < end && line[i] != '"' {
			if !isETagChar(line[i]) {
				return false, false
			}
			i++
		}
		if i == end {
			return false, false
		}
		matched = matched || line[open:i] == opaque
		i = skipSpace(line, i+1)
		if i < end && line[i] != ',' {
			return false, false
		}
	}
	return matched, true
}

// opaqueTag returns the quoted part of an entity tag, without the weakness
// indicator, and whether the tag is well formed. A tag a handler set that is
// not is never matched, so its response is always sent in full.
func opaqueTag(tag string) (string, bool) {
	if len(tag) >= 2 && tag[0] == 'W' && tag[1] == '/' {
		tag = tag[2:]
	}
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return "", false
	}
	opaque := tag[1 : len(tag)-1]
	for i := 0; i < len(opaque); i++ {
		if !isETagChar(opaque[i]) {
			return "", false
		}
	}
	return opaque, true
}

// isETagChar reports whether c is an etagc of RFC 9110 section 8.8.3: any
// visible character but a double quote, or a byte outside ASCII.
func isETagChar(c byte) bool {
	return c == 0x21 || (c >= 0x23 && c <= 0x7e) || c >= 0x80
}
