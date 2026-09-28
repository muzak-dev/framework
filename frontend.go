package muzak

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// FrontendOptions describes a built frontend to serve.
//
// Exactly one source is required. Dir names a directory on disk, which is what
// a build step such as "npm run build" produces:
//
//	app.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
//
// FS serves the frontend from an [io/fs.FS] instead, which is how a frontend
// gets built into the binary. Setting both reads Dir as a subdirectory of FS,
// which is what the directory embed produces:
//
//	//go:embed all:dist
//	var assets embed.FS
//
//	app.Frontend("/", muzak.FrontendOptions{FS: assets, Dir: "dist"})
//
// The zero value of the remaining fields resolves the fallback from what the
// build actually produced, which is what most frontends want; see
// [Router.Frontend].
type FrontendOptions struct {
	// Dir is the directory holding the built frontend, or the subdirectory
	// within FS when both are set.
	Dir string

	// FS serves the frontend from a filesystem rather than from disk. An
	// [embed.FS] is the usual one, which makes the binary the whole
	// deployment.
	//
	// The promise that a symbolic link cannot lead out of the directory is
	// kept by Dir, and by the FS of an [os.Root], but not by whatever else is
	// given here: [os.DirFS] follows a link wherever it points, so a link
	// planted in the directory serves the file it names. To serve a directory
	// on disk, set Dir, or pass the FS of a root opened with [os.OpenRoot].
	FS fs.FS

	// Fallback is the file served, with 200, when a browser navigates to a
	// path with no file behind it, so that a client-side router can take over.
	// It is the entry document of a single page application, normally
	// "index.html".
	//
	// It is used only for a GET or HEAD that asks for HTML, which is what a
	// navigation does. A missing script, stylesheet or image still answers
	// 404, because handing those an HTML document only turns a missing file
	// into a confusing parse error. Since the answer then depends on Accept,
	// every response of a mount with a fallback and no NotFound page carries
	// "Vary: Accept", so that a cache does not hand one to the other.
	Fallback string

	// NotFound is the file served, with 404, when nothing else matched. It is
	// the error page of a site whose pages are files, normally "404.html".
	//
	// It takes precedence over Fallback, and is used for any GET or HEAD.
	NotFound string

	// NoFallback serves a plain 404 for anything with no file behind it,
	// turning off the automatic resolution described on [Router.Frontend].
	NoFallback bool

	// SkipCheck stops the frontend being verified when the application is
	// built, for a directory that something else fills in later. A request
	// arriving before that happens fails with 500 and the reason logged.
	SkipCheck bool

	// AllowDotfiles serves files and directories whose name begins with a
	// dot. By default a request naming one anywhere in its path is answered
	// 404, as though it were not there, and the fallback is not offered in its
	// place; a leading ".well-known" directory is the one exception, because
	// that is where a site publishes files meant to be found.
	//
	// Relaxing it costs whatever the directory holds under such names. A
	// build output or a deployment directory is where a .env, a .git
	// directory, an .htpasswd or an editor's swap file ends up by accident,
	// and serving one publishes credentials and source history to anyone who
	// guesses the name, which every scanner does. Turn it on only for a
	// directory whose every dotfile is meant to be public.
	AllowDotfiles bool
}

// frontend is one resolved frontend mount: a path, the filesystem behind it,
// and the guards, providers and rate limit it inherited from the routers it
// was registered under.
type frontend struct {
	// path is the full mount path, with no trailing slash unless it is root.
	path      string
	opts      FrontendOptions
	guards    []Guard
	providers []*provider

	// limits is a stand-in route that carries the mount's rate limit. It is
	// resolved by the same code that resolves a route's, and completed with
	// every route's once the application is built, so a quota a mount shares
	// with the routes beside it is one budget in one storage. It is never
	// registered and never answers a request.
	limits *Route

	// open resolves the filesystem once, so that a directory named but not yet
	// built is reported on the first request rather than at start-up.
	once  sync.Once
	files fs.FS
	err   error

	// fallback and notFound are the files resolved from the options and from
	// what the build produced, empty when there is nothing to fall back to.
	fallback string
	notFound string

	// index reports whether a directory is served by the index.html inside it,
	// which is what a frontend expects and what a plain file mount does not.
	index bool

	// kind names the mount in a message, so that a problem with a static file
	// mount is not reported as a problem with a frontend.
	kind string

	// sandbox serves a document a browser would run, such as HTML or SVG,
	// under a sandboxing Content-Security-Policy. A static mount sets it
	// unless told otherwise, because its directory may hold files a client
	// wrote; a frontend's documents are the application itself.
	sandbox bool
}

// Frontend serves a built frontend at path.
//
// It is for the static output of a frontend build, which is what React, Vue,
// Svelte, Angular, Solid, Astro and the rest produce. Nothing is rendered on
// the server, and nothing is built here: this serves files that already exist.
//
//	app.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
//
// Routes win. A request is matched against every registered route first, and
// reaches the frontend only when none of them answered, so mounting a frontend
// at "/" cannot shadow an API. Middleware still applies, and so does what the
// routers the frontend was registered under would run before a route's
// handler: their rate limit, then their guards, then every [Needs] and
// [Singleton] provider they declare. An error from any of them is rendered
// exactly as it would be for a route, which is what lets a frontend sit behind
// the same authentication and the same budget as everything else. A mount that
// inherits any guard or provider sends "Cache-Control: private, no-cache" on
// every response, so that a cache shared between clients keeps none of them.
//
// A request for a path with no file behind it falls back to one, resolved from
// the build unless the options say otherwise: a 404.html in the frontend's root
// is served with 404, and failing that an index.html is served with 200 for a
// browser navigation, which is what a client-side router needs to take over.
// Set [FrontendOptions.NoFallback] for a plain 404 instead.
//
// Mounting under a prefix works the way everything else does, through the
// router the frontend is registered on:
//
//	ui := muzak.NewRouter()
//	ui.Frontend("/", muzak.FrontendOptions{Dir: "dist"})
//	app.Include(ui, muzak.WithPrefix("/app"))
//
// When mounts nest, the most specific one answers, and a request that would
// fall under a more specific mount if case were ignored answers 404 rather
// than being served by a less specific one. With another mount at the root
// beside the one above, /APP/x is refused rather than handed to the root: on
// a case-insensitive filesystem the outer mount could otherwise open the
// inner mount's files without the inner mount's guards.
//
// A path naming a file or directory that begins with a dot, such as /.env or
// /.git/config, answers 404 and is not given the fallback, since a build
// output is where such files end up by accident; a leading /.well-known/ is
// served as usual. See [FrontendOptions.AllowDotfiles]. On Windows a path
// segment shaped like an 8.3 short name, such as /ENV~1 or /GIT~1/config, is
// refused the same way whatever the options say, because the filesystem opens
// the long name it stands for without any check having seen that name.
//
// Problems with the mount, including a directory that does not exist, are
// reported when the application is built rather than on the first request.
func (r *Router) Frontend(mountPath string, opts FrontendOptions) {
	r.mustBeOpen("mounting a frontend at " + mountPath)
	if !strings.HasPrefix(mountPath, "/") {
		r.errs = append(r.errs, fmt.Errorf("muzak: frontend at %q: path must begin with %q", mountPath, "/"))
		return
	}
	if opts.Dir == "" && opts.FS == nil {
		r.errs = append(r.errs, fmt.Errorf("muzak: frontend at %q: set Dir, FS, or both", mountPath))
		return
	}
	if opts.NoFallback && (opts.Fallback != "" || opts.NotFound != "") {
		r.errs = append(r.errs, fmt.Errorf("muzak: frontend at %q: NoFallback cannot be combined with Fallback or NotFound", mountPath))
		return
	}
	r.frontends = append(r.frontends, &frontend{path: mountPath, opts: opts, index: true, kind: "frontend"})
}

// resolve computes a frontend's final mount path, dependency chain and rate
// limit, and verifies what it serves unless the mount asked not to be checked.
func (f *frontend) resolve(in inherited) error {
	f.path = strings.TrimSuffix(in.prefix+f.path, "/")
	f.guards = in.guards
	f.providers = in.providers
	// The stand-in is named after the mount, so that a rate limit the mount
	// cannot use is reported against the mount rather than against a route
	// nobody registered.
	f.limits = &Route{Method: f.kind, Path: f.mountPath()}
	if err := f.limits.resolveRateLimit(in); err != nil {
		return err
	}

	if f.opts.SkipCheck {
		// The files are named but not promised. Whether they are there is
		// decided by the first request that needs them.
		f.fallback, f.notFound = f.opts.Fallback, f.opts.NotFound
		return nil
	}
	files, err := f.resolveFS()
	if err != nil {
		return fmt.Errorf("muzak: %s at %q: %w", f.kind, f.mountPath(), err)
	}
	for _, named := range []struct{ what, name string }{
		{"Fallback", f.opts.Fallback},
		{"NotFound", f.opts.NotFound},
	} {
		if named.name == "" {
			continue
		}
		if _, err := fs.Stat(files, named.name); err != nil {
			return fmt.Errorf("muzak: %s at %q: %s file %q: %w", f.kind, f.mountPath(), named.what, named.name, err)
		}
	}
	f.fallback, f.notFound = f.resolveFallback(files)
	return nil
}

// resolveFallback decides which file stands in for a path with no file behind
// it, from the options and from what the build produced.
//
// An explicit choice is taken as given. Otherwise a 404.html means the build
// makes a page per file and has an error page for the rest, and an index.html
// means a single page application whose router wants every path.
func (f *frontend) resolveFallback(files fs.FS) (fallback, notFound string) {
	if f.opts.NoFallback {
		return "", ""
	}
	if f.opts.Fallback != "" || f.opts.NotFound != "" {
		return f.opts.Fallback, f.opts.NotFound
	}
	if _, err := fs.Stat(files, "404.html"); err == nil {
		return "", "404.html"
	}
	if _, err := fs.Stat(files, "index.html"); err == nil {
		return "index.html", ""
	}
	return "", ""
}

// mountPath names the mount for a message, spelling the root as "/" rather
// than as the empty string it is trimmed to.
func (f *frontend) mountPath() string {
	if f.path == "" {
		return "/"
	}
	return f.path
}

// fsys returns the filesystem the frontend serves, opening it once.
func (f *frontend) fsys() (fs.FS, error) {
	f.once.Do(func() { f.files, f.err = f.resolveFS() })
	return f.files, f.err
}

// resolveFS builds the filesystem the options describe.
func (f *frontend) resolveFS() (fs.FS, error) {
	if f.opts.FS != nil {
		if f.opts.Dir == "" {
			return f.opts.FS, nil
		}
		return fs.Sub(f.opts.FS, f.opts.Dir)
	}
	// os.OpenRoot resolves every path inside the directory, so a symbolic link
	// pointing out of the build output cannot be followed out of it.
	root, err := os.OpenRoot(f.opts.Dir)
	if err != nil {
		return nil, err
	}
	return root.FS(), nil
}

// matches reports whether a request path falls under this mount, and returns
// the path relative to it.
func (f *frontend) matches(requestPath string) (string, bool) {
	if f.path == "" {
		return strings.TrimPrefix(requestPath, "/"), true
	}
	if requestPath == f.path {
		return "", true
	}
	if rest, found := strings.CutPrefix(requestPath, f.path+"/"); found {
		return rest, true
	}
	return "", false
}

// serve answers a request from the frontend's files.
func (a *App) serveFrontend(c *Context, f *frontend, relative string) {
	f.describe(c.w.Header())
	if err := f.admit(c); err != nil {
		a.fail(c, err)
		return
	}
	if (!f.opts.AllowDotfiles && namesDotfile(relative)) || (shortNamesResolve && namesShortName(relative)) {
		// Answered as though nothing were there, and without the fallback: a
		// 200 carrying the application document for /.env reads to a scanner
		// as a hit and to a person as the file being served.
		a.fail(c, frontendNotFound(c.r))
		return
	}

	files, err := f.fsys()
	if err != nil {
		a.logger.ErrorContext(c.Context(), "muzak: the directory behind a mount could not be opened",
			slog.String("kind", f.kind),
			slog.String("mount", f.mountPath()),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", err.Error()))
		a.fail(c, errFrontendUnavailable)
		return
	}

	if name, ok := resolveFile(files, relative, f.index); ok {
		if !isRead(c.r.Method) {
			// The file is there. What is not allowed is the method, and
			// answering 404 would say the opposite.
			c.w.Header().Set("Allow", allowedOnFiles)
			a.fail(c, NewHTTPErrorf(http.StatusMethodNotAllowed,
				"%s is not allowed here; allowed methods are %s", quotableMethod(c.r.Method), allowedOnFiles))
			return
		}
		f.write(c, files, name, http.StatusOK)
		return
	}
	a.serveFrontendFallback(c, f, files)
}

// describe sets the headers every response of the mount carries, whichever
// one it turns out to be: a file, an error page, the fallback, or a refusal.
// They are set before anything is decided because they describe the mount
// rather than the answer, and a cache has to see them on every answer.
func (f *frontend) describe(header http.Header) {
	if answersPerClient(f.guards, f.providers) {
		// A mount behind a guard or a provider answers some clients and not
		// others, so a cache shared between them must not keep what it sends.
		// A file carries Last-Modified, which is all a shared cache needs to
		// store it heuristically, and a cookie, unlike an Authorization
		// header, does not stop it. The answer is the one the documentation
		// gets when it is guarded, and it replaces a value a middleware set,
		// because a long-lived public value on a guarded file is exactly the
		// leak this prevents. no-cache still lets the browser keep the file
		// and revalidate it with the modification time.
		header.Set("Cache-Control", "private, no-cache")
	}
	if f.fallback != "" && f.notFound == "" {
		// The fallback answers a path with the application document for a
		// navigation and with a 404 for anything else, so the answer depends
		// on Accept. It is declared on every response rather than only on a
		// miss, because whether a path is a file or a miss is not something a
		// cache can know, and a deployment that removes a file turns one into
		// the other under the same URL. A NotFound page takes precedence over
		// the fallback and is served whatever the request accepts.
		addVary(header, "Accept")
	}
}

// admit runs everything a route of the same routers would run before its
// handler: the rate limit, the guards and the value providers, in the order
// [App.run] runs them.
//
// A mount used to run the guards alone. A router whose authentication is a
// [Needs] provider, which is the documented way to hand the handler the
// current user, therefore served its files to anyone, and an application-wide
// rate limit counted every API call and none of the file requests beside
// them. A mount has no handler to read a resolved value, so a provider runs
// here for its verdict; the value is still recorded, so that a provider which
// reads an earlier one with [From] finds it.
func (f *frontend) admit(c *Context) error {
	limits := f.limits.rateLimit
	if limits != nil && !limits.afterDependencies {
		if err := limits.check(c); err != nil {
			return err
		}
	}
	for _, guard := range f.guards {
		if err := guard(c); err != nil {
			return err
		}
	}
	if err := resolveProviders(c, f.providers); err != nil {
		return err
	}
	if limits != nil && limits.afterDependencies {
		if err := limits.check(c); err != nil {
			return err
		}
	}
	return nil
}

// rateLimitOwners returns every route whose rate limit is completed once the
// application is built: each registered route, and the stand-in each file
// mount counts its requests through. See [frontend.limits].
func (a *App) rateLimitOwners(mounts []*frontend) []*Route {
	owners := slices.Clip(a.routes)
	for _, mount := range mounts {
		owners = append(owners, mount.limits)
	}
	return owners
}

// namesDotfile reports whether any segment of a path relative to a mount
// begins with a dot, other than a leading ".well-known".
func namesDotfile(relative string) bool {
	return anySegment(relative, func(segment string, first bool) bool {
		return strings.HasPrefix(segment, ".") && (!first || segment != ".well-known")
	})
}

// shortNamesResolve reports whether the filesystems of this platform may
// resolve an 8.3 short name, which NTFS generates beside a long one unless a
// volume is configured not to. It is a constant so that the check it guards
// costs nothing where it cannot matter.
const shortNamesResolve = runtime.GOOS == "windows"

// namesShortName reports whether any segment of a path relative to a mount is
// shaped like an 8.3 short name, such as ENV~1 or GIT~1.
//
// On Windows the filesystem opens a long name through its short one, and the
// short name of ".env" is "ENV~1", with the dot gone. A request for /ENV~1 or
// /GIT~1/config therefore reached a dotfile the dotfile check never saw, and
// /ADMINI~1/x reached a directory mounted with guards of its own through a
// parent mount that has none. The name a short name stands for cannot be
// known without asking the filesystem, so every segment of that shape is
// refused, whether or not dotfiles are allowed. A file whose real name looks
// like one is refused with it, which a build output does not produce.
func namesShortName(relative string) bool {
	return anySegment(relative, func(segment string, _ bool) bool {
		return isShortName(segment)
	})
}

// isShortName reports whether one path segment has the shape of an 8.3 short
// name: a base of at most eight characters ending in a tilde and a number,
// then an optional extension of at most three.
//
// Trailing dots and spaces are dropped first, because Windows drops them from
// a name before looking it up, so "ENV~1." opens what "ENV~1" does.
func isShortName(segment string) bool {
	segment = strings.TrimRight(segment, ". ")
	base, extension, _ := strings.Cut(segment, ".")
	if len(base) > 8 || len(extension) > 3 {
		return false
	}
	tilde := strings.LastIndexByte(base, '~')
	if tilde < 0 || tilde == len(base)-1 {
		return false
	}
	for _, digit := range base[tilde+1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

// anySegment reports whether match holds for any segment of a path relative
// to a mount, and tells it whether the segment is the first.
//
// Both separators count, because a filesystem on Windows reads a backslash as
// one, and a name hidden behind it would otherwise pass as part of an
// innocent segment. The path is walked in place rather than split, so the
// check allocates nothing.
func anySegment(relative string, match func(segment string, first bool) bool) bool {
	start := 0
	for i := 0; i <= len(relative); i++ {
		if i < len(relative) && relative[i] != '/' && relative[i] != '\\' {
			continue
		}
		if match(relative[start:i], start == 0) {
			return true
		}
		start = i + 1
	}
	return false
}

// errFrontendUnavailable stands in for a frontend whose files cannot be read,
// so the client is told nothing about the server's filesystem.
var errFrontendUnavailable = errors.New("muzak: the frontend is unavailable")

// resolveFile finds the file a relative request path names, following the
// convention that a directory is served by the index.html inside it.
func resolveFile(files fs.FS, relative string, index bool) (string, bool) {
	// A trailing slash names the same directory as the path without one, and
	// fs rejects it outright, so it is dropped before anything looks at it.
	relative = strings.TrimSuffix(relative, "/")
	if relative == "" {
		if !index {
			return "", false
		}
		relative = "index.html"
	}
	// A path that fs rejects is one no file can be reached through, which is
	// what keeps "../" out of a request.
	if !fs.ValidPath(relative) {
		return "", false
	}
	info, err := fs.Stat(files, relative)
	if err != nil {
		return "", false
	}
	if info.IsDir() {
		if !index {
			return "", false
		}
		nested := path.Join(relative, "index.html")
		if info, err := fs.Stat(files, nested); err == nil && !info.IsDir() {
			return nested, true
		}
		// A directory itself is never served. Listing one would publish the
		// shape of the build output, and no frontend expects it.
		return "", false
	}
	return relative, true
}

// serveFrontendFallback answers a request that named no file, with whichever
// stand-in the mount resolved.
func (a *App) serveFrontendFallback(c *Context, f *frontend, files fs.FS) {
	// Only a read can be answered by a file. Anything else naming a path that
	// exists solely in the frontend is a request for something that is not
	// there, which is a 404 rather than the 405 a real file would give.
	if !isRead(c.r.Method) {
		a.fail(c, frontendNotFound(c.r))
		return
	}
	switch {
	case f.notFound != "":
		f.write(c, files, f.notFound, http.StatusNotFound)
	case f.fallback != "" && acceptsHTML(c.r):
		// The client-side router is being asked for a page it knows about, so
		// the application document answers and takes over from here.
		f.write(c, files, f.fallback, http.StatusOK)
	default:
		a.fail(c, frontendNotFound(c.r))
	}
}

// frontendNotFound reports a path the frontend does not serve, in the same
// envelope every other failure uses.
func frontendNotFound(r *http.Request) error {
	return noRouteError(r)
}

// acceptsHTML reports whether a request is a browser navigation, which is what
// the single page application fallback answers.
//
// A script, stylesheet or image asks for its own type, so this is what keeps a
// missing asset from being answered with an HTML document that the client
// would then fail to parse.
func acceptsHTML(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept") {
		for entry := range strings.SplitSeq(value, ",") {
			media, _, _ := strings.Cut(strings.TrimSpace(entry), ";")
			switch strings.ToLower(strings.TrimSpace(media)) {
			case "text/html", "application/xhtml+xml":
				return true
			}
		}
	}
	return false
}

// write sends one file as the response.
func (f *frontend) write(c *Context, files fs.FS, name string, status int) {
	file, err := files.Open(name)
	if err != nil {
		c.logger.ErrorContext(c.Context(), "muzak: a frontend file vanished between being found and being read",
			slog.String("file", name), slog.String("error", err.Error()))
		http.Error(c.w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		http.Error(c.w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}

	content, ok := file.(io.ReadSeeker)
	if !ok {
		// A filesystem whose files cannot seek still has to answer, so the
		// file is read into memory to give ServeContent something to work
		// with. Frontend assets are small enough for that to be reasonable.
		buffered, err := io.ReadAll(file)
		if err != nil {
			http.Error(c.w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		content = bytes.NewReader(buffered)
	}

	header := c.w.Header()
	// The type comes from the extension alone. ServeContent would otherwise
	// sniff a file with no extension it knows, and a directory of uploads
	// stored under generated names then serves whatever markup a client wrote
	// as text/html, which nosniff cannot undo because it is the server that
	// declared it. A file the extension says nothing about is only bytes.
	setIfAbsent(header, "Content-Type", contentTypeFor(name))
	if f.sandbox && isActiveContent(header.Get("Content-Type")) {
		// Added rather than set: a browser enforces every policy it is sent,
		// so a policy an earlier middleware chose is kept, and this one can
		// only make it stricter.
		header.Add("Content-Security-Policy", "sandbox")
	}

	if status != http.StatusOK {
		// ServeContent always writes 200, and it sets its headers as it does,
		// so writing the status first would send the body with none of them.
		// An error page is described here instead.
		header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		c.w.WriteHeader(status)
		if c.r.Method != http.MethodHead {
			_, _ = io.Copy(c.w, content)
		}
		return
	}
	// ServeContent answers a range request, and turns If-Modified-Since into a
	// 304 where the filesystem records a modification time.
	http.ServeContent(c.w, c.r, name, info.ModTime(), content)
}

// contentTypeFor names the media type of a file from its extension, and never
// from its content: a file with no extension, or one the platform's type table
// does not know, is application/octet-stream, which a browser saves rather
// than renders.
func contentTypeFor(name string) string {
	if declared := mime.TypeByExtension(path.Ext(name)); declared != "" {
		return declared
	}
	return "application/octet-stream"
}

// isActiveContent reports whether a media type is one a browser runs when it
// is opened directly: HTML, and XML in any form, which covers SVG and XHTML
// and anything that can carry an XSLT stylesheet or a script element.
//
// It is decided from the media type rather than the extension, because the
// type table behind [mime.TypeByExtension] comes partly from the platform and
// may give an unexpected extension one of these types.
func isActiveContent(contentType string) bool {
	media, _, _ := strings.Cut(contentType, ";")
	media = strings.ToLower(strings.TrimSpace(media))
	switch media {
	case "text/html", "text/xml", "text/xsl", "application/xml":
		return true
	}
	return strings.HasSuffix(media, "+xml")
}

// frontendFor finds the frontend that should answer a request path, which is
// the most specific mount covering it.
//
// Mount paths are matched exactly, but the filesystem behind a mount may not
// be: on macOS and Windows "ADMIN/secret.txt" opens "admin/secret.txt". With
// a public mount at "/" over a directory whose admin subdirectory is also
// mounted, guarded, at "/admin", a request for /ADMIN/secret.txt missed the
// guarded mount and was served by the public one from the very same file. So
// a request that falls under a more specific mount once case is ignored, or
// once a backslash is read as the separator Windows reads it as, is never
// answered by a less specific one: it answers 404. Routing it to the specific
// mount instead would guess at what the filesystem does; refusing it holds on
// every platform.
func (a *App) frontendFor(requestPath string) (*frontend, string, bool) {
	// Mounts are ordered longest first, so the first one the path falls under
	// loosely is the most specific such mount.
	loose := -1
	for _, mount := range a.frontends {
		if relative, ok := mount.matches(requestPath); ok {
			if len(mount.path) < loose {
				return nil, "", false
			}
			return mount, relative, true
		}
		if loose < 0 && mount.coversLoosely(requestPath) {
			loose = len(mount.path)
		}
	}
	return nil, "", false
}

// coversLoosely reports whether a request path falls under this mount when
// case is ignored and a backslash counts as a separator, which is how a
// case-insensitive filesystem on macOS or Windows would resolve it.
//
// Case is compared rune by rune under Unicode folding rather than byte by
// byte, because a folded spelling need not be the same length: the Kelvin
// sign is three bytes and folds to a one-byte "k".
func (f *frontend) coversLoosely(requestPath string) bool {
	rest, prefix := requestPath, f.path
	for prefix != "" {
		if rest == "" {
			return false
		}
		got, gotSize := utf8.DecodeRuneInString(rest)
		want, wantSize := utf8.DecodeRuneInString(prefix)
		if !equalFoldRune(got, want) {
			return false
		}
		rest, prefix = rest[gotSize:], prefix[wantSize:]
	}
	return rest == "" || rest[0] == '/' || rest[0] == '\\'
}

// equalFoldRune reports whether two runes are the same letter under Unicode
// simple case folding, walking the folding orbit the way strings.EqualFold
// does.
func equalFoldRune(a, b rune) bool {
	if a == b {
		return true
	}
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

// allowedOnFiles is the Allow header of a path served from a filesystem, which
// can only be read.
const allowedOnFiles = "GET, HEAD"

// isRead reports whether a method asks for a representation rather than acting
// on one.
func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// StaticOptions describes a directory of files to serve.
//
// It is the plain form: files are served as they are found and nothing stands
// in for a path with no file behind it. A frontend wants more than that, so a
// single page application belongs in [Router.Frontend] rather than here.
type StaticOptions struct {
	// Dir is the directory holding the files, or the subdirectory within FS
	// when both are set.
	Dir string

	// FS serves the files from a filesystem rather than from disk, which is
	// what an [embed.FS] of assets belonging to a library looks like.
	//
	// The promise that a symbolic link cannot lead out of the directory is
	// kept by Dir, and by the FS of an [os.Root], but not by whatever else is
	// given here: [os.DirFS] follows a link wherever it points, so a link
	// planted in a directory of uploads serves the file it names. To serve a
	// directory on disk, set Dir, or pass the FS of a root opened with
	// [os.OpenRoot].
	FS fs.FS

	// Index serves a directory with the index.html inside it, as a web server
	// does for a site of pages. It is off by default, because a mount of
	// scripts and stylesheets has no index and asking for a directory is a
	// mistake worth reporting.
	Index bool

	// SkipCheck stops the directory being verified when the application is
	// built, for one that something else fills in later.
	SkipCheck bool

	// AllowDotfiles serves files and directories whose name begins with a
	// dot, which are otherwise answered 404 except under a leading
	// ".well-known" directory. See [FrontendOptions.AllowDotfiles] for what
	// relaxing it costs.
	AllowDotfiles bool

	// AllowActiveContent serves HTML, SVG, XHTML and other XML files as
	// ordinary documents. By default each is sent with
	// "Content-Security-Policy: sandbox", so a browser that opens one directly
	// shows it in an isolated origin with scripts, forms and plugins turned
	// off. An image referenced from a page is not affected, because a policy
	// on an image does nothing.
	//
	// Relaxing it costs whatever a client can put in the directory. A mount
	// over uploads serves an attacker's HTML or SVG from the application's own
	// origin, where its script reads the visitor's session and calls the API
	// as them. Turn it on for a directory whose every such file is part of the
	// site, such as a site of pages served with Index; a single page
	// application belongs in [Router.Frontend], which does not sandbox.
	AllowActiveContent bool
}

// Static serves a directory of files at path.
//
//	app.Static("/static", muzak.StaticOptions{Dir: "static"})
//
// It is the same machinery [Router.Frontend] is built on, without the part
// that makes a frontend work: nothing stands in for a path with no file behind
// it, so a miss is a 404 and stays one. Reach for it to publish assets, and
// for [Router.Frontend] to serve an application whose routing happens in the
// browser.
//
// Everything else matches a frontend mount. Routes are matched first, the rate
// limit, guards and providers of the router apply, a directory is never
// listed, a dotfile is not served unless [StaticOptions.AllowDotfiles] says
// so, a symbolic link cannot lead out of the directory named by
// [StaticOptions.Dir] (or served from an [os.Root]; [os.DirFS] follows links),
// and a method other than GET or HEAD on a file that exists is answered 405
// rather than served.
//
// On both kinds of mount a file's type comes from its extension and never from
// its content, so a file with no extension is application/octet-stream. One
// thing differs, because a directory of files may hold files a client wrote:
// an HTML, SVG or other XML file is served under
// "Content-Security-Policy: sandbox" unless [StaticOptions.AllowActiveContent]
// is set, so that markup a client uploaded cannot run script as the
// application.
func (r *Router) Static(mountPath string, opts StaticOptions) {
	r.mustBeOpen("mounting static files at " + mountPath)
	if !strings.HasPrefix(mountPath, "/") {
		r.errs = append(r.errs, fmt.Errorf("muzak: static files at %q: path must begin with %q", mountPath, "/"))
		return
	}
	if opts.Dir == "" && opts.FS == nil {
		r.errs = append(r.errs, fmt.Errorf("muzak: static files at %q: set Dir, FS, or both", mountPath))
		return
	}
	r.frontends = append(r.frontends, &frontend{
		path:    mountPath,
		index:   opts.Index,
		kind:    "static files",
		sandbox: !opts.AllowActiveContent,
		opts: FrontendOptions{
			Dir:           opts.Dir,
			FS:            opts.FS,
			NoFallback:    true,
			SkipCheck:     opts.SkipCheck,
			AllowDotfiles: opts.AllowDotfiles,
		},
	})
}
