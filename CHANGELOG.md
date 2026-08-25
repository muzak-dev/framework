# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0.0, a minor bump may carry a breaking change. Each one is listed under
**Changed** with the migration.

## [Unreleased]

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

[Unreleased]: https://github.com/muzak-dev/framework/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/muzak-dev/framework/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/muzak-dev/framework/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/muzak-dev/framework/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/muzak-dev/framework/releases/tag/v0.1.0
