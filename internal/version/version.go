// Package version holds the build metadata embedded into voicx binaries via
// -ldflags -X at build time. Both the server and the Wails client import it
// (the client module reuses it through `replace voicx => ../`).
//
// Version string format: <base>+<build>[.<commit>][-dirty], e.g.
//
//	0.4.0+87.abc1234
//	0.4.0+87.abc1234-dirty
//
// where Build is `git rev-list --count HEAD` (auto-increments on every
// commit) and Commit is the short SHA. Binaries built without ldflags show
// "0.0.0-dev".
package version

import (
	"fmt"
	"strings"
)

var (
	// Version is the base semver (from the VERSION file at build time).
	Version = "0.0.0-dev"
	// Build is the commit count (git rev-list --count HEAD).
	Build = ""
	// Commit is the short git SHA.
	Commit = ""
	// BuildDate is the RFC 3339 build timestamp.
	BuildDate = ""
	// Dirty marks uncommitted changes at build time ("true"/"false").
	Dirty = ""
	// UpdateRepo is the GitHub "owner/repo" slug used for client
	// auto-updates. Operators set it at build time; it defaults to a
	// placeholder that yields "no update source".
	UpdateRepo = "voicx/voicx"
)

// String returns the full version string, e.g. "0.4.0+87.abc1234-dirty".
func String() string {
	s := Version
	if Build != "" {
		s += "+" + Build
	}
	if Commit != "" {
		if Build == "" {
			s += "+"
		} else {
			s += "."
		}
		s += Commit
	}
	if Dirty == "true" {
		s += "-dirty"
	}
	return s
}

// Short returns the base version plus build number, e.g. "0.4.0+87".
func Short() string {
	if Build == "" {
		return Version
	}
	return Version + "+" + Build
}

// Parse splits a version tag (with optional leading "v" and "+suffix") into
// its base semver and build number. "v0.4.0+87" -> ("0.4.0", 87).
func Parse(v string) (base string, build int) {
	v = strings.TrimPrefix(v, "v")
	base, plus, _ := strings.Cut(v, "+")
	if plus != "" {
		count, rest, _ := strings.Cut(plus, ".")
		_ = rest // commit hash suffix ignored
		fmt.Sscanf(count, "%d", &build)
	}
	return base, build
}

// Compare reports whether release tag a is newer than version b, comparing
// base semver first, then build number. It returns true when a > b.
func Compare(a, b string) bool {
	baseA, buildA := Parse(a)
	baseB, buildB := Parse(b)
	if c := compareSemver(baseA, baseB); c != 0 {
		return c > 0
	}
	return buildA > buildB
}

// compareSemver compares two dotted numeric versions: -1, 0, or 1.
func compareSemver(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			fmt.Sscanf(pa[i], "%d", &x)
		}
		if i < len(pb) {
			fmt.Sscanf(pb[i], "%d", &y)
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
