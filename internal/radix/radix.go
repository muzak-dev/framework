package radix

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors returned by [Tree.Insert]. They are wrapped with context
// describing the offending pattern, so callers should test for them with
// [errors.Is] rather than by string comparison.
var (
	// ErrInvalidPattern reports a pattern that is not a legal route template:
	// one that does not begin with '/', contains a malformed parameter, or
	// places a wildcard anywhere but the final segment.
	ErrInvalidPattern = errors.New("radix: invalid pattern")

	// ErrDuplicateRoute reports a pattern that was registered twice on the
	// same tree.
	ErrDuplicateRoute = errors.New("radix: duplicate route")

	// ErrParamConflict reports two patterns that place differently named
	// parameters at the same position, such as "/a/{x}" and "/a/{y}". Allowing
	// both would make the name a captured parameter resolves to depend on
	// registration order, so the tree rejects the second one.
	ErrParamConflict = errors.New("radix: conflicting parameter name")
)

// Params collects the parameter values captured during a [Tree.Lookup].
//
// The zero Params is ready to use. Its backing arrays are retained across
// [Params.Reset] calls, which is what lets a pooled Params serve many requests
// without allocating. Values are sub-slices of the path string passed to
// Lookup and stay valid only as long as that string does.
type Params struct {
	names  []string
	values []string
}

// Len reports how many parameters were captured.
func (p *Params) Len() int { return len(p.names) }

// At returns the name and value of the parameter at index i. It panics if i is
// out of range, matching the behaviour of a slice index.
func (p *Params) At(i int) (name, value string) {
	return p.names[i], p.values[i]
}

// Get returns the value captured for the named parameter and reports whether
// that parameter was present. When a name was captured more than once (which
// the tree prevents at insertion time), the first value wins.
func (p *Params) Get(name string) (string, bool) {
	for i, n := range p.names {
		if n == name {
			return p.values[i], true
		}
	}
	return "", false
}

// SetValue replaces the value captured at index i, which the router uses to
// store the percent-decoded form of a parameter after matching against the
// escaped path. It panics if i is out of range.
func (p *Params) SetValue(i int, value string) {
	p.values[i] = value
}

// Reset discards every captured parameter while keeping the allocated backing
// arrays for reuse. Call it before handing a Params to Lookup again.
func (p *Params) Reset() {
	p.names = p.names[:0]
	p.values = p.values[:0]
}

// push records a captured parameter.
func (p *Params) push(name, value string) {
	p.names = append(p.names, name)
	p.values = append(p.values, value)
}

// truncate rewinds the capture list to length n, undoing the pushes performed
// by a branch that turned out not to match.
func (p *Params) truncate(n int) {
	p.names = p.names[:n]
	p.values = p.values[:n]
}

// node is a single segment position in the trie. Exactly one of the three
// child kinds may be taken for a given segment, but a node may own all three
// simultaneously, and that is what makes backtracking necessary.
type node[T any] struct {
	static       map[string]*node[T]
	param        *node[T]
	paramName    string
	wildcard     *node[T]
	wildcardName string
	value        T
	hasValue     bool
}

// Tree is a segment-wise radix trie mapping route patterns to values of type
// T. See the package documentation for the pattern syntax, precedence rules
// and complexity bounds.
//
// The zero Tree is not usable; obtain one from [New].
type Tree[T any] struct {
	root *node[T]
	size int
}

// New returns an empty Tree ready for insertion.
func New[T any]() *Tree[T] {
	return &Tree[T]{root: &node[T]{}}
}

// Len reports how many patterns have been inserted.
func (t *Tree[T]) Len() int { return t.size }

// splitSeg splits a path that begins with '/' into its first segment and the
// remainder. The remainder either begins with '/' or is empty, so that ""
// unambiguously means "no segments left" while "/" still denotes one final
// empty segment. That distinction is what keeps "/items" and "/items/" apart.
func splitSeg(path string) (seg, rest string) {
	path = path[1:]
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i], path[i:]
	}
	return path, ""
}

// segKind classifies one pattern segment and extracts the parameter name it
// declares, if any.
type segKind int

const (
	segStatic segKind = iota
	segParam
	segWildcard
)

// classify determines whether seg is static text, a "{name}" parameter or a
// "{name...}" wildcard, returning the declared name for the latter two. It
// reports an error for anything that uses brace characters without forming a
// well-shaped parameter, so that a typo like "{id" fails loudly at startup
// instead of silently becoming a static segment that can never match.
func classify(seg string) (segKind, string, error) {
	openIdx := strings.IndexByte(seg, '{')
	closeIdx := strings.IndexByte(seg, '}')
	if openIdx < 0 && closeIdx < 0 {
		return segStatic, "", nil
	}
	if openIdx != 0 || closeIdx != len(seg)-1 {
		return 0, "", fmt.Errorf("%w: segment %q must be either static text or a whole %q parameter", ErrInvalidPattern, seg, "{name}")
	}
	name := seg[1 : len(seg)-1]
	kind := segParam
	if strings.HasSuffix(name, "...") {
		kind = segWildcard
		name = name[:len(name)-3]
	}
	if name == "" {
		return 0, "", fmt.Errorf("%w: segment %q declares an empty parameter name", ErrInvalidPattern, seg)
	}
	if strings.ContainsAny(name, "{}") {
		return 0, "", fmt.Errorf("%w: parameter name %q contains a brace", ErrInvalidPattern, name)
	}
	return kind, name, nil
}

// Insert associates value with pattern.
//
// The pattern must begin with '/' and may use "{name}" to match a single
// segment or a trailing "{name...}" to match everything that remains. Insert
// returns an error wrapping [ErrInvalidPattern] for a malformed template,
// including a static segment that is not validly percent-encoded or that
// encodes a '/', neither of which a request could ever match,
// [ErrDuplicateRoute] if the same pattern was already inserted, and
// [ErrParamConflict] if the pattern would place a differently named parameter
// where an existing route already declares one.
//
// Insert must not run concurrently with itself or with [Tree.Lookup].
func (t *Tree[T]) Insert(pattern string, value T) error {
	if !strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("%w: %q must begin with %q", ErrInvalidPattern, pattern, "/")
	}
	cur := t.root
	for path := pattern; path != ""; {
		seg, rest := splitSeg(path)
		path = rest
		kind, name, err := classify(seg)
		if err != nil {
			return fmt.Errorf("%w (in pattern %q)", err, pattern)
		}
		switch kind {
		case segStatic:
			seg, err = staticKey(seg)
			if err != nil {
				return fmt.Errorf("%w (in pattern %q)", err, pattern)
			}
			if cur.static == nil {
				cur.static = make(map[string]*node[T])
			}
			next, ok := cur.static[seg]
			if !ok {
				next = &node[T]{}
				cur.static[seg] = next
			}
			cur = next
		case segParam:
			if cur.param == nil {
				cur.param = &node[T]{}
				cur.paramName = name
			} else if cur.paramName != name {
				return fmt.Errorf("%w: %q declares %q where %q is already registered", ErrParamConflict, pattern, name, cur.paramName)
			}
			cur = cur.param
		default: // segWildcard
			if rest != "" {
				return fmt.Errorf("%w: wildcard %q in %q must be the final segment", ErrInvalidPattern, seg, pattern)
			}
			if cur.wildcard == nil {
				cur.wildcard = &node[T]{}
				cur.wildcardName = name
			} else if cur.wildcardName != name {
				return fmt.Errorf("%w: %q declares %q where %q is already registered", ErrParamConflict, pattern, name, cur.wildcardName)
			}
			cur = cur.wildcard
		}
	}
	if cur.hasValue {
		return fmt.Errorf("%w: %q", ErrDuplicateRoute, pattern)
	}
	cur.value = value
	cur.hasValue = true
	t.size++
	return nil
}

// Lookup finds the value registered for the given request path, appending any
// captured parameters to params.
//
// The path is the escaped request path, and is matched exactly, including its
// trailing slash; a static segment is compared once percent-decoded, as the
// package documentation describes. Static segments take precedence over a
// parameter, which takes precedence over a wildcard, and Lookup backtracks
// into the lower-precedence branches when a more specific one leads to a dead
// end. Callers should [Params.Reset] before reusing a Params value; on a
// failed lookup the captures are rewound automatically, so params is left as
// it was found.
//
// Lookup performs no allocation for any segment that decodes to at most
// [maxStackSegment] bytes, and is safe for concurrent use provided no
// insertion is in flight.
func (t *Tree[T]) Lookup(path string, params *Params) (T, bool) {
	// splitSeg drops the first byte on the assumption that it is the '/'
	// every escaped request path begins with. A path that does not (what
	// http.StripPrefix leaves behind for "/apiXadmin", or the "*" of an
	// "OPTIONS *" request) would otherwise lose a byte and be routed as if it
	// had been rooted, so it matches nothing instead.
	if path == "" || path[0] != '/' {
		var zero T
		return zero, false
	}
	// One scan of the whole path decides whether any segment needs decoding,
	// which is cheaper than asking again of every segment on the way down.
	return t.root.lookup(path, params, strings.IndexByte(path, '%') >= 0)
}

// lookup implements the backtracking descent described in the package docs.
// encoded reports whether the path carries a '%' anywhere, and so whether a
// static segment may have to be decoded before it is compared.
func (n *node[T]) lookup(path string, params *Params, encoded bool) (zero T, ok bool) {
	if path == "" {
		if n.hasValue {
			return n.value, true
		}
		return zero, false
	}
	seg, rest := splitSeg(path)
	// The plain map lookup is written out here rather than behind a call,
	// because it is the whole cost of a request that carries no escapes.
	var child *node[T]
	if encoded {
		child = n.encodedStaticChild(seg)
	} else {
		child = n.static[seg]
	}
	if child != nil {
		if v, found := child.lookup(rest, params, encoded); found {
			return v, true
		}
	}
	// A parameter never matches an empty segment, which is what keeps
	// "/users//posts" and a trailing slash from being captured as a value.
	if n.param != nil && seg != "" {
		mark := params.Len()
		params.push(n.paramName, seg)
		if v, found := n.param.lookup(rest, params, encoded); found {
			return v, true
		}
		params.truncate(mark)
	}
	if n.wildcard != nil {
		// The wildcard swallows everything left, minus the leading separator.
		params.push(n.wildcardName, path[1:])
		return n.wildcard.value, true
	}
	return zero, false
}

// encodedStaticChild returns the static child a request segment names, for a
// path carrying at least one '%', comparing the segment in its percent-decoded
// form.
//
// The router matches against the escaped path, so that "%2F" stays inside one
// segment instead of splitting it in two. Comparing that escaped text with a
// static segment byte for byte is what used to let "/users/%61dmin" miss a
// guarded "/users/admin" and land on a public "/users/{id}" with id "admin":
// the parameter branch decodes what it captures, the static branch did not,
// so the two disagreed about which segment a request named. Decoding here
// makes them agree, which is also what net/http.ServeMux does.
//
// A segment whose decoded form contains '/' never matches a static child,
// because a static segment cannot contain one and an encoded separator is
// data, not structure. A segment that is not validly encoded never matches
// one either: net/http refuses such a request before a handler runs, so only a
// caller driving the router directly can produce one, and the parameter branch
// reports it as a bad request when it decodes the capture. Decoding happens
// only for a segment that carries a '%', into a buffer on the stack, so a
// path with no escapes at all costs one byte scan for the whole lookup and the
// lookup stays allocation-free for any segment that fits the buffer.
func (n *node[T]) encodedStaticChild(seg string) *node[T] {
	if n.static == nil || strings.IndexByte(seg, '%') < 0 {
		return n.static[seg]
	}
	var buf [maxStackSegment]byte
	decoded, ok := unescapeSegment(buf[:0], seg)
	if !ok {
		return nil
	}
	// Indexing a map with a converted byte slice does not copy it.
	return n.static[string(decoded)]
}

// maxStackSegment is how long an encoded segment can decode to before the
// decoded copy has to live on the heap. Every realistic static segment is far
// shorter; a longer one still matches, it only allocates to do so.
const maxStackSegment = 128

// staticKey returns the form a static pattern segment is stored under, which
// is its percent-decoded text, so that a pattern and a request are compared
// the same way. See [node.encodedStaticChild].
//
// A pattern that encodes a '/' is refused rather than stored, because the
// segment it describes is one no request can ever match, and a pattern that
// is not validly encoded is refused because no request can ever carry it.
func staticKey(seg string) (string, error) {
	if strings.IndexByte(seg, '%') < 0 {
		return seg, nil
	}
	decoded, ok := unescapeSegment(nil, seg)
	if !ok {
		return "", fmt.Errorf("%w: segment %q is not a validly percent-encoded path segment, or encodes a %q; "+
			"write %q for a literal percent sign, and use a parameter for a value that may contain a separator",
			ErrInvalidPattern, seg, "/", "%25")
	}
	return string(decoded), nil
}

// unescapeSegment appends the percent-decoded form of seg to dst. It reports
// false when seg holds a malformed escape or decodes to text containing '/',
// the two cases in which it cannot be compared with a static segment.
func unescapeSegment(dst []byte, seg string) ([]byte, bool) {
	for i := 0; i < len(seg); i++ {
		b := seg[i]
		if b == '%' {
			if i+2 >= len(seg) {
				return nil, false
			}
			hi, okHi := unhex(seg[i+1])
			lo, okLo := unhex(seg[i+2])
			if !okHi || !okLo {
				return nil, false
			}
			b = hi<<4 | lo
			i += 2
		}
		if b == '/' {
			return nil, false
		}
		dst = append(dst, b)
	}
	return dst, true
}

// unhex decodes one hexadecimal digit, in either case.
func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
