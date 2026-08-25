package version

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadBaseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     string
		expected  string
		wantError bool
	}{
		{name: "stable version", value: "1.2.3\n", expected: "1.2.3"},
		{name: "leading v", value: "v1.2.3", wantError: true},
		{name: "prerelease", value: "1.2.3-rc.1", wantError: true},
		{name: "incomplete", value: "1.2", wantError: true},
		{name: "leading zero", value: "01.2.3", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte(test.value), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readBaseVersion(root)
			if test.wantError {
				if err == nil {
					t.Fatalf("readBaseVersion() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != test.expected {
				t.Errorf("readBaseVersion() = %q, want %q", got, test.expected)
			}
		})
	}
}

func TestSourceFingerprintTracksContentAndIgnoresOutputs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")
	first, err := sourceFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(root, "main.go"), "package changed\n")
	second, err := sourceFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("source fingerprint did not change with source content")
	}

	writeTestFile(t, filepath.Join(root, "dist", "generated.js"), "ignored")
	third, err := sourceFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if second != third {
		t.Fatal("source fingerprint changed for excluded build output")
	}

	writeTestFile(t, filepath.Join(root, "client", "build", "info.json"), "first")
	fourth, err := sourceFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if third == fourth {
		t.Fatal("source fingerprint ignored tracked build configuration")
	}

	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")
	if err := os.RemoveAll(filepath.Join(root, "client")); err != nil {
		t.Fatal(err)
	}
	reverted, err := sourceFingerprint(root)
	if err != nil {
		t.Fatal(err)
	}
	if first != reverted {
		t.Fatal("source fingerprint was not restored after reverting content")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
