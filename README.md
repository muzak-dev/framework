<p align="center">
  <a href="https://muzak.dev"><img src=".github/assets/logo.png" alt="Muzak" width="128"></a>
</p>

<h1 align="center">muzak</h1>

<p align="center">
  <em>A type-safe web framework for Go. Your handler's input type is the request,<br>
  its return type is the response, and both are checked when you compile.</em>
</p>

<p align="center">
<a href="https://github.com/muzak-dev/framework/actions/workflows/ci.yml">
  <img src="https://img.shields.io/github/actions/workflow/status/muzak-dev/framework/ci.yml?branch=main&style=flat-square&logo=githubactions&logoColor=white&label=CI" alt="CI">
</a>
<a href="#test-coverage">
  <img src="https://img.shields.io/badge/coverage-98.7%25-3fb950?style=flat-square&logo=go&logoColor=white" alt="Coverage">
</a>
<a href="https://pkg.go.dev/muzak.dev/framework">
  <img src="https://img.shields.io/badge/pkg.go.dev-reference-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go Reference">
</a>
<a href="https://go.dev/dl/">
  <img src="https://img.shields.io/badge/go-1.27-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.27">
</a>
<a href="#no-dependencies">
  <img src="https://img.shields.io/badge/dependencies-0-3fb950?style=flat-square" alt="Dependencies">
</a>
<a href="#licence">
  <img src="https://img.shields.io/badge/license-MIT%20or%20Apache--2.0-4c6ef5?style=flat-square" alt="License">
</a>
</p>

<p align="center">
<a href="https://muzak.dev/docs">Documentation</a> &nbsp;|&nbsp;
<a href="https://muzak.dev/docs/getting-started/first-steps">Quick start</a> &nbsp;|&nbsp;
<a href="BENCHMARKS.md">Benchmarks</a> &nbsp;|&nbsp;
<a href="CONTRIBUTING.md">Contributing</a>
</p>

---

```bash
go get muzak.dev/framework
```

```go
type Params struct {
    Username string `path:"username" doc:"The username to look up"`
}

type UserOut struct {
    Username string `json:"username"`
}

r.Get("/users/{username}", func(ctx *muzak.Context, in Params) (UserOut, error) {
    return UserOut{Username: in.Username}, nil
})
```

`Params` is the request. `UserOut` is the response. You never write the type
arguments: Go 1.27 added generic methods and generalized function type
inference, so the router reads both types off the handler literal.

There is no wrapper around the response and no filtering pass at run time, which
means a field you did not declare on `Out` cannot leak. It is not part of the
type, and the compiler is what tells you rather than a bug report.

## What you get

| | |
|---|---|
| **Routing** | Segment-wise radix trie. A static match takes 64 ns and allocates nothing |
| **Request binding** | `path`, `query`, `header`, `cookie`, `form`, `file` or the JSON body, by struct tag, with the plan compiled once per route |
| **Validation** | Declared against the field itself, so renaming it is a change the compiler checks |
| **Dependencies** | Guards and typed providers, read with `From[T](ctx)` and no cast anywhere |
| **Real-time** | RFC 6455 WebSockets and typed server-sent events, implemented here rather than delegated |
| **Versioning** | Per route or router, read from the path, a header, the `Accept` header or a function of your own |
| **Documentation** | OpenAPI 3.1 at `/openapi.json`, derived from the code, and an optional dashboard with a request console at `/docs` |
| **Errors** | One envelope for every failure, a constructor per status, and causes that stay server-side |
| **Defaults** | Conservative everywhere. Relaxing one is a decision you make out loud |

## A whole application

```go
settings := muzak.MustLoadConfig[core.Settings](muzak.EnvFile(".env"))

app := muzak.New(muzak.AppOptions{
    Title:   "Awesome API",
    Version: "1.0.0",
    Addr:    settings.Addr,
},
    muzak.WithDependencies(core.GetQueryToken),
    muzak.WithSingleton(settings),
)

app.Include(routers.Users())
app.Include(routers.Items())
app.Include(routers.Admin(),
    muzak.WithPrefix("/admin"),
    muzak.WithTags("admin"),
    muzak.WithDependencies(core.GetTokenHeader(settings)),
)

log.Fatal(app.RunSignals())
```

Each router is written on its own, unaware of the prefix, tags and guards it
will run under. The application decides where things mount and what protects
them, and that decision lives in one visible place.

Start it and open `/docs`. A runnable version is in [`example/`](example).

## Documentation, when you want it

Every application publishes an OpenAPI 3.1 document at `/openapi.json`,
generated from the routes and the types. Rendering it is a separate module, so
a service that wants no documentation UI carries none: Go downloads and links a
module only when something imports it.

```go
import (
    "muzak.dev/framework"
    "muzak.dev/openapi/ui"
)

app := muzak.New(muzak.AppOptions{
    Title:   "Awesome API",
    Version: "1.0.0",
    DocsUI:  ui.Files(), // /docs, or nothing at all if you leave it out
})
```

That is the [OpenAPI dashboard](https://github.com/muzak-dev/openapi): it reads
this application's own document and gives you the reference grouped by tag,
every schema as an outline, request snippets in thirteen languages, and a
console that sends the request from the page and shows the status, the timing,
the headers and the body -- or hands you the same request as a `curl` command.
An event stream is read as it arrives.

Nothing is fetched at run time. The dashboard is embedded in the binary that
imports it, so it works air-gapped and behind a proxy that allows nothing out.

Groups come from the tags a router carries, and are described where the
application is configured:

```go
app := muzak.New(muzak.AppOptions{
    Title:   "Awesome API",
    Version: "1.0.0",
    Tags: []muzak.Tag{
        {Name: "items", Description: "Everything the catalogue holds."},
        {Name: "admin", Description: "Operations that need a staff token."},
    },
})

items := muzak.NewRouter(muzak.WithPrefix("/items"), muzak.WithTags("items"))
```

Described tags lead, in the order they are declared; a tag only a route names
follows. The page and its assets are constants once the application is built,
so each is compressed once, cached by the client and revalidated with an entity
tag, and the page is served under a policy that hashes its own inline script
and permits no network access beyond this origin.

Where it is served, and whether it is served at all, is configuration:

```go
muzak.AppOptions{
    DocsUI:      ui.Files(),                // nil (the default) serves no page
    DocsPath:    "/reference",              // default "/docs"
    OpenAPIPath: "/reference/openapi.json", // default "/openapi.json"
    DisableDocs: false,                     // true serves neither
}
```

Both are announced the moment the socket opens, as URLs you can click:

```
00:27:38.548 INFO  [Server]  Listening on [::]:8099  scheme=http
00:27:38.548 INFO  [Docs]    Documentation at http://localhost:8099/docs  openapi=http://localhost:8099/openapi.json
```

A path that is not absolute, that collides with the other one, or that one of
your own routes already answers fails the build with a message saying which,
rather than becoming a page nobody can reach.

## Validation

Rules are declared against the field, not against its name:

```go
func (in *CreateUser) Validate(v *muzak.Validation) {
    v.String(&in.Email).Trim().Lower().Required().Email()
    v.String(&in.Password).Required().MinLen(12).Must(NotACommonPassword)
    v.Number(&in.Age).Between(18, 120)
    v.Slice(&in.Tags).MaxItems(10).Unique().Each(validate.String().MaxLen(20))
}
```

There is no tag string to typo and no field name written as text. `&in.Email`
**is** the field, so renaming it is a change the compiler checks, and
`v.Number(&in.Email)` does not compile.

The same declarations feed the generated document: `MinLen(12)` emits
`minLength`, `OneOf` emits an `enum`, `Between` emits `minimum` and `maximum`.
The OpenAPI document cannot drift from the validation, because both are read
from one declaration.

## Errors

Every failure renders as one envelope: a machine-readable code, a message safe
to disclose, the status, per-field details and the request identifier that ties
the response to the log. There is a constructor per outcome, so returning the
right response is not a matter of remembering the right number:

```go
return schemas.UserOut{}, muzak.NotFound("no user goes by that name")
return schemas.UserOut{}, muzak.Forbidden("")               // standard sentence
return schemas.UserOut{}, muzak.Conflict("that name is taken").Wrap(err)
```

`BadRequest` `Unauthorized` `PaymentRequired` `Forbidden` `NotFound`
`MethodNotAllowed` `NotAcceptable` `RequestTimeout` `Conflict` `Gone`
`PreconditionFailed` `PayloadTooLarge` `UnsupportedMediaType`
`UnprocessableEntity` `TooManyRequests` `InternalServerError` `NotImplemented`
`BadGateway` `ServiceUnavailable` `GatewayTimeout` -- and `NewHTTPError(status,
message)` for anything else.

Each returns an `*HTTPError`, so `.Wrap(err)`, `.WithCode("card_declined")` and
`.WithDetails(...)` chain onto any of them. What `Wrap` holds is logged and
never transmitted, including behind a 5xx you returned deliberately; an error
that does not describe itself at all becomes an opaque 500 with the cause kept
server-side.

```json
{
  "error": {
    "code": "not_found",
    "message": "no user goes by that name",
    "status": 404,
    "details": [{ "field": "name", "location": "path", "issue": "does not exist" }]
  },
  "request_id": "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"
}
```

## Every language, including the framework's

A locale is resolved once per request, from the Accept-Language header or from
wherever else you say, and every string in the response is written in it. That
includes the ones Muzak produces: each validation rule, each binder message, and
the sentence behind each HTTP status.

```go
//go:embed locales
var locales embed.FS

app := muzak.New(muzak.AppOptions{
    I18n: muzak.I18nOptions{Store: i18n.MustLoad(locales, "locales")},
})
```

```go
func greet(ctx *muzak.Context, in Params) (Out, error) {
    return Out{
        Greeting: ctx.T("greeting.hello", "name", in.Name),
        Items:    ctx.T("greeting.items", "count", in.Items),
        Today:    ctx.L(time.Now(), "as", "date"),
    }, nil
}
```

A count both prints and chooses which wording prints it, so "no items", "one
item" and "5 items" are three entries in the file and the language decides
between them. Muzak carries the CLDR arithmetic for around ninety languages, so
a locale file supplies only the words.

Locale files are YAML, read by a parser written for this and nothing else, so
the zero-dependency guarantee holds and a file from the published rails-i18n
corpus loads unchanged. A file names only what your service adds and the rules
you want worded differently; everything else falls through to the locale Muzak
ships:

```yaml
es:
  errors:
    messages:
      blank: "es obligatorio"
```

```
$ curl -H 'Accept-Language: es' localhost:8080/items -d '{"name": ""}'
{"error":{"message":"La solicitud no pudo ser validada.","status":422,
  "details":[{"field":"name","location":"body","issue":"es obligatorio"}]}}
```

Leave the option out and none of this exists: no middleware runs, every message
reads exactly as it did, and the engine, the YAML parser and the plural rules
are not linked into the binary at all.

## Safe defaults

Every default is the conservative one. Listener timeouts are all non-zero,
request bodies are capped at one mebibyte, unknown JSON members are rejected,
CORS denies every cross-origin request until a policy is written, a cross-origin
WebSocket handshake is refused, no forwarding header is believed until a proxy
is named, and a panic becomes a generic 500 with the stack recorded only in the
log.

Each of these can be relaxed deliberately. None of them is relaxed by omission.
The full list is in
[Safe Defaults](https://muzak.dev/docs/security/safe-defaults).

## Performance

Apple M1 Pro, Go 1.27, medians of three runs. Every case drives the real path
through `App.ServeHTTP`, so routing, binding, dependencies and encoding are all
included.

| | ns/op | allocs/op |
|---|---:|---:|
| Route lookup, static | 64.4 | 0 |
| Route lookup, parameter | 80.5 | 0 |
| Full request, bare | 545.6 | 8 |
| The same work by hand on `http.ServeMux` | 608.1 | 9 |

Matching allocates nothing, and the framework path beats a hand-written
`net/http` handler doing identical work. [BENCHMARKS.md](BENCHMARKS.md) has the
method and the rest of the numbers.

### Test coverage

**98.7% of statements**, measured on Go 1.27.0.

| Package | Covered |
|---|---:|
| `muzak.dev/framework` | 98.8% |
| `muzak.dev/framework/validate` | 99.3% |
| `muzak.dev/framework/internal/radix` | 100.0% |
| `muzak.dev/framework/internal/wsframe` | 100.0% |
| `muzak.dev/framework/testclient` | 93.2% |

CI recomputes this on every push and fails below 98%, so the badge cannot
quietly go stale. The uncovered statements are defensive branches that cannot be
reached; each carries a `// coverage:` comment saying why, and a test enforces
that those comments hold a real justification.

### No dependencies

```
$ go list -m all | tail -n +2 | wc -l
0
```

Nothing is vendored and nothing is pulled in. Everything the framework needs is
in the standard library of Go 1.27, including `encoding/json/v2` and `uuid`.

## Documentation

| | |
|---|---|
| [First steps](https://muzak.dev/docs/getting-started/first-steps) | Install, project layout, your first route |
| [Routers](https://muzak.dev/docs/getting-started/routers) | Paths, methods, nesting, route options |
| [Request data](https://muzak.dev/docs/getting-started/request-data) | Every place an input field can be read from |
| [Dependencies](https://muzak.dev/docs/getting-started/dependencies) | Guards, providers, singletons |
| [Validation](https://muzak.dev/docs/fundamentals/validation) | Rules, transforms, cross-field checks |
| [Internationalization](https://muzak.dev/docs/fundamentals/internationalization) | Locales, translations, pluralization, localized errors |
| [Versioning](https://muzak.dev/docs/fundamentals/versioning) | Four schemes, and what each costs |
| [WebSockets](https://muzak.dev/docs/realtime/websockets) | Handshake, bounds, what a hostile peer cannot do |
| [Server-sent events](https://muzak.dev/docs/realtime/server-sent-events) | Typed streams, resuming, keepalive |
| [Testing](https://muzak.dev/docs/fundamentals/testing) | The in-process client |
| [Deployment](https://muzak.dev/docs/deployment/server-configuration) | Timeouts, shutdown, running behind a proxy |

Machine-readable: [llms.txt](https://muzak.dev/llms.txt) and
[llms-full.txt](https://muzak.dev/llms-full.txt).

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers the toolchain, the checks that have to
pass, and the rules the test suite enforces that are not obvious the first time.
Vulnerabilities go through [SECURITY.md](SECURITY.md) rather than a public
issue.

## Licence

Dual-licensed under either of

- [MIT](LICENSE-MIT)
- [Apache License, Version 2.0](LICENSE-APACHE)

at your option. Unless you state otherwise, any contribution you intentionally
submit for inclusion shall be dual-licensed as above, with no additional terms.

[`LICENSE`](LICENSE) is a copy of the MIT text, so that tools looking for a file
by that name find a licence they can classify rather than a pointer they cannot.
It grants nothing beyond what the two files above already offer.
