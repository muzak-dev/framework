package muzak

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// foldCase is a mount name and the other spellings of it that some filesystem
// resolves to the same directory: full case folding (a ligature or a sharp s
// standing for several letters), canonical normalisation (a precomposed letter
// against its decomposed form), and plain case. The router compares mount
// paths byte for byte and then by simple case folding, so every spelling here
// but the plain case once fell through to the less specific mount.
type foldCase struct {
	mount   string
	aliases []string
}

var foldCases = []foldCase{
	// Plain case, which was already refused and must stay refused.
	{"admin", []string{"ADMIN", "Admin", "aDMIN"}},
	// The ligatures U+FB00 to U+FB06, each standing for several ASCII letters.
	{"staff", []string{"\uFB05aff", "\uFB06aff", "sta\uFB00", "STA\uFB00"}},
	{"files", []string{"\uFB01les", "\uFB01LES"}},
	{"flow", []string{"\uFB02ow"}},
	{"office", []string{"o\uFB03ce"}},
	{"baffle", []string{"ba\uFB04e"}},
	// Sharp s, small and capital, against "ss".
	{"assets", []string{"a\u00DFets", "a\u1E9Eets"}},
	// A decomposed spelling of a precomposed name.
	{"caf\u00E9", []string{"cafe\u0301", "CAFE\u0301"}},
	{"\uD55C\uAD6D", []string{"\u1112\u1161\u11AB\u1100\u116E\u11A8"}},
	{"\u00C5", []string{"A\u030A", "a\u030A"}},
	{"\u01F0", []string{"j\u030C", "J\u030C"}},
	{"\u0130stanbul", []string{"i\u0307stanbul", "I\u0307stanbul"}},
	// An ASCII spelling of a non-ASCII mount name.
	{"stra\u00DFe", []string{"STRASSE", "strasse"}},
	{"\uFB01rst", []string{"first", "FIRST"}},
}

// escaped returns a path as a client sends it, with every byte outside ASCII
// percent-encoded.
func escaped(p string) string {
	segments := strings.Split(p, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// navigation is a browser navigation, which is what a single page application
// answers with its document.
func navigation(target string) *http.Request {
	req := httptest.NewRequest("GET", target, nil)
	req.Header.Set("Accept", "text/html")
	return req
}

// nestedLayout is a public mount at the root over a directory whose
// subdirectories are each mounted again, guarded, at their own name.
func nestedLayout(t *testing.T, root string, mounts []string, spa bool) *App {
	t.Helper()
	app := New(quietOptions())
	if spa {
		app.Frontend("/", FrontendOptions{Dir: root})
	} else {
		app.Static("/", StaticOptions{Dir: root, Index: true})
	}
	for _, name := range mounts {
		guarded := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
		guarded.Static("/", StaticOptions{Dir: filepath.Join(root, name)})
		app.Include(guarded, WithPrefix("/"+name))
	}
	return mustBuild(t, app)
}

// TestGuardedMountIsNotReachedThroughItsFoldedName is the regression test for
// a guarded mount served, with no credentials, by a public parent mount over
// the same directory tree. It runs against a real directory and asks the
// volume, rather than assuming, which spellings it treats as one name: on a
// filesystem that folds none of them the request cannot reach the file and
// there is nothing to defend, so those cases skip, and a case-sensitive Linux
// volume skips them all. TestNestedMountsByIdentity runs the same logic on
// every platform.
func TestGuardedMountIsNotReachedThroughItsFoldedName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	var mounts []string
	for _, c := range foldCases {
		if err := os.MkdirAll(filepath.Join(root, c.mount), 0o755); err != nil {
			t.Logf("this volume cannot hold a directory named %q: %v", c.mount, err)
			continue
		}
		body := []byte("SECRET-" + c.mount)
		if err := os.WriteFile(filepath.Join(root, c.mount, "secret.txt"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		mounts = append(mounts, c.mount)
	}
	for _, spa := range []bool{false, true} {
		app := nestedLayout(t, root, mounts, spa)
		for _, c := range foldCases {
			for _, alias := range c.aliases {
				t.Run(c.mount+"/"+alias, func(t *testing.T) {
					if _, err := os.Stat(filepath.Join(root, alias, "secret.txt")); err != nil {
						t.Skipf("this volume does not treat %q as %q, so the parent mount cannot open the guarded directory through it", alias, c.mount)
					}
					target := "/" + escaped(alias) + "/secret.txt"
					for _, req := range []*http.Request{
						httptest.NewRequest("GET", target, nil),
						navigation(target),
						navigation("/" + escaped(alias)),
						navigation("/" + escaped(alias) + "/nothing-here"),
					} {
						rec := doRequest(t, app, req)
						if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "SECRET-") || rec.Body.String() == "public" {
							t.Errorf("GET %s (spa=%v) = %d %q, want 404 and none of the guarded directory", req.URL.Path, spa, rec.Code, rec.Body.String())
						}
					}
				})
			}
			// The exact spelling still reaches the mount that owns the
			// directory, so its guard answers.
			exact := "/" + escaped(c.mount) + "/secret.txt"
			if _, err := os.Stat(filepath.Join(root, c.mount, "secret.txt")); err != nil {
				continue
			}
			assertStatus(t, do(t, app, "GET", exact), http.StatusUnauthorized)
			req := httptest.NewRequest("GET", exact, nil)
			req.Header.Set("Authorization", "Bearer s3cret")
			rec := doRequest(t, app, req)
			assertStatus(t, rec, http.StatusOK)
			if rec.Body.String() != "SECRET-"+c.mount {
				t.Errorf("GET %s = %q, want the guarded file", exact, rec.Body.String())
			}
		}
		assertStatus(t, do(t, app, "GET", "/"), http.StatusOK)
	}
}

// TestFoldedNameKeepsSandboxAndPrivateCaching pins the two things a request
// served by the wrong mount used to lose besides the guard: the sandbox policy
// of a directory of uploads, and the "private" cache directive of a guarded one.
func TestFoldedNameKeepsSandboxAndPrivateCaching(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range map[string]string{
		"index.html":       "public",
		"files/x.html":     "<script>steal()</script>",
		"staff/secret.txt": "SECRET",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "\uFB01les", "x.html")); err != nil {
		t.Skip("this volume does not fold the fi ligature onto \"fi\", so there is no alias to defend against")
	}
	app := New(quietOptions())
	app.Frontend("/", FrontendOptions{Dir: root})
	app.Static("/files", StaticOptions{Dir: filepath.Join(root, "files")})
	guarded := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	guarded.Static("/", StaticOptions{Dir: filepath.Join(root, "staff")})
	app.Include(guarded, WithPrefix("/staff"))
	mustBuild(t, app)

	exact := do(t, app, "GET", "/files/x.html")
	assertStatus(t, exact, http.StatusOK)
	if exact.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("the uploads mount sends no sandbox policy: %v", exact.Header())
	}
	folded := do(t, app, "GET", "/%EF%AC%81les/x.html")
	assertStatus(t, folded, http.StatusNotFound)
	if strings.Contains(folded.Body.String(), "steal") {
		t.Fatalf("the folded name served the uploaded document: %q", folded.Body.String())
	}
	owned := doRequest(t, app, withToken("/staff/secret.txt"))
	assertStatus(t, owned, http.StatusOK)
	if got := owned.Header().Get("Cache-Control"); got != "private, no-cache" {
		t.Fatalf("the guarded mount sends Cache-Control %q", got)
	}
}

// TestNestedMountsFollowLinksInsideTheirDirectory pins what a symbolic link in
// a served directory does. One that stays inside the directory is followed and
// served by the mount that owns it; one that leads into the directory of a
// mount beneath is refused like any other name for that directory.
func TestNestedMountsFollowLinksInsideTheirDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range map[string]string{
		"index.html":       "public",
		"pub/note.txt":     "a public note",
		"staff/secret.txt": "SECRET",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("pub", filepath.Join(root, "publink")); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	if err := os.Symlink("staff", filepath.Join(root, "stafflink")); err != nil {
		t.Fatal(err)
	}
	app := nestedLayout(t, root, []string{"staff"}, false)

	assertStatus(t, do(t, app, "GET", "/publink/note.txt"), http.StatusOK)
	rec := do(t, app, "GET", "/stafflink/secret.txt")
	assertStatus(t, rec, http.StatusNotFound)
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatal("a link inside the parent's directory served the guarded directory")
	}
	assertStatus(t, do(t, app, "GET", "/staff/secret.txt"), http.StatusUnauthorized)
}

// TestNestedMountsAreFoundFromTheirDirectories checks the relationship the
// check is limited by: only a mount whose directory lies inside another's is a
// candidate for being reached through it, whatever way the two directories are
// spelled, and a mount with none beneath it pays nothing.
func TestNestedMountsAreFoundFromTheirDirectories(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	site, other := filepath.Join(base, "site"), filepath.Join(base, "other")
	for _, dir := range []string{filepath.Join(site, "staff"), other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	via := filepath.Join(base, "via")
	if err := os.Symlink(site, via); err != nil {
		t.Skipf("cannot create a symbolic link here: %v", err)
	}
	// The parent is named through a link, the way a deployment's "current"
	// directory is, and the child by its real path.
	app := New(quietOptions())
	app.Static("/", StaticOptions{Dir: via})
	app.Static("/staff", StaticOptions{Dir: filepath.Join(site, "staff")})
	app.Static("/other", StaticOptions{Dir: other})
	mustBuild(t, app)

	nested := map[string][]string{}
	for _, mount := range app.frontends {
		for _, inner := range mount.nested {
			nested[mount.mountPath()] = append(nested[mount.mountPath()], inner.mountPath())
		}
	}
	if got := nested["/"]; len(got) != 1 || got[0] != "/staff" {
		t.Errorf("mounts inside the root mount's directory = %v, want only /staff", got)
	}
	if len(nested) != 1 {
		t.Errorf("mounts with mounts inside = %v, want only /", nested)
	}
}

// identityInfo is a FileInfo that knows which directory it describes, as the
// FileInfo of a real filesystem does through os.SameFile.
type identityInfo struct {
	fs.FileInfo
	id int
}

func (i identityInfo) sameFile(other fs.FileInfo) bool {
	o, ok := other.(identityInfo)
	return ok && o.id == i.id
}

// idOf gives a name with no declared identity one of its own, well away from
// the identities a test declares.
func idOf(name string) int {
	sum := 1 << 20
	for _, b := range []byte(name) {
		sum = sum*31 + int(b)
	}
	return sum
}

// identityFS models a filesystem on which several names open one directory,
// which is what a case-folding and normalising volume does, without needing
// one: ids says which directory each name is, and a name it does not list is
// a directory of its own. It counts the stats it is asked for.
type identityFS struct {
	fstest.MapFS
	ids   map[string]int
	stats atomic.Int64
}

func (f *identityFS) Stat(name string) (fs.FileInfo, error) {
	f.stats.Add(1)
	info, err := f.MapFS.Stat(name)
	if err != nil {
		return nil, err
	}
	id, ok := f.ids[name]
	if !ok {
		id = idOf(name)
	}
	return identityInfo{info, id}, nil
}

// rootFS is the filesystem of a mount whose own directory has a known
// identity.
type rootFS struct {
	fstest.MapFS
	id int
}

func (f rootFS) Stat(name string) (fs.FileInfo, error) {
	info, err := f.MapFS.Stat(name)
	if err != nil {
		return nil, err
	}
	if name == "." {
		return identityInfo{info, f.id}, nil
	}
	return identityInfo{info, idOf(name)}, nil
}

// TestNestedMountsByIdentity runs the defence against a filesystem model, so
// it holds on every platform: whatever spelling a request uses, a path whose
// directory prefix is the directory of a mount beneath is refused by the mount
// above, and one whose prefix is any other directory is served as before.
func TestNestedMountsByIdentity(t *testing.T) {
	t.Parallel()
	parent := &identityFS{
		MapFS: fstest.MapFS{
			"index.html":    {Data: []byte("public")},
			"pub/note.txt":  {Data: []byte("a public note")},
			"deep/a/b/c.js": {Data: []byte("nested public")},
		},
		ids: map[string]int{},
	}
	app := New(quietOptions())
	var spellings []string
	for i, c := range foldCases {
		id := i + 1
		parent.MapFS[c.mount+"/secret.txt"] = &fstest.MapFile{Data: []byte("SECRET-" + c.mount)}
		parent.ids[c.mount] = id
		for _, alias := range c.aliases {
			parent.MapFS[alias+"/secret.txt"] = &fstest.MapFile{Data: []byte("SECRET-" + c.mount)}
			parent.ids[alias] = id
			spellings = append(spellings, alias)
		}
		guarded := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
		guarded.Static("/", StaticOptions{FS: rootFS{
			MapFS: fstest.MapFS{"secret.txt": {Data: []byte("SECRET-" + c.mount)}},
			id:    id,
		}})
		app.Include(guarded, WithPrefix("/"+c.mount))
	}
	// A link, or a second name, for a guarded directory nested deeper down.
	parent.MapFS["deep/a/link/secret.txt"] = &fstest.MapFile{Data: []byte("SECRET-admin")}
	parent.ids["deep/a/link"] = 1
	app.Frontend("/", FrontendOptions{FS: parent})
	mustBuild(t, app)

	for _, c := range foldCases {
		exact := "/" + escaped(c.mount) + "/secret.txt"
		assertStatus(t, do(t, app, "GET", exact), http.StatusUnauthorized)
	}
	for _, alias := range append(spellings, "deep/a/link") {
		target := "/" + escaped(alias) + "/secret.txt"
		for _, req := range []*http.Request{
			httptest.NewRequest("GET", target, nil),
			navigation(target),
			navigation("/" + escaped(alias)),
			navigation("/" + escaped(alias) + "/missing"),
			httptest.NewRequest("HEAD", target, nil),
		} {
			rec := doRequest(t, app, req)
			if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "SECRET-") || rec.Body.String() == "public" {
				t.Errorf("%s %s = %d %q, want 404 and none of the guarded directory", req.Method, req.URL.Path, rec.Code, rec.Body.String())
			}
		}
	}

	// Anything else is served as before, and a navigation to a path that names
	// nothing still gets the application's document.
	rec := do(t, app, "GET", "/pub/note.txt")
	assertStatus(t, rec, http.StatusOK)
	assertStatus(t, do(t, app, "GET", "/deep/a/b/c.js"), http.StatusOK)
	assertStatus(t, doRequest(t, app, navigation("/pub/not-a-file")), http.StatusOK)
	if rec := doRequest(t, app, navigation("/no/such/page")); rec.Code != http.StatusOK || rec.Body.String() != "public" {
		t.Errorf("a navigation to an unknown page = %d %q, want the fallback document", rec.Code, rec.Body.String())
	}
}

// TestNestedMountsByIdentityCostNothingWhereTheyNeedNothing checks that the
// defence asks a filesystem nothing when there is nothing to defend: no mount
// beneath, or a request the mount beneath already owns.
func TestNestedMountsByIdentityCostNothingWhereTheyNeedNothing(t *testing.T) {
	t.Parallel()
	files := func() *identityFS {
		return &identityFS{
			MapFS: fstest.MapFS{
				"index.html":       {Data: []byte("public")},
				"pub/note.txt":     {Data: []byte("note")},
				"staff/secret.txt": {Data: []byte("SECRET")},
			},
			ids: map[string]int{"staff": 1},
		}
	}

	alone := files()
	lone := New(quietOptions())
	lone.Static("/", StaticOptions{FS: alone})
	mustBuild(t, lone)
	assertStatus(t, do(t, lone, "GET", "/pub/note.txt"), http.StatusOK)
	if got := alone.stats.Load(); got != 1 {
		t.Errorf("a mount with nothing beneath it stats %d times to serve a file, want the one lookup that finds it", got)
	}

	parent := files()
	nested := New(quietOptions())
	nested.Static("/", StaticOptions{FS: parent})
	guarded := NewRouter(WithDependencies(RequireBearerToken("s3cret")))
	guarded.Static("/", StaticOptions{FS: rootFS{MapFS: fstest.MapFS{"secret.txt": {Data: []byte("SECRET")}}, id: 1}})
	nested.Include(guarded, WithPrefix("/staff"))
	mustBuild(t, nested)
	assertStatus(t, do(t, nested, "GET", "/staff/secret.txt"), http.StatusUnauthorized)
	if got := parent.stats.Load(); got != 0 {
		t.Errorf("a request the mount beneath owns cost the mount above %d stats, want none", got)
	}
}
