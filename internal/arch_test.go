// Package internal_test enforces the dependency arrows from blueprint §12:
//
//	domain   → stdlib + domain only (the pure functional core)
//	ports    → stdlib + domain + ports
//	adapters → never app, infra, api, or compat
//	compat   → never infra, adapters, or api (translation layer over app)
//	nobody outside infra/sqlite touches the generated sqlite code
//
// This is the "CI lint enforces the arrows" from the blueprint, implemented
// as a plain Go test so it runs everywhere `go test ./...` runs, with no
// external linter needed.
package internal_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/monarr-media/monarr"

// collectImports maps each package directory (relative to the repo root,
// e.g. "internal/infra/bus") to the set of module-internal imports found in
// its non-test files. Test files may take shortcuts; production code may not.
func collectImports(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRoot(t)

	imports := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == "web" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		rel = filepath.ToSlash(rel)

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			v, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(v, module+"/") {
				imports[rel] = append(imports[rel], strings.TrimPrefix(v, module+"/"))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo: %v", err)
	}
	return imports
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This test lives in <root>/internal.
	return filepath.Dir(wd)
}

type rule struct {
	name string
	// pkg matches package dirs the rule applies to.
	pkg *regexp.Regexp
	// allowed: import is fine if it matches any of these.
	allowed []*regexp.Regexp
	// forbidden: import is a violation if it matches any of these
	// (checked when allowed is nil).
	forbidden []*regexp.Regexp
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }

func TestDependencyArrows(t *testing.T) {
	rules := []rule{
		{
			name:    "domain is pure: imports only domain",
			pkg:     re(`^internal/domain(/|$)`),
			allowed: []*regexp.Regexp{re(`^internal/domain(/|$)`)},
		},
		{
			name:    "ports import only domain and ports",
			pkg:     re(`^internal/ports(/|$)`),
			allowed: []*regexp.Regexp{re(`^internal/(domain|ports)(/|$)`)},
		},
		{
			name:      "adapters never import app, infra, api, or compat",
			pkg:       re(`^internal/adapters(/|$)`),
			forbidden: []*regexp.Regexp{re(`^internal/(app|infra|api|compat)(/|$)`)},
		},
		{
			name:      "compat never imports infra, adapters, or api",
			pkg:       re(`^internal/compat(/|$)`),
			forbidden: []*regexp.Regexp{re(`^internal/(infra|adapters|api)(/|$)`)},
		},
		{
			name:      "generated sqlite code is private to infra/sqlite",
			pkg:       re(`^(?:cmd|internal)(/|$)`),
			forbidden: []*regexp.Regexp{re(`^internal/infra/sqlite/gen(/|$)`)},
		},
	}

	imports := collectImports(t)
	for _, r := range rules {
		t.Run(r.name, func(t *testing.T) {
			for pkg, imps := range imports {
				if !r.pkg.MatchString(pkg) {
					continue
				}
				// The sqlite package itself may use its generated code.
				if r.name == "generated sqlite code is private to infra/sqlite" &&
					strings.HasPrefix(pkg, "internal/infra/sqlite") {
					continue
				}
				for _, imp := range imps {
					if r.allowed != nil {
						ok := false
						for _, a := range r.allowed {
							if a.MatchString(imp) {
								ok = true
								break
							}
						}
						if !ok {
							t.Errorf("%s imports %s/%s — not allowed by rule", pkg, module, imp)
						}
					}
					for _, f := range r.forbidden {
						if f.MatchString(imp) {
							t.Errorf("%s imports %s/%s — forbidden by rule", pkg, module, imp)
						}
					}
				}
			}
		})
	}
}

// TestDomainStdlibOnly additionally verifies domain imports nothing
// third-party (the allowed-list above only constrains module-internal
// imports).
func TestDomainStdlibOnly(t *testing.T) {
	root := repoRoot(t)
	domainDir := filepath.Join(root, "internal", "domain")
	err := filepath.WalkDir(domainDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			v, _ := strconv.Unquote(imp.Path.Value)
			// Stdlib paths have no dot in their first segment.
			first := strings.SplitN(v, "/", 2)[0]
			if strings.Contains(first, ".") && !strings.HasPrefix(v, module+"/domain") &&
				!strings.HasPrefix(v, module+"/internal/domain") {
				t.Errorf("%s imports %q — domain must be stdlib-only", path, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking domain: %v", err)
	}
}
