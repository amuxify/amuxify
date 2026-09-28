package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// citedTest matches one backticked `pkg.TestName` token in docs/safety.md,
// with or without a `/subtest` suffix, which is not checked.
var citedTest = regexp.MustCompile("`([a-z]+)\\.(Test[A-Za-z0-9_]+)(?:/[^`]*)?`")

// declaredTests parses the test files of internal/<pkg> and returns the name
// of every top-level Test function, or nil when the package has no test
// files at all.
func declaredTests(t *testing.T, dir string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil
	}
	names := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Test") {
					names[fd.Name.Name] = true
				}
			}
		}
	}
	return names
}

// TestSafetyDocTestsExist checks that every test docs/safety.md names as the
// proof of a guarantee is a real function in internal/<pkg>. A guarantee
// whose cited test does not exist is a guarantee nobody is checking, and a
// renamed or deleted test must take its citation with it.
func TestSafetyDocTestsExist(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "safety.md"))
	if err != nil {
		t.Fatal(err)
	}
	cited := map[string]map[string]bool{}
	for _, m := range citedTest.FindAllStringSubmatch(string(b), -1) {
		if cited[m[1]] == nil {
			cited[m[1]] = map[string]bool{}
		}
		cited[m[1]][m[2]] = true
	}
	if len(cited) < 5 {
		t.Fatalf("only %d packages cited in docs/safety.md; the pattern is not seeing the Test lines", len(cited))
	}
	var missing []string
	for pkg, names := range cited {
		declared := declaredTests(t, filepath.Join("..", "..", "internal", pkg))
		for name := range names {
			if !declared[name] {
				missing = append(missing, pkg+"."+name)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("cited in docs/safety.md but not declared under internal/: %s", strings.Join(missing, ", "))
	}
}
