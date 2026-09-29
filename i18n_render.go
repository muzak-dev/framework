package muzak

import "strings"

// message renders one of the framework's own strings in the request's locale,
// falling back to the English the framework would otherwise have produced.
//
// The fallback is not a defeat. Every message Muzak produces exists twice: as
// the Go constant that has always produced it, and as a key in the locale this
// package ships. An application that translates neither still reads exactly
// what it read before, which is what makes turning this feature on safe.
func (c *Context) message(key, english string) string {
	if text, ok := c.frameworkText(key); ok {
		return text
	}
	return english
}

// frameworkText renders a translation of one of the framework's own messages,
// and reports whether there was one to use.
//
// A translation that exists but cannot be rendered, because it names a value
// the framework's call site never passes, comes back from the store as the
// empty string, its exception handler having swallowed the failure. Sent as it
// is, that is a response with a blank message where the English it falls back
// to would have said what happened, so an empty rendering counts as no
// translation at all.
func (c *Context) frameworkText(key string, args ...any) (string, bool) {
	if c.i18n == nil || !c.i18n.Exists(c.locale, key) {
		return "", false
	}
	text := c.i18n.Translate(c.locale, key, args...)
	return text, text != ""
}

// translateDetails renders each validation failure in the request's locale.
//
// It returns the details it was given, untouched and uncopied, when there is
// nothing to do: no translation store, or no failure carrying a rule for a
// translator to have translated. That is the path an application which has not
// been localized takes on every rejected request, and it allocates nothing.
func translateDetails(c *Context, model string, details []ErrorDetail) []ErrorDetail {
	if c.i18n == nil {
		return details
	}

	var out []ErrorDetail
	for i := range details {
		text, translated := c.translateDetail(model, details[i])
		if !translated {
			continue
		}
		if out == nil {
			out = make([]ErrorDetail, len(details))
			copy(out, details)
		}
		out[i].Issue = text
	}
	if out == nil {
		return details
	}
	return out
}

// translateDetail renders one failure, and reports whether anything translated
// it.
//
// The keys are tried from the narrowest scope to the widest. That is what lets
// an application phrase one rule on one field of one model differently without
// restating every other.
func (c *Context) translateDetail(model string, detail ErrorDetail) (string, bool) {
	args := detail.Args
	if detail.Field != "" {
		// The field is available to every message, so that a translation may
		// name the thing it is complaining about.
		args = append(append([]any{}, args...), "attribute", detail.Field)
	}

	if detail.Key != "" {
		if text, ok := c.frameworkText(detail.Key, args...); ok {
			return text, true
		}
	}
	if detail.Kind == "" {
		return "", false
	}
	for _, key := range detailKeys(model, detail.Field, detail.Kind) {
		if text, ok := c.frameworkText(key, args...); ok {
			return text, true
		}
	}
	return "", false
}

// detailKeys lists the keys a failure is looked up under, narrowest first.
//
//	errors.models.<model>.attributes.<field>.<kind>
//	errors.models.<model>.<kind>
//	errors.attributes.<field>.<kind>
//	errors.messages.<kind>
//
// A framework that distinguishes a persisted record from a plain model needs a
// fifth level above these. Muzak does not: an input model is an ordinary struct
// with a Validate method, so there is only one kind of model to scope by.
func detailKeys(model, field, kind string) []string {
	keys := make([]string, 0, 4)
	if model != "" {
		if field != "" {
			keys = append(keys, "errors.models."+model+".attributes."+field+"."+kind)
		}
		keys = append(keys, "errors.models."+model+"."+kind)
	}
	if field != "" {
		keys = append(keys, "errors.attributes."+field+"."+kind)
	}
	return append(keys, "errors.messages."+kind)
}

// snakeCase renders a Go type name the way a locale file writes it, so that
// CreateUser is looked up as create_user.
//
// A locale file is edited by translators as often as by programmers, and Go's
// capitalisation is a convention of the language rather than of the content.
func snakeCase(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name) + 4)
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// httpMessage renders the message of an [HTTPError] in the request's locale.
//
// The error's own message is the fallback, which is what a caller who wrote one
// by hand meant to say, and what every one of the status constructors already
// carries.
func (c *Context) httpMessage(e *HTTPError) string {
	if e.MessageKey == "" {
		return e.Message
	}
	if text, ok := c.frameworkText(e.MessageKey, e.MessageArgs...); ok {
		return text
	}
	return e.Message
}

// bindingKey names the translation of a binding failure, or the empty string
// for one this package has no translation of.
//
// The binder's messages live under muzak.binding rather than under
// errors.messages, because they describe a value that could not be read at all
// rather than one that broke a rule the model declared, and there is nothing
// per-field about them for an application to phrase differently.
func bindingKey(err error) string {
	if key := bindingKeyFor(err); key != "" {
		return "muzak.binding." + key
	}
	return ""
}
