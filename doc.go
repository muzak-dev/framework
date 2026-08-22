// Package badele is a type-safe web framework for Go that brings FastAPI's
// developer experience to the language without giving up the compiler.
//
// A handler is an ordinary typed function. Its input type is the request, its
// return type is the response body, and both are checked when the program is
// compiled rather than when a request arrives:
//
//	type Params struct {
//		Username string `path:"username" doc:"The username to look up"`
//	}
//
//	type UserOut struct {
//		Username string `json:"username"`
//	}
//
//	r.Get("/users/{username}", func(ctx *badele.Context, in Params) (UserOut, error) {
//		return UserOut{Username: in.Username}, nil
//	})
//
// There is no wrapper type around the response and no filtering step at run
// time. What the handler returns is what the client receives, which means a
// field that should not be exposed cannot be exposed by accident: it is not
// part of the type.
//
// # Composing an application
//
// Routers are built independently and mounted where the application decides,
// which is the Go counterpart of FastAPI's APIRouter and include_router. A
// package exports its own routes and stays unaware of the prefix, tags and
// guards under which it will eventually run:
//
//	app := badele.New(badele.AppOptions{
//		Title:   "Bigger Applications Example",
//		Version: "1.0.0",
//		Addr:    ":8080",
//	}, badele.WithDependencies(GetQueryToken))
//
//	app.Include(users.NewRouter())
//	app.Include(items.NewRouter())
//	app.Include(admin.NewRouter(),
//		badele.WithPrefix("/admin"),
//		badele.WithTags("admin"),
//		badele.WithDependencies(GetTokenHeader),
//		badele.WithResponseDoc(418, "I'm a teapot"),
//	)
//
//	log.Fatal(app.RunSignals())
//
// App embeds [Router], so routes can be registered on it directly with the
// same generic methods any nested router uses.
//
// # Static declarations, imperative decisions
//
// Anything fixed for a route is declared once, at registration. Anything that
// depends on what happens at run time is decided in the handler. A status code
// that never varies is [Status]; one that does is [Context.SetStatus]. The two
// never compete, so returning a value and choosing a status stay independent
// concerns.
//
// # Dependencies
//
// Dependencies come in two shapes. A guard validates and produces nothing:
//
//	func GetQueryToken(ctx *badele.Context) error {
//		if ctx.Query("token") == "" {
//			return badele.NewHTTPError(400, "token is required")
//		}
//		return nil
//	}
//
// A provider produces a typed value, retrieved in the handler with [From] and
// checked by the compiler, with no cast anywhere in application code:
//
//	r.Get("/items/{id}", func(ctx *badele.Context, in Params) (ItemOut, error) {
//		user := badele.From[CurrentUser](ctx)
//		return ItemOut{ID: in.ID, Owner: user.Username}, nil
//	}, badele.Needs(GetCurrentUser))
//
// Guards attach to an application or a router with [WithDependencies] and
// cover everything beneath them; providers attach per route with [Needs].
// Resolved values live on the request's [Context] and are cleared when it is
// released, so two concurrent requests never see each other's values. Values
// that should outlive a request are published with [WithSingleton], and those
// that need opening and closing implement [Lifecycle].
//
// # Uploads and forms
//
// A field tagged `file:"name"` is bound from a multipart upload, and its Go
// type decides what the handler is handed. [File] carries the metadata and
// leaves the content where it is, which is what a large upload wants, while
// []byte reads it straight into memory:
//
//	type UploadFileIn struct {
//		File badele.File `file:"file" doc:"A file read as an upload"`
//	}
//
//	r.Post("/uploadfile/", func(ctx *badele.Context, in UploadFileIn) (UploadFileOut, error) {
//		return UploadFileOut{Filename: in.File.Filename}, nil
//	})
//
// Declaring the field as []badele.File, or as [][]byte, accepts every file
// sent under the name instead of one:
//
//	type MultiUploadIn struct {
//		Files []badele.File `file:"files"`
//	}
//
// A field tagged `form:"name"` is bound from a form value, converted by the
// same setters that convert a query parameter. A route may bind form values
// with no file at all, which is what a sign-in form is:
//
//	type LoginIn struct {
//		Username string `form:"username"`
//		Password string `form:"password"`
//	}
//
// Such a route accepts application/x-www-form-urlencoded as well as multipart,
// so a plain HTML form posts to it without an enctype. A route that binds a
// file accepts only multipart, because urlencoded cannot carry one. Files and
// form values are body content, so both are required unless the field carries
// `required:"false"` or a default. Two limits bound what a route accepts:
// [MaxUploadSize] for the whole body and [MaxFileSize] for any single file.
//
// A handler that returns [HTML] writes an HTML document instead of JSON, which
// is what serving an upload form from the same application takes:
//
//	r.Get("/", func(ctx *badele.Context, _ badele.Empty) (badele.HTML, error) {
//		return badele.HTML(`<form action="/files/" enctype="multipart/form-data" method="post">` +
//			`<input name="files" type="file" multiple><input type="submit"></form>`), nil
//	})
//
// # Middleware
//
// The built-in chain assigns a request identifier, recovers panics, writes the
// access log and sets the security headers before any route runs. [App.Use]
// installs more inside that chain, so anything added there already has an
// identifier and is already covered by recovery:
//
//	app.Use(badele.Compress(badele.CompressionOptions{}))
//
// Two are ready to use. [CORS] is configured rather than installed: a policy on
// [AppOptions.CORS] installs it, and no policy at all means no CORS header is
// ever emitted, so a browser refuses every cross-origin read until the policy
// is written down. A wildcard origin combined with credentials is refused as a
// configuration error rather than served.
//
// [Compress] negotiates gzip or deflate from Accept-Encoding and leaves alone
// what is not worth compressing: a body under [DefaultCompressionMinSize], a
// media type that is already compressed, an event stream, a range, and
// anything the handler encoded itself. Vary records the dependency on every
// response either way, so a cache cannot hand a compressed body to a client
// that cannot read it.
//
// Anything else is an ordinary func(http.Handler) http.Handler, so writing one
// takes no framework knowledge. One thing does differ from frameworks in other
// languages, and it fails quietly: Go puts the header block on the wire at the
// first WriteHeader, so a header set after the next handler returns is
// dropped without a word. Middleware that reports something only known at the
// end, such as how long the request took, has to wrap the writer and fill the
// value in as the response starts.
//
// # Rate limiting
//
// Rate limiting is built in and off until a policy names a [Quota]. A policy is
// several quotas at once, because one number cannot tell a burst from sustained
// abuse:
//
//	app := badele.New(badele.AppOptions{Title: "Shop"},
//		badele.WithRateLimit(badele.RateLimitOptions{
//			Storage: NewRedisRateLimitStorage(settings.RedisAddr),
//			Tracker: UserOrIPTracker,
//			Quotas: []badele.Quota{
//				{Name: "short", Window: time.Second, Limit: 3},
//				{Name: "medium", Window: 10 * time.Second, Limit: 20},
//				{Name: "long", Window: time.Minute, Limit: 100},
//			},
//		}),
//	)
//
// Every quota is counted for every request, so a client that overruns the short
// window still accrues against the long one and cannot launder a flood by
// pausing between bursts. A refused request is answered with 429, a Retry-After
// and the RateLimit headers describing the whole policy.
//
// Three decisions are the application's. [RateLimitStorage] says where the
// counters live, defaulting to a bounded in-process table that is right for one
// process and wrong for several. [RateLimitTracker] says whose budget a request
// is spent from, defaulting to [IPTracker], and is where an API key, a tenant
// or a resolved user identity belongs instead. [ClientIPOptions] says which
// address a request is attributed to, believing no forwarding header until a
// proxy is named, because a header any client can write is a budget any client
// can escape.
//
// Narrowing works like everything else here. [RateLimit] replaces the quotas
// for one route, which is what a login route wants, and [SkipRateLimit] exempts
// one, which is what a health check wants:
//
//	r.Get("/health", health, badele.SkipRateLimit())
//	r.Post("/login", login, badele.RateLimit(badele.Quota{Name: "login", Window: time.Minute, Limit: 5}))
//
// The count happens before the route's guards and dependencies, so a client
// past its limit is refused before anything expensive runs on its behalf and a
// request a guard rejects is still counted, which is the half that matters for
// brute force. [RateLimitOptions.AfterDependencies] moves it after them, for a
// tracker that keys on an identity a dependency produced, and gives up the
// other half. A storage that cannot answer refuses the request with 503 unless
// [RateLimitOptions.FailOpen] trades that for availability.
//
// [WSOptions.MessageLimits] applies the same quotas, storage and tracker to the
// messages a connected peer sends, which is the one thing the other WebSocket
// bounds do not cover.
//
// # WebSockets
//
// [Router.WS] registers a WebSocket route. The handshake is an ordinary GET,
// so everything that applies to a route applies to it: middleware runs, guards
// run, dependencies resolve, and the input struct is bound and validated
// before a single byte is upgraded. What is different is the third argument,
// which is the connection the handler owns until it returns:
//
//	type WSItemIn struct {
//		ItemID string `path:"item_id"`
//		Q      *int   `query:"q"`
//	}
//
//	r.WS("/items/{item_id}/ws", func(ctx *badele.Context, in WSItemIn, conn *badele.WSConn) error {
//		session := badele.From[SessionOrToken](ctx)
//		for {
//			message, err := conn.ReadText(ctx.Context())
//			if err != nil {
//				return nil
//			}
//			if err := conn.WriteText(ctx.Context(), "you said "+message); err != nil {
//				return err
//			}
//			_ = session
//		}
//	}, badele.Needs(GetSessionOrToken))
//
// A request that fails to bind, or a guard that refuses, is answered with the
// usual JSON error and never becomes a connection at all, which is what makes
// a rejection something a client can read rather than a socket that closes a
// moment after it opened.
//
// Reading and writing are message oriented: a message split across frames is
// delivered once and whole, a ping is answered without the handler knowing,
// and a close is answered and then reported as a *[WSCloseError], which is why
// the loop above ends on any error. Writes are serialized, so any number of
// goroutines may write to one connection. The protocol is implemented here
// rather than delegated: RFC 6455 framing, masking, UTF-8 validation and the
// close handshake, with every rule the specification lays down enforced and
// every violation answered with the status it calls for.
//
// # What a hostile peer cannot do
//
// A WebSocket is the longest-lived thing an unauthenticated stranger can ask a
// server for, so every direction a peer controls is bounded, and each bound is
// there to stop something specific.
//
//   - A message larger than [WSOptions.ReadLimit] is refused before any of it
//     is buffered, and a frame is taken a chunk at a time as the bytes arrive,
//     so a six byte header cannot buy an allocation the size of the limit.
//   - A message that begins and does not finish is closed after
//     [WSOptions.ReadTimeout], which is what a peer dribbling one out a byte at
//     a time looks like. Waiting between messages is not bounded, because
//     waiting is what most connections are for.
//   - A message fragmented endlessly, or interleaved with an endless run of
//     pings, is closed once too many frames have arrived without one
//     completing. Neither grows the message, so no size limit would ever catch
//     them.
//   - A write to a peer that has stopped reading gives up after
//     [WSOptions.WriteTimeout] rather than pinning a goroutine and a buffer.
//   - The application holds at most [WSOptions.MaxConnections] connections at
//     once, and answers 503 with a Retry-After beyond that, because file
//     descriptors run out before anything else does.
//   - A handshake from another origin is refused outright, because a WebSocket
//     handshake is not subject to the same-origin policy and is never
//     preflighted, which is what makes cross-site hijacking possible in the
//     first place. [AppOptions.CORS] does not cover it and never could.
//   - A handshake carrying a body is refused, because whatever went unread
//     would sit on the connection and be taken for frames the moment it was
//     upgraded.
//   - No extension is negotiated, so no peer can ask the server to keep
//     decompression state on its behalf.
//
// Nothing a peer sends is echoed into a response header: only a subprotocol the
// route itself offered can be answered with, and a route that offers one which
// is not a token is refused when the application is built. Nothing a handler
// fails with is disclosed either; the peer is closed with
// [WSStatusInternalError] and the reason goes to the log.
//
// Configure the rest with [WithWebSocket], and see [WSOptions] for what each
// limit is there to stop.
//
// [WSDial] is the other end of the same engine, which is what lets a route be
// tested over a real connection rather than against a second implementation.
// It checks what a server answers rather than trusting it, and never follows a
// redirect, because following one would send the headers of the handshake to
// whatever host the answer named. The test client wraps it as
// [badele/testclient.Client.WS].
//
// # Server-sent events
//
// [Router.SSE] registers a route whose response is a stream rather than a body.
// It is the other half of what a WebSocket is usually reached for, and it is
// the simpler half: the server sends, the client listens, and a browser reads
// it natively with EventSource, reconnecting on its own when the stream drops.
//
//	type StreamIn struct {
//		Room string `path:"room"`
//	}
//
//	r.SSE("/rooms/{room}/stream", func(ctx *badele.Context, in StreamIn, stream *badele.SSEStream[MessageOut]) error {
//		for message := range room(in.Room).Messages(stream.Context()) {
//			if err := stream.Send(message); err != nil {
//				return err
//			}
//		}
//		return nil
//	})
//
// The type parameter is the contract: nothing but a MessageOut can be sent, and
// the generated document describes the stream with that type, in the same way a
// handler's return type describes an ordinary response. Everything else about
// the route is ordinary too, so middleware runs, guards run, dependencies
// resolve, and the input is bound and validated before a byte of the stream is
// written. A request that fails any of that is answered with the usual JSON
// error and never becomes a stream at all.
//
// Nothing here takes a context, unlike [WSConn], because a stream belongs to
// one request: [SSEStream.Context] governs every send and is cancelled when the
// client disconnects or the server begins shutting down, so a handler watches
// one thing and every send after it reports [ErrSSEStreamEnded]. Writes are
// serialized, so any number of goroutines may write to one stream.
//
// [SSEStream.SendEvent] carries what a bare value cannot: a name to dispatch
// under, an identifier to resume from, a reconnection delay, or a payload that
// is not JSON, such as the "[DONE]" sentinel some protocols end with. A browser
// sends the last identifier it saw back in the Last-Event-ID header when it
// reconnects, which [SSEStream.LastEventID] reads, and that is what turns a
// dropped connection into a stream that picks up where it left off.
//
// A stream is not tied to GET. [Router.SSEHandle] registers one for any method,
// which is what a protocol that streams its answer to a posted document needs,
// and there the input binds a request body like any other route.
//
// # What a stream bounds
//
// An event stream costs a connection and a goroutine for as long as a client
// cares to hold it, so the same reasoning applies as to a WebSocket.
//
//   - A client that stops reading is given up on after
//     [SSEOptions.WriteTimeout], rather than pinning a goroutine and a growing
//     socket buffer for as long as it likes.
//   - The application serves at most [SSEOptions.MaxStreams] streams at once,
//     and answers 503 with a Retry-After beyond that.
//   - The listener's own timeouts are cleared for the stream and replaced with
//     a deadline per event, because a stream is a response that does not end
//     and would otherwise die at [ServerOptions.WriteTimeout] however healthy
//     it was. The read deadline goes too: it would cancel the request, and with
//     it the stream, at [ServerOptions.ReadTimeout] and blame the client.
//   - An event name or identifier carrying a line break is refused rather than
//     repaired. An event stream is a sequence of lines, so a break in one of
//     those fields would end it and let whatever followed be read as fields of
//     its own, which on a stream carrying one client's input to another is
//     event forgery.
//   - A payload spanning several lines is written as several data lines and
//     arrives whole, which is both what the format asks for and what stops a
//     value from ending its own field.
//   - A comment goes out every [SSEOptions.KeepAlive] on a stream that has said
//     nothing, because a proxy that sees an idle connection for long enough
//     closes it, and because a silent stream is indistinguishable from a dead
//     one.
//   - Nothing a handler fails with is disclosed: the stream ends and the reason
//     goes to the log. The response header was written before the handler ran,
//     which is what lets a client see the stream open immediately, so anything
//     that decides whether to serve a stream at all belongs in a guard or a
//     dependency, where there is still a response to say it in.
//
// Compression leaves an event stream alone, because holding events in a
// compressor's window until something forces them out is the one thing a stream
// cannot survive. The origin policy a WebSocket needs has no counterpart here
// either: an EventSource is subject to the same-origin policy and to CORS like
// any other request, so [AppOptions.CORS] governs it.
//
// [SSEDial] is the reading end of the same engine, so a stream route is tested
// over a real connection rather than against a second implementation, and the
// test client wraps it as [badele/testclient.Client.SSE].
//
// # Serving a frontend
//
// [Router.Frontend] serves the static output of a frontend build, which is what
// React, Vue, Svelte, Angular, Solid and Astro produce:
//
//	app.Frontend("/", badele.FrontendOptions{Dir: "dist"})
//
// Routes win. A request is matched against every registered route first and
// reaches the frontend only when none of them answered, so mounting at the root
// cannot shadow an API. Middleware applies, and so do the guards of the router
// the frontend was registered on, which is what lets a frontend sit behind the
// same authentication as everything else.
//
// A path with no file behind it falls back to one, chosen from what the build
// produced: a 404.html is served with 404, and failing that an index.html is
// served with 200 for a browser navigation, which is what a client-side router
// needs in order to take over. A missing script or stylesheet still answers
// 404, because handing those an HTML document turns a missing file into a parse
// error somewhere further from the cause. Name the file with
// [FrontendOptions.Fallback] or [FrontendOptions.NotFound] to decide instead,
// or set [FrontendOptions.NoFallback] for a plain 404.
//
// [FrontendOptions.FS] serves the frontend from an [io/fs.FS] rather than from
// disk, which is how it gets built into the binary and the deployment becomes
// one file:
//
//	//go:embed all:dist
//	var assets embed.FS
//
//	app.Frontend("/", badele.FrontendOptions{FS: assets, Dir: "dist"})
//
// Nothing is rendered on the server and nothing is built here. A directory is
// never listed, a symbolic link cannot lead out of the build output, a method
// other than GET or HEAD on a file is refused with 405 rather than served, and
// a directory that does not exist is reported when the application is built
// rather than on the first request.
//
// [Router.Static] mounts a directory of files on the same machinery, without
// the part that makes a frontend work:
//
//	app.Static("/static", badele.StaticOptions{Dir: "static"})
//
// Nothing stands in for a path with no file behind it, so a miss is a 404 and
// stays one, and a directory is served by its index.html only when
// [StaticOptions.Index] asks. That is the whole difference: reach for Static to
// publish assets, and for Frontend to serve an application whose routing
// happens in the browser.
//
// # What is generated
//
// The OpenAPI 3.1 document at /openapi.json and the documentation UI at /docs
// are derived from the registrations themselves: path templates, tags,
// summaries, the schemas of the In and Out types, declared statuses and the
// entries added by [WithResponseDoc]. All of that reflection happens once,
// while the application is being built. Nothing on the request path inspects a
// type, because the binding plan and the response schema were both compiled at
// start-up.
//
// # Errors
//
// Every failure renders as one envelope, carrying a machine-readable code, a
// message safe to disclose, the status, per-field details and the request
// identifier that ties the response to the server's log:
//
//	{
//	  "error": {
//	    "code": "validation_error",
//	    "message": "The request could not be validated.",
//	    "status": 422,
//	    "details": [
//	      { "field": "limit", "location": "query", "issue": "must be a valid integer" }
//	    ]
//	  },
//	  "request_id": "0611f4b2-2f0a-4b57-9c1a-6e6a2e2f9b31"
//	}
//
// An error that describes itself, such as one from [NewHTTPError], reaches the
// client as written. Anything else becomes an opaque 500 with the real cause
// logged and never transmitted. Replace the shape entirely with
// [AppOptions.ErrorRenderer].
//
// # Defaults worth knowing
//
// Badele starts from settings that are safe rather than permissive. Every
// listener timeout is non-zero, request bodies are capped at one mebibyte and
// uploads at 32, WebSocket messages at one mebibyte, unknown JSON members are
// rejected, duplicate members and invalid UTF-8 are refused by
// encoding/json/v2, CORS denies every cross-origin request until it is
// configured, a WebSocket handshake from another origin is refused until it is
// allowed, the connections and the event streams one application holds are both
// capped, a write to a client that stopped reading gives up, no forwarding
// header is believed until a proxy is named, and a panic becomes a generic 500
// with the stack recorded only in the log. Each of these can be relaxed
// deliberately; none of them is relaxed by omission.
//
// Rate limiting is the deliberate exception, and is off until a quota is
// declared. There is no limit that is right for every application, and a
// default one would be a number nobody chose refusing traffic nobody expected.
// What is safe by default is what happens once one is declared: the counters
// are bounded, the address is not taken from a header anyone can write, and a
// storage that stops answering stops traffic rather than stopping the limit.
//
// # Testing
//
// The badele/testclient package serves an application in-process and issues
// real requests against it, so a test exercises middleware, routing, binding,
// dependencies and error rendering together rather than any one of them in
// isolation.
package badele
