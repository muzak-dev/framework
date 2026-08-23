// Package core holds the pieces every other package in the service depends on:
// its configuration, its guard and value dependencies, and the resources whose
// lifetime the application manages.
//
// Nothing here imports the handlers, the schemas or the routers, which keeps
// the dependency direction one way and makes each of those packages testable on
// its own.
package core

import (
	"muzak.dev/framework"
)

// Settings is the service configuration, read once at start-up from the
// environment and from a .env file when one is present.
//
// It is published to every handler with muzak.WithSingleton, so a handler
// reads it with muzak.From[core.Settings](ctx) rather than through a package
// level variable.
type Settings struct {
	// AppName titles the API in the generated documentation.
	AppName string `env:"APP_NAME" default:"Awesome API"`
	// AdminEmail is who to contact about the API.
	AdminEmail string `env:"ADMIN_EMAIL" default:"admin@example.com"`
	// ItemsPerUser caps how many items one user may hold.
	ItemsPerUser int `env:"ITEMS_PER_USER" default:"50"`
	// Addr is the address the server listens on.
	Addr string `env:"ADDR" default:":8080"`
	// AdminToken guards the admin subtree. It is marked secret so that a
	// malformed value never appears in a start-up error.
	AdminToken string `env:"ADMIN_TOKEN" default:"coneofsilence" secret:"true"`
	// TrustedProxies lists the proxies whose X-Forwarded-For header is
	// believed, as a comma-separated list of addresses or CIDR prefixes. It is
	// empty by default, so every request is attributed to the peer that made
	// it: a forwarding header is written by whatever sent the request, and
	// believing one from an unknown sender hands every client the ability to
	// choose which budget it spends.
	TrustedProxies []string `env:"TRUSTED_PROXIES" required:"false"`
}

// LoadSettings reads the configuration, stopping the process if a required
// value is missing.
//
// A service that cannot read its own configuration has nothing useful to do, so
// failing loudly at start-up beats starting in an undefined state. Every
// problem is reported together, so a first run in a new environment lists all
// the missing variables at once.
func LoadSettings() Settings {
	return muzak.MustLoadConfig[Settings](muzak.EnvFile(".env"))
}
