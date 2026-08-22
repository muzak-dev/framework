package badele

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
	"strconv"
	"strings"
	"sync"
)

// FrontendOptions describes a built frontend to serve.
//
// Exactly one source is required. Dir names a directory on disk, which is what
// a build step such as "npm run build" produces:
//
//	app.Frontend("/", badele.FrontendOptions{Dir: "dist"})
//
// FS serves the frontend from an [io/fs.FS] instead, which is how a frontend
// gets built into the binary. Setting both reads Dir as a subdirectory of FS,
// which is what the directory embed produces:
//
//	//go:embed all:dist
//	var assets embed.FS
//
//	app.Frontend("/", badele.FrontendOptions{FS: assets, Dir: "dist"})
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
	FS fs.FS

	// Fallback is the file served, with 200, when a browser navigates to a
	// path with no file behind it, so that a client-side router can take over.
	// It is the entry document of a single page application, normally
	// "index.html".
	//
	// It is used only for a GET or HEAD that asks for HTML, which is what a
	// navigation does. A missing script, stylesheet or image still answers
	// 404, because handing those an HTML document only turns a missing file
	// into a confusing parse error.
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
}

// frontend is one resolved frontend mount: a path, the filesystem behind it,
// and the guards it inherited from the routers it was registered under.
type frontend struct {
	// path is the full mount path, with no trailing slash unless it is root.
	path   string
	opts   FrontendOptions
	guards []Guard

	// open resolves the filesystem once, so that a directory named but not yet
	// built is reported on the first request rather than at start-up.
	once  sync.Once
	files fs.FS
	err   error

	// fallback and notFound are the files resolved from the options and from
	// what the build produced, empty when there is nothing to fall back to.
	fallback string
	notFound string
}

// Frontend serves a built frontend at path.
//
// It is for the static output of a frontend build, which is what React, Vue,
// Svelte, Angular, Solid, Astro and the rest produce. Nothing is rendered on
// the server, and nothing is built here: this serves files that already exist.
//
//	app.Frontend("/", badele.FrontendOptions{Dir: "dist"})
//
// Routes win. A request is matched against every registered route first, and
// reaches the frontend only when none of them answered, so mounting a frontend
// at "/" cannot shadow an API. Middleware still applies, as do the guards of
// the routers the frontend was registered under, which is what lets a frontend
// sit behind the same authentication as everything else.
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
//	ui := badele.NewRouter()
//	ui.Frontend("/", badele.FrontendOptions{Dir: "dist"})
//	app.Include(ui, badele.WithPrefix("/app"))
//
// Problems with the mount, including a directory that does not exist, are
// reported when the application is built rather than on the first request.
func (r *Router) Frontend(mountPath string, opts FrontendOptions) {
	if !strings.HasPrefix(mountPath, "/") {
		r.errs = append(r.errs, fmt.Errorf("badele: frontend at %q: path must begin with %q", mountPath, "/"))
		return
	}
	if opts.Dir == "" && opts.FS == nil {
		r.errs = append(r.errs, fmt.Errorf("badele: frontend at %q: set Dir, FS, or both", mountPath))
		return
	}
	if opts.NoFallback && (opts.Fallback != "" || opts.NotFound != "") {
		r.errs = append(r.errs, fmt.Errorf("badele: frontend at %q: NoFallback cannot be combined with Fallback or NotFound", mountPath))
		return
	}
	r.frontends = append(r.frontends, &frontend{path: mountPath, opts: opts})
}

// resolve computes a frontend's final mount path and guard chain, and verifies
// what it serves unless the mount asked not to be checked.
func (f *frontend) resolve(in inherited) error {
	f.path = strings.TrimSuffix(in.prefix+f.path, "/")
	f.guards = in.guards

	if f.opts.SkipCheck {
		// The files are named but not promised. Whether they are there is
		// decided by the first request that needs them.
		f.fallback, f.notFound = f.opts.Fallback, f.opts.NotFound
		return nil
	}
	files, err := f.resolveFS()
	if err != nil {
		return fmt.Errorf("badele: frontend at %q: %w", f.mountPath(), err)
	}
	for _, named := range []struct{ what, name string }{
		{"Fallback", f.opts.Fallback},
		{"NotFound", f.opts.NotFound},
	} {
		if named.name == "" {
			continue
		}
		if _, err := fs.Stat(files, named.name); err != nil {
			return fmt.Errorf("badele: frontend at %q: %s file %q: %w", f.mountPath(), named.what, named.name, err)
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
	for _, guard := range f.guards {
		if err := guard(c); err != nil {
			a.fail(c, err)
			return
		}
	}

	files, err := f.fsys()
	if err != nil {
		a.logger.ErrorContext(c.Context(), "badele: the frontend directory could not be opened",
			slog.String("mount", f.mountPath()),
			slog.String(RequestIDKey, c.RequestID()),
			slog.String("error", err.Error()))
		a.fail(c, errFrontendUnavailable)
		return
	}

	if name, ok := resolveFile(files, relative); ok {
		if !isRead(c.r.Method) {
			// The file is there. What is not allowed is the method, and
			// answering 404 would say the opposite.
			c.w.Header().Set("Allow", allowedOnFiles)
			a.fail(c, NewHTTPErrorf(http.StatusMethodNotAllowed,
				"%s is not allowed here; allowed methods are %s", c.r.Method, allowedOnFiles))
			return
		}
		f.write(c, files, name, http.StatusOK)
		return
	}
	a.serveFrontendFallback(c, f, files)
}

// errFrontendUnavailable stands in for a frontend whose files cannot be read,
// so the client is told nothing about the server's filesystem.
var errFrontendUnavailable = errors.New("badele: the frontend is unavailable")

// resolveFile finds the file a relative request path names, following the
// convention that a directory is served by the index.html inside it.
func resolveFile(files fs.FS, relative string) (string, bool) {
	// A trailing slash names the same directory as the path without one, and
	// fs rejects it outright, so it is dropped before anything looks at it.
	relative = strings.TrimSuffix(relative, "/")
	if relative == "" {
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
	return NewHTTPErrorf(http.StatusNotFound, "no route matches %s %s", r.Method, r.URL.Path)
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
		c.logger.ErrorContext(c.Context(), "badele: a frontend file vanished between being found and being read",
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

	if status != http.StatusOK {
		// ServeContent always writes 200, and it sets its headers as it does,
		// so writing the status first would send the body with none of them.
		// An error page is described here instead.
		header := c.w.Header()
		setIfAbsent(header, "Content-Type", contentTypeFor(name))
		header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		c.w.WriteHeader(status)
		if c.r.Method != http.MethodHead {
			_, _ = io.Copy(c.w, content)
		}
		return
	}
	// ServeContent picks the content type from the extension, answers a range
	// request, and turns If-Modified-Since into a 304 where the filesystem
	// records a modification time.
	http.ServeContent(c.w, c.r, name, info.ModTime(), content)
}

// contentTypeFor names the media type of a file from its extension, which is
// the same rule net/http applies when it serves one.
func contentTypeFor(name string) string {
	if declared := mime.TypeByExtension(path.Ext(name)); declared != "" {
		return declared
	}
	return "application/octet-stream"
}

// frontendFor finds the frontend that should answer a request path, which is
// the most specific mount covering it.
func (a *App) frontendFor(requestPath string) (*frontend, string, bool) {
	for _, mount := range a.frontends {
		if relative, ok := mount.matches(requestPath); ok {
			return mount, relative, true
		}
	}
	return nil, "", false
}

// allowedOnFiles is the Allow header of a path served from a filesystem, which
// can only be read.
const allowedOnFiles = "GET, HEAD"

// isRead reports whether a method asks for a representation rather than acting
// on one.
func isRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}
