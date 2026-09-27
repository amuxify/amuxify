package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// tree records name, size, mtime and mode of everything under root.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			out[p] = "dir"
			return nil
		}
		out[p] = fmt.Sprintf("%d %d %o", fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func unchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed: %q -> %q", p, v, after[p])
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s appeared", p)
		}
	}
}

func TestQuarantineFlagForms(t *testing.T) {
	var q quarantineFlag
	if !q.IsBoolFlag() || q.String() != "" || q.resolve("/s") != "" {
		t.Errorf("zero value: %q %q", q.String(), q.resolve("/s"))
	}
	for _, tc := range []struct {
		arg, str, resolved string
		set                bool
	}{
		{"", "state-dir", filepath.Join("/s", "quarantine"), true},
		{"true", "state-dir", filepath.Join("/s", "quarantine"), true},
		{"/q", "/q", "/q", true},
		{"off", "", "", false},
		{"false", "", "", false},
		{"0", "", "", false},
		{"relative/dir", "relative/dir", "relative/dir", true},
		{"--verify", "--verify", "--verify", true},
	} {
		var q quarantineFlag
		if err := q.Set(tc.arg); err != nil {
			t.Fatalf("Set(%q): %v", tc.arg, err)
		}
		if q.set != tc.set || q.String() != tc.str || q.resolve("/s") != tc.resolved {
			t.Errorf("Set(%q): set=%v String=%q resolve=%q", tc.arg, q.set, q.String(), q.resolve("/s"))
		}
	}
	// Setting off after a directory clears the directory too.
	var q2 quarantineFlag
	q2.Set("/q")
	q2.Set("off")
	if q2.set || q2.dir != "" {
		t.Errorf("off did not clear: %+v", q2)
	}
}

// Every usage error is reported before anything is touched, even with the
// destructive flags set.
func TestIngestUsage(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
	write(t, filepath.Join(dir, "notes.nfo"), "notes\n")
	noneProfile := testutil.Profile(t, "[verify]\ntier=\"none\"\n")
	missing := filepath.Join(t.TempDir(), "nonexistent")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no path", []string{"ingest", "--remove-blocked-sidecars"}, "ingest: at least one path is required"},
		{"bad verify", []string{"ingest", "--verify", "loose", "--remove-blocked-sidecars", dir}, "ingest: --verify must be quick, full or none"},
		{"bad hardlinks", []string{"ingest", "--hardlinks", "follow", "--remove-blocked-sidecars", dir}, "ingest: --hardlinks must be skip, break or copy"},
		{"verify none flag", []string{"ingest", "--verify", "none", "--remove-blocked-sidecars", dir}, "ingest: in-place writes require verification; verify tier none is refused (from --verify)"},
		{"verify none profile", []string{"--profile", noneProfile, "ingest", "--remove-blocked-sidecars", dir}, "ingest: in-place writes require verification; verify tier none is refused (from profile " + noneProfile + ")"},
		{"quarantine dir as positional", []string{"ingest", "--quarantine", missing, dir}, fmt.Sprintf("ingest: %q does not exist; if it was meant as the quarantine directory write --quarantine=%s", missing, missing)},
		{"hook without its environment", []string{"hook", "sabnzbd"}, "hook sabnzbd: not started by SABnzbd"},
		{"hook without an adapter", []string{"hook"}, "hook: adapter required"},
		{"unknown flag", []string{"ingest", "--no-such-flag", dir}, "flag provided but not defined"},
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
		before := tree(t, dir)
		code, _, errb := run(t, "ingest", "--remove-blocked-sidecars", dir)
		if code != 2 || !strings.Contains(errb, "mkvpropedit") {
			t.Errorf("exit %d stderr %q", code, errb)
		}
		unchanged(t, before, tree(t, dir))
	})
	t.Run("help lists ingest and hook", func(t *testing.T) {
		code, out, errb := run(t, "help")
		if code != 0 {
			t.Errorf("exit %d", code)
		}
		out += errb
		for _, want := range []string{"\n  ingest    ", "\n  hook      "} {
			if !strings.Contains(out, want) {
				t.Errorf("help lacks %q:\n%s", want, out)
			}
		}
	})
}

// Per-file streaming: a file's line is written before the next file is
// opened, and the tail comes last.
func TestIngestStreamsBeforeTail(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.txt"), "a\n")
	b := write(t, filepath.Join(dir, "b.txt"), "b\n")
	w := &triggerWriter{marker: "PASS  " + a + "\n", victim: b}
	var errb bytes.Buffer
	code := Main([]string{"ingest", "--dry-run", dir}, w, &errb)
	out := w.buf.String()
	if !w.fired {
		t.Fatalf("marker line never streamed; output:\n%s", out)
	}
	if w.err != nil {
		t.Fatalf("remove: %v", w.err)
	}
	if code != 3 {
		t.Fatalf("exit %d, want 3 (FAIL) for the vanished file; output:\n%s%s", code, out, errb.String())
	}
	if !strings.Contains(out, "FAIL  "+b+"\n") || !strings.Contains(out, "UNREADABLE") {
		t.Fatalf("vanished file not reported as unreadable:\n%s", out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "FAIL: 2 file(s)") {
		t.Fatalf("tail is not last: %q\n%s", last, out)
	}
	if strings.Index(out, "PASS  "+a) > strings.Index(out, "FAIL  "+b) {
		t.Fatalf("files out of order:\n%s", out)
	}
	code, qout, _ := run(t, "--quiet", "ingest", "--dry-run", dir)
	if qout != "" {
		t.Errorf("--quiet wrote %q", qout)
	}
	_, jout, _ := run(t, "--json", "ingest", "--dry-run", dir)
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatalf("--json output is not a document: %v\n%s", err, jout)
	}
	if doc["command"] != "ingest" {
		t.Errorf("json command %v", doc["command"])
	}
	_, vout, _ := run(t, "--verbose", "ingest", "--dry-run", dir)
	if !strings.Contains(vout, "ROUTE") || !strings.Contains(vout, "skip: sidecar") {
		t.Errorf("verbose output lacks the route:\n%s", vout)
	}
}

// Sidecars and empty media need no tool; the cli wiring of the sidecar
// removal flag, the quarantine flag and the state dir is checked here.
func TestIngestSidecarsAndQuarantineWiring(t *testing.T) {
	testutil.Stubs(t)
	t.Run("blocked sidecar kept without the flag", func(t *testing.T) {
		dir := t.TempDir()
		url := write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		code, out, errb := run(t, "ingest", "--state-dir", t.TempDir(), dir)
		if code != 4 || !strings.Contains(out, "BLOCK "+url) {
			t.Errorf("exit %d\n%s%s", code, out, errb)
		}
		if _, err := os.Lstat(url); err != nil {
			t.Error("x.url removed without the flag")
		}
	})
	t.Run("blocked sidecar removed with the flag", func(t *testing.T) {
		dir := t.TempDir()
		url := write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		nfo := write(t, filepath.Join(dir, "x.nfo"), "notes\n")
		code, out, _ := run(t, "--verbose", "ingest", "--remove-blocked-sidecars", "--state-dir", t.TempDir(), dir)
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
	t.Run("dry run removes nothing", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		write(t, filepath.Join(dir, "empty.mkv"), "")
		state := t.TempDir()
		before := tree(t, dir)
		code, out, _ := run(t, "--dry-run", "--verbose", "ingest", "--remove-blocked-sidecars", "--quarantine", "--state-dir", state, dir)
		if code != 4 || !strings.Contains(out, "DRY_RUN") {
			t.Errorf("exit %d\n%s", code, out)
		}
		unchanged(t, before, tree(t, dir))
		if _, err := os.Lstat(filepath.Join(state, "quarantine")); err == nil {
			t.Error("dry run created the quarantine directory")
		}
	})
	t.Run("bare quarantine uses the state dir", func(t *testing.T) {
		dir := t.TempDir()
		empty := write(t, filepath.Join(dir, "sub", "empty.mkv"), "")
		url := write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		base := t.TempDir()
		// A state dir given through a .. segment still resolves to one place.
		state := filepath.Join(base, "x", "..", "state")
		code, out, _ := run(t, "--verbose", "ingest", "--quarantine", "--remove-blocked-sidecars", "--state-dir", state, dir)
		if code != 4 {
			t.Errorf("exit %d\n%s", code, out)
		}
		for _, p := range []string{empty, url} {
			if _, err := os.Lstat(p); err == nil {
				t.Errorf("%s still in the tree", p)
			}
		}
		for _, rel := range []string{filepath.Join("sub", "empty.mkv"), "x.url"} {
			if _, err := os.Lstat(filepath.Join(base, "state", "quarantine", rel)); err != nil {
				t.Errorf("%s not in quarantine: %v", rel, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(base, "x")); err == nil {
			t.Error("the .. segment created a sibling directory")
		}
		if strings.Contains(out, "SIDECAR_REMOVED") {
			t.Errorf("quarantined sidecar also reported removed:\n%s", out)
		}
	})
	t.Run("named quarantine directory", func(t *testing.T) {
		dir := t.TempDir()
		empty := write(t, filepath.Join(dir, "empty.mkv"), "")
		q := filepath.Join(t.TempDir(), "q")
		code, _, errb := run(t, "ingest", "--quarantine="+q, dir)
		if code != 4 {
			t.Errorf("exit %d %s", code, errb)
		}
		if _, err := os.Lstat(filepath.Join(q, "empty.mkv")); err != nil {
			t.Errorf("not quarantined: %v", err)
		}
		if _, err := os.Lstat(empty); err == nil {
			t.Error("still in the tree")
		}
	})
	t.Run("quarantine off leaves files in place", func(t *testing.T) {
		dir := t.TempDir()
		empty := write(t, filepath.Join(dir, "empty.mkv"), "")
		state := t.TempDir()
		code, _, _ := run(t, "ingest", "--quarantine", "--quarantine=off", "--state-dir", state, dir)
		if code != 4 {
			t.Errorf("exit %d", code)
		}
		if _, err := os.Lstat(empty); err != nil {
			t.Error("moved although quarantine was turned off")
		}
		if _, err := os.Lstat(filepath.Join(state, "quarantine")); err == nil {
			t.Error("quarantine directory created")
		}
	})
	t.Run("bidi and double extension names stay put", func(t *testing.T) {
		dir := t.TempDir()
		names := []string{"movie\u202evkm.mkv", "double.mkv.exe", "run.sh", "\u200b.mkv"}
		for _, n := range names {
			write(t, filepath.Join(dir, n), "")
		}
		before := tree(t, dir)
		code, out, _ := run(t, "--verbose", "ingest", "--force", "--hardlinks", "break", dir)
		if code != 4 {
			t.Errorf("exit %d\n%s", code, out)
		}
		unchanged(t, before, tree(t, dir))
		blocks := 0
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "BLOCK ") {
				blocks++
			}
		}
		if blocks != len(names) {
			t.Errorf("want %d BLOCK lines:\n%s", len(names), out)
		}
	})
}

func needTools(t *testing.T) {
	t.Helper()
	testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
}

// The dry run over the whole corpus prints one verdict line per file, in
// walk order, before the tail, and changes nothing on disk.
func TestIngestDryRunOnFixtures(t *testing.T) {
	needTools(t)
	corpus := testutil.CopyTree(t)
	before := tree(t, corpus)
	var files []string
	for p, v := range before {
		if v != "dir" && filepath.Base(p) != ".DS_Store" {
			files = append(files, p)
		}
	}
	code, out, errb := run(t, "--dry-run", "ingest", "--force", "--remove-blocked-sidecars", "--hardlinks", "break", "--quarantine", corpus)
	if code != 4 {
		t.Errorf("exit %d, want 4 (BLOCK from the polyglot):\n%s%s", code, out, errb)
	}
	unchanged(t, before, tree(t, corpus))
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "BLOCK: ") {
		t.Errorf("tail %q", last)
	}
	verdicts := 0
	for _, l := range lines[:len(lines)-1] {
		if strings.HasPrefix(l, "PASS  ") || strings.HasPrefix(l, "WARN  ") || strings.HasPrefix(l, "FAIL  ") || strings.HasPrefix(l, "BLOCK ") {
			verdicts++
		}
	}
	if verdicts != len(files) {
		t.Errorf("%d verdict lines for %d files:\n%s", verdicts, len(files), out)
	}
	for _, p := range files {
		// The terminal form escapes the bidi override in one corpus name.
		if !strings.Contains(out, " "+report.Sanitize(p)+"\n") {
			t.Errorf("no line for %s", p)
		}
	}
	if !strings.Contains(out, "BLOCK "+filepath.Join(corpus, "polyglot.mkv")+"\n") {
		t.Errorf("polyglot.mkv not BLOCK:\n%s", out)
	}
	if !strings.Contains(out, "PASS  "+filepath.Join(corpus, "conforming.mkv")+"\n") {
		t.Errorf("conforming.mkv not PASS:\n%s", out)
	}
	if strings.Contains(out, "__remuxed") {
		t.Errorf("an output tree was mentioned in place:\n%s", out)
	}
	_, vout, _ := run(t, "--verbose", "--dry-run", "ingest", filepath.Join(corpus, "conforming.mkv"))
	if !strings.Contains(vout, "ROUTE              clean: tracks, flags, attachments and chapters already match the profile") || !strings.Contains(vout, "NOTHING_TO_CLEAN") {
		t.Errorf("verbose conforming:\n%s", vout)
	}
	code, vout, _ = run(t, "--verbose", "--dry-run", "ingest", filepath.Join(corpus, "purchased.mp4"))
	if code != 1 || !strings.Contains(vout, "ROUTE              remux: container mp4 is rebuilt as Matroska") || !strings.Contains(vout, "DRY_RUN") {
		t.Errorf("verbose purchased exit %d:\n%s", code, vout)
	}
	if _, err := os.Lstat(filepath.Join(corpus, "purchased.mkv")); err == nil {
		t.Error("dry run wrote purchased.mkv")
	}
}

// A hostile --original-language value and hostile environment values are
// data, never commands.
func TestIngestHostileArguments(t *testing.T) {
	needTools(t)
	src := testutil.Copy(t, "conforming.mkv")
	marker := filepath.Join(t.TempDir(), "pwned")
	before := tree(t, filepath.Dir(src))
	for _, orig := range []string{
		"eng; touch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"-o " + marker,
		"--output=" + marker,
		strings.Repeat("x", 100000),
	} {
		code, out, errb := run(t, "--dry-run", "--profile", "anime", "ingest", "--original-language", orig, src)
		if code != 0 {
			t.Errorf("%q: exit %d\n%s%s", orig[:20], code, out, errb)
		}
		if _, err := os.Lstat(marker); err == nil {
			t.Fatalf("%q created the marker file", orig[:20])
		}
	}
	unchanged(t, before, tree(t, filepath.Dir(src)))
	// A path argument that looks like a flag after -- is a path.
	code, _, errb := run(t, "--dry-run", "ingest", "--", "-not-a-flag.mkv")
	if code != 0 && code != 3 {
		t.Errorf("exit %d %s", code, errb)
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Fatal("marker exists")
	}
}

// The in-place run rewrites a copy of the corpus and a second run finds
// nothing left to do; the summary line of the first run names every verdict.
func TestIngestInPlaceTwice(t *testing.T) {
	needTools(t)
	corpus := testutil.CopyTree(t)
	polyglot := filepath.Join(corpus, "polyglot.mkv")
	polyBefore := tree(t, corpus)[polyglot]
	code, out, errb := run(t, "ingest", "--state-dir", t.TempDir(), corpus)
	if code != 4 {
		t.Errorf("exit %d\n%s%s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(corpus, "purchased.mkv")); err != nil {
		t.Errorf("purchased.mkv: %v", err)
	}
	if tree(t, corpus)[polyglot] != polyBefore {
		t.Error("polyglot.mkv changed")
	}
	after := tree(t, corpus)
	code, out2, _ := run(t, "--verbose", "ingest", "--state-dir", t.TempDir(), corpus)
	if code != 4 {
		t.Errorf("second exit %d\n%s", code, out2)
	}
	if !strings.Contains(out2, "NOTHING_TO_CLEAN") {
		t.Errorf("second pass has no NOTHING_TO_CLEAN:\n%s", out2)
	}
	if strings.Contains(out2, "PLACED") || strings.Contains(out2, "METADATA") {
		t.Errorf("second pass rewrote a file:\n%s", out2)
	}
	unchanged(t, after, tree(t, corpus))
	for p := range after {
		if strings.HasPrefix(filepath.Base(p), ".amuxify-") {
			t.Errorf("temp file left: %s", p)
		}
	}
}
