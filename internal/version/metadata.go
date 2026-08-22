package version

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// Metadata is one complete, format-independent build identity.
type Metadata struct {
	Version   string `json:"version"`
	Build     string `json:"build,omitempty"`
	Commit    string `json:"commit,omitempty"`
	BuildDate string `json:"build_date,omitempty"`
	Dirty     bool   `json:"dirty"`
}

// Current returns the metadata embedded in the running binary. Missing linker
// values are filled from Go's VCS build settings, so direct builds are never
// reported as 0.0.0.
func Current() Metadata {
	result := Metadata{
		Version:   normalizeVersion(Version),
		Build:     Build,
		Commit:    Commit,
		BuildDate: BuildDate,
		Dirty:     Dirty == "true",
	}
	fallback := vcsFallback()
	usesFallbackVersion := result.Version == "" ||
		result.Version == defaultVersion ||
		strings.HasPrefix(result.Version, "0.0.0")
	if usesFallbackVersion {
		result.Version = fallback.Version
	}
	if result.Commit == "" {
		result.Commit = fallback.Commit
	}
	if result.BuildDate == "" {
		result.BuildDate = fallback.BuildDate
	}
	if Dirty == "" {
		result.Dirty = fallback.Dirty
	}
	return result
}

// String returns the canonical semantic version.
func (m Metadata) String() string {
	version := normalizeVersion(m.Version)
	if version == "" || strings.HasPrefix(version, "0.0.0") {
		return defaultVersion + "+unknown"
	}
	return version
}

// Short returns the same canonical identity. It remains as a named method for
// compatibility with the existing client API.
func (m Metadata) Short() string {
	return m.String()
}

func parse(value string) (base string, build int) {
	value = normalizeVersion(value)
	main, metadata, _ := strings.Cut(value, "+")
	base, _, _ = strings.Cut(main, "-")
	return base, legacyBuild(metadata)
}

func compareVersions(a, b string) int {
	parsedA, okA := parseSemver(a)
	parsedB, okB := parseSemver(b)
	if !okA || !okB {
		return 0
	}
	for i := range parsedA.numbers {
		if parsedA.numbers[i] != parsedB.numbers[i] {
			if parsedA.numbers[i] > parsedB.numbers[i] {
				return 1
			}
			return -1
		}
	}
	return comparePrerelease(parsedA.prerelease, parsedB.prerelease)
}

type parsedVersion struct {
	numbers    [3]int
	prerelease []string
}

func parseSemver(value string) (parsedVersion, bool) {
	value = normalizeVersion(value)
	main, metadata, hasMetadata := strings.Cut(value, "+")
	core, prerelease, hasPrerelease := strings.Cut(main, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsedVersion{}, false
	}

	result := parsedVersion{}
	for i, part := range parts {
		if !validNumericIdentifier(part) {
			return parsedVersion{}, false
		}
		number, err := strconv.Atoi(part)
		if err != nil {
			return parsedVersion{}, false
		}
		result.numbers[i] = number
	}
	if hasPrerelease {
		result.prerelease = strings.Split(prerelease, ".")
		if !validIdentifiers(result.prerelease, true) {
			return parsedVersion{}, false
		}
	}
	if hasMetadata && !validIdentifiers(strings.Split(metadata, "."), false) {
		return parsedVersion{}, false
	}
	return result, true
}

func validNumericIdentifier(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validIdentifiers(values []string, rejectNumericLeadingZero bool) bool {
	for _, value := range values {
		if value == "" {
			return false
		}
		isNumeric := true
		for _, r := range value {
			isDigit := r >= '0' && r <= '9'
			isLetter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
			if !isDigit && !isLetter && r != '-' {
				return false
			}
			isNumeric = isNumeric && isDigit
		}
		if rejectNumericLeadingZero && isNumeric && len(value) > 1 && value[0] == '0' {
			return false
		}
	}
	return true
}

func comparePrerelease(a, b []string) int {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	if len(a) == 0 {
		return 1
	}
	if len(b) == 0 {
		return -1
	}

	for i := 0; i < min(len(a), len(b)); i++ {
		if a[i] == b[i] {
			continue
		}
		numberA, errA := strconv.Atoi(a[i])
		numberB, errB := strconv.Atoi(b[i])
		switch {
		case errA == nil && errB == nil:
			return compareInt(numberA, numberB)
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			return strings.Compare(a[i], b[i])
		}
	}
	return compareInt(len(a), len(b))
}

func compareInt(a, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}

func legacyBuild(metadata string) int {
	parts := strings.Split(metadata, ".")
	if len(parts) == 0 {
		return 0
	}
	build, _ := strconv.Atoi(parts[0])
	return build
}

func normalizeVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

var fallback = sync.OnceValue(readVCSFallback)

func vcsFallback() Metadata {
	return fallback()
}

func readVCSFallback() Metadata {
	result := Metadata{Version: defaultVersion + "+unknown"}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return result
	}
	moduleVersion, hasReleaseModuleVersion := releaseModuleVersion(info.Main.Version)
	if hasReleaseModuleVersion {
		result.Version = moduleVersion
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			result.Commit = shortRevision(setting.Value)
		case "vcs.time":
			result.BuildDate = setting.Value
		case "vcs.modified":
			result.Dirty = setting.Value == "true"
		}
	}
	if !hasReleaseModuleVersion {
		result.Version = developmentVersion(result.Commit, result.Dirty, executableFingerprint())
	}
	return result
}

func releaseModuleVersion(value string) (string, bool) {
	value = normalizeVersion(value)
	if value == "" || value == "(devel)" {
		return "", false
	}
	if _, valid := parseSemver(value); !valid {
		return "", false
	}
	base, _ := parse(value)
	if base == "0.0.0" {
		return "", false
	}
	return value, true
}

func developmentVersion(commit string, dirty bool, fingerprint string) string {
	metadata := "unknown"
	if commit != "" {
		metadata = "g" + shortRevision(commit)
	} else if fingerprint != "" {
		metadata = "bin.h" + shortRevision(fingerprint)
	}
	if dirty {
		metadata += ".dirty"
		if commit != "" && fingerprint != "" {
			metadata += ".h" + shortRevision(fingerprint)
		}
	}
	return defaultVersion + "+" + metadata
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}

func executableFingerprint() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	return readerFingerprint(file)
}

func readerFingerprint(reader io.Reader) string {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))[:12]
}
