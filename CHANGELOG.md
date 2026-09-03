# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0.0, a minor bump may carry a breaking change. Each one is listed under
**Changed** with the migration.

## [Unreleased]

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
