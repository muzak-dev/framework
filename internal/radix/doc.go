// Package radix implements the path-matching tree that backs Muzak's router.
//
// The tree is a segment-wise radix trie: each edge consumes one complete path
// segment (the text between two '/' separators) rather than a single byte.
// Segment granularity is what makes matching allocation-free: a matched
// parameter is a sub-slice of the request path, never a freshly built string.
//
// # Supported patterns
//
// A pattern is a '/'-separated sequence of three kinds of segment:
//
//	/users/list        static     matches itself, once percent-decoded
//	/users/{id}        param      matches exactly one non-empty segment
//	/files/{rest...}   wildcard   matches every remaining segment, greedily
//
// Lookup is given the escaped request path, so that an encoded "%2F" stays
// inside the segment it belongs to. A static segment is nevertheless compared
// in its decoded form, on both sides: "/users/%61dmin" matches the pattern
// "/users/admin", exactly as it would under net/http.ServeMux, rather than
// falling through to a sibling parameter that would capture "admin". A
// segment that decodes to text containing '/', or that is not validly
// encoded, never matches a static segment, and a pattern whose static segment
// is either of those is refused. Parameters and wildcards capture the escaped
// text as it is, and decoding it is left to the caller.
//
// A wildcard is only legal as the final segment of a pattern. Patterns are
// matched exactly: "/items" and "/items/" are distinct routes, because the
// trailing slash produces a final empty segment that a parameter refuses to
// match.
//
// # Precedence
//
// At every node the tree tries static children first, then the parameter
// child, then the wildcard child. Because a deeper mismatch can occur after a
// more specific edge has been taken, [Tree.Lookup] backtracks: if the static
// branch fails to produce a terminal value, matching resumes at the parameter
// branch, and then at the wildcard branch. This yields the intuitive result
// that "/users/me" beats "/users/{id}" no matter which was registered first,
// while "/users/{id}/posts" still matches when "/users/me" exists but
// "/users/me/posts" does not.
//
// # Complexity
//
// Let S be the number of segments in the request path and L the length of the
// longest segment. A single downward walk costs O(S) map lookups, each hashing
// at most L bytes, for O(S*L) work, independent of the number of registered
// routes. Backtracking is bounded by the number of nodes along the path that
// own both a static and a dynamic child, so the worst case is O(S^2 * L) for a
// pathological route set (every level ambiguous) and O(S*L) for realistic ones.
// Matching never allocates: captured parameters are appended to a caller-owned
// [Params] whose backing arrays are reused across requests, and a segment is
// decoded into a buffer on the stack, and only when it carries a '%'.
//
// # Usage
//
//	tree := radix.New[string]()
//	tree.Insert("/users/{id}", "show-user")
//
//	var p radix.Params
//	if v, ok := tree.Lookup("/users/42", &p); ok {
//		id, _ := p.Get("id") // "42"
//		_ = v                // "show-user"
//	}
//
// A Tree is safe for concurrent [Tree.Lookup] once every [Tree.Insert] has
// returned; it does not support insertion concurrent with lookup, because
// Muzak registers all routes at startup and only reads them thereafter.
package radix
