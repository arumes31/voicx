package version

import (
	"strings"
	"testing"
)

func TestMetadataVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		metadata Metadata
		expected string
	}{
		{
			name:     "release",
			metadata: Metadata{Version: "v0.4.0"},
			expected: "0.4.0",
		},
		{
			name:     "clean development commit",
			metadata: Metadata{Version: "0.4.0-dev+gabc1234"},
			expected: "0.4.0-dev+gabc1234",
		},
		{
			name:     "dirty source fingerprint",
			metadata: Metadata{Version: "0.4.0-dev+gabc1234.dirty.hdef5678", Dirty: true},
			expected: "0.4.0-dev+gabc1234.dirty.hdef5678",
		},
		{
			name:     "obsolete zero fallback",
			metadata: Metadata{Version: "0.0.0-dev"},
			expected: "0.4.0-dev+unknown",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.metadata.String(); got != test.expected {
				t.Errorf("String() = %q, want %q", got, test.expected)
			}
			if got := test.metadata.Short(); got != test.expected {
				t.Errorf("Short() = %q, want %q", got, test.expected)
			}
		})
	}
}

func TestCurrentNeverReportsZeroVersion(t *testing.T) {
	old := [5]string{Version, Build, Commit, BuildDate, Dirty}
	t.Cleanup(func() {
		Version, Build, Commit, BuildDate, Dirty = old[0], old[1], old[2], old[3], old[4]
	})
	Version, Build, Commit, BuildDate, Dirty = "0.0.0", "", "", "", ""

	if got := String(); !strings.HasPrefix(got, "0.4.0-dev+") {
		t.Fatalf("String() = %q, want automatic 0.4.0 development version", got)
	}
}

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		base  string
		build int
	}{
		{name: "legacy numeric metadata", value: "v0.4.0+87", base: "0.4.0", build: 87},
		{name: "canonical development", value: "0.4.0-dev+gabc1234", base: "0.4.0"},
		{name: "release", value: "v1.2.3", base: "1.2.3"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			base, build := Parse(test.value)
			if base != test.base || build != test.build {
				t.Errorf("Parse(%q) = (%q, %d), want (%q, %d)", test.value, base, build, test.base, test.build)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		candidate string
		current   string
		expected  bool
	}{
		{name: "patch release", candidate: "v0.4.1", current: "0.4.0", expected: true},
		{name: "older release", candidate: "v0.4.0", current: "0.5.0"},
		{name: "stable beats development", candidate: "v0.4.0", current: "0.4.0-dev+gabc", expected: true},
		{name: "development does not beat stable", candidate: "v0.4.0-dev.2", current: "0.4.0"},
		{name: "numeric prerelease", candidate: "v0.4.0-rc.10", current: "0.4.0-rc.2", expected: true},
		{name: "build metadata has equal precedence", candidate: "v0.4.0+new", current: "0.4.0+old"},
		{name: "major release", candidate: "v1.0.0", current: "0.9.9", expected: true},
		{name: "invalid candidate fails closed", candidate: "latest", current: "0.4.0"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Compare(test.candidate, test.current); got != test.expected {
				t.Errorf("Compare(%q, %q) = %v, want %v", test.candidate, test.current, got, test.expected)
			}
		})
	}
}

func TestIsPrerelease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version  string
		expected bool
	}{
		{version: "v0.4.0-rc.1", expected: true},
		{version: "0.4.0-dev+gabc", expected: true},
		{version: "0.4.0"},
		{version: "not-semver"},
	}
	for _, test := range tests {
		if got := IsPrerelease(test.version); got != test.expected {
			t.Errorf("IsPrerelease(%q) = %v, want %v", test.version, got, test.expected)
		}
	}
}

func TestDevelopmentVersionForBase(t *testing.T) {
	t.Parallel()

	if got := developmentVersionForBase("0.4.0", "abc123456789ffff", false, ""); got != "0.4.0-dev+gabc123456789" {
		t.Errorf("clean version = %q", got)
	}
	if got := developmentVersionForBase("0.4.0", "abc123456789ffff", true, "def987654321aaaa"); got != "0.4.0-dev+gabc123456789.dirty.hdef987654321" {
		t.Errorf("dirty version = %q", got)
	}
}

func TestDevelopmentVersionUsesBinaryWhenVCSIsUnavailable(t *testing.T) {
	t.Parallel()

	got := developmentVersion("", false, "abc123456789ffff")
	if got != "0.4.0-dev+bin.habc123456789" {
		t.Errorf("developmentVersion() = %q", got)
	}
}

func TestReleaseModuleVersionRejectsGoPseudoVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		value    string
		expected string
		valid    bool
	}{
		{name: "release", value: "v0.4.0", expected: "0.4.0", valid: true},
		{name: "release candidate", value: "v0.4.0-rc.1", expected: "0.4.0-rc.1", valid: true},
		{name: "go vcs pseudo-version", value: "v0.0.0-20260808015137-996a4344dcd6+dirty"},
		{name: "development sentinel", value: "(devel)"},
		{name: "invalid", value: "latest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, valid := releaseModuleVersion(test.value)
			if got != test.expected || valid != test.valid {
				t.Errorf("releaseModuleVersion(%q) = (%q, %v), want (%q, %v)", test.value, got, valid, test.expected, test.valid)
			}
		})
	}
}

func TestReaderFingerprint(t *testing.T) {
	t.Parallel()

	first := readerFingerprint(strings.NewReader("first"))
	again := readerFingerprint(strings.NewReader("first"))
	second := readerFingerprint(strings.NewReader("second"))
	if first == "" || first != again || first == second {
		t.Fatalf("fingerprints: first=%q again=%q second=%q", first, again, second)
	}
}
