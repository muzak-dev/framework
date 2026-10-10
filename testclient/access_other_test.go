//go:build !unix && !windows

package testclient_test

import "testing"

// restrictionsBind reports whether a directory's permissions bind the user
// running the tests, which these systems give no way to arrange.
const restrictionsBind = false

// denyCreate does nothing; see restrictionsBind.
func denyCreate(*testing.T, string) {}
