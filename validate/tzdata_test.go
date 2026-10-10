package validate

// The test binary carries Go's own time zone database, so the zone rules are
// tested against the same zones on every host, whether or not the host has a
// database of its own, as a container built from scratch does not.
import _ "time/tzdata"
