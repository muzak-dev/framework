<div align="center">

# Muzak

**A type-safe web framework for Go that brings FastAPI's developer experience<br>to the language without giving up the compiler.**

[![CI](https://img.shields.io/github/actions/workflow/status/muzak-dev/framework/ci.yml?branch=main&style=flat-square&logo=githubactions&logoColor=white&label=CI)](https://github.com/muzak-dev/framework/actions/workflows/ci.yml)
[![Coverage](https://img.shields.io/badge/coverage-98.7%25-3fb950?style=flat-square&logo=go&logoColor=white)](#test-coverage)
[![Go Reference](https://img.shields.io/badge/pkg.go.dev-reference-00ADD8?style=flat-square&logo=go&logoColor=white)](https://pkg.go.dev/muzak.dev/framework)
[![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/dl/)
[![Dependencies](https://img.shields.io/badge/dependencies-0-3fb950?style=flat-square)](#no-dependencies)
[![License](https://img.shields.io/badge/license-MIT%20or%20Apache--2.0-4c6ef5?style=flat-square)](#licence)

[Documentation](https://muzak.dev/docs) &nbsp;|&nbsp;
[Quick start](https://muzak.dev/docs/getting-started/first-steps) &nbsp;|&nbsp;
[Benchmarks](BENCHMARKS.md) &nbsp;|&nbsp;
[Contributing](CONTRIBUTING.md)

</div>

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

That is the whole idea. `Params` is the request. `UserOut` is the response. Both
are checked when you compile, not when a request arrives.

Nested routers, generated OpenAPI, first-class dependency injection and
automatic request and response handling, built on `net/http` and Go 1.27, with
no third-party dependencies at all.

### At a glance

| | |
|---|---|
| **Routing** | Segment-wise radix trie. A static match takes 64 ns and allocates nothing |
| **Request** | Bound from `path`, `query`, `header`, `cookie`, `form`, `file` or the JSON body, by struct tag, with the plan compiled once per route |
| **Validation** | Declared against the field itself, so renaming it is a change the compiler checks |
| **Dependencies** | Guards and typed providers, retrieved with `From[T](ctx)` and no cast anywhere |
| **Real-time** | RFC 6455 WebSockets and typed server-sent events, implemented here rather than delegated |
| **Documentation** | OpenAPI 3.1 at `/openapi.json` and a self-contained UI at `/docs`, both derived from the code |
| **Defaults** | Conservative everywhere. Relaxing one is a decision you make out loud |

---

## Why this exists

FastAPI is pleasant to use because the type annotation *is* the contract. You
declare what comes in, you declare what goes out, and the framework handles
parsing, validation and documentation from those declarations alone.

Go has a stronger type system than Python and almost none of that ergonomics.
Most Go frameworks hand you `func(c *Context)` and leave you to pull values out
of the request by hand, marshal the response by hand, and describe the endpoint
in a comment nobody updates.

Muzak is the argument that you can have both. Go 1.27 added generic methods,
which is the piece that was missing: a router can now offer `r.Get`, `r.Post`
and the rest as methods that carry their own type parameters, so the framework
learns your input and output types from the handler itself.

You never write the type arguments. Go infers them.

---

## The six ideas

### 1. The type signature is the contract

A handler is `func(ctx *muzak.Context, in In) (Out, error)`.

`In` is the fully typed request. Its fields are bound from the path, query
string, headers, cookies and JSON body according to their struct tags. `Out` is
the response body, serialized exactly as returned.

There is no wrapper type. No `Response[T]`, no `response_model=`, no filtering
pass at run time. The handler's return type **is** the response model.

That last point is the one that matters most in practice. FastAPI can let you
`return item` where `item` is an arbitrary ORM object, because Pydantic filters
it against `response_model` before it goes out. Go cannot do that safely, and
faking it with reflection-based filtering would trade a compile-time guarantee
for a run-time one.

So Muzak asks you to construct the actual `Out` value. That is more typing
than FastAPI. In exchange, a field you did not declare on `Out` cannot leak,
and the compiler is what tells you, not a bug report.

### 2. Static things are declared, dynamic things are imperative

A status code that never changes for a route is part of the route:

```go
r.Post("/items/", createItem, muzak.Status(201))
```

A status code that depends on what happened is set in the handler:

```go
if in.Async {
    ctx.SetStatus(202)
}
return ItemOut{ID: "42", Name: in.Name}, nil
```

The two are orthogonal. You never have to choose between returning a value and
setting a status, and you never declare something dynamic as if it were fixed.

The same split runs through the rest of the API. Tags, summaries, documented
responses and body limits are declarations. Headers, cookies and statuses that
depend on the request are imperative calls on the context.

### 3. Dependencies come in exactly two shapes

A **guard** validates and produces nothing:

```go
func GetQueryToken(ctx *muzak.Context) error {
    if ctx.Query("token") == "" {
        return muzak.NewHTTPError(400, "token is required")
    }
    return nil
}
```

A **provider** produces a typed value:

```go
func GetCurrentUser(ctx *muzak.Context) (CurrentUser, error) {
    token, ok := muzak.BearerToken(ctx)
    if !ok {
        return CurrentUser{}, muzak.NewHTTPError(401, "unauthorized")
    }
    return lookUp(token)
}
```

Guards attach to an application or a router and cover everything beneath them.
Providers attach to a route, and the handler retrieves the value by type:

```go
r.Get("/items/{id}", func(ctx *muzak.Context, in Params) (ItemOut, error) {
    user := muzak.From[CurrentUser](ctx)
    return ItemOut{ID: in.ID, Owner: user.Username}, nil
}, muzak.Needs(GetCurrentUser))
```

`muzak.From[CurrentUser](ctx)` is checked at compile time. There is no
`interface{}`, no type assertion, no service locator, and no string key to
mistype. Ask for a type the route never declared and the framework tells you so
by name, because that is a bug in the wiring rather than a condition to handle.

Resolved values live on the request's context and are cleared when it returns
to the pool, so two concurrent requests can never see each other's values.
There is a test for exactly that, run under the race detector.

### 4. Compile-time safety wherever Go allows it

Muzak does as much as possible at start-up and as little as possible per
request.

- Binding plans are compiled once per route. The per-request path walks a list
  of precompiled setters; it never inspects a type.
- Response schemas are derived once, at build time, from the same `In` and
  `Out` types.
- Route conflicts, unbindable field types, path parameters no field binds,
  duplicate operation identifiers and malformed prefixes are all reported when
  the application is built, before a socket is opened.
- All of those problems are reported *together*, so a first run in a new
  environment lists everything wrong at once instead of one thing per attempt.

The same principle governs the router: matching walks a segment-wise radix trie
and allocates nothing.

### 5. Safe defaults, not permissive ones

Every default is the conservative one. Relaxing any of them is a decision you
make out loud.

| | |
|---|---|
| Listener timeouts | All four set to non-zero values. An `http.Server` left at its zero values holds a connection open forever, which is all a slow-loris client needs. |
| Request bodies | Capped at 1 MiB, overridable per route. |
| Unknown JSON members | Rejected. A client's typo becomes an immediate 422 instead of a silently dropped value. |
| Duplicate members, invalid UTF-8 | Rejected by `encoding/json/v2`. |
| Cross-origin requests | Denied. No CORS headers are emitted until a policy is configured, and a wildcard origin combined with credentials is refused outright. |
| Cross-origin WebSocket handshakes | Refused. A handshake is not subject to the same-origin policy and is never preflighted, so CORS cannot cover it and a separate origin check does. |
| WebSocket messages | Capped at 1 MiB, refused before any of the payload is buffered, and read a chunk at a time so a declared length costs nothing until it arrives. |
| WebSocket message time | One message has 30 seconds to arrive once it has begun, and a bounded number of frames to arrive in. Idle connections are left alone. |
| WebSocket connections | 1024 per application, then 503 with a Retry-After. File descriptors run out before anything else does. |
| WebSocket extensions | None negotiated, so no peer can ask the server to hold compression state on its behalf. |
| Event stream writes | Given up on after 10 seconds, so a client that opens a stream and never reads it cannot pin a goroutine and a growing socket buffer. |
| Event streams | 1024 per application, then 503 with a Retry-After, and a keepalive comment every 15 seconds so a proxy does not close one it believes idle. |
| Event fields | A name or identifier carrying a line break is refused, because it would end its own field and let what follows be read as events of its own. |
| Panics | Logged with a full stack trace, answered with a generic 500. Nothing derived from the panic reaches the client. |
| Request identifiers | Not trusted from the client, because an attacker-controlled identifier is an attacker-controlled log field. |
| Forwarding headers | Not believed. `X-Forwarded-For` is read only for a request that arrived from a proxy named in `TrustedProxies`, because a header any client can write is an identity any client can claim. |
| Rate limit counters | Bounded in number and in key length, so per-client accounting cannot become the exhaustion it was added to prevent. A storage that stops answering refuses traffic rather than silently enforcing nothing. |
| Secrets in logs | Attribute keys such as `authorization`, `token` and `api_key` are redacted by default. |
| Token comparison | Constant-time, over hashed inputs, so neither the contents nor the length of a secret leaks through timing. |
| Documentation UI | Self-contained. It fetches nothing from a third party and is served under a strict content security policy with a per-response nonce. |

---

## A whole application

```go
settings := muzak.MustLoadConfig[Settings](muzak.EnvFile(".env"))

app := muzak.New(muzak.AppOptions{
    Title:   "Bigger Applications Example",
    Version: "1.0.0",
    Addr:    settings.Addr,
},
    muzak.WithDependencies(GetQueryToken),
    muzak.WithSingleton(settings),
)

app.Include(users.NewRouter())
app.Include(items.NewRouter())
app.Include(admin.NewRouter(),
    muzak.WithPrefix("/admin"),
    muzak.WithTags("admin"),
    muzak.WithDependencies(GetTokenHeader),
    muzak.WithResponseDoc(418, "I'm a teapot"),
)

log.Fatal(app.RunSignals())
```

Each router is written on its own, unaware of the prefix, tags or guards it will
eventually run under. The application decides where things mount and what
protects them, and that decision lives in one visible place.

`App` embeds `*Router`, so `app.Get(...)` works at the root using the same
generic methods any nested router uses.

Start it and open `/docs`.

---

### 6. Validation is part of the type, not a string in a tag

Rules are declared against the field itself:

```go
func (in *CreateUser) Validate(v *muzak.Validation) {
    v.String(&in.Email).Trim().Lower().Required().Email()
    v.String(&in.Password).Required().MinLen(12).Must(NotACommonPassword)
    v.String(&in.Confirm).Equal(in.Password).Message("must match the password")
    v.Number(&in.Age).Between(18, 120)
    v.String(&in.Role).OneOf("admin", "editor", "viewer")
    v.Slice(&in.Tags).MaxItems(10).Unique().Each(validate.String().MaxLen(20))
}
```

There is no tag string to typo and no field name written as text. `&in.Email`
**is** the field, so renaming it is a change the compiler checks, and
`v.Number(&in.Email)` does not compile. Type `v.` and your editor lists the
kinds; type `v.String(&in.Email).` and it lists every rule that applies to a
string.

A custom rule is an ordinary function, so it needs no registration and is
testable on its own:

```go
func NotACommonPassword(password string) error {
    if commonPasswords[password] {
        return errors.New("is too common, choose something less guessable")
    }
    return nil
}
```

Cross-field rules are ordinary Go, because `in` is right there:

```go
v.When(in.Role == "admin" && in.Age < 21).
    Reject(&in.Role, "an admin must be at least 21")
```

Four things worth knowing about how it behaves:

- **Transforms mutate.** `Trim().Lower()` changes what the handler receives, and
  runs before the checks, so `Trim().Required()` rejects a field of pure
  whitespace.
- **Optional means optional.** A field without `Required()` skips its remaining
  rules when it is empty, so `MaxLen(20)` has nothing to say about a value
  nobody sent. A nil pointer field is skipped entirely.
- **One failure per field.** The first rule that fails reports; a single empty
  field does not also complain that it is too short and not an email address.
- **Guards run first.** An unauthenticated caller gets 401, not a map of your
  schema.

Failures merge into the same envelope as binding failures, with the location the
binder already established, and every field is reported at once:

```json
{
  "error": {
    "code": "validation_error",
    "message": "The request could not be validated.",
    "status": 422,
    "details": [
      { "field": "username", "location": "body",  "issue": "is reserved" },
      { "field": "email",    "location": "body",  "issue": "must be a valid email address" },
      { "field": "password", "location": "body",  "issue": "must be at least 12 characters" },
      { "field": "tags",     "location": "body",  "issue": "must not repeat fine" },
      { "field": "limit",    "location": "query", "issue": "must be between 1 and 100" }
    ]
  },
  "request_id": "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"
}
```

And the rules feed the documentation. `MinLen(12)` emits `minLength`, `OneOf`
emits an `enum`, `Between` emits `minimum` and `maximum`, `Email` emits a
`format`, and element rules describe an array's `items`. The OpenAPI document
cannot drift from the validation, because both are read from the same
declaration:

```json
"role": { "type": "string", "enum": ["admin", "editor", "viewer"] },
"tags": {
  "type": "array", "maxItems": 10, "uniqueItems": true,
  "items": { "type": "string", "maxLength": 20, "pattern": "^[a-z0-9-]+$" }
}
```

## What comes in the box

**Configuration** from struct tags, loaded once at start-up, with every missing
or malformed variable reported together:

```go
type Settings struct {
    AppName      string `env:"APP_NAME" default:"Awesome API"`
    AdminEmail   string `env:"ADMIN_EMAIL" required:"true"`
    ItemsPerUser int    `env:"ITEMS_PER_USER" default:"50"`
    DatabaseURL  string `env:"DATABASE_URL" secret:"true"`
}
```

The real environment beats a checked-in `.env`, a field marked `secret` keeps
its value out of error messages, and the same setters the request binder uses
do the conversion, so configuration and requests agree on what an `int` is.

**A lifecycle** for anything that must be opened before traffic and closed
after it:

```go
type Lifecycle interface {
    Name() string
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

Publish a value with `WithSingleton` and, if it implements `Lifecycle`, Muzak
takes it from there. Components start **in parallel**, so start-up costs the
slowest one rather than the sum:

```
14:32:07.482 INFO  [Server]        Starting Muzak application...
14:32:07.483 INFO  [Server]        Starting 3 lifecycle components in parallel: redis, database, ml-model
14:32:07.501 INFO  [Server]        Started "redis" (18ms)
14:32:07.512 INFO  [Server]        Started "ml-model" (29ms)
14:32:07.544 INFO  [Server]        Started "database" (61ms)
14:32:07.545 INFO  [Server]        All lifecycle components ready (61ms total)
```

If one fails, the others are cancelled immediately, everything that did start is
stopped, and the failures are reported together. A failed start-up never leaks a
connection pool.

Shutdown runs in the order that matters: the HTTP server stops accepting and
drains its in-flight requests **first**, and only then are components released.
Pulling a database connection out from under a request that is still running
would turn an orderly shutdown into a burst of errors.

For anything that does not warrant its own type, `LifecycleFunc` builds a
component from two closures.

**A logger** that is readable in development and parseable in production, and
picks between them by looking at where its output is going:

```
14:32:10.114 INFO  [UsersService]  user created  user_id=42 request_id=0611f4b2
```

Aligned columns, colour on a terminal, `NO_COLOR` honoured, secrets redacted by
key, buffers pooled, and `With` attributes rendered once when the child logger
is built rather than on every record. Off a terminal it writes JSON instead.

**One error envelope** for every failure, carrying the request identifier that
ties the response to the server's log:

```json
{
  "error": {
    "code": "validation_error",
    "message": "The request could not be validated.",
    "status": 422,
    "details": [
      { "field": "name", "location": "body", "issue": "is required" },
      { "field": "limit", "location": "query", "issue": "must be a valid integer" }
    ]
  },
  "request_id": "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"
}
```

Every offending field is reported at once. An error that describes itself
reaches the client as written; anything else becomes an opaque 500 with the real
cause logged and never transmitted. Replace the shape entirely with
`AppOptions.ErrorRenderer`.

**File uploads** bound by the same rules as everything else, where the Go type
decides what the handler is handed:

```go
type FileBytesIn struct {
    File []byte `file:"file" doc:"A file read as bytes"`
}

type UploadFileIn struct {
    File muzak.File `file:"file" doc:"A file read as an upload"`
    Note string      `form:"note" required:"false"`
}

type MultiUploadIn struct {
    Files []muzak.File `file:"files"`
}
```

`[]byte` reads the content into memory. `muzak.File` carries the filename,
media type and size and leaves the content where the parser put it, reachable
with `Open`, `Bytes` or `Save`. A slice of either accepts every file sent under
the name. Fields tagged `form` come from the same body, converted by the setters
that convert a query parameter, so a multipart route validates like any other. A
route may bind form values and no file at all, which is what a sign-in form is,
and it then accepts `application/x-www-form-urlencoded` too so that a plain HTML
form posts to it without an enctype:

```go
type LoginIn struct {
    Username string `form:"username"`
    Password string `form:"password"`
}
```

Both limits are declared, never remembered:

```go
muzak.MaxUploadSize(32 << 20) // the whole form body
muzak.MaxFileSize(10 << 20)   // any single file inside it
```

A body over the upload limit is refused with 413 while it is being read, so the
server never buffers more than the limit. Anything over 10 MiB spills to a
temporary file that is removed when the handler returns.

**An HTML escape hatch** for the pages that go with those forms. A handler
returning `muzak.HTML` bypasses JSON encoding entirely, and the generated
document says `text/html` rather than describing a schema:

```go
r.Get("/", func(ctx *muzak.Context, _ muzak.Empty) (muzak.HTML, error) {
    return muzak.HTML(`<body>
<form action="/files/" enctype="multipart/form-data" method="post">
<input name="files" type="file" multiple>
<input type="submit">
</form>
</body>`), nil
})
```

**Middleware** that is already installed, plus two you can add. The built-in
chain identifies the request, recovers panics, writes the access log and sets
the security headers. `CORS` is configured rather than installed, and denies
everything until a policy says otherwise. `Compress` is one line:

```go
app.Use(muzak.Compress(muzak.CompressionOptions{}))
```

It negotiates gzip or deflate, records `Vary` on every response, and declines
what compressing would not help: a body under 1400 bytes, an image, an event
stream, a range, or anything the handler encoded itself. On this project's own
example app that is 91% off the OpenAPI document and 68% off the docs UI.

Anything else is an ordinary `func(http.Handler) http.Handler`. One thing
differs from FastAPI and fails quietly: Go writes the header block at the first
`WriteHeader`, so a header set *after* the next handler returns is dropped
without a word. Middleware reporting something only known at the end wraps the
writer and fills it in as the response starts.

**A rate limiter**, off until a policy names a quota, and then a policy rather
than a number. One limit cannot tell a burst from sustained abuse, so a policy
is several at once and every one of them is counted for every request:

```go
app := muzak.New(muzak.AppOptions{Title: "Shop"},
    muzak.WithRateLimit(muzak.RateLimitOptions{
        Storage: NewRedisRateLimitStorage(settings.RedisAddr), // or leave it out for memory
        Tracker: UserOrIPTracker,                              // or leave it out for the address
        Quotas: []muzak.Quota{
            {Name: "short", Window: time.Second, Limit: 3},
            {Name: "medium", Window: 10 * time.Second, Limit: 20},
            {Name: "long", Window: time.Minute, Limit: 100},
        },
    }),
)

r.Get("/health", health, muzak.SkipRateLimit())
r.Post("/login", login, muzak.RateLimit(muzak.Quota{Name: "login", Window: time.Minute, Limit: 5}))
```

A client that overruns the short window still accrues against the long one, so
pausing between bursts launders nothing. A refusal carries 429, `Retry-After`
and the `RateLimit` headers that describe the whole policy, so a client can stay
inside a budget it can see.

Three decisions are yours, and each has a default that is honest about what it
is. `RateLimitStorage` is where the counters live, defaulting to an in-process
table that is right for one process and says so; anything running more than once
wants a shared one, and the interface is three arguments wide so that whatever
you already run can satisfy it:

```go
Increment(ctx context.Context, quota, key string, window time.Duration) (count int, reset time.Duration, err error)
```

`RateLimitTracker` is whose budget a request is spent from, defaulting to the
address, and is where an API key, a tenant or a resolved user identity goes
instead. `ClientIPOptions` is which address that means, and it believes no
forwarding header until a proxy is named, because a header any client can write
is a budget any client can escape.

The count happens **before** the route's guards and dependencies. That is the
half that matters for brute force: a request a guard rejects is still counted, so
failed sign-ins cost the attacker their budget rather than being free.
`AfterDependencies` moves it after them for a tracker that keys on a resolved
user, and gives that half up in exchange.

The table is bounded in two directions, because per-client accounting is itself
something an attacker can grow: expired counters are swept, a full table discards
the counter closest to expiring, and a tracker key the client chose the length of
is hashed rather than truncated, so two clients cannot be merged into one budget.
A storage that cannot answer refuses the request with 503 rather than quietly
enforcing nothing; `FailOpen` trades that for availability. The key never reaches
a log line, because it carries whatever credential the tracker read.

**WebSockets**, with the protocol implemented here rather than delegated to a
library. A route is registered like any other and the handler is handed the
connection:

```go
type WSItemIn struct {
    ItemID string `path:"item_id"`
    Q      *int   `query:"q"`
}

r.WS("/items/{item_id}/ws", func(ctx *muzak.Context, in WSItemIn, conn *muzak.WSConn) error {
    session := muzak.From[SessionOrToken](ctx)
    for {
        message, err := conn.ReadText(ctx.Context())
        if err != nil {
            return nil // the client closed
        }
        conn.WriteText(ctx.Context(), "credential: "+session.Value)
        if in.Q != nil {
            conn.WriteText(ctx.Context(), fmt.Sprintf("q is %d", *in.Q))
        }
        conn.WriteText(ctx.Context(), fmt.Sprintf("you said %q, about item %s", message, in.ItemID))
    }
}, muzak.Needs(GetSessionOrToken))
```

The handshake is an ordinary `GET`, so middleware runs, guards run, dependencies
resolve, and the input is bound and validated **before a single byte is
upgraded**. A request that fails any of that gets the usual JSON error and never
becomes a connection at all, which is the difference between a rejection a
client can read and a socket that closes a moment after it opened.

Reading and writing are message oriented. A message split across frames arrives
once and whole, a ping is answered without the handler knowing, and a close is
answered and reported as a `*WSCloseError`, which is why the loop above ends on
any error. Writes are serialised, so a broadcast from many goroutines cannot
interleave two messages on one connection.

A WebSocket is the longest-lived thing an unauthenticated stranger can ask a
server for, so every direction a peer controls is bounded, and each bound stops
something specific:

| A peer that... | ...is stopped by |
|---|---|
| sends a message larger than the limit | refused with 1009 before any of the payload is buffered |
| declares a huge payload and sends none of it | a frame is taken a chunk at a time, so six bytes of header cannot buy an allocation the size of the limit |
| dribbles a message out a byte at a time | closed once `ReadTimeout` passes with the message unfinished; waiting *between* messages stays unbounded |
| fragments a message endlessly, or floods pings | closed once too many frames arrive without one completing, which no size limit would ever catch |
| stops reading what it asked for | writes give up after `WriteTimeout` rather than pinning a goroutine |
| opens connections without end | `MaxConnections` per application, then 503 with `Retry-After` |
| opens one from another origin | refused outright: a handshake is not subject to the same-origin policy and is never preflighted, so `CORS` cannot cover it |
| sends a body with the handshake | refused, because what went unread would be taken for frames the moment it was upgraded |
| asks for an extension | none is negotiated, so no peer can make the server hold decompression state |

RFC 6455 is enforced rather than assumed: reserved opcodes, reserved bits,
unmasked client frames, fragmented control frames, lengths not minimally
encoded, text that is not UTF-8 and reserved close codes are each answered with
the status the specification names. Nothing a peer sends is echoed into a
response header, and nothing a handler fails with is disclosed to the peer.

Each of those rows is a test that plays the attacker, and each of them was
checked by removing the defence and watching the test fail.

Every bound above is structural: it says what one message, one connection or one
application may cost, which is something the framework can decide on its own.
How fast a peer may send is a policy question, and it is answered by the same
rate limiter ordinary routes use, with `MessageLimits` alongside the rest:

```go
muzak.WithWebSocket(muzak.WSOptions{
    MessageLimits: []muzak.Quota{{Name: "ws-messages", Window: time.Second, Limit: 20}},
})
```

A peer that goes over is closed with 1008 rather than left connected and
ignored, because a message silently dropped is a protocol nobody can debug. The
budget belongs to the client rather than the connection, so opening a second one
does not buy a second budget.

```go
app.Include(chat, muzak.WithWebSocket(muzak.WSOptions{
    ReadLimit:      64 << 10,
    PingInterval:   30 * time.Second,
    AllowedOrigins: []string{"https://app.example.com"},
}))
```

Open connections are tracked, so a graceful shutdown tells every peer it is
going away with 1001 and waits for the handlers, which `net/http` cannot do on
its own: a hijacked connection is no longer one it knows about. `WSDial` is the
client half of the same engine, which is what lets a route be tested over a real
connection instead of against a second implementation:

```go
conn := client.WS("/items/plumbus/ws", testclient.Query("token", "jessica"))
conn.WriteText(t.Context(), "hello")
reply, err := conn.ReadText(t.Context())
```

**Server-sent events**, the other half of what a WebSocket is usually reached
for, and the simpler half: the server sends, the client listens, and a browser
reads it natively with `EventSource`, reconnecting on its own when a stream
drops. The stream is typed, so what a route may send is checked by the compiler
and described in the generated document:

```go
r.SSE("/rooms/{room}/stream", func(ctx *muzak.Context, in StreamIn, stream *muzak.SSEStream[MessageOut]) error {
    for message := range room(in.Room).Messages(stream.Context()) {
        if err := stream.Send(message); err != nil {
            return err
        }
    }
    return nil
})
```

Everything else about the route is ordinary: middleware runs, guards run,
dependencies resolve, and the input is bound and validated **before a byte of
the stream is written**, so a refusal is a JSON error rather than a stream that
opens and shuts. `Router.SSEHandle` registers one for any method, which is what
a protocol that streams its answer to a posted document needs.

Nothing on the stream takes a context, unlike `WSConn`, because a stream belongs
to one request. `stream.Context()` governs every send and is cancelled when the
client disconnects or the server shuts down, so a handler watches one thing and
every send afterwards reports `ErrSSEStreamEnded`. Writes are serialised, so a
broadcast from many goroutines cannot interleave two events.

`SendEvent` carries what a bare value cannot: a name to dispatch under, an
identifier to resume from, a reconnection delay, or a payload that is not JSON,
such as the `[DONE]` sentinel some completion APIs end with. A browser sends the
last identifier it saw back in `Last-Event-ID`, which `stream.LastEventID()`
reads, and that is what turns a dropped connection into a stream that picks up
where it left off rather than one that starts again.

| A client that... | ...is stopped by |
|---|---|
| stops reading what it asked for | writes give up after `WriteTimeout` rather than pinning a goroutine and a growing socket buffer |
| opens streams without end | `MaxStreams` per application, then 503 with `Retry-After` |
| holds a stream open for hours | the listener's own timeouts are cleared for it and replaced with a deadline per event, so a healthy stream is never cut off and an unhealthy one still is |
| sends a `Last-Event-ID` a handler echoes | a name or identifier with a line break in it is refused, because it would forge events of its own on every stream that carries it |
| reads through a buffering proxy | `Cache-Control: no-transform`, `X-Accel-Buffering: no`, and a keepalive comment every `KeepAlive` |

Compression leaves an event stream alone: holding events in a compressor's
window until something forces them out is the one thing a stream cannot survive.
The origin check a WebSocket needs has no counterpart here, because an
`EventSource` is subject to the same-origin policy and to CORS like any other
request, so `AppOptions.CORS` already governs it.

```go
app.Include(live, muzak.WithSSE(muzak.SSEOptions{
    KeepAlive: 15 * time.Second,
    Retry:     2 * time.Second,
}))
```

Open streams are tracked, so a graceful shutdown ends every one of them and
waits for the handlers instead of waiting out its whole deadline once per
stream. `SSEDial` is the reading half of the same engine, so a route is tested
over a real connection rather than against a second implementation, and the test
client wraps it:

```go
stream := client.SSE("/items/stream", testclient.Query("token", "jessica"))
item := stream.Decode[ItemOut]()
```

**Frontend serving** for the static output of a frontend build, so the API and
the app it powers are one binary:

```go
app.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
```

Routes are matched first, so mounting at the root cannot shadow an API. A path
with no file behind it falls back to what the build produced: a `404.html` with
404, or an `index.html` with 200 for a browser navigation, which is what a
client-side router needs. A missing script still answers 404 rather than being
handed HTML it cannot parse. Point `FS` at an `embed.FS` and the frontend ships
inside the binary. Directories are never listed, a symlink cannot lead out of
the build output, and a missing directory is a build error, not a surprise on
the first request. A method other than `GET` or `HEAD` on a file is refused with
405, not answered with the file.

For assets that are not a frontend, `Static` is the same machinery without the
fallback, so a miss is a 404 and stays one:

```go
app.Static("/static", muzak.StaticOptions{Dir: "static"})
```

**A test client** that serves the application in-process over an in-memory
network, so a test exercises middleware, routing, binding, dependencies and
error rendering together:

```go
client := testclient.New(t, buildApp(), testclient.WithHeader("X-Token", "coneofsilence"))

client.Get("/items/foo").
    AssertStatus(200).
    AssertJSON(`{"id":"foo","title":"Foo","description":"There goes my hero"}`)

item := client.Get("/items/foo").Decode[Item]()
```

No free port needed, and nothing else on the machine can disturb it.

---

## What Go 1.27 made possible

Muzak needs Go 1.27, and uses it rather than merely declaring it:

- **Generic methods** are what make `r.Get`, `r.Post` and the rest work at all.
  Before 1.27 a method could not carry its own type parameters, so the
  registration API would have had to be package-level functions taking a router,
  which reads far worse. `Response.Decode[Item]()` in the test client is the
  same feature used for a different purpose.
- **Generalized function type inference** is why you never write those type
  arguments. Go reads them off the handler literal.
- **Struct literal nested field initialization** is why `AppOptions` can be
  written flat, with `Title:` and `ReadTimeout:` sitting next to `Addr:` even
  though they live in embedded structs.
- **`encoding/json/v2`** supplies the strictness Muzak relies on: duplicate
  members and invalid UTF-8 are rejected without asking, and `Deterministic`
  makes the generated OpenAPI document reproducible enough to commit and diff.
- **The standard library `uuid` package** generates request identifiers, with no
  third-party dependency for something the standard library now covers. Version
  7 identifiers sort chronologically, which turns a log store's index into a
  time index for free.
- **Goroutine leak profiling** is wired into the test suite, which asserts that
  request handling, the dependency container and the server lifecycle leave
  nothing running.
- **`httptest.NewTestServer`** backs the test client, including its in-memory
  network.

---

## Layout

```
muzak.dev/framework     the module root
|-- muzak.go           App, AppOptions, New, request dispatch
|-- router.go           Router, generic registration methods, options, Include
|-- context.go          request context, accessors, SetStatus
|-- binding.go          precompiled binding plans
|-- di.go               guards, providers, singletons, From
|-- lifecycle.go        parallel start, drain-ordered shutdown
|-- config.go           struct-tag configuration
|-- errors.go           the error envelope and its renderer
|-- logging.go          console and JSON handlers
|-- middleware.go       request ids, recovery, access log, CORS, headers
|-- ratelimit.go        quotas, storage, trackers, the per-route policy
|-- ratelimit_memory.go the bounded in-process counter table
|-- clientip.go         which address a request is attributed to
|-- auth.go             constant-time token guards
|-- openapi.go          OpenAPI 3.1 generation
|-- docs.go             embedded documentation UI
|-- server.go           timeouts, TLS, graceful shutdown
|-- websocket.go        the WSConn: messages, control frames, close, keepalive
|-- websocket_route.go  Router.WS, the handshake, options, the open connections
|-- websocket_client.go WSDial, the client half of the same engine
|-- sse.go              the SSEStream: events, encoding, keepalive, teardown
|-- sse_route.go        Router.SSE and SSEHandle, options, the open streams
|-- sse_client.go       SSEDial and the reader, the client half of that engine
|-- registry.go         the register of long-lived responses, and its bounds
|-- validation.go       the Validation entry points and field identity
|-- validate/           the rule sets: string, number, slice, time, value
|-- internal/radix/     the routing tree
|-- internal/wsframe/   the RFC 6455 frame codec: headers, masking, close codes
|-- testclient/         in-process client for tests
|-- docs/               the roadmap and the security review
`-- example/            runnable example service
    |-- cmd/main.go     composition, and nothing else
    |-- core/           configuration, dependencies, managed resources
    |-- schemas/        the request and response models the API exposes
    |-- handlers/       the functions that answer requests
    `-- routers/        which handler answers which path
```

The example's dependencies point one way. Routers know handlers, handlers know
schemas and core, and core knows nothing about any of them, so every package is
testable on its own and `cmd/main.go` does nothing but compose.

## Running things

```
go test ./...                                                  # tests
go test ./... -race                                            # race detector
go test ./... -bench=. -benchmem -run=XXX                      # benchmarks
golangci-lint run ./...                                        # linters

cd example && go run ./cmd                                        # the example, on :8080
```

### Test coverage

Coverage is generated rather than committed:

```
go test ./... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out
go tool cover -html=coverage.out -o coverage.html
```

**98.7% of statements** overall, measured on Go 1.27.0. Per package:

| Package | Statements covered |
|---|---:|
| `muzak.dev/framework` | 98.8% |
| `muzak.dev/framework/validate` | 99.3% |
| `muzak.dev/framework/internal/radix` | 100.0% |
| `muzak.dev/framework/internal/wsframe` | 100.0% |
| `muzak.dev/framework/testclient` | 93.2% |
| **total** | **98.7%** |

CI recomputes this on every push and fails the build if it drops below 98%, so
the badge cannot quietly go stale.

The uncovered statements are defensive branches that cannot be reached (a
`crypto/rand` failure, a marshal error on a fixed struct of strings) plus the
test client's own failure reporting, which would fail whichever test executed
it. Every one carries a `// coverage:` comment saying why, and a test enforces
that those comments contain a real justification rather than a bare marker.

### No dependencies

```
$ go list -m all | tail -n +2 | wc -l
0
```

Nothing is vendored and nothing is pulled in. Everything the framework needs is
in the standard library of Go 1.27, including `encoding/json/v2` and `uuid`.

## Where it differs from the design sketch

Two deviations, both forced and both flagged:

- **`Router.Options` does not exist.** `App.Options(...)` applies configuration
  to an application, and since `App` embeds `*Router` the two would shadow each
  other. OPTIONS is answered automatically with an `Allow` header, and
  `r.Handle(http.MethodOptions, ...)` registers one explicitly.
- **`/docs` serves Muzak's own documentation UI, not the Swagger UI
  distribution.** Vendoring Swagger UI would mean either a megabyte of
  third-party JavaScript in the repository or a CDN request on every page load.
  The built-in renderer reads the same OpenAPI document, fetches nothing
  external, and runs under a content security policy that forbids everything
  except its own nonce-tagged script.

---

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers the toolchain, the checks that have to
pass, and the rules the test suite enforces that are not obvious the first time.
Vulnerabilities go through [SECURITY.md](SECURITY.md) rather than a public issue.

## Licence

Dual-licensed under either of

- [MIT](LICENSE-MIT)
- [Apache License, Version 2.0](LICENSE-APACHE)

at your option. Unless you state otherwise, any contribution you intentionally
submit for inclusion shall be dual-licensed as above, with no additional terms.
