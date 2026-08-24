package core

import (
	"embed"

	"muzak.dev/framework"
	"muzak.dev/framework/i18n"
)

// locales holds the translations, built into the binary.
//
// They sit beside the configuration because that is what they are: a setting
// the service is started with. A Go embed reaches only its own directory and
// below, so a locale directory lives next to the package that embeds it rather
// than at the root of the module.
//
// Embedding them rather than reading them from disk is what keeps a deployment
// a single file: a service that shipped without its locale directory would
// start cleanly and then answer every request in the wrong language, which is
// exactly the kind of failure that reaches production.
//
//go:embed locales
var locales embed.FS

// LocaleOptions configures how a request's locale is chosen and where the
// translations come from.
//
// The Accept-Language header is read first, because every browser sends one and
// nothing has to be added to a URL for it to work. A query parameter is
// consulted first only so that the service can be tried in another language
// from a terminal:
//
//	curl 'http://localhost:8080/greeting?token=jessica&locale=es'
//	curl -H 'Accept-Language: es' 'http://localhost:8080/greeting?token=jessica'
func LocaleOptions() muzak.I18nOptions {
	return muzak.I18nOptions{
		Store: i18n.MustLoad(locales, "locales"),
		Sources: []muzak.LocaleSource{
			muzak.LocaleFromQuery,
			muzak.LocaleFromAcceptLanguage,
		},
	}
}
