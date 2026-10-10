package muzak

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// CodeCrossOriginRequest classifies a state-changing request a browser sent
// from another origin, refused because of [AppOptions.CrossOriginProtection].
const CodeCrossOriginRequest = "cross_origin_request"

// crossOriginMessage is what a refused request is told, in English, and the
// fallback for the translation at muzak.security.cross_origin. It names
// neither the origin nor the policy, which the client has no use for.
const crossOriginMessage = "Cross-origin requests that change state are not accepted."

// errCrossOrigin is the refusal of a cross-origin request. It is shared, and
// never edited: [App.fail] only reads it.
var errCrossOrigin = &HTTPError{
	Status:     http.StatusForbidden,
	Code:       CodeCrossOriginRequest,
	Message:    crossOriginMessage,
	MessageKey: "muzak.security.cross_origin",
}

// CrossOriginOptions configures [AppOptions.CrossOriginProtection], which
// refuses a state-changing request a browser sends from another origin.
//
// That is the shape of cross-site request forgery: a page elsewhere submits a
// form or makes a fetch to this application, and the browser attaches the
// user's cookies to it, so a session alone cannot tell the user's own
// request from one a hostile page made in their name. The check is net/http's
// [http.CrossOriginProtection], which needs no token in any form: every
// browser since 2023 says where a request came from in Sec-Fetch-Site, and
// one that does not is judged by whether its Origin names the host it was
// sent to.
//
// GET, HEAD and OPTIONS are never refused, which is why they must never
// change anything. Every other method is refused with 403, classified
// [CodeCrossOriginRequest] and rendered by the application's error renderer,
// problem details included, when the browser says it came from another
// origin, a sibling subdomain ("same-site") included, unless that origin is
// trusted or the request matches a bypass pattern. A request with neither
// header, from curl, a mobile application or another server, carries no
// ambient credentials a browser attached for it and is allowed.
//
// The check runs before anything else of the application's: no middleware
// installed with [App.Use], no guard, provider or handler, and no mount or
// documentation sees a refused request. It runs inside the CORS policy, so
// a refusal sent to an origin CORS allows carries the headers that let its
// script read why. CORS does not decide what is trusted here: CORS says which
// origins may read responses, and an origin allowed to read is not thereby
// allowed to change state, so an application whose own frontend lives on
// another origin lists that origin in TrustedOrigins as well.
//
// A WebSocket handshake is a GET, so this never refuses one; the handshake's
// origin is checked by [WSOptions.AllowedOrigins], which refuses a
// cross-origin handshake by default.
type CrossOriginOptions struct {
	// TrustedOrigins lists the origins allowed to send state-changing
	// requests from another origin, each written scheme://host[:port] as a
	// browser sends it in Origin, such as "https://app.example.com". They
	// are checked as [CORSOptions.AllowedOrigins] entries are: an entry that
	// could never match the header, with a path, a trailing slash, upper case
	// or a default port, is a build error naming the spelling to use, a
	// pattern is a build error, and "null", which any page can send, is
	// refused outright. Each origin is matched exactly, and nothing decides
	// one dynamically, so every subdomain to trust is listed.
	TrustedOrigins []string

	// InsecureBypassPatterns lists the requests exempt from the check, as
	// [http.ServeMux] patterns: "POST /webhooks/{provider}" or "/callback/".
	// A bypassed route accepts a forged request from any page, so list only
	// one that authenticates the request some other way, such as a webhook
	// that verifies its signature, or an endpoint a third party posts a form
	// to that carries no state worth forging. A request matches only as
	// ServeMux would match it directly; one ServeMux would first redirect,
	// to clean its path or add a slash, is checked as usual. A pattern
	// ServeMux would refuse, two that conflict, and one whose method is GET,
	// HEAD or OPTIONS, which are never refused and so bypass nothing, are
	// build errors.
	InsecureBypassPatterns []string
}

// newCrossOriginProtection builds the check, reporting every entry that
// cannot be served as one joined error.
func newCrossOriginProtection(opts CrossOriginOptions) (*http.CrossOriginProtection, error) {
	protection := http.NewCrossOriginProtection()
	var errs []error
	for _, origin := range opts.TrustedOrigins {
		if strings.Contains(origin, "*") {
			// Refused here rather than by checkOriginEntry, whose advice is
			// for a list beside an AllowOriginFunc, which this one has none of.
			errs = append(errs, fmt.Errorf("muzak: CrossOriginOptions.TrustedOrigins entry %q is a pattern, and "+
				"TrustedOrigins matches each origin exactly, so it never matches; list every origin to trust, "+
				"subdomains included, since cross-origin protection has no pattern or function that decides one", origin))
			continue
		}
		if err := checkOriginEntry("CrossOriginOptions.TrustedOrigins entry", origin, false); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := protection.AddTrustedOrigin(origin); err != nil {
			// coverage: checkOriginEntry accepts only an origin net/http
			// accepts too; this keeps the two from drifting apart silently.
			errs = append(errs, fmt.Errorf("muzak: CrossOriginOptions.TrustedOrigins entry %q: %w", origin, err))
		}
	}
	for _, pattern := range opts.InsecureBypassPatterns {
		if err := addBypassPattern(protection, pattern); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return protection, nil
}

// addBypassPattern adds one bypass pattern, turning the panic net/http raises
// for a pattern it cannot parse or that conflicts with another into an error.
func addBypassPattern(protection *http.CrossOriginProtection, pattern string) (err error) {
	if method, _, found := strings.Cut(pattern, " "); found {
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return fmt.Errorf("muzak: CrossOriginOptions.InsecureBypassPatterns entry %q names %s, which cross-origin "+
				"protection never refuses, so it bypasses nothing; name the method that changes state, or none", pattern, method)
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("muzak: CrossOriginOptions.InsecureBypassPatterns entry %q is not a pattern net/http accepts: %v",
				pattern, recovered)
		}
	}()
	protection.AddInsecureBypassPattern(pattern)
	return nil
}

// crossOriginMiddleware returns the check as middleware.
//
// A GET, HEAD or OPTIONS is passed on after a comparison of its method, which
// is all the check costs a read; anything else reads two request headers.
func (a *App) crossOriginMiddleware(protection *http.CrossOriginProtection) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := protection.Check(r); err != nil {
				a.refuseCrossOrigin(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// refuseCrossOrigin answers a refused request with 403 through the error
// renderer, and records where it came from.
//
// The refusal depends on Origin and Sec-Fetch-Site, so it says so in Vary,
// although an error is sent with "Cache-Control: no-store" and should never
// be stored at all.
func (a *App) refuseCrossOrigin(w http.ResponseWriter, r *http.Request) {
	rw := asResponseWriter(w)
	rw.varyOn("Origin", "Sec-Fetch-Site")
	c := a.acquire(rw, r)
	defer a.release(c)
	a.logger.WarnContext(r.Context(), "muzak: refused a state-changing request from another origin",
		slog.String("origin", quotableHost(r.Header.Get("Origin"))),
		slog.String("sec_fetch_site", quotableHost(r.Header.Get("Sec-Fetch-Site"))),
		slog.String("method", truncateForMessage(r.Method)),
		slog.String("path", truncateForMessage(r.URL.Path)),
		slog.String(RequestIDKey, c.RequestID()))
	a.fail(c, errCrossOrigin)
}

// buildSessions resolves [AppOptions.Sessions] and
// [AppOptions.CrossOriginProtection], reporting what cannot be served with
// the other build errors.
//
// Sessions without cross-origin protection are refused, not warned about. A
// session cookie is an ambient credential: the browser attaches it to a
// request whichever page made it. SameSite=Lax keeps it off a form another
// site posts, but not off one posted from a sibling subdomain, which is the
// same site, and SameSite=None keeps it off nothing. An application that
// trusts the cookie and does not check where its state-changing requests come
// from is open to forgery, the fix is one line, and a warning in a start-up
// log is read by nobody until it matters.
func (a *App) buildSessions(state *buildState) {
	if opts := a.opts.CrossOriginProtection; opts != nil {
		protection, err := newCrossOriginProtection(*opts)
		if err != nil {
			state.errs = append(state.errs, err)
		} else {
			a.crossOrigin = a.crossOriginMiddleware(protection)
		}
	}
	opts := a.opts.Sessions
	if opts == nil {
		return
	}
	if a.opts.CrossOriginProtection == nil {
		state.errs = append(state.errs, errors.New("muzak: AppOptions.Sessions is set without AppOptions.CrossOriginProtection; "+
			"a session cookie is sent with requests other pages make, so state-changing requests must be checked for "+
			"where they came from. Set CrossOriginProtection: &muzak.CrossOriginOptions{}, and list the origins of any "+
			"frontend served from elsewhere in its TrustedOrigins"))
	}
	manager, err := newSessionManager(*opts, a.logger)
	if err != nil {
		state.errs = append(state.errs, err)
		return
	}
	a.sessions = manager
	// A store that manages something, a connection pool say, is brought up
	// with the application, as a rate limit storage is.
	if opts.Store != nil {
		state.lifecycles = observabilityComponents(state.lifecycles, opts.Store)
	}
}
