package main

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// frameworkModule is the module this command belongs to, and the one a new
// project requires.
const frameworkModule = "muzak.dev/framework"

// releaseVersion is the framework release this source is. A project muzak new
// creates requires it whenever the binary records no release of its own. The
// release that adds a version to CHANGELOG.md changes it too, and a test
// fails when it falls behind the newest one there.
const releaseVersion = "v0.3.1"

// frameworkVersion returns the version a new project requires: the release
// the go command recorded when it built this binary, as
// "go install muzak.dev/framework/cmd/muzak@v0.3.1" does, and releaseVersion
// for any other build. A local build records "(devel)", or a pseudo-version
// naming a commit that may never have been pushed, or one marked +dirty, and
// a go.mod requiring any of those would not resolve for anyone else.
func frameworkVersion(info *debug.BuildInfo, ok bool) string {
	if ok && info.Main.Path == frameworkModule && isReleaseVersion(info.Main.Version) {
		return info.Main.Version
	}
	return releaseVersion
}

// isReleaseVersion reports whether v is a release, vMAJOR.MINOR.PATCH, with
// no pre-release or build suffix and no leading zeros.
func isReleaseVersion(v string) bool {
	numbers, ok := strings.CutPrefix(v, "v")
	if !ok {
		return false
	}
	parts := strings.Split(numbers, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 9 || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for i := range len(part) {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return true
}

// versionCmd prints the version.
var versionCmd = &command{
	name:    "version",
	summary: "print the version of muzak and of the framework it scaffolds",
	about: `Version prints the framework version this muzak belongs to, which is the
version a project muzak new creates requires, and the Go toolchain and platform
it was built for.`,
	make: func() runner { return &versionRunner{} },
}

type versionRunner struct{}

func (*versionRunner) flags(*flag.FlagSet) {}

func (*versionRunner) run(_ context.Context, c *console, args, passthrough []string) error {
	if err := noArguments(versionCmd, args, passthrough); err != nil {
		return err
	}
	_, err := fmt.Fprintf(c.stdout, "muzak %s (%s %s/%s)\n", c.version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return err
}
