# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0.0, a minor bump may carry a breaking change. Each one is listed under
**Changed** with the migration.

## [Unreleased]

This release is the result of two rounds of adversarial review of the
framework, run as an attacker would against a service built on it, the second
of them also against the first round's fixes. Every finding below was
reproduced with a test before it was fixed, and each of those tests is now a
regression test. Several fixes tighten a default; each one is listed under
**Changed** with its migration.

### Added

- **Declare how a route authenticates, so the documentation can offer
  Authorize.** A guard is a function the framework can run but not read, so the
  document could never say whether a route wants a bearer token, an API key or a
  session cookie, and guarded operations looked public. `AppOptions.SecuritySchemes`
  now declares named schemes (`muzak.BearerAuth("JWT")`, `BasicAuth()`,
  `HTTPAuth("digest")`, `APIKeyHeader` / `APIKeyQuery` / `APIKeyCookie`,
  `OAuth2(OAuthFlows{...})`, `OpenIDConnect(url)`), and `WithSecurity` says which
  a router or route sits behind, with scopes: `muzak.WithSecurity(muzak.Require("oauth", "items:read"))`.
  `Public()` marks an exception, and is written as an empty `security` list. They
  are emitted as `components.securitySchemes` and a `security` list on each
  operation that declared one. A route's declaration replaces its router's, and
  several requirements are alternatives (a `SecurityRequirement` naming several
  schemes needs all of them). Nothing changes for an application that declares
  none: the document carries neither. **A scheme only describes.** It is never
  consulted while a request is served, so naming one protects nothing; the route
  still needs the `Guard` that refuses. Schemes are checked when the application
  is built: a name outside `[A-Za-z0-9._-]`, a type OpenAPI does not define, an
  `http` scheme with no `Scheme`, an `apiKey` with no valid `In` or `Name`, an
  `oauth2` scheme with no flow or a flow without the URL its kind needs, an
  `openIdConnect` URL, a flow URL that is not `http` or `https`, a route naming a
  scheme that is not declared or a scope an OAuth 2.0 scheme does not offer, and
  a `WithSecurity()` naming nothing are all build errors.

- **`SSEOptions.MaxLifetime` and `WSOptions.MaxLifetime` end a long-lived
  connection on purpose.** The write timeout bounds one write and a keepalive
  is a few bytes, so a client that reads nothing while comments trickle in
  never fills a socket buffer, and a websocket peer that answers every ping and
  says nothing is alive as far as a keepalive can tell. Either held its slot,
  a goroutine and a file descriptor until the server stopped. When the lifetime
  has passed an event stream writes a closing comment and ends the way a
  finished response does, and a websocket is closed with `1001` going away, so
  a browser's `EventSource` reconnects on its own with `Last-Event-ID` and a
  handler that resumes from `stream.LastEventID()` loses nothing. The handler
  is told through its context and the errors later sends report, and the slot
  is released when it returns. Both are unset by default, which leaves
  connections unbounded as before; a negative value removes a bound a wider
  scope set. They are worth setting on any route a client one does not control
  can open. Migration: none; a client that does not reconnect on its own (a
  script using `SSEDial`) must be written to.

- **`WSOptions.AllowedHosts` says which `Host` values make an origin the
  server's own.** The same-origin rule compared the `Origin` header's host with
  the request's `Host`, which is whatever the client wrote, so in a
  DNS-rebinding attack a page served from the attacker's name, made to resolve
  to the server, sent that name as both and read as same-origin. A route that
  lists the names it answers to refuses any other `Host` with 403 unless
  `AllowedOrigins` or `AllowOriginFunc` names the origin. Unset, the rule is
  what it was, so nothing changes until it is set; it is worth setting for a
  server on a loopback or private address, where such a page is a real threat.
  The option bounds the websocket handshake only: a plain HTTP route is not
  protected against rebinding by it, and an application that wants every
  request held to its own names checks `Host` in a middleware.

### Changed

- **A JSON body sent without a `Content-Type` is refused with 415.** Accepting
  it let any page drive a JSON route cross-site without a CORS preflight, by
  posting an untyped `Blob`. A request with no body and no `Content-Type` is
  unaffected. Migration: send `Content-Type: application/json` (or a `+json`
  type) with every JSON body.

- **A location tag the binder cannot reach is a build error.** A `path`,
  `query`, `header`, `cookie`, `form` or `file` tag inside an embedded
  pointer, a named nested struct, or a slice, map or pointer of one was
  silently ignored, so the field became JSON body content and a request could
  set, for example, the user ID a gateway puts in a header. A location tag on
  an unexported field is refused the same way. Migration: embed the struct by
  value, move the field to the top level of the input, or export it.

- **Guards and providers given to `muzak.New` also cover the documentation.**
  `/openapi.json`, `/docs` and its assets used to be served before any guard,
  so an application-wide guard left every non-hidden route, header and schema
  readable by anyone. They now run the application's guards and providers
  first, and guarded docs are sent `Cache-Control: private, no-cache`. Router
  and route guards do not apply to the docs. Migration: to keep the docs
  public while the API is private, declare the guard where routers are
  included rather than on `New` (the example does this now), or set
  `AppOptions.DisableDocs`.

- **Frontend and Static mounts run their routers' providers and rate limits.**
  A mount ran only inherited guards, so a router authenticated with
  `Needs(...)` served its files to anyone, and no quota counted a file
  request. Mounts now apply the rate limit, guards and every inherited `Needs`
  and `Singleton` provider in route order. Migration: move assets meant to be
  public onto a router without those options, and use `SkipRateLimit()` to
  keep a mount outside a quota.

- **Frontend and Static mounts no longer serve dotfiles.** A path with any
  segment starting with `.` (`/.env`, `/.git/config`) answers 404, with no SPA
  fallback. A leading `/.well-known/` is still served. Migration: set
  `FrontendOptions.AllowDotfiles` or `StaticOptions.AllowDotfiles` for a
  directory whose dotfiles are meant to be public.

- **Every provider along a route's chain runs.** Declaring the same type twice
  (`app.Include(admin, Needs(RequireAdmin))` with the admin router declaring
  `Needs(CurrentUser)` of that type) kept only the innermost provider, so the
  outer authorization never ran. Every provider now runs, outermost first, and
  the first error aborts the request; `From` and `TryFrom` return the
  innermost value. Migration: an outer provider that was deliberately replaced
  now also runs and can fail the request; declare it only where it applies, or
  give the two values distinct types.

- **An IPv6 client is counted by its /64.** `IPTracker` and the per-client
  connection caps (`MaxConnectionsPerIP`, `MaxStreamsPerIP`) keyed on the
  exact address, so one /64 got a fresh budget from every address in it. IPv4
  is still counted exactly and its keys are unchanged; IPv6 tracker keys are
  now `ip:<prefix>/64`. Migration: clients sharing a /64 now share a budget.
  Raise the limit, or set `Tracker: IPPrefixTracker(32, 128)` if a proxy in
  front already bounds per-address traffic.

- **A request that fails after its response has started aborts the
  connection.** A panic, or an error returned after the first byte, was logged
  and the response then ended cleanly, so a truncated export looked complete.
  Muzak now logs the failure and panics with `http.ErrAbortHandler`, which
  `net/http` turns into an aborted connection without logging it again.
  Hijacked connections, WebSocket and SSE routes are unchanged. The access log
  marks such requests `aborted=true`, and now also records a middleware panic
  as a 500. Migration: code that calls `App.ServeHTTP` or `Recovery` directly
  should expect `http.ErrAbortHandler` in this case.

- **A status coder error renders only its own message.** Below 500 the client
  gets the `StatusCoder`'s own `Error()` text; from 500 up it gets the standard
  internal message and the error is logged. Migration: put a 4xx explanation
  in the `StatusCoder`'s own `Error()`, and use an `*HTTPError` for a 5xx whose
  message is meant for the client.

- **A singleton caches only a successful value.** An error or a panic fails
  the request that triggered it and the next request retries. The provider now
  sees a context without the triggering request's cancellation or deadline.
  Migration: a provider that relied on the request deadline to bound its I/O
  should apply its own with `context.WithTimeout`.

- **A static route pattern must be validly encoded.** A static segment that is
  not (`/50%off`) or that encodes a `/` (`/a%2Fb`) could never match and is now
  a build error, and two spellings of one path (`/a`, `/%61`) conflict.
  Migration: write `%25` for a literal percent sign, and use a parameter for a
  value that may contain `/`.

- **Validation reports at most 100 failures, and follows nested models at
  most 32 levels deep.** A full report ends with a detail of kind
  `too_many_problems` and stops evaluating, and a model nested past the limit
  is refused as `too_deep`. The limits are `MaxValidationDetails` and
  `MaxNestedDepth`. `Nested` now validates the model when it is called, so
  code after it sees the nested model's transforms applied, and a model nested
  per element is named by its position (`items[3].name`). Migration: read the
  marker and resubmit to see further failures; a recursive model that must go
  deeper bounds its own depth; clients matching field names match the indexed
  ones.

- **`Timezone()` accepts only zone names spelled as the IANA database spells
  them.** Case variants, doubled or trailing slashes, `Local`, `posix/` and
  `right/` names, and zones newer than tzdata 2026c are refused. Migration:
  send canonical names.

- **A 422 no longer quotes the client's value or a parser's own error.** A
  rejected entry of a repeated parameter is named by position ("entry 2 must
  be a valid integer"), and a failing `encoding.TextUnmarshaler` reads "is not
  in the expected format" (`muzak.binding.format`). Migration: return an
  `*HTTPError` from `UnmarshalText` to send your own wording.

- **A urlencoded form body is bounded by `MaxBodySize` (1 MiB by default), not
  `MaxUploadSize` (32 MiB).** Multipart bodies keep `MaxUploadSize`.
  Migration: set `MaxBodySize` on a form route that takes larger urlencoded
  bodies.

- **Error responses drop the headers set for the response they replace, and
  are sent with `Cache-Control: no-store`.** Content-Type, Content-Length,
  Content-Disposition, Content-Encoding, Content-Range, ETag, Last-Modified,
  Cache-Control and Expires are removed, and Content-Language is reset to the
  request's locale. Vary, Retry-After, Allow, WWW-Authenticate, Set-Cookie and
  security headers are kept. Migration: a custom `ErrorRenderer` sets any
  header it wants.

- **Routes and file mounts behind a guard or a request-scoped provider are
  sent `Cache-Control: private, no-cache`.** A route keeps a value its handler
  or a middleware set; a mount replaces one. A singleton alone does not count.
  Event streams and WebSocket upgrades are unchanged. Migration: a handler
  serving public data sets its own `Cache-Control`.

- **A static mount serves HTML, SVG and other XML under
  `Content-Security-Policy: sandbox`, and no mount guesses a type from
  content.** A file with no extension or an unknown one is
  `application/octet-stream`. Migration: set `StaticOptions.AllowActiveContent`
  for a static site whose scripts must run, and give files an extension.

- **The WebSocket and SSE per-client caps count an IPv6 client by its /56,
  and the prefix is configurable.** New `ClientIPOptions.ConnectionIPv6Prefix`
  (default 56) and `ConnectionIPv4Prefix` (default 32) are checked at build.
  Migration: set `ConnectionIPv6Prefix: 64` to keep the previous grouping.

- **`SSEDial` does not follow redirects.** A 3xx is refused and the response
  returned with the error. Migration: dial the `Location` yourself if it is
  trusted.

- **Configuring an application after it was built panics.** `Use`,
  `Options`, `Include`, route, WebSocket and SSE registration, and `Frontend`
  and `Static` mounts after `Build`, `ServeHTTP`, `Document` or a run method
  used to be dropped silently, so a guard added late left routes open.
  Migration: make every configuration call before the first build.

- **Shutdown is bounded by one `ShutdownTimeout` deadline, and lifecycle
  `Stop` gets what remains.** The WebSocket, SSE and HTTP drains used to take a
  full timeout each, and `Stop` had no deadline and could run under live
  handlers. `Stop` now runs after in-flight handlers (hijacked ones included)
  return or a short grace past the deadline ends, with at least one second;
  `Run` returns once `Shutdown` has finished. Migration: make `Stop` honour its
  context, or raise `ShutdownTimeout`.

- **Log redaction matches keys that contain a sensitive term.** `db_password`,
  `X-Api-Key`, `jwt` and `session_id` are now redacted, and `jwt`, `bearer`,
  `signature` and `credential` join the defaults. A custom `RedactKeys` list is
  matched the same way. Migration: rename a non-secret key containing a term
  if it must be logged.

- **The documentation counts against the application rate limit.** See
  **Security**. Migration: exempt it with `SkipRateLimit`, or disable the docs.

- **`HTTPError`'s `WithDetails`, `WithCode`, `WithMessageKey` and `Wrap`
  return a modified copy and leave the receiver untouched.** They edited the
  error in place, so a package-level error decorated per request handed one
  request's details to the next response, grew them with every request and
  raced between concurrent ones. Migration: use the returned value (every
  documented use already does); a call whose result is discarded, such as
  `err.WithCode("x")` on its own line, no longer has any effect.

- **A certificate without a key, or a key without a certificate, is a build
  error.** With only one of `ServerOptions.CertFile` and `KeyFile` set the
  server came up in plaintext on the port its operator believed was HTTPS,
  with nothing said. Migration: set both, or neither.

- **`required:"true"` on a config field refuses an empty value.** `API_KEY=`,
  which Compose and Kubernetes produce when they expand an unset variable,
  satisfied `required` and loaded an empty secret. An empty value is now
  treated as missing, so a `default` applies if there is one and the load
  fails otherwise. Fields that are not `required` are unchanged. Migration:
  drop `required` from a field whose empty value is legitimate, or give it a
  `default`.

- **The context `Lifecycle.Start` receives stays live until the components
  are stopped.** It was cancelled as soon as `Start` returned, so a component
  that kept it for a background worker had the worker cancelled during boot.
  It is still cancelled when a sibling fails to start and when the context
  passed to `StartLifecycle` is; it is now also cancelled after `Stop` has
  run.

- **A route template that declares a parameter name twice is a build error.**
  `/orgs/{id}/users/{id}` used to build, and a handler binding `path:"id"`
  silently received the first segment, so a check on "the user's id" was made
  against the organisation's. Migration: give each parameter its own name.

- **Two static mounts at one path are a build error.** A second `Static` or
  `Frontend` at a path used to lose to the first without a word, guards
  included. Migration: keep one mount per path, or give them different paths.

- **`Accept-Encoding: deflate` is answered in the zlib format.** RFC 9110
  defines the `deflate` content coding as zlib around DEFLATE, and the bare
  DEFLATE stream sent before was refused by strict decoders. A client that
  decodes it as raw DEFLATE, which was only ever a lenient guess, should
  decode zlib; every conforming client already does.

- **A version is read from `Accept` and from a version header more strictly.**
  With media type versioning, each range of `Accept` is parsed as a media
  range: the parameter name is matched without regard to case, a quoted value
  is unquoted, the range with the highest `q` picks the version (one refused
  with `q=0` is ignored), every `Accept` line is read, and a bare `v=1` that is
  not a media range no longer names a version. `Key` may be written with or
  without its `=`. With header versioning, a header sent on several lines is
  one list and names no version, so it matches no route rather than the first
  line's. A proxy that reads the same header the standard way now agrees with
  the application about which version, and so which guards, a request reached.
  Migration: send one `Accept` range per version, in the form
  `application/json;v=2`, and one line of the version header.

- **A multipart body sent to a route that binds no file is bounded by
  `MaxBodySize`, not `MaxUploadSize`.** A login or sign-up form that binds only
  `form:` values took its `multipart/form-data` body under the 32 MiB upload
  limit, so net/http held up to 20 MiB of text values (about 49 MiB of heap per
  in-flight request) before any rule could run, and a file sent to it, which
  nothing reads, was spooled to disk first. The urlencoded encoding of the same
  route was already bounded by `MaxBodySize`. A route that declares a `file:`
  field keeps `MaxUploadSize`. Migration: a form-only route that legitimately
  receives more than `MaxBodySize` (1 MiB by default) as multipart must raise
  `MaxBodySize` for it.

- **`Required()` fails a nil pointer field.** Every rule was skipped for a nil
  pointer, `Required()` included, so `v.String(&in.Name).Required()` on a
  `*string` accepted `{}` and `{"name":null}` while the generated document
  listed the field as required. A nil pointer now reports the field's
  `Required()` rule (`is required`, with its custom message if it has one); its
  other rules are still skipped. Migration: a pointer field that is meant to be
  optional must not declare `Required()`.

- **`Nested` that would validate nothing is a build error.** `v.Nested(in.Addr)`
  with a value instead of a pointer was skipped without a word, and a model
  whose `Validate` has a value receiver bound its rules to a copy, so
  `{"addr":{"city":""}}` passed a `Required()` city and its transforms were
  lost. Both are now refused when the route is compiled, for the input and for
  every model it nests. Migration: pass the model's address, `v.Nested(&in.Addr)`,
  and declare `Validate` on the pointer receiver.

- **`Unique()` treats two `time.Time` values as equal when they are the same
  instant.** It used `reflect.DeepEqual`, which compares zone pointers, so
  `12:00Z` and `13:00+01:00` were distinct. They are now a repeat, as
  `time.Time.Equal` has it. `[]time.Time` and `[]netip.Addr`, `[]netip.AddrPort`
  and `[]netip.Prefix` are checked in linear time (see Fixed).

- **`UUID()` accepts only the canonical 8-4-4-4-12 form.** It read a URN, braces
  and 32 bare digits as the same identifier, which the generated document's
  `format: uuid` does not describe and which let one UUID be held under several
  strings. Migration: normalise the spelling before validating, or use
  `Matches` for a wider set.

- **`Base64()`, `URL()`, `HTTPS()`, `URLWithSchemes()` and `NotBlank()` are
  stricter.** `Base64()` rejects a value with a CR or LF (Go's decoder skips
  them). The URL rules reject a port above 65535 and a value containing a
  control, format or separator character (bidirectional overrides, zero-width
  characters). `NotBlank()` treats a value of only invisible characters (zero
  width space, Hangul filler, braille blank, format characters) as blank.

- **`default:"..."` on a member of a JSON body is applied, and documented as
  its type.** The document listed the default and marked the member optional
  while the binder never filled it in. A top-level member (or one promoted from
  an embedded struct) that the client omits now takes the default; one it sends
  wins. A default that does not parse as its field's type is a build error, and
  a member whose type cannot be written from text (a struct, a map) still has no
  default. Defaults are now emitted as the JSON type of the member (`10`, not
  `"10"`) for query, header, form and body, and are no longer emitted on a
  nested member or on a response type, where nothing applies them.

- **The OpenAPI document no longer describes a `Clamp` as a range.** `Clamp`
  moves a value into range rather than rejecting one outside it, so `minimum`
  and `maximum` told a client that 500 is refused.

- **A WebSocket handshake's `Origin` is read strictly.** The same-origin rule
  took the host out of any value a lenient URL parse could find one in, so
  `http://attacker@host`, `http://host/path`, `http://host?x` and `//host`
  passed as the server's own origin, and only the first of several `Origin`
  headers was looked at. An origin is now a scheme and a host with nothing after
  it, and a handshake with more than one `Origin` header is refused with 400.
  Values listed in `AllowedOrigins`, and the ones `AllowOriginFunc` sees, are
  still compared exactly as they arrived. Migration: none for browsers, which
  send neither shape. A non-browser client that sent a decorated `Origin`
  must send a bare `scheme://host[:port]` or none at all.

- **A websocket connection that sends more than 65536 pings, pongs and empty
  fragments in a minute is closed with 1008.** The frame guard restarted with
  every message, so a peer completing one empty message per 65535 pings was
  answered without end while `MessageLimits` saw a single message per round.
  Migration: none for an honest peer, whose keepalive sends one ping every few
  seconds.

- **A negative `WSOptions.PongTimeout` means the default.** It was read as zero,
  so the first keepalive ping closed every connection whose peer could not
  answer instantly. Migration: none.

- **`SSEDial` bounds how long opening a stream may take.** It removed the
  supplied client's `Timeout` so that it would not cut the stream short, and
  put nothing in its place, so a server that accepted the request and never
  answered, or trickled the body of a refusal, held the call for as long as a
  context without a deadline lasts. Opening is now bounded by the new
  `SSEDialOptions.HandshakeTimeout`, which defaults to the `Timeout` of the
  supplied `HTTPClient` when it has one and otherwise to
  `DefaultSSEHandshakeTimeout` (30 seconds); a negative value removes the
  bound. The bound ends when the stream is open and says nothing about how
  long the stream lasts or idles. Migration: a server that takes longer than
  30 seconds to send response headers needs `HandshakeTimeout` set higher, or
  negative.

- **`SSEReader.Retry` is capped at an hour and accepts digits only.** A
  `retry:` field was parsed with `ParseInt` and multiplied into a duration, so
  a server sending `9223372036855` made `Retry()` negative, and a client that
  sleeps for it reconnected in a tight loop. A value with a sign, a space or
  an exponent is now ignored as the format requires, and a value beyond an
  hour is reported as an hour. Migration: none for a server that asks for a
  sensible delay.

- **Default log redaction covers more secret-bearing keys.** `DefaultRedactedKeys`
  now also matches `passphrase`, `pwd`, `dsn`, `connection-string`,
  `access-key`, `signing-key`, `master-key`, `encryption-key`, `hmac`, `totp`,
  `otp-code`, `basic-auth`, `auth-header` and `x-auth`, in any case and with
  any of `-`, `_`, `.` or space between the words. `auth`, `pass`, `sig` and
  `otp` are left out on purpose, since they sit inside `author`, `passenger`,
  `design` and `hot_path`. Migration: a key that these now hide and that is
  not a secret can be kept by passing `LoggerOptions.RedactKeys` without the
  term; a secret under a shorter name is added the same way.

- **A recovered panic's value is cut to 4 KiB in the log.** The value was
  logged whole, so a panic message built from a request's input made a log
  line as large as the input. It is now rendered with `fmt` and cut, with a
  `...(truncated)` marker, in both `Recovery` and the router's own recovery.
  The `panic` attribute is a string in the JSON format for every kind of
  value; it used to be the marshalled value for one that was not a string or
  an error.

- **A stream that cannot carry write deadlines logs a warning.** Middleware
  that wraps the response writer, forwards `Flush` and has no
  `Unwrap() http.ResponseWriter` hides the connection's deadlines, so the
  stream's `WriteTimeout` was silently not enforced and a client that stopped
  reading held its stream indefinitely. The stream is still served, because a
  writer supplied by a test cannot carry deadlines either, but the first one on
  each route now logs which writer stopped the chain. Migration: give the
  wrapper an `Unwrap` method.

- **The default rate limit tracker counts an IPv6 client by its /56, not its
  /64.** A /56 is what providers delegate to one home or small site (RFC 6177),
  so keying on the /64 still handed one subscriber 256 budgets: rotating the
  low bits of the fourth group of an address, all inside their own /56, turned a
  limit of 5 a minute into more than a thousand. The per-client connection caps
  already counted a /56 for exactly this reason; the rate limiter now uses the
  same prefix, and the same setting. `ClientIPOptions.ConnectionIPv6Prefix`
  (default 56) now also sets the prefix `IPTracker` counts, so one subscriber
  has one budget however it is limited. IPv4 is unchanged, and its keys are
  spelled as before.

  Migration: a counter kept in a shared storage under an IPv6 client's old
  `ip:2001:db8:1:2::/64` key is not carried over, so IPv6 clients start with a
  full budget once after the upgrade. Where many unrelated users share one /56
  (a campus, or a carrier that gives each device a /64), set
  `ClientIPOptions.ConnectionIPv6Prefix: 64` to keep the old grouping, which
  also loosens the connection caps to match; for one route only,
  `IPPrefixTracker(32, 64)` (any length from 0 to 128) still chooses its own.
  Where sites are delegated a /48, or the deployment is hostile to begin with,
  set 48 (anything from 32 to 128 is accepted).

- **A locale file that reaches one key two ways is refused.** A key that
  contains a dot (`"a.b": one`) next to a namespace that spells the same path
  out (`a: {b: two}`) is the key `a.b` twice, and which of the two a lookup
  returned was decided by the order Go ranged the map in, so the same file
  answered differently after each restart and one translator's entry could
  silently shadow another's. Loading it is now an error naming the key, whether
  the two are in one file or in two files loaded into the same locale, and the
  locale is left as it was. A locale tree that nests more than 100 levels (the
  depth the YAML reader already stops at) is refused as well; see below.
  Migration: write each key one way. Dotted keys that collide with nothing keep
  working.

- **YAML aliases are bounded.** The aliases of one document may copy at most
  `yaml.MaxAliasNodes` (100,000) values between them, and an alias adds the
  depth of the value it names to its own, so it counts against `MaxDepth`. A
  document that exceeds either is refused with a located syntax error. Sharing
  a block of defaults between locales, which is what aliases are for in a
  locale file, is far below either limit. Migration: none for a real file.

- **Strftime patterns, and `%<name>d` interpolation directives, are bounded.**
  A strftime width is at most 64, a pattern's nesting through `%c` and `%x` at
  most four levels, one expansion reads at most 16,384 characters and writes at
  most 16 KiB (cut at a character boundary). A `%<name>` directive with a
  width or precision above 64, or a `*` width, is left as literal text rather
  than rendered. Migration: none for a real pattern or message.

- **A directory that has a mount of its own is reachable only through that
  mount, under any name.** When a mount's directory lies inside another
  mount's, a request that reaches it through the outer mount is answered 404,
  not only when the path differs in case but whenever a directory on the way
  is the inner mount's directory: a symbolic link inside the outer directory
  that leads to it, or a mount whose path differs from its directory's name
  (a guarded mount at `/x` over `site/staff`, beside a public mount at `/`
  over `site`, no longer serves the same files at `/staff/...`). The fallback
  page is not offered in its place. A request the inner mount already answers
  is unaffected, and so is every mount with no mount beneath it. Migration:
  none for a layout where the inner mount is mounted at its own directory
  name; to publish the same files at two paths, mount them at both.

- **WebSocket keepalive is on by default.** `WSOptions.PingInterval` defaults
  to 30 seconds and `PongTimeout` to 10, so a peer that connects and goes
  silent, or whose network dropped without a word, no longer holds a
  connection slot for as long as it likes: only the per-IP and global caps
  bounded it before. A connection is closed for an unanswered ping only when
  its handler was reading during it, because a pong is consumed by a read like
  any other frame; a handler that only writes, or one busy with a message, is
  left alone, which also stops keepalive closing a healthy peer of a handler
  that does not read. Migration: none is needed for a peer that answers pings,
  as every browser does. A negative `PingInterval` turns keepalive off.

- **The default `MaxHeaderBytes` is 64 KiB, not 1 MiB.** A client that never
  finishes a header block holds all it has sent for as long as
  `ReadHeaderTimeout` allows, and with no cap on connections as a whole a
  megabyte each adds up. An ordinary request, with its cookies and a token,
  needs a small fraction of the new limit. Migration: set
  `ServerOptions.MaxHeaderBytes` for a deployment that sends larger headers.

- **The document lists a JSON body member as `required` only when a `Required()`
  rule refuses a body without it.** Every non-pointer member without `omitempty`
  was listed, but the decoder accepts a body that leaves any member out, so a
  client, or a gateway validating requests against the document, was told that a
  request the server accepts was malformed. A member of a request body is now
  required exactly when a `Required()` rule is declared for it (and it has no
  default, which is written before the rules run). This changes the emitted
  document for existing applications: a request schema that listed every member
  now lists only those with a rule, or none. A `Required()` on a pointer refuses
  `null` as well as absence, so the member is required and no longer described as
  nullable. The response side is unchanged, since a member that is not a pointer
  and not `omitempty` is always written. A type used both as a request and as a
  response therefore gets two schemas: the component keeps its shape for the
  response, and the request refers to a copy named for it with `Input` after
  (`Item` and `ItemInput`); a type only requests use keeps its name. Types nested
  inside a body keep their shape unless their own rules speak for them, and are
  never split. Form values keep the binder's own rule, to which a rule can only
  add. Migration: a client generator will now type an unruled request member as
  optional; declare `Required()` on the members that must be sent.

- **`format: duration` is gone from a `time.Duration`.** It is ISO 8601 (`PT1.5S`)
  in JSON Schema, while the binder and the JSON codec read `1500ms`, so a client
  that checks formats refused the value the document told it to send. The schema
  is a string with a `pattern` that agrees with `time.ParseDuration`, and the
  description is kept.

- **A `[]byte` query, header, cookie or form value is described as an array of
  integers from 0 to 255, not a base64 string.** The binder reads each value of a
  parameter as one element, so `?data=65` is `[]byte{65}` and `?data=aGk=` is a
  422. A `[]byte` in a JSON body is still base64. The runtime is unchanged.

- **`Unique()` is linear for every element type.** Elements that cannot be
  hashed as they are, pointers, interfaces, slices, maps, and structs holding
  them, were compared pair by pair, so twenty thousand of them, a few hundred
  kilobytes of JSON, cost minutes of CPU unless the rule carried a `MaxItems`
  bound the developer had to know to write. They are now keyed by a canonical
  encoding that is equal exactly when `reflect.DeepEqual` is, with the two
  exceptions `Unique` already made: a `time.Time` is the instant it names,
  wherever in an element it sits, and `-0` is `0`. A nil slice or map is still
  not an empty one, two pointers to equal values are a repeat, a map is the
  same map in any order, and a `NaN` equals nothing. A cyclic value is encoded
  once. Memory follows the size of the collection, so the body limit bounds it,
  and no build error or default cap is needed. `MaxItems` and `Items` still
  short-circuit a collection that is already too long. Migration: none.

- **`WSDial` refuses a 101 that names an extension.** The client never offers
  one, and RFC 6455 section 4.1 says a response that negotiates one it did not
  ask for fails the connection. It used to read on with frames whose meaning
  the server had changed. Migration: none for a server that does not negotiate
  extensions unasked.

- **Middleware installed with `App.Use` now runs inside the CORS policy, not
  outside it.** Until now the policy was the innermost layer, so an
  authenticating middleware refused every preflight (a browser never sends
  credentials on one) and its own 401 or 429 carried no
  `Access-Control-Allow-Origin`, which a browser reports as a network error
  rather than the status. The chain is now the built-in layers (request id,
  security headers, recovery, locale, access log), then CORS, then `Use`
  middleware. A preflight is answered by the policy and no longer reaches
  `Use` middleware; a response such a middleware writes itself carries the
  policy's headers for an allowed origin and none for a denied one.

  Migration: a `Use` middleware that relied on seeing an `OPTIONS` preflight
  (to log it, or to answer CORS itself) no longer does when
  `AppOptions.CORS` names origins; without a CORS policy nothing changes.

- **`RateLimitStorage.Increment` now runs under a timeout.**
  `RateLimitOptions.StorageTimeout` bounds each call, defaulting to
  `DefaultRateLimitStorageTimeout` (2 seconds); a negative value removes the
  bound. A store that stopped answering used to hold every request that reached
  it until its client gave up, and `FailOpen` could not help because no error
  ever came back. A call that outlives the bound has its context cancelled and
  counts as a storage failure: 503, or unmetered with `FailOpen`. The
  process-local storage is called without the deadline. A custom storage must
  return when its context ends, which any client library's context-taking call
  does.

- **A file larger than 8 MiB from a filesystem whose files cannot seek is
  streamed, not read whole into memory.** Smaller files are still buffered, so
  range and conditional requests keep working for them; a streamed one is sent
  with its size as `Content-Length` when known and `Last-Modified`, without
  range support.

- **A configuration setting declared where it cannot be loaded is an error.**
  A tagged unexported field, or an unexported embedded pointer whose type holds
  tagged fields, loaded nothing without a word, so a `required` setting was
  never enforced. `LoadConfig` now reports the field. Export it, embed the
  struct by value, or tag it `env:"-"` to opt out.

### Security

- **A WebSocket message can no longer slip past the read limit.** The limit
  check added the bytes already received to the length the next frame
  declared, so one byte followed by a continuation frame declaring
  `0x7FFFFFFFFFFFFFFF` wrapped negative and passed, and the peer could stream
  as much as it liked into one message. With the default configuration and one
  unauthenticated connection this could exhaust the server's memory. The check
  cannot overflow now, and such a frame is refused with 1009 from its header
  alone. `WSDial` connections were affected the same way and are fixed too.

- **Percent-encoding can no longer route a request around a static route's
  guards.** `GET /users/%61dmin` missed a guarded `/users/admin` and was
  answered by a public `/users/{id}` with `id` set to `admin`. Static segments
  are now compared percent-decoded, as `net/http.ServeMux` does, for prefixes,
  versions, HEAD, OPTIONS, 405, WebSocket and SSE routes alike. An encoded
  `%2F` still never matches a static segment.

- **A request can no longer set a located field from its body.** See the build
  error under **Changed**: a tag the binder silently ignored left the field to
  the JSON body.

- **`Unique` is linear for hashable elements, and defers to a count bound.**
  It compared every pair with `reflect.DeepEqual`, so a 228 KB body cost 24
  seconds of CPU, and `.Unique().MaxItems(100)` still paid it because rules
  ran in declaration order. Strings, numbers, booleans and arrays or structs
  of them now use a hash set, and a collection over a `MaxItems` or `Items`
  bound is never searched for repeats. Other element types keep the pairwise
  search and should carry a `MaxItems` bound.

- **A status coder error no longer sends its wrapped cause to the client.**
  `DefaultErrorRenderer` rendered the outermost `err.Error()` whenever a
  `StatusCoder` was anywhere in the chain, so context added with
  `fmt.Errorf("...: %w")` -- queries, connection strings, tokens -- reached the
  response, 5xx included.

- **A singleton can no longer be poisoned for the life of the process.** A
  provider that panicked once, or that failed because the first client
  disconnected and cancelled the request context, failed every later request
  until restart, which one aborted request could trigger.

- **A float parameter refuses `NaN` and infinity.** Query, path, header,
  cookie and form fields accepted `NaN`, `Inf` and `Infinity`, which pass
  comparisons such as `amount > balance`. They now fail as "must be a valid
  number". The 0.2.6 fix covered only `validate.Number`.

- **`IP` and `IPv6` refuse an address with a zone.** The zone is free text to
  the parser, so `fe80::1%` followed by CRLF and a header, or by markup,
  passed as a valid address.

- **One rate limit quota can no longer evict another's counters.** When the
  in-memory storage was full, eviction was global, so flooding one quota with
  new keys -- easily done from a single IPv6 /64 with the default tracker --
  evicted and so reset a per-account login limit. It now evicts from the quota
  holding the most counters.

- **Localized responses vary on the header or cookie that chose their
  language.** With `LocaleFromHeader` or `LocaleFromCookie` only
  `Vary: Accept-Language` was added, so a shared cache could be made to serve
  an attacker-chosen language to everyone. A `LocaleFromCustom` extractor
  should add its own `Vary`.

- **CORS adds `Vary: Origin` to every response under a named-origin policy.**
  It was added only when the origin was allowed, so a shared cache could serve
  the allowed origin a response without CORS headers. OPTIONS responses also
  vary on `Access-Control-Request-Method` and `Access-Control-Request-Headers`.

- **Every console log record is exactly one line.** The console format, the
  default when stderr is a terminal, wrote the message (which carries the
  decoded request path) and attribute values as they were, so `%0a` forged log
  lines and ESC drove the operator's terminal. Control characters, DEL, C1
  controls, U+2028, U+2029, bidi controls and invalid UTF-8 are now escaped.

- **A panic below `Compress` is answered with a 500 instead of an empty 200.**
  Its deferred finish sent the pending header while the panic unwound, so
  Recovery could no longer write its error.

- **A less specific mount no longer serves a path inside a more specific one
  by case.** On a case-insensitive filesystem `GET /ADMIN/secret.txt` was
  served by a public `Static("/")` instead of the guarded `/admin` mount. Such
  requests, and ones using `\` as a separator, now answer 404.

- **`Email` refuses invisible characters.** Runes in Unicode categories Cc and
  Cf and U+2028, U+2029 -- bidi overrides, zero-width characters, C1 controls
  -- were accepted. Internationalised addresses are still accepted.

- **`WSDial` connections apply `ReadTimeout` and `WriteTimeout`.** The
  transport `net/http` hands back is not a `net.Conn`, so no deadline was ever
  set, and a server that stopped reading or stalled mid-message held the
  client forever.

- **An env file parse error no longer repeats the offending line.** A malformed
  line, often part of an unquoted multi-line PEM key or a `KEY: value` typo,
  was quoted into the error. Errors now name the file, the line number and,
  when it looks like one, the key. A value read by any `secret:"true"` field is
  also hidden from every other field reading the same variable.

- **A path that is not valid UTF-8 gets its 404 instead of a 500.** The "no
  route matches" message quoted the decoded path, so `GET /%ff` made the error
  envelope impossible to encode. The path is quoted in its escaped form when
  it is not printable ASCII, and the method likewise in 404 and 405 messages.

- **`json:"-"` on a located field no longer disables its binding.** The header
  or query value, its default and its required check were all dropped; the
  tag now only keeps the field out of the JSON body and schema.

- **Validation work and output are bounded.** `Each`, or `Nested` called per
  element, reported every failing element, so a 1 MiB body produced a 22 MB
  422 and half a gigabyte of allocation; a recursive model nested 10,000 deep
  cost a 300 MB report and gigabytes of allocation, and the pooled validator
  kept the tree alive. See **Changed** for the limits.

- **An event stream never writes into a response it has handed back.** A send
  still encoding when the handler returned held the write lock; the stream
  gave up after `WriteTimeout` and returned the response anyway, which crashed
  the process or could write one client's event into the next request on the
  connection.

- **Static mounts no longer serve uploads as live pages.** An upload stored
  without an extension was sniffed as `text/html`, and an `.svg` was served
  inline, so client-written markup ran script on the API's origin.

- **An error response can no longer be served as HTML or cached publicly.**
  A handler that set `Content-Type: text/html` or a public `Cache-Control` and
  then failed sent its JSON error, reflected input included, under those
  headers.

- **The documentation counts against the application rate limit.** Once the
  docs ran the application's guards but not its rate limit, `/openapi.json`
  could be used to guess the application's token without limit.

- **A failing singleton no longer makes requests queue for it.** Each waiting
  request retried in turn under a lock and ignored its own cancellation, so a
  slow failure multiplied into queued latency and goroutines. One attempt is
  in flight at a time, its waiters share its outcome and leave when their own
  request ends, and only success is cached.

- **Guarded responses are kept out of shared caches.** Guarded routes and file
  mounts sent per-user JSON and files with no `Cache-Control`, so a CDN could
  hand one user's response to another.

- **A multipart form's temporary files are removed at the end of every
  request, whoever parsed it.** A guard or handler reading the form through
  `ctx.Request()` parsed it on a copy of the request that net/http's cleanup
  never saw, and every such upload stayed on disk.

- **A malformed form body is no longer written to the error log.** Its parse
  error, which quoted the bad part header whole, was logged at ERROR, so a
  1 MiB request wrote a 5 MB log line.

- **`Timezone()` no longer grows a cache from client input**, and an unknown
  name no longer costs a read of the zone database each time.

- **A path is quoted at most 1 KiB long.** The 404 and 405 messages and the
  access, error and panic logs cut a longer path or method, so a request of
  invalid bytes can no longer write several times its size to the client and
  the log store.

- **`SSEDial` no longer sends credentials to a redirect's target.** It sent
  custom headers such as `X-Api-Key` to another host, and `Authorization`
  over an https-to-http downgrade.

- **One IPv6 allocation can no longer take every WebSocket or SSE slot.** With
  the caps at /64, a home /56 held 256 separate allowances.

- **Log redaction covers groups.** A redacted key whose value was a group,
  through `slog.Group`, a `LogValuer` or `WithGroup`, was logged in full.

- **`secret:"true"` on an embedded config struct hides every field inside
  it.** It was ignored, so a malformed value appeared in the startup error.

- **A shutdown requested while the server is starting is honoured.** It
  returned nil and the server came up anyway.

- **A WebSocket handler's context ends with its connection.** A hijacked
  connection's context was cancelled only when the handler returned, so a
  handler waiting on it outlived its peer and the server's close at shutdown.

- **Responses that depend on a request header say so in `Vary`.** Header and
  media-type versioning now name the header (or `Accept`), and a frontend with
  an SPA fallback sends `Vary: Accept`, so a cache cannot serve one variant in
  place of another.

- **On Windows, a mount refuses a path segment shaped like an 8.3 short name**
  such as `/ENV~1`, which reached dotfiles and nested mounts by another name.

- **SSE keepalives and WebSocket pong checks use the monotonic clock**, so a
  wall-clock step cannot starve keepalives or close healthy connections.

- **A 103 Early Hints no longer swallows the response after it.**

### Fixed

- **A failure after a `Flush` that came before the first `Write` aborts the
  connection.** The implicit `200` a flush sends was not recorded, so a
  handler or middleware that flushed its headers early and then failed had the
  JSON error envelope appended to the stream as though it were the body, and
  `SetStatus` after the flush was silently accepted and logged as the status.
  The response is now treated as started from the flush, so the failure is
  logged once, the transfer fails for the client and the access log says
  `aborted`.

- **`Run` stops the lifecycle components when the server fails to serve.** A
  certificate that would not load, or an accept error that cannot be retried,
  returned the error with every component that had started before the socket
  opened still running. The components are now stopped as a shutdown stops
  them, and the listener is closed.

- **A second `SIGINT` or `SIGTERM` while `RunSignals` is draining ends the
  process.** `signal.NotifyContext` kept swallowing signals until `RunSignals`
  returned, so a second Ctrl-C during a drain of up to `ShutdownTimeout`, or
  without limit when it is negative, did nothing.

- **`TryFrom` and `From` accept a nil interface value from a provider.** A
  provider of an interface type returning `nil, nil`, the natural encoding of
  an anonymous caller, made every request a 500 because the stored `nil` failed
  the type assertion. It now returns the nil the provider produced.

- **A CORS policy that cannot be served is logged as a build failure.** It was
  recorded after the start-up log had reported the routes registered, so
  `ServeHTTP` answered every request with a bare 500 and the log named no
  reason. It is now reported with the other build errors.

- **An `App` that was shut down can be run again.** The WebSocket and event
  stream registries stayed in the draining state, so the second run answered
  every WebSocket and event stream with 503. They are reopened when the
  application starts running.

- **`Context.Context`'s documentation says when the context is cancelled.** It
  claimed the context is cancelled when the server begins shutting down, but
  `Shutdown` leaves an ordinary request to finish and cancels its context only
  with its connection at the deadline. The behaviour is unchanged; the
  documentation now says so and points long-lived responses at event streams
  and WebSocket routes, whose contexts do end when shutdown begins.

- **A lookup path that does not begin with `/` matches nothing.** The router
  dropped the first byte of one, so behind `http.StripPrefix("/api", app)` a
  request for `/apiXadmin/panel` was served as `/admin/panel`, and an
  `OPTIONS *` request matched a root wildcard. Guards still ran, but a
  firewall or cache keyed on the URL saw a different path from the one the
  application routed on.

- **The IPv6 zone is dropped from a client address, and a trusted proxy written
  as an IPv4-in-IPv6 CIDR is honoured.** A zoned address (`fe80::1%eth0`) was
  never inside a trusted prefix, so a trusted proxy that reported itself with a
  zone was taken for the client, and `Context.ClientIP()` and the per-client
  connection key at a full-length prefix were a different string for every
  zone suffix, so one client could mint any number of them. A trusted proxy
  written as `::ffff:10.0.0.0/104` parsed and then matched nothing, leaving the
  proxy it named untrusted without a message; it now means `10.0.0.0/8`.

- **`Unique()` over `[]time.Time` was quadratic.** `time.Time` holds a
  `*Location`, so it was not hashable and 16000 timestamps (368 KB) cost 12 s of
  CPU. Times are keyed by instant, and `netip` addresses by themselves.
  Element types that cannot be keyed (`[]*T`, `[]any`, nested slices) are still
  compared pair by pair and should carry a `MaxItems` bound.

- **`Matches()` and `MatchesNot()` recompiled their expression on every
  request.** Rules are declared inside `Validate`, which runs per request, so a
  slug pattern cost 83 microseconds and 491 allocations, twenty times the rest
  of the validation. Compiled expressions are kept by pattern text, up to 1024
  distinct patterns.

- **A negative `MaxBodySize` refused every JSON body.** It is documented to
  remove the limit, but `http.MaxBytesReader` clamps a negative limit to zero,
  and a `CaptureBody` route with `MaxBodySize(math.MaxInt64)` overflowed
  `limit+1` and read the body as empty. A limit that is not positive now leaves
  the body unwrapped, and one too large to add a byte to is unbounded.

- **`MultipleOf` used an absolute tolerance of 1e-9.** `19.99` failed
  `MultipleOf(0.01)`, `5.0000000001` passed `MultipleOf(5)`, and any value
  passed a factor below 1e-9. The quotient is now compared with its nearest
  whole number within a few units of float64 precision.

- **Registering a route on a self-referential collection type hung or
  overflowed the stack.** `type Tree map[string]Tree` as a body field looped in
  `locatedWithin`, `type List []List` as a query field overflowed `setterFor`,
  and both looped in the schema builder. Registration now terminates: a body
  field is accepted and described once, a parameter is refused with an error.

- **A `time.Duration` in a JSON body was a 422 for every value and a 500 in a
  response.** encoding/json/v2 has no representation for it and accepts no
  `format` tag. It is now a string such as `"1500ms"`, both ways, as the
  document says. SSE and WebSocket payloads are not covered.

- **OpenAPI: an embedded `*T` is described as a top-level, optional set of
  members** instead of a nullable member named for the type, matching what the
  decoder accepts.

- **OpenAPI: a generic type has a component key a `$ref` can resolve.**
  `Page[example.com/app.Item]` was used as the key as it stood, so its
  reference held a slash and brackets; it is now `Page_app.Item`.

- **OpenAPI: the rules of a nested model are documented.** `describeConstraints`
  returned before descending. The rules of every model that `Nested` reaches
  from a zero value are written onto the schema of its type; a model nested once
  per element of a collection is not among them.

- **A websocket connection now holds its slot until its socket is closed.**
  The slot was returned when the handler did, but closing waits for the peer's
  close frame (`CloseGracePeriod`) and, when another goroutine is writing to a
  peer that does not read, for the write half (`WriteTimeout`), so a peer that
  withheld its close frame held sockets and goroutines that `MaxConnections` and
  `MaxConnectionsPerIP` say nobody may: about 300 sockets against a limit of 4.
  Shutdown now also waits for connections that are closing.

- **A websocket's room is taken before the handshake is answered.** The
  connection was recorded after the 101 had told the peer it was connected, so
  a client dialling again as soon as it read the response could be admitted past
  the connection limits. Room is reserved under the register's lock before the
  upgrade.

- **A refused websocket handshake no longer runs the route's guards and
  dependencies first.** A cross-origin handshake, or one arriving at a full
  server, answered 403 or 503 only after the session dependency had run with the
  visitor's cookie, so a refused handshake still cost a store lookup and any
  side effect it has. A request that asks to upgrade is now judged for its
  origin and the connection limits first. A plain GET still reaches the guards
  first, so a client without credentials still gets 401.

- **A websocket's close status reaches the peer when two closers race.** A
  second `Close` (shutdown, a keepalive timeout, a returning handler) tore the
  transport down under the first one's close frame, so the peer saw 1006. It
  now waits for that frame. A peer's own close is also echoed when another
  goroutine is writing at that moment; it used to be skipped.

- **A close reason that is not UTF-8 is repaired before it is sent.** A reason an
  application forwarded from elsewhere went out as it was, obliging the peer to
  fail the connection instead of reading the status. Invalid bytes become U+FFFD.

- **An idle event stream over HTTP/2 was reset one `WriteTimeout` after its
  last event.** A stream armed a write deadline before every event and never
  took it off. On HTTP/2 that deadline is a timer that resets the stream when
  it fires, so with the default 10 second `WriteTimeout` and 15 second
  keepalive every idle stream died before its first keepalive, and browsers
  reconnected in a loop. A deadline now exists only while a write is in
  progress. On HTTP/1 the same change stops the bytes that end a response from
  meeting a deadline that had long expired, and a graceful shutdown or a
  handler's return now ends an idle stream cleanly rather than aborting it,
  since an interrupt is applied only to a write that is in flight.

- **A stream's deadline state raced with shutdown.** A stream is in the
  register before its header is written, so a shutdown could end it while it
  was still opening. `open` wrote the field recording whether the response
  carries deadlines without the lock that `interrupt` read it under, which
  the race detector reported, and the header was written with no deadline at
  all, so an interrupt landing in that window was lost. It is now decided under
  the lock and the header is written as an armed write that a shutdown can
  stop; a stream that has already ended never writes it.

- **JSON log output escapes C1 controls, DEL and bidirectional controls.** The
  JSON handler wrote U+0080 to U+009F, DEL and the bidirectional controls as
  themselves, so a decoded request path carrying a CSI reached a terminal
  reading the log with `tail`. The finished record is now escaped as `\uXXXX`,
  which a JSON reader decodes to the same text, covering messages, keys and
  values logged with `slog.Any`. The console format already did this.

- **A handler that sets `Vary` no longer erases the `Vary` the framework had
  added.** CORS (`Origin`, and the preflight request headers), the `Locale`
  middleware (`Accept-Language`, `Cookie` or the locale header, whichever
  sources are configured) and header or media-type versioning added their field
  before the handler ran, into a header the handler owns, so a handler doing
  ordinary content negotiation with `ctx.SetHeader("Vary", "Accept")` replaced
  it. The response then carried a reflected `Access-Control-Allow-Origin` or a
  `Content-Language` chosen from `Accept-Language` with nothing telling a
  shared cache what it depended on, and the cache stored it for every client.
  The fields are now recorded on the response writer and merged into `Vary`
  when the response is written (and when a response with no body is committed),
  as compression already did for `Accept-Encoding`; a handler's own `Vary`
  adds to them.

- **A locale whose strftime format names itself no longer kills the process.**
  `time.formats.default: "%c"`, or a date and a time format that name each
  other, recursed until the stack was exhausted, which Go reports as a fatal
  error that no `recover` catches, on the first request that localized a time in
  that locale rather than when the file was loaded. Expansion is now bounded, so
  the directive is written out as it stands past four levels.

- **A locale file can no longer amplify its own text without bound.** A strftime
  width was read as written, so `%500000000Y`, eleven bytes, made half a
  gigabyte; a hundred `%<n>1000000d` placeholders, twelve hundred bytes of
  translation, rendered ninety-five megabytes and left the pooled buffer holding
  that capacity; and a JSON locale nested to the decoder's own limit of ten
  thousand levels kept a dotted path of every level above each node, so a 54 KB
  file was retained as 85 MB. Each is now bounded (see above). A format name
  with no pattern under it is still used as the pattern by `Localize`, which
  made the width reachable from a value an application took from a client; the
  bound is what makes that safe, and the documentation now says so.

- **YAML alias bomb.** A 330-byte file of seven levels of aliases, each
  aliasing the one above it ten times, allocated 216 MB while loading and each
  further level cost ten times as much for 45 more bytes; a chain of 5,000
  single aliases parsed to a depth of 5,000 under a `MaxDepth` of 100 and copied
  the whole chain at every step.

- **`FailOpen` no longer discards a quota that had already said no.** A storage
  error on one quota of a policy returned from the check, so a request over the
  limit of an earlier quota was served, and a store that fails for some
  counters only (one shard of a cluster) stopped every other limit of the route
  from holding. The failing quota now goes unmetered and the others still
  decide; with none counted, no `RateLimit` headers are sent.

- **Quotas returned by a `QuotaResolver` are held to the rules static ones
  follow.** A resolved quota was checked for a name, a window and a limit, and
  nothing else. A name with a colon collides with the storage key of another
  quota (`plan:x` for client `y` is `plan` for client `x:y` in the key the
  storage documentation recommends), a NUL does the same in the in-process
  storage, and a name repeated within one policy, or repeating a static one,
  counted every request once per copy against one counter, so a limit of two
  refused the second request. Both paths now share one check, and a resolver
  that returns an invalid or repeated name fails the request with a 500, as a
  zero limit already did.

- **`Accept-Language` negotiation no longer costs more the more locales there
  are.** Every range of the header was compared with every available locale by
  lower-casing and splitting the locale again, so a 700-byte header against 300
  locales cost about 19,000 allocations and 750 microseconds per request, ahead
  of routing and of the rate limit. The lower-cased and split forms are
  computed once when the middleware is built.

- **A flush that failed is reported through the compression wrapper.** The
  wrapper had only `Flush()`, so `http.ResponseController` got nil whatever
  became of the bytes underneath and a stream could not tell the client had
  gone. It now has `FlushError`.

- **An event's `Retry` shorter than a millisecond is written as 1, not 0.** The
  field is whole milliseconds and the duration was truncated, so a positive
  delay under a millisecond told a client to reconnect at once.

- **A guarded mount was served, unguarded, through a folded spelling of its
  name.** The router refused a request that fell under a more specific mount
  once simple case was ignored (`/ADMIN`), but a filesystem that folds more
  than that, such as APFS, opened the same directory through a ligature (U+FB05 for the "st" of `/staff`), a sharp s (`/a` U+00DF
  `ets` for `/assets`), a combining accent (`/cafe` U+0301 for `/caf` U+00E9),
  decomposed Hangul, or `/STRASSE` for a mount at `/stra` U+00DF `e`. The outer mount served the files with
  none of the inner mount's guards, providers, rate limit,
  `Cache-Control: private` or, for a directory of uploads, the sandbox
  `Content-Security-Policy`, so uploaded HTML ran as the application. The
  outer mount now compares each directory a path passes through with the
  directory of every mount beneath it, by file identity, so the answer is
  exact on every platform and for every folding rule and no table is copied.
  Hard links and bind mounts are covered because only the directory's
  identity matters. Where both directories are given as `Dir` the relationship
  is worked out when the application is built; a mount served from an
  `os.DirFS` is compared against every mount beneath it, since its path cannot
  be read back, and one served from an `embed.FS` has no second name to defend
  against. A test that needs a folding volume checks for one at run time and
  skips on a case-sensitive one, and a model of such a filesystem runs the
  same logic everywhere.

- **A body that declares a length over the route's limit is refused before it is
  read.** A request that sent `Expect: 100-continue` with a `Content-Length`
  far over the limit was told to go ahead by the first read, and the 413 came
  only after the body had been read up to the limit. It is now refused from
  the declared length, for JSON, urlencoded and multipart bodies. A body that
  declares no length is still bounded as it is read.

- **A stalled socket write over HTTP/2 is bounded by `WriteTimeout`.** A write
  deadline on an HTTP/2 stream cannot interrupt a frame already being written,
  so a client that granted a large flow-control window and then stopped reading
  the socket kept every stream on its connection open past every timeout, SSE
  streams included, and a shutdown ran to its full timeout. The same client
  over HTTP/1 was cut in under a second. `http.HTTP2Config.WriteByteTimeout`
  now follows `ServerOptions.WriteTimeout`, so a connection whose socket
  accepts no bytes for that long is closed, and a disabled `WriteTimeout`
  disables it too.

- **A query parameter and a body member of the same name shared their rules.**
  The rules were collected and matched by name alone, so a body member's
  `Required`, `MaxLen` or `Pattern` landed on the query parameter (or header,
  cookie, form value) called the same, and the other way round, in the generated
  document. They are keyed by where the field is read from as well as by its
  name, in the collection, in the parameters, in the body schema and in nested
  models, and the same holds at runtime: a query value that failed to parse no
  longer hides the failure of a body member of the same name.

- **A `Required()` rule on a parameter, form value or body member that has a
  default made the document call it required.** The default is written before any
  rule runs, so leaving it out is not a failure.

- **A cancelled context could end the next websocket read.** `context.AfterFunc`
  does not wait for a callback that has already started when it is stopped, so
  a context cancelled at the moment its read finished had its callback run
  after the next read had cleared the deadline, and moved that read's deadline
  into the past: a healthy connection closed with 1008 for a message that
  "did not arrive". Stopping now waits for a callback in progress, and one that
  had not begun finds itself stopped. The same applies to writes and to a
  transport without deadlines.

- **The OpenAPI document lists what a websocket handshake or an event stream
  is refused with.** A websocket route documented 101, 426, 422 and the default
  but not the 400 for a malformed handshake, the 403 for a foreign origin (left
  out for a route that skips the check) or the 503 for a draining or full
  server; an event stream did not document the 503. A status a route declared
  with `WithResponseDoc` keeps its own description.

- **A failed start-up, or a run whose socket could not be opened, no longer
  waits forever on a lifecycle component's `Stop`.** Nothing bounded the release
  on those paths, so a `Stop` stuck on an unreachable broker held `Run` (or
  `StartLifecycle`) for good. It now uses `ServerOptions.ShutdownTimeout`, never
  less than one second. `StartLifecycle` called while another is still starting
  waits for it and reports its outcome instead of returning success early, and
  a `StopLifecycle` that overtakes a start makes it release what it had started
  rather than leave it running. The documentation now says that components are
  stopped in parallel, in no order, and must not need each other in `Stop`.

- **`Content-Digest`, `Repr-Digest`, `Digest`, `Content-MD5`, `Accept-Ranges`,
  `Trailer`, `Content-Location` and `Location` set by a handler that then
  failed no longer ride on the error response.** They describe the body that
  was never sent. `Location` is kept when the error itself answers with a 3xx.

- **An env file value can be followed by a comment after its closing quote.**
  `KEY="value" # note` (and the single-quoted form) was refused as an unclosed
  quote; it now reads `value`. A comment needs whitespace before it, and a value
  that ends in the quote it opened with reads as before.

- **Rate limit seconds and counts no longer wrap.** A window near 292 years made
  `RateLimit-Reset` negative and `Retry-After` 1; seconds now clamp to what a
  32-bit int holds. The in-memory storage's count saturates at the top of an
  `int` instead of wrapping under every limit.

- **A pooled route `Params` no longer keeps the captured path.** `Reset` and a
  rewound branch left the request path's substrings in the backing array until
  a later request overwrote the slot.

- **An `Accept-Language` or `Accept-Encoding` quality is read only between 0
  and 1.** `q=NaN`, `q=Inf` and `q=1e9` parsed as valid, so an infinity
  outranked every honest range and NaN made the ordering meaningless. NaN is
  unreadable (one, as before), anything above one is one, and a negative value
  is a refusal.

- **A translation of a framework message that cannot render falls back to
  English.** A translation naming a value the call site never passes came back
  as the empty string, so the summary, an issue or a status message went out
  blank.

- **An empty `ValidationError` has a message.** `Error()` indexed its first
  detail and panicked for one an application built with none, inside the code
  that reports the error.

### Documentation

- **The symbolic-link guarantee holds for `Dir` and for the `FS` of an
  `os.Root`, not for `os.DirFS`.** `os.DirFS` follows a link out of its
  directory, so a link planted in a directory served through it serves the
  file it names. The `Static`, `Frontend` and package documentation now say
  so and point at `Dir` and `os.OpenRoot(...).FS()`.

- **OPTIONS and 405 answers skip a route's guards and rate limit, and the
  documentation now says what that discloses.** They are answered from the
  route table alone, as `net/http.ServeMux` does, so an unauthenticated client
  learns which paths exist and which methods they take, and never a route's
  data. Middleware installed with `App.Use` still covers them; put a guard or
  a limit there when the existence of a path is itself a secret.

- **A decoded path parameter can contain `/`, `..` and NUL.** A wildcard value
  is percent-decoded once, so `/files/a%2F..%2Fb` binds `a/../b`. The route
  documentation says to validate one before using it as a file name.

- An event stream has no maximum lifetime: the write timeout bounds one write,
  the keepalive bounds silence, and a client that keeps reading holds its
  stream and its slot until the handler returns. `SSEStream` now says so, and
  how a handler sets a ceiling of its own.

- A symbolic link or hard link inside a served directory is followed and served
  by the mount that owns the directory; only a link that leaves the directory is
  refused. A deployment step that links files into a served directory (`cp -l`,
  `rsync -H`, a build cache) therefore publishes them under the new name,
  dotfiles included, unless the link leads into the directory of a mount
  beneath.

- `SSEStream` and `SSEOptions.WriteTimeout` now say what bounds a stream's
  age (`MaxLifetime`) and why a client that reads nothing is not caught by the
  write timeout. `WSOptions.AllowedOrigins` says what the same-origin rule
  trusts.
- The residual "a `Unique()` on an element type that cannot be keyed still
  needs a `MaxItems` bound" in the changelog's known limits and in
  `docs/security-review.md` no longer holds and can be dropped.

- `Guard`: every guard on a route runs before any provider, whatever order they
  were declared in, so a guard cannot read a `Needs` value; a check that needs
  the caller's identity belongs in the provider.
- `Quota`: a window is fixed, so up to twice `Limit` can be spent across a
  boundary; a short quota in the same policy bounds that.

### Known limits

The latest review found these and left them, each with the reason.

- The rules that have no JSON Schema keyword (`Prefix`, `Suffix`, `Contains`,
  `NotBlank`, `MinBytes`, the date rules and others) are not described in the
  document, which therefore says less than the server enforces there, not
  more. A member of a JSON body that is not a pointer still accepts `null`,
  which the decoder reads as the zero value, and the schema does not list
  `null` for it. `Matches()` writes its Go (RE2) pattern into the document as
  it stands.
- `ReadHeaderTimeout` does not apply to HTTP/2: `net/http` reads an HTTP/2
  header block under the connection's own timeouts, so a client that never
  finishes one is held until `IdleTimeout` (2 minutes by default), not for 5
  seconds. `ServerOptions.WriteTimeout` says so.
- An event-stream client that never reads is bounded by its keepalive writes
  filling the socket, and by nothing else, unless `SSEOptions.MaxLifetime` is
  set, which it is not by default. `SSEStream` says so.
- An HTTP/2 GET to a WebSocket route runs the route's guards before it is
  answered 426, as an HTTP/1 GET without an upgrade does, so that a client
  without credentials is told to authenticate first.

### Investigated, not reproduced

- `SSEReader` read-timeout timer racing a returned `Next`: the timer is stopped
  when the event completes and none runs between events; 150 events straddling
  the timeout never lost a stream. An event that finishes in the same instant
  its timer fires is delivered and the stream ends on the next call, which is
  what an event that took the whole timeout is due.
- A zoned IPv6 address (`%a`, `%b`, `%eth0`) as its own client under
  `ConnectionIPv6Prefix: 128`: the zone is dropped before a key is made.
- Keepalive against a handler that reads slowly: pongs wait in the socket for
  the next read and an unanswered ping is judged only while a read is pending.
- A route's `InsecureSkipOriginCheck` cannot be switched off by a narrower
  scope, and an application's `AllowOriginFunc` stays in force for a route that
  only lists origins: this is the field-by-field layering the options document,
  pinned by a test.

## [0.2.7] - 2026-09-03

### Fixed

- **A rule bound to a value rather than to a field is refused when the route is
  compiled.** A rule set is matched to its field by that field's address, so
  `v.String(&in.Name)` is matched and `v.String(in.Name)` -- the pointer the
  field holds rather than the field -- is not. Both compile, because
  `StringField` and `NumberField` admit the pointer type so that one entry point
  can serve `Name string` and `Nickname *string` alike.

  The wrong one failed in the two ways that look most like working. The request
  was still refused, and the detail carried an empty `field`, so a client was
  told that something was wrong and not what. And the rule contributed nothing
  to the generated document, because the description step skips a rule it
  cannot name -- so the published schema quietly lost a constraint the code was
  still enforcing.

  Neither shows up in a test that asserts a status: a 422 with a nameless detail
  is still a 422. In the service this was found in it had gone unnoticed across
  nineteen call sites and five models -- every optional field on five resources.

  ```
  muzak: PATCH /orgs/{org_id}: a String rule is bound to a value rather than to
  a field of schemas.UpdateOrgIn, so its failures would name no field and its
  constraints would be missing from the generated document; pass the field's
  address (v.Rule(&in.Field), not v.Rule(in.Field)), or name it with As()
  ```

  The check runs once, against a zero value, when the route is compiled --
  declaring a rule set only records it, which is what makes that safe. A rule
  named with `As()` is left alone: binding outside the model is then deliberate
  and the name it reports under is the one you chose.

  **A rule declared only under a condition is not seen**, since the zero-value
  pass does not take that branch. That residue is smaller than it looks: the
  condition is usually a nil check that is itself redundant, because a nil
  pointer field already skips its rules.


## [0.2.6] - 2026-09-03

### Changed

- **A numeric rule whose bounds exclude zero now applies to zero.** Previously
  every rule on a number was skipped when the value was nought, so

  ```go
  v.Number(&in.ClickWindowHours).Between(1, 720)
  ```

  accepted `0` -- a rule set saying in as many words that zero is out of range,
  quietly letting it through. The request reached the database, died on a CHECK
  constraint, and the caller got a `500` where the whole point of the rule was
  to produce a `422`.

  "Optional means optional" reads an empty value as an absent one. For a string
  that is fair: a field nobody filled in arrives as `""`. For a number it is a
  guess, because zero is a value people mean, and the guess was wrong in the
  direction that fails open. It also made the generated document lie -- the
  OpenAPI said `minimum: 18` while the server accepted nought, so a client
  reading the contract and a client testing the server learned different rules.

  The skip now applies only where zero would have passed anyway. Nothing
  changes for `Max(10)`, `Between(0, 10)`, `NonNegative()`, `MultipleOf(5)` or
  a bare `Must`, and a rule of your own is never evaluated speculatively, so a
  field guarded only by `Must` behaves exactly as before.

  **Migration.** A numeric field that may legitimately be *absent* and whose
  bounds exclude zero is a pointer, which is what it always should have been --
  the same rule that already applied to a field which may legitimately *be*
  zero:

  ```go
  // before: optional, and silently unvalidated when omitted
  Age int `json:"age"`
  v.Number(&in.Age).Between(18, 120)

  // after: optional, and validated when supplied
  Age *int `json:"age,omitzero"`
  v.Number(&in.Age).Between(18, 120)
  ```

  A field that was always meant to be mandatory needs nothing: it now fails
  with its own words (`must be between 1 and 720`) instead of passing.
  `Required()` still means what it did and is still the way to demand presence
  when the bounds themselves admit zero.


## [0.2.5] - 2026-09-02

### Added

- **`RouteFromContext` names the route a request matched, so a service can be
  instrumented from outside.** Middleware is the seam for tracing and metrics,
  and it runs below the typed layer and before routing, so it never learned
  which route matched. All it had was `r.URL.Path` -- the concrete path.

  For a log that is right; you want the path that was requested. For a span
  name or a metric label it is fatal. `GET /orgs/01a0.../apps/01a0.../keys` is
  one time series per organization per app, which is the classic way to take a
  tracing backend down, and it makes the only question worth asking -- how slow
  is this endpoint -- unanswerable, because every request is its own endpoint.

  ```go
  func Tracing(next http.Handler) http.Handler {
      return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          next.ServeHTTP(w, r)
          if route, ok := muzak.RouteFromContext(r.Context()); ok {
              // "/orgs/{org_id}/apps/{app_id}/keys"
          }
      })
  }
  ```

  Read it **after** `next.ServeHTTP` returns; before that, routing has not
  happened and it reports false. A request that matched nothing also reports
  false, so a 404 is counted as a 404 rather than as traffic to a route named
  `""`.

  The framework takes no dependency for this and should not: it has none at
  all, and the OpenTelemetry API alone is ten modules, its SDK fifteen, and an
  OTLP exporter eighty-eight. Instrumentation belongs beside the framework, and
  this is the one fact it could not get from outside.

- **`StatusRecorder`**, implemented by the response writer the access log
  installs, so middleware can read a response's status without wrapping the
  writer a second time. The assertion can fail -- `DisableAccessLog` means no
  wrapper -- so middleware that needs the status either way should still fall
  back to wrapping.

### Changed

- **The access log records `route` beside the path.** The concrete path stays
  in the message, because that is what somebody reading a log wants; the
  template is the field a query groups by, and the one that joins a log line to
  the trace and the metric for the same endpoint.

## [0.2.4] - 2026-09-02

### Fixed

- **A location tag is read the way `encoding/json` reads its own, cut at the
  first comma.** `query:"limit,omitzero"` registered a parameter literally named
  `limit,omitzero`, because the tag's whole value was taken as the name.

  Options have never meant anything in a location tag -- `default` and
  `required` are tags of their own -- so this was a footgun with no upside: a
  tag that looks like a json tag gets written like one eventually.

  It failed in the worst way available, which is why it went unnoticed. A
  parameter nothing can send is not an error; it is a filter that silently does
  not filter, and the endpoint answers the unfiltered question with `200`. In
  the service it was found in, it had disabled twenty-five parameters across
  eight endpoints -- every environment and platform filter, every granularity,
  every row limit and every percentile -- each with a plausible default sitting
  behind it, so every response looked reasonable.

### Added

- **`CaptureBody` keeps the request body as it arrived, read back with
  `Context.RawBody`.** A request whose authentication covers its bytes could not
  be verified: the binder reads the body to decode the input, a route declared
  with `Empty` drains it, and a handler found nothing left to hash. The only way
  through was middleware that read the body and put an identical reader back,
  which every user had to discover for themselves, and the failure while they
  were looking was a signature that never matched for reasons that look like a
  bug in the client's signing code.

  ```go
  r.Post("/webhooks/stripe", handlers.Stripe,
      muzak.CaptureBody(),
      muzak.Needs(core.VerifyStripeSignature))

  body, ok := ctx.RawBody()
  ```

  It is a shared option, so a router of webhook receivers declares it once.
  Capture happens **before the dependencies resolve**, so a guard verifying a
  signature refuses an unsigned request before anything decodes it, and **after
  the rate limit is counted**, so a client past its budget never makes the
  server buffer on its behalf. It works for a route whose input is `Empty` and
  for one that binds a multipart form, neither of which reaches the JSON body
  path at all.

  The body is bounded by the route's existing `MaxBodySize` rather than a second
  limit, and one over it is refused with `413` rather than truncated: a
  truncated body fails its signature check, and a signature failure reads as an
  attack rather than as the oversized request it is.

  `RawBody` returns `([]byte, bool)`. The second value is not decoration -- a
  helper shared between a route that captures and one that does not would
  otherwise verify a signature against an empty body and pass. The bytes are
  released with the pooled `Context`, so anything outliving the request copies
  them.

  This is not a niche case. Stripe, GitHub, Slack and Apple's SKAdNetwork
  postbacks all sign the raw body.

## [0.2.3] - 2026-09-02

### Added

- **`RateLimitOptions.Resolver` supplies quotas per request.** A `Quota` is
  fixed when the application is built and one name may not carry two policies,
  which is right for a policy the application owns and cannot express one its
  customers do: a plan, a negotiated ceiling, a tier read from a database.

  ```go
  muzak.WithRateLimit(muzak.RateLimitOptions{
      AfterDependencies: true,
      Resolver: func(ctx *muzak.Context) ([]muzak.Quota, error) {
          plan, ok := muzak.TryFrom[Plan](ctx)
          if !ok {
              return nil, nil // nothing resolved, nothing enforced
          }
          return plan.Quotas, nil
      },
  })
  ```

  Resolved quotas are counted alongside any static ones, so a route can carry
  both a floor everyone shares and a ceiling that varies, and `RateLimit-Policy`
  describes the union. Setting a resolver turns limiting on even with no static
  quota, because what is enforced becomes a run time question. Returning nothing
  enforces nothing, which is what an unauthenticated request reaching a
  plan-based route should do.

  A quota that arrives at run time cannot be validated when the application is
  built, so one with no name, no window or no limit fails the request rather
  than being counted under an empty name. Nothing can check a resolver's names
  against the statically declared ones either, which is the price of the
  flexibility.

### Changed

- **A panic in a lifecycle component's `Start` or `Stop` becomes an error
  instead of killing the process.** `Stop` runs after the drain, on the
  framework's goroutine, when every request has already been answered and there
  is nothing left to report a crash to. A partially constructed component
  panicking there took the process down at the one moment where doing so
  achieves nothing, and the stack pointed at the framework rather than at the
  application that supplied the value.

  A handler that panics already becomes a 500 rather than a dead process; this
  is the same bargain for the same reason. The panic and its stack are returned
  as the component's error, so a failed stop is reported the way any other
  failed stop is, and the components either side of it are still released.

  No migration. A component that never panicked behaves exactly as before.

### Documentation

- `DefaultShutdownTimeout` names the grace periods of the platforms it has to
  sit below. The default of 15s is longer than Cloud Run's ten, so the platform
  killed a drain that was still running and nothing said why.

- `LogFormatAuto` says what it does in a container. A container's stdout is not
  a terminal, so a service under Compose gets JSON even locally, which is right
  for production and a surprise in front of `docker compose logs`.

## [0.2.2] - 2026-08-25

### Added

- `validate.TimezoneDataAvailable` reports whether a binary can resolve time
  zone names at all.

  `Timezone()` resolves against the zone database, and a Go binary carries one
  only when something imports `time/tzdata`. An image built from scratch has no
  `/usr/share/zoneinfo` either, so the rule refuses every value including the
  correct ones and nothing says why. Call this where the application is built
  and refuse to start without it, so the fault is a failed deploy rather than a
  week of rejected requests.

### Changed

- **The locales a lookup walks are worked out once rather than per lookup.** A
  chain was rebuilt on every translated message, costing a slice, a map and a
  closure, which on a rejected request is once per field. They are settled while
  the application is being built, alongside the plural rules, and only read
  afterwards.

  A message with nothing to interpolate now allocates nothing: 50ns and no
  allocations, against 91ns and one before.

  `FallbacksFor` returns a copy, since the chains are now shared by every
  request and handing one out directly would let a caller reorder what the whole
  process resolves against.

## [0.2.1] - 2026-08-25

### Added

- **Internationalization, with the framework's own messages included.** A
  request's locale is resolved once, before the handler runs, and every string
  in the response is written in it: the wording of each validation rule, what
  the request binder says about a value it could not read, and the sentence
  behind each HTTP status.

  ```go
  //go:embed locales
  var locales embed.FS

  app := muzak.New(muzak.AppOptions{
      I18n: muzak.I18nOptions{Store: i18n.MustLoad(locales, "locales")},
  })
  ```

  A handler translates through its context, which already knows the locale:

  ```go
  func greet(ctx *muzak.Context, in Params) (Out, error) {
      return Out{
          Greeting: ctx.T("greeting.hello", "name", in.Name),
          Items:    ctx.T("greeting.items", "count", in.Items),
          Today:    ctx.L(time.Now(), "as", "date"),
      }, nil
  }
  ```

  A locale is passed rather than set. Go has no per-goroutine storage, and a
  package-level current locale shared by every goroutine would leak one
  request's language into another: one request setting it, and another two
  microseconds later reading it. So it is resolved once per request, carried on
  the request's context, and named explicitly anywhere there is no request.

  Where it comes from is declared rather than written by hand. `Accept-Language`
  is negotiated by default, honouring the quality values the client sent, and
  `LocaleFromPath`, `LocaleFromQuery`, `LocaleFromHeader`, `LocaleFromCookie`
  and `LocaleFromCustom` cover the rest in whatever order `Sources` lists them.
  Nothing a request carries is used unless it matches a locale the application
  declared, so what reaches a filesystem path or a response header is always one
  of the application's own strings.

  Leaving `AppOptions.I18n` unset is the feature turned off: no middleware is
  installed, every message reads exactly as it did before, and nothing in
  `muzak.dev/framework/i18n` is linked into the binary. The framework reaches a
  translation engine only through the `Translator` interface, which is what
  keeps a service that answers in one language from carrying the machinery for
  ninety.

- `muzak.dev/framework/i18n` is the engine: `Store`, `Backend`, `Simple`,
  `Chain`, YAML and JSON locale files, `%{name}` interpolation, CLDR plural
  rules for around ninety languages, locale fallbacks, default chains,
  pluggable exception handlers, and `Localize` with a strftime formatter and the
  number helpers.

  Go's own time formatting cannot produce a localized month name, because the
  reference layout hard-codes `January`. Patterns are therefore strftime, which
  names the field and so leaves the formatter free to fill it from the locale.
  It is also the notation the published locale corpora are written in, so a file
  taken from one loads unchanged. A pattern beginning `go:` is handed to Go's
  formatter instead, for the formats meant to be read by a machine.

- The locale the framework ships covers every string it produces, and is chained
  beneath an application's own translations. A locale file names only what a
  service adds and the rules it wants worded differently; everything else falls
  through. Failures are looked up under four scopes, narrowest first, so one
  rule on one field of one model can be phrased without restating any other:

  ```yaml
  es:
    errors:
      messages:
        blank: "es obligatorio"
      models:
        create_item:
          attributes:
            name:
              blank: "cada articulo necesita un nombre"
  ```

- `Context.T`, `Context.L` and `Context.Locale` translate, localize and report
  the locale of the request in hand. `LocaleFromContext` reads the same value
  where there is a `context.Context` but no Muzak one, such as in a repository
  or a goroutine started from a handler.

- **Forty three more validation rules.** The engine had twenty string rules,
  eight numeric, six for collections and five for times. What it was missing was
  not exotic: a way to say https rather than any URL, an address of any kind, an
  exclusive numeric bound, and a check that a field of spaces is not a value.

  URLs and addresses, all narrower than the `URL` that was already there:

  ```go
  HTTPS()  URLWithSchemes(...)  Host()
  IP()  IPv4()  IPv6()  CIDR()  MAC()
  ```

  What a value may hold at all, and the character classes:

  ```go
  MatchesNot(pattern)  NotBlank()  NoControl()
  Alpha()  Alphanumeric()  Numeric()  ASCII()
  ```

  Formats that parse rather than merely match:

  ```go
  Slug()  Hex()  HexColour()  Base64()  JSON()  Semver()
  E164()  LanguageTag()  Timezone()  CountryCode()  CurrencyCode()
  ```

  Comparisons, the last two counting bytes rather than characters:

  ```go
  EqualFold(other)  MinBytes(n)  MaxBytes(n)
  ```

  Numbers, collections and times:

  ```go
  GreaterThan(n)  LessThan(n)  NonNegative()  NonPositive()
  Whole()  Port()  OneOf(...)

  Items(n)  NotEmpty()  Contains(v)  Excludes(v)

  Past()  Future()  Within(d)
  ```

  Each carries the name of the rule behind it, so each is translated the way
  every rule that came before it is, and each describes itself in the generated
  document wherever JSON Schema has a way to say it: a format for the addresses,
  an expression for the character classes, bounds for the numbers and item
  counts for the collections.

  Three of these close holes rather than add conveniences. `GreaterThan` and
  `LessThan` are the exclusive numeric bounds, which had no equivalent at all:
  only the inclusive pair and the comparisons against zero existed, so a price
  that must be above nothing could not be stated. `NotBlank` rejects a field of
  spaces, which satisfies `Required` while carrying nothing anyone would call a
  value. `NoControl` rejects a carriage return in something bound for a header,
  a log line or a redirect, which is where an injection starts.

  `NotBlank` and `NotEmpty` run even when a field was not supplied, unlike every
  other rule, because an absent value is blank and an absent collection is
  empty. That makes each a stronger presence check rather than something to pair
  `Required` with. The skip every other rule follows is now a named predicate,
  so the exception is stated in one place rather than implied by a comparison.

  `CountryCode` and `CurrencyCode` look the value up in the assigned registers
  rather than merely counting letters, so `XQ` is refused although it is two
  upper case letters. Both tables carry the date they were taken, and both need
  regenerating when the standards change: countries are added and withdrawn, and
  currencies are redenominated.

  The generated document describes the shape rather than the table. Two hundred
  values in every schema that names a country would be noise rather than
  documentation. Use `OneOf` with your own list when a service trades in a known
  handful, which is narrower and self-documenting.

  `Alpha` and `Alphanumeric` judge letters as Unicode letters rather than as the
  twenty six of English, so a name in any script passes. Narrow it by composing:
  `Alpha().ASCII()`.

- `validate.Kind` names the rule behind a failure, and `Problem` carries it
  alongside the English it has always produced. `MessageKey` on every rule set,
  and `Validation.RejectKey`, name a translation for an override rather than
  fixing its wording in one language. `HTTPError.WithMessageKey` does the same
  for an error an application raises itself.

- Responses report `Content-Language`, and add `Vary: Accept-Language` when that
  header took part in choosing the locale, so a cache in front of the service
  cannot serve one language to a client that asked for another.

### Changed

- `muzak.ErrorDetail`, `muzak.AppOptions`, `muzak.HTTPError`,
  `muzak.ValidationError` and `validate.Problem` each gained fields. The new
  members of `ErrorDetail` are tagged `json:"-"`, so neither the error envelope
  nor the generated OpenAPI document changes.

  Migration: a keyed composite literal is unaffected. An unkeyed one, such as
  `muzak.ErrorDetail{"limit", "query", "is required"}`, no longer compiles; add
  the field names.

- The default middleware chain gains one entry, between panic recovery and the
  access log, when a translation store is configured. It is not installed
  otherwise. The access log records the resolved locale when there is one.

- `validate.Positive` and `validate.Negative` report their own rules rather than
  the general numeric bounds, because their wording names zero rather than
  interpolating it. Their English is unchanged.

- **`Positive` and `Negative` now describe an exclusive bound.** Both reject
  zero, and both were generating `minimum: 0` and `maximum: 0`, which tell a
  client that zero is allowed. They now generate `exclusiveMinimum` and
  `exclusiveMaximum`, and `validate.Constraints` and `muzak.Schema` gained the
  two fields to carry them.

  Migration: none in code. A generated document changes, and it changes to
  describe what the rules have always enforced.


## [0.2.0] - 2026-08-24

### Changed

- **The documentation page is no longer part of the framework.** The small
  self-contained page that was embedded in the module is gone, and
  `AppOptions.DocsUI` names the UI to serve instead. It is nil by default, so
  an application publishes its OpenAPI document at `/openapi.json` and serves
  no page at all unless it asks for one:

  ```go
  import "muzak.dev/openapi/ui"

  app := muzak.New(muzak.AppOptions{DocsUI: ui.Files()})
  ```

  A UI is a module of its own so that a service which does not want one does
  not carry it: Go downloads and links a module only when something imports it,
  so leaving `DocsUI` unset costs a binary nothing rather than embedding a page
  it will never serve. Nothing is fetched at run time either way.

  Migration: add the import and the option to keep a page at `/docs`; change
  nothing to keep only the document. `DocsPath` is no longer reserved when no
  UI is configured, so a route of your own may use it.

### Added

- `muzak.dev/openapi/ui` serves the OpenAPI dashboard: the reference grouped by
  tag, every schema as an outline, request snippets in thirteen languages, and
  a console that sends a request from the page and reports the status, the
  timing, the headers and the body. It is built from the same document the
  framework generates, so tags, summaries, descriptions, deprecations,
  parameters with their validation constraints, request bodies and the response
  model declared for every status code all reach it.
- `AppOptions.DocsUI` takes any `fs.FS` meeting a small contract - an
  `index.html` whose absolute URLs are written under `/__muzak_docs__/` and
  which reads its document from `/__muzak_spec__` - so a service can serve a
  dashboard of its own instead.

## [0.1.1] - 2026-08-24

### Added

- `WithResponseModel[T](code, description)` documents a status code and the
  model its body carries, so one operation can describe a different schema per
  status code rather than the error envelope everywhere: a `400` and a `500`
  carrying the service's own error type, a `409` carrying a conflict report, a
  `304` carrying nothing. The type argument is written exactly as a handler's
  `Out` type is, and is described once in the components section. Like
  `WithResponseDoc` it is a router option as well as a route option, and the
  last declaration of a status code wins, so a route replaces what it
  inherited.
- An empty description passed to `WithResponseDoc` or `WithResponseModel` now
  falls back to the status code's standard reason phrase, so
  `WithResponseDoc(404, "")` is documented as "Not Found" rather than as a
  response with no description at all.
- A documented response status outside 100-599 is a build error naming the
  route and the code, rather than a response key in the document that no client
  could ever receive.

- A request console in the documentation page at `/docs`. Every operation can
  be sent from the page itself, with the parameters, the JSON body and the
  multipart form filled in beside the schema they come from; the response is
  shown with its status, timing, size, headers and body, an event stream is
  read as it arrives, and the same request can be copied as a `curl` command.
  An **Authorize** panel adds a bearer token, an API key header or basic
  credentials to what the console sends. They are held in the tab and never
  stored.
- The rest of the page grew with it: operations grouped by tag with a
  description per group, schemas as expandable outlines carrying the
  constraints the application enforces, generated examples, a filter over every
  operation, deep links to an operation or a group, and a light, dark or
  system theme.
- A constructor for each HTTP outcome worth a name: `muzak.NotFound(message)`,
  `muzak.Forbidden(message)`, `muzak.Conflict(message)` and seventeen more,
  covering 400 through 504. Each returns an `*HTTPError` carrying that status
  and its classifier, so `Wrap`, `WithCode` and `WithDetails` chain onto every
  one of them, and an empty message uses the standard sentence for the status.
  `NewHTTPError(status, message)` still covers anything without a name of its
  own.
- Machine-readable codes for the statuses that had none: `payment_required`,
  `not_acceptable`, `request_timeout`, `gone`, `precondition_failed`,
  `not_implemented`, `bad_gateway`, `service_unavailable` and
  `gateway_timeout`.
- The address the documentation ended up at is reported when the server starts
  listening, as a URL that can be opened from the terminal:
  `Documentation at http://localhost:8080/docs`. A wildcard bind is reported as
  localhost, since that is where a browser can reach it.
- `AppOptions.DocsPath` and `AppOptions.OpenAPIPath` are now validated while
  the application is built. A path that is not absolute, one that is the same
  as the other, or one that an application route already answers is a build
  error naming the option to change, rather than a page nobody can reach or a
  route silently shadowed by the documentation.
- `OpenAPIOptions.Tags` describes the groups operations are sorted into and
  decides the order the documentation presents them in. Routes join a group
  with `WithTags` as before; a described tag no route carries is left out, and
  a tag nothing describes follows the described ones.

### Changed

- `CodeForStatus` returns a specific classifier for the nine statuses listed
  above instead of the generic `client_error` or `internal_error`. A client
  switching on the code sees the more precise value; one switching on the
  status is unaffected.
- The documentation page and the OpenAPI document are compressed once when the
  application is built and served with an entity tag per representation, so a
  client that accepts gzip transfers a fraction of the bytes and a reload
  transfers none.
- The page's content security policy now names the page's own script and
  stylesheet by hash instead of by a nonce issued per response. The page is a
  constant again, which is what lets it be cached, revalidated and compressed
  ahead of time; `Cache-Control` is `no-cache` rather than `no-store`.

## [0.1.0] - 2026-08-23

First public release, published as `muzak.dev/framework` on the Go module proxy.

This is a pre-1.0 version. The surface is covered by tests and used by the
example application, but it is not frozen: expect it to move before 1.0.0.

### Added

- Routing on a segment-wise radix trie, with static and parameter segments and
  routers that nest under a prefix.
- Handlers of the form `func(ctx *muzak.Context, in In) (Out, error)`, where the
  input type is the request and the return type is the response. Neither type
  argument is written at the call site.
- Request binding by struct tag from `path`, `query`, `header`, `cookie`, `form`
  and `file`, or from the JSON body when no tag is present, with the binding
  plan compiled once per route.
- Validation declared against the field address rather than its name, so
  `v.String(&in.Email)` survives a rename and `v.Number(&in.Email)` does not
  compile.
- Dependencies: guards through `WithDependencies` and typed providers read back
  with `From[T](ctx)`, plus `WithSingleton` for values built once.
- OpenAPI 3.1 generated from the same declarations the code runs on, served at
  `/openapi.json` with a self-contained UI at `/docs`.
- RFC 6455 WebSockets and typed server-sent events, implemented in the module
  rather than delegated to a dependency.
- Rate limiting, configuration loading through `MustLoadConfig`, and an
  in-process test client under `testclient`.
- API versioning. `AppOptions.Versioning` turns it on, `WithVersion` declares
  what a route or router answers, and the version is read from the path, a
  header, the `Accept` header or a function of your own. See
  [Versioning](https://muzak.dev/docs/fundamentals/versioning).
- Per-address bounds on long-lived connections: `WSOptions.MaxConnectionsPerIP`
  and `SSEOptions.MaxStreamsPerIP`, so one client cannot hold every slot the
  process has.
- Conservative defaults throughout: non-zero listener timeouts, a one mebibyte
  body cap, rejection of unknown JSON members, CORS closed until a policy is
  written, cross-origin WebSocket handshakes refused, no forwarding header
  believed until a proxy is named, and a panic reported as a generic 500 with
  the stack kept in the log. The full list is in
  [Safe Defaults](https://muzak.dev/docs/security/safe-defaults).
- Dual licence, MIT or Apache-2.0 at your option.

[Unreleased]: https://github.com/muzak-dev/framework/compare/v0.2.5...HEAD
[0.2.5]: https://github.com/muzak-dev/framework/compare/v0.2.4...v0.2.5
[0.2.4]: https://github.com/muzak-dev/framework/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/muzak-dev/framework/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/muzak-dev/framework/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/muzak-dev/framework/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/muzak-dev/framework/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/muzak-dev/framework/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/muzak-dev/framework/releases/tag/v0.1.0
