# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0.0, a minor bump may carry a breaking change. Each one is listed under
**Changed** with the migration.

## [Unreleased]

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

[Unreleased]: https://github.com/muzak-dev/framework/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/muzak-dev/framework/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/muzak-dev/framework/releases/tag/v0.1.0
