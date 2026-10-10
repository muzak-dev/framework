// Package muzak is a type-safe web framework for Go. A route is declared once
// and the compiler checks it: what a handler accepts, what it returns and what
// the documentation says are the same thing rather than three that have to be
// kept in step.
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
//	r.Get("/users/{username}", func(ctx *muzak.Context, in Params) (UserOut, error) {
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
// so a package exports its own routes and stays unaware of the prefix, tags and
// guards under which it will eventually run:
//
//	app := muzak.New(muzak.AppOptions{
//		Title:   "Bigger Applications Example",
//		Version: "1.0.0",
//		Addr:    ":8080",
//	}, muzak.WithDependencies(GetQueryToken))
//
//	app.Include(users.NewRouter())
//	app.Include(items.NewRouter())
//	app.Include(admin.NewRouter(),
//		muzak.WithPrefix("/admin"),
//		muzak.WithTags("admin"),
//		muzak.WithDependencies(GetTokenHeader),
//		muzak.WithResponseDoc(418, "I'm a teapot"),
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
//	func GetQueryToken(ctx *muzak.Context) error {
//		if ctx.Query("token") == "" {
//			return muzak.NewHTTPError(400, "token is required")
//		}
//		return nil
//	}
//
// A provider produces a typed value, retrieved in the handler with [From] and
// checked by the compiler, with no cast anywhere in application code:
//
//	r.Get("/items/{id}", func(ctx *muzak.Context, in Params) (ItemOut, error) {
//		user := muzak.From[CurrentUser](ctx)
//		return ItemOut{ID: in.ID, Owner: user.Username}, nil
//	}, muzak.Needs(GetCurrentUser))
//
// Guards attach to an application or a router with [WithDependencies] and
// cover everything beneath them; providers attach per route with [Needs].
// Resolved values live on the request's [Context] and are cleared when it is
// released, so two concurrent requests never see each other's values. Values
// that should outlive a request are published with [WithSingleton], and those
// that need opening and closing implement [Lifecycle].
//
// A route that runs any guard or provider, its own or one it inherited, is
// presumed to answer per user, so its response is sent with
// "Cache-Control: private, no-cache" unless a middleware or the handler set a
// Cache-Control of its own. A handler serving something every user may share
// says so with [Context.SetHeader].
//
// A value can also be declared on the input type, as a [Dep] field, and read
// with Get. The route is then refused at build time, naming the field, if no
// provider of exactly that type is declared for it, rather than failing on its
// first request; the field is never read from the request and never appears
// in the OpenAPI document:
//
//	type ReadItem struct {
//		ID   string                 `path:"id"`
//		User muzak.Dep[CurrentUser]
//	}
//
// A provider that has to clean up after the request is declared with
// [Acquire], which returns a [Release] beside the value. Releases run once
// each, last acquired first, on every way a request can end, and are told
// whether it failed; for a buffered response they run before anything is
// written, so a release that fails turns a success into an error.
// [Transaction] is the one most applications need: a *sql.Tx begun for the
// request, committed if it succeeded and rolled back otherwise.
//
// A test replaces a provider with [App.Override], or testclient.Override,
// before the application is built. An override replaces every provider of its
// type in that application alone, keeps the lifetime of the provider it
// stands in for, and leaves guards alone.
//
// # Uploads and forms
//
// A field tagged `file:"name"` is bound from a multipart upload, and its Go
// type decides what the handler is handed. [File] carries the metadata and
// leaves the content where it is, which is what a large upload wants, while
// []byte reads it straight into memory:
//
//	type UploadFileIn struct {
//		File muzak.File `file:"file" doc:"A file read as an upload"`
//	}
//
//	r.Post("/uploadfile/", func(ctx *muzak.Context, in UploadFileIn) (UploadFileOut, error) {
//		return UploadFileOut{Filename: in.File.Filename}, nil
//	})
//
// Declaring the field as []muzak.File, or as [][]byte, accepts every file
// sent under the name instead of one:
//
//	type MultiUploadIn struct {
//		Files []muzak.File `file:"files"`
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
// [MaxUploadSize] for the whole of a multipart body and [MaxFileSize] for any
// single file. A urlencoded body carries no file and is read into memory
// whole, so it is bounded by [MaxBodySize] like a JSON body.
//
// A handler that returns [HTML] writes an HTML document instead of JSON, which
// is what serving an upload form from the same application takes:
//
//	r.Get("/", func(ctx *muzak.Context, _ muzak.Empty) (muzak.HTML, error) {
//		return muzak.HTML(`<form action="/files/" enctype="multipart/form-data" method="post">` +
//			`<input name="files" type="file" multiple><input type="submit"></form>`), nil
//	})
//
// # Middleware
//
// The built-in chain assigns a request identifier, recovers panics, writes the
// access log and sets the security headers before any route runs; see
// [SecurityHeaders] for which, including the Strict-Transport-Security a
// request that arrived over TLS is answered with, and for why no
// Content-Security-Policy is among them. [App.Use]
// installs more inside that chain, so anything added there already has an
// identifier and is already covered by recovery:
//
//	app.Use(muzak.Compress(muzak.CompressionOptions{}))
//
// Two are ready to use. [CORS] is configured rather than installed: a policy on
// [AppOptions.CORS] installs it, and no policy at all means no CORS header is
// ever emitted, so a browser refuses every cross-origin read until the policy
// is written down. A wildcard origin combined with credentials is refused as a
// configuration error rather than served, and so is an allowed origin that
// could never match the Origin a browser sends, "https://app.example.com/" or
// "*.example.com", or that any page can send, "null". A wildcard policy sends
// "Access-Control-Allow-Origin: *" on every response, including one to a
// request with no Origin, so that a copy a cache kept serves every origin
// alike.
//
// [Compress] negotiates gzip or deflate from Accept-Encoding and leaves alone
// what is not worth compressing: a body under [DefaultCompressionMinSize], a
// media type that is already compressed, an event stream, a range, and
// anything the handler encoded itself. Vary records the dependency on every
// response either way, so a cache cannot hand a compressed body to a client
// that cannot read it. A compressed response carries its strong ETag weakened,
// as does a 304 to a client that negotiated an encoding, and no Accept-Ranges;
// on the way in, an If-Match tag gets its strong form back so that conditional
// writes still match, and a Range whose If-Range is a date rather than a strong
// tag is answered with the whole content, since the date cannot say which
// representation a resuming client holds.
//
// Anything else is an ordinary func(http.Handler) http.Handler, so writing one
// takes no framework knowledge. One thing does differ from frameworks in other
// languages, and it fails quietly: Go puts the header block on the wire at the
// first WriteHeader, so a header set after the next handler returns is
// dropped without a word. Middleware that reports something only known at the
// end, such as how long the request took, has to wrap the writer and fill the
// value in as the response starts.
//
// # What a route's guards do not cover
//
// A route's guards, providers and rate limit run when the route is called, and
// a request that never reaches the route never runs them. Two kinds of request
// do not: an OPTIONS request to a path that has routes, which is answered 204
// with an Allow header, and a request whose method the path has no route for,
// which is answered 405 with the same header. Both are answered from the
// route table alone, exactly as net/http.ServeMux does, and both stay in reach
// of what [App.Use] installs, since middleware wraps every request.
//
// What that discloses is which paths exist, and which methods they take,
// to a client who has not authenticated and whom no route quota counts: 405
// against 404 tells a client that "/admin/export" is a real path. That is
// route structure, which the OpenAPI document publishes anyway unless it is
// turned off with [AppOptions.DisableDocs] or put behind a guard, and it is
// never a route's data. A deployment for which the existence of a path is
// itself a secret puts a guard or a limit in middleware installed with
// [App.Use], where it covers these answers too.
//
// A route's guards do not cover a wildcard beside it either. Paths are matched
// as they arrive and nothing normalises them, so with a guarded "/admin/panel"
// and a public "/{rest...}", the requests "//admin/panel", "/admin//panel",
// "/admin/panel/", "/ADMIN/panel", "/./admin/panel", "/x/../admin/panel",
// "/%2e/admin/panel" and "/admin%2Fpanel" all reach the public wildcard, the
// last with rest set to "admin/panel". The guarded route is never served
// without its guard, which is also how net/http.ServeMux behaves. The risk is
// a wildcard handler that acts on the captured value: one that serves files,
// fetches records or proxies by it has to check authorization against what
// the value refers to, and must never pass it to a backend that cleans,
// decodes or case-folds paths, since that backend may turn it back into the
// guarded resource.
//
// # Rate limiting
//
// Rate limiting is built in and off until a policy names a [Quota]. A policy is
// several quotas at once, because one number cannot tell a burst from sustained
// abuse:
//
//	app := muzak.New(muzak.AppOptions{Title: "Shop"},
//		muzak.WithRateLimit(muzak.RateLimitOptions{
//			Storage: NewRedisRateLimitStorage(settings.RedisAddr),
//			Tracker: UserOrIPTracker,
//			Quotas: []muzak.Quota{
//				{Name: "short", Window: time.Second, Limit: 3},
//				{Name: "medium", Window: 10 * time.Second, Limit: 20},
//				{Name: "long", Window: time.Minute, Limit: 100},
//			},
//		}),
//	)
//
// Every quota is counted for every request that is served. They are counted
// longest window first and the first to refuse a request ends the count, so a
// client that overruns the short window has still been counted against the
// long one and cannot launder a flood by pausing between bursts, while a
// request the long one refuses spends nothing of the shorter ones. A refused
// request is answered with 429, a Retry-After for the quota that refused it
// and the RateLimit headers describing the whole policy.
//
// Three decisions are the application's. [RateLimitStorage] says where the
// counters live, defaulting to a bounded in-process table that is right for one
// process and wrong for several. [RateLimitTracker] says whose budget a request
// is spent from, defaulting to [IPTracker], which counts an IPv4 address on
// its own and an IPv6 address by its /56, so that a client cannot mint a fresh
// budget from its own range; it is where an API key, a tenant or a resolved
// user identity belongs instead. [ClientIPOptions] says which
// address a request is attributed to, believing no forwarding header until a
// proxy is named, because a header any client can write is a budget any client
// can escape.
//
// Narrowing works like everything else here. [RateLimit] replaces the quotas
// for one route, which is what a login route wants, and [SkipRateLimit] exempts
// one, which is what a health check wants:
//
//	r.Get("/health", health, muzak.SkipRateLimit())
//	r.Post("/login", login, muzak.RateLimit(muzak.Quota{Name: "login", Window: time.Minute, Limit: 5}))
//
// The count happens before the route's guards and dependencies, so a client
// past its limit is refused before anything expensive runs on its behalf and a
// request a guard rejects is still counted, which is the half that matters for
// brute force. [RateLimitOptions.AfterDependencies] moves it after them, for a
// tracker that keys on an identity a dependency produced, and gives up the
// other half. A storage that cannot answer refuses the request with 503 and a
// Retry-After unless [RateLimitOptions.FailOpen] trades that for availability.
//
// [WSOptions.MessageLimits] applies the same quotas, storage and tracker to the
// messages a connected peer sends, which is the one thing the other WebSocket
// bounds do not cover.
//
// # Versioning
//
// Versioning is off until [AppOptions.Versioning] names a [VersioningType]. A
// route or router opts into a version with [WithVersion], and a route without
// one answers no request at all once versioning is on, unless
// [VersioningOptions.DefaultVersion] supplies it or the route is deliberately
// marked [VersionNeutral], which answers every version, including a request
// naming none:
//
//	app := muzak.New(muzak.AppOptions{
//		Versioning: muzak.VersioningOptions{Type: muzak.VersioningURI},
//	})
//	r.Get("/cats", findAllV1, muzak.WithVersion("1"))
//	r.Get("/cats", findAllV2, muzak.WithVersion("2"))
//	r.Get("/health", health, muzak.WithVersion(muzak.VersionNeutral))
//
// [VersioningURI] reads the version from the path itself, inserting a prefix
// ("v" by default; see [VersioningOptions.Prefix]) in front of every route
// that declares one, so findAllV1 above answers "/v1/cats". A route naming
// more than one version is registered once per version, each at its own path,
// because there the version is part of routing rather than something read
// off the request. [VersioningHeader], [VersioningMediaType] and
// [VersioningCustom] instead leave the path alone and read the version from a
// header, from a parameter of the Accept header, or from
// [VersioningOptions.Extractor], matching whichever registered route answers
// it; more than one such route may share a path so long as their versions
// never overlap, which is checked when the application is built.
// [VersioningCustom] alone can offer several versions in order of
// preference, matched from most to least preferred against whatever a route
// actually answers. A response chosen by a header names that header in Vary,
// the 404 for a version nothing answers included, so a shared cache keeps one
// answer per version; [VersioningCustom] cannot know what its extractor reads,
// so there the application adds it to Vary itself.
//
// # Internationalization
//
// A request's locale is resolved once, before the handler runs, and every
// message the response carries is written in it. That includes the ones Muzak
// produces itself: the wording of each validation rule, what the binder says
// about a value it could not read, and the sentence behind each HTTP status.
//
//	//go:embed locales
//	var locales embed.FS
//
//	app := muzak.New(muzak.AppOptions{
//		I18n: muzak.I18nOptions{Store: i18n.MustLoad(locales, "locales")},
//	})
//
// A handler translates through its context, which already knows the locale:
//
//	func greet(ctx *muzak.Context, in Params) (Out, error) {
//		return Out{
//			Greeting: ctx.T("greeting.hello", "name", in.Name),
//			Items:    ctx.T("greeting.items", "count", in.Items),
//			Today:    ctx.L(time.Now(), "as", "date"),
//		}, nil
//	}
//
// The count both prints and chooses: "no items", "one item" and "5 items" are
// three entries in the locale file, and the language decides which of them a
// number selects. Muzak knows the CLDR arithmetic for around ninety languages,
// so a locale file supplies only the words.
//
// Where the locale comes from is declared rather than written by hand. The
// Accept-Language header is negotiated by default, honouring the quality values
// the client sent; [LocaleFromPath], [LocaleFromQuery], [LocaleFromHeader],
// [LocaleFromCookie] and [LocaleFromCustom] cover the rest, in whatever order
// [I18nOptions.Sources] lists them. Nothing a request carries is used unless it
// matches a locale the application declared, so what reaches a filesystem path
// or a response header is always one of the application's own strings.
//
// A locale file names only what a service adds and the rules it wants worded
// differently. Everything else falls through to the locale Muzak ships, so a
// translation grows as a service is translated rather than having to be
// complete before it is useful:
//
//	es:
//	  errors:
//	    messages:
//	      blank: "es obligatorio"
//	    models:
//	      create_item:
//	        attributes:
//	          name:
//	            blank: "cada articulo necesita un nombre"
//
// The four scopes there are tried narrowest first, so one rule on one field of
// one model can be phrased without restating any other.
//
// Leaving [AppOptions.I18n] unset is internationalization turned off. No
// middleware is installed, every message reads exactly as it does without this
// feature existing, and nothing in muzak.dev/framework/i18n is linked into the
// binary at all.
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
//	r.WS("/items/{item_id}/ws", func(ctx *muzak.Context, in WSItemIn, conn *muzak.WSConn) error {
//		session := muzak.From[SessionOrToken](ctx)
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
//	}, muzak.Needs(GetSessionOrToken))
//
// A request that fails to bind, or a guard that refuses, is answered with the
// usual JSON error and never becomes a connection at all, which is what makes
// a rejection something a client can read rather than a socket that closes a
// moment after it opened. What can be judged without running any of that comes
// first: a handshake from an origin that may not connect, and one arriving when
// the application already holds as many connections as it may, are answered
// before the guards and dependencies run, so a refused handshake never costs a
// session lookup or its side effects.
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
// The context passed to a read or a write bounds it. A handler that wants to
// limit how long its peer may stay silent gives each read a deadline, and when
// one passes the peer is closed with [WSStatusPolicyViolation] and told why,
// and the handler gets the context's error back. A cancelled read is closed
// with [WSStatusGoingAway]. Returning either error from the handler is how
// the conversation ends rather than a failure, and is not logged as one.
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
//     them. The pings, pongs and empty fragments are also counted for the life
//     of the connection, so a peer cannot start the count over by completing
//     an empty message every so often.
//   - A write to a peer that has stopped reading gives up after
//     [WSOptions.WriteTimeout] rather than pinning a goroutine and a buffer.
//   - The application holds at most [WSOptions.MaxConnections] connections at
//     once, and answers 503 with a Retry-After beyond that, because file
//     descriptors run out before anything else does.
//   - A handshake from another origin is refused outright, because a WebSocket
//     handshake is not subject to the same-origin policy and is never
//     preflighted, which is what makes cross-site hijacking possible in the
//     first place. [AppOptions.CORS] does not cover it and never could. An
//     application or router that turns the check off with
//     [WSOptions.InsecureSkipOriginCheck] has not turned it off for good: a
//     route beneath it that authenticates by cookie turns it back on with
//     [WSOptions.EnforceOriginCheck].
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
// whatever host the answer named. It closes as RFC 6455 asks a client to,
// waiting for up to its CloseGracePeriod for the server to hang up first. The
// test client wraps it as
// [muzak.dev/framework/testclient.Client.WS].
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
//	r.SSE("/rooms/{room}/stream", func(ctx *muzak.Context, in StreamIn, stream *muzak.SSEStream[MessageOut]) error {
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
// dropped connection into a stream that picks up where it left off. A client
// dispatches an event only when it has a data field, so one that carries only a
// name or an identifier is applied but never delivered; [SSEEvent.EmptyData]
// gives an event an empty one, which is also how a stream of text sends an
// empty message.
//
// A stream is not tied to GET. [Router.SSEHandle] registers one for any method,
// which is what a protocol that streams its answer to a posted document needs,
// and there the input binds a request body like any other route. An input that
// binds none leaves the body to the handler, which reads it from the request
// after the stream has opened, under [ServerOptions.ReadTimeout], over HTTP/1.1
// as over HTTP/2.
//
// # What a stream bounds
//
// An event stream costs a connection and a goroutine for as long as a client
// cares to hold it, so the same reasoning applies as to a WebSocket.
//
//   - A client that stops reading a stream that keeps writing is given up on
//     after [SSEOptions.WriteTimeout] once the socket buffers are full, rather
//     than pinning a goroutine and a growing buffer for as long as it likes. A
//     stream that writes too little to fill them, as one that only sends its
//     keepalive does, is never ended by it.
//   - The application serves at most [SSEOptions.MaxStreams] streams at once,
//     and one client address holds at most [SSEOptions.MaxStreamsPerIP] of
//     them, answering 503 with a Retry-After beyond either.
//   - How long a stream lasts is bounded only by [SSEOptions.MaxLifetime],
//     which is unset by default.
//   - The listener's own timeouts are cleared for the stream and replaced with
//     a deadline per event, because a stream is a response that does not end
//     and would otherwise die at [ServerOptions.WriteTimeout] however healthy
//     it was. The read deadline goes too, once the request body has been read:
//     it would cancel the request, and with it the stream, at
//     [ServerOptions.ReadTimeout] and blame the client. A body the input left
//     for the handler is read under it, like any route's.
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
// The defaults do not stop a few clients from taking every stream. One that
// reads nothing is never ended by the write timeout, and nothing else ends it
// unless MaxLifetime is set, so MaxStreams / MaxStreamsPerIP client addresses
// fill the application, 1024 / 64 = 16 with the defaults: sixteen IPv4
// addresses, or one IPv6 /52, since an IPv6 client is counted by its /56. Every
// other stream is refused until they let go. On a route that clients one does
// not control can open, authenticate the client with a guard where possible,
// raise MaxStreams and lower MaxStreamsPerIP so that their quotient exceeds
// the addresses an abuser can be expected to have, count IPv6 clients by their
// /48 ([ClientIPOptions.ConnectionIPv6Prefix]), and set MaxLifetime to minutes,
// which costs a browser's EventSource a reconnection that resumes from the last
// event identifier. [SSEOptions] says more.
//
// Compression leaves an event stream alone, because holding events in a
// compressor's window until something forces them out is the one thing a stream
// cannot survive. The origin policy a WebSocket needs has no counterpart here
// either: an EventSource is subject to the same-origin policy and to CORS like
// any other request, so [AppOptions.CORS] governs it.
//
// [SSEDial] is the reading end of the same engine, so a stream route is tested
// over a real connection rather than against a second implementation, and the
// test client wraps it as [muzak.dev/framework/testclient.Client.SSE].
//
// # Serving a frontend
//
// [Router.Frontend] serves the static output of a frontend build, which is what
// React, Vue, Svelte, Angular, Solid and Astro produce:
//
//	app.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
//
// Routes win. A request is matched against every registered route first and
// reaches the frontend only when none of them answered, so mounting at the root
// cannot shadow an API. Middleware applies, and so does everything a route of
// the same router would run before its handler: its rate limit, its guards and
// its [Needs] and [Singleton] providers, whose errors are rendered as they
// would be for a route. That is what lets a frontend sit behind the same
// authentication and the same budget as everything else, and a mount behind
// any guard or provider answers with "Cache-Control: private, no-cache" so
// that a shared cache does not hand one client's files to another.
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
//	app.Frontend("/", muzak.FrontendOptions{FS: assets, Dir: "dist"})
//
// Nothing is rendered on the server and nothing is built here. A directory is
// never listed, a symbolic link cannot lead out of the build output (when it
// is read from [FrontendOptions.Dir], or from an [os.Root]; [os.DirFS] follows
// links), a path naming a dotfile such as /.env or /.git/config answers 404
// unless [FrontendOptions.AllowDotfiles] is set (a leading /.well-known/ is
// served), on Windows a segment shaped like an 8.3 short name such as /ENV~1 answers
// 404 because it would open a long name no check has seen, a method other
// than GET or HEAD on a file is refused with 405 rather than served, and a
// directory that does not exist is reported when the application is built
// rather than on the first request. A link that stays inside the directory is
// followed and served by the mount that owns the directory; only one that
// leaves it is refused.
//
// Mounts may nest, and the most specific one answers. When one mount's
// directory lies inside another's, as a guarded /admin does inside a public
// mount at the root, the directory is reachable only through the mount that
// serves it: a path that differs from the mount's only in case answers 404,
// and so does one that reaches the directory under any other name the
// filesystem accepts (a ligature or a sharp s written in full, a letter
// composed or decomposed, a link inside the outer directory), because each
// directory a request passes through is compared with the inner mount's own by
// identity rather than by name. That comparison needs both directories on disk,
// from [FrontendOptions.Dir] or an [os.Root]; a directory served from an
// [embed.FS] has no other name.
//
// [Router.Static] mounts a directory of files on the same machinery, without
// the part that makes a frontend work:
//
//	app.Static("/static", muzak.StaticOptions{Dir: "static"})
//
// Nothing stands in for a path with no file behind it, so a miss is a 404 and
// stays one, and a directory is served by its index.html only when
// [StaticOptions.Index] asks. Reach for Static to publish assets, and for
// Frontend to serve an application whose routing happens in the browser.
//
// Because a directory of files may hold files a client wrote, Static serves an
// HTML, SVG or other XML file under "Content-Security-Policy: sandbox", so
// that uploaded markup opened in a browser cannot run script as the
// application; [StaticOptions.AllowActiveContent] turns that off for a site of
// pages. On either kind of mount a file's type comes from its extension alone,
// and a file with no extension is application/octet-stream rather than
// whatever its first bytes resemble.
//
// # Responses other than JSON
//
// A handler's Out is encoded as JSON unless it is one of the types that say
// otherwise, which the router recognises when the route is registered and the
// document describes as what they send. [HTML] writes a page. [Bytes] writes a
// body already in memory under the media type it names, and [Stream] copies
// one that is read as it is sent, closing its body exactly once on every path
// the request can take, a HEAD, an error returned beside it and a client that
// went away included. [Produces] lists their media types in the document,
// which otherwise says application/octet-stream:
//
//	r.Get("/reports/{id}.csv", handlers.Report, muzak.Produces("text/csv"))
//
// [FileResponse] serves one file from an [io/fs.FS] with
// [net/http.ServeContent], so ranges and the conditional headers work, after
// refusing any name that would leave the filesystem, name a dotfile, or mean
// something else to Windows. The name is all it checks: serve a directory on
// disk through the FS of an [os.Root], which a symbolic link cannot lead out
// of, rather than [os.DirFS], which follows one anywhere. Each of the three is
// sent with "X-Content-Type-Options: nosniff", a content type a handler names
// is refused with a 500 rather than sent when it is not a media type, and a
// filename offered for saving is cleaned before it reaches
// Content-Disposition.
//
// [Redirect] sends the client elsewhere, and by default only to a path on this
// origin: an absolute URL must name a host listed with [RedirectHosts], or be
// marked External by a handler that built it itself. A target a browser or a
// decoding proxy would read as another host, "//evil.com" and every encoded,
// dotted or Unicode spelling of it, fails the request with a 500 instead, and
// is logged without the target.
//
// [AutoETag] tags a JSON, HTML or Bytes response to a GET or HEAD with a hash
// of its body, and answers a request that already holds the body with 304 Not
// Modified, carrying only the headers a cache needs:
//
//	app := muzak.New(opts, muzak.AutoETag())
//
// It composes with [Compress], which weakens the tag of what it compresses,
// and with a guarded route, whose 304 stays private. A route that does not
// declare it pays nothing for it.
//
// # Mounting a net/http handler
//
// [Router.Mount] serves any [net/http.Handler] at a prefix, for the prefix and
// everything beneath it and every method: net/http/pprof, a metrics handler, a
// connect-go service, or a legacy application being replaced one route at a
// time:
//
//	app.Mount("/metrics", promhttp.Handler(), muzak.Needs(auth.RequireOperator))
//	app.Mount("/debug/pprof", pprofMux, muzak.StripPrefix())
//	app.Mount("/", legacy)
//
// The most specific answer wins. A route at a path beneath the prefix answers
// the methods it registers, and the mounted handler every other method there;
// a longer mount, or a frontend or static mount beneath the prefix, answers
// what is beneath its own. The handler runs inside the application: the
// middleware, the CORS policy, the access log and panic recovery all apply,
// and before it runs, the rate limit, guards and providers of the routers it
// was registered under and of the options given to Mount, whose refusals are
// rendered as a route's are. It is handed the original *http.Request, with
// the request identifier and [RouteFromContext] reporting the prefix, never
// the pooled [Context], and a body bounded by [MaxBodySize]. [StripPrefix]
// gives it paths relative to the mount.
//
// Nothing normalises a path first. A request reaches the mount only through
// the routing tree's exact match of the prefix, so "//debug/x", "/DEBUG/x",
// "/./debug/x" and "/debug%2Fx" do not reach a mount at /debug, and whatever
// the mounted handler serves has passed its guards. Mounts are not in the
// OpenAPI document.
//
// # Hosts and HTTPS
//
// [AppOptions.AllowedHosts] names the hosts the application answers to, and
// refuses any other with 421 Misdirected Request before anything else runs:
//
//	app := muzak.New(muzak.AppOptions{
//		AllowedHosts:  []string{"example.com", "*.example.com"},
//		RedirectHTTPS: &muzak.RedirectHTTPSOptions{},
//	})
//
// The Host header is whatever the client wrote, so an application that builds
// a link, a redirect or a cache key from it, or that listens where a DNS
// rebinding page can reach it, wants the list. Matching ignores the case of
// ASCII letters and a trailing dot, a leading "*." allows every subdomain but
// not the domain, and an entry without a port allows any. No forwarding
// header is consulted.
//
// [AppOptions.RedirectHTTPS] answers a plain-HTTP request with a permanent
// redirect to the same URL over https: 301 for GET and HEAD, 308 for the rest.
// A request counts as https when it arrived over TLS, or when a proxy named in
// [ClientIPOptions.TrustedProxies] says so in X-Forwarded-Proto, or in
// Forwarded when that is the header the proxy writes; a claim from any other
// peer is ignored. The redirect names only a host the application vouches for,
// an allowed one or [RedirectHTTPSOptions.Host], so a forged Host cannot turn
// it into a redirect elsewhere, and an ACME HTTP-01 challenge is never
// redirected.
//
// # Problem details
//
// [AppOptions.ProblemDetails] makes RFC 9457 problem details the error
// format: every error is answered with a [Problem] as application/problem+json,
// and the OpenAPI document describes every error response that way.
//
//	app := muzak.New(muzak.AppOptions{
//		ProblemDetails: &muzak.ProblemOptions{TypeBase: "https://errors.example.com/"},
//	})
//
//	{
//	  "type": "https://errors.example.com/not_found",
//	  "title": "Not Found",
//	  "status": 404,
//	  "detail": "The requested resource was not found.",
//	  "instance": "urn:uuid:0611f4b2-2f0a-7b57-9c1a-6e6a2e2f9b31",
//	  "code": "not_found",
//	  "request_id": "0611f4b2-2f0a-7b57-9c1a-6e6a2e2f9b31"
//	}
//
// The renderer decides everything the default one decides, by asking it, so a
// problem makes the same promises the envelope does: the same codes, details
// and translations, and a server-side fault reported with a fixed sentence
// while its cause is logged. The title is the status's reason phrase,
// translated where the application translates muzak.status.<code>, and the
// instance names the request rather than its path. The default envelope
// remains the default.
//
// # Authentication that enforces what it documents
//
// A security scheme built by [JWTBearer] or [APIKeyVerifier] is written into
// the OpenAPI document like any other, and is also enforced: a route whose
// [WithSecurity] names it refuses a request without a valid credential before
// its guards, providers and handler run, and hands the verified principal to
// the rest of the route, so the document and the behaviour are one
// declaration and cannot drift apart:
//
//	app := muzak.New(muzak.AppOptions{
//		SecuritySchemes: map[string]muzak.SecurityScheme{
//			"oidc": muzak.JWTBearer(muzak.JWTOptions{
//				Issuers:    []string{"https://login.example.com/"},
//				Audience:   "https://api.example.com",
//				Algorithms: []string{"RS256"},
//				JWKS:       muzak.JWKSOptions{URL: "https://login.example.com/.well-known/jwks.json"},
//			}),
//		},
//	})
//
//	type MeIn struct {
//		Claims muzak.Dep[*muzak.Claims]
//	}
//
//	r := muzak.NewRouter(muzak.WithSecurity(muzak.Require("oidc", "profile")))
//	r.Get("/me", func(ctx *muzak.Context, in MeIn) (Me, error) {
//		return Me{Subject: in.Claims.Get().Subject}, nil
//	})
//
// A token is verified against an explicit list of algorithms, never "none",
// with a key of the algorithm's own kind that its kid selects, so an RSA
// public key is never an HMAC secret and a token cannot pick its key; its
// issuer, audience and dates are checked with a bounded leeway; and a token
// that names its own key with jku, jwk, x5u or x5c, or asks for an extension
// with crit, is refused. Every bound is explicit: the token's length, its
// base64url, which must be strict, the size and depth of its JSON, which may
// not repeat a member, and the keys a signature is tried against. A key set
// is fetched over https through the SSRF-safe [Client], cached within the
// bounds its Cache-Control allows, refreshed in the background while the
// application runs, and fetched again for an unknown kid at most once per
// interval, however many such tokens arrive. A failed fetch keeps the last
// set.
//
// A refusal is 401 with an RFC 6750 challenge, or 403 with insufficient_scope
// when a valid credential lacks a scope [Require] names, rendered by the error
// renderer, and it says nothing about which check failed. A key is compared
// in constant time against digests, and neither a token nor a key is ever
// logged or echoed. [ClaimsAs] decodes a token's custom claims into a type of
// the application's own, and [ResourceMetadata] publishes the RFC 9728
// document an OAuth client discovers the API's authorization server by. A
// route that names only descriptive schemes is documented and not enforced,
// exactly as before, and pays nothing.
//
// # Sessions and cross-origin requests
//
// [AppOptions.Sessions] gives every request a session, read the first time a
// handler, guard or provider calls [Context.Session], so a request that never
// asks pays nothing:
//
//	app := muzak.New(muzak.AppOptions{
//		Sessions:              &muzak.SessionOptions{Secrets: settings.SessionSecrets},
//		CrossOriginProtection: &muzak.CrossOriginOptions{},
//	})
//
//	func Login(ctx *muzak.Context, in LoginIn) (LoginOut, error) {
//		// ... check the password ...
//		s := ctx.Session()
//		if err := s.Regenerate(); err != nil { // always, on sign-in
//			return LoginOut{}, err
//		}
//		return LoginOut{}, s.Set("user", user.ID)
//	}
//
//	id, ok := muzak.SessionGet[int64](ctx.Session(), "user")
//
// By default the session lives in the cookie itself, encrypted and
// authenticated with AES-256-GCM under keys derived from the secrets, the
// first of which encrypts and all of which decrypt. A [SessionStore], such as
// the bounded [MemorySessionStore], keeps it on the server instead, so that
// [Session.Destroy] revokes every copy, and the cookie carries a random
// 256-bit identifier the store only ever sees a digest of. The idle timeout
// and the lifetime are kept inside the encrypted or stored record, never in
// the cookie's attributes. The cookie is HttpOnly, Secure, SameSite=Lax and
// named "__Host-session", a prefix that stops a sibling subdomain or a plain
// HTTP page from planting or shadowing it. A cookie that was tampered with,
// truncated, replayed under another name or has expired reads as no session.
//
// The session is written once: after the handler and every release, before
// the response, and only when the request succeeded with a status below 400
// and the session changed or is due for renewal. [Session.Save] writes it at
// once, for a change to keep whatever follows. [Session.Regenerate] must be
// called when a user signs in, or an identifier an attacker planted becomes
// the victim's session.
//
// A session cookie is sent with requests other pages make, so sessions
// cannot be configured without [AppOptions.CrossOriginProtection]. It is
// net/http's [http.CrossOriginProtection]: a request other than GET, HEAD
// or OPTIONS that a browser says came from another origin, in Sec-Fetch-Site
// or by an Origin that does not name the host, is refused with 403,
// classified "cross_origin_request" and rendered by the error renderer,
// before any middleware, guard or handler runs. Origins listed in
// [CrossOriginOptions.TrustedOrigins] are let through; an origin CORS allows
// is not trusted by that alone. A WebSocket handshake is a GET, and is left
// to [WSOptions.AllowedOrigins].
//
// # What is generated
//
// The OpenAPI 3.1 document at /openapi.json and the documentation UI at /docs
// are derived from the registrations themselves: path templates, tags,
// categories, titles, summaries, the schemas of the In and Out types, declared statuses and the
// entries added by [WithResponseDoc] and [WithResponseModel]. The return type
// describes the response a route succeeds with; every other status code it
// answers is described by one of those two, either as the standard error
// envelope or as a model of its own, so a single operation can carry a
// different schema per status code. All of that reflection happens once,
// while the application is being built. Nothing on the request path inspects a
// type, because the binding plan and the response schema were both compiled at
// start-up.
//
// The document says of a request only what the server enforces. A member of a
// JSON body is listed as required when a Required rule refuses a body without
// it; the decoder accepts an absent member otherwise, so it is optional however
// the Go field is declared. The same goes for the members of every object the
// body nests, which only the rules of a model nested with [Validation.Nested]
// can require, or a rule the input declares on a member of a struct it holds,
// as v.String(&in.Price.Currency).Required() is. Such a rule is written beside
// the reference to the struct's component, for that member alone, so the other
// uses of the type say nothing of it. What a response always carries is
// described from the shape of its type, as before, so a type used both ways is
// described twice: once for the responses and once, named with Input after it,
// for the requests. The request's description also says additionalProperties:
// false at every depth, because the decoder refuses a member it does not know
// unless the route was registered with [AllowUnknownFields]; a response is left
// open, so that adding a member to it later breaks no client.
//
// The document is what the framework publishes; rendering it is a separate
// concern, and a separate module. [AppOptions.DocsUI] is nil by default, so a
// service describes itself at /openapi.json and carries no page at all:
//
//	import "muzak.dev/openapi/ui"
//
//	muzak.AppOptions{DocsUI: ui.Files()}
//
// That is the OpenAPI dashboard, which reads this application's own document
// and renders the operations grouped by tag, every schema as an outline, and a
// console that sends a request from the page and reports the status, the
// timing, the headers and the body, or writes that same request out as a curl
// command. Operations are grouped by the tags [WithTags] puts on a router or a
// route; [OpenAPIOptions.Tags] describes those groups and decides the order
// they are presented in.
//
// A UI is a module of its own so that a service which does not want one does
// not carry it: Go downloads and links a module only when something imports
// it. Nothing is fetched at run time either way, so a configured dashboard
// works air-gapped, exactly as the document does.
//
// Everything is rendered, hashed and compressed while the application is
// built, so a request for the document, the page or one of its assets is a few
// header writes and a copy of bytes that never change: each carries an entity
// tag the client revalidates against, and the page is served under a content
// security policy that hashes its own inline script and permits no network
// access beyond this origin.
//
// The guards and providers given to [New] run before any of it is sent, and
// a refusal is rendered as it would be for a route, so an application-wide
// token covers the documentation as well as the API it describes. Declare the
// guard on an included router instead to keep the documentation public.
//
// A guard is a function, which Muzak can run and cannot read, so the document
// does not say how a route authenticates unless the application does. Declare
// the schemes once in [OpenAPIOptions.SecuritySchemes] and say which a route,
// or a router, sits behind with [WithSecurity], or [Public] for an exception:
//
//	app := muzak.New(muzak.AppOptions{
//		SecuritySchemes: map[string]muzak.SecurityScheme{
//			"bearer": muzak.BearerAuth("JWT"),
//		},
//	})
//	admin := muzak.NewRouter(muzak.WithSecurity(muzak.Require("bearer")), muzak.WithDependencies(requireStaff))
//	admin.Get("/status", status, muzak.Public())
//
// They are emitted as components.securitySchemes and a security list on each
// operation that declared one, which is what gives the documentation UI
// something to offer under Authorize. A descriptive scheme describes and
// nothing more: it is never consulted while a request is served, so a route
// that names one is only protected by the guard it also has. A verifying one
// enforces as well; see "Authentication that enforces what it documents". An application that declares none
// emits none, and a scheme, a scope or a requirement that cannot be described is
// reported when the application is built.
//
// With a hundred routers a list of tags is too long to read, so a router can be
// filed under a category as well, and a route can be given a human title to be
// listed by in place of its path:
//
//	billing := muzak.NewRouter(muzak.WithCategory("Billing"))
//	billing.Get("/users/{id}", profile, muzak.Title("Fetch User Profile"))
//	billing.Get("/reports", reports, muzak.WithCategory("Reports"))
//
// A category is not a tag, and tags group operations exactly as before. A
// router has one category, which every route beneath it inherits; several
// routers may share one; and a router included into another, or a route itself,
// replaces the category it would have inherited, where tags would add up. Both
// are emitted as vendor extensions on the operation, "x-category" and
// "x-title", and only where one was set, so an application that sets neither
// publishes the same document as before. The document also carries a top-level
// "x-categories" list, holding each category some operation in it carries once,
// in the order the categories were first registered (a router counts where it
// was included), which is the order to present them in because the paths are
// listed sorted. A category of at most 64 characters and a title of at most 120
// must be one line of text, which is checked when the application is built.
//
// [AppOptions.DocsPath] and [AppOptions.OpenAPIPath] decide where the two are
// served, and [AppOptions.DisableDocs] turns both off for a deployment that
// must not describe itself. Where they ended up is reported as the socket
// opens:
//
//	INFO [Docs] Documentation at http://localhost:8080/docs  openapi=http://localhost:8080/openapi.json
//
// A path that is not absolute, that is the same as the other one, or that one
// of the application's own routes already answers is a build error rather than
// an address nobody can reach.
//
// # API compatibility
//
// A committed document is a promise to every client written against it, and
// [CompareDocuments] says which changes keep it. It judges each change the way
// a client meets it: what a client sends may only widen and what it reads may
// only narrow, so a new required request member, a narrowed request type or a
// response member that is gone or may now be null is [Breaking], a new value
// in a response enum is [PossiblyBreaking], and a new optional member or a new
// operation is [Compatible]. Each [APIChange] names its kind, where it is and
// what it does to a client, and [WriteChanges] prints a list of them for a
// person:
//
//	baseline, err := muzak.ReadDocument(file)
//	...
//	current, err := app.Document()
//	...
//	err = muzak.WriteChanges(os.Stdout, muzak.CompareDocuments(baseline, current))
//
// [ReadDocument] reads a stored document strictly and within fixed bounds,
// since the file may have come from anywhere, and the comparison follows each
// shared or recursive component once, so it costs what the documents weigh
// rather than the number of paths through them. In a test,
// testclient.AssertCompatible does all of it against a baseline kept in the
// repository, and records a new baseline when MUZAK_UPDATE_OPENAPI=1 is set.
//
// # The muzak command
//
// The command in muzak.dev/framework/cmd/muzak works on a project from the
// outside. muzak new creates one laid out the way the framework's example is,
// muzak dev builds and runs it and restarts it whenever a source changes,
// muzak routes lists the operations a document describes, muzak diff compares
// two documents with [CompareDocuments] and exits non-zero on the changes a CI
// step should refuse, and muzak ts writes the declarations the tsgen package
// generates:
//
//	go install muzak.dev/framework/cmd/muzak@latest
//
//	muzak new shop -module example.com/shop
//	muzak diff -fail-on breaking baseline.json current.json
//
// It belongs to this module, so the command installed at a version is the one
// that matches the framework of that version, and the project it creates
// requires that version. It uses the standard library alone, like the rest of
// the module, and escapes everything it prints from a document for the
// terminal.
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
// An error replaces the response its handler was preparing, so the headers
// that described that response go with it: Content-Type, Content-Length,
// Content-Disposition, Content-Encoding, Content-Range, ETag, Last-Modified,
// Cache-Control and Expires are dropped, Content-Language is set back to the
// locale the error is written in, and the error is sent with
// "Cache-Control: no-store". Every other header, Vary, Retry-After, Allow,
// WWW-Authenticate and Set-Cookie among them, is kept. A custom renderer runs
// after this and may set any of them again.
//
// There is a constructor for each outcome worth a name of its own, so that
// returning the right response does not mean remembering the right number:
//
//	return schemas.UserOut{}, muzak.NotFound("no user goes by that name")
//	return schemas.UserOut{}, muzak.Forbidden("")          // the standard sentence
//	return schemas.UserOut{}, muzak.Conflict("that name is taken").Wrap(err)
//
// [BadRequest], [Unauthorized], [PaymentRequired], [Forbidden], [NotFound],
// [MethodNotAllowed], [NotAcceptable], [RequestTimeout], [Conflict], [Gone],
// [PreconditionFailed], [PayloadTooLarge], [UnsupportedMediaType],
// [UnprocessableEntity], [TooManyRequests], [InternalServerError],
// [NotImplemented], [BadGateway], [ServiceUnavailable] and [GatewayTimeout]
// each return an [*HTTPError] carrying that status and its classifier, so
// [HTTPError.Wrap], [HTTPError.WithCode] and [HTTPError.WithDetails] chain
// onto every one of them. Any other status is one [NewHTTPError] can produce.
//
// # Observability
//
// Tracing is off until [AppOptions.Tracing] names a [Tracer], and costs nothing
// while it is: no header is parsed, no identifier is drawn and nothing is
// allocated. With one, every request gets a server span, named after its
// method and route template, "GET /items/{id}", and never after its path,
// which carries identifiers and often personal data:
//
//	exporter, err := otlp.New(otlp.Options{Endpoint: "http://localhost:4318", ServiceName: "shop"})
//	if err != nil {
//		log.Fatal(err)
//	}
//	app := muzak.New(muzak.AppOptions{Tracing: muzak.TracingOptions{Tracer: exporter}})
//
// A request carrying a W3C traceparent continues that trace, and its sampling
// decision with it, unless [TracingOptions.Parent] says to believe only a
// trusted proxy's, [TraceParentFromTrustedProxies], which is what a service
// reached directly from the internet wants: a client choosing its own trace
// identifier can attach its requests to another's trace, or have every one of
// them sampled. The header is read strictly, as the recommendation writes it:
// lowercase hex only, no identifier of zeroes, version ff refused and a later
// version read forward-compatibly, and a tracestate of more than 32 members or
// 512 bytes, or with one member out of grammar, dropped whole. Identifiers
// this server draws come from crypto/rand.
//
// A handler reaches the span through its context: [SpanFromContext] to add
// what it knows, [StartSpan] for a child span recorded through the same
// Tracer, and [SpanContextFromContext] and [InjectTraceContext] to carry the
// trace into a request of its own. The access log and every record written
// through [Context.Logger] carry trace_id and span_id.
//
// The span carries the OpenTelemetry HTTP attributes, user_agent.original only
// when asked for. A 5xx, a response aborted after it started and a stream or
// WebSocket handler that failed mark it an error, and a failure the log records
// is added as an "exception" event with the same text the log line holds and
// nothing more. It ends once the response is written, which for an event
// stream or a WebSocket is when the stream or the connection ends. A Tracer
// that panics costs the request its span and nothing else.
//
// [AppOptions.Observer] is the hook request metrics are built on. A
// [RequestObserver] is called exactly once per request, after the response,
// with a method, a route template and a status whose values are bounded, so a
// metric labelled by them cannot grow a series per request. A Tracer or an
// observer that implements [Lifecycle] is started and stopped with the
// application. [muzak.dev/framework/otlp] is a Tracer that sends spans to any
// OpenTelemetry collector.
//
// # Calling other services
//
// [NewClient] builds the client an application calls other services with. It
// is a [net/http.Client] with the decisions that make it safe to point at a
// URL someone else chose already made, so that fetching a webhook target, an
// avatar or an import URL does not become a request forgery:
//
//	client := muzak.NewClient(muzak.ClientOptions{})
//	item, err := client.GetJSON[ItemOut](ctx.Context(), in.URL)
//
// Loopback, private, link-local, shared, reserved and documentation
// addresses are refused, and so are cloud metadata services, at the moment of
// connecting rather than when the URL is read, so a name that resolves to one,
// a name that resolves to one the second time it is asked, and a redirect to
// one are all refused alike. So is every IPv6 spelling of a refused IPv4
// address and every way of writing an IPv4 address that a browser or the C
// resolver reads differently from Go, such as 127.1 or 2130706433. A refusal
// is an [*AddressRefusedError], and nothing was sent. A client calling its own
// network sets [ClientOptions.AllowPrivateNetworks], or names the networks it
// calls in [ClientOptions.AllowedNetworks]; neither opens a metadata service
// unless it is named outright.
//
// Every wait is bounded, the call as a whole by [ClientOptions.Timeout]; a
// response body stops at [ClientOptions.MaxResponseBytes], decompressed; at
// most five redirects are followed, never from https down to http, and
// credentials are dropped from any that leave the origin. A request is retried
// only when sending it twice is harmless, a GET, PUT or DELETE or anything with
// an Idempotency-Key, after a connection error or a 429, 502, 503 or 504, with
// a randomised exponential wait that honours Retry-After, and never more than
// a [RetryBudget] allows, so that retries cannot become the outage. A
// per-host circuit breaker is there to turn on with
// [ClientOptions.CircuitBreaker]. The request identifier of the request being
// served is sent along in X-Request-Id, which is what lets two services' logs
// be joined; see [ClientOptions.Propagate].
//
// # Typed endpoints
//
// An operation two Go programs share is declared once, with [NewEndpoint], in
// a package both import. The service implements it with [Router.Implement]
// and its callers call it with [Endpoint.Call], so the method, the path and
// both types are written in one place and a mismatch on either side is a
// compile error:
//
//	var GetUser = muzak.NewEndpoint[GetUserIn, User](http.MethodGet, "/users/{id}")
//
//	r.Implement(userapi.GetUser, handlers.GetUser)
//
//	users := muzak.NewClient(muzak.ClientOptions{AllowPrivateNetworks: true, BaseURL: "http://users:8080"})
//	user, err := userapi.GetUser.Call(ctx.Context(), users, userapi.GetUserIn{ID: "42"})
//
// A call writes its input as the request the binder reads back into an equal
// value, field by field from the tags it is bound by, and refuses, with an
// error wrapping [ErrCallRefused] and before anything is sent, a value no
// request carries unchanged: a path parameter that would name another route,
// a header with a line break in it, a cookie outside the characters cookies
// may hold. An answer that is not a success is a [*RemoteError] carrying the
// code, message, details and request identifier of the service's error
// envelope. The TypeScript side of the same API is generated from the
// document by the tsgen package, muzak.dev/framework/tsgen.
//
// # Routes as MCP tools
//
// [App.MCP] serves a Model Context Protocol endpoint through which an AI
// client calls routes as tools. Nothing is a tool until it is chosen, with
// [MCPTool] on a route or with the tags and the rule of [MCPOptions]:
//
//	app.MCP("/mcp", muzak.MCPOptions{Tags: []string{"agent"}},
//		muzak.WithSecurity(muzak.Require("oidc")))
//
// A tool is described from the OpenAPI document: named after the operation
// id, its input an object with a member for each part of the request the route
// reads, path, query, header, cookie, body or form, and its output the success
// response's schema, references resolved. A call is the route's own request,
// written from the arguments with the encoder [Endpoint.Call] uses and served
// in-process by the whole application, so its security, guards, providers,
// rate limit, validation and releases apply, and its error answer is the
// tool's error result. The request carries the MCP request's Authorization,
// the headers and cookies the options forward, its client address and request
// identifier, and its cancellation, and may reach the tool's own route and no
// other. The endpoint speaks Streamable HTTP, with sessions for revisions
// 2025-03-26, 2025-06-18 and 2025-11-25 and statelessly for 2026-07-28; it
// checks the Origin against DNS rebinding, bounds messages, sessions and
// results, and is left out of the document.
//
// # Defaults worth knowing
//
// Muzak starts from settings that are safe rather than permissive. Every
// listener timeout is non-zero, request bodies are capped at one mebibyte and
// uploads at 32, WebSocket messages at one mebibyte, unknown JSON members are
// rejected, duplicate members and invalid UTF-8 are refused by
// encoding/json/v2, a query, header or form parameter that holds one value is
// refused when it is sent more than once, rather than one of its values being
// chosen, a JSON body without a JSON Content-Type is refused, CORS
// denies every cross-origin request until it is
// configured, a WebSocket handshake from another origin is refused until it is
// allowed, the connections and the event streams one application holds are both
// capped, a write to a client that stopped reading gives up, no forwarding
// header is believed until a proxy is named, and a panic becomes a generic 500
// with the stack recorded only in the log, or, once the response has started,
// an aborted connection that a client cannot mistake for a complete response.
// Each of these can be relaxed deliberately; none of them is relaxed by
// omission. The age of an event stream is not among them:
// [SSEOptions.MaxLifetime] is unset by default, so the cap on streams is a
// number of slots that sixteen addresses reading nothing can hold
// indefinitely, as [SSEOptions] explains with the settings that change it.
//
// Rate limiting is the deliberate exception, and is off until a quota is
// declared. There is no limit that is right for every application, and a
// default one would be a number nobody chose refusing traffic nobody expected.
// What is safe by default is what happens once one is declared: the counters
// are bounded, the address is not taken from a header anyone can write, and a
// storage that stops answering stops traffic rather than stopping the limit.
//
// # Operations
//
// Three features serve an application that a platform runs, restarts and
// routes traffic to, and each is off until it is asked for.
//
// [AppOptions.Health] serves a liveness endpoint, which answers 200 for as
// long as the process serves, and a readiness endpoint, which answers 503
// until the lifecycle components have started, then runs the configured
// checks, and answers 503 again from the moment a shutdown begins.
// [ServerOptions.DrainDelay] then keeps the listeners open, still serving,
// long enough for a load balancer to notice:
//
//	app := muzak.New(muzak.AppOptions{
//		Health: muzak.HealthOptions{
//			Enabled: true,
//			Checks:  []muzak.HealthCheck{{Name: "database", Check: db.PingContext}},
//		},
//		ServerOptions: muzak.ServerOptions{DrainDelay: 5 * time.Second},
//	})
//
// Probes are answered ahead of routing, before the middleware installed with
// [App.Use], the guards and the rate limit, none of which a probe could
// satisfy. The checks run concurrently, each under its own timeout, and a
// flood of probes runs them once per [HealthOptions.CacheInterval]. A body
// never carries what a check returned; the log does.
//
// [Context.AfterResponse] registers work the client should not wait for. It
// runs on a small bounded pool once the handler has returned nil, keeps the
// request's values but not its cancellation, and is waited for by a shutdown
// before the lifecycle components it may use are stopped. A full queue refuses
// a task with [ErrBackgroundQueueFull] rather than making the request wait:
//
//	log := ctx.Logger()
//	email := in.Email
//	if err := ctx.AfterResponse(func(bg context.Context) {
//		if err := mailer.SendWelcome(bg, email); err != nil {
//			log.ErrorContext(bg, "welcome email failed", "error", err)
//		}
//	}); err != nil {
//		log.Warn("welcome email not queued", "error", err)
//	}
//
// [Timeout] gives a route, or every route of a router, a deadline on the
// request's context. It is cooperative: no second goroutine runs on the
// request's behalf, whatever honours the context gives up, a failure the
// deadline caused is answered 503 with a Retry-After, and a handler that
// ignores the deadline still has its success sent:
//
//	r.Get("/reports/{id}", buildReport, muzak.Timeout(2*time.Second))
//
// # Testing
//
// The muzak.dev/framework/testclient package serves an application in-process and issues
// real requests against it, so a test exercises middleware, routing, binding,
// dependencies and error rendering together rather than any one of them in
// isolation. The application is served by the same server, with the same
// [ServerOptions], that [App.Run] serves it with, and is shut down the way
// [App.Shutdown] shuts it down, so a header too large for production is too
// large in a test as well.
package muzak
