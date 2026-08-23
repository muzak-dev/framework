package handlers

import (
	"net/http"
	"slices"
	"time"

	"muzak.dev/framework"
	"muzak.dev/framework/example/schemas"
)

// feedUpdatedAt stands in for the time the feed last changed, which a real
// service would read from its store.
var feedUpdatedAt = time.Date(2026, time.August, 20, 9, 30, 0, 0, time.UTC)

// feedEntries stands in for the feed itself.
var feedEntries = []schemas.FeedEntry{
	{ID: "1", Title: "Binding without reflection on the request path", Tags: []string{"go", "muzak.dev/framework"},
		Summary: "How the plan is compiled once, at start-up."},
	{ID: "2", Title: "Uploads that cannot outgrow their limit", Tags: []string{"go", "http"},
		Summary: "Why the body limit is enforced while the body is read."},
	{ID: "3", Title: "A logger that is readable and parseable", Tags: []string{"go"},
		Summary: "Aligned columns for people, JSON for machines."},
}

// Feed answers the reader's feed.
//
// Every header and cookie it reads is already bound and converted by the time
// this runs, so the handler contains the decisions and none of the parsing.
func Feed(ctx *muzak.Context, in schemas.FeedIn) (schemas.FeedOut, error) {
	// A conditional request is answered without a body when nothing changed.
	// The header was parsed into a time by the binder, so this is a comparison
	// rather than a parse that could fail here.
	if !in.IfModifiedSince.IsZero() && !feedUpdatedAt.After(in.IfModifiedSince.Time) {
		ctx.SetStatus(http.StatusNotModified)
		return schemas.FeedOut{}, nil
	}

	// The caller's trace context is echoed so its span and this response can be
	// tied together, and the request identifier is what ties it to our log.
	if in.Traceparent != "" {
		ctx.SetHeader("traceparent", in.Traceparent)
	}
	// The response varies by all three, so caches must be told.
	ctx.SetHeader("Vary", "Save-Data, X-Tag, Cookie")
	ctx.SetHeader("Last-Modified", feedUpdatedAt.UTC().Format(http.TimeFormat))

	trimmed := in.SaveData == "on"
	entries := make([]schemas.FeedEntry, 0, len(feedEntries))
	for _, entry := range feedEntries {
		// A repeated X-Tag narrows the feed. No tags at all means no filter.
		if len(in.Tags) > 0 && !slices.ContainsFunc(in.Tags, func(tag string) bool {
			return slices.Contains(entry.Tags, tag)
		}) {
			continue
		}
		entry.URL = "https://" + in.Host + "/entries/" + entry.ID
		if trimmed {
			// The client asked for less, so the summary is left out. The field
			// is omitzero, so it disappears from the response entirely.
			entry.Summary = ""
		}
		entries = append(entries, entry)
		if len(entries) == in.Limit {
			break
		}
	}

	return schemas.FeedOut{
		Reader:    in.SessionID,
		Trimmed:   trimmed,
		Tracked:   in.FatebookTracker != "" || in.GoogallTracker != "",
		Entries:   entries,
		UpdatedAt: feedUpdatedAt,
	}, nil
}
