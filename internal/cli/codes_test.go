package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/exec"
)

// doctorCodes are the finding codes doctor --json emits: the upper-cased
// name of each check. They are not Go constants, so they are listed here.
var doctorCodes = []string{
	"MKVMERGE", "MKVPROPEDIT", "MKVEXTRACT", "FFMPEG", "FFPROBE", "EXIFTOOL", "CLAMSCAN",
	"LOCALE", "PROFILE", "CLAMAV", "USER", "STATE-DIR", "TMPDIR",
}

// reserved codes are declared in the code but never emitted. The docs must
// still list them, with the word "reserved" in the row.
var reserved = map[string]bool{"EXT_UNKNOWN": true, "TRACK": true, "ATTACHMENT": true}

// declaredCodes parses every non-test Go file under internal/ and collects
// each top-level constant whose name starts with Code and whose value is a
// string literal.
func declaredCodes(t *testing.T, root string) map[string][]string {
	t.Helper()
	codes := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, path, func(fi fs.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, 0)
		if err != nil {
			return err
		}
		for _, pkg := range pkgs {
			for _, f := range pkg.Files {
				for _, decl := range f.Decls {
					gd, ok := decl.(*ast.GenDecl)
					if !ok || gd.Tok != token.CONST {
						continue
					}
					for _, spec := range gd.Specs {
						vs := spec.(*ast.ValueSpec)
						for i, name := range vs.Names {
							if !strings.HasPrefix(name.Name, "Code") || i >= len(vs.Values) {
								continue
							}
							lit, ok := vs.Values[i].(*ast.BasicLit)
							if !ok || lit.Kind != token.STRING {
								continue
							}
							v, err := strconv.Unquote(lit.Value)
							if err != nil {
								return err
							}
							codes[v] = append(codes[v], pkg.Name+"."+name.Name)
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return codes
}

var backticked = regexp.MustCompile("`([A-Z][A-Z0-9_-]*)`")

// documentedCodes reads the table rows of the "Codes" section of
// docs/report.md and returns each backticked upper-case token with the row
// it appears in.
func documentedCodes(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.TrimSpace(strings.TrimPrefix(line, "## ")) == "Codes"
			continue
		}
		if !in || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 || strings.Trim(cells[1], " -") == "" || strings.TrimSpace(cells[1]) == "Code" {
			continue
		}
		for _, m := range backticked.FindAllStringSubmatch(cells[1], -1) {
			if prev, dup := rows[m[1]]; dup {
				t.Errorf("code %s is documented twice:\n%s\n%s", m[1], prev, line)
			}
			rows[m[1]] = line
		}
	}
	return rows
}

// TestCodesDocumented checks that the Codes table in docs/report.md lists
// exactly the finding codes the code base declares, plus the doctor check
// codes, and that the reserved codes are marked as such.
func TestCodesDocumented(t *testing.T) {
	declared := declaredCodes(t, filepath.Join("..", "..", "internal"))
	for _, c := range doctorCodes {
		declared[c] = append(declared[c], "doctor")
	}
	if len(declared) < 60 {
		t.Fatalf("only %d codes found under internal/; the parser is not seeing the code base", len(declared))
	}
	for _, tool := range []string{exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.FFmpeg, exec.FFprobe, exec.ExifTool, exec.ClamScan} {
		if _, ok := declared[strings.ToUpper(tool)]; !ok {
			t.Errorf("doctor code list lacks the tool check %q", strings.ToUpper(tool))
		}
	}
	for c := range reserved {
		if _, ok := declared[c]; !ok {
			t.Errorf("reserved code %s is no longer declared; nothing is deleted from the code base", c)
		}
	}

	documented := documentedCodes(t, filepath.Join("..", "..", "docs", "report.md"))
	if len(documented) == 0 {
		t.Fatal("no codes found in the Codes table of docs/report.md")
	}
	var missing, extra []string
	for c, where := range declared {
		row, ok := documented[c]
		if !ok {
			missing = append(missing, c+" ("+strings.Join(where, ", ")+")")
			continue
		}
		if reserved[c] && !strings.Contains(row, "reserved") {
			t.Errorf("reserved code %s must say so in its row: %s", c, row)
		}
		if !reserved[c] && strings.Contains(row, "reserved") {
			t.Errorf("code %s is marked reserved in the docs but is emitted by %s", c, strings.Join(where, ", "))
		}
	}
	for c := range documented {
		if _, ok := declared[c]; !ok {
			extra = append(extra, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("declared in the code but not in the Codes table of docs/report.md: %s", strings.Join(missing, ", "))
	}
	if len(extra) > 0 {
		t.Errorf("in the Codes table of docs/report.md but not declared in the code: %s", strings.Join(extra, ", "))
	}
}
