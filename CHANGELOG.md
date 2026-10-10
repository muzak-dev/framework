# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0.0, a minor bump may carry a breaking change. Each one is listed under
**Changed** with the migration.

## [Unreleased]

## [0.3.0] - 2026-10-10

This release adds what a service needs beside its routes: dependencies that
clean up after themselves and can be replaced in tests, responses that are not
JSON, any net/http handler mounted beside the routes, health checks,
background tasks and timeouts, tracing and request metrics, an outbound HTTP
client that is safe to point at a URL someone else chose, authentication that
enforces what the document describes, sessions with cross-origin protection,
typed endpoints a Go client calls with the server's own types, TypeScript for
every other client, a check that fails a build which breaks the API, an MCP
endpoint that offers chosen routes to AI clients as tools, and a `muzak`
command. Every feature is opt-in and costs nothing until it is used. Each was
written test first, fuzzed wherever it parses what a client or another server
sends, and then reviewed by someone trying to break it. What those reviews
found was fixed before the release and is described with the feature it
belongs to; what they found and left is under **Known limits**, each with the
reason. An existing application behaves as it did unless a change is listed
under **Changed**.

### Added

#### Dependencies

- **`muzak.Dep[T]` declares a dependency on the input type.** A field such as
  `User muzak.Dep[CurrentUser]` receives the value the route's provider
  resolved, read with `in.User.Get()`. A Dep without a provider of exactly its
  type on the route's chain is a build error naming the route, the field and
  the type, instead of a 500 on the first request, and a Dep placed where it
  could never be filled (behind a pointer, in a slice or nested struct,
  unexported, or tagged) is refused the same way. It is never read from the
  request, never a body member and never in the OpenAPI document, and it costs
  no allocation over `From`. Migration: none.

- **`muzak.Acquire` declares a provider that cleans up after the request.**
  Its provider returns a `muzak.Release` beside the value. Releases run
  exactly once each, last acquired first, on every way a request ends:
  success, error, panic, a later refusal, the rate limit counted after the
  dependencies, binding or validation failure, the client leaving, and the end
  of an event stream or WebSocket. Each release is told whether the request
  failed. For a buffered response they run after encoding and before anything
  is written, so a release that fails replaces the success with its error,
  rendered like a handler's error. A file mount and the documentation, which
  already ran the providers they inherit, release what an application-wide
  `Acquire` acquired for them, a file mount answering beneath a handler mount
  included. Migration: none.

- **`muzak.Transaction(db, opts)` gives each request a `*sql.Tx`.** It is
  committed only if the handler and everything after it succeeded, and rolled
  back otherwise. A failed commit is an opaque 500 with the cause logged. A
  handler that carries on past a route's `Timeout` finds its transaction
  already rolled back by database/sql, and is answered with the 503 the
  deadline calls for rather than a 500. Migration: none.

- **`App.Override`, `App.OverrideAcquire`, `testclient.Override` and
  `testclient.OverrideAcquire` replace providers in tests.** Every provider of
  a type in that one application is replaced (routes, mounts and the
  documentation), keeping its lifetime and leaving guards alone. An override
  for a type nothing provides is a build error, and overriding after the build
  panics. Migration: none.

#### Responses that are not JSON

- **`muzak.Bytes`, `muzak.Stream` and `muzak.FileResponse` send a body that is
  not JSON.** A handler returns them as its Out; they are recognised at
  registration and documented as binary content. `Bytes` writes a body held in
  memory under the media type it names, with Content-Length. `Stream` copies
  an `io.Reader` through a pooled buffer and closes it exactly once on every
  path (success, failed copy, client gone, HEAD, an error returned beside it,
  a panic); it aborts the connection when the body fails, ends short of its
  declared `Length` or runs past it, compression included. The route's
  `Acquire` releases run before a stream's body is read, so a stream does not
  read from what they release. `FileResponse` serves one file from an `fs.FS`
  with `http.ServeContent`, so ranges and conditional requests work. Its
  `Name` is refused as a missing file when `fs.ValidPath` refuses it, when it
  names a dotfile, or when it holds a backslash or control character; on
  Windows also for a drive, stream, device name, trailing dot or space, or 8.3
  short name. Types come from the extension, never the content, and HTML, SVG
  and XML are sandboxed unless `AllowActiveContent` is set. A content type
  that is not an RFC 9110 media type fails with a 500 rather than reaching the
  header. Every such response carries `X-Content-Type-Options: nosniff`, and
  `Filename` and `Download` build a cleaned `Content-Disposition` with an RFC
  8187 `filename*`. Migration: none.

- **`muzak.Redirect` refuses open redirects by default.** It sends a Location
  and no body, with 302 for GET and HEAD and 303 otherwise unless `Status` or
  the route says otherwise. Only a path on this origin is accepted: `//`,
  `/\`, tabs, line breaks, controls, non-ASCII, and paths that decode (up to
  three times) or dot-resolve to another host are a 500 with the reason logged
  and the target left out. An absolute http or https URL needs its host listed
  with the new `muzak.RedirectHosts(...)` option (exact, case-insensitive,
  port-aware, no user information or encoded hosts) or `External: true`.
  Migration: none.

- **`muzak.Produces` documents the media types of a Bytes, Stream or
  FileResponse route** as `format: binary` with `contentMediaType`; it is a
  build error anywhere else. The document gains `Response.Headers`
  (`muzak.ResponseHeader`) and `Schema.ContentMediaType`; a document of
  JSON-only routes is unchanged. Migration: none.

- **`muzak.AutoETag()` answers a client that already holds the body with
  304.** On GET and HEAD 200 responses whose body is JSON, HTML or Bytes it
  sets a strong ETag (SHA-256 truncated to 128 bits, base64url) unless the
  handler set one. `If-None-Match` is compared weakly as RFC 9110 says, read
  in one pass that allocates nothing, and ignored when malformed. The 304
  keeps ETag, Cache-Control, Content-Location, Date, Expires and Vary and
  drops body metadata. JSON is encoded with sorted map keys on tagged routes.
  It composes with `Compress` and with guarded routes. It is off by default
  and free when off. Migration: none.

#### Serving beside net/http

- **`Router.Mount` serves any `http.Handler` at a prefix.** pprof, a metrics
  handler, a connect-go service or a legacy application is served at a fixed
  prefix and everything beneath it, for every method, with the most specific
  answer winning: a route beneath the prefix keeps the methods it registers
  and the mount answers the rest, which is what moving a legacy application
  route by route needs. The handler runs inside the middleware chain and
  behind the rate limit, guards and providers of its routers and of the
  options given to `Mount`. It receives the original `*http.Request` with the
  request id and the prefix as its route template, its body is bounded by
  `MaxBodySize`, and its panics are recovered as a route's are.
  `StripPrefix()` gives it paths relative to the mount, and a handler mounted
  that way that parses a multipart body has its temporary files removed when
  it returns or panics, as a route's are. Nothing normalises the path, so a
  request reaches the mount only through its exact prefix, and whatever it
  serves has passed its guards. `App.Override` reaches the providers given to
  `Mount`, and a provider given to it with a nil argument is a build error. A
  `Timeout` given to `Mount` is a build error; one inherited from a router is
  not applied to a mounted handler, as it is not to an event stream.
  Conflicting mounts and invalid prefixes are build errors, and mounts are not
  in the OpenAPI document. Migration: none.

- **`AppOptions.AllowedHosts` refuses requests for hosts the application does
  not serve.** Anything else gets 421 Misdirected Request through the error
  renderer, and is logged, before CORS, middleware, routing or documentation
  run. Entries are `host` or `host:port`, case-insensitive for ASCII only; a
  trailing dot is ignored, a leading `*.` matches subdomains but not the
  domain, and IPv6 goes in brackets. Forwarding headers are never consulted.
  Entries that can never match are build errors. Unset, it costs nothing.
  Migration: none.

- **`AppOptions.RedirectHTTPS` redirects plain HTTP to https.** GET and HEAD
  get 301, everything else 308. A request counts as https over TLS, or when a
  trusted proxy's own `X-Forwarded-Proto` (or `Forwarded proto=`) says so; a
  claim from any other peer is ignored. The redirect only names a host the
  application vouches for, so enabling it without `AllowedHosts` or
  `RedirectHTTPSOptions.Host` is a build error. ACME HTTP-01 challenges are
  never redirected. Migration: none.

- **`AppOptions.ProblemDetails` renders errors as RFC 9457 problem details.**
  Every error becomes a `muzak.Problem` sent as `application/problem+json`,
  with `type`, `title` (translatable at `muzak.status.<code>`), `status`,
  `detail`, `instance` (`urn:uuid:` and the request id), `code`, `errors` and
  `request_id`, and the OpenAPI document describes errors that way. That
  includes the 405 the documentation answers and the 500 of an application
  that did not build. It decides everything by asking `DefaultErrorRenderer`,
  so codes, details, translations, 5xx opacity and preserved headers are
  identical. `muzak.ProblemDetails` is exported for composition. The default
  envelope stays the default. Migration: none.

#### Operations

- **`AppOptions.Health` serves liveness and readiness endpoints a platform can
  probe.** With `Enabled` set, `/livez` answers 200 for as long as the process
  serves, and `/readyz` answers 503 until the lifecycle components have
  started, 200 once every `HealthCheck` passes, and 503 again from the moment
  a shutdown begins, before any connection is closed. Probes are answered
  ahead of routing: before `AllowedHosts`, `RedirectHTTPS`, `App.Use`
  middleware, guards, the rate limit and CORS. They take GET and HEAD only,
  are sent `Cache-Control: no-store` and are not in the OpenAPI document.
  Checks run concurrently under their own timeouts; concurrent probes share
  one run and the result is reused for `CacheInterval`, so a flood of probes
  costs one run of the checks per interval, and a check that hangs is not
  started again while it runs. A body carries `{"status":"ok"}` or
  `{"status":"unavailable"}` and, only with `ReportChecks`, each check's name
  and outcome; what a check returned is logged when its state changes, never
  sent. Probes are access-logged at debug level. A health path that a route, a
  mount or the documentation answers is a build error, and so is a health path
  at `/` beside a frontend or a handler mounted at the root. Migration: none;
  the zero value serves nothing.

- **`ServerOptions.DrainDelay` keeps serving, with readiness down, before the
  listeners close.** It gives a load balancer, or Kubernetes removing a
  terminating pod from its Service, time to stop sending traffic, replacing a
  `sleep` in a `preStop` hook. The delay counts against `ShutdownTimeout`; a
  negative one, or one not shorter than `ShutdownTimeout`, is a build error.
  Migration: none; it defaults to zero.

- **`Context.AfterResponse` runs work the client should not wait for.** A task
  registered from a handler, guard or provider runs on a bounded pool
  (`AppOptions.Background`, 4 workers and a queue of 64 by default) once the
  handler has returned nil without panicking and the releases were told the
  request succeeded, so a task never follows a transaction that was rolled
  back or a value that failed to encode. It runs with a context that keeps the
  request's values but not its cancellation, and is cancelled at the shutdown
  deadline. It never receives the pooled `Context`. A full queue returns
  `ErrBackgroundQueueFull` and a shutdown that has closed the listeners
  returns `ErrBackgroundShuttingDown`, at once and without starting a
  goroutine. Workers start only when a task is queued and exit when idle. A
  shutdown drains in-flight requests, then the tasks, then stops the lifecycle
  components, so a task can still use them; at the deadline queued tasks are
  dropped and running ones cancelled. A panic in a task is recovered and
  logged with the request identifier. It is refused on event stream and
  WebSocket routes. Migration: none.

- **`muzak.Timeout(d)` gives a route or router a cooperative deadline.** The
  deadline is set on the request's context before the route's dependencies,
  binding and handler run; no second goroutine ever runs on the request's
  behalf. A failure the route's own deadline caused, before the response
  started, is answered 503 with `Retry-After: 1` through the error renderer
  and documented in the OpenAPI document; an error with its own status, a
  client that left and a shorter deadline the handler set are left alone, and
  a handler that ignores the deadline has its success sent. The narrower
  declaration wins and a negative value removes an inherited one. Declared on
  an event stream or WebSocket route it is a build error naming `MaxLifetime`;
  inherited from a router it is not applied to them. A route without one pays
  nothing. Migration: none.

#### Observability

- **Tracing with W3C trace context, through `AppOptions.Tracing`.** Naming a
  `Tracer` gives every request one server span named after its method and
  route template, never its path, with the OpenTelemetry HTTP attributes. A
  valid `traceparent` is continued along with its sampling decision;
  `TracingOptions.Parent` can limit that to `ClientIP.TrustedProxies` or turn
  it off. `traceparent` and `tracestate` are parsed strictly and fuzzed, and
  identifiers come from crypto/rand. A 5xx, an aborted response and a failed
  stream or WebSocket mark the span an error. A failure the log records
  becomes an `exception` event with the same text. The span ends when the
  response, stream or connection does. `SpanContextFromContext`,
  `InjectTraceContext`, `SpanFromContext` and `StartSpan` give handlers the
  trace, and `SampleRatio` samples new traces. Left unset, nothing is
  installed and nothing is allocated. Migration: none.

- **The access log and `Context.Logger` carry `trace_id` and `span_id` when
  tracing is on.** Migration: none; records are unchanged when it is off.

- **`AppOptions.Observer` is the request metrics hook.** A `RequestObserver`
  is called exactly once per request after the response, including 404s,
  failures, panics, aborts, event streams and WebSockets. It receives a
  bounded method, the route template, status, duration, request and response
  sizes, whether the response was aborted, and the span context for exemplars.
  A method no standard defines is recorded as itself only when a route
  registered for that method answered; through a handler mount, which answers
  every method, it is `_OTHER`, so a client cannot mint labels. A Tracer or
  observer that implements `Lifecycle` is started and stopped with the
  application, and one that panics is logged and contained. Migration: none.

- **`muzak.dev/framework/otlp` exports spans to any OpenTelemetry collector.**
  It uses OTLP/HTTP with the JSON encoding and the standard library only, and
  is a `Tracer` and a `Lifecycle`. Spans go into a bounded queue that drops
  and counts rather than waits, and are batched by size or interval, with
  optional gzip. A span is held to 64 KiB, counting every value it keeps, not
  only its text. It retries 429, 502, 503 and 504 with backoff and jitter,
  honouring a capped `Retry-After` that asks for longer but never one that
  would shorten the backoff, never follows redirects, and reads answers up to
  64 KiB. `Stop` flushes within its deadline and leaves no goroutine behind,
  and a second `Stop` returns at once. Header values are validated and never
  printed, an endpoint with credentials in it is refused, and `Stop` closes
  idle connections only on a transport the exporter made itself, never on a
  supplied client's or on `http.DefaultTransport`. Migration: none.

#### Calling other services

- **`muzak.NewClient` builds an outbound HTTP client that is safe to point at
  a URL someone else chose.** Loopback, private, link-local, shared, reserved,
  documentation and cloud metadata addresses are refused on the socket at the
  moment of connecting. So a name that resolves to one, DNS rebinding and
  redirects all meet the same check, as do IPv6 forms that embed a refused
  IPv4 address (NAT64, 6to4, IPv4-compatible and the IPv4-translated form of
  SIIT) and spellings such as `127.1` or `2130706433`. The local-use NAT64
  block `64:ff9b:1::/48` is refused as a private range is, and is read in
  every layout a translator may use, so a metadata service or a denied network
  stays refused through it. `AllowPrivateNetworks`, `AllowedNetworks` and
  `DeniedNetworks` relax or tighten the policy, and a metadata service stays
  refused unless it is named. Environment proxies are never read. A refusal is
  an `*AddressRefusedError`. Migration: none.

- **The client bounds everything and retries only what is safe.** There is a
  30-second overall timeout that covers the body, beside dial, TLS and header
  timeouts, and a 10 MiB body cap counted after decompression
  (`ErrResponseTooLarge`). At most five redirects are followed, never from
  https to http, and credentials are dropped once a redirect leaves the
  origin. TLS 1.2 is the minimum, and headers carrying CR, LF or NUL are
  refused. Idempotent requests, and requests with an `Idempotency-Key`, are
  retried on connection errors and on 429, 502, 503 and 504. The wait between
  attempts is randomised and doubles, and honours `Retry-After` up to a cap. A
  `RetryBudget` stops retry storms. Migration: none.

- **An opt-in per-host circuit breaker, propagation and JSON helpers.**
  `CircuitBreakerOptions{Threshold}` opens a host's circuit after consecutive
  failures, sends one probe after `Cooldown`, and keeps a bounded LRU of
  failing hosts; an open circuit returns `ErrCircuitOpen`. `DefaultPropagate`
  copies the request id into `X-Request-Id` and, when the request is traced,
  the trace context into `traceparent` and `tracestate`, leaving alone what
  the caller set. `GetJSON`, `PostJSON` and `DoJSON` decode bounded JSON and
  report any other status as a `*RemoteError`. Migration: none.

#### API compatibility

- **`muzak.CompareDocuments` reports what a change to the API does to its
  clients.** It compares two OpenAPI documents the way a client meets them: a
  request schema may only widen and a response schema may only narrow, so a
  new required request member, a narrowed request type, or a response member
  that is gone or may now be null is `Breaking`, a new value in a response
  enum or a relaxed response bound is `PossiblyBreaking`, and a new optional
  member or operation is `Compatible`. Operations, parameters, bodies,
  statuses, media types, the headers each response documents and security
  requirements are judged too: a required response header that is gone or may
  now be absent is breaking, and a removed operation that was deprecated is
  only possibly breaking. Each `APIChange` carries a stable kind, a JSON
  pointer location and a one-sentence message, and the list is sorted.
  References are followed through components, so a renamed component, or the
  `Input` copy of a type a response also uses, is compatible. Each pair of
  components is compared once per direction, so shared and recursive schemas
  cost time linear in the documents. The work is charged for every value, name
  and byte a schema holds each time it is read, the report is capped at
  100,000 changes or 64 MiB of text, and a document built to be expensive
  stops at that bound and says so with a breaking `comparison-incomplete`.
  `muzak.ReadDocument` reads a stored document strictly, within 16 MiB and 128
  levels, refuses a null response or header, and escapes terminal control
  characters in every location an error quotes; `muzak.WriteChanges` prints a
  report grouped by severity with the same escaping. Migration: none.

- **`testclient.AssertCompatible` fails a test that breaks the committed
  API.** It compares the application's document with a baseline file, fails
  listing the breaking changes and logs the possibly breaking ones. Run with
  `MUZAK_UPDATE_OPENAPI=1` it records the current document instead, creating
  the directories it needs and writing through a temporary file renamed into
  place, and logs what the new baseline accepts. A missing baseline fails with
  instructions to create it. Migration: none.

#### Authentication

- **`muzak.JWTBearer` is a security scheme that verifies the JWT bearer tokens
  it documents.** A route whose `WithSecurity` names it refuses a request
  without a valid token before any guard, provider or handler runs, and hands
  the verified `*muzak.Claims` to `From`, `TryFrom` and `Dep`. Tokens are
  checked against an explicit allowlist of HS, RS, PS, ES and EdDSA
  algorithms, never `none`, with a key of the algorithm's own kind that the
  token's `kid` selects, so an RSA public key is never an HMAC secret and a
  token cannot pick its key. Issuer, audience, `exp`, `nbf` and `iat` are
  checked with a bounded leeway. `crit`, `jku`, `jwk`, `x5u` and `x5c` are
  refused. The token's length, its base64url and the depth of its JSON are all
  bounded, and JSON members may not repeat. Keys are configured in the
  application or fetched from a JWKS through the SSRF-safe client: within
  size, count and time bounds, cached as `Cache-Control` allows, refreshed in
  the background as a lifecycle component, fetched again for an unknown `kid`
  at most once per interval, and the last good set is kept when a fetch fails.
  A key set is used only when it arrived over https, or plain http to a
  loopback address, wherever a redirect took the client, so a redirect down to
  plain http cannot replace the keys. A refusal is RFC 6750's `401` or `403
  insufficient_scope`, rendered by the error renderer and saying nothing about
  which check failed. `muzak.ClaimsAs[T]` decodes custom claims. Migration:
  none.

- **`muzak.APIKeyVerifier` verifies API keys in a header, query parameter or
  cookie.** Keys are held as SHA-256 digests and compared with every key in
  constant time. A request that sends its key twice is refused. Neither a key
  nor its digest is ever logged. A `Lookup` resolves keys stored elsewhere.
  The verified caller is a `*muzak.APIKeyPrincipal`, and its scopes are
  checked against `Require`. Migration: none.

- **`JWTOptions.ResourceMetadata` publishes RFC 9728 protected resource
  metadata** at `/.well-known/oauth-protected-resource`, public and left out
  of the OpenAPI document, and every challenge names it in
  `resource_metadata`. This is how an OAuth or MCP client finds the
  authorization server. Off by default. Migration: none.

#### Sessions and cross-origin requests

- **`AppOptions.Sessions` gives every request a session through
  `ctx.Session()`.** It is read the first time a handler, guard or provider
  asks for it, so a request that never asks parses, decrypts and writes
  nothing. By default the session lives in the cookie, encrypted with
  AES-256-GCM under keys derived with HKDF-SHA256 from
  `SessionOptions.Secrets`: the first secret encrypts, all decrypt, and
  secrets under 32 bytes are a build error. Each cookie uses its own key
  derived from a random 192-bit nonce, and the cookie name is bound as
  additional data, so a cookie cannot be replayed under another name. A
  `SessionStore` (`Load`, `Create`, `Update`, `Delete`), such as the bounded
  `NewMemorySessionStore`, keeps sessions on the server; the cookie then
  carries a random 256-bit identifier that the store only sees hashed. A
  session another request ended, such as by signing out, is never brought
  back: not by `Update`, not by a `Save` followed by another change, and not
  by `Regenerate`, which first checks that the old entry still exists. The
  idle timeout (2h) and lifetime (24h) are kept inside the authenticated
  record, and tampered, truncated, foreign or expired cookies read as no
  session. The cookie is HttpOnly, Secure, SameSite=Lax and named
  `__Host-session` by default; attribute mistakes are build errors. It is
  written once, after the releases and before the response, only when the
  request succeeded and the session changed or is due for renewal; `Save`
  keeps a change regardless of what follows. A response carrying the cookie is
  never stored by a shared cache, whatever sets `Cache-Control` after the
  cookie (the handler after `Save`, an error renderer, or a file range). An
  event stream keeps its `no-transform`, a session read on an event stream or
  WebSocket leaves their `Cache-Control` to them, and a change made after the
  response started is warned of. `muzak.SessionGet[T]`, `Set`, `Delete`,
  `Clear`, `Regenerate` (call it on sign-in) and `Destroy` work on it, and
  `Set` refuses growth past `MaxSize` with `ErrSessionTooLarge`. Migration:
  none.

- **`AppOptions.CrossOriginProtection` refuses state-changing requests a
  browser sends from another origin.** It is built on net/http's
  `CrossOriginProtection` (Sec-Fetch-Site, falling back to Origin against
  Host), needs no tokens and never refuses GET, HEAD or OPTIONS. A refusal is
  a `403` classified `cross_origin_request` (`CodeCrossOriginRequest`),
  rendered by the error renderer (problem details included) with `Vary:
  Origin, Sec-Fetch-Site`, logged, and made before any middleware, guard,
  handler, mount or documentation runs. `TrustedOrigins` are validated as CORS
  origins are, and a CORS-allowed origin is not trusted for unsafe methods
  unless it is listed. `TrustedOrigins` are matched exactly, so a pattern in
  one is a build error saying that each origin must be listed.
  `InsecureBypassPatterns` are `ServeMux` patterns; invalid, conflicting and
  safe-method patterns are build errors, and so is a pattern whose path is
  `/`, with or without a method or host, since it would turn the protection
  off for everything beneath it. Any other pattern ending in `/` covers its
  whole subtree, as a `ServeMux` pattern does, and is warned of at build;
  ending it in `{$}` matches one path. WebSocket handshakes stay with
  `WSOptions.AllowedOrigins`. Sessions cannot be configured without it.
  Migration: none.

#### Typed endpoints and TypeScript

- **Typed endpoints: declare an operation once and call it from Go.**
  `muzak.NewEndpoint[In, Out](method, path, opts...)` puts the method, path
  template, route options and both types in one value, which a package shared
  by a service and its callers declares. `Router.Implement(ep, handler,
  opts...)` registers it exactly as `Handle` would, and a handler of the wrong
  types does not compile. `Endpoint.Call(ctx, client, in, opts...)` writes
  `in` as the exact inverse of binding, compiled from the binder's own plan:
  path segments escaped, query and header lists, quoted cookies, a JSON body
  of body members only, and multipart forms. An input that decodes itself is
  written from its body members alone, never its located fields, so a
  credential bound to a header never lands in a body. Before sending, a call
  refuses any value no request carries unchanged, wrapping `ErrCallRefused`
  and never quoting the value: that includes a body member with a default that
  its json tag's `omitzero` or `omitempty` would leave out, which would
  otherwise arrive as the default, and a client's `Propagate` that changes a
  header the input set. Outputs `Bytes`, `Stream` and `Redirect` come back as
  themselves, a redirect is returned rather than followed, and the file name
  another server offers for a Bytes or Stream is cleaned to one name.
  `CallHeader` adds unbound headers, and `ValidateFirst` runs the input's
  rules before sending. An input whose `Validate` panics while the call is
  compiled panics alike on every call. Migration: none.

- **`ClientOptions.BaseURL` names the service a client calls endpoints on.**
  It must be an absolute http or https URL with no query, fragment or user
  information, or `NewClient` panics. `Do` ignores it. Migration: none.

- **`RemoteError` reads the error envelope.** `Code`, `Message`, `Details` and
  `RequestID` are filled from the default envelope or from problem details.
  The body is read only under a JSON media type, at most 64 KiB, whole or not
  at all, keeping at most a hundred details. `Error()` names the code when it
  is a plain identifier of at most 64 bytes (`muzak: GET
  https://api.example.com answered with status 404 (not_found)`). This applies
  to `Call` and to `DoJSON`, `GetJSON` and `PostJSON`. Migration: none.

- **The `tsgen` package writes TypeScript for an application's API.**
  `tsgen.Generate(doc, tsgen.Options{Client: true})` writes an interface or
  alias per component, `Params`, `Body`, `Response` and `Error` types per
  operation with an `Operations` map, and optionally a small fetch client.
  Output is deterministic and safe to generate from a document someone else
  wrote: names are sanitised, every string is escaped so nothing in the
  document can break out of a comment or a string literal, and the client's
  own type names are reserved. With `Client`, the static text of a path is
  escaped so a request cannot leave the `baseUrl` it is given, and a path
  holding a dot segment is an error. A parameter described twice is one
  member. Generation is bounded in depth and work and linear in what a
  document repeats. Migration: none.

#### MCP

- **`App.MCP` serves a Model Context Protocol endpoint, through which an AI
  client calls the routes you choose as tools.** The endpoint speaks the
  Streamable HTTP transport of MCP 2025-03-26, 2025-06-18 and 2025-11-25,
  opening a session with `initialize`, and of 2026-07-28 statelessly,
  answering every message as `application/json`. Nothing is a tool until it is
  chosen, with `muzak.MCPTool()`, `MCPOptions.Tags` or `MCPOptions.Include`;
  WebSocket, event stream and hidden routes, mounts, static files, the
  documentation and the endpoint itself never are. A tool is described from
  the OpenAPI document: its name is the operation id, its input is grouped by
  path, query, header, cookie, body and form, and its output is the success
  schema, with every reference resolved. A call decodes its arguments by the
  binder's rules, writes them with the typed-endpoint encoder, and serves the
  request in-process through the whole application, so the route's security,
  guards, providers, rate limit, validation and releases apply, and the call
  can reach its own route alone. It carries the MCP request's `Authorization`,
  the headers and cookies the options forward, the client's address, the
  request identifier, the trace and the cancellation, and nothing else. A
  failure is an `isError` result carrying the application's error envelope,
  and every answer is bounded by `MaxResultSize`. The `Origin` is checked
  against DNS rebinding. Messages are read strictly within the body limit and
  128 levels of nesting. Sessions are bounded and bound to the principal that
  opened them, and `tools/list` cursors are authenticated. An application
  without an endpoint behaves and allocates exactly as before. A tool call's
  credentials come only from the client's request: a route whose input binds
  the header, query parameter or cookie that an API key scheme or the session
  reads one from cannot be a tool, so a model can never choose whose key a
  call is made with. The request a call writes is held to the smaller of its
  route's body limit and the endpoint's, so a short argument cannot be written
  out as a request hundreds of times its size, and a request carrying
  `Mcp-Method` or `Mcp-Name` is held to them whatever revision it speaks.
  Migration: none.

#### The muzak command

- **The `muzak` command creates, runs and inspects an application.** `go
  install muzak.dev/framework/cmd/muzak@latest` installs a command that is
  part of the framework's own module and uses the standard library alone, so
  the command at a version matches the framework of that version. `muzak new
  <dir> [-module path]` creates a project laid out as `example/` is: a
  `go.mod` requiring that framework version, `cmd/server`, `routers`,
  `handlers`, `schemas`, a handler test served through `testclient`, a
  `.gitignore` and a README. It writes only into a directory it creates or one
  that is empty, refuses a file or a symbolic link in the way, writes every
  file exclusively through an `os.Root` so nothing can lead a write outside
  the directory, checks the module path as the go command would, and removes
  what it created when a write fails. Everything the command prints from a
  document, a server or a file is escaped for the terminal, and an unknown
  command or flag exits 2 with a suggestion. Migration: none.

- **`muzak dev` rebuilds and restarts an application when its sources
  change.** It builds the package with `go build`, runs it, and polls the tree
  by stat, which works the same on every platform and file system. It watches
  only the `-ext` extensions (by default Go sources, module files, YAML and
  `.env`, so an application writing a database or log into its tree does not
  restart itself), skips `.git`, `node_modules`, `vendor`, `testdata` and
  dot-directories, and refuses a tree of more than 10,000 watched files. A
  rebuild waits for a save to settle, and a build that fails prints the
  compiler's output and leaves the running application up. On Unix the
  application runs in a process group of its own: `SIGINT`, `SIGTERM`, a
  terminal hang-up (`SIGHUP`) and Ctrl-\ (`SIGQUIT`) are forwarded to the
  whole group, which is given `-grace` before it is killed, so processes the
  application started are stopped too, closing the terminal never leaves the
  application holding its port, and none is left as a zombie. On Windows the
  application is killed at once and a process it started is left running. A
  `-watch` root that is a symbolic link is followed. Migration: none.

- **`muzak routes`, `muzak diff` and `muzak ts` read an OpenAPI document from
  a file, standard input or a running application.** `routes` prints each
  operation's method, path, operation id, summary and security requirements.
  `diff` runs `CompareDocuments`, prints the `WriteChanges` report, and exits
  1 on changes as serious as `-fail-on breaking|possibly|never` names and 2
  when a document cannot be read, for a CI step to gate on. `ts` writes the
  `tsgen` declarations, with `-client` the fetch client too, to standard
  output or atomically to the `-o` file. `-url` fetches through a
  `muzak.Client` bounded at 30 seconds and 16 MiB that may reach loopback and
  private addresses, where a developer's own server listens, but never a cloud
  metadata address. A new `-o` file is created under the umask. Migration:
  none.

### Changed

- **`WithSecurity` and `Public` enforce a scheme built by `JWTBearer` or
  `APIKeyVerifier`.** Every descriptive scheme behaves exactly as before.
  Where a verifying scheme is named, the requirements are enforced as the
  document describes them: alternatives, schemes needed together, scopes, and
  an empty requirement for anonymous access. Enforcement also covers the HEAD
  a GET route answers, the `Mount`, `Static` and `Frontend` beneath the
  router, and, for a declaration on `New`, the documentation. The requirements
  are judged before a route declared with `CaptureBody` reads its body, so a
  request without a valid credential cannot make the server buffer one.
  Alternatives that mix verifying and descriptive schemes are a build error,
  and so is a `Dep[*Claims]` or `Dep[*APIKeyPrincipal]` that the route's
  security does not always fill. Migration: none for an existing application;
  this applies only once a verifying scheme is declared.

- **The documentation's 405 is rendered by the error renderer.** A method
  other than GET or HEAD on the OpenAPI document or the documentation UI is
  answered with the JSON envelope (or a problem) and `Allow: GET, HEAD`
  instead of a line of text/plain; on guarded documentation it is still
  answered after the guards. Migration: none unless a client read that text.

- **An OpenAPI document that cannot be encoded as JSON is a build error.** The
  document was encoded only when it was first served, and a failure was logged
  and `/openapi.json` silently left unserved, so an application learned of it
  from a client. Text a struct tag supplies that is not valid UTF-8 is how one
  gets there. The document is now encoded once at build, a failure is a build
  error saying why, and the documentation serves the bytes that were checked.
  Migration: fix the tag the error names.

- **A request that failed because its client went away is logged at debug
  level.** A session store read, a rate limit count, or a handler that
  returned the request's own cancellation was logged as an error (or a warning
  with `FailOpen`), so a client could write error lines at will by connecting
  and hanging up. These are now logged at debug level, as an event stream or
  WebSocket its client ended already was. A `StoreTimeout` or `StorageTimeout`
  that runs out is still logged as the failure it is, and what the client is
  sent does not change. Migration: none, though an alert on those error lines
  stops firing for abandoned requests.

- **A frontend file that cannot be opened, measured or read is answered
  through the error renderer.** It was answered with a line of text/plain from
  `http.Error` and settled as a success, so `ProblemDetails` and a custom
  renderer were bypassed and the releases were told the request succeeded. It
  is now rendered as any failure is, still 404 for a file that vanished and
  500 for one that could not be read, the read failure is logged, and the
  request counts as failed. Migration: none unless a client read that text.

- **The example application signs in with a session.** `example/` signs in
  with `Regenerate`, signs out at `/logout/` with `Destroy`, and no longer
  returns a `session_id` from sign-in. Its item WebSocket resolves its caller
  from the session, or a token, as a `Dep`, instead of reading a raw cookie
  and echoing the credential in every message. Its `SESSION_SECRETS` has a
  development default, as `ADMIN_TOKEN` does, which a deployment must replace.
  Migration: none for the framework; a client of the example that read
  `session_id` reads the cookie instead.

### Fixed

- **A HEAD answered by a GET route is named after that route.** The route
  template was published where a request is dispatched, which a HEAD answered
  by its GET route never passes through, so the access log and
  `RouteFromContext` saw a request to no route at all, and so would the new
  server span and request observer. It is now published where every route is
  run. Migration: none.

### Documentation

- **The documentation site has a page for each feature.** New pages: Command
  Line, Observability, TypeScript, API Compatibility, Files, Streams and
  Redirects, Problem Details, Mounting net/http Handlers, Background Tasks,
  Timeouts, HTTP Client, Typed Endpoints, MCP Tools, Allowed Hosts and HTTPS,
  Sessions, Cross-Origin Requests, and Health Checks. The dependencies and
  authentication pages are rewritten, and the pages the new features extend
  link to them.

- **The package documentation describes each feature** in its own section:
  dependencies, responses that are not JSON, serving beside net/http,
  operations, observability, calling other services, API compatibility,
  authentication, sessions and cross-origin requests, typed endpoints, routes
  as MCP tools and the `muzak` command.

### Tests and continuous integration

- **Tests that failed only on a loaded machine no longer do.** Tests that
  compared a duration with a fixed deadline now measure a yardstick on the
  same machine, or wait for the state they expect instead of sleeping: event
  streams and their keepalives, WebSockets, `Unique`, the client and exporter,
  the server, lifecycle and background states, singleton waiters and the
  request timing tests. The two tests that assert no goroutine leaked run one
  at a time, the WebSocket connection caps count only upgraded sockets, and
  the comparisons of allocation counts are skipped under the race detector,
  which allocates on its own account.

- **The source checks skip a nested checkout**, such as a git worktree inside
  the tree, which is another copy of the code rather than part of it.

- **36 new fuzz targets** cover every parser the release adds and every value
  it builds from one: `traceparent` and `tracestate`, a JWS, its claims and a
  JWKS, the session cookie, identifier, record and request, the host
  allowlist, `Forwarded` parameters, redirect targets, media types, file names
  and `Content-Disposition` both ways, `If-None-Match`, `Retry-After` and
  cache lifetimes, an OTLP partial success, the client's address policy, mount
  prefixes, health probes, cross-origin headers, `Dep` fields a request must
  never fill, typed endpoint round trips, TypeScript generation, reading and
  comparing documents, what the command prints, and the MCP message reader,
  tool arguments and transport headers.

### Known limits

The reviews found these and left them, each with the reason.

- **Calling other services.** A NAT64 translator using a network-specific
  prefix cannot be told apart from a public IPv6 address; name its prefix in
  `DeniedNetworks` where a network has one. A 307 or 308 that a client follows
  to another origin sends the body again (credentials are dropped), as
  net/http does; with `AllowPrivateNetworks` that origin can be another
  internal service. `DefaultPropagate` sends the request id and trace context
  to every host a client calls; a client that calls hosts someone else chose
  can use a `Propagate` of its own.
- **Responses.** `FileResponse` serves an uploaded `.js` or `.css` file under
  its real type, which a `script-src 'self'` policy trusts; serve uploads from
  another origin or as `application/octet-stream`, since `Download` alone does
  not stop a page loading one as a script. `Bytes` and `Stream` sent as
  `text/html` are not sandboxed, since the handler chose the type.
- **Logging.** The access log logs every 5xx at error level by its documented
  rule, so a fail-closed 503 answered to a client that has already left is
  still an error line there.
- **Operations.** A slow request holds its `AfterResponse` queue slot from the
  moment it registers a task, which is what makes a full queue an answer at
  registration rather than a task dropped later. With `ReportChecks`, a
  check's name and outcome are readable through any host name, a DNS-rebinding
  page included, because probes are answered before `AllowedHosts`; leave it
  off where the probe port is reachable from outside. Probes reach the
  observer and the tracer with an empty route. A refused host is logged at
  warning level on each request.
- **Observability.** The default `TraceParentAccept` lets a client ask for its
  request to be sampled; use `TraceParentFromTrustedProxies` where that
  matters. One `otlp.Exporter` shared by two applications is stopped by the
  first to stop. An application that failed to build does not call its
  observer for the 500s it answers. The 64 KiB span bound does not count the
  name, status and `tracestate`, which are bounded on their own.
- **Authentication.** Several issuers in one `JWTBearer` scheme share its
  keys, so give each identity provider a scheme of its own. An API key in a
  cookie is sent by the browser on its own, so pair it with
  `CrossOriginProtection`. An event stream or WebSocket outlives the token
  that opened it; set `MaxLifetime`. The request that starts a JWKS fetch
  waits for it, up to `JWKSOptions.Timeout`, and a key set is kept for as long
  as fetches fail. A `Public()` or descriptive `WithSecurity` on an inner
  router replaces a verifying requirement given at `Include`, as OpenAPI's own
  rule for nested requirements says.
- **Sessions.** Two applications sharing one `SessionStore` share sessions,
  since keys are not namespaced by application or cookie name; give each its
  own store. A deletion that lands between `Regenerate`'s check and its delete
  can still be raced.
- **Typed endpoints and TypeScript.** A nil slice or map body member arrives
  as `[]` or `{}`, a pointer to a nil pointer arrives as nil, and a
  `time.Time` keeps its instant but not its location. A path value that spells
  a static sibling route reaches that route, and a header the input leaves out
  (a nil pointer or an empty list) is filled by `Propagate`. A `BaseURL` with
  dot or empty segments is accepted as written, since it is the operator's own
  setting. A schema property named `toString` makes a generated interface
  unassignable from an object literal, which is TypeScript's own rule.
- **MCP.** Sessions of the session-based revisions live in memory, so a
  deployment of several instances needs sticky routing for them; 2026-07-28
  clients do not. Batches are refused for every revision, including
  2025-03-26, which allowed them. A route that binds a non-pointer
  `User-Agent` cannot be called unless the model supplies one, and an optional
  non-pointer string header or cookie is sent empty, as `Endpoint.Call` does.
  A path that `App.Use` middleware or the documentation answers, which a
  wildcard argument can spell, runs before the result is refused; only routes
  are refused beforehand. A route's response headers, `Set-Cookie` among them,
  are not returned, so a cookie session a tool changes is not kept, and a 401
  or 403 is an error result rather than a challenge the client can answer. An
  API key read from the query string cannot authenticate a tool call. A
  session opened without credentials is bound to nobody, a flood of
  `initialize` on an unguarded endpoint evicts the longest-idle sessions, and
  `tools/list` shows every tool to whoever the endpoint admits; guard or
  rate-limit the endpoint. `MCPTool` on a route of an application without an
  endpoint is ignored, so one router can serve applications with and without
  one.
- **The muzak command.** On Windows `muzak dev` kills the application at once
  and leaves a process it started running. A poll reads every directory under
  `-watch` that is not skipped, and only watched files count toward its cap,
  so a large data directory belongs outside the watched tree. A `muzak dev`
  killed with `SIGKILL` leaves the application running. An interrupted `muzak
  ts -o` can leave a temporary file beside its output. `tsgen` refuses schemas
  nested more than 64 levels, where 128 are read.

## [0.2.9] - 2026-10-09

This release is the result of a fifth review of the framework, done the way
the earlier ones were and covering every subsystem again: routing, binding,
validation and the generated document, the server and its lifecycle, logging
and configuration, rate limiting, CORS and client addresses, compression and
static files, event streams, WebSockets and the translation engine. Most of
what it found is a promise the documentation made that the code did not keep,
and each of those is now either kept or no longer made. Every fix below was
reproduced with a test that failed before it, and that test is now a
regression test. Several fixes tighten a default or change what a client is
told; each one is listed under **Changed** with its migration. The suite now
runs on Windows as well as Linux.

### Added

- **`ClientIPOptions.Header` reads the RFC 7239 `Forwarded` header.** The
  setting accepted `"Forwarded"`, but its entries are parameter lists, not
  addresses, so `for=203.0.113.9;proto=https` could not be read, the walk
  stopped at the proxy, and every client behind it shared one `IPTracker`
  budget and one per-client WebSocket and SSE allowance. The `for` parameter
  is now read from each entry: names in any case, quoted strings, a bracketed
  IPv6 address with or without a port, an IPv4 address with a port, several
  entries and several header lines, walked right to left with the same
  trusted-proxy rule as `X-Forwarded-For`. An entry whose `for` is `unknown`
  or an obfuscated `_name`, or that has none, stops the walk at the nearest
  trusted hop. A comma or semicolon inside a quoted parameter is data, so a
  proxy that quotes a client's `Host` into its own entry cannot be talked into
  a client's `for`. The parser is fuzzed. Migration: none.

- **`WSOptions.EnforceOriginCheck` turns the origin check back on beneath a
  scope that turned it off.** Options layer field by field and a `false` bool
  reads as "not set", so `InsecureSkipOriginCheck` set on the application or a
  router left every route beneath it open to any origin, with no way for one
  cookie-authenticated route to close again: it was the only security setting
  whose layering could only move towards less secure, and 0.2.8 listed it as
  by design. A router or route that sets `EnforceOriginCheck: true` gets the
  ordinary check back, a scope beneath it can set `InsecureSkipOriginCheck`
  again, and the narrowest scope that set either wins. A scope that sets both
  keeps the check. Migration: none; nothing changes until it is set.

- **`SSEEvent.EmptyData` sends an event whose data is empty.** An event is
  dispatched by a browser, and by `SSEReader`, only when it carries a data
  field, and an empty `Text` wrote none, so a stream of text had no way to
  send an empty message and a name-only tick was never dispatched. With
  `EmptyData: true` the event carries `data:` with nothing after it. It has no
  effect on an event that carries `Data` or a non-empty `Text`, and every
  existing event is written exactly as before. See **Fixed** for the
  documentation it corrects.

- **`i18n.PluralOperands` and `i18n.PluralOperandsOf`.** A plural rule now
  reads CLDR's operands (`n`, `i`, `v`, `w`, `f` and `t`) instead of an `int`,
  and `PluralOperandsOf` reads them off any integer or float, from the
  shortest decimal that names it, which is the number a translation prints.
  The built-in rules `hindi`, `punjabi`, `belarusian` and `macedonian` are
  new. See **Changed** for the signature this replaces.

### Changed

- **One application is run by one run method at a time.** A second `Run`,
  `RunContext` or `RunSignals` on an App that was still serving failed to bind
  the same address and then, as any run whose socket could not be opened does,
  stopped the lifecycle components the first run was still serving with. It
  had also replaced the runner the App kept, so cancelling the first run's
  context shut down a runner that never served and the first run never
  returned. A run method called while another is starting, serving or
  shutting down now returns an error at once and touches nothing the first
  one uses. Running the application again after a run has returned works as
  before. Migration: build a second App to serve a second address.

- **A `Shutdown` that comes before its run is no longer lost.** `go app.Run()`
  followed by `app.Shutdown(ctx)` returned nil and was forgotten when the
  goroutine had not reached `Run` yet, and the server went on serving; after
  an earlier run, the `Shutdown` reached that run's finished runner instead. A
  `Shutdown` that finds no run in progress is now kept for the next run, which
  builds the application, logs that a shutdown was requested, and returns nil
  without starting the components or opening a socket, much as
  `ListenAndServe` returns `ErrServerClosed` after `Shutdown`. Only that run is
  stopped, so the application can still be run again. Migration: code that
  runs an application again after it returned must not call `Shutdown` a
  second time in between, or the next run stops at once.

- **The test client serves the application through the application's own
  server.** `testclient.New` served through httptest, so none of
  `ServerOptions` applied: a 100 KB header that production answers 431 was
  answered 200, `BaseContext`, the timeouts and the HTTP/2 settings never
  reached the server under test, and `App.Shutdown`, with the drain that ends
  event streams and WebSockets, found no run to stop. The client now serves on
  a loopback port through the same run path `App.Run` uses, in plain HTTP
  whatever TLS is configured, and the test's cleanup shuts the application
  down as `App.Shutdown` does. A test may call `App.Shutdown` itself to assert
  on a shutdown. Migration: an App is served by one client at a time, so a
  test that created two clients for one App must build one App per client; a
  build failure is now reported as "the application could not be served".

- **A query, header or form parameter that holds one value is refused when
  sent more than once.** The binder took the first value, where FastAPI and
  most proxies take the last, and a proxy or firewall that judged the other
  copy passed a request the handler then served with a value it never saw. A
  422 now reports `must be given only once` (`muzak.binding.repeated`). A
  slice field still collects every value. Cookies are unchanged, because a
  browser legitimately sends two cookies of one name for different paths.
  Migration: send the parameter once, or declare the field as a slice to
  accept a list.

- **A number parameter must be written as JSON writes it.** Path, query,
  header, cookie and form numbers were read by `strconv`, which took
  `0x1p-2`, `1_000`, `.5` and a leading `+`. None of those is accepted in a
  JSON body or described by the document's `type: number`, and a proxy reading
  them differently from the binder could be told one number and the handler
  another. Leading zeros are still accepted. `LoadConfig` reads numbers the
  same way. Migration: send plain decimal numbers.

- **A located field is never a member of the JSON body.** In an input that
  mixes path, query, header or cookie fields with a body, a body member naming
  a located field was accepted and silently dropped (`{"ID":999}`), or failed
  as a member the schema does not list (`{"ID":"zz"}`). The body is now
  decoded into a struct of the body members alone, so such a member is an
  unknown member: a 422 `is not a field this endpoint accepts`, or ignored
  under `AllowUnknownFields`. Migration: stop sending located values in the
  body, or turn on `AllowUnknownFields` for the route.

- **A body type encoding/json/v2 refuses is a build error.** An embedded
  struct tagged `json:",omitzero"`, a `format:` option, two fields claiming one
  name, a channel, function, complex or non-empty-interface field, or the
  `string` option on a non-number used to build. Every request that reached it
  then got a 422 blaming the client, usually against an empty field, with
  nothing logged. Such a failure that can still only show up at request time,
  such as an embedded pointer to an unexported struct, is now a logged 500.
  Migration: fix the tag or the type the build error names, or tag the field
  `json:"-"`.

- **A header bound to a slice is split on its commas.** RFC 9110 says
  `X-Ids: 1, 2` and two `X-Ids` lines mean the same thing, but the binder read
  only the lines, so the one-line form was a single element and a 422 for
  `[]int`. Commas inside quoted strings are kept. Migration: a `[]string`
  header whose values contain commas now gets several elements; declare a
  `string` field to read the line whole.

- **`Email()` requires the domain to be a hostname.** net/mail reads the
  domain as a dot-atom, which let through `a@-`, `a@foo_bar.com` and `a@b=c`.
  Labels must now be letters (in any script), digits and hyphens, with no
  hyphen at either end. A bracketed IP address is still accepted. Migration:
  none for a deliverable address.

- **An integer out of range names the range.** `?n=300` for an `int8`, and
  the same value in a JSON body, now read `must be between -128 and 127`
  (`muzak.binding.range`) instead of `must be a valid integer`. Migration: a
  client matching on the old text matches on the code and field instead.

- **Quotas are counted longest window first, and a refused request stops at
  the quota that refused it.** Every quota of a policy used to be counted for
  every request, refused ones included. A request refused by a short burst
  quota has still been counted against the longer ones, so pausing between
  bursts launders nothing, as before; a request a longer quota refuses is no
  longer counted against the shorter ones, and `Retry-After` is the wait of
  the quota that refused. Declaration order no longer matters, and
  `RateLimit-Policy` still lists quotas as declared. WebSocket
  `MessageLimits` use the same order. Migration: none for most applications;
  a custom `RateLimitStorage` sees fewer `Increment` calls, and a test that
  asserted the order of storage calls or that a refused request reached every
  quota needs updating.

- **A wildcard CORS policy sends `Access-Control-Allow-Origin: *` on every
  response.** It was sent only to a request carrying an `Origin`, with no
  `Vary: Origin`, so a browser or shared cache that kept the answer to a plain
  navigation (a `Static` or `Frontend` file with its `Last-Modified` above all)
  handed it to a later cross-origin fetch, which the browser refused. The
  header and `Access-Control-Expose-Headers` now go on every response, which
  is what makes leaving `Origin` out of `Vary` true. Migration: none;
  same-origin responses carry two more headers.

- **An `AllowedOrigins` entry that can never match, and `"null"`, are build
  errors.** An entry is compared exactly with the `Origin` header, so
  `https://app.example.com/`, a path, `HTTPS://App.example.com`,
  `https://app.example.com:443`, `*.example.com` or an entry with no scheme
  was accepted and then allowed nobody, and `null`, which a sandboxed iframe,
  a `file:` page or a cross-origin redirect sends, would have allowed any page
  that arranged to send it. Each is now reported, joined with the other build
  errors, with the spelling to use instead. Migration: write each origin as
  `scheme://host[:port]` in lower case with no path and no default port, move
  wildcard subdomains to `AllowOriginFunc`, and remove `null`.

- **`WSOptions.AllowedOrigins` and `AllowedHosts` are held to the same rules.**
  A WebSocket route's origin list took any string, so an entry with a
  trailing slash, a path, a pattern or the default port left the browser it
  was meant for refused with no reason given, and `null` let any page that
  arranged to send it open a connection with the visitor's cookies, which is
  the hijacking the origin check exists to stop. Each such entry is now a build
  error naming the route, as it is for CORS, except that case is no mistake
  here, since the WebSocket list is compared without regard to it, and the
  single entry `*` keeps meaning any origin. An `AllowedHosts` entry carrying
  a scheme, a path, credentials or a pattern, which can never equal a Host
  header, is refused too. Migration: as for CORS above; write a host as
  `host[:port]`.

- **`SecurityHeaders` sends `Strict-Transport-Security` over TLS.** A server
  answering over TLS never told a browser to stay on HTTPS. A response to a
  request that arrived over TLS now carries
  `Strict-Transport-Security: max-age=31536000`, without `includeSubDomains`
  or `preload`, and never for `localhost`, a `.localhost` name or an IP
  address, since the header covers every port of a host and would otherwise
  push every other local development server onto HTTPS. Behind a proxy that
  terminates TLS nothing changes. A handler's own value still wins. No
  `Content-Security-Policy` is set: one strict enough to matter would break
  pages served with `muzak.HTML`. Migration: a host that also serves plain
  HTTP on another port, or may leave HTTPS, removes the header in middleware
  installed with `App.Use` (or sends `max-age=0`); to add `includeSubDomains`
  or `preload`, set the header yourself.

- **A read or write that the handler's own context ends tells the WebSocket
  peer with a close frame.** The usual way to bound how long a peer may stay
  silent, a deadline on each read, ended the connection with no close frame,
  so the peer saw `1006` with no reason. A read whose deadline passes now
  closes with `1008` ("no message arrived within the time allowed"), the
  status the read timeout and keepalive use. A cancelled read closes with
  `1001`. A write whose context had already ended when it got the connection
  sends nothing of the message and closes with `1001`; before, it raced the
  cancellation and the message could still go out. A write interrupted part
  way through a frame still ends without a close frame, since one would be
  read as more of the frame. The read returns the context's error after the
  close handshake, so up to `CloseGracePeriod` (250 ms by default) after its
  deadline. A `WSDial` connection cannot send a close after a cancelled read,
  because the cancellation closes its transport. Migration: a peer or test
  that expected the end of the stream after an idle timeout now receives
  `1008` or `1001` first. Set `CloseGracePeriod` negative if a read must
  return the moment its deadline passes.

- **A WebSocket handler that returns its own context's error is logged at
  debug level, not as a failure.** Returning the error from a read, write or
  wait that the handler's context ended, or `ctx.Context().Err()` once that
  context has ended, used to be logged at `ERROR` as "a websocket handler
  failed" and closed with `1011`. It is now logged at debug as "muzak: a
  websocket connection ended", and a connection still open is closed with
  `1001`. A deadline that ran out anywhere else, on a query for instance, is
  still a failure. Migration: alerts keyed on "a websocket handler failed" no
  longer fire for idle timeouts.

- **`Compress` takes the `W/` off the tags of an incoming `If-Match`.**
  Weakening the entity tag of a compressed response meant a client that
  accepts gzip held a weak tag, which `If-Match`'s strong comparison never
  matches, so every conditional write it made got 412. A tag the middleware
  weakened is the strong tag of the same content, so the strong form is now
  restored before the handler compares. For a handler that compares as RFC
  9110 requires, this only turns a guaranteed failure into the right answer.
  Migration: a handler that issues weak tags of its own and compares
  `If-Match` by string equality, which RFC 9110 does not allow, now sees the
  strong form. Compare the opaque tag instead.

- **A compressed response no longer advertises `Accept-Ranges`, and a date in
  `If-Range` gets the whole content.** A download resumed with
  `If-Range: <Last-Modified>` was answered with an uncompressed 206, which a
  client could splice onto the compressed bytes it already held. When an
  encoding is negotiated, `Range` with an `If-Range` that is not a strong tag
  is now answered with the full representation. A `Range` without `If-Range`,
  or with a strong tag, is still answered with an uncompressed 206. Migration:
  a client that resumes through `Compress` sends a strong entity tag in
  `If-Range`, or asks for no encoding along with its `Range`, as Go's client
  and most download tools already do.

- **A file named with a trailing slash is a miss.** `/page.html/` and
  `/assets/app.js/` served the file under a second URL, against which its
  relative references resolve elsewhere. They are now answered as any path
  with no file behind it: 404, the NotFound page, or the fallback for a
  navigation. A directory is still served with or without the slash.
  Migration: link to files without a trailing slash.

- **A request body left to an event stream handler is read under
  `ServerOptions.ReadTimeout`.** See **Fixed** for why the handler can now read
  it at all. Until that body has been read to its end, net/http cannot watch
  for the client going away, so a disconnect is noticed when a write fails
  rather than at once. Migration: read the body before sending the first
  event. A handler that streams a request body for longer than `ReadTimeout`
  over HTTP/2, which used to work, raises the timeout or calls
  `http.NewResponseController(ctx.ResponseWriter()).SetReadDeadline(time.Time{})`.

- **A plural rule reads a count as CLDR's operands rather than as an `int`.**
  `i18n.PluralRule` is now `func(i18n.PluralOperands) i18n.PluralCategory`, and
  so is `EnglishPluralRule`. A rule over an `int` could not be asked about
  1.5, so a fractional count was cut to its whole part first (see **Fixed**).
  Migration: write a rule supplied through `StoreOptions.PluralRules` as
  `func(n i18n.PluralOperands) i18n.PluralCategory`, reading `n.I` where it
  read `n` and checking `n.V == 0` for a whole count. To call a built-in rule,
  use `ops, _ := i18n.PluralOperandsOf(count)` and then
  `i18n.PluralRuleFor(locale)(ops)`.

- **`InvalidPluralizationDataError.Count` holds the count as it was given.**
  It was an `int`, which cannot hold 1.5. It is now `any`: an `int` from
  `Lookup.Count`, or whichever numeric type was passed to `Translate`.
  Migration: comparing it with an integer constant still compiles, and works
  for an `int` count; type-assert it before doing arithmetic.

- **A count that is not a finite number below 2^63 in magnitude is refused.**
  NaN, the infinities, floats of 2^63 or more and `math.MinInt64` were
  narrowed to an `int` whatever that made of them, so NaN counted as whatever
  the platform's conversion produced. They are now an `ArgumentError`
  (`ErrMalformedArguments`), whose message says the count was the problem
  rather than naming "argument -1". Migration: none for a count of anything
  real.

- **Marathi counts zero as plural.** `mr` was listed with the languages that
  put zero in `one`. CLDR gives it `one: n = 1`, which is the English rule.
  Migration: a Marathi `one` form that was written to read well for zero
  should become a `zero` form, which still wins for a count of zero.

- **A request body is described as refusing the members the decoder
  refuses.** The decoder rejects an unknown member at every depth unless the
  route was registered with `AllowUnknownFields`, but the request schemas said
  nothing about it, so a client generated from them, or a gateway validating
  against them, passed what the server answers with a 422. Every object a
  request is read into, nested ones included, now says
  `additionalProperties: false`; one that collects unknown members in an
  embedded map says what it takes instead, and responses are left open so that
  adding a member later breaks no client. Because a response's component
  cannot say this, a type used both as a request and as a response is now
  always described twice, as `Item` and `ItemInput`, even where the two used
  to agree. Migration: regenerate clients; request types may gain an `Input`
  copy, and a client that sent extra members learns from the document what the
  server already refused.

- **Objects nested in a request body require only what a rule enforces.** The
  top level has followed this since 0.2.8, but nested types kept the shape a
  response has, so a client was told to send members the server never asked
  for. A nested object now requires what the rules of a model nested with
  `Nested`, or a rule on a nested path, require of it, and nothing otherwise.
  A nested type a response also uses gets a request copy (`OwnerInput`) that
  the request's copy of its parent refers to, so the response's component
  keeps its shape. Migration: a client generator types unruled nested request
  members as optional; declare `Required()` on the ones that must be sent.

- **`omitempty` is read the way json/v2 reads it.** json/v2 leaves out only
  what encodes as null, `""`, `{}` or `[]`, so a number, a bool, a time and a
  non-empty array tagged `omitempty` are always written. Response schemas now
  list them as required. Migration: a generated client may type those members
  as non-optional.

- **A model nested by embedding reports its failures under its members' own
  names.** An embedded struct is flattened into its parent on the wire, but a
  failure inside a model nested that way was reported as `code.code`, a path
  no client could find in the body. It is now `code`, the member's name, as
  the decoder and the document both have it. Migration: a client matching on
  the old `field` value matches on the member's name.

### Security

- **A URL rule no longer approves a URL that names no machine.** `URL()`,
  `HTTPS()` and `URLWithSchemes()` checked `url.URL.Host`, which is `":8080"`
  for `http://:8080/admin`, and Go's HTTP client dials an empty hostname as the
  local machine. So a value approved as somewhere else on the network reached
  a port on the server itself, which is the request forgery a URL check is
  meant to stop. The rules now require a hostname. A scheme with no
  authority, such as `mailto:`, still passes on what follows its colon. The
  rule checks the form of a URL, not where it leads: a hostname that resolves
  to a private address is still the application's to refuse.

- **A key minted under one quota can no longer reset another quota's
  counter.** The 0.2.8 fix took the counter to discard from the quota holding
  the most counters, which held only while the flooded quota was the largest.
  A client that spent an application-wide per-address quota early, so that
  its counter was the oldest there, could mint fresh keys under a smaller
  quota keyed on something it chooses (an API key or a username counted before
  any guard), and each key evicted the oldest counter of the larger quota, its
  own: 31 keys took its count from 11 back to 1 against a limit of 10. A full
  table now makes room from the quota being counted; only a quota holding no
  counter takes the one it needs from the largest. A full table therefore
  keeps each quota at the size it had when it filled, so size `MaxEntries` for
  the keys the application really sees.

- **A refused request no longer creates rate-limit counters.** Every quota
  after the one that refused a request still counted it, so a client being
  answered 429 kept minting counters under keys those quotas had not seen,
  which made the eviction above cheap. The count now stops at the refusing
  quota; see **Changed**.

- **A dotenv comment after an empty value no longer satisfies
  `required:"true"`.** `API_KEY= # fill me in`, the way a template `.env`
  leaves a setting to supply, loaded the comment text as the key, which
  satisfied `required:"true"` and defeated the check that refuses an empty
  required value. A tab before the `#` did not start a comment either. In an
  unquoted value, a `#` after a space or a tab now starts a comment wherever it
  is, including right after the `=`. A `#` with no whitespace before it, as in
  `COLOR=#336699`, is still part of the value, as in a POSIX shell.

- **Redaction reaches the entries of a logged header map.** Matching was by
  attribute key alone, so `slog.Any("headers", r.Header)`, the case the
  redaction list was documented to exist for, wrote `Authorization` and
  `Cookie` in full in both the JSON and console formats. The entries of an
  `http.Header`, `url.Values`, `map[string][]string` or `map[string]string` are
  now matched by their own keys, and a matching value is replaced in a copy,
  leaving the caller's map untouched. A struct, a pointer or any other map is
  still not looked inside, and the documentation now says so instead of
  implying a request struct was covered.

- **An oversized `Sec-WebSocket-Key` no longer buys an allocation.**
  `wsAcceptKey` decoded the key and only then checked it was 24 characters,
  contrary to its own comment, so a handshake carrying a key the size of the
  header limit cost an allocation three quarters that size on every attempt
  that reached the route, which on a public route is anyone's. The length is
  now checked first.

- **A locale file can no longer multiply a long key into hundreds of
  megabytes.** The YAML reader bounds how many values aliases copy, but
  flattening then stored one full dotted path per value. A 4.5 KB file with a
  4 KB key above 78,000 aliased values stayed inside that budget and was
  retained as about 394 MB, and the cost grew with the length of the key.
  JSON, which has no aliases, paid the same way, quadratically, for a long key
  above many short values, and alias copies of a text full of placeholders
  were each compiled separately. What a locale costs to hold once flattened
  (paths, texts, compiled placeholders and entries) is now reckoned before
  anything is built, and a locale, or the locales of one file together, past
  64 MiB is refused and the locale is left as it was. This applies to
  `LoadFile`, `Load` and `StoreTranslations` alike. The 4.5 KB file now costs
  about what parsing it does, roughly 6 MB, and the locale Muzak ships comes
  to under a tenth of a megabyte. A new fuzz target follows locale files into
  the store. Migration: none for a real file.

### Fixed

- **The context a lifecycle component was started with stays live through a
  run's shutdown drain.** `RunContext` started the components with a child of
  its own context, and cancelling that context is how `RunContext` is asked to
  stop, and how `RunSignals` answers SIGTERM. A worker a component kept on its
  start context was therefore cancelled the moment the drain began, while
  requests that still used it were being served, and `Stop` was called on a
  context that had already ended. That broke the promise that it stays live
  until the components are stopped. The start context now keeps the run
  context's values without its cancellation, is cancelled by it only while the
  components are still starting, and ends after `Stop`.

- **`Context.Logger` carries the request.** It was documented to attach the
  method, path and request identifier, and returned the application's logger
  with none of them, so a line a handler wrote could not be joined to its
  request's access log line. Every record it writes now carries `method`,
  `path` and `request_id`. The logger is built the first time it is asked for,
  so a request that never logs allocates nothing more, and it holds nothing of
  the pooled Context, so a goroutine the handler starts may keep it.

- **A handler that hijacks the connection is no longer logged as a failure.**
  Hijacking through `http.NewResponseController(ctx.ResponseWriter())`, as
  `Context.ResponseWriter` documents, reached net/http's writer past every
  wrapper, so none of them knew. The framework then wrote its response onto
  the hijacked socket, net/http warned about a `WriteHeader` on a hijacked
  connection, the failed write was logged at error level as an aborted
  request, and the access log recorded `status=200 aborted=true`. The response
  writer, and the one `Compress` installs, now implement `Hijack` and mark the
  whole chain beneath them, as the WebSocket route already did by hand.
  Nothing is written afterwards, and the access log records the request with
  status 101.

- **An error renderer's status is clamped when it returns no body.** A status
  returned with a body was clamped to 100 to 599, but one returned with a nil
  body was written as it stood. A renderer that left the status at 0 therefore
  made net/http panic, and the client lost its connection instead of receiving
  an error. It is now written as 500.

- **A model is not validated when its body failed to decode.** The decoder
  stops at the first member it cannot read, and the rules ran on what was
  left. Members the client had sent were reported missing, and with a located
  field beside the body every required member was. A `Must` check judged a
  value nobody wrote. Parameter failures are still reported beside the body's,
  and the model's rules run once the body decodes.

- **A body decode failure names the member by its full path and says what was
  wrong.** `addr.zip` and `items[1].zip` were both reported as `zip`, which
  disagreed with validation and defeated its deduplication. A time that did
  not parse read `a string is not accepted here`, and 300 for an `int8`, or 1.5
  for an `int`, read `a number is not accepted here`. The issue now follows
  the field's type, with the same wording and translation keys a parameter
  uses. Every decode message, including `is not a field this endpoint accepts`
  and `is not valid JSON`, is now translatable.

- **`NumberRules` compare integer fields exactly.** Values went through
  `float64`, so above 2^53 a bound was off by one: `Max(1<<53)` admitted
  2^53+1, and `MultipleOf(2)` admitted an odd number. `Clamp` truncated a
  fractional bound and wrapped round on a bound the field type cannot hold,
  so an `int8` clamped to 200 read -56; it now saturates at the type's range.
  The documentation already promised exact comparison. `Must` and a
  fractional `MultipleOf` still see a `float64`, as documented.

- **A mixed input with an unexported embedded struct of body members no
  longer fails with a 500.** Copying the struct out of the scratch value it
  was decoded into panicked.

- **A third type of the same name no longer takes over a second's schema.**
  The package-qualified fallback name was handed out without checking it was
  free, so three route-local `response` types, or `models.Item` in three
  packages, left `/b` pointing at `/c`'s schema and every client generated
  from the document typed one response as another. Names are now qualified by
  as much of the import path as tells the types apart (`models.Item`,
  `v3.models.Item`), then numbered, and are the same on every run.

- **A rule on the first member of a nested struct is reported and documented
  as that member's.** Fields were told apart by their offset alone, and
  `&in.Price.Amount` shares its offset with `&in.Price`, so a failure was
  reported as `price`, and `exclusiveMinimum` was written onto the reference
  every use of `Money` shared, `refund` and responses included. The same rule
  on the second member was refused at build. Fields are now told apart by
  their type as well as their offset, the members of a struct held by value
  are named by path (`price.amount`), and the rule is written beside the
  reference for that member alone. A struct behind a pointer still needs
  `Nested`.

- **The `string` and `embed` tag options and byte arrays are documented as
  json/v2 encodes them.** `json:"n,string"` was described as a number, though
  the server reads only a quoted one; it is now a string with the pattern
  json/v2 accepts. An `embed` field's members were described under the field's
  name instead of at the top level, and a `[N]byte` as an array of integers
  instead of base64 (`contentEncoding: base64`, with its exact length). A
  slice of a named byte type is an array, as json/v2 writes it, and `[]byte`
  carries `contentEncoding` too. The document now reads a struct through the
  same field resolution json/v2 uses, rather than a walk of its own.

- **Embedded members that share a name are resolved as json/v2 resolves
  them.** An outer `ID string` beside an embedded `ID int` was documented as an
  integer and listed in `required` twice, which is not valid JSON Schema, while
  the wire carried the string. The shallower member wins, a tie at one depth
  is decided by a tag or drops the name, and `required` names each member
  once.

- **A nullable member held to a list of values admits null.** A nil pointer
  skips its rules, so `{"status":null}` was accepted while
  `type: ["string","null"]` beside `enum: ["open","closed"]` refused it. The
  list now includes null unless a `Required()` rule refuses it.

- **Every rule a field is held to is documented.** A second `v.String` on the
  same field replaced the first's description wholesale, dropping its pattern,
  its bounds and even `Required`, and within one chain the last step of each
  kind replaced the others, so `Min(10).Min(5)` was documented as a minimum of
  5 and two `OneOf` lists as one that allowed values neither did. All of them
  are now combined: the tightest bound of each kind, only the values every
  list allows, and more than one pattern, format or multiple under `allOf`.
  `validate.Constraints` gains `AllOf` to carry them.

- **Documentation assets load under a `DocsPath` that needs encoding.** The
  assets were keyed by the encoded path and matched against the decoded one,
  so `/api docs` served the page and a 404 for each of its scripts.

- **The 503 for a rate-limit storage that could not count carries
  `Retry-After: 5`.** Every client refused because the store had stopped
  answering was free to retry at once against a store trying to recover. The
  header is the same five seconds the connection caps send, is set even with
  `DisableHeaders`, and outlives the reset an error response gets.

- **Keepalive no longer closes a WebSocket peer that is still sending a
  message.** A peer part way through one large frame cannot send its pong
  until the frame is finished, and the keepalive waited for the pong alone, so
  a healthy peer whose message took longer than `PongTimeout` to arrive was
  closed with `1008` while its bytes were still coming in. With the defaults
  that was any 1 MiB message sent at under about 100 KB/s, closed ten seconds
  in although `ReadTimeout` allowed thirty; 0.2.8 had investigated a slow
  reader and missed a slow sender. Every frame header and every read that
  brings payload now counts as hearing from the peer, so only a peer from
  which nothing at all has arrived for a pong timeout is closed. One that
  stalls part way through a frame is still closed by the keepalive, and by
  `ReadTimeout` in any case.

- **A WebSocket handler's reads and writes no longer set up a context
  arrangement on every call.** The connection watched the request's context
  and handed the handler a child of it, so `ctx.Context()` never matched and
  every read and write registered a `context.AfterFunc` with its own mutex and
  closure: 6 allocations and 224 bytes per `WriteBinary`, against none for
  `context.Background()`. The handler's context is now the one watched, and
  the keepalive pings with it. `BenchmarkWebSocketHandlerWrite` measures
  writes from a real route, which the existing benchmarks, all on
  `context.Background()`, could not see.

- **`WSDial` connections wait for the server to hang up first, and
  `WSDialOptions.CloseGracePeriod` does what it says.** The dialled transport
  is the body of the `101` response, not a `net.Conn`, so the closing wait
  returned at once and the client closed TCP first, against RFC 6455 section
  7.1.1, whatever the grace period said. The same happened after the client
  echoed a close the server started. The grace period is now enforced by
  closing the transport when it runs out, as the read timeout already is. The
  client does not half-close, and it reads on to the end of the stream, so
  `Close` returns once the server has hung up, or after `CloseGracePeriod` at
  the most.

- **An event stream handler can read its own request body over HTTP/1.1.**
  `SSEHandle` leaves a body its input does not bind to the handler, as its
  documentation says, but the stream writes its header before the handler
  runs, and net/http drops an unread HTTP/1.1 body at that moment. Under
  256 KiB the handler read `http: invalid Read on closed Body`, and a larger
  body cost the connection. The stream now opens in full duplex mode when a
  body is left to the handler. It also keeps the request's read deadline
  until net/http takes it off at the end of the body: a client that declared a
  body and sent only part of it used to hold the opening of the stream, with
  no deadline at all, for as long as it liked.

- **`SSEReader.LastEventID` moves only when an event completes.** An `id:`
  field set it the moment it was read. The HTML specification keeps it in a
  buffer that becomes the last event ID only when the blank line dispatches
  the event, and Muzak's server writes `id` before `data`, so a stream cut off
  mid-event, by a dropped connection, the read timeout or the read limit,
  moved the ID past an event that was never delivered, and a reconnect lost
  that event. The buffer persists across events as it does in a browser, and
  an `id` on an event without data is still applied once that event
  completes.

- **A closed `SSEReader` delivers nothing more.** `Close` ended the
  connection, but events and comments already in the reader's buffer were
  still handed out by later calls to `Next`, so a loop that closed the reader
  could go on receiving, depending on how the bytes had happened to arrive.
  `Close`, a cancelled read and an expired `ReadTimeout` now end the reader at
  once, and `Next` reports `ErrSSEStreamEnded` even for what was buffered.

- **An event stream is always typed `text/event-stream`.** The type was set
  only when nothing else had set one, so a middleware that typed every
  response as `application/json` in advance served the stream under that
  type, which `EventSource` refuses to read.

- **A 304 from `Compress` carries the entity tag in the form the 200 it
  revalidates carried.** The compressed 200 carried `W/"v1"` and the 304
  carried `"v1"`, and under RFC 9111 section 4.3.4 a cache cannot use a strong
  304 to refresh a response stored under a weak tag. The tag is now weakened
  on a 304 whenever an encoding was negotiated. That still matches a stored
  uncompressed response, because tags correspond by weak comparison.

- **A count with a fraction chooses its plural form the way CLDR says.** The
  count was cut to an `int` first, so `T("en", "km", "count", 1.5)` read "1.5
  kilometre". Czech never reached its `many` form for a fraction, and Latvian,
  Icelandic and Hebrew never reached `one` for 0.1 or 0.5. Every built-in rule
  is now stated over CLDR's operands, and languages that agreed on whole
  counts but part on fractions have rules of their own: `hindi`
  (`i = 0 or n = 1`) and `punjabi` (`n = 0..1`) sit beside `one_other_zero_one`
  (`i = 0,1`, French and Portuguese), and `belarusian` and `macedonian` beside
  `slavic` and `icelandic`. A zero form is still used for 0 and 0.0, but not
  for 0.5. Whole counts are read without formatting or allocation, and a
  counted message makes one allocation fewer, because the count no longer
  travels behind a pointer. `Lookup.Count` stays an `*int`; pass a fractional
  count to `Translate`.

- **European Portuguese counts zero as plural.** `pt-PT` fell back to the rule
  for `pt`, which CLDR states as `i = 0..1`, so a count of zero read "0
  ficheiro". `pt-PT`'s own rule, `i = 1 and v = 0`, gives "0 ficheiros".
  `pt` and `pt-BR` are unchanged.

- **`Store.LoadFile` applies the plural rule a file names.** It stored the
  translations and stopped there, so a file's `i18n.plural.rule` was ignored,
  and its locale had no fallback chain worked out, until a later `Load` or
  `StoreTranslations` happened to refresh them. It now refreshes as `Load`
  does. `Load` and `LoadFile` also refresh when only some of the files or
  locales could be stored, so what did load counts by its own rule.

- **`testclient.Cookie` sends only the cookie's name and value.** It sent
  `cookie.String()`, the `Set-Cookie` form, so a cookie with a `Path` reached
  the server as `a=b; Path=/` and was read as two cookies, one named `Path`.
  Each option also added a `Cookie` field of its own, and the second was
  dropped once the jar added a cookie. All cookies now go into the one
  `Cookie` header a request carries.

- **`testclient.Header("Host", ...)` sets the host the request is sent for.**
  net/http writes the Host line from `Request.Host` and drops a `Host` header
  without a word, so a test of host-based routing or of
  `WSOptions.AllowedHosts` tested the loopback address. The header is now
  applied, for ordinary, WebSocket and event stream requests and through
  `WithHeader`, and is kept across a redirect that stays on the same server.

### Documentation

- **`SSEOptions` and `WSOptions` no longer call their zero value safe, and
  state how few clients take every slot.** A client that reads nothing is
  never ended by an event stream's `WriteTimeout`, since a keepalive never
  fills a socket buffer (the 0.2.8 notes below said otherwise), and a
  WebSocket peer that answers every ping is alive as far as a keepalive can
  tell; `MaxLifetime` is unset by default for both. So `MaxStreams /
  MaxStreamsPerIP` addresses fill an application's event streams, and
  `MaxConnections / MaxConnectionsPerIP` its WebSockets: 1024 / 64 = 16 with
  the defaults, which is sixteen IPv4 addresses or a single IPv6 /52. Every
  other client is then refused with 503 until they let go. `SSEOptions`, its
  fields and the package documentation, and `WSOptions`, now give that
  arithmetic and the settings that change it: authenticate with a guard, raise
  the total and lower the per-address cap, count IPv6 clients by their /48,
  and set `MaxLifetime` to minutes.

- **A `{name...}` wildcard receives unnormalised spellings of the routes
  beside it.** With a guarded `/admin/panel` and a public `/{rest...}`,
  `//admin/panel`, `/admin//panel`, `/admin/panel/`, `/ADMIN/panel`,
  `/./admin/panel`, `/x/../admin/panel`, `/%2e/admin/panel` and
  `/admin%2Fpanel` reach the wildcard, the last with `rest` set to
  `admin/panel`. The guarded route is never served unguarded, as with
  `net/http.ServeMux`. `Router.Get`, `Router.Handle` and the package
  documentation now say so: a handler that serves files, fetches records or
  proxies by the captured value checks authorization against what it refers
  to, and never forwards it to a backend that cleans, decodes or case-folds
  paths.

- **`SSEEvent` says when an event is dispatched.** It was documented as
  dispatched with empty data in its zero value. In fact the zero value is
  written as a blank line and a name-only event without a data field, and
  neither a browser nor `SSEReader` dispatches either. The documentation now
  says an event is dispatched only when it has a data field, and points at
  `EmptyData`.

- `MemoryRateLimitOptions.MaxEntries` says a full table keeps each quota at
  the size it had when it filled, `SecurityHeaders` says why it sets no
  `Content-Security-Policy`, and `AllowUnknownFields` says a member naming a
  located field is an unknown member too.

### Tests and continuous integration

- **CI runs on Windows.** A `test (windows)` job builds, vets and runs the
  suite, with and without the race detector, on `windows-latest`, and builds
  the example. It is the first place the code only Windows reaches runs at
  all: a frontend treating a backslash as a separator and refusing 8.3 short
  names, `os.Root` on NTFS, and the timing of a coarser clock. It checks out
  with line endings as committed, because that is what the module proxy
  serves. The coverage gate stays on Linux.

- **Three tests that failed only on a loaded machine no longer do.** One
  counted calls through a package-level counter that a parallel test also
  advanced, one compared two error messages that quoted a bound read off the
  clock a second apart, and one asserted that a closed event stream ends while
  the reader still handed out what it had buffered (see **Fixed**).

### Known limits

The review found these and left them, each with the reason.

- `SSEOptions.MaxLifetime` and `WSOptions.MaxLifetime` are still unset by
  default, so the arithmetic under **Documentation** holds for an application
  that sets neither. A lifetime ends connections that a client may not
  reconnect on its own, most WebSocket clients among them, which is a decision
  for the application rather than a default. Nor would one stop a client that
  holds slots on purpose, since it reconnects at once; a guard that
  authenticates does, and the documentation now says so.
- A cookie sent twice under one name still binds its first value: a browser
  sends the one with the most specific path first (RFC 6265 section 5.4), so
  the first is the one meant for the path asked for, and refusing a repeat
  would break real sessions. A boolean parameter still accepts the spellings
  `strconv.ParseBool` does (`1`, `t`, `T`), where a JSON body accepts only
  `true` and `false`, because `?debug=1` is how a query string is commonly
  written and FastAPI accepts it too.
- `Lookup.Count` is still an `*int`, so `Store.Get` cannot be given a
  fractional count; `Translate` can. The CLDR `e` and `c` operands are not
  implemented, which leaves the `many` form French, Spanish, Italian,
  Portuguese and Catalan use for millions falling back to `other`, as before.
- Counts already made against longer quotas before a refusal are not taken
  back, because a refused request is still a request and undoing a count would
  need a second storage call that could itself fail.

## [0.2.8] - 2026-09-29

This release is the result of four rounds of adversarial review of the
framework, run as an attacker would against a service built on it, each later
round also against the earlier rounds' fixes, and of testing on a real network
and on Cloud Run. Every finding below was reproduced with a test before it was
fixed, and each of those tests is now a regression test. Several fixes tighten
a default; each one is listed under **Changed** with its migration.

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

- **File a router under a category, and give a route a title, so a large API
  stays readable in the documentation.** With a hundred routers the tag tree in
  a docs sidebar is unreadable, and a tag is the wrong tool: it names what an
  operation is about, and several add up. `muzak.WithCategory("Billing")` files
  a router, or a single route, under one heading. It is a `SharedOption`, so it
  works at both levels: every route beneath a router inherits its category, many
  routers may share one, and a router included into another, or a route itself,
  replaces the category it would have inherited, since a category is a single
  value and never a list. `muzak.Title("Fetch User Profile")` is a `RouteOption`
  next to `Summary` giving a route a human name, for a documentation tool to
  list in place of the endpoint's path. Both reach the OpenAPI document as
  vendor extensions on the operation, `"x-category"` and `"x-title"`, emitted
  only where one was set, and are exposed as `Route.Category` and `Route.Title`;
  generic, WebSocket and event stream routes carry them alike. A category is not
  a tag, and tags behave exactly as before. The document also carries
  a top-level `"x-categories"` array naming every category some operation in it
  has, each once, in the order they were first registered: a route counts where
  it was declared and a router included into another counts where the `Include`
  call was made, so a hundred routers list in the order the application
  mounted them and not in the sorted order of their paths. A category met only
  on a hidden route is left out. Nothing changes for an application that sets
  neither: the document carries neither extension nor the list. Both are checked when the
  application is built, and the error names the route or the router (by its
  mount prefix): after trimming surrounding whitespace the value must not be
  empty, must be valid UTF-8 of at most 64 characters for a category and 120 for
  a title, and must hold no control character, line break or bidirectional
  control. Titles need not be unique. The `example/` application now files its
  routers under categories and titles some routes.

- **`ServerOptions.UnencryptedHTTP2` accepts HTTP/2 without TLS.** It is for a
  server behind a platform that speaks HTTP/2 to the container in the clear,
  such as Cloud Run with an `h2c` port, where a request is cancelled the moment
  its client leaves; over HTTP/1 the platform's proxy keeps the connection to
  the container open and a handler runs on. `App.Run` and `App.RunSignals`
  could not serve it before. See `docs/cloud-run.md`, which records what was
  measured on Cloud Run: the proxy's peer address and `TrustedProxies`, HTTP/1
  against `h2c`, timeouts, streams and rollouts.

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

- **An IPv6 client is counted by its /56.** `IPTracker` and the per-client
  connection caps (`MaxConnectionsPerIP`, `MaxStreamsPerIP`) keyed on the exact
  address, so one subscriber got a fresh budget from every address it held. A
  /56 is what providers delegate to one home or small site (RFC 6177): rotating
  the low bits of the fourth group of an address, all inside its own /56,
  turned a limit of 5 a minute into more than a thousand.
  `ClientIPOptions.ConnectionIPv6Prefix` (default 56) sets the prefix both
  count, so one subscriber has one budget however it is limited. IPv4 is still
  counted exactly and its keys are unchanged; IPv6 tracker keys are now
  `ip:<prefix>/56`.

  Migration: a counter kept in a shared storage under an IPv6 client's old
  exact-address key is not carried over, so IPv6 clients start with a full
  budget once after the upgrade. Raise the limit, or set
  `Tracker: IPPrefixTracker(32, 128)`, if a proxy in front already bounds
  per-address traffic. Where many unrelated users share one /56
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
  new keys -- easily done from a single IPv6 allocation with the default tracker --
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

- **One IPv6 allocation can no longer take every WebSocket or SSE slot.** A home
  /56 held 256 separate allowances, one per /64, and now counts as one client.

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

- **An event stream keepalive arrives every interval.** It waited on fixed
  ticks and skipped a tick whose last write was younger than the interval; its
  own write lands a hair after its tick, so the tick after it always skipped,
  and a keepalive asked for every 15 seconds arrived at 15, 45 and 60: twice
  the silence a proxy idle timeout was meant to be kept off with. Seen behind
  Cloud Run. The wait is now measured from the last write.

- **Nothing is written after the closing comment of a stream that reached its
  `MaxLifetime`.** A keepalive already waiting for the write lock could go out
  behind it.

- **A handler on a route with no bound input can read the whole request
  body.** The framework drained up to 4 KiB of the body before the handler ran
  whenever the route's input bound nothing from it (`Empty`, or only path,
  query, header and cookie fields), so a handler that read
  `ctx.Request().Body` itself, a proxy, an upload streamed to disk or a
  signature check over the raw bytes, lost the first 4 KiB without any error.
  Seen on Cloud Run. The drain now happens after the handler returns, which is
  still what lets a keep-alive connection be reused when the body was not
  read, and is still bounded at 4 KiB. A route that binds a body is unchanged.
  An event stream route is treated the same way, and a WebSocket handshake
  still drains first: it is refused if it carries a body, and nothing may be
  left to be taken for frames.

- **A peer still sending when a WebSocket connection is refused reads the
  close status instead of a reset.** A message over the read limit is answered
  with a close frame `1009`, but a client that kept sending, a message still
  streaming in, often saw the connection reset (`1006`) and never read it: the
  server closed the socket with unread data in its receive buffer, which makes
  the kernel send a reset and drop the close frame in flight. Seen on Cloud
  Run. It was the same after every close the server started on finding a
  fault, not only `1009`: `1002`, `1003`, `1007` and `1008` too. Once the close
  frame is out, the server now stops sending (a half-close, or the `close_notify`
  alert on TLS), keeps reading and dropping what arrives until the peer's close
  frame, the end of its stream, `CloseGracePeriod` or 1 MiB, whichever is
  first, and only then closes the transport. Nothing read is kept, a peer that
  answers the close frame promptly is not made to wait, and one that never
  stops is cut off at the grace period as before. A close the handler starts
  with `Close` gets the same half-close. In a test that sends a message over
  the limit and keeps sending, the `1009` reached the client in 0 of 50 runs
  before and 50 of 50 after.

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

[Unreleased]: https://github.com/muzak-dev/framework/compare/v0.2.9...HEAD
[0.2.9]: https://github.com/muzak-dev/framework/compare/v0.2.8...v0.2.9
[0.2.8]: https://github.com/muzak-dev/framework/compare/v0.2.7...v0.2.8
[0.2.7]: https://github.com/muzak-dev/framework/compare/v0.2.6...v0.2.7
[0.2.6]: https://github.com/muzak-dev/framework/compare/v0.2.5...v0.2.6
[0.2.5]: https://github.com/muzak-dev/framework/compare/v0.2.4...v0.2.5
[0.2.4]: https://github.com/muzak-dev/framework/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/muzak-dev/framework/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/muzak-dev/framework/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/muzak-dev/framework/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/muzak-dev/framework/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/muzak-dev/framework/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/muzak-dev/framework/releases/tag/v0.1.0
