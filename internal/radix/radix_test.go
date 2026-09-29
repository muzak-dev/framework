package radix

import (
	"errors"
	"strings"
	"testing"
)

func TestInsertRejectsBadPatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pattern string
		want    error
	}{
		{"no leading slash", "users", ErrInvalidPattern},
		{"empty", "", ErrInvalidPattern},
		{"unclosed brace", "/users/{id", ErrInvalidPattern},
		{"unopened brace", "/users/id}", ErrInvalidPattern},
		{"brace mid segment", "/users/v{id}x", ErrInvalidPattern},
		{"prefix before brace", "/users/v{id}", ErrInvalidPattern},
		{"empty param name", "/users/{}", ErrInvalidPattern},
		{"empty wildcard name", "/files/{...}", ErrInvalidPattern},
		{"nested braces", "/users/{a{b}}", ErrInvalidPattern},
		{"brace inside param name", "/users/{a{b}", ErrInvalidPattern},
		{"wildcard not last", "/files/{rest...}/tail", ErrInvalidPattern},
		{"encoded separator", "/a%2Fb", ErrInvalidPattern},
		{"encoded separator lower case", "/a%2fb", ErrInvalidPattern},
		{"bare percent", "/100%", ErrInvalidPattern},
		{"truncated escape", "/a%4", ErrInvalidPattern},
		{"non-hex escape", "/%zz", ErrInvalidPattern},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := New[int]()
			err := tree.Insert(tc.pattern, 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Insert(%q) error = %v, want %v", tc.pattern, err, tc.want)
			}
			if !strings.Contains(err.Error(), "radix:") {
				t.Errorf("error %q should name the package", err)
			}
		})
	}
}

func TestInsertRejectsConflicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		first  string
		second string
		want   error
	}{
		{"duplicate static", "/users", "/users", ErrDuplicateRoute},
		{"duplicate param", "/users/{id}", "/users/{id}", ErrDuplicateRoute},
		{"duplicate wildcard", "/files/{rest...}", "/files/{rest...}", ErrDuplicateRoute},
		{"param name conflict", "/users/{id}", "/users/{name}", ErrParamConflict},
		{"wildcard name conflict", "/files/{a...}", "/files/{b...}", ErrParamConflict},
		{"same static once decoded", "/users/admin", "/users/%61dmin", ErrDuplicateRoute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tree := New[int]()
			if err := tree.Insert(tc.first, 1); err != nil {
				t.Fatalf("first Insert(%q) = %v", tc.first, err)
			}
			err := tree.Insert(tc.second, 2)
			if !errors.Is(err, tc.want) {
				t.Fatalf("second Insert(%q) error = %v, want %v", tc.second, err, tc.want)
			}
		})
	}
}

// buildTree returns a tree covering every pattern shape the router supports.
func buildTree(t *testing.T) *Tree[string] {
	t.Helper()
	tree := New[string]()
	for _, pattern := range []string{
		"/",
		"/users",
		"/users/",
		"/users/me",
		"/users/me/posts",
		"/users/{id}",
		"/users/{id}/posts",
		"/files/{path...}",
		"/a/b/c",
	} {
		if err := tree.Insert(pattern, pattern); err != nil {
			t.Fatalf("Insert(%q) = %v", pattern, err)
		}
	}
	return tree
}

func TestLookup(t *testing.T) {
	t.Parallel()
	tree := buildTree(t)

	tests := []struct {
		name    string
		path    string
		want    string
		params  map[string]string
		missing bool
	}{
		{name: "root", path: "/", want: "/"},
		{name: "static", path: "/users", want: "/users"},
		{name: "trailing slash is distinct", path: "/users/", want: "/users/"},
		{name: "static beats param", path: "/users/me", want: "/users/me"},
		{name: "param", path: "/users/42", want: "/users/{id}", params: map[string]string{"id": "42"}},
		{name: "static child of static", path: "/users/me/posts", want: "/users/me/posts"},
		{name: "param child", path: "/users/42/posts", want: "/users/{id}/posts", params: map[string]string{"id": "42"}},
		{
			// "me" matches the static branch, which has no "comments" child, so
			// matching backtracks into the parameter branch.
			name:   "backtrack from static into param",
			path:   "/users/me/x",
			want:   "",
			params: nil, missing: true,
		},
		{name: "wildcard", path: "/files/a/b/c.txt", want: "/files/{path...}", params: map[string]string{"path": "a/b/c.txt"}},
		{name: "wildcard matches empty", path: "/files/", want: "/files/{path...}", params: map[string]string{"path": ""}},
		{name: "wildcard needs a separator", path: "/files", missing: true},
		{name: "deep static", path: "/a/b/c", want: "/a/b/c"},
		{name: "unknown", path: "/nope", missing: true},
		{name: "too deep", path: "/a/b/c/d", missing: true},
		{name: "param refuses empty segment", path: "/users//posts", missing: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var params Params
			got, ok := tree.Lookup(tc.path, &params)
			if tc.missing {
				if ok {
					t.Fatalf("Lookup(%q) = %q, want no match", tc.path, got)
				}
				if params.Len() != 0 {
					t.Errorf("a failed lookup left %d captured parameters behind", params.Len())
				}
				return
			}
			if !ok {
				t.Fatalf("Lookup(%q) found nothing, want %q", tc.path, tc.want)
			}
			if got != tc.want {
				t.Fatalf("Lookup(%q) = %q, want %q", tc.path, got, tc.want)
			}
			if params.Len() != len(tc.params) {
				t.Fatalf("captured %d parameters, want %d", params.Len(), len(tc.params))
			}
			for name, want := range tc.params {
				value, found := params.Get(name)
				if !found {
					t.Fatalf("parameter %q was not captured", name)
				}
				if value != want {
					t.Errorf("parameter %q = %q, want %q", name, value, want)
				}
			}
		})
	}
}

// TestLookupBacktracksIntoWildcard exercises the path where a static branch and
// a parameter branch both fail and matching falls through to the wildcard.
func TestLookupBacktracksIntoWildcard(t *testing.T) {
	t.Parallel()
	tree := New[string]()
	for _, pattern := range []string{"/x/static/deep", "/x/{id}/other", "/x/{rest...}"} {
		if err := tree.Insert(pattern, pattern); err != nil {
			t.Fatalf("Insert(%q) = %v", pattern, err)
		}
	}
	var params Params
	got, ok := tree.Lookup("/x/static/nomatch", &params)
	if !ok || got != "/x/{rest...}" {
		t.Fatalf("Lookup = %q, %v; want the wildcard", got, ok)
	}
	value, _ := params.Get("rest")
	if value != "static/nomatch" {
		t.Errorf("wildcard captured %q, want %q", value, "static/nomatch")
	}
}

// TestLookupDecodesStaticSegments pins that a static segment is compared in
// its decoded form. Comparing the escaped text instead is what let an encoded
// byte steer a request away from a guarded static route and into a sibling
// parameter that captured the same value.
func TestLookupDecodesStaticSegments(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 2*maxStackSegment)
	tree := New[string]()
	for _, pattern := range []string{
		"/users/admin", "/users/{id}", "/files/{path...}", "/files/readme",
		"/caf\u00e9", "/100%25", "/a b", "/" + long,
	} {
		if err := tree.Insert(pattern, pattern); err != nil {
			t.Fatalf("Insert(%q) = %v", pattern, err)
		}
	}
	tests := []struct {
		name    string
		path    string
		want    string
		capture string
	}{
		{name: "plain", path: "/users/admin", want: "/users/admin"},
		{name: "one byte encoded", path: "/users/%61dmin", want: "/users/admin"},
		{name: "last byte encoded", path: "/users/admi%6E", want: "/users/admin"},
		{name: "every byte encoded", path: "/users/%61%64%6d%69%6e", want: "/users/admin"},
		{name: "before a wildcard", path: "/files/%72eadme", want: "/files/readme"},
		{name: "encoded utf-8", path: "/caf%C3%A9", want: "/caf\u00e9"},
		{name: "encoded percent", path: "/100%25", want: "/100%25"},
		{name: "encoded space", path: "/a%20b", want: "/a b"},
		{name: "longer than the stack buffer", path: "/%61" + long[1:], want: "/" + long},
		{name: "encoded separator stays a parameter", path: "/users/admin%2F", want: "/users/{id}", capture: "admin%2F"},
		{name: "encoded separator alone", path: "/users/%2f", want: "/users/{id}", capture: "%2f"},
		{name: "malformed escape stays a parameter", path: "/users/%zzadmin", want: "/users/{id}", capture: "%zzadmin"},
		{name: "truncated escape stays a parameter", path: "/users/admin%6", want: "/users/{id}", capture: "admin%6"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var params Params
			got, ok := tree.Lookup(tc.path, &params)
			if !ok || got != tc.want {
				t.Fatalf("Lookup(%q) = %q, %v; want %q", tc.path, got, ok, tc.want)
			}
			if tc.capture == "" {
				if params.Len() != 0 {
					t.Fatalf("a static match captured %d parameters", params.Len())
				}
				return
			}
			if value, _ := params.Get("id"); value != tc.capture {
				t.Errorf("captured %q, want the escaped text %q", value, tc.capture)
			}
		})
	}

	var params Params
	if got, ok := tree.Lookup("/100%", &params); ok {
		t.Errorf("a malformed escape matched the static segment %q", got)
	}
}

func TestParams(t *testing.T) {
	t.Parallel()
	var params Params
	if params.Len() != 0 {
		t.Fatalf("the zero Params has %d entries, want 0", params.Len())
	}
	if _, ok := params.Get("missing"); ok {
		t.Error("Get on an empty Params reported a value")
	}

	params.push("a", "1")
	params.push("b", "2")
	if params.Len() != 2 {
		t.Fatalf("Len = %d, want 2", params.Len())
	}
	name, value := params.At(1)
	if name != "b" || value != "2" {
		t.Errorf("At(1) = %q, %q; want %q, %q", name, value, "b", "2")
	}

	params.SetValue(0, "decoded")
	if got, _ := params.Get("a"); got != "decoded" {
		t.Errorf("after SetValue, Get(a) = %q, want %q", got, "decoded")
	}

	params.truncate(1)
	if params.Len() != 1 {
		t.Errorf("after truncate, Len = %d, want 1", params.Len())
	}

	params.Reset()
	if params.Len() != 0 {
		t.Errorf("after Reset, Len = %d, want 0", params.Len())
	}
}

// TestParamsGetReturnsFirst documents that the first capture of a name wins,
// which only matters for a Params reused without a reset.
func TestParamsGetReturnsFirst(t *testing.T) {
	t.Parallel()
	var params Params
	params.push("id", "first")
	params.push("id", "second")
	if got, _ := params.Get("id"); got != "first" {
		t.Errorf("Get(id) = %q, want %q", got, "first")
	}
}

func TestLen(t *testing.T) {
	t.Parallel()
	tree := New[int]()
	if tree.Len() != 0 {
		t.Fatalf("a new tree reports %d patterns, want 0", tree.Len())
	}
	for i, pattern := range []string{"/a", "/b", "/c/{d}"} {
		if err := tree.Insert(pattern, i); err != nil {
			t.Fatalf("Insert(%q) = %v", pattern, err)
		}
	}
	if tree.Len() != 3 {
		t.Errorf("Len = %d, want 3", tree.Len())
	}
}

// TestLookupIsAllocationFree pins the property the whole design exists for.
func TestLookupIsAllocationFree(t *testing.T) {
	tree := buildTree(t)
	var params Params
	// Warm the backing arrays so the measured runs reuse them.
	tree.Lookup("/users/42/posts", &params)
	params.Reset()

	allocs := testing.AllocsPerRun(200, func() {
		params.Reset()
		tree.Lookup("/users/42/posts", &params)
	})
	if allocs != 0 {
		t.Errorf("Lookup allocated %.1f times per call, want 0", allocs)
	}

	// An encoded segment is decoded on the stack, so it costs nothing either.
	allocs = testing.AllocsPerRun(200, func() {
		params.Reset()
		tree.Lookup("/users/%6De/posts", &params)
	})
	if allocs != 0 {
		t.Errorf("Lookup of an encoded segment allocated %.1f times per call, want 0", allocs)
	}
}

func FuzzLookup(f *testing.F) {
	tree := New[string]()
	for _, pattern := range []string{
		"/", "/users", "/users/", "/users/me", "/users/{id}", "/users/{id}/posts",
		"/files/{path...}", "/a/b/c",
	} {
		if err := tree.Insert(pattern, pattern); err != nil {
			f.Fatalf("Insert(%q) = %v", pattern, err)
		}
	}
	for _, seed := range []string{
		"/", "", "/users", "/users/", "//", "/users//posts", "/users/%2F/posts",
		"/files/a/b", strings.Repeat("/a", 200), "/users/\x00", "/users/{id}",
		"/users/%6De", "/users/%6", "/users/%", "/users/%2F%6D",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, path string) {
		var params Params
		// The contract under test: matching untrusted input never panics, and a
		// failed match leaves no captures behind.
		value, ok := tree.Lookup(path, &params)
		if !ok {
			if params.Len() != 0 {
				t.Fatalf("failed lookup of %q left %d captures", path, params.Len())
			}
			return
		}
		if value == "" {
			t.Fatalf("lookup of %q succeeded with an empty value", path)
		}
		for i := range params.Len() {
			name, _ := params.At(i)
			if name == "" {
				t.Fatalf("lookup of %q captured a parameter with no name", path)
			}
		}
	})
}

func FuzzInsert(f *testing.F) {
	for _, seed := range []string{
		"/users/{id}", "/{a}/{b}", "/files/{rest...}", "/", "users", "/{",
		"/}", "/{}", "/{...}", "/a/{b...}/c", strings.Repeat("/{a}", 40),
		"/%61", "/100%25", "/a%2Fb", "/%",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		tree := New[int]()
		// Insert must either accept a pattern or reject it with a classified
		// error; it must never panic on hostile input.
		if err := tree.Insert(pattern, 1); err != nil {
			if !errors.Is(err, ErrInvalidPattern) && !errors.Is(err, ErrDuplicateRoute) && !errors.Is(err, ErrParamConflict) {
				t.Fatalf("Insert(%q) returned an unclassified error: %v", pattern, err)
			}
			return
		}
		// An accepted pattern must be findable by a path derived from it.
		var params Params
		if _, ok := tree.Lookup(concretePath(pattern), &params); !ok && !strings.Contains(pattern, "{") {
			t.Fatalf("Insert accepted %q but Lookup could not find it", pattern)
		}
	})
}

// concretePath substitutes a value for each parameter so that a pattern can be
// looked up as though it were a request path.
func concretePath(pattern string) string {
	var b strings.Builder
	for i, seg := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if i > 0 || true {
			b.WriteByte('/')
		}
		if len(seg) > 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			b.WriteString("value")
			continue
		}
		b.WriteString(seg)
	}
	return b.String()
}

func BenchmarkLookupStatic(b *testing.B) {
	tree := benchTree(b)
	var params Params
	b.ReportAllocs()
	for b.Loop() {
		params.Reset()
		tree.Lookup("/api/v1/users/me/settings", &params)
	}
}

func BenchmarkLookupParam(b *testing.B) {
	tree := benchTree(b)
	var params Params
	b.ReportAllocs()
	for b.Loop() {
		params.Reset()
		tree.Lookup("/api/v1/users/12345/posts/67890", &params)
	}
}

func BenchmarkLookupWildcard(b *testing.B) {
	tree := benchTree(b)
	var params Params
	b.ReportAllocs()
	for b.Loop() {
		params.Reset()
		tree.Lookup("/static/css/site/theme/dark.css", &params)
	}
}

func BenchmarkLookupMiss(b *testing.B) {
	tree := benchTree(b)
	var params Params
	b.ReportAllocs()
	for b.Loop() {
		params.Reset()
		tree.Lookup("/api/v1/nothing/here/at/all", &params)
	}
}

// benchTree builds a tree of a size a real service might register, so that the
// benchmarks measure lookup against a populated structure rather than a toy.
func benchTree(b *testing.B) *Tree[string] {
	b.Helper()
	tree := New[string]()
	patterns := []string{
		"/api/v1/users", "/api/v1/users/me", "/api/v1/users/me/settings",
		"/api/v1/users/{id}", "/api/v1/users/{id}/posts", "/api/v1/users/{id}/posts/{post_id}",
		"/api/v1/items", "/api/v1/items/{id}", "/api/v1/items/{id}/reviews",
		"/api/v1/orders", "/api/v1/orders/{id}", "/api/v1/orders/{id}/lines",
		"/static/{path...}", "/health", "/metrics", "/openapi.json", "/docs",
	}
	for _, pattern := range patterns {
		if err := tree.Insert(pattern, pattern); err != nil {
			b.Fatalf("Insert(%q) = %v", pattern, err)
		}
	}
	return tree
}

func TestLookupRejectsUnrootedPath(t *testing.T) {
	t.Parallel()
	tr := New[int]()
	if err := tr.Insert("/admin/panel", 1); err != nil {
		t.Fatal(err)
	}
	wild := New[int]()
	if err := wild.Insert("/{rest...}", 2); err != nil {
		t.Fatal(err)
	}
	var p Params
	for _, path := range []string{"Xadmin/panel", "admin/panel", "*", ""} {
		if _, ok := tr.Lookup(path, &p); ok {
			t.Errorf("Lookup(%q) matched a tree of rooted routes", path)
		}
		p.Reset()
		if _, ok := wild.Lookup(path, &p); ok {
			t.Errorf("Lookup(%q) matched a root wildcard", path)
		}
		p.Reset()
	}
}

func TestInsertRejectsDuplicateParamNames(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"/a/{id}/b/{id}",
		"/a/{id}/b/{id...}",
		"/orgs/{id}/users/{id}/x",
	} {
		err := New[int]().Insert(pattern, 1)
		if !errors.Is(err, ErrInvalidPattern) {
			t.Errorf("Insert(%q) = %v, want ErrInvalidPattern", pattern, err)
		}
	}
	if err := New[int]().Insert("/a/{x}/b/{y}", 1); err != nil {
		t.Errorf("distinct names rejected: %v", err)
	}
}

// A pooled Params outlives the request it captured from, so what a Reset or a
// rewound branch leaves in its backing arrays is retained, up to a whole
// request path, until a later request writes over the same slot.
func TestParamsDoNotRetainCapturedPaths(t *testing.T) {
	t.Parallel()
	tree := New[string]()
	for _, pattern := range []string{"/a/{one}/deep/{two}", "/a/{rest...}"} {
		if err := tree.Insert(pattern, pattern); err != nil {
			t.Fatal(err)
		}
	}
	retained := func(p *Params) []string {
		var held []string
		for _, v := range p.values[:cap(p.values)] {
			if v != "" {
				held = append(held, v)
			}
		}
		return held
	}

	var params Params
	// The first branch captures {one} and then fails at "nomatch"; the
	// wildcard then answers. The rewind must not leave {one} behind.
	if _, ok := tree.Lookup("/a/secret-one/nomatch/secret-two", &params); !ok {
		t.Fatal("no match")
	}
	params.Reset()
	if held := retained(&params); len(held) != 0 {
		t.Errorf("Reset left %q in the backing array", held)
	}

	if _, ok := tree.Lookup("/a/secret-one/deep/secret-two", &params); !ok {
		t.Fatal("no match")
	}
	params.Reset()
	if held := retained(&params); len(held) != 0 {
		t.Errorf("Reset left %q in the backing array", held)
	}
}
