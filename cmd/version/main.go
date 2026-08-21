// Command version calculates voicx build metadata without modifying source
// files. It is shared by local builds, release CI, and container publishing.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	appversion "voicx/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "version:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("version", flag.ContinueOnError)
	flags.SetOutput(stderr)
	format := flags.String("format", "version", "output: version, runtime, json, ldflags, github, docker, or base")
	root := flags.String("root", "", "repository root (auto-detected by default)")
	check := flags.Bool("check", false, "verify all tracked version declarations")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if *format == "runtime" && !*check {
		_, err := fmt.Fprintln(stdout, appversion.String())
		return err
	}

	projectRoot, err := resolveRoot(*root)
	if err != nil {
		return err
	}
	metadata, err := appversion.Detect(projectRoot)
	if err != nil {
		return err
	}
	if *check {
		if err := checkDeclarations(projectRoot, metadata); err != nil {
			return err
		}
	}

	switch *format {
	case "version":
		_, err = fmt.Fprintln(stdout, metadata.Version)
	case "base":
		base, _ := appversion.Parse(metadata.Version)
		_, err = fmt.Fprintln(stdout, base)
	case "json":
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(metadata)
	case "ldflags":
		_, err = fmt.Fprintln(stdout, linkerFlags(metadata))
	case "runtime":
		_, err = fmt.Fprintln(stdout, appversion.String())
	case "github":
		err = writeGitHubEnvironment(stdout, metadata)
	case "docker":
		for _, argument := range dockerArguments(metadata) {
			if _, err = fmt.Fprintln(stdout, argument); err != nil {
				break
			}
	default:
		return fmt.Errorf("unknown format %q", *format)
	}
	return err
}

func resolveRoot(value string) (string, error) {
	var directory string
	var err error
	if value != "" {
		directory, err = filepath.Abs(value)
	} else {
		directory, err = os.Getwd()
	}
	if err != nil {
		return "", fmt.Errorf("resolving start directory: %w", err)
	}
	for {
		versionPath := filepath.Join(directory, "VERSION")
		modulePath := filepath.Join(directory, "go.mod")
		if fileExists(versionPath) && fileExists(modulePath) {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("could not find repository root containing VERSION and go.mod")
		}
		directory = parent
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func linkerFlags(metadata appversion.Metadata) string {
	values := []struct {
		name  string
		value string
	}{
		{name: "Version", value: metadata.Version},
		{name: "Commit", value: metadata.Commit},
		{name: "BuildDate", value: metadata.BuildDate},
		{name: "Dirty", value: strconv.FormatBool(metadata.Dirty)},
	}
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, "-X=voicx/internal/version."+value.name+"="+value.value)
	}
	return strings.Join(parts, " ")
}

func writeGitHubEnvironment(writer io.Writer, metadata appversion.Metadata) error {
	values := []struct {
		name  string
		value string
	}{
		{name: "VOICX_VERSION", value: metadata.Version},
		{name: "VOICX_COMMIT", value: metadata.Commit},
		{name: "VOICX_BUILD_DATE", value: metadata.BuildDate},
		{name: "VOICX_DIRTY", value: strconv.FormatBool(metadata.Dirty)},
		{name: "VOICX_PRERELEASE", value: strconv.FormatBool(appversion.IsPrerelease(metadata.Version))},
		{name: "VOICX_LDFLAGS", value: linkerFlags(metadata)},
	}
	for _, value := range values {
		if strings.ContainsAny(value.value, "\r\n") {
			return fmt.Errorf("%s contains a newline", value.name)
		}
		if _, err := fmt.Fprintf(writer, "%s=%s\n", value.name, value.value); err != nil {
			return err
		}
	}
	return nil
}

func dockerArguments(metadata appversion.Metadata) []string {
	values := []struct {
		name  string
		value string
	}{
		{name: "VOICX_VERSION", value: metadata.Version},
		{name: "VOICX_COMMIT", value: metadata.Commit},
		{name: "VOICX_DIRTY", value: strconv.FormatBool(metadata.Dirty)},
		{name: "VOICX_BUILD_DATE", value: metadata.BuildDate},
	}
	parts := make([]string, 0, len(values)*2)
	for _, value := range values {
		parts = append(parts, "--build-arg", value.name+"="+value.value)
	}
	return parts
}

func checkDeclarations(root string, metadata appversion.Metadata) error {
	base, _ := appversion.Parse(metadata.Version)
	expected := map[string]string{
		"client/frontend/package.json":          base,
		"client/frontend/package-lock.json":     base,
		"client/wails.json":                     base,
		"client/go.mod local voicx requirement": "v" + base,
		"internal/version default":              base,
	}

	packageVersion, err := readJSONVersion(filepath.Join(root, "client", "frontend", "package.json"))
	if err != nil {
		return err
	}
	lockVersion, err := readLockVersion(filepath.Join(root, "client", "frontend", "package-lock.json"))
	if err != nil {
		return err
	}
	wailsVersion, err := readWailsVersion(filepath.Join(root, "client", "wails.json"))
	if err != nil {
		return err
	}
	moduleVersion, err := readLocalModuleVersion(filepath.Join(root, "client", "go.mod"))
	if err != nil {
		return err
	}
	actual := map[string]string{
		"client/frontend/package.json":          packageVersion,
		"client/frontend/package-lock.json":     lockVersion,
		"client/wails.json":                     wailsVersion,
		"client/go.mod local voicx requirement": moduleVersion,
		"internal/version default":              appversion.DeclaredRelease,
	}
	problems := []string{}
	for name, wanted := range expected {
		if actual[name] != wanted {
			problems = append(problems, fmt.Sprintf("%s is %q, want %q", name, actual[name], wanted))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func readJSONVersion(path string) (string, error) {
	var document struct {
		Version string `json:"version"`
	}
	if err := decodeJSON(path, &document); err != nil {
		return "", err
	}
	return document.Version, nil
}

func readLockVersion(path string) (string, error) {
	var document struct {
		Version  string `json:"version"`
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := decodeJSON(path, &document); err != nil {
		return "", err
	}
	rootVersion := document.Packages[""].Version
	if rootVersion != document.Version {
		return "", fmt.Errorf("%s has inconsistent root versions %q and %q", path, document.Version, rootVersion)
	}
	return document.Version, nil
}

func readWailsVersion(path string) (string, error) {
	var document struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	if err := decodeJSON(path, &document); err != nil {
		return "", err
	}
	return document.Info.ProductVersion, nil
}

func decodeJSON(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decoding %s: multiple JSON values", path)
		}
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

func readLocalModuleVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	parsed, err := modfile.Parse(path, raw, nil)
	if err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	versions := []string{}
	for _, requirement := range parsed.Require {
		if requirement.Mod.Path == "voicx" {
			versions = append(versions, requirement.Mod.Version)
		}
	}
	if len(versions) != 1 {
		return "", fmt.Errorf("%s must have exactly one voicx requirement, found %d", path, len(versions))
	}
	return versions[0], nil
}
