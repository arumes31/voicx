package version

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const gitTimeout = 30 * time.Second

// Detect calculates a deterministic version from VERSION and the repository
// state rooted at root. It does not modify tracked files.
func Detect(root string) (Metadata, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Metadata{}, fmt.Errorf("resolving repository root: %w", err)
	}
	base, err := readBaseVersion(root)
	if err != nil {
		return Metadata{}, err
	}

	runner := gitRunner{root: root}
	if _, err := runner.run("rev-parse", "--is-inside-work-tree"); err != nil {
		return detectSourceArchive(root, base)
	}
	topLevel, err := runner.text("rev-parse", "--show-toplevel")
	if err != nil || !samePath(root, strings.TrimSpace(topLevel)) {
		return detectSourceArchive(root, base)
	}

	commit, err := runner.text("rev-parse", "HEAD")
	if err != nil {
		return Metadata{}, fmt.Errorf("reading git revision: %w", err)
	}
	buildDate, err := runner.text("show", "-s", "--format=%cI", "HEAD")
	if err != nil {
		return Metadata{}, fmt.Errorf("reading git commit time: %w", err)
	}
	status, err := runner.run("status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if err != nil {
		return Metadata{}, fmt.Errorf("reading git status: %w", err)
	}
	dirty := len(status) > 0

	result := Metadata{
		Commit:    strings.TrimSpace(commit),
		BuildDate: strings.TrimSpace(buildDate),
		Dirty:     dirty,
	}
	tag, err := exactVersionTag(runner, base)
	if err != nil {
		return Metadata{}, err
	}
	if tag != "" && !dirty {
		result.Version = normalizeVersion(tag)
		return result, nil
	}

	var fingerprint string
	if dirty {
		fingerprint, err = dirtyFingerprint(root, runner)
		if err != nil {
			return Metadata{}, fmt.Errorf("fingerprinting dirty source: %w", err)
		}
	}
	result.Version = developmentVersionForBase(base, result.Commit, dirty, fingerprint)
	return result, nil
}

func detectSourceArchive(root, base string) (Metadata, error) {
	fingerprint, err := sourceFingerprint(root)
	if err != nil {
		return Metadata{}, fmt.Errorf("fingerprinting source archive: %w", err)
	}
	return Metadata{Version: base + "-dev+src.h" + shortRevision(fingerprint)}, nil
}

func samePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if strings.EqualFold(a, b) {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && strings.EqualFold(resolvedA, resolvedB)
}

func readBaseVersion(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return "", fmt.Errorf("reading VERSION: %w", err)
	}
	base := strings.TrimSpace(string(raw))
	_, valid := parseSemver(base)
	if !valid || strings.ContainsAny(base, "+-") || strings.HasPrefix(base, "v") {
		return "", fmt.Errorf("VERSION must contain MAJOR.MINOR.PATCH, got %q", base)
	}
	return base, nil
}

type gitRunner struct {
	root string
}

func (r gitRunner) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	commandArgs := append([]string{"-C", r.root, "--no-optional-locks"}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	output, err := command.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			message := strings.TrimSpace(string(exitErr.Stderr))
			if message != "" {
				return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
			}
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return output, nil
}

func (r gitRunner) text(args ...string) (string, error) {
	output, err := r.run(args...)
	return string(output), err
}

func exactVersionTag(runner gitRunner, base string) (string, error) {
	output, err := runner.text("tag", "--points-at", "HEAD", "--format=%(refname:short)")
	if err != nil {
		return "", fmt.Errorf("reading git tags: %w", err)
	}
	tags := []string{}
	for _, tag := range strings.Fields(output) {
		normalized := normalizeVersion(tag)
		if !strings.HasPrefix(tag, "v") {
			continue
		}
		if _, valid := parseSemver(normalized); !valid {
			return "", fmt.Errorf("release tag %q is not valid semantic versioning", tag)
		}
		tagBase, _ := parse(normalized)
		if tagBase != base {
			return "", fmt.Errorf("release tag %q does not match VERSION %q", tag, base)
		}
		tags = append(tags, tag)
	}
	if len(tags) > 1 {
		return "", fmt.Errorf("multiple release tags point at HEAD: %s", strings.Join(tags, ", "))
	}
	if len(tags) == 1 {
		return tags[0], nil
	}
	return "", nil
}

func dirtyFingerprint(root string, runner gitRunner) (string, error) {
	filesOutput, err := runner.run("ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", ".")
	if err != nil {
		return "", err
	}
	files := splitNull(filesOutput)
	sort.Strings(files)

	hash := sha256.New()
	for _, name := range files {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("reading %q: %w", name, err)
		}
		if err := hashPath(hash, root, name); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sourceFingerprint(root string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		name := filepath.ToSlash(relative)
		if entry.IsDir() && excludedSourceDirectory(name) {
			return filepath.SkipDir
		}
		if entry.IsDir() || excludedSourceFile(name) {
			return nil
		}
		return hashPath(hash, root, name)
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func excludedSourceDirectory(name string) bool {
	name = strings.Trim(filepath.ToSlash(name), "/")
	parts := strings.Split(name, "/")
	base := parts[len(parts)-1]
	if base == ".git" || base == ".cache" || base == "node_modules" ||
		strings.HasPrefix(base, ".test-results-full-") {
		return true
	}
	switch name {
	case "bin", "data", "dist", "graphify-out", "out", "temp",
		"client/build/bin", "client/frontend/dist",
		"client/frontend/playwright-report", "client/frontend/test-results":
		return true
	default:
		return false
	}
}

func excludedSourceFile(name string) bool {
	base := filepath.Base(name)
	return base == ".env" || strings.HasSuffix(base, ".exe") || strings.HasSuffix(base, ".test")
}

func hashPath(hash io.Writer, root, name string) error {
	cleanName := filepath.Clean(filepath.FromSlash(name))
	if cleanName == "." || filepath.IsAbs(cleanName) || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return fmt.Errorf("unsafe repository path %q", name)
	}
	path := filepath.Join(root, cleanName)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("reading %q: %w", name, err)
	}
	_, _ = io.WriteString(hash, filepath.ToSlash(cleanName))
	_, _ = io.WriteString(hash, "\x00"+info.Mode().String()+"\x00")
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("reading symlink %q: %w", name, err)
		}
		_, _ = io.WriteString(hash, target)
		_, _ = io.WriteString(hash, "\x00")
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %q: %w", name, err)
	}
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("hashing %q: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %q: %w", name, closeErr)
	}
	_, _ = io.WriteString(hash, "\x00")
	return nil
}

func splitNull(value []byte) []string {
	parts := strings.Split(string(value), "\x00")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func developmentVersionForBase(base, commit string, dirty bool, fingerprint string) string {
	metadata := "unknown"
	if commit != "" {
		metadata = "g" + shortRevision(commit)
	}
	if dirty {
		metadata += ".dirty"
		if fingerprint != "" {
			metadata += ".h" + shortRevision(fingerprint)
		}
	}
	return base + "-dev+" + metadata
}
