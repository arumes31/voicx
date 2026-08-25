package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectGitStates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "VERSION"), "0.4.0\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n")
	writeTestFile(t, filepath.Join(root, ".gitignore"), "ignored/\n")
	runTestGit(t, root, "init")
	runTestGit(t, root, "config", "user.name", "Version Test")
	runTestGit(t, root, "config", "user.email", "version-test@example.invalid")
	runTestGit(t, root, "add", ".")
	runTestGit(t, root, "commit", "-m", "initial")

	clean, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty || !strings.HasPrefix(clean.Version, "0.4.0-dev+g") {
		t.Fatalf("clean metadata = %+v", clean)
	}
	archiveRoot := filepath.Join(root, "ignored", "source-archive")
	writeTestFile(t, filepath.Join(archiveRoot, "VERSION"), "0.4.0\n")
	writeTestFile(t, filepath.Join(archiveRoot, "go.mod"), "module archive\n")
	writeTestFile(t, filepath.Join(archiveRoot, "main.go"), "package main\n")
	archive, err := Detect(archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(archive.Version, "0.4.0-dev+src.h") {
		t.Fatalf("nested source archive metadata = %+v", archive)
	}
	writeTestFile(t, filepath.Join(root, "ignored", "output.bin"), "ignored")
	ignored, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Version != clean.Version || ignored.Dirty {
		t.Fatalf("ignored output changed metadata: clean=%+v ignored=%+v", clean, ignored)
	}

	writeTestFile(t, filepath.Join(root, "main.go"), "package changed\n")
	dirty, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty.Dirty || dirty.Version == clean.Version || !strings.Contains(dirty.Version, ".dirty.h") {
		t.Fatalf("dirty metadata = %+v; clean = %+v", dirty, clean)
	}
	runTestGit(t, root, "add", "main.go")
	staged, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Version != dirty.Version {
		t.Fatalf("staging changed the effective-tree version: unstaged=%q staged=%q", dirty.Version, staged.Version)
	}

	writeTestFile(t, filepath.Join(root, "new.go"), "package main\n")
	untracked, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if untracked.Version == dirty.Version {
		t.Fatal("untracked source did not change the version")
	}
	runTestGit(t, root, "add", "new.go")
	stagedUntracked, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if stagedUntracked.Version != untracked.Version {
		t.Fatalf("staging a new file changed the effective-tree version: untracked=%q staged=%q", untracked.Version, stagedUntracked.Version)
	}

	runTestGit(t, root, "restore", "--source=HEAD", "--staged", "--worktree", "main.go")
	runTestGit(t, root, "restore", "--staged", "new.go")
	if err := os.Remove(filepath.Join(root, "new.go")); err != nil {
		t.Fatal(err)
	}
	reverted, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Version != clean.Version {
		t.Fatalf("reverted version = %q, want %q", reverted.Version, clean.Version)
	}

	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	deleted, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.Dirty || deleted.Version == clean.Version {
		t.Fatalf("deleted-file metadata = %+v; clean = %+v", deleted, clean)
	}
	runTestGit(t, root, "add", "-A")
	stagedDeletion, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if stagedDeletion.Version != deleted.Version {
		t.Fatalf("staging a deletion changed the effective-tree version: unstaged=%q staged=%q", deleted.Version, stagedDeletion.Version)
	}
	runTestGit(t, root, "restore", "--source=HEAD", "--staged", "--worktree", "main.go")

	if err := os.Rename(filepath.Join(root, "main.go"), filepath.Join(root, "renamed.go")); err != nil {
		t.Fatal(err)
	}
	renamed, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "add", "-A")
	stagedRename, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if stagedRename.Version != renamed.Version {
		t.Fatalf("staging a rename changed the effective-tree version: unstaged=%q staged=%q", renamed.Version, stagedRename.Version)
	}
	runTestGit(t, root, "restore", "--source=HEAD", "--staged", "--worktree", ".")

	writeTestFile(t, filepath.Join(root, "main.go"), "package main\n\n// second commit\n")
	runTestGit(t, root, "add", "main.go")
	runTestGit(t, root, "commit", "-m", "second")
	nextCommit, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if nextCommit.Dirty || nextCommit.Version == clean.Version {
		t.Fatalf("next commit metadata = %+v; initial = %+v", nextCommit, clean)
	}

	runTestGit(t, root, "tag", "-a", "v0.4.0", "-m", "release")
	release, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if release.Version != "0.4.0" || release.Dirty {
		t.Fatalf("release metadata = %+v", release)
	}

	runTestGit(t, root, "tag", "v0.4.0-rc.1")
	if _, err := Detect(root); err == nil {
		t.Fatal("multiple matching release tags were accepted")
	}
	runTestGit(t, root, "tag", "-d", "v0.4.0-rc.1")

	runTestGit(t, root, "tag", "v0.5.0")
	if _, err := Detect(root); err == nil {
		t.Fatal("release tag with mismatched VERSION was accepted")
	}
	runTestGit(t, root, "tag", "-d", "v0.5.0")

	runTestGit(t, root, "tag", "vnot-semver")
	if _, err := Detect(root); err == nil {
		t.Fatal("invalid v-prefixed release tag was accepted")
	}
}

func runTestGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
