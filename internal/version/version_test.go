package version

import "testing"

func TestString(t *testing.T) {
	old := [6]string{Version, Build, Commit, BuildDate, Dirty, UpdateRepo}
	defer func() {
		Version, Build, Commit, BuildDate, Dirty, UpdateRepo = old[0], old[1], old[2], old[3], old[4], old[5]
	}()

	Version, Build, Commit, Dirty = "0.4.0", "87", "abc1234", ""
	if got := String(); got != "0.4.0+87.abc1234" {
		t.Errorf("String() = %q", got)
	}
	if got := Short(); got != "0.4.0+87" {
		t.Errorf("Short() = %q", got)
	}
	Dirty = "true"
	if got := String(); got != "0.4.0+87.abc1234-dirty" {
		t.Errorf("String() dirty = %q", got)
	}

	Version, Build, Commit, Dirty = "0.0.0-dev", "", "", ""
	if got := String(); got != "0.0.0-dev" {
		t.Errorf("String() default = %q", got)
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		v     string
		base  string
		build int
	}{
		{"v0.4.0+87", "0.4.0", 87},
		{"0.4.0+87.abc1234", "0.4.0", 87},
		{"v1.2.3", "1.2.3", 0},
		{"0.4.0", "0.4.0", 0},
	}
	for _, tc := range cases {
		base, build := Parse(tc.v)
		if base != tc.base || build != tc.build {
			t.Errorf("Parse(%q) = (%q, %d), want (%q, %d)", tc.v, base, build, tc.base, tc.build)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.4.1", "0.4.0", true},
		{"v0.4.0", "0.5.0", false},
		{"v0.4.0+88", "0.4.0+87", true},
		{"v0.4.0+87", "0.4.0+87", false},
		{"v0.4.0+87.abc1234", "0.4.0+87.def5678", false}, // equal: not newer
		{"v1.0.0", "0.9.9", true},
		{"0.4.0+2", "0.4.0+10", false},
	}
	for _, tc := range cases {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
