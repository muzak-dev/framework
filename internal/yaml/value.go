package yaml

import "strings"

// value parses whatever follows a key or a dash on the same line.
//
// The empty string means the value is the block below, which is the ordinary
// case in a locale file; everything else is decided by the character the value
// opens with.
func (p *parser) value(rest string, l line, indent, depth int) (any, error) {
	col := l.indent + (len(stripComment(l.text)) - len(rest)) + 1

	if rest == "" {
		p.pos++
		return p.owned(indent, depth)
	}

	switch rest[0] {
	case '|', '>':
		return p.blockScalar(rest, indent)
	case '&':
		return p.anchored(rest, l, indent, depth)
	case '*':
		return p.alias(rest, l, col)
	case '!':
		return nil, errAt(p.file, l, col, "this parser does not read explicit tags")
	case '[', '{':
		p.pos++
		return p.flowValue(rest, l, col, depth)
	case '\'', '"':
		p.pos++
		text, used, err := p.quoted(rest, l, col)
		if err != nil {
			return nil, err
		}
		if trailing := strings.TrimSpace(rest[used:]); trailing != "" {
			return nil, errAt(p.file, l, col+used, "this text follows the end of a quoted value")
		}
		return text, nil
	default:
		return p.plainValue(rest, indent), nil
	}
}

// owned parses the node a key with no value of its own owns.
//
// It is [parser.block] with one exception: a block sequence may be written
// flush with its key rather than indented under it, which is how most locale
// files write a list of day names. The two layouts cannot be confused, because
// a mapping key can never begin with a dash followed by a space.
func (p *parser) owned(indent, depth int) (any, error) {
	p.skipBlank()
	if p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent == indent && !isEnd(l.text) && isSequenceEntry(stripComment(l.text)) {
			return p.sequence(indent, depth+1)
		}
	}
	return p.block(indent, depth+1)
}

// plainValue reads an unquoted value, including one continued on the lines
// below it.
//
// A continuation is any following line indented further that is not itself a
// mapping entry or a sequence entry, which is how a long sentence is wrapped in
// a locale file. A value spread over lines is always text: it is prose by
// construction, so resolving it as a number or a boolean would be wrong.
func (p *parser) plainValue(rest string, indent int) any {
	p.pos++
	parts := []string{rest}
	for p.pos < len(p.lines) {
		n := p.lines[p.pos]
		if n.blank || n.indent <= indent || n.tabColumn > 0 || isEnd(n.text) {
			break
		}
		text := stripComment(n.text)
		if text == "" || isSequenceEntry(text) {
			break
		}
		if _, _, _, isEntry := splitKey(text); isEntry {
			break
		}
		parts = append(parts, text)
		p.pos++
	}
	if len(parts) == 1 {
		return resolvePlain(rest)
	}
	return strings.Join(parts, " ")
}

// anchored reads a value that names itself, so that a later alias can refer to
// it.
//
// The anchor is recorded only once its value is complete, which is what makes a
// recursive alias impossible rather than merely discouraged: an alias inside the
// value it names finds nothing recorded yet and is refused as unknown.
func (p *parser) anchored(rest string, l line, indent, depth int) (any, error) {
	col := l.indent + (len(stripComment(l.text)) - len(rest)) + 1
	name, remainder, _ := strings.Cut(rest[1:], " ")
	if name == "" {
		return nil, errAt(p.file, l, col, "an anchor is written without a name")
	}
	value, err := p.value(strings.TrimLeft(remainder, " "), l, indent, depth)
	if err != nil {
		return nil, err
	}
	p.anchors[name] = value
	return value, nil
}

// alias resolves a reference to an anchor declared earlier in the document.
func (p *parser) alias(rest string, l line, col int) (any, error) {
	p.pos++
	name := strings.TrimSpace(rest[1:])
	if name == "" {
		return nil, errAt(p.file, l, col, "an alias is written without a name")
	}
	value, known := p.anchors[name]
	if !known {
		return nil, errAt(p.file, l, col, "an alias names an anchor that has not been declared above it")
	}
	return copyNode(value), nil
}

// copyNode duplicates an aliased value.
//
// YAML lets two aliases share one node, but a merge key writes into the mapping
// it produced, and a shared node would let one locale's override reach another's
// defaults. Copying costs a walk over a handful of keys, once per alias, at load.
func copyNode(v any) any {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, child := range node {
			out[k] = copyNode(child)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			out[i] = copyNode(child)
		}
		return out
	default:
		return v
	}
}

// flowValue parses a whole flow collection written on one line.
func (p *parser) flowValue(s string, l line, col, depth int) (any, error) {
	f := &flow{p: p, s: s, l: l, col: col}
	v, err := f.value(depth)
	if err != nil {
		return nil, err
	}
	f.space()
	if f.i < len(f.s) {
		return nil, f.fail("this text follows the end of the flow collection")
	}
	return v, nil
}

// flow is a cursor over the one line a flow collection occupies.
type flow struct {
	p   *parser
	s   string
	i   int
	l   line
	col int
}

// fail builds a failure at the cursor's current column.
func (f *flow) fail(message string) error {
	return errAt(f.p.file, f.l, f.col+f.i, message)
}

// space advances past the blanks between flow tokens.
func (f *flow) space() {
	for f.i < len(f.s) && (f.s[f.i] == ' ' || f.s[f.i] == '\t') {
		f.i++
	}
}

// value parses one flow node.
func (f *flow) value(depth int) (any, error) {
	if depth >= MaxDepth {
		return nil, f.fail("the document nests more deeply than this parser will follow")
	}
	f.space()
	if f.i >= len(f.s) {
		return nil, f.fail("a value is missing")
	}
	switch f.s[f.i] {
	case '[':
		return f.sequence(depth)
	case '{':
		return f.mapping(depth)
	case '\'', '"':
		return f.quoted()
	case '*':
		return f.alias()
	case '!', '&':
		return nil, f.fail("this parser reads neither tags nor anchors inside a flow collection")
	default:
		return resolvePlain(f.plain()), nil
	}
}

// sequence parses "[a, b, c]", allowing the trailing comma YAML permits.
func (f *flow) sequence(depth int) (any, error) {
	f.i++
	out := []any{}
	for {
		f.space()
		if f.i >= len(f.s) {
			return nil, f.fail("a flow sequence is never closed")
		}
		if f.s[f.i] == ']' {
			f.i++
			return out, nil
		}
		item, err := f.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		done, err := f.separator(']', "sequence")
		if err != nil {
			return nil, err
		}
		if done {
			return out, nil
		}
	}
}

// mapping parses "{a: 1, b: 2}".
func (f *flow) mapping(depth int) (any, error) {
	f.i++
	out := map[string]any{}
	for {
		f.space()
		if f.i >= len(f.s) {
			return nil, f.fail("a flow mapping is never closed")
		}
		if f.s[f.i] == '}' {
			f.i++
			return out, nil
		}
		key, err := f.key()
		if err != nil {
			return nil, err
		}
		f.space()
		if f.i >= len(f.s) || f.s[f.i] != ':' {
			return nil, f.fail("a flow mapping needs a colon between a key and its value")
		}
		f.i++
		value, err := f.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out[key] = value
		done, err := f.separator('}', "mapping")
		if err != nil {
			return nil, err
		}
		if done {
			return out, nil
		}
	}
}

// separator consumes the comma between two flow entries, or the bracket that
// ends them. It reports whether the collection is finished.
func (f *flow) separator(closer byte, kind string) (bool, error) {
	f.space()
	if f.i >= len(f.s) {
		return false, f.fail("a flow " + kind + " is never closed")
	}
	switch f.s[f.i] {
	case ',':
		f.i++
		return false, nil
	case closer:
		f.i++
		return true, nil
	default:
		return false, f.fail("a flow " + kind + " needs a comma between its entries")
	}
}

// key reads a flow mapping key, which is a string whatever it looks like.
func (f *flow) key() (string, error) {
	f.space()
	if f.i < len(f.s) && (f.s[f.i] == '\'' || f.s[f.i] == '"') {
		text, err := f.quoted()
		if err != nil {
			return "", err
		}
		return text.(string), nil
	}
	key := f.plain()
	if key == "" {
		return "", f.fail("a flow mapping entry is written without a key")
	}
	return key, nil
}

// quoted reads a quoted scalar at the cursor.
func (f *flow) quoted() (any, error) {
	text, used, err := f.p.quoted(f.s[f.i:], f.l, f.col+f.i)
	if err != nil {
		return nil, err
	}
	f.i += used
	return text, nil
}

// alias resolves a reference to an anchor from inside a flow collection, which
// is what a merge naming several sources needs: "<<: [*base, *extra]".
func (f *flow) alias() (any, error) {
	f.i++
	name := strings.TrimSpace(f.plain())
	if name == "" {
		return nil, f.fail("an alias is written without a name")
	}
	value, known := f.p.anchors[name]
	if !known {
		return nil, f.fail("an alias names an anchor that has not been declared above it")
	}
	return copyNode(value), nil
}

// plain reads an unquoted flow scalar, which ends at the punctuation that
// separates it from whatever follows.
func (f *flow) plain() string {
	start := f.i
	for f.i < len(f.s) {
		c := f.s[f.i]
		if c == ',' || c == ']' || c == '}' {
			break
		}
		if c == ':' && (f.i+1 >= len(f.s) || isFlowBreak(f.s[f.i+1])) {
			break
		}
		f.i++
	}
	return strings.TrimRight(f.s[start:f.i], " \t")
}

// isFlowBreak reports whether a byte ends the key half of a flow entry.
func isFlowBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == ',' || c == ']' || c == '}'
}
