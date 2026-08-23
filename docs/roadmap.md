# Muzak build plan

Framework module lives in `framework/` (module path `muzak`, Go 1.27).
Example app and docs panels sit alongside it.

## Ground rules

- [x] Verify every Go 1.27 feature the spec assumes actually exists
- [x] Go 1.27.0 toolchain resolved via `GOTOOLCHAIN` from `go.mod`
- [x] Git repo, commits authored by muzakon <hasanmuzak@hotmail.com>
- [x] No em dash anywhere in source, docs or output (guarded by a test)
- [x] Commit each unit of work separately

## Framework packages

- [x] `internal/radix` segment trie, backtracking, wildcard, conflict errors
- [x] `errors.go` HTTPError, StatusCoder, error to response mapping
- [x] `context.go` request context, accessors, SetStatus, pooling reset
- [x] `binding.go` precompiled bind plans for path/query/header/cookie/body
- [x] `di.go` guards, value providers, singletons, From/TryFrom
- [x] `router.go` generic Get/Post/Put/Patch/Delete, options, Include, finalize
- [x] `muzak.go` App, AppOptions, New, dispatch, build
- [x] `logging.go` built-in console + JSON slog handlers, scopes, redaction
- [x] `middleware.go` recovery, request logging, request id, CORS deny by default
- [x] `server.go` timeouts, graceful shutdown
- [x] `openapi.go` OpenAPI 3.1 generation from In/Out at startup
- [x] `docs.go` self-contained documentation UI at /docs
- [x] `doc.go` package documentation with usage example

## Error envelope

Default shape, developer-overridable through `AppOptions.ErrorRenderer`:

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

- [x] `ErrorResponse` / `ErrorBody` / `ErrorDetail` types matching that shape
- [x] Machine readable `code` derived from status, overridable per error
- [x] `request_id` generated per request with the standard library `uuid`
- [x] `ErrorRenderer` hook on `AppOptions`, default exported as `DefaultErrorRenderer`
- [x] Binder issues phrased as `is required`, `must be a valid integer`

## Built-in logger

Aligned console format for humans, JSON for production, chosen automatically:

```
14:32:07.482 INFO  [Server]        Starting Muzak application...
14:32:07.485 INFO  [Router]        Registered 12 routes across 3 routers
14:32:07.492 INFO  [Server]        Listening on :8080
14:32:10.114 INFO  [UsersService]  user created  user_id=42 request_id=0611f4b2
```

- [x] `ConsoleHandler` with aligned time / level / scope / message columns
- [x] Colour when attached to a terminal, honours `NO_COLOR`
- [x] JSON handler for production, chosen automatically off a terminal
- [x] Secret redaction by attribute key, on by default
- [x] Pooled buffers, pre-rendered `WithAttrs`, single locked write per line
- [x] Framework start-up lines under the Server and Router scopes
- [x] Per-request access log with request id and duration

## Configuration

Struct-tag driven, loaded once at start-up, injected as a singleton:

```go
type Settings struct {
    AppName      string `env:"APP_NAME" default:"Awesome API"`
    AdminEmail   string `env:"ADMIN_EMAIL" required:"true"`
    ItemsPerUser int    `env:"ITEMS_PER_USER" default:"50"`
}

settings := muzak.MustLoadConfig[Settings](muzak.EnvFile(".env"))
app := muzak.New(muzak.AppOptions{}, muzak.WithSingleton(settings))
// inside a handler: s := muzak.From[Settings](ctx)
```

- [x] `config.go` with `LoadConfig[T]` and `MustLoadConfig[T]`
- [x] `env`, `default`, `required` and `secret` struct tags
- [x] `EnvFile(path)` dotenv source, real environment wins over the file
- [x] Reuse the binder's setters so config and request parsing agree on types
- [x] `WithSingleton(value)` to publish an already built value to every handler
- [x] Aggregate every missing or malformed variable into one error

## Test client

Mirrors `fastapi.testclient.TestClient`, over `net/http/httptest`:

```go
client := testclient.New(t, app)
res := client.Get("/items/foo", testclient.Header("X-Token", "coneofsilence"))
res.AssertStatus(200)
res.AssertJSON(`{"id":"foo","title":"Foo"}`)
```

- [x] `testclient` package wrapping an in-process server
- [x] `Get` / `Post` / `Put` / `Patch` / `Delete` with JSON body helpers
- [x] Response helpers: status, headers, raw body, decode into a typed value
- [x] Assertion helpers that report through `testing.TB`
- [x] Cookie jar so a login flow can be exercised across calls

## Testing

- [x] Table driven unit tests per package
- [x] Integration tests with `httptest.NewTestServer`
- [x] Fuzz tests for path matcher and binder
- [x] Goroutine leak assertions via the `goroutineleak` pprof profile
- [x] Race clean under `go test -race`
- [x] Statement coverage at 99.4%, every uncovered block carrying a written
      justification, enforced by a test. Reports are generated rather than
      committed, at your request
- [x] Example functions with verified `// Output:` comments

## Validation engine

Declarative, compile-time checked rules that also feed the OpenAPI document.
No tag strings and no field names as strings: `&in.Email` is the field, so a
rename is a compiler-checked refactor.

```go
func (in *CreateUser) Validate(v *muzak.Validation) {
    v.String(&in.Email).Trim().Lower().Required().Email()
    v.String(&in.Password).Required().MinLen(12).Must(NotACommonPassword)
    v.String(&in.Confirm).Equal(in.Password).Message("must match the password")
    v.Number(&in.Age).Between(18, 120)
    v.String(&in.Role).OneOf("admin", "editor", "viewer")
    v.Slice(&in.Tags).MaxItems(10).Each(validate.String().MaxLen(20))
}
```

- [x] `validate` package: rules, transforms, problems, constraints
- [x] `StringRules`: Required, Email, URL, UUID, MinLen, MaxLen, Len, Matches,
      OneOf, NotOneOf, Prefix, Suffix, Contains, Trim, Lower, Upper, Must
- [x] `NumberRules`: Min, Max, Between, Positive, Negative, MultipleOf, Must
- [x] `SliceRules[E]`: MinItems, MaxItems, Unique, Each, Must
- [x] `ValueRules[T]`: Required, OneOf, Equal, Must (the generic escape hatch)
- [x] `TimeRules`: Before, After, Between
- [x] `validation.go`: `Validation`, `Validatable`, generic entry points
- [x] Field identity from the pointer, offsets resolved once at start-up
- [x] Pointer fields via a union constraint; a nil pointer skips its rules
- [x] Optional fields skip their rules when empty unless `Required` is present
- [x] One failure reported per field, the first that fails
- [x] Transforms mutate and run before the checks
- [x] Cross-field with plain Go, plus `v.When(cond).Reject(&in.F, "...")`
- [x] `v.Nested(&in.Address)` for nested models, paths joined with a dot
- [x] Custom messages per rule with `Message`, field relabel with `As`
- [x] Validation runs after binding and after guards, never before
- [x] Failures merge into the existing 422 envelope with the right `location`
- [x] Rules work against every location, not just the body: a header, cookie,
      form value or path parameter is transformed and checked like any other
      field, reported under the name the client sent, and its constraints reach
      the OpenAPI parameter. Covered by `validation_located_test.go`
- [x] Fields that already failed binding are not validated again
- [x] `muzak.SkipValidation()` route option
- [x] Rules feed OpenAPI: minLength, maximum, enum, format and friends
- [x] Table-driven tests per rule, fuzz the whole pipeline, 100% coverage

## File uploads and forms

Multipart bound by the same rules as everything else, with the Go type deciding
what the handler is handed:

```go
type UploadFileIn struct {
    File muzak.File `file:"file" doc:"A file read as an upload"`
    Note string      `form:"note" required:"false"`
}

type MultiUploadIn struct {
    Files []muzak.File `file:"files"`
}

r.Post("/uploadfiles/", handlers.UploadFiles,
    muzak.MaxUploadSize(32<<20), muzak.MaxFileSize(10<<20))
```

- [x] `file` struct tag binding `[]byte`, `[][]byte`, `muzak.File`, `[]muzak.File`
- [x] `muzak.File` with Filename, ContentType, Size, Open, Bytes, Save, Header,
      Present, and an unexported part pointer so the content is never copied
      unless the handler asks for it
- [x] `form` struct tag for the values that share the body, reusing the binder's
      setters so a form value and a query parameter agree on what an `int` is
- [x] Files and form values are required by default, being body content, with
      `required:"false"` or a `default` to opt out
- [x] `MaxUploadSize` bounds the whole body and refuses it with 413 while it is
      being read; `MaxFileSize` bounds any single file inside it
- [x] Beyond 10 MiB a body spills to temporary files, removed when the handler
      returns rather than only when net/http finishes the response
- [x] Form-only routes also accept `application/x-www-form-urlencoded`, so a
      plain HTML form works; a route expecting a file does not, since urlencoded
      cannot carry one
- [x] A form value's requiredness comes from its tag and the binder enforces it,
      so validation rules may only add to the document's required list, never
      clear it. A sign-in form that constrains a password without declaring it
      required is still documented as demanding one
- [x] Form values are read from `PostForm`, so a query string cannot spoof one
- [x] Mixing a JSON body with `file` or `form` fields is a registration error,
      not a confusing runtime one
- [x] OpenAPI describes the body as `multipart/form-data` with binary strings,
      so the documentation UI offers a file picker
- [x] `muzak.HTML` as an output type, bypassing JSON and documented as
      `text/html`, for the pages that serve those forms

## Middleware

Installed by default, or added with one line:

```go
app.Use(muzak.Compress(muzak.CompressionOptions{}))
```

- [x] Built-in chain: request id, recovery, access log, security headers
- [x] `CORS` configured through `AppOptions.CORS`, denying every cross-origin
      request until a policy is written, and refusing a wildcard origin combined
      with credentials rather than serving a policy browsers reject
- [x] `Compress` negotiating gzip or deflate from `Accept-Encoding`, honouring
      an explicit `q=0` refusal, preferring gzip because every client that takes
      deflate takes gzip
- [x] Declines what compression would not help: under `MinSize`, an already
      compressed media type, an event stream, a range response, one the handler
      encoded itself, and one whose type nothing declared
- [x] A body whose length the handler never declared is held until it passes
      `MinSize`, so a streamed response is judged on what it actually sends
- [x] `Vary: Accept-Encoding` written at the point the header goes out, not
      before, so a handler setting its own `Vary` cannot replace it and leave a
      cache free to serve a compressed body to a client that cannot read it
- [x] Compressors pooled per policy, because a pooled one keeps the level it was
      built with and two policies would otherwise trade compressors
- [x] `Content-Length` dropped and a strong `ETag` weakened once the body is
      encoded, since neither describes the representation any more
- [x] Three compression levels as a named type, so an invalid level cannot be
      written down and there is no error to return at install time
- [x] BREACH written into the doc comment rather than left for the reader to
      remember: compressing a response that mixes a secret with client-controlled
      input leaks the secret over enough requests
- [x] 91% off the example application's OpenAPI document, 68% off its docs UI

## Frontend and static files

The static output of a frontend build, served by the same application:

```go
app.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
```

- [x] `Router.Frontend` on any router, so a mount inherits its prefix and guards
- [x] Routes matched first, so a mount at the root cannot shadow an API, and a
      method a route does not answer stays a 405 rather than falling through
- [x] `Dir` for a directory, `FS` for an `io/fs.FS`, both together for a
      subdirectory of one, which is what `go:embed` of a build output needs
- [x] Fallback resolved from what the build produced: `404.html` served with
      404, otherwise `index.html` served with 200 for a browser navigation
- [x] The single page fallback answers only a GET or HEAD that accepts HTML, so
      a missing script gets a 404 rather than a document it cannot parse
- [x] `Fallback`, `NotFound` and `NoFallback` to decide explicitly instead
- [x] Directories served by the `index.html` inside them, never listed
- [x] `os.OpenRoot` under the directory, so a symbolic link cannot lead out of
      the build output, and `fs.ValidPath` rejects a traversal before that
- [x] A missing directory, or a named fallback the build does not contain, is a
      build error; `SkipCheck` defers that to the first request for a directory
      something else fills in later
- [x] Conditional and range requests answered through `http.ServeContent`
- [x] A filesystem that fails mid-request answers rather than panicking, and
      says nothing about the server's filesystem while doing it
- [x] A method other than GET or HEAD on a file that exists is refused with 405
      and an Allow header, rather than answered with the file. A path that only
      the fallback covers stays a 404, because nothing is there to refuse
- [x] `Router.Static` on the same machinery without the fallback, which is the
      whole difference between publishing assets and serving an application
      whose routing happens in the browser
- [x] `StaticOptions.Index` for a directory served by its index.html, off by
      default because a mount of scripts and stylesheets has no index
- [x] Each mount names its own kind in a message, so a missing static directory
      is not reported as a missing frontend

## WebSockets

Our own engine, no library. A route is registered like any other and the
handler is handed the connection:

```go
type WSItemIn struct {
    ItemID string `path:"item_id"`
    Q      *int   `query:"q"`
}

r.WS("/items/{item_id}/ws", func(ctx *muzak.Context, in WSItemIn, conn *muzak.WSConn) error {
    session := muzak.From[SessionOrToken](ctx)
    for {
        msg, err := conn.ReadText(ctx.Context())
        if err != nil {
            return nil
        }
        conn.WriteText(ctx.Context(), "credential: "+session.Value)
        if in.Q != nil {
            conn.WriteText(ctx.Context(), fmt.Sprintf("q is %d", *in.Q))
        }
        conn.WriteText(ctx.Context(), fmt.Sprintf("you said %q, about item %s", msg, in.ItemID))
    }
}, muzak.Needs(GetSessionOrToken))
```

- [x] `internal/wsframe`: RFC 6455 header codec, word-at-a-time masking, close
      payloads, 100% coverage and three fuzz targets
- [x] Every rule the specification lays down enforced with the status it names:
      reserved opcodes and reserved bits, unmasked client frames, fragmented or
      oversized control frames, a length not minimally encoded, a continuation
      with nothing to continue, a message begun before the last one finished,
      text that is not UTF-8, a one byte close payload, a reserved close code,
      and a close reason that is not UTF-8
- [x] `Router.WS` on any router, so a WebSocket route inherits its prefix, tags,
      guards and dependencies like every other route
- [x] The handshake is an ordinary GET: middleware, guards, dependencies,
      binding and validation all run before a byte is upgraded, so a refusal is
      a JSON error rather than a socket that closes a moment later
- [x] An input type with a body field is a registration error, because a
      handshake carries no body and a field that silently never binds is worse
- [x] `WSConn` is message oriented: fragments are reassembled, interleaved
      control frames are handled, a ping is answered without the handler
      knowing, and a close is answered and reported as a `*WSCloseError`
- [x] Writes are serialised, so a broadcast from many goroutines cannot
      interleave two messages; reads are serialised too
- [x] `ReadLimit` refuses an oversized message before any of it is buffered, so
      a peer cannot choose how much memory the server spends, and there is
      deliberately no way to remove the limit
- [x] `WriteTimeout` bounds a write to a peer that stopped reading;
      `CloseGracePeriod` bounds the wait for the peer's own goodbye
- [x] Cross-origin handshakes refused by default. A handshake is not subject to
      the same-origin policy and is never preflighted, so `CORS` cannot cover it
- [x] No extension negotiated, so no peer can ask the server to hold
      decompression state on its behalf
- [x] Contexts drive both halves: a deadline is applied to the socket and a
      cancellation interrupts an operation already in progress, through
      `context.AfterFunc` rather than a goroutine per read
- [x] The listener's read and write deadlines are cleared on upgrade, or every
      connection would die at `WriteTimeout` no matter how healthy it was
- [x] Frames are read from the socket rather than through the reader the hijack
      returns, which reads through `net/http`'s own connection reader and would
      cancel the request context on the first read that timed out
- [x] Bytes a client sent in the same packet as the handshake are taken over
      rather than thrown away
- [x] Open connections are tracked so a graceful shutdown can tell each peer it
      is going away with 1001 and wait for the handlers, which `net/http` cannot
      do because a hijacked connection is no longer one of its own
- [x] `PingInterval` keepalive, off by default, that ends a connection whose
      peer stopped answering
- [x] Every response writer in the chain is told about the hijack, so a
      compressing wrapper does not try to finish a response that no longer exists
- [x] OpenAPI describes the handshake: parameters, 101, 426 and 422, with no
      request body and no 200
- [x] `WSDial` is the client half of the same engine, so a route is tested over
      a real connection rather than against a second implementation, and
      `testclient.Client.WS` wraps it
- [x] Tested against a hand-written client that shares no code with the engine,
      so a mistake in the codec cannot hide by being made twice; plus pipe-driven
      unit tests for the interleavings a real conversation cannot produce on
      demand, and a fuzz target that plays arbitrary bytes at the read path

### Hardening, with an attack per defence

Every row below is a test that plays the hostile peer, and every one of them was
checked by removing the defence and watching the test fail.

- [x] A message fragmented into empty continuation frames, or interleaved with
      an endless ping flood, never grows and so never meets the read limit. A
      bound on the frames one message may take ends both
- [x] A six byte header can declare a payload the size of the read limit. The
      payload is taken a chunk at a time as it arrives, so eight connections
      that send one byte each cost kilobytes rather than the 256 MiB the test
      measures without it
- [x] A message dribbled out a byte at a time is closed once `ReadTimeout`
      passes with it unfinished, while a connection idling between messages is
      left alone, because waiting is what most connections are for
- [x] `MaxConnections` bounds what one application holds, answering 503 with a
      Retry-After beyond it. It may only be set on the application, and a router
      or a route that sets it is refused when the application is built
- [x] A handshake carrying a body is refused, because whatever went unread would
      sit on the connection and be taken for frames once it was upgraded
- [x] A repeated `Sec-WebSocket-Key` or `Sec-WebSocket-Version` is refused, so
      this end and whatever sits in front of it cannot read different handshakes
- [x] Cross-site hijacking: a cross-origin handshake carrying the victim's
      cookies is refused before the handler runs, and the origin check is tested
      against ten shapes that have fooled one somewhere
- [x] Nothing a peer sends is echoed into a response header: only a subprotocol
      the route offered can be answered with, and one that is not a token is a
      build error rather than a sanitised value
- [x] A client-supplied value is cut down before it reaches an error message, so
      a header the size of the header limit cannot become a body that size
- [x] A handler's failure discloses nothing: the peer gets 1011 and the reason,
      the host and the query go to the log
- [x] The dialer never follows a redirect, which would send the handshake
      headers, an Authorization header among them, to whatever host answered
- [x] An attack storm of nine malformed shapes across many connections leaves no
      handler running, no connection in the register and no goroutine leaked
- [x] Concurrent peers never read each other's bytes, and a long conversation
      costs what one message costs rather than what all of them do, both
      measured against the live heap
- [x] A fuzz target throws arbitrary headers at the handshake, and one that is
      accepted has to have asked for websocket 13 in the way the specification
      requires

### Rate limiting

- [x] Nothing bounded how fast a peer could send. The limits above bound what
      any one message costs and how many connections one application holds, so
      no single request can run away with the process, but a peer that stays
      inside all of them and simply sends without pause was still spending the
      server's time. It is a policy question rather than a protocol one, and it
      is built as a policy: `RateLimitOptions` on an application, a router or a
      route, and `WSOptions.MessageLimits` for the WebSocket half

The decisions it needed, and what each of them was settled as:

- [x] What is counted. Requests, and for a WebSocket, messages. Bytes are
      already bounded by `MaxBodySize` and `ReadLimit`, and frames by the
      per-message frame cap, so what was left uncounted was the rate itself. A
      `Quota` is a name, a window and a limit, and a policy is several of them
      at once, because one number cannot tell a burst from sustained abuse.
      Every quota is counted for every request, so pausing between bursts does
      not launder a flood out of the long window
- [x] Whose budget it is. A `RateLimitTracker` decides, defaulting to the
      client address, and the address question is settled once for the whole
      framework rather than inside a WebSocket: `Context.ClientIP` walks a
      forwarding header right to left and believes an entry only when the hop
      that wrote it is named in `ClientIPOptions.TrustedProxies`. Nothing is
      believed by default. A tracker keying on an API key, a tenant or a
      resolved user replaces the default, and the budget belongs to the client
      rather than the connection, so a second connection does not double it
- [x] What happens at the limit. A request gets 429 with `Retry-After` and the
      `RateLimit` headers describing the whole policy. A WebSocket peer is
      closed with 1008 and told why. Refusing to read was rejected: it would
      have to be designed against the deadline a message is given once it has
      begun, and a peer that cannot tell a throttle from a hang is a peer that
      reconnects. Dropping messages silently was never offered
- [x] Where the state lives. `RateLimitStorage` is one method wide, taking a
      quota name, a key and a window and returning the new count and what is
      left of the window, so an application satisfies it with whatever it
      already runs. Redis does it in one script, Postgres in one upsert. A
      storage implementing `Lifecycle` is started and stopped with the
      application. A policy that names none is given a memory storage, which
      says plainly that it is a per-process limit
- [x] How the state is bounded. The memory storage holds at most `MaxEntries`
      counters, sweeps expired ones on a ticker and, when full, discards the
      counter closest to expiring, which is the one whose loss costs least. A
      tracker key whose length the client chose is hashed rather than
      truncated past 256 bytes, so two clients cannot be merged into one budget
- [x] What it looks like at the call site. `WithRateLimit` at any level,
      `RateLimit` to replace the quotas for one route, `SkipRateLimit` to
      exempt one. The check runs before the route's guards and dependencies, so
      a request a guard rejects is still counted, which is the half that
      matters for brute force; `AfterDependencies` moves it after them for a
      tracker that keys on a resolved user and gives that half up

What it deliberately does not cover:

- [ ] The documentation UI, the OpenAPI document and a static frontend mount are
      not routes, so no quota reaches them. A policy that must cover them is an
      ordinary middleware installed with `App.Use`
- [ ] A policy costs one storage round trip per quota per request. A shared
      storage with three quotas is three round trips, which a batching method on
      the interface could reduce to one; the interface was kept one method wide
      instead, because the shape an application has to implement is the thing
      worth keeping small

## Server-sent events

The other half of what a WebSocket is usually reached for, and the simpler half.
A route is registered like any other and the handler is handed the stream:

```go
r.SSE("/items/stream", func(ctx *muzak.Context, _ muzak.Empty, stream *muzak.SSEStream[schemas.ItemOut]) error {
    updates := store.Watch(stream.Context())
    for _, change := range store.Since(seen) {
        stream.SendEvent(muzak.SSEEvent[schemas.ItemOut]{Name: "item_update", ID: strconv.Itoa(change.Seq), Data: &item})
    }
    for {
        select {
        case <-stream.Context().Done():
            return nil
        case change := <-updates:
            // ...
        }
    }
}, muzak.WithSSE(muzak.SSEOptions{KeepAlive: 15 * time.Second, Retry: 2 * time.Second}))
```

- [x] Our own engine, no library: the event stream format written and parsed
      here, with the line splitting, the field rules and the dispatch rule the
      HTML specification lays down
- [x] `Router.SSE` on any router, so a stream route inherits its prefix, tags,
      guards and dependencies like every other route, and `Router.SSEHandle` for
      a stream reached by POST, which is what a protocol that streams its answer
      to a posted document needs
- [x] The stream is typed. `SSEStream[Out]` sends nothing but an Out, which the
      compiler enforces and the generated document describes under
      `text/event-stream`, exactly as a handler's return type describes a body
- [x] The request is an ordinary one: middleware, guards, dependencies, binding
      and validation all run before a byte of the stream is written, so a
      refusal is a JSON error rather than a stream that opens and shuts
- [x] `SSEEvent` carries a name to dispatch under, an identifier to resume from,
      a reconnection delay, a comment, and either a value encoded as JSON or
      text written as it stands, for a log line or a `[DONE]` sentinel
- [x] `stream.LastEventID()` reads what a browser sends back when its
      `EventSource` reconnects, which is what turns a dropped connection into a
      stream that picks up rather than one that starts again
- [x] Nothing on the stream takes a context, unlike `WSConn`: a stream belongs
      to one request, `stream.Context()` governs every send, and it is cancelled
      by the client going away, the request ending and the server shutting down
- [x] Writes are serialised, so a broadcast from many goroutines cannot
      interleave two events, and every event is assembled in full before any of
      it is written
- [x] `SSEOptions.KeepAlive` writes a comment to a stream that has said nothing,
      because a proxy closes a connection it believes to be idle and a silent
      stream is indistinguishable from a dead one
- [x] Open streams are tracked, so a graceful shutdown ends every one of them
      and waits for the handlers rather than waiting out its whole deadline once
      per stream. The register is shared with the WebSocket one, which needed it
      for the opposite reason: those connections are invisible to `net/http`
- [x] OpenAPI describes the stream: the parameters, a 200 whose
      `text/event-stream` content is the schema of one event's data, and no
      content at all for a stream of text that is not JSON
- [x] `SSEDial` and `SSEReader` are the reading half of the same engine, so a
      route is tested over a real connection rather than against a second
      implementation, and `testclient.Client.SSE` wraps them
- [x] Tested against streams written out by hand as well as against the engine,
      so a mistake in the format cannot hide by being made identically at both
      ends

### Hardening, with an attack per defence

Every row below is a test that plays the hostile client, and every one of them
was checked by removing the defence and watching the test fail.

- [x] A client that opens a stream and never reads it is given up on after
      `WriteTimeout`, rather than pinning a goroutine and a socket buffer that
      grows for as long as it cares to wait
- [x] `MaxStreams` bounds what one application serves, answering 503 with a
      Retry-After beyond it. Like the WebSocket limit it may only be set on the
      application, and a router or a route that sets it is refused at build time
- [x] The listener's write deadline is cleared for the stream and replaced with
      one per event, or every stream would die at `WriteTimeout` however healthy
      it was. The read deadline is cleared too, or the background read
      `net/http` makes to notice a client leaving would cancel the request, and
      with it the stream, at `ReadTimeout`
- [x] An event name or identifier carrying a line break or a null byte is
      refused rather than repaired, because a break would end its own field and
      let what followed be read as events of its own, which on a stream carrying
      one client's input to another is event forgery
- [x] A payload spanning several lines is written as several data lines, so a
      value cannot end its own field either, and a client joins them back into
      what was sent
- [x] Text that is not UTF-8 is refused, because that is the only encoding an
      event stream has
- [x] A goroutine the handler left behind cannot write into a response
      `net/http` has taken back: the stream waits for the write half before the
      handler's return is allowed to hand the response over, and every send
      afterwards reports that the stream has ended
- [x] A handler's failure discloses nothing: the stream ends and the reason goes
      to the log, while a stream that merely ended is not logged as a failure
- [x] A client-supplied value is cut down before it reaches an error message, so
      a header the size of the header limit cannot become a log line that size
- [x] The reader bounds what a server can make a client hold: one event may not
      exceed `ReadLimit`, whether it arrives as one long line or as many short
      ones, and an event that begins and never finishes ends the stream after
      `ReadTimeout`
- [x] The reader refuses a response that is not a 200 event stream rather than
      parsing one, because a reader that quietly accepts an HTML error page
      reports "no events" for what is actually a failure
- [x] A storm of streams opened and abandoned leaves no handler running, no
      stream in the register and no goroutine leaked

### What this found

- [x] The framework's own response wrapper swallowed flushes, because it
      implemented neither `Flush` nor `FlushError` and nothing above it could
      reach past it. Any handler streaming by hand behind `Compress` was
      therefore buffered until the response ended, which is a stream that is not
      a stream. Both spellings are forwarded now, and two tests read an event
      while the handler is still running so the wrapper cannot swallow one again

## Performance pass

Deferred until the rest was right. The numbers to beat are in BENCHMARKS.md.

- [x] Profile rather than guess: cpu and alloc profiles for a routed request
- [x] Validation was the target, at ~1.7us and 46 allocations per request.
      It is now ~0.77us and 11, a 76% reduction
- [x] Both, as it turned out: rules carry their parameters as data rather than
      closures, and the rule sets are recycled between requests. Neither alone
      helped; the first only pays off once the second keeps the slices warm
- [x] Written-out string and numeric paths, because passing the value pointer
      into an indirect call made the compiler heap-allocate it every request
- [x] Re-measure and rewrite BENCHMARKS.md with the new numbers
- [x] Keep every behaviour and every test intact; no semantics traded for speed
- [x] The profile caught a benchmark measuring an error path, which had produced
      a published claim that was wrong. Every benchmark now asserts the status it
      means to serve before its timed loop
- [ ] The request identifier is still most of the gap between 545.6 ns bare and
      1141 ns with the default middleware. Left alone: a version 7 UUID per
      request is the price of being able to correlate a response with its log
      line, and that is worth more than the nanoseconds

## Results

- Coverage 98.7% of statements overall, 98.8% of the framework package itself
  and 99.3% of `validate`, with `internal/radix` and `internal/wsframe` at 100%.
  Every uncovered block carries a written justification, enforced by a test
- `go build`, `go vet` and `golangci-lint run` all clean
- `go test ./... -race` clean, goroutine leak profile reports none
- All ten fuzz targets clean at 20s each
- Muzak's own layer: 451.8 ns/op vs 610.4 ns/op for a bare `http.ServeMux`
- A WebSocket message a handler sends with `ctx.Context()` allocates nothing

Defects the tests found and fixed along the way:

1. Struct walking skipped embedded fields of unexported type
2. A request body could overwrite a field bound from the query or path
3. Recovery appended an error envelope to a response already on the wire
4. `App.Addr` raced with `App.listen` over the server pointer
5. Pointer fields were described as non-nullable in generated schemas
6. A WebSocket read that timed out cancelled the request context, because the
   reader a hijack hands back still reads through net/http's own connection
   reader, which treats any read failure as the end of the request
7. A peer's close status was overwritten by the failure of the echo sent back
   to it, which a server is entitled to cause by closing the moment its own
   close frame is out
8. The register of open connections waited on a wait group in a goroutine,
   which the leak profile caught when a shutdown gave up on a stuck handler
9. A message could be fragmented or ping-flooded without end, because neither
   grows the message the read limit is measured against
10. A frame header could declare a payload the size of the read limit and have
    it committed to memory before a byte of it arrived
11. A handshake could carry a body, whose unread remainder would have been
    taken for the frames that followed the upgrade
12. The response wrapper every request is served through forwarded no flush, so
    a handler streaming by hand behind `Compress` was buffered until the
    response ended. The event stream work found it, because a stream is the one
    response for which that is fatal rather than merely slow

## Deliverables

- [x] `README.md` explaining the design philosophy
- [x] `BENCHMARKS.md` with routing, binding and JSON numbers
- [x] Example app reproducing users / items / admin, runnable with `go run .`
- [x] `go vet` and linter clean
