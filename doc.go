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
// uploads at 32, unknown JSON members are rejected, duplicate members and
// invalid UTF-8 are refused by encoding/json/v2, CORS denies every
// cross-origin request until it is configured, and a panic becomes a generic
// 500 with the stack recorded only in the log. Each of these can be relaxed
// deliberately; none of them is relaxed by omission.
//
// # Testing
//
// The badele/testclient package serves an application in-process and issues
// real requests against it, so a test exercises middleware, routing, binding,
// dependencies and error rendering together rather than any one of them in
// isolation.
package badele
