package core

import (
	"time"

	"muzak.dev/framework"
)

// RateLimitPolicy is the application-wide budget every route inherits.
//
// Three windows rather than one, because a single number cannot tell a person
// clicking from a script that never stops: three requests a second is generous
// for the first and impossible for the second, while a hundred a minute is the
// reverse. Every quota is counted for every request, so a client that overruns
// the short window still accrues against the long one.
//
// No storage is named, so the application is given one that counts in memory,
// which is the right answer for a single process and the wrong one the moment
// there are two. A service that runs more than once names a shared storage
// here instead.
func RateLimitPolicy() muzak.RateLimitOptions {
	return muzak.RateLimitOptions{
		Tracker: UserOrIPTracker,
		Quotas: []muzak.Quota{
			{Name: "short", Window: time.Second, Limit: 3},
			{Name: "medium", Window: 10 * time.Second, Limit: 20},
			{Name: "long", Window: time.Minute, Limit: 100},
		},
	}
}

// UserOrIPTracker spends a request from the caller's budget when there is a
// caller, and from the address's otherwise.
//
// The identity is read with TryFrom rather than From, because most routes here
// resolve no user at all and an absent dependency is a legitimate state for
// this tracker rather than a programming error. Keys from the two sources are
// prefixed differently so that a username can never collide with an address.
//
// Reading a resolved identity only works where the count happens after the
// route's dependencies have run, which the items router asks for with
// muzak.RateLimitOptions.AfterDependencies. Everywhere else the count comes
// first and this falls back to the address, which is the trade the application
// makes on purpose: a request a guard rejects is worth counting.
func UserOrIPTracker(ctx *muzak.Context) (string, error) {
	if user, ok := muzak.TryFrom[CurrentUser](ctx); ok {
		return "user:" + user.Username, nil
	}
	return muzak.IPTracker(ctx)
}
