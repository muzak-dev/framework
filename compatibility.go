package muzak

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ChangeSeverity says what a difference between two OpenAPI documents does to
// a client written against the older one. It is what decides whether a change
// may ship: a test or a CI step refuses [Breaking] changes, reports
// [PossiblyBreaking] ones for a person to judge, and lets [Compatible] ones
// through.
type ChangeSeverity int

const (
	// Compatible is a change no client written against the old document can
	// notice going wrong: a new operation, a new optional request member, a
	// relaxed request bound, a new response member.
	Compatible ChangeSeverity = iota + 1
	// PossiblyBreaking is a change that breaks only some clients, depending on
	// how they were written: a response enum that gained a value breaks a client
	// that switches over it exhaustively, a removed deprecated operation breaks
	// one that ignored the deprecation, and a member a request object no longer
	// reads is ignored rather than refused. It is reported for a person to
	// judge rather than failed outright.
	PossiblyBreaking
	// Breaking is a change that makes a request a client was entitled to send
	// fail, or a response it was entitled to rely on differ: a removed
	// operation, a new required request member, a narrowed request type, a
	// response member that is gone or may now be null.
	Breaking
)

// String names the severity as a report prints it.
func (s ChangeSeverity) String() string {
	switch s {
	case Compatible:
		return "compatible"
	case PossiblyBreaking:
		return "possibly breaking"
	case Breaking:
		return "breaking"
	}
	return "ChangeSeverity(" + strconv.Itoa(int(s)) + ")"
}

// APIChange is one difference between two OpenAPI documents, judged from the
// side of a client written against the older one. [CompareDocuments] returns
// them.
type APIChange struct {
	// Severity says whether the change breaks such a client.
	Severity ChangeSeverity
	// Kind names the change for a machine, such as "operation-removed" or
	// "request-property-added". It never changes once released, so a tool can
	// filter on it. A change to a schema starts with "request-" or "response-",
	// the direction the schema was compared in, since the same change to a
	// schema is judged one way when clients send it and the other way when they
	// read it.
	Kind string
	// Location points at the change in the older document, in the syntax of a
	// JSON pointer (RFC 6901): "/paths/~1users~1{id}/get/responses/200". A
	// parameter is addressed by where it is read from and its name, as in
	// ".../parameters/query/limit", because its index in the list is not
	// stable. A change inside a named schema is located in components, where
	// it is reported once however many operations reach it. Something only the
	// newer document has is located where it would be in the older one, and a
	// change to the document as a whole has the empty location.
	Location string
	// Message says what changed, and what it does to a client, in one
	// sentence. It quotes names taken from the documents, but it is not
	// escaped for a terminal; [WriteChanges] is.
	Message string
}

// String renders the change on one line, with every character a terminal
// would interpret escaped, since the names in it come from a document.
func (c APIChange) String() string {
	return c.Severity.String() + ": " + c.line()
}

// line renders the change without its severity, which is what a report
// grouped by severity prints.
func (c APIChange) line() string {
	text := printableReport(c.Message) + " [" + printableReport(c.Kind) + "]"
	if c.Location == "" {
		return text
	}
	return printableReport(c.Location) + ": " + text
}

// printableReport escapes what a terminal would act on rather than print:
// control characters, which include the escape that starts an ANSI sequence,
// invalid UTF-8 and any other rune Unicode does not call printable. A name in
// a document someone else wrote must not be able to rewrite the report it
// appears in.
func printableReport(s string) string {
	clean := true
	for _, r := range s {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case !unicode.IsPrint(r):
			quoted := strconv.QuoteRuneToASCII(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// WriteChanges writes a plain-text report of changes, grouped by severity from
// the most to the least serious, one change per line, in the order given within
// each group. It is the report a command line tool or a failing test prints.
// Every name taken from a document is escaped for a terminal, so a hostile
// document cannot inject control sequences into the output.
//
//	Breaking (1):
//	  /paths/~1items/post: The operation POST /items is no longer served. [operation-removed]
//	Compatible (1):
//	  /paths/~1items~1{id}/get: The operation is now deprecated. [operation-deprecated]
//
// An empty list is reported as "No changes.".
func WriteChanges(w io.Writer, changes []APIChange) error {
	var b strings.Builder
	if len(changes) == 0 {
		b.WriteString("No changes.\n")
	}
	groups := []struct {
		heading string
		belongs func(ChangeSeverity) bool
	}{
		{"Breaking", func(s ChangeSeverity) bool { return s == Breaking }},
		{"Possibly breaking", func(s ChangeSeverity) bool { return s == PossiblyBreaking }},
		{"Compatible", func(s ChangeSeverity) bool { return s == Compatible }},
		// A list built by hand may hold a severity this package does not
		// define. It is still printed, since a report that dropped a change
		// would say less than it was given.
		{"Unclassified", func(s ChangeSeverity) bool { return s < Compatible || s > Breaking }},
	}
	for _, group := range groups {
		var lines []string
		for _, change := range changes {
			if group.belongs(change.Severity) {
				lines = append(lines, change.line())
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%s (%d):\n", group.heading, len(lines))
		for _, line := range lines {
			b.WriteString("  ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// CompareDocuments reports every difference between two OpenAPI documents
// that a client can observe, judged from the side of a client written against
// before: whether a request it was entitled to send can now fail, and whether a
// response it was entitled to rely on can now differ.
//
// A schema is judged by the direction it travels in. What a client sends may
// only widen: a new optional member, a relaxed bound or a longer enum is
// compatible, while a new required member, a narrower type, enum, bound,
// pattern or format, or null no longer being accepted, breaks it, and so does
// a removed member of an object that refuses members it does not know, as
// every object Muzak reads a request into does. What a client reads may only
// narrow: a removed member, a member that may now be absent or null, or a
// changed type breaks it, a new member does not, because a response is left
// open, and a response enum with a new value is possibly breaking, for a
// client that switches over it exhaustively. Operations, parameters, request
// bodies, statuses, media types and security requirements are judged the same
// way. Prose (summaries, descriptions, titles, contact details) is not
// compared, since no client can observe it.
//
// A reference into components is followed, so a component renamed without
// changing shape, as Item becomes ItemInput once a response also uses the type,
// is compatible. A named schema is compared once for each pair of components
// two positions of the documents refer to, and its changes are located in
// components, so a schema every operation shares, or one that refers to itself,
// costs as much as it is large, not as much as the paths through it: the
// comparison is linear in the size of the two documents whenever each
// component corresponds to a fixed few in the other, as it does between two
// versions of one application. It is also bounded outright, at a number of
// steps proportional to their size, names and values included, and at 100,000
// changes holding at most 64 MiB of text, which only a document built to be
// expensive reaches, such as one whose single component is compared against
// thousands of different ones; the result then holds a
// breaking "comparison-incomplete" change rather than passing in silence, and
// lists nothing it did not finish comparing.
//
// The result is sorted by severity, most serious first, then by location, so
// the same two documents always give the same list. A nil document is compared
// as one describing nothing. The documents are only read.
func CompareDocuments(before, after *Document) []APIChange {
	if before == after {
		// A document is the same as itself, and comparing it would only spend
		// steps finding that out, or, for a Document built in Go with a cycle
		// of pointers, run out of them first.
		return nil
	}
	c := newDocComparer(before, after)
	c.run()
	return c.result()
}

// apiDirection is the way a schema travels: a request schema is written by the
// client and read by the server, so it may only widen, and a response schema
// the other way round, so it may only narrow.
type apiDirection uint8

const (
	towardServer apiDirection = iota
	towardClient
)

// prefix starts the kind of a schema change, which is judged by direction.
func (d apiDirection) prefix() string {
	if d == towardServer {
		return "request-"
	}
	return "response-"
}

// where starts the message of a schema change.
func (d apiDirection) where() string {
	if d == towardServer {
		return "In requests, "
	}
	return "In responses, "
}

// pick returns the severity of a change in this direction.
func (d apiDirection) pick(request, response ChangeSeverity) ChangeSeverity {
	if d == towardServer {
		return request
	}
	return response
}

// The bounds on a comparison. Each is far above what any document Muzak
// generates reaches, and exists so that a document built to be expensive, which
// a stored baseline may be, costs a known amount instead of whatever it asks.
const (
	// compareBaseBudget and compareBudgetPerNode decide how many steps a
	// comparison may take: a fixed allowance plus a multiple of the number of
	// nodes in the two documents. Each pair of components is compared once,
	// so an honest comparison spends a small constant per node, which the
	// tests hold at no more than 8; the budget leaves four times that.
	compareBaseBudget    = 1 << 16
	compareBudgetPerNode = 32
	// compareBytesPerStep is how many bytes of text, such as a name, a
	// pattern or the JSON of an enum value, one step pays for reading. A
	// document Muzak generates holds little text per schema, so honest
	// comparisons hardly notice it; a schema holding megabytes is charged
	// for them each time it is read. See [docComparer.facts].
	compareBytesPerStep = 256
	// compareMaxDepth bounds how deeply schemas written inline are walked into
	// at one position. A document read by [ReadDocument] cannot nest deeper
	// than this; a Document built in Go with a cycle of pointers can.
	compareMaxDepth = 256
	// compareMaxChanges bounds how many changes one comparison lists. A report
	// is read by a person, and one past this length says no more than that the
	// two documents describe different APIs; the bound keeps a document built
	// to differ everywhere from costing memory in proportion to its steps.
	compareMaxChanges = 100_000
	// compareMaxChangeBytes bounds the text the changes of one comparison
	// hold, their locations, kinds and messages together. It is far above
	// what compareMaxChanges changes of a generated document take, and keeps
	// a document with long names from making each change as long as they are.
	compareMaxChangeBytes = 64 << 20
)

// schemaPair is one pair of components compared in one direction. textual is
// set for a parameter or a form value, which travels as text; see
// [schemaView.types].
type schemaPair struct {
	old, cur string
	dir      apiDirection
	textual  bool
}

// docComparer holds one comparison. old is the document a client was written
// against and cur the one it now meets.
type docComparer struct {
	old, cur               *Document
	oldSchemas, curSchemas map[string]*Schema
	oldSchemes, curSchemes map[string]SecurityScheme

	changes []APIChange
	// pairs holds every pair of components already queued, so each is
	// compared once per direction however many positions lead to it; queue
	// holds those not yet compared, in the order they were met.
	pairs map[schemaPair]bool
	queue []schemaPair
	// admitsNull keeps whether each schema met so far admits null; see
	// [docComparer.partAdmitsNull]. schemaFacts keeps what each schema met so
	// far says; see [docComparer.facts].
	admitsNull  map[*Schema]bool
	schemaFacts map[*Schema]*schemaFacts

	// budget is what is left of the steps the comparison may take, and depth
	// how deep the current walk into inline schemas is. changeBytes is the
	// text the changes listed so far hold.
	budget      int
	depth       int
	incomplete  bool
	changeBytes int
}

func newDocComparer(before, after *Document) *docComparer {
	if before == nil {
		before = &Document{}
	}
	if after == nil {
		after = &Document{}
	}
	c := &docComparer{old: before, cur: after, pairs: map[schemaPair]bool{}, admitsNull: map[*Schema]bool{},
		schemaFacts: map[*Schema]*schemaFacts{}}
	if before.Components != nil {
		c.oldSchemas, c.oldSchemes = before.Components.Schemas, before.Components.SecuritySchemes
	}
	if after.Components != nil {
		c.curSchemas, c.curSchemes = after.Components.Schemas, after.Components.SecuritySchemes
	}
	c.budget = compareBaseBudget + compareBudgetPerNode*(c.countNodes(before)+c.countNodes(after))
	return c
}

// spend takes n steps from the budget, reporting false once it is exhausted,
// at which point the comparison stops descending and the result says it is
// incomplete.
func (c *docComparer) spend(n int) bool {
	c.budget -= n
	if c.budget < 0 {
		c.incomplete = true
		return false
	}
	return true
}

// run compares the two documents, components last: a component is compared
// when some position leads to it, and in the direction that position needs.
func (c *docComparer) run() {
	c.compareServers()
	c.compareTags()
	c.compareSecuritySchemes()
	c.comparePaths()
	for len(c.queue) > 0 && !c.incomplete {
		pair := c.queue[0]
		c.queue = c.queue[1:]
		c.compareParts("/components/schemas/"+pointerToken(pair.old),
			oneSchema(c.oldSchemas[pair.old]), oneSchema(c.curSchemas[pair.cur]), pair.dir, pair.textual)
	}
}

// result sorts the changes, most serious first, and drops exact duplicates,
// which two positions leading to the same pair of components can produce.
func (c *docComparer) result() []APIChange {
	if c.incomplete {
		c.changes = append(c.changes, APIChange{
			Severity: Breaking,
			Kind:     "comparison-incomplete",
			Message: "The documents were not compared in full, because the comparison reached its bound on steps or " +
				"on the changes it lists, so changes may be missing from this list.",
		})
	}
	slices.SortFunc(c.changes, func(x, y APIChange) int {
		return cmp.Or(cmp.Compare(y.Severity, x.Severity), strings.Compare(x.Location, y.Location),
			strings.Compare(x.Kind, y.Kind), strings.Compare(x.Message, y.Message))
	})
	return slices.Compact(c.changes)
}

// add records one change, unless the comparison already lists as many as it
// may, or would hold more text than it may, in which case it stops.
func (c *docComparer) add(severity ChangeSeverity, kind, location, message string) {
	switch {
	case c.incomplete:
		// Once the comparison has stopped, what it is still finishing may rest
		// on a view it stopped gathering half way, so nothing more is listed:
		// a stopped comparison lists less than a full one, never something else.
		return
	case len(c.changes) >= compareMaxChanges, c.changeBytes+len(kind)+len(location)+len(message) > compareMaxChangeBytes:
		c.incomplete, c.budget = true, 0
		return
	}
	c.changeBytes += len(kind) + len(location) + len(message)
	c.changes = append(c.changes, APIChange{Severity: severity, Kind: kind, Location: location, Message: message})
}

// step takes the steps that comparing one position under location costs: one,
// and one for every compareBytesPerStep bytes of the location, which building
// the location of what it holds copies. A long name above many positions is
// so charged for each of them, rather than copied for free.
func (c *docComparer) step(location string) bool {
	return c.spend(1 + len(location)/compareBytesPerStep)
}

// pointerToken escapes one segment of a location as RFC 6901 does.
func pointerToken(s string) string {
	if !strings.ContainsAny(s, "~/") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// compareServers reports the base URLs that came and went. A client reading
// its base URL from the document loses a server that is no longer listed, which
// may or may not still answer.
func (c *docComparer) compareServers() {
	urls := func(servers []Server) []string {
		out := make([]string, 0, len(servers))
		for _, s := range servers {
			out = append(out, s.URL)
		}
		return out
	}
	removed, added := setDifference(urls(c.old.Servers), urls(c.cur.Servers))
	for _, url := range removed {
		c.add(PossiblyBreaking, "server-removed", "/servers", fmt.Sprintf("The server %q is no longer listed.", url))
	}
	for _, url := range added {
		c.add(Compatible, "server-added", "/servers", fmt.Sprintf("The server %q is new.", url))
	}
}

// compareTags reports the groups that came and went. A tag only groups
// operations in documentation, so neither breaks a client.
func (c *docComparer) compareTags() {
	names := func(tags []Tag) []string {
		out := make([]string, 0, len(tags))
		for _, t := range tags {
			out = append(out, t.Name)
		}
		return out
	}
	removed, added := setDifference(names(c.old.Tags), names(c.cur.Tags))
	for _, name := range removed {
		c.add(Compatible, "tag-removed", "/tags", fmt.Sprintf("The tag %q is no longer listed.", name))
	}
	for _, name := range added {
		c.add(Compatible, "tag-added", "/tags", fmt.Sprintf("The tag %q is new.", name))
	}
}

// setDifference returns, sorted and without repeats, what only the first list
// holds and what only the second does.
func setDifference(old, cur []string) (removed, added []string) {
	in := func(list []string) map[string]bool {
		set := make(map[string]bool, len(list))
		for _, s := range list {
			set[s] = true
		}
		return set
	}
	oldSet, curSet := in(old), in(cur)
	for s := range oldSet {
		if !curSet[s] {
			removed = append(removed, s)
		}
	}
	for s := range curSet {
		if !oldSet[s] {
			added = append(added, s)
		}
	}
	slices.Sort(removed)
	slices.Sort(added)
	return removed, added
}

// compareSecuritySchemes reports the schemes that changed how a client
// presents its credentials. A scheme that came or went changes nothing by
// itself: what a client has to present is decided by the requirements of each
// operation, which are compared there, by what the schemes they name ask for
// rather than by their names, so a renamed scheme is not a change.
func (c *docComparer) compareSecuritySchemes() {
	for _, name := range slices.Sorted(maps.Keys(c.oldSchemes)) {
		location := "/components/securitySchemes/" + pointerToken(name)
		scheme, kept := c.curSchemes[name]
		switch {
		case !kept:
			c.add(Compatible, "security-scheme-removed", location,
				fmt.Sprintf("The security scheme %q is no longer declared.", name))
		case schemeIdentity(c.oldSchemes[name]) != schemeIdentity(scheme):
			c.add(Breaking, "security-scheme-changed", location,
				fmt.Sprintf("The security scheme %q now asks for credentials differently, so a client configured for it no longer authenticates.", name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.curSchemes)) {
		if _, existed := c.oldSchemes[name]; !existed {
			c.add(Compatible, "security-scheme-added", "/components/securitySchemes/"+pointerToken(name),
				fmt.Sprintf("The security scheme %q is new.", name))
		}
	}
}

// schemeIdentity reduces a scheme to what decides how a client presents its
// credentials. Its description and bearer format are hints to a person, and
// the scopes a flow offers are compared through the requirements that name
// them, so none of them is part of it. A header name and an HTTP scheme are
// matched without regard to case, as HTTP matches them.
func schemeIdentity(s SecurityScheme) string {
	name := s.Name
	if s.In == "header" {
		name = strings.ToLower(name)
	}
	parts := []string{s.Type, strings.ToLower(s.Scheme), s.In, name, s.OpenIDConnectURL}
	if s.Flows != nil {
		for _, flow := range []*OAuthFlow{s.Flows.Implicit, s.Flows.Password, s.Flows.ClientCredentials, s.Flows.AuthorizationCode} {
			if flow == nil {
				parts = append(parts, "-")
				continue
			}
			parts = append(parts, flow.AuthorizationURL, flow.TokenURL, flow.RefreshURL)
		}
	}
	return strings.Join(parts, "\x00")
}

// countDocumentNodes counts the schemas, operations, parameters and responses
// a document holds, which is what the budget of a comparison is proportional
// to, each schema weighed by what reading it costs and each operation by its
// security requirements. A schema held by several
// positions, or one a Document built in Go reaches again through a cycle of
// pointers, is counted once; the walk keeps a stack of its own rather than
// recursing, so a document of any depth is counted in constant stack.
func countDocumentNodes(d *Document) int {
	return (&docComparer{schemaFacts: map[*Schema]*schemaFacts{}}).countNodes(d)
}

// countNodes is [countDocumentNodes], keeping what it learns of each schema
// for the comparison.
func (c *docComparer) countNodes(d *Document) int {
	seen := map[*Schema]bool{}
	var stack []*Schema
	push := func(s *Schema) {
		if s != nil && !seen[s] {
			seen[s] = true
			stack = append(stack, s)
		}
	}
	count := 0
	if d.Components != nil {
		for _, s := range d.Components.Schemas {
			push(s)
		}
	}
	for _, item := range d.Paths {
		for _, op := range pathOperations(item) {
			if op.op == nil {
				continue
			}
			count += requirementsCost(op.op.Security)
			for i := range op.op.Parameters {
				count++
				push(op.op.Parameters[i].Schema)
			}
			if op.op.RequestBody != nil {
				for _, media := range op.op.RequestBody.Content {
					push(media.Schema)
				}
			}
			for _, response := range op.op.Responses {
				count++
				if response != nil {
					for _, media := range response.Content {
						push(media.Schema)
					}
				}
			}
		}
	}
	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		count += c.facts(s).cost
		for _, property := range s.Properties {
			push(property)
		}
		push(s.Items)
		push(additionalSchema(s.AdditionalProperties))
		for _, alternative := range s.AnyOf {
			push(alternative)
		}
		for _, also := range s.AllOf {
			push(also)
		}
	}
	return count
}

// methodOperation pairs an operation with the method its slot is for.
type methodOperation struct {
	method string
	op     *Operation
}

// pathOperations lists the slots of a path item in a fixed order, so that a
// comparison walks them the same way every time.
func pathOperations(item *PathItem) []methodOperation {
	if item == nil {
		return nil
	}
	return []methodOperation{
		{"get", item.Get}, {"put", item.Put}, {"post", item.Post}, {"delete", item.Delete},
		{"patch", item.Patch}, {"head", item.Head}, {"options", item.Options},
	}
}

// liveOperations counts the operations a path item holds, and how many of
// them are deprecated.
func liveOperations(item *PathItem) (count, deprecated int) {
	for _, op := range pathOperations(item) {
		if op.op != nil {
			count++
			if op.op.Deprecated {
				deprecated++
			}
		}
	}
	return count, deprecated
}

// comparePaths pairs the paths of the two documents and compares each pair.
//
// A path is paired with the same path first. One left over is paired with a
// path that differs from it only in the names of its parameters, when exactly
// one such path is left over on each side: /users/{id} and /users/{userID}
// match the same requests, so renaming the parameter breaks no client.
func (c *docComparer) comparePaths() {
	oldPaths := livePaths(c.old.Paths)
	curPaths := livePaths(c.cur.Paths)
	partner := map[string]string{}
	for _, path := range oldPaths {
		if count, _ := liveOperations(c.cur.Paths[path]); count > 0 {
			partner[path] = path
		}
	}
	unmatched := func(paths []string, taken func(string) bool) map[string][]string {
		byShape := map[string][]string{}
		for _, path := range paths {
			if !taken(path) {
				byShape[templateShape(path)] = append(byShape[templateShape(path)], path)
			}
		}
		return byShape
	}
	matchedCur := map[string]bool{}
	for _, cur := range partner {
		matchedCur[cur] = true
	}
	oldLeft := unmatched(oldPaths, func(p string) bool { _, ok := partner[p]; return ok })
	curLeft := unmatched(curPaths, func(p string) bool { return matchedCur[p] })
	for shape, olds := range oldLeft {
		if curs := curLeft[shape]; len(olds) == 1 && len(curs) == 1 {
			partner[olds[0]] = curs[0]
			matchedCur[curs[0]] = true
		}
	}

	versions := indexByVersion(curPaths)
	for _, path := range oldPaths {
		if !c.spend(1) {
			return
		}
		location := "/paths/" + pointerToken(path)
		cur, kept := partner[path]
		if !kept {
			c.removedPath(location, path, versions)
			continue
		}
		renames := map[string]string{}
		if cur != path {
			c.add(Compatible, "path-renamed", location,
				fmt.Sprintf("The path %q is now written %q, which matches the same requests.", path, cur))
			renames = pathParameterRenames(path, cur)
		}
		c.comparePathItem(location, path, c.old.Paths[path], c.cur.Paths[cur], renames)
	}
	for _, path := range curPaths {
		if !matchedCur[path] {
			c.add(Compatible, "path-added", "/paths/"+pointerToken(path), fmt.Sprintf("The path %q is new.", path))
		}
	}
}

// livePaths lists, sorted, the paths of a document that hold an operation. A
// path item with none describes nothing a client can call.
func livePaths(paths map[string]*PathItem) []string {
	var out []string
	for path, item := range paths {
		if count, _ := liveOperations(item); count > 0 {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// removedPath reports a path that is no longer served. A path all of whose
// operations were deprecated is possibly breaking rather than breaking, since
// clients were told to move; and a path that another version of still serves,
// as /v1/cats is to /v2/cats, says where.
func (c *docComparer) removedPath(location, path string, versions map[string][]string) {
	count, deprecated := liveOperations(c.old.Paths[path])
	severity, message := Breaking, fmt.Sprintf("The path %q is no longer served.", path)
	if deprecated == count {
		severity, message = PossiblyBreaking, fmt.Sprintf("The path %q, which was deprecated, is no longer served.", path)
	}
	if key, versioned := versionShape(path); versioned {
		for _, sibling := range versions[key] {
			if sibling != path {
				message = strings.TrimSuffix(message, ".") + fmt.Sprintf("; another version of it is served at %q.", sibling)
				break
			}
		}
	}
	c.add(severity, "path-removed", location, message)
}

// templateShape is a path with the names of its parameters left out, which is
// what decides the requests it matches.
func templateShape(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if isPathParameter(segment) {
			segments[i] = "{}"
		}
	}
	return strings.Join(segments, "/")
}

// isPathParameter reports whether a segment of a path template is a parameter.
// "{$}", which anchors a template at a trailing slash, is not.
func isPathParameter(segment string) bool {
	return len(segment) > 2 && segment[0] == '{' && segment[len(segment)-1] == '}' && segment != "{$}"
}

// pathParameterRenames maps the parameter names of one path template to those
// at the same positions of another with the same shape.
func pathParameterRenames(old, cur string) map[string]string {
	renames := map[string]string{}
	oldSegments, curSegments := strings.Split(old, "/"), strings.Split(cur, "/")
	for i, segment := range oldSegments {
		if isPathParameter(segment) && i < len(curSegments) {
			renames[strings.Trim(segment, "{}")] = strings.Trim(curSegments[i], "{}")
		}
	}
	return renames
}

// versionShape is a path with its version segment left out, which is the
// first literal segment holding a digit, as v1 in /v1/cats does whatever
// prefix [VersioningOptions.Prefix] chose. It reports false for a path with
// none.
func versionShape(path string) (string, bool) {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if !isPathParameter(segment) && strings.ContainsAny(segment, "0123456789") {
			segments[i] = "{version}"
			return strings.Join(segments, "/"), true
		}
	}
	return "", false
}

// indexByVersion groups paths by their shape without a version, so that a
// removed path finds the other versions of itself in one lookup.
func indexByVersion(paths []string) map[string][]string {
	index := map[string][]string{}
	for _, path := range paths {
		if key, versioned := versionShape(path); versioned {
			index[key] = append(index[key], path)
		}
	}
	return index
}

// comparePathItem compares the operations of two paired paths.
func (c *docComparer) comparePathItem(location, path string, old, cur *PathItem, renames map[string]string) {
	curOps := pathOperations(cur)
	for i, op := range pathOperations(old) {
		next := curOps[i].op
		opLocation := location + "/" + op.method
		method := strings.ToUpper(op.method)
		switch {
		case op.op == nil && next != nil:
			c.add(Compatible, "operation-added", opLocation, fmt.Sprintf("The operation %s %s is new.", method, path))
		case op.op != nil && next == nil:
			if op.op.Deprecated {
				c.add(PossiblyBreaking, "operation-removed", opLocation,
					fmt.Sprintf("The operation %s %s, which was deprecated, is no longer served.", method, path))
			} else {
				c.add(Breaking, "operation-removed", opLocation, fmt.Sprintf("The operation %s %s is no longer served.", method, path))
			}
		case op.op != nil:
			c.compareOperation(opLocation, op.op, next, renames)
		}
	}
}

// compareOperation compares one operation present in both documents.
func (c *docComparer) compareOperation(location string, old, cur *Operation, renames map[string]string) {
	if !c.step(location) {
		return
	}
	if old.OperationID != cur.OperationID {
		c.add(PossiblyBreaking, "operation-id-changed", location,
			fmt.Sprintf("The operation identifier is now %q instead of %q, which renames the method a generated client calls.", cur.OperationID, old.OperationID))
	}
	switch {
	case !old.Deprecated && cur.Deprecated:
		c.add(Compatible, "operation-deprecated", location, "The operation is now deprecated.")
	case old.Deprecated && !cur.Deprecated:
		c.add(Compatible, "operation-undeprecated", location, "The operation is no longer deprecated.")
	}
	removed, added := setDifference(old.Tags, cur.Tags)
	for _, tag := range removed {
		c.add(Compatible, "operation-tag-removed", location+"/tags", fmt.Sprintf("The operation is no longer tagged %q.", tag))
	}
	for _, tag := range added {
		c.add(Compatible, "operation-tag-added", location+"/tags", fmt.Sprintf("The operation is now tagged %q.", tag))
	}
	c.compareSecurity(location+"/security", old.Security, cur.Security)
	c.compareParameters(location+"/parameters", old.Parameters, cur.Parameters, renames)
	c.compareRequestBody(location+"/requestBody", old.RequestBody, cur.RequestBody)
	c.compareResponses(location+"/responses", old.Responses, cur.Responses)
}

// securityAlternative is one requirement of an operation: the credentials a
// client presents together, keyed by what each scheme asks for rather than by
// its name, with the scopes each needs. names is how the document named them,
// for the message.
type securityAlternative struct {
	schemes map[string]map[string]bool
	names   string
}

// alternatives reads an operation's requirements. An operation that declares
// none, and one that declares an empty list, accepts a request with no
// credentials, which is the alternative that requires nothing.
func alternatives(requirements []SecurityRequirement, schemes map[string]SecurityScheme) []securityAlternative {
	if len(requirements) == 0 {
		return []securityAlternative{{schemes: map[string]map[string]bool{}}}
	}
	out := make([]securityAlternative, 0, len(requirements))
	for _, requirement := range requirements {
		alternative := securityAlternative{schemes: map[string]map[string]bool{}}
		var names []string
		for _, name := range slices.Sorted(maps.Keys(requirement)) {
			key := "name\x00" + name
			if scheme, declared := schemes[name]; declared {
				key = schemeIdentity(scheme)
			}
			scopes := alternative.schemes[key]
			if scopes == nil {
				scopes = map[string]bool{}
				alternative.schemes[key] = scopes
			}
			for _, scope := range requirement[name] {
				scopes[scope] = true
			}
			label := strconv.Quote(name)
			if len(requirement[name]) > 0 {
				label += " with " + quotedList(requirement[name])
			}
			names = append(names, label)
		}
		alternative.names = strings.Join(names, " and ")
		out = append(out, alternative)
	}
	return out
}

// requirementsCost is what checking one client against every requirement of
// an operation costs: a step for each requirement, each scheme it names and
// each scope it needs. An operation that lists none is checked against the one
// requirement of no credentials.
func requirementsCost(requirements []SecurityRequirement) int {
	cost := 1
	for _, requirement := range requirements {
		cost++
		for _, scopes := range requirement {
			cost += 1 + len(scopes)
		}
	}
	return cost
}

// describe names what a client satisfying an alternative presents.
func (a securityAlternative) describe() string {
	if a.names == "" {
		return "a request with no credentials"
	}
	return "credentials for " + a.names
}

// satisfies reports whether a client holding the credentials of have is
// accepted by the requirement need: every scheme need names is among them,
// with every scope it needs.
func satisfies(have, need securityAlternative) bool {
	for scheme, scopes := range need.schemes {
		held, ok := have.schemes[scheme]
		if !ok {
			return false
		}
		for scope := range scopes {
			if !held[scope] {
				return false
			}
		}
	}
	return true
}

// compareSecurity compares the credentials an operation accepts. A client
// that holds exactly what one old alternative asks for must still be accepted
// by some new one, or the requirements were tightened; one that holds what a
// new alternative asks for and was refused before shows that they were
// loosened.
func (c *docComparer) compareSecurity(location string, old, cur []SecurityRequirement) {
	before := alternatives(old, c.oldSchemes)
	after := alternatives(cur, c.curSchemes)
	// Each alternative of one side is checked against every alternative of
	// the other, scheme by scheme and scope by scope, which is what is
	// charged: a list of many requirements of many scopes pays for all of it.
	if !c.spend(len(before)*requirementsCost(cur) + len(after)*requirementsCost(old)) {
		return
	}
	accepted := func(have securityAlternative, by []securityAlternative) bool {
		return slices.ContainsFunc(by, func(need securityAlternative) bool { return satisfies(have, need) })
	}
	for _, have := range before {
		if !accepted(have, after) {
			c.add(Breaking, "security-tightened", location,
				fmt.Sprintf("The operation no longer accepts %s.", have.describe()))
			break
		}
	}
	for _, have := range after {
		if !accepted(have, before) {
			c.add(Compatible, "security-loosened", location,
				fmt.Sprintf("The operation now also accepts %s.", have.describe()))
			break
		}
	}
}

// parameterKey identifies a parameter by where it is read from and its name,
// which for a header is matched without regard to case, as HTTP matches it.
type parameterKey struct{ in, name string }

func keyOf(p *Parameter, renames map[string]string) parameterKey {
	name := p.Name
	switch p.In {
	case "header":
		name = strings.ToLower(name)
	case "path":
		if renamed, ok := renames[name]; ok {
			name = renamed
		}
	}
	return parameterKey{p.In, name}
}

// parametersByKey indexes a list of parameters, keeping the first of any two
// that share a key and returning the keys in a fixed order.
func parametersByKey(list []Parameter, renames map[string]string) (map[parameterKey]*Parameter, []parameterKey) {
	index := map[parameterKey]*Parameter{}
	var keys []parameterKey
	for i := range list {
		key := keyOf(&list[i], renames)
		if _, seen := index[key]; !seen {
			index[key] = &list[i]
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(x, y parameterKey) int {
		return cmp.Or(strings.Compare(x.in, y.in), strings.Compare(x.name, y.name))
	})
	return index, keys
}

// compareParameters compares the parameters of one operation. A parameter
// that disappeared from one location while one of the same name appeared in
// another moved, which is reported as such rather than as two changes.
func (c *docComparer) compareParameters(location string, old, cur []Parameter, renames map[string]string) {
	before, beforeKeys := parametersByKey(old, renames)
	after, afterKeys := parametersByKey(cur, nil)
	matched := map[parameterKey]bool{}
	// The parameters only the newer operation has, by name, are where one
	// that moved is looked for, in a fixed order.
	arrived := map[string][]parameterKey{}
	for _, key := range afterKeys {
		if before[key] == nil {
			name := strings.ToLower(key.name)
			arrived[name] = append(arrived[name], key)
		}
	}
	for _, key := range beforeKeys {
		if !c.step(location) {
			return
		}
		p := before[key]
		at := location + "/" + pointerToken(p.In) + "/" + pointerToken(p.Name)
		if next, kept := after[key]; kept {
			matched[key] = true
			c.compareParameter(at, p, next)
			continue
		}
		if candidates := arrived[strings.ToLower(key.name)]; len(candidates) > 0 {
			moved := candidates[0]
			arrived[strings.ToLower(key.name)] = candidates[1:]
			matched[moved] = true
			c.add(Breaking, "parameter-location-changed", at,
				fmt.Sprintf("The parameter %q is now read from the %s instead of the %s.", p.Name, after[moved].In, p.In))
			continue
		}
		c.add(Breaking, "parameter-removed", at,
			fmt.Sprintf("The %s parameter %q is no longer read, so a client that sends it is ignored.", p.In, p.Name))
	}
	for _, key := range afterKeys {
		if matched[key] {
			continue
		}
		if !c.step(location) {
			return
		}
		p := after[key]
		at := location + "/" + pointerToken(p.In) + "/" + pointerToken(p.Name)
		if p.Required {
			c.add(Breaking, "parameter-added", at,
				fmt.Sprintf("The %s parameter %q is new and required, which a client written against the old document does not send.", p.In, p.Name))
		} else {
			c.add(Compatible, "parameter-added", at, fmt.Sprintf("The %s parameter %q is new and optional.", p.In, p.Name))
		}
	}
}

// compareParameter compares one parameter present in both documents. Its
// value travels as text, which is what the schema is compared as.
func (c *docComparer) compareParameter(location string, old, cur *Parameter) {
	switch {
	case !old.Required && cur.Required:
		c.add(Breaking, "parameter-became-required", location, fmt.Sprintf("The %s parameter %q is now required.", old.In, old.Name))
	case old.Required && !cur.Required:
		c.add(Compatible, "parameter-became-optional", location, fmt.Sprintf("The %s parameter %q is now optional.", old.In, old.Name))
	}
	c.compareParts(location+"/schema", oneSchema(old.Schema), oneSchema(cur.Schema), towardServer, true)
}

// isFormMedia reports whether a body of this media type carries its members as
// text, as a form does.
func isFormMedia(media string) bool {
	media = strings.ToLower(media)
	return media == "multipart/form-data" || media == "application/x-www-form-urlencoded"
}

// compareRequestBody compares the bodies an operation reads.
func (c *docComparer) compareRequestBody(location string, old, cur *RequestBody) {
	switch {
	case old == nil && cur == nil:
		return
	case old == nil:
		if cur.Required {
			c.add(Breaking, "request-body-added", location,
				"The operation now requires a request body, which a client written against the old document does not send.")
		} else {
			c.add(Compatible, "request-body-added", location, "The operation now accepts an optional request body.")
		}
		return
	case cur == nil:
		c.add(Breaking, "request-body-removed", location, "The operation no longer reads a request body, so what a client sends in one is ignored.")
		return
	}
	switch {
	case !old.Required && cur.Required:
		c.add(Breaking, "request-body-became-required", location, "The request body is now required.")
	case old.Required && !cur.Required:
		c.add(Compatible, "request-body-became-optional", location, "The request body is now optional.")
	}
	for _, media := range slices.Sorted(maps.Keys(old.Content)) {
		if !c.step(location) {
			return
		}
		at := location + "/content/" + pointerToken(media)
		next, kept := cur.Content[media]
		if !kept {
			c.add(Breaking, "request-media-type-removed", at, fmt.Sprintf("A request body of type %q is no longer accepted.", media))
			continue
		}
		c.compareParts(at+"/schema", oneSchema(old.Content[media].Schema), oneSchema(next.Schema), towardServer, isFormMedia(media))
	}
	for _, media := range slices.Sorted(maps.Keys(cur.Content)) {
		if _, existed := old.Content[media]; !existed && c.step(location) {
			c.add(Compatible, "request-media-type-added", location+"/content/"+pointerToken(media),
				fmt.Sprintf("A request body of type %q is now accepted.", media))
		}
	}
}

// isSuccessStatus reports whether a status key is one a client acts on as an
// outcome rather than handles as a failure: an informational, successful or
// redirecting status, or a range of them. "default" and the failures are
// handled by any client that handles errors at all.
func isSuccessStatus(status string) bool {
	return status != "" && status[0] >= '1' && status[0] <= '3'
}

// compareResponses compares the outcomes an operation documents.
func (c *docComparer) compareResponses(location string, old, cur map[string]*Response) {
	for _, status := range slices.Sorted(maps.Keys(old)) {
		if !c.step(location) {
			return
		}
		at := location + "/" + pointerToken(status)
		next, kept := cur[status]
		switch {
		case !kept && isSuccessStatus(status):
			c.add(Breaking, "response-status-removed", at,
				fmt.Sprintf("The operation no longer answers %s, which a client could expect.", status))
		case !kept:
			c.add(Compatible, "response-status-removed", at, fmt.Sprintf("The operation no longer documents the %s response.", status))
		default:
			c.compareResponse(at, status, old[status], next)
		}
	}
	for _, status := range slices.Sorted(maps.Keys(cur)) {
		if _, existed := old[status]; existed {
			continue
		}
		if !c.step(location) {
			return
		}
		at := location + "/" + pointerToken(status)
		if isSuccessStatus(status) {
			c.add(PossiblyBreaking, "response-status-added", at,
				fmt.Sprintf("The operation may now answer %s, which a client written against the old document does not expect.", status))
		} else {
			c.add(Compatible, "response-status-added", at, fmt.Sprintf("The operation now documents a %s response.", status))
		}
	}
}

// compareResponse compares one outcome present in both documents.
func (c *docComparer) compareResponse(location, status string, old, cur *Response) {
	if !c.step(location) {
		return
	}
	var oldContent, curContent map[string]MediaType
	if old != nil {
		oldContent = old.Content
	}
	if cur != nil {
		curContent = cur.Content
	}
	for _, media := range slices.Sorted(maps.Keys(oldContent)) {
		if !c.step(location) {
			return
		}
		at := location + "/content/" + pointerToken(media)
		next, kept := curContent[media]
		if !kept {
			c.add(Breaking, "response-media-type-removed", at,
				fmt.Sprintf("The %s response no longer carries a body of type %q.", status, media))
			continue
		}
		c.compareParts(at+"/schema", oneSchema(oldContent[media].Schema), oneSchema(next.Schema), towardClient, false)
	}
	for _, media := range slices.Sorted(maps.Keys(curContent)) {
		if _, existed := oldContent[media]; !existed && c.step(location) {
			c.add(Compatible, "response-media-type-added", location+"/content/"+pointerToken(media),
				fmt.Sprintf("The %s response may now carry a body of type %q.", status, media))
		}
	}
}

// quotedList renders names for a message, quoted; see [shortList].
func quotedList(values []string) string {
	return shortList(values, true)
}
