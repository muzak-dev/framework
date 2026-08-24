package yaml

import (
	"strings"
)

// MaxDepth bounds how deeply a document may nest.
//
// A locale file nests perhaps six levels; the limit is far above anything real
// so that it never rejects a genuine file, and far below what would exhaust the
// stack so that a hostile one cannot. It matters because the fuzz target in
// this package reaches the same entry point an application does.
const MaxDepth = 100

// Parse decodes a document into a tree.
//
// Values are one of map[string]any, []any, string, int64, float64, bool or nil.
// Keys are always strings, whatever they look like. The top level must be a
// mapping, which every locale file is: its one key is the locale.
func Parse(data []byte) (map[string]any, error) {
	return ParseFile("", data)
}

// ParseFile is [Parse] with a file name attached to any error it reports, so
// that a document which will not load says which one it was.
func ParseFile(name string, data []byte) (map[string]any, error) {
	p := &parser{file: name, lines: scan(data), anchors: map[string]any{}}
	return p.document()
}

// parser holds the position of one parse over one document.
type parser struct {
	file    string
	lines   []line
	pos     int
	anchors map[string]any
}

// errAt builds a located failure. Columns are 1-based, as an editor counts them.
func errAt(file string, at line, col int, message string) error {
	if col < 1 {
		col = 1
	}
	return &SyntaxError{File: file, Line: at.num, Column: col, Message: message}
}

// document parses a whole file: the directives and markers that may wrap it,
// and the single node inside.
func (p *parser) document() (map[string]any, error) {
	p.skipBlank()
	if err := p.directives(); err != nil {
		return nil, err
	}
	p.skipBlank()

	root, err := p.block(-1, 0)
	if err != nil {
		return nil, err
	}

	if err := p.end(); err != nil {
		return nil, err
	}

	switch node := root.(type) {
	case nil:
		return map[string]any{}, nil
	case map[string]any:
		return node, nil
	default:
		return nil, errAt(p.file, p.lines[0], 1, "the top level of a locale file must be a mapping")
	}
}

// directives reads the "%YAML" and "---" that may open a document, and refuses
// the directives this parser has no meaning for.
func (p *parser) directives() error {
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if !strings.HasPrefix(l.text, "%") {
			break
		}
		if !strings.HasPrefix(l.text, "%YAML") {
			return errAt(p.file, l, l.indent+1, "this parser understands no directive other than %YAML")
		}
		p.pos++
		p.skipBlank()
	}
	p.skipBlank()
	if p.pos < len(p.lines) {
		if l := p.lines[p.pos]; l.text == "---" || strings.HasPrefix(l.text, "--- ") {
			p.pos++
		}
	}
	return nil
}

// end checks that nothing follows the document but its terminator.
func (p *parser) end() error {
	p.skipBlank()
	if p.pos >= len(p.lines) {
		return nil
	}
	l := p.lines[p.pos]
	if l.text == "..." {
		p.pos++
		p.skipBlank()
		if p.pos >= len(p.lines) {
			return nil
		}
		l = p.lines[p.pos]
	}
	if l.text == "---" || strings.HasPrefix(l.text, "--- ") {
		return errAt(p.file, l, l.indent+1, "this parser reads one document per file")
	}
	return errAt(p.file, l, l.indent+1, "this content follows the end of the document")
}

// skipBlank advances past blank and comment-only lines.
func (p *parser) skipBlank() {
	for p.pos < len(p.lines) && p.lines[p.pos].blank {
		p.pos++
	}
}

// block parses the node made of the lines indented further than parent, or
// nothing when the next line is not one of them.
func (p *parser) block(parent, depth int) (any, error) {
	p.skipBlank()
	if p.pos >= len(p.lines) {
		return nil, nil
	}
	l := p.lines[p.pos]
	if l.indent <= parent || isEnd(l.text) {
		return nil, nil
	}
	return p.node(l.indent, depth)
}

// node parses the node whose entries all sit at the given indentation,
// choosing between a sequence and a mapping by what the first line looks like.
func (p *parser) node(indent, depth int) (any, error) {
	if depth >= MaxDepth {
		return nil, errAt(p.file, p.lines[p.pos], indent+1, "the document nests more deeply than this parser will follow")
	}
	l := p.lines[p.pos]
	if l.tabColumn > 0 {
		return nil, errAt(p.file, l, l.tabColumn, "a tab is used to indent this line; YAML requires spaces")
	}
	if isSequenceEntry(stripComment(l.text)) {
		return p.sequence(indent, depth)
	}
	return p.mapping(indent, depth)
}

// isSequenceEntry reports whether a line opens a sequence entry. A dash is only
// an entry when a space or the end of the line follows it, so that "-3" stays a
// number and "-- see below" stays text.
func isSequenceEntry(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

// isEnd reports whether a line is a document marker rather than content.
func isEnd(text string) bool {
	return text == "..." || text == "---" || strings.HasPrefix(text, "--- ")
}

// mapping parses a block mapping whose keys all sit at the given indentation.
func (p *parser) mapping(indent, depth int) (any, error) {
	out := map[string]any{}
	for {
		p.skipBlank()
		if p.pos >= len(p.lines) {
			return out, nil
		}
		l := p.lines[p.pos]
		if l.indent < indent || isEnd(l.text) {
			return out, nil
		}
		if l.tabColumn > 0 {
			return nil, errAt(p.file, l, l.tabColumn, "a tab is used to indent this line; YAML requires spaces")
		}
		if l.indent > indent {
			return nil, errAt(p.file, l, l.indent+1, "this line is indented further than the mapping it belongs to")
		}

		text := stripComment(l.text)
		if strings.HasPrefix(text, "? ") {
			return nil, errAt(p.file, l, l.indent+1, "this parser does not read the explicit key form")
		}
		raw, rest, quoted, ok := splitKey(text)
		if !ok {
			return nil, errAt(p.file, l, l.indent+1, "this line is not a mapping entry; a key must be followed by a colon and a space")
		}
		key, err := p.key(raw, quoted, l)
		if err != nil {
			return nil, err
		}

		if key == "<<" {
			if err := p.merge(out, rest, l, indent, depth); err != nil {
				return nil, err
			}
			continue
		}

		value, err := p.value(rest, l, indent, depth)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
}

// key reads a mapping key, which is always a string.
func (p *parser) key(raw string, quoted bool, l line) (string, error) {
	if quoted {
		text, used, err := p.quoted(raw, l, l.indent+1)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(raw[used:]) != "" {
			return "", errAt(p.file, l, l.indent+used+1, "a quoted key is followed by text that is not part of it")
		}
		return text, nil
	}
	key := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(key, "&"):
		return "", errAt(p.file, l, l.indent+1, "this parser does not anchor a key")
	case strings.HasPrefix(key, "!"):
		return "", errAt(p.file, l, l.indent+1, "this parser does not read explicit tags")
	case strings.HasPrefix(key, "["), strings.HasPrefix(key, "{"):
		return "", errAt(p.file, l, l.indent+1, "this parser reads only scalar keys")
	}
	return key, nil
}

// quoted reads a quoted scalar, dispatching on which quote opened it.
func (p *parser) quoted(s string, l line, col int) (string, int, error) {
	if s[0] == '\'' {
		return unquoteSingle(s, p.file, l, col)
	}
	return unquoteDouble(s, p.file, l, col)
}

// sequence parses a block sequence whose dashes all sit at the given
// indentation.
func (p *parser) sequence(indent, depth int) (any, error) {
	out := []any{}
	for {
		p.skipBlank()
		if p.pos >= len(p.lines) {
			return out, nil
		}
		l := p.lines[p.pos]
		if l.indent < indent || isEnd(l.text) {
			return out, nil
		}
		if l.tabColumn > 0 {
			return nil, errAt(p.file, l, l.tabColumn, "a tab is used to indent this line; YAML requires spaces")
		}
		if l.indent > indent {
			return nil, errAt(p.file, l, l.indent+1, "this line is indented further than the sequence it belongs to")
		}
		text := stripComment(l.text)
		if !isSequenceEntry(text) {
			// A key at the same indentation as the dashes ends the sequence and
			// belongs to the mapping above it, which is the layout that writes
			// a sequence flush with its own key.
			return out, nil
		}

		body := strings.TrimLeft(text[1:], " ")
		if body == "" {
			p.pos++
			item, err := p.block(l.indent, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
			continue
		}

		// An entry that itself opens a collection is re-read as a line of its
		// own, positioned at the column its content starts in. That is what
		// makes "- name: a" the first key of a mapping rather than a scalar,
		// and it is why the following keys line up under it.
		offset := 1 + (len(text) - 1 - len(body))
		if _, _, _, isEntry := splitKey(body); isEntry || isSequenceEntry(body) {
			p.lines[p.pos] = line{num: l.num, indent: l.indent + offset, text: body, raw: l.raw}
			item, err := p.node(l.indent+offset, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, item)
			continue
		}

		item, err := p.value(body, l, l.indent+offset-1, depth)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
}

// merge applies a merge key, folding an aliased mapping into this one.
//
// A key already written here wins over a merged one, which is what YAML says
// and what makes a merge useful: the alias supplies the defaults and the entry
// below it states the exception.
func (p *parser) merge(out map[string]any, rest string, l line, indent, depth int) error {
	value, err := p.value(rest, l, indent, depth)
	if err != nil {
		return err
	}
	sources := []any{value}
	if list, isList := value.([]any); isList {
		sources = list
	}
	for _, source := range sources {
		from, isMap := source.(map[string]any)
		if !isMap {
			return errAt(p.file, l, l.indent+1, "a merge key names something that is not a mapping")
		}
		for k, v := range from {
			if _, taken := out[k]; !taken {
				out[k] = v
			}
		}
	}
	return nil
}
