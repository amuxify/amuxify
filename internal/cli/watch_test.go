package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// once runs a watch --once with a short settle window and returns the exit
// code and both streams. The settle window is short but not zero, so the
// two passes of a --once run are exercised.
func once(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return run(t, append([]string{"watch", "--once", "--interval", "10ms", "--settle", "20ms"}, args...)...)
}

func countLines(out, prefix string) int {
	n := 0
	for _, l := range lines(out) {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

// noTemp fails when a temp file of amuxify's own shape sits anywhere under
// root (guarantee 2).
func noTemp(t *testing.T, root string) {
	t.Helper()
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".amuxify-") && strings.HasSuffix(d.Name(), ".tmp") {
			t.Errorf("temp file left: %s", p)
		}
		return nil
	})
}

// Every usage error is reported before anything is touched and before the
// first pass, with the same wording as ingest where the check is shared.
func TestWatchUsage(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
	file := write(t, filepath.Join(t.TempDir(), "file.nfo"), "nfo\n")
	link := filepath.Join(t.TempDir(), "link")
	if runtime.GOOS != "windows" {
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
	}
	noneProfile := testutil.Profile(t, "[verify]\ntier=\"none\"\n")
	missing := filepath.Join(t.TempDir(), "nonexistent")
	q := filepath.Join(t.TempDir(), "q")
	inQ := filepath.Join(q, "inside")
	write(t, filepath.Join(inQ, "a.nfo"), "nfo\n")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no directory", []string{"watch"}, "watch: a directory is required"},
		{"two directories", []string{"watch", dir, dir}, "watch: exactly one directory is watched, got 2 arguments"},
		{"missing directory", []string{"watch", missing}, "watch: lstat " + missing},
		{"a file", []string{"watch", file}, "watch: " + file + " is not a directory"},
		{"bad verify", []string{"watch", "--verify", "loose", dir}, "watch: --verify must be quick, full or none"},
		{"bad hardlinks", []string{"watch", "--hardlinks", "follow", dir}, "watch: --hardlinks must be skip, break or copy"},
		{"verify none flag", []string{"watch", "--verify", "none", dir}, "ingest: in-place writes require verification; verify tier none is refused (from --verify)"},
		{"verify none profile", []string{"--profile", noneProfile, "watch", dir}, "ingest: in-place writes require verification; verify tier none is refused (from profile " + noneProfile + ")"},
		{"zero interval", []string{"watch", "--interval", "0s", dir}, "watch: --interval must be longer than 0s"},
		{"negative settle", []string{"watch", "--settle", "-1s", dir}, "watch: --settle must not be negative"},
		{"quarantine dir as positional", []string{"watch", "--quarantine", missing}, fmt.Sprintf("watch: %q does not exist; if it was meant as the quarantine directory write --quarantine=%s", missing, missing)},
		{"watching the quarantine directory", []string{"watch", "--quarantine=" + q, q}, "watch: " + q + " is the quarantine directory"},
		{"watching inside the quarantine directory", []string{"watch", "--quarantine=" + q, inQ}, "watch: " + inQ + " lies inside the quarantine directory " + q},
		{"unknown flag", []string{"watch", "--no-such-flag", dir}, "flag provided but not defined"},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			name string
			args []string
			want string
		}{"symlink root", []string{"watch", link}, "watch: " + link + " is a symbolic link; name the directory itself"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tree(t, dir)
			code, out, errb := run(t, tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2; stdout %q stderr %q", code, out, errb)
			}
			if !strings.Contains(errb, tc.want) {
				t.Errorf("stderr %q does not contain %q", errb, tc.want)
			}
			if out != "" {
				t.Errorf("stdout %q on a usage error", out)
			}
			unchanged(t, before, tree(t, dir))
		})
	}
	t.Run("mkvpropedit missing", func(t *testing.T) {
		t.Setenv("AMUXIFY_MKVPROPEDIT", filepath.Join(t.TempDir(), "no-such-tool"))
		code, _, errb := run(t, "watch", "--once", dir)
		if code != 2 || !strings.Contains(errb, "mkvpropedit") {
			t.Errorf("exit %d stderr %q", code, errb)
		}
	})
	t.Run("help lists watch", func(t *testing.T) {
		code, out, errb := run(t, "help")
		if code != 0 || !strings.Contains(out+errb, "\n  watch     ") {
			t.Errorf("exit %d; help lacks watch:\n%s%s", code, out, errb)
		}
	})
}

// Guarantee 10: watch modifies files and refuses to run as root, with and
// without the flags that move or delete, and a dry run is allowed.
func TestWatchRefusesRoot(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	q := filepath.Join(t.TempDir(), "q")
	before := tree(t, dir)
	asUser(t, 0)
	for _, args := range [][]string{
		{"watch", "--once", dir},
		{"watch", "--once", "--quarantine=" + q, "--remove-blocked-sidecars", dir},
	} {
		code, out, errs := run(t, args...)
		if code != 2 || !strings.Contains(errs, "refusing to modify files as root") {
			t.Errorf("%v as root: code %d stderr %q", args, code, errs)
		}
		if out != "" {
			t.Errorf("%v as root: stdout %q", args, out)
		}
	}
	unchanged(t, before, tree(t, dir))
	if _, err := os.Lstat(q); err == nil {
		t.Error("quarantine directory created by a refused run")
	}
	code, out, errs := once(t, "--dry-run", dir)
	if code != 0 || !strings.Contains(out, "PASS  ") {
		t.Errorf("dry run as root: code %d\n%s%s", code, out, errs)
	}
}

// A --once run prints the start line, makes a pass, waits one settle window,
// ingests what settled, streams each verdict once in the ingest format, ends
// the pass with the count line and exits with the worst verdict.
func TestWatchOnceIngestsSettledFiles(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	url := write(t, filepath.Join(dir, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n")
	code, out, errb := once(t, "--verbose", dir)
	if code != 4 {
		t.Fatalf("exit %d, want 4\n%s%s", code, out, errb)
	}
	ls := lines(out)
	if len(ls) == 0 || ls[0] != "amuxify watch: "+dir+" every 10ms, ingesting each file after 20ms unchanged" {
		t.Errorf("start line missing or wrong:\n%s", out)
	}
	if countLines(out, "PASS  "+a) != 1 || countLines(out, "BLOCK "+url) != 1 {
		t.Errorf("each file must be reported exactly once:\n%s", out)
	}
	if !strings.Contains(out, "ROUTE") || !strings.Contains(out, "skip: sidecar") {
		t.Errorf("verbose output lacks the route line ingest prints:\n%s", out)
	}
	last := ls[len(ls)-1]
	if last != "BLOCK: 2 file(s) BLOCK=1 PASS=1" {
		t.Errorf("count line %q", last)
	}
	if errb != "" {
		t.Errorf("stderr %q", errb)
	}
	if _, err := os.Lstat(url); err != nil {
		t.Error("the blocked sidecar was removed without --remove-blocked-sidecars")
	}
	noTemp(t, dir)
	// --quiet prints nothing at all.
	_, qout, qerr := run(t, "--quiet", "watch", "--once", "--settle", "0s", dir)
	if qout != "" || qerr != "" {
		t.Errorf("--quiet wrote %q %q", qout, qerr)
	}
}

// Wiring of the shared ingest flags: a BLOCK file is moved under the
// quarantine directory in the mirrored tree ingest uses (a top-level file
// by its base name), a quarantine directory inside the watched tree is not
// entered, --remove-blocked-sidecars deletes a blocked sidecar, and a dry
// run changes nothing and creates no quarantine directory.
func TestWatchQuarantineAndSidecarWiring(t *testing.T) {
	testutil.Stubs(t)
	t.Run("named quarantine outside the tree", func(t *testing.T) {
		dir := t.TempDir()
		top := write(t, filepath.Join(dir, "empty.mkv"), "")
		nested := write(t, filepath.Join(dir, "sub", "empty.mkv"), "")
		q := filepath.Join(t.TempDir(), "q")
		code, out, errb := once(t, "--quarantine="+q, dir)
		if code != 4 {
			t.Fatalf("exit %d\n%s%s", code, out, errb)
		}
		for _, p := range []string{top, nested} {
			if _, err := os.Lstat(p); err == nil {
				t.Errorf("%s still in the tree", p)
			}
		}
		for _, rel := range []string{"empty.mkv", filepath.Join("sub", "empty.mkv")} {
			if _, err := os.Lstat(filepath.Join(q, rel)); err != nil {
				t.Errorf("%s not in quarantine: %v", rel, err)
			}
		}
		if countLines(out, "BLOCK ") != 2 || !strings.Contains(out, "QUARANTINED") {
			t.Errorf("quarantine not reported:\n%s", out)
		}
	})
	t.Run("quarantine inside the tree is not entered", func(t *testing.T) {
		dir := t.TempDir()
		q := filepath.Join(dir, "quarantine")
		planted := write(t, filepath.Join(q, "planted.mkv"), "")
		empty := write(t, filepath.Join(dir, "empty.mkv"), "")
		// The bare form resolves through the state dir; the directory
		// form is given through a symlink and a .. segment so the
		// exclusion is matched by identity, not by spelling.
		alias := filepath.Join(t.TempDir(), "alias")
		if runtime.GOOS != "windows" {
			if err := os.Symlink(dir, alias); err != nil {
				t.Fatal(err)
			}
		} else {
			alias = dir
		}
		spelled := filepath.Join(alias, "sub", "..", "quarantine")
		code, out, errb := once(t, "--quarantine="+spelled, dir)
		if code != 4 {
			t.Fatalf("exit %d\n%s%s", code, out, errb)
		}
		if strings.Contains(out, planted) {
			t.Errorf("the quarantine directory was entered:\n%s", out)
		}
		if _, err := os.Lstat(planted); err != nil {
			t.Errorf("planted file moved: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(q, "quarantine")); err == nil {
			t.Error("quarantine nested one level deeper")
		}
		if _, err := os.Lstat(filepath.Join(q, "empty.mkv")); err != nil {
			t.Errorf("empty.mkv not quarantined: %v", err)
		}
		if _, err := os.Lstat(empty); err == nil {
			t.Error("empty.mkv still in the tree")
		}
	})
	t.Run("blocked sidecar removed with the flag", func(t *testing.T) {
		dir := t.TempDir()
		url := write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		nfo := write(t, filepath.Join(dir, "x.nfo"), "notes\n")
		code, out, _ := once(t, "--verbose", "--remove-blocked-sidecars", dir)
		if code != 4 || !strings.Contains(out, "SIDECAR_REMOVED") {
			t.Errorf("exit %d\n%s", code, out)
		}
		if _, err := os.Lstat(url); err == nil {
			t.Error("x.url still exists")
		}
		if _, err := os.Lstat(nfo); err != nil {
			t.Error("x.nfo removed")
		}
	})
	t.Run("dry run changes nothing", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		write(t, filepath.Join(dir, "empty.mkv"), "")
		state := t.TempDir()
		before := tree(t, dir)
		code, out, _ := run(t, "--dry-run", "--verbose", "--state-dir", state, "watch", "--once", "--settle", "0s", "--remove-blocked-sidecars", "--quarantine", dir)
		if code != 4 || !strings.Contains(out, "DRY_RUN") {
			t.Errorf("exit %d\n%s", code, out)
		}
		unchanged(t, before, tree(t, dir))
		if _, err := os.Lstat(filepath.Join(state, "quarantine")); err == nil {
			t.Error("dry run created the quarantine directory")
		}
	})
}

// A file that is still being written when the settle window ends is not
// ingested by the --once run; only the file that stayed unchanged is.
func TestWatchGrowingFileIsNotIngested(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	still := write(t, filepath.Join(dir, "still.nfo"), "nfo\n")
	growing := write(t, filepath.Join(dir, "growing.nfo"), "nfo\n")
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Keep the file changing until the command has returned, whatever
		// the command's own timing: every 60ms a new size and a new
		// modification time, so no pass ever sees it unchanged for the
		// settle window.
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(60 * time.Millisecond):
			}
			f, err := os.OpenFile(growing, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return
			}
			f.WriteString("more\n")
			f.Close()
			later := time.Now().Add(time.Duration(i) * time.Second)
			os.Chtimes(growing, later, later)
		}
	}()
	code, out, errb := run(t, "watch", "--once", "--interval", "10ms", "--settle", "400ms", dir)
	close(stop)
	<-done
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if countLines(out, "PASS  "+still) != 1 {
		t.Errorf("the settled file was not ingested once:\n%s", out)
	}
	if strings.Contains(out, growing) {
		t.Errorf("the growing file was ingested:\n%s", out)
	}
}

// Guarantee 3: a symbolic link in the watched directory is skipped, never
// followed and named once on stderr rather than in the report; its target
// is never ingested.
func TestWatchSymlinkIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	testutil.Stubs(t)
	dir := t.TempDir()
	target := write(t, filepath.Join(t.TempDir(), "target.url"), "[InternetShortcut]\nURL=http://x\n")
	link := filepath.Join(dir, "link.url")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	code, out, errb := once(t, "--remove-blocked-sidecars", dir)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if countLines(out, "PASS  "+a) != 1 || strings.Contains(out, link) {
		t.Errorf("stdout:\n%s", out)
	}
	if countLines(errb, "amuxify watch: skipping "+link+": a symbolic link") != 1 {
		t.Errorf("stderr:\n%s", errb)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("the target of the link was removed: %v", err)
	}
}

// Under --json stdout carries one complete report document for the pass that
// ingested something, on a single line, and the operator's lines go to
// stderr; a pass that ingested nothing writes nothing. The property across
// several passes is proved by TestWatchJSONWritesOneDocumentPerPass.
func TestWatchJSONIsOneDocumentPerLine(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	url := write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
	code, out, errb := run(t, "--json", "watch", "--once", "--interval", "10ms", "--settle", "20ms", dir)
	if code != 4 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if !strings.HasPrefix(errb, "amuxify watch: "+dir+" every") {
		t.Errorf("start line not on stderr: %q", errb)
	}
	ls := lines(out)
	if len(ls) != 1 {
		t.Fatalf("want exactly one line on stdout, got %d:\n%s", len(ls), out)
	}
	doc := decodeReport(t, []byte(ls[0]))
	if doc["schema"] != report.SchemaID || doc["command"] != "ingest" {
		t.Errorf("schema %v command %v", doc["schema"], doc["command"])
	}
	files, _ := doc["files"].([]interface{})
	if len(files) != 2 {
		t.Errorf("files %v", files)
	}
	paths := map[string]bool{}
	for _, f := range files {
		m := f.(map[string]interface{})
		paths[m["path"].(string)] = true
	}
	if !paths[a] || !paths[url] {
		t.Errorf("paths %v", paths)
	}
	if doc["verdict"] != "BLOCK" {
		t.Errorf("verdict %v", doc["verdict"])
	}
	var nulls []string
	walkNulls("", doc, &nulls)
	if len(nulls) > 0 {
		t.Errorf("null values: %v", nulls)
	}
}

// Hostile names go through the same sanitiser as ingest: a bidi override, a
// zero-width space, shell metacharacters, an escape sequence and a newline
// in a name cannot forge or split a verdict line, on stdout or on stderr,
// while the JSON document keeps the raw name.
func TestWatchEscapesHostileNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control characters are not allowed in file names on windows")
	}
	testutil.Stubs(t)
	dir := t.TempDir()
	names := []string{
		"movie\u202evkm.nfo",
		"zero\u200bwidth.nfo",
		"$(touch pwned); `id` | rm -rf ~ ; &.nfo",
		"\x1b[2K\rPASS  forged\nBLOCK forged.nfo",
	}
	var paths []string
	for _, n := range names {
		paths = append(paths, write(t, filepath.Join(dir, n), "nfo\n"))
	}
	link := filepath.Join(dir, "\u202elink\nPASS  forged.nfo")
	if err := os.Symlink(paths[1], link); err != nil {
		t.Fatal(err)
	}
	forged := regexp.MustCompile(`^(PASS|BLOCK) +forged`)
	code, out, errb := once(t, dir)
	if code != 4 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	for _, l := range append(lines(out), lines(errb)...) {
		for _, r := range l {
			if r < 0x20 && r != '\t' || r == 0x7f || r == 0x202e || r == 0x200b {
				t.Errorf("raw %U in %q", r, l)
			}
		}
		for _, part := range strings.Split(l, "\r") {
			if forged.MatchString(part) {
				t.Errorf("forged verdict line %q", l)
			}
		}
	}
	for _, p := range paths {
		if !strings.Contains(out, " "+report.Sanitize(p)+"\n") {
			t.Errorf("no escaped line for %q:\n%s", p, out)
		}
	}
	if !strings.Contains(errb, "skipping "+report.Sanitize(link)+": a symbolic link") {
		t.Errorf("the link notice is not escaped:\n%s", errb)
	}
	if _, err := os.Lstat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("a name was executed")
	}
	_, jout, _ := run(t, "--json", "watch", "--once", "--settle", "0s", dir)
	doc := decodeReport(t, []byte(strings.TrimSpace(jout)))
	files, _ := doc["files"].([]interface{})
	raw := map[string]bool{}
	for _, f := range files {
		raw[f.(map[string]interface{})["path"].(string)] = true
	}
	for _, p := range paths {
		if !raw[p] {
			t.Errorf("JSON lacks the raw path %q", p)
		}
	}
}

// An unreadable subdirectory raises the pass to FAIL once, as ingest does,
// and the exit code of a --once run reflects it.
func TestWatchUnreadableDirectoryFailsOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	testutil.Stubs(t)
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	write(t, filepath.Join(locked, "hidden.nfo"), "nfo\n")
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	code, out, errb := once(t, dir)
	if code != 3 {
		t.Fatalf("exit %d, want 3\n%s%s", code, out, errb)
	}
	if countLines(out, "ERROR cannot read "+locked) != 1 {
		t.Errorf("the unreadable directory must be reported once across the two passes:\n%s", out)
	}
	if countLines(out, "PASS  "+a) != 1 {
		t.Errorf("the readable file was not ingested:\n%s", out)
	}
}
