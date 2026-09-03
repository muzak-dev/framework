// Package validate holds the rules Muzak applies to a bound request.
//
// Rules are declared against the field itself rather than against its name:
//
//	func (in *CreateUser) Validate(v *muzak.Validation) {
//		v.String(&in.Email).Required().Email()
//		v.Number(&in.Age).Between(18, 120)
//	}
//
// Because &in.Email is the field, renaming it is a change the compiler checks,
// and asking for a string rule on an integer field does not compile. The field
// name that reaches the client is derived from the same struct tag the binder
// used, so a query parameter and a body member are reported the same way.
//
// # What a rule set does
//
// Each builder collects transforms and checks in the order they are written.
// Transforms run first and mutate the bound value, so a handler sees the
// trimmed and lower-cased email rather than whatever arrived. Checks then run
// in order and stop at the first failure, which keeps a single empty field from
// producing a paragraph of complaints.
//
// A field that is empty and not marked Required skips its remaining checks
// entirely. That is what makes an optional field optional: MaxLen(20) has
// nothing to say about a value nobody sent.
//
// A number is the exception, and it is narrower. Zero is a value people mean,
// unlike an empty string, so it only counts as empty when the rules would have
// accepted zero anyway: Between(0, 60) skips it, and Between(1, 720) applies to
// it and rejects it. A rule set saying in as many words that zero is out of
// range should not be the one thing that lets it through, and the generated
// document publishes those bounds, so the server has to mean them. A number
// that may legitimately be absent and whose bounds exclude zero is a pointer.
//
// # Writing a rule
//
// A rule is an ordinary function, so it needs no registration and can be tested
// on its own:
//
//	func NotACommonPassword(password string) error {
//		if commonPasswords[password] {
//			return errors.New("is too common, choose something less guessable")
//		}
//		return nil
//	}
//
//	v.String(&in.Password).Must(NotACommonPassword)
//
// The error text is phrased to read after the field name, matching the issue
// strings the binder produces.
//
// # Documentation
//
// Rules describe themselves, so the OpenAPI document Muzak generates carries
// the constraints the code actually enforces: MinLen becomes minLength, OneOf
// becomes an enum, Between becomes minimum and maximum. The documentation
// cannot drift from the validation because both are read from the same
// declaration.
package validate
