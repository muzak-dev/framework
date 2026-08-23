package schemas

import (
	"errors"
	"net/http"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/validate"
)

// HTTPDate is a time carried in the format HTTP dates use.
//
// It exists because If-Modified-Since is not RFC 3339, which is what a bare
// time.Time parses. Implementing encoding.TextUnmarshaler is all it takes for
// the binder to accept the type, and the message below is what a client sees
// when it sends something else.
type HTTPDate struct {
	time.Time
}

// UnmarshalText parses an HTTP date, as RFC 9110 defines it.
func (d *HTTPDate) UnmarshalText(text []byte) error {
	parsed, err := http.ParseTime(string(text))
	if err != nil {
		return errors.New("must be an HTTP date, such as Wed, 21 Oct 2026 07:28:00 GMT")
	}
	d.Time = parsed
	return nil
}

// ClientHeaders groups the request headers every read endpoint cares about.
//
// Embedding it into an input promotes these fields onto that input, so a route
// that needs them declares one line rather than five, and the names, docs and
// defaults live in exactly one place.
type ClientHeaders struct {
	// Host is the authority the client addressed, which is what absolute URLs
	// in the response are built from.
	Host string `header:"Host" doc:"The authority the request was addressed to"`

	// SaveData is the client's data saver preference. It is the string "on"
	// when set, not a boolean, so it is bound as one.
	SaveData string `header:"Save-Data" default:"off" doc:"Set to on by a client asking for a smaller payload"`

	// IfModifiedSince turns the request into a conditional one.
	IfModifiedSince HTTPDate `header:"If-Modified-Since" doc:"Answer 304 when nothing changed since this time"`

	// Traceparent carries the caller's trace context, echoed back so a client
	// can correlate its span with this response.
	Traceparent string `header:"traceparent" doc:"W3C trace context of the calling span"`

	// Tags filter the feed. The header may be repeated, and every value is
	// bound in the order it arrived.
	Tags []string `header:"X-Tag" doc:"Restrict the feed to these tags; may be repeated"`
}

// SessionCookies groups the cookies the browser sends back.
type SessionCookies struct {
	// SessionID identifies the signed-in reader and must be present.
	SessionID string `cookie:"session_id" required:"true" doc:"The reader's session"`

	// The trackers are declared so they are documented and so a reader can be
	// told what is being read, not because the feed needs them.
	FatebookTracker string `cookie:"fatebook_tracker" doc:"Third party analytics cookie, if the reader accepted one"`
	GoogallTracker  string `cookie:"googall_tracker" doc:"Third party analytics cookie, if the reader accepted one"`
}

// FeedIn is the input of the feed endpoint: two shared groups and the one
// parameter that belongs to this route alone.
type FeedIn struct {
	ClientHeaders
	SessionCookies

	Limit int `query:"limit" default:"20" doc:"How many entries to return"`
}

// Validate declares what this route accepts, wherever a value came from.
//
// Rules bind to the field, not to the location, so a header is checked exactly
// as a query parameter is and its failure is reported against the header name
// the client sent. Transforms run first, which is what lets Save-Data be
// compared against a single spelling further down.
func (in *FeedIn) Validate(v *muzak.Validation) {
	v.String(&in.SaveData).Trim().Lower().OneOf("on", "off")
	v.String(&in.Traceparent).Matches(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`).
		Message("must be a W3C trace context, such as 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	v.Slice(&in.Tags).MaxItems(5).Each(validate.String().MaxLen(24))
	v.String(&in.SessionID).Trim().MinLen(4).MaxLen(64)
	v.Number(&in.Limit).Between(1, 100)
}

// FeedEntry is one item in the feed. Summary is dropped for a client that
// asked to save data.
type FeedEntry struct {
	ID      string   `json:"id"`
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	Tags    []string `json:"tags"`
	Summary string   `json:"summary,omitzero"`
}

// FeedOut is what the feed endpoint returns.
type FeedOut struct {
	Reader    string      `json:"reader"`
	Trimmed   bool        `json:"trimmed"`
	Tracked   bool        `json:"tracked"`
	Entries   []FeedEntry `json:"entries"`
	UpdatedAt time.Time   `json:"updated_at"`
}
