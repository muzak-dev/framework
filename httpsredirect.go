package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// RedirectHTTPSOptions configures [AppOptions.RedirectHTTPS], which sends a
// request that arrived over plain HTTP to the same host, path and query over
// https.
//
// A redirect is only as trustworthy as the host it names, and the Host header
// is whatever the client wrote. The redirect therefore never names a host the
// application has not vouched for: it keeps the request's own host only when
// [AppOptions.AllowedHosts] has already admitted it, and otherwise needs Host
// set here. Enabling it with neither is a build error, because a forged Host
// header would then turn the redirect into one to anywhere.
type RedirectHTTPSOptions struct {
	// Host is the host every redirect is sent to, written as an
	// [AppOptions.AllowedHosts] entry is, such as "example.com" or
	// "example.com:8443" but with no wildcard, in place of the host the
	// request named. Set it to move every client onto one canonical name, or
	// to redirect without an allowlist.
	Host string

	// Port is the port https is served on, for a redirect that keeps the
	// request's own host. The port the request named is never kept, since it
	// is the one plain HTTP was served on. Zero means 443, which a URL leaves
	// out. A Host that names a port of its own cannot be combined with it.
	Port int
}

// acmeChallengePrefix is where an ACME certificate authority looks for the
// token that proves control of a host, over plain HTTP by design: a host that
// has no certificate yet cannot be asked for one over https.
const acmeChallengePrefix = "/.well-known/acme-challenge/"

// maxACMEToken bounds an ACME token, which is a base64url string of a few dozen
// characters, so the exemption cannot be stretched over a path of any length.
const maxACMEToken = 256

// httpsRedirect is [RedirectHTTPSOptions] checked and prepared once.
type httpsRedirect struct {
	// host is the canonical host and port every redirect names, and is empty
	// when the redirect keeps the request's own host, followed by port.
	host string
	port string
}

// newHTTPSRedirect checks the options, reporting every problem at once.
// allowlisted reports whether [AppOptions.AllowedHosts] is set, which is one
// of the two ways a redirect can come to name a host the application vouches
// for.
func newHTTPSRedirect(opts RedirectHTTPSOptions, allowlisted bool) (*httpsRedirect, error) {
	var errs []error
	redirect := &httpsRedirect{}
	if opts.Port < 0 || opts.Port > 65535 {
		errs = append(errs, fmt.Errorf("muzak: RedirectHTTPS.Port is %d, which is not a port; leave it at zero for 443", opts.Port))
	} else if opts.Port != 0 && opts.Port != 443 {
		redirect.port = ":" + strconv.Itoa(opts.Port)
	}
	switch {
	case opts.Host != "":
		name, port, wildcard, err := parseHostEntry("RedirectHTTPS.Host", opts.Host)
		switch {
		case err != nil:
			errs = append(errs, err)
		case wildcard:
			errs = append(errs, fmt.Errorf("muzak: RedirectHTTPS.Host %q is a wildcard, but a redirect needs one host "+
				"to send the client to", opts.Host))
		case port >= 0 && opts.Port != 0:
			errs = append(errs, fmt.Errorf("muzak: RedirectHTTPS.Host %q names a port and RedirectHTTPS.Port names "+
				"another; set the port in one place", opts.Host))
		default:
			redirect.host = name + redirect.port
			if port >= 0 && port != 443 {
				redirect.host = name + ":" + strconv.Itoa(port)
			}
		}
	case !allowlisted:
		errs = append(errs, errors.New("muzak: RedirectHTTPS is enabled with neither AppOptions.AllowedHosts nor "+
			"RedirectHTTPS.Host set, so a redirect would name whatever host the client wrote, and a forged Host "+
			"header would make it a redirect to anywhere; list the hosts the application serves, or name the one "+
			"to redirect to"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return redirect, nil
}

// redirected redirects a plain-HTTP request, and reports whether it did.
//
// GET and HEAD are answered 301 and every other method 308. A 301 is
// understood by every client and is kept by a browser, so a visitor's next
// navigation goes straight to https without a plaintext round trip; 308 is
// the permanent redirect that keeps the method and the body, which a client
// following a 301 for a POST is allowed to turn into a GET without one. Either
// way the body of a request that was redirected has already crossed the
// network in the clear, which is why the redirect is a convenience for
// navigation and [SecurityHeaders]'s Strict-Transport-Security is the defence.
func (h *httpsRedirect) redirected(rw *responseWriter, r *http.Request, ips *clientIPResolver) bool {
	plain, consulted := plainHTTP(r, ips)
	if consulted != "" {
		// What was answered depends on a header a proxy sets, and a cache
		// between that proxy and this server sees it.
		rw.varyOn(consulted)
	}
	if !plain || isACMEChallenge(r.URL.Path) {
		return false
	}
	status := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status = http.StatusMovedPermanently
	}
	rw.Header().Set("Location", h.location(r))
	rw.WriteHeader(status)
	return true
}

// location builds the https URL a request is redirected to.
//
// The host is the canonical one, or the request's own normalised by the same
// reading the allowlist admitted it with, so nothing the client wrote beyond
// a name the application lists reaches it. A redirect that keeps the request's
// host is only ever built beside an allowlist, which has already refused a
// host that does not read as one, so the reading here always succeeds.
//
// The path and query are copied with every byte that is not plainly allowed
// in them percent-encoded: a control character, a space, a backslash, a quote
// or a byte outside ASCII cannot reach the header, an escape the client wrote
// is kept as written, and a stray '%' becomes "%25". A path that does not
// begin with a slash, as the "*" of an OPTIONS request does, is redirected to
// the root, so that nothing the request carries can run into the host. Since
// the URL always names its host, a path beginning "//" or "/\" is a path on
// that host and nothing else.
func (h *httpsRedirect) location(r *http.Request) string {
	path := r.URL.EscapedPath()
	if path == "" || path[0] != '/' {
		path = "/"
	}
	var b strings.Builder
	b.Grow(len("https://") + maxHostHeader + len(path) + len(r.URL.RawQuery) + 1)
	b.WriteString("https://")
	if h.host != "" {
		b.WriteString(h.host)
	} else {
		var buf hostBuffer
		name, _, _ := splitHostHeader(&buf, r.Host)
		b.Write(name)
		b.WriteString(h.port)
	}
	writeURLPart(&b, path, false)
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		b.WriteByte('?')
		writeURLPart(&b, r.URL.RawQuery, true)
	}
	return b.String()
}

// writeURLPart copies a path or a query into b, percent-encoding every byte
// RFC 3986 does not allow there as it is. A valid escape is copied as it was
// written, and a '%' that does not begin one is encoded.
func writeURLPart(b *strings.Builder, part string, query bool) {
	const hexDigits = "0123456789ABCDEF"
	for i := 0; i < len(part); i++ {
		c := part[i]
		switch {
		case c == '%' && i+2 < len(part) && isPercentHex(part[i+1]) && isPercentHex(part[i+2]):
			b.WriteString(part[i : i+3])
			i += 2
		case isMountPathByte(c), c == '/', query && c == '?':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
	}
}

// isPercentHex reports whether c is a hexadecimal digit.
func isPercentHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// isACMEChallenge reports whether a path is an ACME HTTP-01 challenge: the
// challenge directory followed by a single token of base64url characters.
// Anything else under the directory, a second segment or a "..", is not one,
// so the exemption cannot be stretched over a path that only begins like one.
func isACMEChallenge(path string) bool {
	token, ok := strings.CutPrefix(path, acmeChallengePrefix)
	if !ok || token == "" || len(token) > maxACMEToken {
		return false
	}
	for i := 0; i < len(token); i++ {
		if !isBase64URLByte(token[i]) {
			return false
		}
	}
	return true
}

// isBase64URLByte reports whether c belongs to the base64url alphabet, which
// is what an ACME token is written in.
func isBase64URLByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
		return true
	}
	return false
}

// plainHTTP reports whether a request reached the application over plain
// HTTP, and names the header the answer was read from when one was.
//
// A request that arrived over TLS is not plain. One that did not is plain
// unless the peer that sent it is a proxy [ClientIPOptions.TrustedProxies]
// names, and that proxy says the client used https. A proxy says so the way
// [ClientIPOptions.Header] says it reports addresses: in the proto parameter
// of RFC 7239's Forwarded when that is the header named, and in
// X-Forwarded-Proto otherwise. Only the value the trusted peer itself wrote is
// read, which is the last one: an earlier value may have come from the client,
// through a proxy that appends rather than replaces, and a client that could
// claim https would have its plain request served, and kept by a cache, as
// though it were secure. A request from any other peer is plain whatever it
// claims.
func plainHTTP(r *http.Request, ips *clientIPResolver) (plain bool, consulted string) {
	if r.TLS != nil {
		return false, ""
	}
	peer, ok := peerAddr(r.RemoteAddr)
	if !ok || !ips.trusts(peer) {
		return true, ""
	}
	if ips.rfc7239 {
		return !forwardedHTTPS(r.Header.Values("Forwarded")), "Forwarded"
	}
	proto := ""
	if values := r.Header.Values("X-Forwarded-Proto"); len(values) > 0 {
		proto, _ = lastField(values[len(values)-1])
	}
	return !strings.EqualFold(proto, "https"), "X-Forwarded-Proto"
}

// forwardedHTTPS reports whether the last entry of a Forwarded header says the
// request arrived over https.
func forwardedHTTPS(values []string) bool {
	if len(values) == 0 {
		return false
	}
	element, _ := lastForwardedElement(values[len(values)-1])
	proto, ok := forwardedParam(element, "proto")
	return ok && strings.EqualFold(proto, "https")
}

// forwardedParam returns the value of one parameter of a Forwarded entry,
// unquoted, as [forwardedFor] does for the for parameter. It reports false for
// an entry that is not well formed or names the parameter other than once.
func forwardedParam(element, name string) (string, bool) {
	var value string
	found := false
	for element != "" {
		pair, rest, ok := nextForwardedPair(element)
		if !ok {
			return "", false
		}
		element = rest
		key, raw, _ := strings.Cut(pair, "=")
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		if found {
			return "", false
		}
		if value, ok = unquoteForwardedValue(strings.TrimSpace(raw)); !ok {
			return "", false
		}
		found = true
	}
	return value, found
}
