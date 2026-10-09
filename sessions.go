package muzak

import (
	"context"
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Defaults applied when [SessionOptions] leaves them unset.
const (
	// DefaultSessionIdleTimeout is how long a session survives without a
	// request, at two hours. It is a default for an application that has not
	// thought about it yet, which is why it is short: a session is a bearer
	// credential, and one left signed in on a shared computer is one somebody
	// else can use for as long as this allows.
	DefaultSessionIdleTimeout = 2 * time.Hour
	// DefaultSessionMaxLifetime is how long a session survives at all, however
	// busy, at 24 hours. Past it the session is gone and the user signs in
	// again, which is what bounds the use of a session cookie that was stolen
	// and is being kept alive by its thief.
	DefaultSessionMaxLifetime = 24 * time.Hour
	// DefaultSessionStoreTimeout bounds one call to a [SessionStore], at two
	// seconds, as [DefaultRateLimitStorageTimeout] bounds a rate limit storage.
	DefaultSessionStoreTimeout = 2 * time.Second
	// DefaultSessionMaxSize is the largest session a server-side
	// [SessionStore] is given, at 16 KiB of encoded data. The cookie store's
	// bound is whatever fits in one cookie, and is computed instead.
	DefaultSessionMaxSize = 16 << 10
	// MinSessionSecretLength is the shortest secret [SessionOptions.Secrets]
	// accepts, in bytes. A secret is the key every session cookie is encrypted
	// under, so it is held to the length of the key it becomes.
	MinSessionSecretLength = 32
)

// maxCookieBytes is the most a session's Set-Cookie may hold, name,
// value and attributes together. RFC 6265 asks a browser to keep at least
// 4096 bytes per cookie measured that way, which makes it the largest cookie
// that every browser is sure to keep; one past it is dropped by some of them
// without a word, which is a session that silently never sticks.
const maxCookieBytes = 4096

// Errors a [Session] reports.
var (
	// ErrSessionTooLarge reports a change that would make the session larger
	// than [SessionOptions.MaxSize], or than one cookie can carry. The change
	// is refused rather than truncated, because a session that loses some of
	// its keys on the way to the browser is one that misbehaves a request
	// later, far from the code that overfilled it.
	ErrSessionTooLarge = errors.New("muzak: the session is larger than its store can hold")

	// ErrSessionUnavailable reports a [SessionStore] that could not be read.
	// The session then reads as empty and refuses every change, so that a
	// store that is down cannot be mistaken for a user who is signed out and
	// then overwritten as one. [Session.Err] returns it, wrapping the store's
	// own error.
	ErrSessionUnavailable = errors.New("muzak: the session store could not be read")
)

var (
	// errSessionsOff is what [Context.Session] panics with in an application
	// that configured no sessions: a handler asking for one is a programming
	// error that no request can correct.
	errSessionsOff = errors.New("muzak: Context.Session was called, but AppOptions.Sessions is not set")
	// errSessionEnded is what a session reports when it is used after its
	// request has ended.
	errSessionEnded = errors.New("muzak: the session belongs to a request that has already ended")
	// errSessionStarted is what [Session.Save] reports once the response has
	// started, when a Set-Cookie can no longer reach the client.
	errSessionStarted = errors.New("muzak: the session cannot be saved once the response has started")
)

// SessionOptions configures the sessions [Context.Session] gives a request.
//
// Sessions are opt-in, and an application that sets none pays nothing for
// them, not even a field read on a request. With them set, a request still
// pays nothing until its handler, a guard or a provider calls
// [Context.Session]: no cookie is parsed, nothing is decrypted and no store
// is asked.
//
// Sessions authenticate a browser by a cookie it sends on its own, which is
// what cross-site request forgery exploits, so they cannot be configured
// without [AppOptions.CrossOriginProtection]; see [CrossOriginOptions].
//
// The cookie is HttpOnly, Secure, SameSite=Lax and scoped to the whole
// origin by default. Its name is "__Host-session", which a browser accepts
// only from a secure origin, only for the path "/" and only without a
// Domain: a sibling subdomain or a page served over plain HTTP on the same
// host cannot set a cookie of that name, so neither can plant a session of its
// own choosing in the user's browser or shadow the real one. A cookie that
// names a Domain cannot carry the prefix and is named "__Secure-session"
// instead, and one that is not Secure carries neither.
type SessionOptions struct {
	// Secrets are the keys the cookie store encrypts with, each at least
	// [MinSessionSecretLength] bytes of randomness: generate one with
	// "openssl rand -base64 32" and keep it out of the source. The first
	// encrypts every cookie written; every one of them decrypts. Rotating a
	// secret is putting a new one first and keeping the old one after it until
	// [SessionOptions.MaxLifetime] has passed, when no cookie encrypted under
	// it can still be valid. Decrypting tries each secret in turn, so a cookie
	// costs one decryption per secret listed at most.
	//
	// They are required by the cookie store and unused by a [SessionStore].
	Secrets []string

	// Store keeps sessions on the server, leaving the cookie to carry only a
	// random 256-bit identifier. Nil, the default, keeps the session in the
	// cookie itself, encrypted and authenticated with AES-256-GCM; see
	// [SessionOptions.Secrets].
	//
	// The cookie store needs nothing running and nothing shared between
	// replicas, and the price is that a session cannot be revoked: a copy of
	// a cookie taken before [Session.Destroy] ran stays valid until it
	// expires, and nothing the server holds can say otherwise. An application
	// that must be able to end a session everywhere, at logout, on a password
	// change or when an account is suspended, wants a server-side store.
	Store SessionStore

	// Name is the cookie's name. It defaults to "__Host-session", or to
	// "__Secure-session" with a Domain or a Path other than "/", or to
	// "session" when AllowInsecure is set; see [SessionOptions] for why. A
	// name must be an HTTP token, and one beginning with "__Host-" or
	// "__Secure-", in any case, must satisfy what browsers require of those
	// prefixes; either mistake is a build error rather than a cookie a
	// browser silently refuses.
	Name string

	// Path scopes the cookie, defaulting to "/". A narrower path does not
	// keep the session from the rest of the origin, which any page on it can
	// reach by script or frame, so it is not a security boundary.
	Path string

	// Domain sends the cookie to every subdomain of the one named as well,
	// and is empty by default, which keeps it to the host that set it. Every
	// subdomain is then trusted with the session, and any one of them can set
	// a cookie of the same name that the browser sends in its place.
	Domain string

	// SameSite is the cookie's SameSite attribute, [http.SameSiteLaxMode]
	// by default. Strict also withholds the cookie from a link followed from
	// another site, so the user arrives signed out. None sends it with every
	// cross-site request, which needs Secure and leaves
	// [AppOptions.CrossOriginProtection] as the only defence against forgery.
	// [http.SameSiteDefaultMode], which writes no attribute and leaves the
	// choice to each browser, is refused.
	SameSite http.SameSite

	// AllowInsecure sends the cookie without the Secure attribute, so that a
	// browser sends it over plain HTTP, where anyone on the network can read
	// it. It is for development on a host other than localhost: browsers
	// accept a Secure cookie from http://localhost already. It is off by
	// default, and cannot be combined with SameSite None or a prefixed name.
	AllowInsecure bool

	// IdleTimeout ends a session that has not been used for this long,
	// defaulting to [DefaultSessionIdleTimeout]. The time is kept inside the
	// encrypted cookie or the stored session, never taken from the cookie's
	// own attributes, which the client controls. A session in use is renewed
	// at most once in a tenth of this, so the cookie is not rewritten on every
	// request; a session therefore lapses between nine tenths of the timeout
	// and the whole of it after its last use. It must not exceed MaxLifetime.
	IdleTimeout time.Duration

	// MaxLifetime ends a session this long after it began however busy it is,
	// defaulting to [DefaultSessionMaxLifetime]. [Session.Regenerate] begins
	// it again. Like the idle timeout it is kept where the client cannot
	// change it.
	MaxLifetime time.Duration

	// MaxSize bounds the session's encoded data in bytes. A change that would
	// exceed it fails with [ErrSessionTooLarge] where it is made. With a
	// server-side store it defaults to [DefaultSessionMaxSize]. The cookie
	// store holds what fits in one 4096-byte cookie once it is encrypted and
	// encoded, a little under 3 KB of data with the default name, and that is
	// its default; a larger value is a build error.
	MaxSize int

	// StoreTimeout bounds one call to the [SessionStore], defaulting to
	// [DefaultSessionStoreTimeout]. It must not be negative.
	StoreTimeout time.Duration

	// now is the clock, replaceable so that a test can expire a session
	// without waiting for it.
	now func() time.Time
}

// sessionManager is [SessionOptions] resolved at build: the cookie's
// attributes fixed, the secrets turned into keys and the bounds computed, so
// that loading a session does nothing but load it.
type sessionManager struct {
	name     string
	path     string
	domain   string
	sameSite http.SameSite
	secure   bool

	idle       time.Duration
	lifetime   time.Duration
	renewAfter time.Duration
	// maxSize bounds the encoded JSON object a session holds.
	maxSize int
	timeout time.Duration

	// Exactly one of the two is set: sealer for the cookie store, store for
	// a server-side one.
	sealer *cookieSealer
	store  SessionStore

	now    func() time.Time
	logger *slog.Logger
}

// newSessionManager resolves the options, reporting every one that cannot be
// served as one joined error.
func newSessionManager(opts SessionOptions, logger *slog.Logger) (*sessionManager, error) {
	m := &sessionManager{
		path:     opts.Path,
		domain:   opts.Domain,
		sameSite: opts.SameSite,
		secure:   !opts.AllowInsecure,
		idle:     orDefaultDuration(opts.IdleTimeout, DefaultSessionIdleTimeout),
		lifetime: orDefaultDuration(opts.MaxLifetime, DefaultSessionMaxLifetime),
		timeout:  orDefaultDuration(opts.StoreTimeout, DefaultSessionStoreTimeout),
		store:    opts.Store,
		now:      opts.now,
		logger:   logger,
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.path == "" {
		m.path = "/"
	}
	if m.sameSite == 0 {
		m.sameSite = http.SameSiteLaxMode
	}
	m.name = opts.Name
	if m.name == "" {
		m.name = defaultSessionCookieName(m.secure, m.domain, m.path)
	}

	var errs []error
	switch {
	case opts.IdleTimeout < 0:
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.IdleTimeout is %s; it must not be negative", opts.IdleTimeout))
	case opts.MaxLifetime < 0:
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.MaxLifetime is %s; it must not be negative", opts.MaxLifetime))
	case m.idle > m.lifetime:
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.IdleTimeout (%s) is longer than SessionOptions.MaxLifetime (%s), "+
			"so it could never end a session; shorten it or lengthen the lifetime", m.idle, m.lifetime))
	}
	if opts.StoreTimeout < 0 {
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.StoreTimeout is %s; it must not be negative, since a "+
			"session store that stops answering would otherwise hold every request that reads a session", opts.StoreTimeout))
	}
	if opts.MaxSize < 0 {
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.MaxSize is %d; it must not be negative", opts.MaxSize))
	}
	cookieErrs := m.checkCookie()
	errs = append(errs, cookieErrs...)
	// A tenth of the idle timeout, so a session in use is rewritten a few
	// times an idle period rather than on every request; see IdleTimeout.
	m.renewAfter = m.idle / 10

	if m.store == nil {
		sealer, err := newCookieSealer(opts.Secrets, m.name)
		if err != nil {
			errs = append(errs, err)
		}
		m.sealer = sealer
		// Measured only on a cookie that is valid, since net/http logs
		// rather than refuses the attributes of one that is not.
		limit := 0
		if len(cookieErrs) == 0 {
			limit = m.cookieCapacity()
		}
		switch {
		case len(cookieErrs) > 0:
		case limit < minSessionCapacity:
			errs = append(errs, fmt.Errorf("muzak: the session cookie's name, path and domain leave room for only %d bytes "+
				"of session data in a %d-byte cookie; shorten them", max(limit, 0), maxCookieBytes))
		case opts.MaxSize > limit:
			errs = append(errs, fmt.Errorf("muzak: SessionOptions.MaxSize is %d bytes, but an encrypted cookie can carry "+
				"at most %d; keep the session smaller or configure a SessionOptions.Store", opts.MaxSize, limit))
		case opts.MaxSize > 0:
			m.maxSize = opts.MaxSize
		default:
			m.maxSize = limit
		}
	} else {
		m.maxSize = opts.MaxSize
		if m.maxSize == 0 {
			m.maxSize = DefaultSessionMaxSize
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return m, nil
}

// minSessionCapacity is the least session data a cookie must be able to
// carry for the cookie store to be worth configuring: a user identifier and
// little else.
const minSessionCapacity = 256

// defaultSessionCookieName chooses the strongest prefix the cookie's
// attributes allow; see [SessionOptions].
func defaultSessionCookieName(secure bool, domain, path string) string {
	switch {
	case !secure:
		return "session"
	case domain == "" && path == "/":
		return "__Host-session"
	default:
		return "__Secure-session"
	}
}

// checkCookie reports the cookie attributes no browser would accept as
// written, and the combinations that would weaken the cookie without saying
// so.
func (m *sessionManager) checkCookie() []error {
	var errs []error
	probe := &http.Cookie{Name: m.name, Value: "x", Path: m.path, Domain: m.domain} //nolint:gosec // only validated, never sent
	if err := probe.Valid(); err != nil {
		errs = append(errs, fmt.Errorf("muzak: the session cookie is not one a browser accepts (name %q, path %q, domain %q): %w",
			m.name, m.path, m.domain, err))
	}
	if !strings.HasPrefix(m.path, "/") {
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.Path %q must begin with a slash", m.path))
	}
	switch m.sameSite {
	case http.SameSiteLaxMode, http.SameSiteStrictMode:
	case http.SameSiteNoneMode:
		if !m.secure {
			errs = append(errs, errors.New("muzak: SessionOptions.SameSite None requires a Secure cookie, which browsers "+
				"insist on; remove AllowInsecure or choose Lax"))
		}
	default:
		errs = append(errs, fmt.Errorf("muzak: SessionOptions.SameSite %d is not Lax, Strict or None; a cookie with no "+
			"SameSite attribute is treated differently by different browsers, so choose one", m.sameSite))
	}
	// Browsers match the prefixes without regard to case, so a name that
	// differs only in case is held to the same rules.
	lower := strings.ToLower(m.name)
	switch {
	case strings.HasPrefix(lower, "__host-"):
		if !m.secure || m.domain != "" || m.path != "/" {
			errs = append(errs, fmt.Errorf("muzak: the session cookie %q has the __Host- prefix, which a browser accepts "+
				"only on a Secure cookie with the path \"/\" and no Domain; remove AllowInsecure, Domain and Path, or rename it", m.name))
		}
	case strings.HasPrefix(lower, "__secure-"):
		if !m.secure {
			errs = append(errs, fmt.Errorf("muzak: the session cookie %q has the __Secure- prefix, which a browser accepts "+
				"only on a Secure cookie; remove AllowInsecure or rename it", m.name))
		}
	}
	return errs
}

// cookie returns the session cookie carrying value for ttl, or, with a
// negative ttl, the one that removes it.
func (m *sessionManager) cookie(value string, ttl time.Duration) *http.Cookie {
	maxAge := -1
	if ttl >= 0 {
		// Whole seconds, and never zero, which would remove the cookie
		// rather than keep it.
		maxAge = max(int(ttl/time.Second), 1)
	}
	return &http.Cookie{ //nolint:gosec // Secure unless AllowInsecure, HttpOnly always, SameSite validated at build
		Name:     m.name,
		Value:    value,
		Path:     m.path,
		Domain:   m.domain,
		MaxAge:   maxAge,
		Secure:   m.secure,
		HttpOnly: true,
		SameSite: m.sameSite,
	}
}

// cookieCapacity returns the most session data, in encoded JSON bytes, that
// the cookie store can fit in one cookie of at most [maxCookieBytes].
//
// The attributes are measured with the longest Max-Age the cookie can carry,
// so the bound holds for every cookie written.
func (m *sessionManager) cookieCapacity() int {
	fixed := len(m.cookie("", m.lifetime).String())
	value := maxCookieBytes - fixed
	// base64url without padding encodes n bytes in ceil(4n/3) characters, so
	// this is the most bytes that encode within value characters.
	sealed := value * 3 / 4
	return sealed - cookieOverhead - recordHeaderSize
}

// ttl returns how long a session created at created may live from now: the
// idle timeout, or what is left of its lifetime if that is shorter.
func (m *sessionManager) ttl(created, now time.Time) time.Duration {
	return max(min(m.idle, created.Add(m.lifetime).Sub(now)), time.Second)
}

// storeContext returns the context a store call runs under: bounded by
// [SessionOptions.StoreTimeout], and for a write detached from the request's
// cancellation, so that a client hanging up after logging out does not
// cancel the deletion of its session half way through.
func (m *sessionManager) storeContext(c *Context, write bool) (context.Context, context.CancelFunc) {
	ctx := c.Context()
	if write {
		ctx = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(ctx, m.timeout)
}

// Session is one request's session: string keys holding JSON values, kept
// between requests in an encrypted cookie or a [SessionStore].
//
//	func Login(ctx *muzak.Context, in LoginIn) (LoginOut, error) {
//		user, err := accounts.Verify(ctx.Context(), in.Username, in.Password)
//		if err != nil {
//			return LoginOut{}, muzak.Unauthorized("the username or password is incorrect")
//		}
//		s := ctx.Session()
//		// Always, before anything else: see Regenerate.
//		if err := s.Regenerate(); err != nil {
//			return LoginOut{}, err
//		}
//		if err := s.Set("user", user.ID); err != nil {
//			return LoginOut{}, err
//		}
//		return LoginOut{Username: user.Name}, nil
//	}
//
//	func Profile(ctx *muzak.Context, _ muzak.Empty) (ProfileOut, error) {
//		id, ok := muzak.SessionGet[int64](ctx.Session(), "user")
//		if !ok {
//			return ProfileOut{}, muzak.Unauthorized("sign in first")
//		}
//		...
//	}
//
// Changes are written once, when the request ends: after the handler has
// returned, after every [Acquire] release has run, and before the response
// is written, so that a transaction that fails to commit takes the session
// change with it. They are written only for a request that succeeded with a
// status below 400; a handler that wants a change kept whatever happens next,
// such as a count of failed sign-in attempts, calls [Session.Save]. A
// session nothing changed is not written, except to renew one that is a
// tenth of its idle timeout old, and a request that never asked for its
// session writes nothing at all. A response that carries the cookie is marked
// "Cache-Control: private, no-cache", replacing a Cache-Control that would let
// a shared cache keep it, since a cache that stored it would hand one user's
// session to the next; a response from a handler that read the session varies
// on Cookie and is marked the same way unless the handler set its own.
//
// A handler that writes its response itself, through
// [Context.ResponseWriter] or by returning a stream, has its session written
// as the status line goes out, if that status is below 400, since a cookie
// cannot follow it. An event stream or a WebSocket started its response
// before its handler ran, so a session changed there is not saved, and a
// warning is logged.
//
// A cookie that was tampered with, truncated, encrypted under a secret no
// longer listed, copied from a cookie of another name, or past its idle
// timeout or lifetime, reads as no session at all: the request proceeds
// signed out, and nothing is logged of its contents.
//
// Two requests carrying one session are each given a copy, and whichever
// writes last decides what is kept, its cookie or its entry in the store.
// Neither ever writes half of one. With a server-side store, a request that
// finishes after another destroyed or regenerated the session finds it gone
// and writes nothing, so it cannot bring the session back.
//
// A Session is not safe for concurrent use, and like the [Context] it came
// from it must not be used once the handler has returned: from then on every
// change fails.
type Session struct {
	m *sessionManager
	// c is the request the session belongs to, and nil once it has ended.
	c *Context

	data map[string]sessionValue
	// entries is the sum of what every member of data adds to the encoded
	// object, which with the braces and the commas is its exact size; see
	// [Session.encodedSize].
	entries int

	created time.Time
	issued  time.Time

	// id is the identifier the cookie carries for a server-side store, and
	// stored the key the store holds the session under; both are empty when
	// the store holds nothing for this session.
	id     string
	stored string

	// isNew reports that no valid session came with the request, or that the
	// one that did has since been destroyed, and hadCookie that the request
	// carried a cookie of the session's name, valid or not.
	isNew     bool
	hadCookie bool

	changed    bool
	regenerate bool
	destroyed  bool
	// settled is set once the session has been written or deliberately left
	// alone for good, so that nothing writes it twice.
	settled bool
	// header is the Set-Cookie value this session added to the response, so
	// that a later write replaces it rather than adding a second.
	header string

	// err is why the store could not be read, and lateErr why a write made as
	// the response started failed, held for the request to end with.
	err     error
	lateErr error
}

// sessionValue is one member of a session: its value encoded, and how many
// bytes the member adds to the encoded object.
type sessionValue struct {
	raw  jsontext.Value
	size int
}

// Session returns the request's session, reading it the first time it is
// asked for. It is never nil.
//
// Nothing is read until then, so a request whose handler, guards and
// providers never ask costs nothing: no cookie is parsed, nothing decrypted
// and no store consulted. It panics when [AppOptions.Sessions] is not set,
// since no request could make that call succeed.
//
// A session that could not be read reads as empty and refuses changes; see
// [Session.Err].
func (c *Context) Session() *Session {
	if c.session != nil {
		return c.session
	}
	if c.app == nil || c.app.sessions == nil {
		panic(errSessionsOff)
	}
	s := c.app.sessions.load(c)
	c.session = s
	// What the handler answers now depends on who asked, which a cache only
	// knows by the cookie; see [Session].
	c.w.varyOn("Cookie")
	setIfAbsent(c.w.Header(), "Cache-Control", privateCacheControl)
	// So a response the handler writes itself carries the session too.
	c.w.commitHook = c
	return s
}

// load reads the session the request carries, if any.
func (m *sessionManager) load(c *Context) *Session {
	s := &Session{m: m, c: c, isNew: true, data: map[string]sessionValue{}}
	cookie, err := c.r.Cookie(m.name)
	if err != nil || cookie.Value == "" {
		return s
	}
	s.hadCookie = true
	var record []byte
	if m.store == nil {
		plain, ok := m.sealer.open(cookie.Value)
		if !ok {
			m.unreadable(c, "it could not be decrypted with any configured secret")
			return s
		}
		record = plain
	} else {
		id, ok := parseSessionID(cookie.Value)
		if !ok {
			// Not an identifier this application issues, so the store is not
			// asked about it at all.
			m.unreadable(c, "it is not a session identifier")
			return s
		}
		key := sessionStoreKey(id)
		ctx, cancel := m.storeContext(c, false)
		data, found, err := m.store.Load(ctx, key)
		cancel()
		if err != nil {
			// Neither the identifier nor its key is logged: the first is a
			// credential, and the second names the session in the store.
			s.err = fmt.Errorf("%w: %w", ErrSessionUnavailable, err)
			s.stored = key
			m.logger.ErrorContext(c.Context(), "muzak: the session store could not be read; the request proceeds without its session",
				slog.String(RequestIDKey, c.RequestID()),
				slog.String("error", err.Error()))
			return s
		}
		if !found {
			return s
		}
		// Kept even if the record proves expired, so that the stale entry
		// is removed when this session is next written.
		s.stored = key
		record = data
		s.id = cookie.Value
	}
	if !m.decodeRecord(s, record) {
		s.id = ""
		m.unreadable(c, "it has expired or is malformed")
		return s
	}
	s.isNew = false
	return s
}

// unreadable records, at debug level, why a session cookie was ignored. The
// cookie itself is never logged: it is a credential, and a malformed one is
// whatever a client chose to send.
func (m *sessionManager) unreadable(c *Context, why string) {
	m.logger.DebugContext(c.Context(), "muzak: a session cookie was ignored because "+why,
		slog.String(RequestIDKey, c.RequestID()))
}

// The record a session is kept as, in the cookie or the store: a version
// byte, the time the session began and the time it was last written, each
// as milliseconds since the Unix epoch in eight big-endian bytes, and then
// the data as one JSON object.
const (
	recordVersion    = 1
	recordHeaderSize = 1 + 8 + 8
)

// decodeRecord fills s from a record, reporting false for one that is
// malformed, larger than the session may be, or expired. It is linear in the
// record's length, which the cookie's own bound or MaxSize limits.
//
// A record whose object is larger than the bound, one written under a larger
// MaxSize than this one say, is refused here, since any write to it would
// fail. Nothing it decodes to can be larger than the object it was read from:
// each key is re-encoded in its shortest form and each value kept verbatim, so
// the session's running size never exceeds what was checked.
func (m *sessionManager) decodeRecord(s *Session, record []byte) bool {
	if len(record) < recordHeaderSize+2 || record[0] != recordVersion || len(record)-recordHeaderSize > m.maxSize {
		return false
	}
	created := time.UnixMilli(int64(binary.BigEndian.Uint64(record[1:9]))) //nolint:gosec // a timestamp this package wrote
	issued := time.UnixMilli(int64(binary.BigEndian.Uint64(record[9:17]))) //nolint:gosec // a timestamp this package wrote
	now := m.now()
	if issued.Before(created) || now.Sub(issued) > m.idle || now.Sub(created) > m.lifetime {
		return false
	}
	object := record[recordHeaderSize:]
	if object[0] != '{' {
		return false
	}
	var data map[string]jsontext.Value
	// json/v2 refuses duplicate members and invalid UTF-8, so a record has
	// exactly one reading.
	if err := json.Unmarshal(object, &data); err != nil {
		return false
	}
	var scratch []byte
	for key, raw := range data {
		scratch, _ = jsontext.AppendQuote(scratch[:0], key)
		entry := sessionValue{raw: raw, size: len(scratch) + 1 + len(raw)}
		s.data[key] = entry
		s.entries += entry.size
	}
	s.created, s.issued = created, issued
	return true
}

// encodedSize returns how many bytes the session's data encodes to.
func (s *Session) encodedSize() int {
	return sizeWith(s.entries, len(s.data))
}

// sizeWith returns the size of a JSON object whose n members add up to
// entries bytes: the two braces and a comma between each pair of members.
func sizeWith(entries, n int) int {
	return 2 + entries + max(n-1, 0)
}

// encode returns the session's record, stamped with when it began and when
// it is being written.
func (s *Session) encode(created, issued time.Time) []byte {
	buf := make([]byte, 0, recordHeaderSize+s.encodedSize())
	buf = append(buf, recordVersion)
	buf = binary.BigEndian.AppendUint64(buf, uint64(created.UnixMilli())) //nolint:gosec // a time after 1970
	buf = binary.BigEndian.AppendUint64(buf, uint64(issued.UnixMilli()))  //nolint:gosec // a time after 1970
	buf = append(buf, '{')
	// Sorted, so a session encodes the same way every time.
	for i, key := range slices.Sorted(maps.Keys(s.data)) {
		if i > 0 {
			buf = append(buf, ',')
		}
		// Every key was quoted once already, when it was set or read, so
		// this cannot fail.
		buf, _ = jsontext.AppendQuote(buf, key)
		buf = append(buf, ':')
		buf = append(buf, s.data[key].raw...)
	}
	return append(buf, '}')
}

// SessionGet returns the value stored under key, decoded as T, and whether
// there was one.
//
//	cart, ok := muzak.SessionGet[[]Item](ctx.Session(), "cart")
//
// A value that does not decode as T, because an earlier release stored it as
// something else, reads as absent rather than failing the request: the
// session belongs to the user, and the user cannot fix it.
func SessionGet[T any](s *Session, key string) (T, bool) {
	var value T
	entry, ok := s.data[key]
	if !ok {
		return value, false
	}
	if err := json.Unmarshal(entry.raw, &value); err != nil {
		var zero T
		return zero, false
	}
	return value, true
}

// Has reports whether the session holds a value under key, without decoding
// it.
func (s *Session) Has(key string) bool {
	_, ok := s.data[key]
	return ok
}

// IsNew reports whether the request came without a valid session, or the
// session it came with has been destroyed: whether, as far as this session
// can tell, the user is anonymous.
func (s *Session) IsNew() bool { return s.isNew }

// Err reports why the session could not be read, wrapping
// [ErrSessionUnavailable] and the store's own error, and is nil otherwise.
//
// A session that could not be read reads as empty and refuses every change
// but [Session.Destroy], so that an outage of the store is not mistaken for
// an anonymous user and then saved over as one. A handler that would rather
// fail than serve such a request signed out checks it:
//
//	if err := ctx.Session().Err(); err != nil {
//		return Out{}, err
//	}
func (s *Session) Err() error { return s.err }

// Set stores value under key, encoded as JSON.
//
// It fails, leaving the session as it was, when the value cannot be encoded,
// when the key is empty or not valid UTF-8, when the session would grow past
// [SessionOptions.MaxSize] or what a cookie can carry, which is
// [ErrSessionTooLarge], and when the session could not be read; see
// [Session.Err]. Keep it to identifiers and small preferences: the session
// travels with every request, in the cookie store as part of the cookie.
func (s *Session) Set(key string, value any) error {
	if err := s.usable(); err != nil {
		return err
	}
	if key == "" {
		return errors.New("muzak: a session key must not be empty")
	}
	quoted, err := jsontext.AppendQuote(nil, key)
	if err != nil {
		return fmt.Errorf("muzak: the session key %q is not valid UTF-8", key)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("muzak: the session value for %q could not be encoded as JSON: %w", key, err)
	}
	entry := sessionValue{raw: raw, size: len(quoted) + 1 + len(raw)}
	entries, n := s.entries+entry.size, len(s.data)+1
	if old, had := s.data[key]; had {
		entries, n = entries-old.size, n-1
	}
	if size := sizeWith(entries, n); size > s.m.maxSize {
		return fmt.Errorf("%w: setting %q would make it %d bytes, and it may hold %d", ErrSessionTooLarge, key, size, s.m.maxSize)
	}
	s.data[key] = entry
	s.entries = entries
	s.changed = true
	return nil
}

// usable reports why the session cannot be changed: its request has ended,
// or its store could not be read.
func (s *Session) usable() error {
	if s.c == nil {
		return errSessionEnded
	}
	return s.err
}

// Delete removes the value under key, if there is one.
func (s *Session) Delete(key string) {
	if s.usable() != nil {
		return
	}
	if old, had := s.data[key]; had {
		delete(s.data, key)
		s.entries -= old.size
		s.changed = true
	}
}

// Clear removes every value. A session left empty when the request ends is
// not kept: its cookie is removed and its stored entry deleted, exactly as
// [Session.Destroy] would, except that a value set afterwards in the same
// request continues the same session rather than beginning a new one.
func (s *Session) Clear() {
	if s.usable() != nil || len(s.data) == 0 {
		return
	}
	clear(s.data)
	s.entries = 0
	s.changed = true
}

// Regenerate keeps the session's values and gives them a new session: a new
// identifier with a server-side store, with the old one deleted, and a new
// ciphertext with the cookie store, and in either case a new start for
// [SessionOptions.MaxLifetime].
//
// Call it whenever the privilege a session carries changes, and above all
// when a user signs in, before setting anything that says who they are.
// Without it a session fixation attack works: an attacker who planted their
// own session in the victim's browser, through a sibling subdomain when the
// cookie names a Domain, through a plain HTTP page on the same host when it
// is not Secure, or through a cross-site scripting bug, waits for the victim
// to sign in, and is then signed in as the victim with the session they
// already hold. Regenerating makes the session the attacker knows worthless at
// the moment it would have become valuable. The default "__Host-" cookie
// closes the first two routes; regenerating closes all of them.
//
// It fails only when the session could not be read, see [Session.Err], or
// its request has ended.
func (s *Session) Regenerate() error {
	if err := s.usable(); err != nil {
		return err
	}
	s.regenerate = true
	s.changed = true
	return nil
}

// Destroy ends the session: every value is removed, the cookie is removed
// from the browser, and with a server-side store its entry is deleted, so
// that a copy of the cookie taken earlier is worthless too. A value set
// afterwards in the same request begins a new session under a new
// identifier.
//
// It is what signing out calls. With the cookie store nothing can revoke a
// copy of the cookie taken before Destroy ran, which stays valid until its
// idle timeout or lifetime ends it; see [SessionOptions.Store]. Unlike every
// other change it is made even when the store could not be read, since the
// cookie can be removed regardless, and the deletion is attempted when the
// request ends.
func (s *Session) Destroy() {
	if s.c == nil {
		return
	}
	clear(s.data)
	s.entries = 0
	s.isNew = true
	s.destroyed = true
	s.regenerate = true
	s.changed = true
}

// Save writes the session now, rather than when the request ends, and
// reports whether it could be written.
//
// A change is otherwise kept only by a request that succeeds. One saved here
// is kept whatever happens next, which is what a count of failed sign-in
// attempts or a message for the next page wants. A change made after Save is
// written when the request ends, as any other, if the request succeeds, and
// replaces what Save wrote; the response never carries two cookies for one
// session.
//
// It fails once the response has started, when a cookie can no longer reach
// the client, when the session could not be read, and when the store refuses
// the write.
func (s *Session) Save() error {
	if s.c == nil {
		return errSessionEnded
	}
	if s.err != nil && !s.destroyed {
		return s.err
	}
	if s.c.w.written {
		return errSessionStarted
	}
	return s.write()
}

// needsWrite reports whether ending the request would write the session:
// whether it changed, or is an existing one due for renewal.
func (s *Session) needsWrite() bool {
	if s.changed {
		return true
	}
	return !s.isNew && len(s.data) > 0 && s.m.now().Sub(s.issued) >= s.m.renewAfter
}

// write puts the session where it is kept and the cookie that names it on
// the response, if anything has changed or it is due for renewal.
func (s *Session) write() error {
	if !s.needsWrite() {
		return nil
	}
	m := s.m
	if len(s.data) == 0 {
		return s.end()
	}
	now := m.now()
	fresh := s.isNew || s.regenerate
	created := s.created
	if fresh {
		created = now
	}
	record := s.encode(created, now)
	ttl := m.ttl(created, now)

	var value string
	if m.store == nil {
		sealed, err := m.sealer.seal(record)
		if err != nil {
			// coverage: sealing fails only if AES or HKDF refuse sizes fixed at build.
			return err
		}
		value = sealed
	} else {
		var ok bool
		var err error
		if value, ok, err = s.persist(record, ttl, fresh); err != nil || !ok {
			return err
		}
	}
	if err := s.setCookie(m.cookie(value, ttl)); err != nil {
		// coverage: setCookie refuses only a cookie the build already proved fits.
		return err
	}
	s.created, s.issued = created, now
	s.isNew, s.regenerate, s.destroyed, s.changed = false, false, false, false
	return nil
}

// persist writes a record to the server-side store and returns the
// identifier the cookie is to carry, or false when the session was deleted
// by another request while this one was running, in which case nothing is
// written: bringing it back would undo a sign-out.
//
// A fresh session, a new one or one regenerated, is created under a new
// identifier, after the entry it replaces is deleted; an identifier a client
// sent is never adopted for a new session, which is what would make fixation
// possible. Any other is updated in place.
func (s *Session) persist(record []byte, ttl time.Duration, fresh bool) (string, bool, error) {
	m := s.m
	if fresh {
		if s.stored != "" {
			if err := s.storeDelete(s.stored); err != nil {
				return "", false, err
			}
			s.stored, s.id = "", ""
		}
		id, key := newSessionID()
		ctx, cancel := m.storeContext(s.c, true)
		err := m.store.Create(ctx, key, record, ttl)
		cancel()
		if err != nil {
			return "", false, fmt.Errorf("muzak: the session could not be saved: %w", err)
		}
		s.id, s.stored = id, key
		return id, true, nil
	}
	ctx, cancel := m.storeContext(s.c, true)
	found, err := m.store.Update(ctx, s.stored, record, ttl)
	cancel()
	if err != nil {
		return "", false, fmt.Errorf("muzak: the session could not be saved: %w", err)
	}
	if !found {
		s.id, s.stored = "", ""
		s.isNew = true
		s.changed = false
		return "", false, nil
	}
	return s.id, true, nil
}

// end removes a session that has been emptied: the browser's cookie first, so
// that signing out signs the browser out even if the store then fails, and
// then the stored entry.
func (s *Session) end() error {
	if s.hadCookie || s.header != "" {
		if err := s.setCookie(s.m.cookie("", -1)); err != nil {
			// coverage: a removal carries no value, so it always fits.
			return err
		}
	}
	s.changed, s.regenerate = false, false
	s.isNew = true
	if s.stored == "" {
		return nil
	}
	key := s.stored
	s.stored, s.id = "", ""
	return s.storeDelete(key)
}

// storeDelete deletes one stored session.
func (s *Session) storeDelete(key string) error {
	ctx, cancel := s.m.storeContext(s.c, true)
	defer cancel()
	if err := s.m.store.Delete(ctx, key); err != nil {
		return fmt.Errorf("muzak: the session could not be deleted: %w", err)
	}
	return nil
}

// setCookie puts the session's cookie on the response, in place of any this
// session put there earlier, and keeps the response out of shared caches.
func (s *Session) setCookie(cookie *http.Cookie) error {
	value := cookie.String()
	if value == "" || len(value) > maxCookieBytes {
		// coverage: the attributes were validated and the capacity computed
		// when the application was built, so neither can happen; the check
		// is what stops a cookie a browser would drop from being sent.
		return fmt.Errorf("%w: the cookie would be %d bytes", ErrSessionTooLarge, len(value))
	}
	header := s.c.w.Header()
	if s.header != "" {
		if values := header["Set-Cookie"]; len(values) > 0 {
			header["Set-Cookie"] = slices.DeleteFunc(values, func(v string) bool { return v == s.header })
		}
	}
	header.Add("Set-Cookie", value)
	s.header = value
	if !keepsPrivate(header.Values("Cache-Control")) {
		header.Set("Cache-Control", privateCacheControl)
	}
	return nil
}

// keepsPrivate reports whether a Cache-Control already keeps a response out
// of shared caches, with a private or no-store directive. It is linear in the
// header's length.
func keepsPrivate(values []string) bool {
	for _, value := range values {
		for directive := range strings.SplitSeq(value, ",") {
			name, _, _ := strings.Cut(directive, "=")
			name = strings.TrimSpace(name)
			if strings.EqualFold(name, "private") || strings.EqualFold(name, "no-store") {
				return true
			}
		}
	}
	return false
}

// commitSession settles the session as the request ends, after its releases
// have run, and returns what the request ends with. See [Context.settle].
func (c *Context) commitSession(failure error) error {
	s := c.session
	// Every request that read its session ends here, before its Context goes
	// back to the pool, so this is where the writer stops calling back into
	// it.
	if c.w.commitHook == commitHook(c) {
		c.w.commitHook = nil
	}
	if s.settled {
		if failure == nil && s.lateErr != nil {
			failure, s.lateErr = s.lateErr, nil
			c.handled = false
		}
		return failure
	}
	s.settled = true
	if failure != nil || c.status >= http.StatusBadRequest {
		return failure
	}
	if s.err != nil && !s.destroyed {
		return nil
	}
	if !s.needsWrite() {
		return nil
	}
	if c.w.written {
		// The response began before the handler returned, as an event
		// stream's or a WebSocket's does, and a cookie can no longer follow
		// it. A renewal can wait for the next request; a change is lost.
		if s.changed {
			c.logger.WarnContext(c.Context(), "muzak: the session changed after the response had started, so the change was not saved",
				slog.String("route", c.route.pathOrRequest(c.r)),
				slog.String(RequestIDKey, c.RequestID()))
		}
		return nil
	}
	if err := s.write(); err != nil {
		// The request fails after all, so what it registered to follow it
		// with [Context.AfterResponse] does not run, as for a release.
		c.handled = false
		return err
	}
	return nil
}

// commitHook is told when a response is about to start, which is the last
// moment a header can be added to it.
type commitHook interface {
	beforeCommit(status int)
}

// beforeCommit writes the session of a handler that is writing its response
// itself, as the status goes out; see [Session]. A write that fails cannot
// change a status already chosen, so its error is held for the request to
// end with, which aborts a response that has started.
//
// The hook is installed only once a session has been read and is removed
// whenever the session is settled, so the session is always there and not
// yet settled when it is called.
func (c *Context) beforeCommit(status int) {
	s := c.session
	s.settled = true
	if status >= http.StatusBadRequest || (s.err != nil && !s.destroyed) {
		return
	}
	if err := s.write(); err != nil {
		s.lateErr = err
	}
}

// resetSession detaches the request's session as its Context goes back to
// the pool, so that a session a handler kept hold of cannot write to the
// next request's response. The writer's hook is already gone: the release
// that precedes this settles every session, and settling removes it.
func (c *Context) resetSession() {
	if c.session != nil {
		c.session.c = nil
		c.session = nil
	}
}
