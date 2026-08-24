package yaml

import (
	"errors"
	"fmt"
)

// ErrSyntax is the sentinel every parse failure wraps, so that a caller which
// only needs to know that a file would not load can ask with errors.Is without
// caring which construct was at fault.
var ErrSyntax = errors.New("yaml: the document could not be parsed")

// SyntaxError reports a document this parser will not read.
//
// It names the line, the column and the construct rather than failing vaguely,
// because a locale file is edited by people who are translating rather than
// programming: "line 42, column 3" is what lets them find the problem, and the
// name of the construct is what tells them it was never going to work.
type SyntaxError struct {
	// File is the name the document was read under. It is empty for a document
	// parsed from memory.
	File string
	// Line is the 1-based line the problem was found on.
	Line int
	// Column is the 1-based column the problem was found at.
	Column int
	// Message says what is wrong, phrased as a sentence that reads after the
	// position.
	Message string
}

// Error renders the failure with its position, in the file:line:column form an
// editor can jump to.
func (e *SyntaxError) Error() string {
	if e.File == "" {
		return fmt.Sprintf("yaml: line %d, column %d: %s", e.Line, e.Column, e.Message)
	}
	return fmt.Sprintf("yaml: %s:%d:%d: %s", e.File, e.Line, e.Column, e.Message)
}

// Unwrap reports [ErrSyntax], so that every parse failure answers to one
// errors.Is check however it was produced.
func (e *SyntaxError) Unwrap() error { return ErrSyntax }
