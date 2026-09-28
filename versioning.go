package muzak

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// VersioningType selects how a request declares which version of the API it
// wants.
type VersioningType uint8

const (
	// VersioningNone is the zero value: versioning is off. [WithVersion] may
	// not be used anywhere in the application, and every route answers
	// exactly as it would if this package did not exist.
	VersioningNone VersioningType = iota
	// VersioningURI reads the version from the request's own path, such as
	// "/v1/cats", inserting it automatically for every route that declares
	// one. It is the default a new application should reach for first.
	VersioningURI
	// VersioningHeader reads the version from a request header named by
	// [VersioningOptions.Header].
	VersioningHeader
	// VersioningMediaType reads the version from a parameter of the request's
	// Accept header, named by [VersioningOptions.Key], as in
	// "application/json;v=2".
	VersioningMediaType
	// VersioningCustom reads the version using [VersioningOptions.Extractor],
	// for anything the three built-in types do not cover.
	VersioningCustom
)

// Version identifies one version a route or router answers.
//
// It is an ordinary string with one reserved value, [VersionNeutral]; every
// other value is whatever an application chooses to call its versions, such
// as "1", "2" or "2023-01-01".
type Version string

// VersionNeutral marks a route or router as answering every request
// regardless of the version it names, including a request that names none
// at all. For [VersioningURI] specifically, a version-neutral route is
// reached at its plain path, with no version segment inserted.
//
// It cannot be combined with another version in the same [WithVersion] call
// or the same [VersioningOptions.DefaultVersion], because it already answers
// every request the narrower version would.
const VersionNeutral Version = "\x00neutral"

// VersionExtractor pulls the version(s) a request declares out of it, for
// [VersioningCustom].
//
// Return them in order from most to least preferred: a request that says it
// accepts several versions is matched against the first of them that some
// route answers, exactly as [VersioningOptions.DefaultVersion] and a route's
// own [WithVersion] list are. Returning nil or an empty slice reports a
// request that names no version at all, which only a route registered
// [VersionNeutral] answers.
type VersionExtractor func(r *http.Request) []string

// VersioningOptions configures how requests declare which version of the API
// they want.
//
// The zero value leaves versioning off: Type is [VersioningNone],
// [WithVersion] may not be used anywhere in the application, and every route
// answers exactly as it would without this package existing at all. Set Type
// to turn versioning on, through [AppOptions.Versioning]:
//
//	app := muzak.New(muzak.AppOptions{
//		Versioning: muzak.VersioningOptions{Type: muzak.VersioningURI},
//	})
type VersioningOptions struct {
	// Type selects how a request declares its version. It defaults to
	// [VersioningNone], which is versioning turned off.
	Type VersioningType

	// Prefix is prepended to the version in the request path, for
	// [VersioningURI], defaulting to "v" so that version "1" is reached at
	// "/v1". Point it at an empty string, through a pointer to one, to use
	// the version verbatim with no prefix at all.
	Prefix *string

	// Header names the request header carrying the version, for
	// [VersioningHeader]. It is required for that type.
	Header string

	// Key is the Accept header parameter naming the version, for
	// [VersioningMediaType], such as "v=" for "application/json;v=2". It is
	// required for that type.
	Key string

	// Extractor pulls the version(s) a request carries, for
	// [VersioningCustom]. It is required for that type.
	//
	// The response to a versioned path depends on whatever the extractor
	// reads, and a shared cache has to be told so with Vary, or it hands the
	// answer for one version to a client asking for another. [VersioningHeader]
	// and [VersioningMediaType] add their header to Vary on their own, but an
	// extractor may read anything, so under [VersioningCustom] the application
	// declares it: a middleware installed with [App.Use] that adds the header
	// the extractor reads to Vary on every response, for example
	// w.Header().Add("Vary", "X-Api-Version") before calling the next handler.
	Extractor VersionExtractor

	// DefaultVersion is used for a route or router that declares none of its
	// own with [WithVersion]. Left unset, such a route answers no request at
	// all while versioning is enabled, rather than being served
	// unversioned: an application has to opt a route into being
	// version-independent deliberately, with [VersionNeutral], not by
	// omission.
	DefaultVersion []Version
}

// enabled reports whether versioning is turned on at all.
func (o VersioningOptions) enabled() bool { return o.Type != VersioningNone }

// prefix resolves the URI prefix, defaulting to "v".
func (o VersioningOptions) prefix() string {
	if o.Prefix != nil {
		return *o.Prefix
	}
	return "v"
}

// validate reports a [VersioningOptions] that cannot mean anything: a type
// missing the field it depends on, or a default version list that is
// malformed on its own terms.
func (o VersioningOptions) validate() error {
	switch o.Type {
	case VersioningNone:
		if len(o.DefaultVersion) > 0 {
			return errors.New("muzak: AppOptions.Versioning.DefaultVersion is set but Type is not; set Type to enable versioning")
		}
		return nil
	case VersioningURI:
	case VersioningHeader:
		if o.Header == "" {
			return errors.New("muzak: AppOptions.Versioning.Header must name a header for header versioning")
		}
	case VersioningMediaType:
		if o.Key == "" {
			return errors.New("muzak: AppOptions.Versioning.Key must be set for media type versioning")
		}
	case VersioningCustom:
		if o.Extractor == nil {
			return errors.New("muzak: AppOptions.Versioning.Extractor must be set for custom versioning")
		}
	default:
		return fmt.Errorf("muzak: AppOptions.Versioning.Type %d is not a recognised versioning type", o.Type)
	}
	return validateVersionList(o.DefaultVersion, "AppOptions.Versioning.DefaultVersion")
}

// validateVersionList rejects a version list that cannot mean anything:
// [VersionNeutral] combined with another version, which cannot narrow what
// neutral already answers. what names the declaration in the error, such as
// "GET /cats" or "AppOptions.Versioning.DefaultVersion".
func validateVersionList(versions []Version, what string) error {
	if len(versions) < 2 {
		return nil
	}
	for _, v := range versions {
		if v == VersionNeutral {
			return fmt.Errorf("muzak: %s: VersionNeutral cannot be combined with another version", what)
		}
	}
	return nil
}

// isVersionNeutral reports whether a route's resolved versions are exactly
// [VersionNeutral].
func (rt *Route) isVersionNeutral() bool {
	return len(rt.Versions) == 1 && rt.Versions[0] == VersionNeutral
}

// answersVersion reports whether the route answers a request that declared
// version v.
func (rt *Route) answersVersion(v Version) bool {
	if rt.isVersionNeutral() {
		return true
	}
	for _, have := range rt.Versions {
		if have == v {
			return true
		}
	}
	return false
}

// versionsOverlap reports whether two routes' resolved version sets could
// both answer the same request, which is what makes registering both of them
// at the same method and path ambiguous.
//
// An empty set never overlaps with anything: a route that resolved no
// version at all answers no request (see [Route.versionVariants]), so there
// is nothing it could collide with. [VersionNeutral] overlaps with
// everything, including another version-neutral declaration, because it
// answers every request the narrower side would.
func versionsOverlap(a, b []Version) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if isNeutralList(a) || isNeutralList(b) {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// isNeutralList reports whether a resolved version list is exactly
// [VersionNeutral].
func isNeutralList(versions []Version) bool {
	return len(versions) == 1 && versions[0] == VersionNeutral
}

// versionVariants returns the Route(s) that should actually be registered
// for dispatch, given the application's versioning configuration.
//
//   - Versioning off: exactly [rt], unchanged. This is the common case, and
//     it costs nothing beyond the slice allocation the caller already pays
//     for holding one route.
//   - Enabled but rt resolved no version at all, meaning neither it nor
//     anything it is declared under named one and no
//     [VersioningOptions.DefaultVersion] applies: no variants at all, so the
//     route is never registered and answers no request, matching the
//     documented behaviour for an unversioned resource under an
//     application that has versioning enabled.
//   - [VersioningURI]: a route naming more than one version is exploded into
//     one path-distinct clone per version, since there the version is part
//     of the path rather than something matched once a request arrives. A
//     version-neutral route is not exploded, because it is reached at its
//     plain path with no version segment.
//   - Every other type: rt is kept as one entry carrying every version it
//     declared, matched against the request when it arrives; see
//     [selectVersion].
func (rt *Route) versionVariants(v VersioningOptions) []*Route {
	if !v.enabled() {
		return []*Route{rt}
	}
	if len(rt.Versions) == 0 {
		return nil
	}
	if v.Type != VersioningURI || rt.isVersionNeutral() {
		return []*Route{rt}
	}

	explicit := rt.cfg.operationID != ""
	variants := make([]*Route, len(rt.Versions))
	for i, ver := range rt.Versions {
		clone := *rt
		clone.Path = v.prefix() + string(ver) + rt.Path
		// The leading prefix and version are text an application chose, not
		// something matched by "/", so they are joined to the path directly
		// rather than through a "/" the caller might already have supplied.
		if !strings.HasPrefix(clone.Path, "/") {
			clone.Path = "/" + clone.Path
		}
		clone.Versions = []Version{ver}
		if explicit {
			clone.OperationID = rt.cfg.operationID + "_" + sanitizeIdent(string(ver))
		} else {
			clone.OperationID = deriveOperationID(clone.Method, clone.Path)
		}
		variants[i] = &clone
	}
	return variants
}

// requestedVersions extracts the version(s) a request declares, in order
// from most to least preferred, according to o.Type. It returns nil for a
// request that declares no version at all, which only a route registered
// [VersionNeutral] answers, and for a type that cannot extract one from this
// request (an empty header, an Accept header without the configured
// parameter, or an extractor that found nothing).
func (o VersioningOptions) requestedVersions(r *http.Request) []Version {
	switch o.Type {
	case VersioningHeader:
		v := r.Header.Get(o.Header)
		if v == "" {
			return nil
		}
		return []Version{Version(v)}
	case VersioningMediaType:
		return o.mediaTypeVersion(r)
	case VersioningCustom:
		return o.extractedVersions(r)
	default:
		return nil
	}
}

// varyField names the request header a response depends on when a route is
// chosen by version, for [App.matchVersion] to add to Vary: the configured
// header for [VersioningHeader], and Accept for [VersioningMediaType]. It is
// empty for [VersioningCustom], whose extractor may read anything and has to
// declare it itself (see [VersioningOptions.Extractor]), and for
// [VersioningURI], where the version is part of the URL a cache already keys
// on.
func (o VersioningOptions) varyField() string {
	switch o.Type {
	case VersioningHeader:
		return http.CanonicalHeaderKey(o.Header)
	case VersioningMediaType:
		return "Accept"
	default:
		return ""
	}
}

// mediaTypeVersion finds o.Key's value among the parameters of the request's
// Accept header, which may name several media ranges separated by commas,
// each with its own parameters separated by semicolons.
func (o VersioningOptions) mediaTypeVersion(r *http.Request) []Version {
	accept := r.Header.Get("Accept")
	if accept == "" || o.Key == "" {
		return nil
	}
	for mediaRange := range strings.SplitSeq(accept, ",") {
		for param := range strings.SplitSeq(mediaRange, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(param), o.Key); ok && v != "" {
				return []Version{Version(v)}
			}
		}
	}
	return nil
}

// extractedVersions runs the configured extractor and drops any empty
// strings it returned, which is what an extractor built from a raw,
// possibly-absent header naturally produces.
func (o VersioningOptions) extractedVersions(r *http.Request) []Version {
	if o.Extractor == nil {
		return nil
	}
	raw := o.Extractor(r)
	if len(raw) == 0 {
		return nil
	}
	out := make([]Version, 0, len(raw))
	for _, v := range raw {
		if v != "" {
			out = append(out, Version(v))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// versionSuffix joins a route's own resolved versions into the text an
// auto-derived OperationID is extended with, which is what keeps two
// registrations sharing one method and path unique once a header, media type
// or custom versioning scheme distinguishes them only at dispatch time
// rather than in the path itself; see the OperationID derivation in
// [Route.resolve].
func versionSuffix(versions []Version) string {
	parts := make([]string, len(versions))
	for i, v := range versions {
		parts[i] = sanitizeIdent(string(v))
	}
	return strings.Join(parts, "_")
}

// selectVersion finds, among candidates (every route registered for the same
// method and path), the one that answers requested, or nil when none does.
//
// Each requested version is tried in order from most to least preferred, so
// that a client offering several versions is matched against the highest one
// a route actually answers; a version-neutral candidate is tried last,
// because it is only reached once nothing more specific matched, or when
// requested is empty (a request that named no version at all).
func selectVersion(candidates []*Route, requested []Version) *Route {
	for _, want := range requested {
		for _, rt := range candidates {
			if rt.answersVersion(want) {
				return rt
			}
		}
	}
	for _, rt := range candidates {
		if rt.isVersionNeutral() {
			return rt
		}
	}
	return nil
}
