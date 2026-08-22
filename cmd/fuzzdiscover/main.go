// fuzzdiscover emits the repository's Go fuzz targets as a stable JSON array.
//
// It deliberately uses the Go parser instead of source-text matching so a
// valid target is not hidden by parameter names, import aliases, formatting, or
// comments. The output is consumed by .github/workflows/fuzz.yml.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type fuzzTarget struct {
	Module  string `json:"module"`
	Package string `json:"package"`
	Target  string `json:"target"`
}

type moduleRoot struct {
	dir  string
	name string
}

type parsedFile struct {
	file *ast.File
}

func main() {
	root := flag.String("root", ".", "repository root to inspect")
	flag.Parse()

	targets, err := discover(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovering fuzz targets: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(targets); err != nil {
		fmt.Fprintf(os.Stderr, "encoding fuzz targets: %v\n", err)
		os.Exit(1)
	}
}

func discover(root string) ([]fuzzTarget, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving root: %w", err)
	}
	modules, err := findModuleRoots(absRoot)
	if err != nil {
		return nil, err
	}

	byDir := make(map[string][]parsedFile)
	fset := token.NewFileSet()
	err = filepath.WalkDir(absRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != absRoot && skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		applicable, err := build.Default.MatchFile(filepath.Dir(path), entry.Name())
		if err != nil {
			return fmt.Errorf("checking build constraints for %s: %w", path, err)
		}
		if !applicable {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		byDir[filepath.Dir(path)] = append(byDir[filepath.Dir(path)], parsedFile{
			file: file,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", absRoot, err)
	}

	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	seen := make(map[fuzzTarget]bool)
	for _, dir := range dirs {
		for _, target := range discoverDir(dir, byDir[dir], modules) {
			seen[target] = true
		}
	}
	targets := make([]fuzzTarget, 0, len(seen))
	for target := range seen {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Module != targets[j].Module {
			return targets[i].Module < targets[j].Module
		}
		if targets[i].Package != targets[j].Package {
			return targets[i].Package < targets[j].Package
		}
		return targets[i].Target < targets[j].Target
	})
	return targets, nil
}

func findModuleRoots(root string) ([]moduleRoot, error) {
	modules := []moduleRoot{{dir: root, name: "."}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "go.mod" || filepath.Dir(path) == root {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		modules = append(modules, moduleRoot{dir: filepath.Dir(path), name: filepath.ToSlash(rel)})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("finding Go modules: %w", err)
	}
	sort.Slice(modules, func(i, j int) bool {
		return len(modules[i].dir) > len(modules[j].dir)
	})
	return modules, nil
}

func skipDir(name string) bool {
	switch name {
	case ".git", ".cache", "graphify-out", "node_modules", "testdata", "vendor":
		return true
	}
	return strings.HasPrefix(name, ".")
}

func discoverDir(dir string, files []parsedFile, modules []moduleRoot) []fuzzTarget {
	byPackage := make(map[string][]parsedFile)
	for _, parsed := range files {
		byPackage[parsed.file.Name.Name] = append(byPackage[parsed.file.Name.Name], parsed)
	}
	module, ok := moduleForDir(dir, modules)
	if !ok {
		return nil
	}
	pkg, err := packagePath(module, dir)
	if err != nil {
		return nil
	}

	var targets []fuzzTarget
	for _, files := range byPackage {
		for _, parsed := range files {
			for _, declaration := range parsed.file.Decls {
				fn, ok := declaration.(*ast.FuncDecl)
				if !ok || !isFuzzTarget(fn) {
					continue
				}
				targets = append(targets, fuzzTarget{Module: module.name, Package: pkg, Target: fn.Name.Name})
			}
		}
	}
	return targets
}

func moduleForDir(dir string, modules []moduleRoot) (moduleRoot, bool) {
	for _, module := range modules {
		rel, err := filepath.Rel(module.dir, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return module, true
		}
	}
	return moduleRoot{}, false
}

func packagePath(module moduleRoot, dir string) (string, error) {
	rel, err := filepath.Rel(module.dir, dir)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return ".", nil
	}
	return "./" + filepath.ToSlash(rel), nil
}

func isFuzzTarget(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || (fn.Type.Results != nil && len(fn.Type.Results.List) > 0) ||
		fn.Type.TypeParams != nil || !validFuzzName(fn.Name.Name) {
		return false
	}
	if fn.Type.Params == nil {
		return false
	}
	parameters, fuzzParameters := 0, 0
	for _, field := range fn.Type.Params.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		parameters += count
		if isFuzzParameter(field.Type) {
			fuzzParameters += count
		}
	}
	return parameters == 1 && fuzzParameters == 1
}

func validFuzzName(name string) bool {
	if !strings.HasPrefix(name, "Fuzz") {
		return false
	}
	if len(name) == len("Fuzz") {
		return true
	}
	rune, _ := utf8.DecodeRuneInString(name[len("Fuzz"):])
	return !unicode.IsLower(rune)
}

func isFuzzParameter(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	// Match cmd/go's syntactic test-function rule. Import resolution is not
	// part of target discovery: *F and *anything.F are candidates, while a
	// local alias has the wrong signature and go test reports it as such.
	expr = star.X
	switch expr := expr.(type) {
	case *ast.SelectorExpr:
		return expr.Sel.Name == "F"
	case *ast.Ident:
		return expr.Name == "F"
	default:
		return false
	}
}
