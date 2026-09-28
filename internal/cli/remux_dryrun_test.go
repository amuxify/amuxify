package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/testutil"
)

// remuxFile is the part of one file entry of an amuxify.report/1 document
// these tests read.
type remuxFile struct {
	Path     string `json:"path"`
	Verdict  string `json:"verdict"`
	Output   string `json:"output"`
	Findings []struct {
		Code     string `json:"code"`
		Severity string `json:"severity"`
		Message  string `json:"message"`
	} `json:"findings"`
}

func (f remuxFile) finding(code string) (severity, message string, ok bool) {
	for _, fd := range f.Findings {
		if fd.Code == code {
			return fd.Severity, fd.Message, true
		}
	}
	return "", "", false
}

func parseRemuxDoc(t *testing.T, out string) []remuxFile {
	t.Helper()
	var doc struct {
		Command string      `json:"command"`
		Files   []remuxFile `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json output is not a document: %v\n%s", err, out)
	}
	if doc.Command != "remux" {
		t.Fatalf("json command %q", doc.Command)
	}
	return doc.Files
}

// The remux command's dry run predicts the OUTPUT_EXISTS collision two
// files of one run produce when they rebuild to one destination, ep.mov
// and ep.webm both becoming ep.mkv, for --output and for --in-place, over
// a directory and over the two files named separately on the command
// line. The exit code is the FAIL the live run gives and nothing on disk
// changes.
func TestRemuxDryRunPredictsDestinationCollision(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	prepare := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		for name, as := range map[string]string{"sample.mov": "ep.mov", "sample.webm": "ep.webm"} {
			if err := os.Rename(testutil.Copy(t, name), filepath.Join(dir, as)); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for _, tc := range []struct {
		name    string
		inPlace bool
		asFiles bool
	}{
		{"output over a directory", false, false},
		{"output over two files", false, true},
		{"in place over a directory", true, false},
		{"in place over two files", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := prepare(t)
			outRoot := filepath.Join(t.TempDir(), "out")
			args := []string{"--dry-run", "--json", "remux"}
			dest := filepath.Join(outRoot, "ep.mkv")
			if tc.inPlace {
				args = append(args, "--in-place")
				dest = filepath.Join(dir, "ep.mkv")
			} else {
				args = append(args, "--output", outRoot)
			}
			if tc.asFiles {
				args = append(args, filepath.Join(dir, "ep.mov"), filepath.Join(dir, "ep.webm"))
			} else {
				args = append(args, dir)
			}
			before := tree(t, dir)
			code, out, errb := run(t, args...)
			if code != 3 {
				t.Errorf("exit %d, want 3 (FAIL from the collision):\n%s%s", code, out, errb)
			}
			unchanged(t, before, tree(t, dir))
			if _, err := os.Lstat(outRoot); err == nil {
				t.Error("dry run created the output root")
			}
			files := parseRemuxDoc(t, out)
			if len(files) != 2 {
				t.Fatalf("files %d:\n%s", len(files), out)
			}
			mov, webm := files[0], files[1]
			if filepath.Base(mov.Path) != "ep.mov" || filepath.Base(webm.Path) != "ep.webm" {
				t.Fatalf("file order %s, %s", mov.Path, webm.Path)
			}
			if _, _, ok := mov.finding("DRY_RUN"); !ok || mov.Output != dest {
				t.Errorf("ep.mov: %+v", mov)
			}
			if _, _, ok := mov.finding("OUTPUT_EXISTS"); ok {
				t.Errorf("ep.mov collides with itself: %+v", mov)
			}
			sev, msg, ok := webm.finding("OUTPUT_EXISTS")
			if !ok || sev != "FAIL" || msg != dest+" already exists" || webm.Verdict != "FAIL" || webm.Output != "" {
				t.Errorf("ep.webm: %+v", webm)
			}
			if _, _, ok := webm.finding("DRY_RUN"); ok {
				t.Errorf("ep.webm is planned although its destination is taken: %+v", webm)
			}
		})
	}
}

// Guarantee 3 for the output root the remux command names itself: with no
// --output, the run writes into <root>__remuxed, a sibling of the input
// directory that anyone with write access to the parent can plant a
// symlink at before the run starts. The command must refuse that root as a
// run-level error, exit with FAIL, write nothing through the link, leave
// the link in place and leave the input tree alone; the same link named
// with --output stays the user's own choice and is followed. The ingest
// and watch commands always rebuild in place, so no defaulted root exists
// for them; the per-file form of the same check, for a caller that hands
// a scanned result to the remuxer without going through RemuxPath, is
// proven in the remux package.
func TestDefaultOutputRootRefusesPlantedSymlink(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	plant := func(t *testing.T) (root, elsewhere string) {
		t.Helper()
		parent := t.TempDir()
		root = filepath.Join(parent, "in")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(testutil.Copy(t, "sample.mov"), filepath.Join(root, "ep.mov")); err != nil {
			t.Fatal(err)
		}
		elsewhere = t.TempDir()
		if err := os.Symlink(elsewhere, root+"__remuxed"); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return root, elsewhere
	}
	untouched := func(t *testing.T, root, elsewhere string, before map[string]string) {
		t.Helper()
		unchanged(t, before, tree(t, root))
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Errorf("written through the planted link: %v", entries)
		}
		if fi, err := os.Lstat(root + "__remuxed"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("planted symlink was replaced or removed: %v", err)
		}
	}

	t.Run("remux without --output", func(t *testing.T) {
		root, elsewhere := plant(t)
		before := tree(t, root)
		code, out, errb := run(t, "--json", "remux", root)
		if code != 3 {
			t.Errorf("exit %d, want 3 (FAIL from the refused root):\n%s%s", code, out, errb)
		}
		var doc struct {
			Errors []string `json:"errors"`
			Files  []remuxFile
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("--json output is not a document: %v\n%s", err, out)
		}
		if len(doc.Errors) != 1 || !strings.Contains(doc.Errors[0], "symlink") || !strings.Contains(doc.Errors[0], root+"__remuxed") {
			t.Errorf("run-level errors %q do not name the symlink at the output root", doc.Errors)
		}
		if len(doc.Files) != 0 {
			t.Errorf("files were processed before the output root was refused: %+v", doc.Files)
		}
		untouched(t, root, elsewhere, before)
	})

	t.Run("remux with --output naming the link", func(t *testing.T) {
		root, elsewhere := plant(t)
		code, out, errb := run(t, "--json", "remux", "--output", root+"__remuxed", root)
		if code != 0 {
			t.Fatalf("exit %d, want 0:\n%s%s", code, out, errb)
		}
		files := parseRemuxDoc(t, out)
		if len(files) != 1 || files[0].Verdict != "PASS" || files[0].Output != filepath.Join(root+"__remuxed", "ep.mkv") {
			t.Fatalf("explicit --output refused: %+v", files)
		}
		if _, err := os.Lstat(filepath.Join(elsewhere, "ep.mkv")); err != nil {
			t.Errorf("output not placed where --output pointed: %v", err)
		}
	})

}
