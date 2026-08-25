// Package version owns the version displayed by voicx binaries.
//
// Release tags provide stable semantic versions. Development builds add the
// Git revision and (when needed) a source fingerprint. Linker
// flags may provide richer metadata, but plain `go build` and `wails build`
// still fall back to the VCS information embedded by the Go toolchain.
package version

// DeclaredRelease is the compile-time release line used by unstamped builds.
// The repository check keeps it synchronized with VERSION.
const DeclaredRelease = "0.4.0"

const defaultVersion = DeclaredRelease + "-dev"

var (
	// Version is the semantic release line. Untagged builds use a -dev suffix.
	Version = defaultVersion
	// Build is retained as diagnostic metadata for older build scripts.
	Build = ""
	// Commit is the Git revision.
	Commit = ""
	// BuildDate is the RFC 3339 commit or build timestamp.
	BuildDate = ""
	// Dirty marks a build made from modified or untracked files.
	Dirty = ""
	// UpdateRepo is the GitHub "owner/repo" slug used for client
	// auto-updates. Operators set it at build time; it defaults to a
	// placeholder that yields "no update source".
	UpdateRepo = "voicx/voicx"
	// UpdatePublicKeys contains one or more comma-separated, base64-encoded
	// Ed25519 public keys trusted to authenticate client update manifests.
	// It deliberately defaults to empty so non-release builds fail closed.
	UpdatePublicKeys = ""
)

// String returns the complete semantic build identity.
func String() string {
	return Current().String()
}

// Short returns the canonical semantic build identity used in the main UI and
// update comparisons.
func Short() string {
	return Current().Short()
}

// Release returns only the stable numeric release line.
func Release() string {
	base, _ := Parse(Current().Version)
	return base
}

// Parse splits a version into its stable numeric base and legacy numeric build
// metadata. For example, "v0.4.0+87" returns ("0.4.0", 87).
func Parse(value string) (base string, build int) {
	return parse(value)
}

// Compare reports whether release a is newer than version b. It follows SemVer
// precedence, so build metadata does not affect ordering.
func Compare(a, b string) bool {
	return compareVersions(a, b) > 0
}

// IsPrerelease reports whether value is a valid semantic prerelease.
func IsPrerelease(value string) bool {
	parsed, valid := parseSemver(value)
	return valid && len(parsed.prerelease) > 0
}
