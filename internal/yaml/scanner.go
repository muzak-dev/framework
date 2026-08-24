package yaml

import "strings"

// line is one physical line of the document, split into the indentation that
// positions it and the content that follows.
//
// The raw text is kept alongside the split form because a block scalar is
// defined by columns rather than by structure: it has to measure the original
// indentation of lines that the block parser has already stepped past.
type line struct {
	// num is the 1-based line number, for errors.
	num int
	// indent is the number of leading spaces.
	indent int
	// text is what follows the indentation, with trailing spaces removed.
	text string
	// raw is the line exactly as written, minus its line ending.
	raw string
	// blank reports a line holding nothing but spaces or nothing but a comment,
	// which the block parser skips and a block scalar keeps.
	blank bool
	// tabColumn is the 1-based column of a tab used for indentation, or zero.
	// It is recorded rather than reported immediately so that a tab inside a
	// value, which is legal, is not confused with a tab positioning a line.
	tabColumn int
}

// scan splits a document into lines, normalizing the two things that differ
// between the machines a locale file is edited on: a byte order mark left by a
// Windows editor, and CRLF line endings.
func scan(data []byte) []line {
	text := string(data)
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	if text == "" {
		return nil
	}
	// A document ending in a newline would otherwise yield a final empty line,
	// which is blank and harmless but shows up in the line count.
	text = strings.TrimSuffix(text, "\n")

	raw := strings.Split(text, "\n")
	lines := make([]line, len(raw))
	for i, r := range raw {
		lines[i] = measure(i+1, r)
	}
	return lines
}

// measure splits one raw line into its indentation and its content.
func measure(num int, raw string) line {
	l := line{num: num, raw: raw}
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case ' ':
			l.indent++
		case '\t':
			if l.tabColumn == 0 {
				l.tabColumn = i + 1
			}
			l.indent++
		default:
			l.text = strings.TrimRight(raw[i:], " \t")
			l.blank = l.text == "" || l.text[0] == '#'
			return l
		}
	}
	l.text = ""
	l.blank = true
	return l
}

// blockScalar reads the lines of a literal or folded scalar.
//
// The header is the part of the value line from the indicator onwards, such as
// "|-" or ">2"; parent is the indentation of the key the scalar belongs to.
// The reader stops at the first non-blank line indented no further than the
// parent, which is the next structural line.
func (p *parser) blockScalar(header string, parent int) (string, error) {
	at := p.lines[p.pos]
	fold := header[0] == '>'

	chomp, explicit, err := scalarHeader(header, p.file, at)
	if err != nil {
		return "", err
	}
	p.pos++

	// content is the column the scalar's text starts at. An explicit indicator
	// fixes it relative to the parent; otherwise it is taken from the first
	// line that has any text, which is what YAML calls auto-detection.
	content := 0
	if explicit > 0 {
		content = parent + explicit
	}

	var body []string
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if !l.blank && l.indent <= parent {
			break
		}
		if l.blank && strings.TrimSpace(l.raw) == "" {
			body = append(body, "")
			p.pos++
			continue
		}
		if content == 0 {
			content = l.indent
		}
		if l.indent < content {
			break
		}
		body = append(body, l.raw[content:])
		p.pos++
	}

	return assemble(body, fold, chomp), nil
}

// scalarHeader reads the chomping and indentation indicators off a block scalar
// header, rejecting anything else written there.
//
// The header holds nothing but the indicators. A trailing comment was removed
// and trailing spaces were trimmed before the line reached this point, so
// anything still here that is not an indicator was never one.
func scalarHeader(header, file string, at line) (chomp byte, indent int, err error) {
	for i := 1; i < len(header); i++ {
		c := header[i]
		switch {
		case c == '-' || c == '+':
			if chomp != 0 {
				return 0, 0, errAt(file, at, at.indent+i+1, "a block scalar declares its chomping twice")
			}
			chomp = c
		case c >= '1' && c <= '9':
			if indent != 0 {
				return 0, 0, errAt(file, at, at.indent+i+1, "a block scalar declares its indentation twice")
			}
			indent = int(c - '0')
		default:
			return 0, 0, errAt(file, at, at.indent+i+1,
				"a block scalar header accepts only a chomping indicator and an indentation digit")
		}
	}
	return chomp, indent, nil
}

// assemble joins the lines of a block scalar according to its style and its
// chomping indicator.
//
// Folding is the part with rules worth stating. A break between two ordinary
// lines becomes a space; a blank line becomes a break of its own, so a
// paragraph survives; and a line indented further than the block keeps its
// break, so that a verse or a code sample is not run together.
func assemble(body []string, fold bool, chomp byte) string {
	// Trailing blank lines are held back until chomping decides how many of
	// them survive.
	trailing := 0
	for trailing < len(body) && body[len(body)-1-trailing] == "" {
		trailing++
	}
	kept := body[:len(body)-trailing]

	var b strings.Builder
	broken := false
	for i, l := range kept {
		switch {
		case i == 0:
		case !fold:
			b.WriteByte('\n')
		case l == "":
			// A blank line is a paragraph break. It contributes the break and
			// nothing else, and the line after it needs no separator.
			b.WriteByte('\n')
			broken = true
			continue
		case broken:
			broken = false
		case isIndented(l) || isIndented(kept[i-1]):
			b.WriteByte('\n')
		default:
			b.WriteByte(' ')
		}
		b.WriteString(l)
	}

	out := b.String()
	switch chomp {
	case '-':
		return out
	case '+':
		// Every content line ends in a break of its own, and each blank line
		// held back above contributes one more. An all-blank block has no
		// content line, so it contributes only the blanks.
		if out == "" {
			return strings.Repeat("\n", trailing)
		}
		return out + "\n" + strings.Repeat("\n", trailing)
	default:
		if out == "" && trailing == 0 {
			return ""
		}
		return out + "\n"
	}
}

// isIndented reports whether a line inside a folded scalar carries indentation
// of its own, which is what stops it being folded into its neighbour.
func isIndented(l string) bool {
	return l != "" && (l[0] == ' ' || l[0] == '\t')
}
