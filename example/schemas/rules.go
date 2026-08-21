package schemas

import (
	"errors"
	"strings"

	"badele/validate"
)

// commonPasswords stands in for the list a real service would load. Keeping it
// here rather than inside the rule means the rule stays a plain function that a
// test can call directly.
var commonPasswords = map[string]bool{
	"password1234":  true,
	"qwertyuiop12":  true,
	"letmeinplease": true,
}

// NotACommonPassword rejects a password from the known-bad list.
//
// It is an ordinary func(string) error, so it needs no registration, and its
// message is phrased to read after the field name, the way every built-in rule
// words its own failures.
func NotACommonPassword(password string) error {
	if commonPasswords[strings.ToLower(password)] {
		return errors.New("is too common, choose something less guessable")
	}
	return nil
}

// validateTag returns the rules every tag must satisfy.
//
// A rule set built once and reused is how a constraint stays consistent across
// the models that share it.
func validateTag() *validate.StringRules {
	return validate.String().Trim().Lower().MaxLen(20).
		Matches(`^[a-z0-9-]+$`).
		Message("may only contain lower case letters, digits and hyphens")
}
