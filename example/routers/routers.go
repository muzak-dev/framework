// Package routers binds handlers to paths.
//
// Each function returns a router that knows nothing about where it will be
// mounted: no prefix, no tags beyond its own group, and no authentication.
// Those are the application's decisions, applied in main where the routers are
// included, which keeps every router reusable and keeps the security decision
// in one visible place.
package routers
