package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// asUser makes the process look like the given uid to the CLI.
func asUser(t *testing.T, uid int) {
	t.Helper()
	orig := fsutil.Geteuid
	fsutil.Geteuid = func() int { return uid }
	t.Cleanup(func() { fsutil.Geteuid = orig })
}

// Guarantee 8: modifying commands refuse to run as root. That includes
// ingest and every hook adapter, which is where a container running as root
// would hit it, so the rows below cover each of them with the flags that
// delete or move files. Nothing under the directory may change and no
// quarantine directory may appear.
func TestSetupRefusesRoot(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	write(t, filepath.Join(dir, "b.url"), "x\n")
	state := t.TempDir()
	q := filepath.Join(t.TempDir(), "q")
	before := tree(t, dir)
	asUser(t, 0)

	for _, tc := range []struct {
		env  []string
		args []string
		code int
	}{
		{nil, []string{"clean", dir}, 2},
		{nil, []string{"remux", dir}, 2},
		{nil, []string{"scan", "--quarantine", q, dir}, 2},
		{nil, []string{"clean", "--remove-blocked-sidecars", dir}, 2},
		{nil, []string{"remux", "--in-place", dir}, 2},
		{nil, []string{"ingest", dir}, 2},
		{nil, []string{"ingest", "--remove-blocked-sidecars", dir}, 2},
		{nil, []string{"ingest", "--quarantine=" + q, dir}, 2},
		{nil, []string{"--state-dir", state, "ingest", "--quarantine", "--force", dir}, 2},
		{jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd"}, 2},
		{jobEnv("sabnzbd", dir), []string{"--state-dir", state, "hook", "sabnzbd", "--quarantine", "--remove-blocked-sidecars"}, 2},
		{nil, []string{"hook", "sabnzbd", dir, "n", "c", "1", "tv", "g", "0", ""}, 2},
		{jobEnv("nzbget", dir), []string{"hook", "nzbget"}, 94},
		{jobEnv("nzbget", dir), []string{"hook", "nzbget", "--quarantine=" + q, "--remove-blocked-sidecars"}, 94},
		{jobEnv("sonarr", dir), []string{"hook", "sonarr"}, 2},
		{jobEnv("sonarr", dir), []string{"hook", "sonarr", "--remove-blocked-sidecars"}, 2},
		{jobEnv("radarr", dir), []string{"hook", "radarr"}, 2},
		{jobEnv("radarr", dir), []string{"--state-dir", state, "hook", "radarr", "--quarantine"}, 2},
	} {
		hookEnv(t, tc.env...)
		code, out, errs := run(t, tc.args...)
		if code != tc.code || !strings.Contains(errs, "refusing to modify files as root") {
			t.Errorf("%v as root: code %d stderr %q", tc.args, code, errs)
		}
		if out != "" {
			t.Errorf("%v as root: stdout %q", tc.args, out)
		}
	}
	unchanged(t, before, tree(t, dir))
	for _, p := range []string{q, filepath.Join(state, "quarantine")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created by a refused run", p)
		}
	}
	// Reading commands and dry runs are fine as root.
	for _, tc := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"scan", dir}},
		{nil, []string{"--dry-run", "clean", dir}},
		{nil, []string{"--dry-run", "remux", dir}},
		{nil, []string{"--dry-run", "ingest", "--remove-blocked-sidecars", dir}},
		{nil, []string{"--dry-run", "--state-dir", state, "ingest", "--quarantine", dir}},
		{jobEnv("sabnzbd", dir), []string{"--dry-run", "hook", "sabnzbd", "--remove-blocked-sidecars"}},
		{jobEnv("nzbget", dir), []string{"--dry-run", "--state-dir", state, "hook", "nzbget", "--quarantine"}},
		{jobEnv("sonarr", dir), []string{"--dry-run", "hook", "sonarr"}},
		{jobEnv("radarr", dir), []string{"--dry-run", "hook", "radarr", "--remove-blocked-sidecars"}},
	} {
		hookEnv(t, tc.env...)
		code, _, errs := run(t, tc.args...)
		if strings.Contains(errs, "refusing to modify files as root") {
			t.Errorf("%v as root refused: code %d stderr %q", tc.args, code, errs)
		}
	}
	unchanged(t, before, tree(t, dir))
	for _, p := range []string{q, filepath.Join(state, "quarantine")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created by a dry run", p)
		}
	}
	// --allow-root lets a writer proceed past setup, for ingest and the
	// hooks as well as for clean.
	for _, tc := range []struct {
		env  []string
		args []string
		code int
	}{
		{nil, []string{"--allow-root", "clean", dir}, 1},
		{nil, []string{"--allow-root", "ingest", dir}, 4},
		{jobEnv("sabnzbd", dir), []string{"--allow-root", "hook", "sabnzbd"}, 1},
		{jobEnv("nzbget", dir), []string{"--allow-root", "hook", "nzbget"}, 94},
		{jobEnv("sonarr", dir), []string{"--allow-root", "hook", "sonarr"}, 1},
		{jobEnv("radarr", dir), []string{"--allow-root", "hook", "radarr"}, 1},
	} {
		hookEnv(t, tc.env...)
		code, out, errs := run(t, tc.args...)
		if strings.Contains(errs, "refusing to modify files as root") || code != tc.code {
			t.Fatalf("%v: code %d stdout %q stderr %q", tc.args, code, out, errs)
		}
		if !strings.Contains(out, "a.nfo") {
			t.Fatalf("%v did not reach the file: %q", tc.args, out)
		}
	}
	// Without --remove-blocked-sidecars nothing was deleted or moved.
	unchanged(t, before, tree(t, dir))

	// A non-root user is not refused, and the check reads the seam rather
	// than any cached value.
	asUser(t, 1000)
	if code, _, errs := run(t, "clean", dir); code != 1 || strings.Contains(errs, "root") {
		t.Fatalf("uid 1000: code %d stderr %q", code, errs)
	}
}

func TestRemuxInPlaceVerifyNoneIsUsage(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.mkv"), "\x1a\x45\xdf\xa3")
	none := testutil.Profile(t, "[verify]\ntier=\"none\"\n")
	for _, args := range [][]string{
		{"remux", "--in-place", "--verify", "none", dir},
		{"remux", "--verify", "none", "--in-place", dir},
		{"--profile", none, "remux", "--in-place", dir},
		{"remux", "--profile", none, "--in-place", dir},
	} {
		code, out, errs := run(t, args...)
		if code != 2 || !strings.Contains(errs, "--in-place requires verification; --verify none is refused") {
			t.Errorf("%v: code %d stderr %q", args, code, errs)
		}
		if out != "" {
			t.Errorf("%v: stdout %q", args, out)
		}
	}
	// The same profile without --in-place is accepted (the stubs then fail
	// the actual work, which is not a usage error).
	if code, _, errs := run(t, "--profile", none, "--dry-run", "remux", dir); code == 2 {
		t.Fatalf("tier none without --in-place refused: %q", errs)
	}
	// A flag lifting the tier over the profile is accepted.
	if code, _, errs := run(t, "--profile", none, "--dry-run", "remux", "--in-place", "--verify", "quick", dir); code == 2 {
		t.Fatalf("--verify quick over a none profile refused: %q", errs)
	}
	if code, _, errs := run(t, "remux", "--in-place", "--output", t.TempDir(), dir); code != 2 || !strings.Contains(errs, "mutually exclusive") {
		t.Fatalf("--in-place with --output: code %d stderr %q", code, errs)
	}
}

// triggerWriter removes a file the moment a marker line is written.
type triggerWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	victim string
	fired  bool
	err    error
}

func (w *triggerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if !w.fired && strings.Contains(w.buf.String(), w.marker) {
		w.fired = true
		w.err = os.Remove(w.victim)
	}
	return len(p), nil
}

// Per-file streaming: a file's line is written before the next file is
// opened, and the tail comes last.
func TestScanStreamsBeforeTail(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.txt"), "a\n")
	b := write(t, filepath.Join(dir, "b.txt"), "b\n")
	w := &triggerWriter{marker: "PASS  " + a + "\n", victim: b}
	var errb bytes.Buffer
	code := Main([]string{"scan", dir}, w, &errb)
	out := w.buf.String()
	if !w.fired {
		t.Fatalf("marker line never streamed; output:\n%s", out)
	}
	if w.err != nil {
		t.Fatalf("remove: %v", w.err)
	}
	if code != 3 {
		t.Fatalf("exit %d, want 3 (FAIL) for the vanished file; output:\n%s", code, out)
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
	// --quiet prints nothing at all, --json prints no per-file lines
	// before the document.
	code, qout, _ := run(t, "--quiet", "scan", dir)
	if qout != "" {
		t.Errorf("--quiet wrote %q", qout)
	}
	_, jout, _ := run(t, "--json", "scan", dir)
	if !strings.HasPrefix(jout, "{") {
		t.Errorf("--json output does not start with the document: %q", jout)
	}
}

func TestUsageErrors(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	cases := []struct {
		args []string
		msg  string
	}{
		{nil, "usage"},
		{[]string{"bogus"}, "unknown command"},
		{[]string{"scan"}, "scan: at least one path is required"},
		{[]string{"remux"}, "remux: at least one path is required"},
		{[]string{"clean"}, "clean: at least one path is required"},
		{[]string{"scan", "--verify", "bogus", dir}, "--verify must be quick, full or none"},
		{[]string{"remux", "--verify", "bogus", dir}, "--verify must be quick, full or none"},
		{[]string{"remux", "--hardlinks", "bogus", dir}, "--hardlinks must be skip, break or copy"},
		{[]string{"clean", "--hardlinks", "copy", dir}, "--hardlinks must be skip or break"},
		{[]string{"--profile", "nope", "scan", dir}, ""},
		{[]string{"--profile", filepath.Join(dir, "missing.toml"), "scan", dir}, ""},
		{[]string{"--no-such-flag", "scan", dir}, "flag provided but not defined"},
		{[]string{"scan", "--no-such-flag", dir}, "flag provided but not defined"},
		{[]string{"profile", "show"}, "usage: amuxify profile show"},
		{[]string{"profile", "show", "../../etc/passwd"}, ""},
		{[]string{"profile", "bogus"}, ""},
	}
	for _, tc := range cases {
		code, _, errs := run(t, tc.args...)
		if code != 2 {
			t.Errorf("%v: code %d, want 2 (stderr %q)", tc.args, code, errs)
		}
		if tc.msg != "" && !strings.Contains(errs, tc.msg) {
			t.Errorf("%v: stderr %q lacks %q", tc.args, errs, tc.msg)
		}
	}
	// Missing tools are a usage error pointing at doctor.
	missing := filepath.Join(t.TempDir(), "none")
	t.Setenv("AMUXIFY_FFMPEG", missing)
	if code, _, errs := run(t, "scan", dir); code != 2 || !strings.Contains(errs, "ffmpeg not found; run 'amuxify doctor'") {
		t.Errorf("missing ffmpeg: code %d stderr %q", code, errs)
	}
}

func TestVersionAndHelp(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"version", "extra"}} {
		code, out, errs := run(t, args...)
		if code != 0 || out != "amuxify "+Version+"\n" || errs != "" {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errs)
		}
	}
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		code, out, errs := run(t, args...)
		if code != 0 || out != "" || !strings.Contains(errs, "Exit status: 0 PASS, 1 WARN, 2 usage error, 3 FAIL, 4 BLOCK, 130 interrupted.") {
			t.Errorf("%v: code %d stdout %q stderr %q", args, code, out, errs)
		}
	}
	// Extra arguments after help and version are ignored, never executed.
	if code, _, _ := run(t, "version", "scan", "/"); code != 0 {
		t.Error("version with trailing arguments failed")
	}
	// The help page lists every global flag with the help string bind gives
	// it, each on its own line, so the two cannot drift apart.
	_, _, help := run(t, "help")
	fs := flag.NewFlagSet("amuxify", flag.ContinueOnError)
	(&Global{}).bind(fs)
	n := 0
	fs.VisitAll(func(f *flag.Flag) {
		n++
		_, usage := flag.UnquoteUsage(f)
		found := false
		for _, line := range strings.Split(help, "\n") {
			if strings.HasPrefix(line, "  --"+f.Name+" ") && strings.HasSuffix(line, usage) {
				found = true
			}
		}
		if !found {
			t.Errorf("help lacks a line for --%s with %q:\n%s", f.Name, usage, help)
		}
	})
	if n == 0 {
		t.Fatal("no global flags bound")
	}
	if !strings.Contains(help, "--timeout <duration>    per-tool timeout (default: 60s probe, 1h verify, 6h remux, 2h clean)\n") {
		t.Errorf("help lacks the --timeout line:\n%s", help)
	}
	if !strings.Contains(help, "amuxify "+Version+" - the ingest gate") || strings.Contains(help, "%!") {
		t.Errorf("help header or format verbs broken:\n%s", help)
	}
}

// walkNulls reports every JSON path whose value is null.
func walkNulls(prefix string, v interface{}, out *[]string) {
	switch x := v.(type) {
	case nil:
		*out = append(*out, prefix)
	case map[string]interface{}:
		for k, val := range x {
			walkNulls(prefix+"."+k, val, out)
		}
	case []interface{}:
		for i, val := range x {
			walkNulls(prefix+"["+string(rune('0'+i%10))+"]", val, out)
		}
	}
}

func TestJSONReportHasSchemaAndNoNulls(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	write(t, filepath.Join(dir, "b.url"), "x\n")
	write(t, filepath.Join(dir, "c.xyz"), "x\n")
	code, out, errs := run(t, "--json", "scan", dir, filepath.Join(dir, "missing"))
	if code != 4 {
		t.Fatalf("code %d stderr %q", code, errs)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	var nulls []string
	walkNulls("$", doc, &nulls)
	if len(nulls) != 0 {
		t.Errorf("null values at %v", nulls)
	}
	for _, k := range []string{"tool", "version", "command", "profile", "started", "finished", "verdict", "counts", "files", "errors"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	files, _ := doc["files"].([]interface{})
	if len(files) != 3 {
		t.Errorf("%d files in the document", len(files))
	}
	for _, f := range files {
		m := f.(map[string]interface{})
		if _, ok := m["findings"].([]interface{}); !ok {
			t.Errorf("file %v has no findings array", m["path"])
		}
	}
	if doc["verdict"] != "BLOCK" {
		t.Errorf("verdict %v", doc["verdict"])
	}
	// Nothing but the document reaches stdout.
	if strings.Count(out, "\n{") != 0 || !strings.HasPrefix(out, "{") {
		t.Errorf("stdout is not a single document:\n%s", out)
	}
	if doc["schema"] != report.SchemaID {
		t.Errorf("schema %v, want %q", doc["schema"], report.SchemaID)
	}
}

// Hostile arguments and environment never crash the CLI or run anything.
func TestHostileArgumentsAndEnvironment(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	canary := filepath.Join(t.TempDir(), "pwned")
	for _, tc := range []struct {
		env  map[string]string
		args []string
	}{
		{map[string]string{"AMUXIFY_PROFILE": "../../etc/passwd"}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_PROFILE": "$(touch " + canary + ")"}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_PROFILE": "‮homelab"}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_PROFILE": strings.Repeat("x", 100000)}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_FFMPEG": "/bin/sh -c 'touch " + canary + "'"}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_FFMPEG": dir}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_MKVMERGE": "$(touch " + canary + ")"}, []string{"scan", dir}},
		{map[string]string{"AMUXIFY_STATE_DIR": "$(touch " + canary + ")"}, []string{"scan", dir}},
		{nil, []string{"scan", "$(touch " + canary + ")"}},
		{nil, []string{"scan", "`touch " + canary + "`"}},
		{nil, []string{"scan", "--", "--quarantine"}},
		{nil, []string{"scan", "-"}},
		{nil, []string{"scan", ""}},
		{nil, []string{"scan", "\x00"}},
		{nil, []string{"scan", dir + strings.Repeat("/", 5000)}},
		{nil, []string{"scan", dir + "/./" + strings.Repeat("../"+filepath.Base(dir)+"/", 200)}},
		{nil, []string{"scan", "--verify", "", dir}},
		{nil, []string{"scan", "--quarantine", "", dir}},
		{nil, []string{"--timeout", "-1s", "scan", dir}},
		{nil, []string{"--timeout", "999999h", "scan", dir}},
		{nil, []string{"--profile", "", "scan", dir}},
		{nil, []string{"‮scan", dir}},
		{nil, []string{"profile", "show", "\x00"}},
		{nil, []string{"profile", "show", strings.Repeat("a", 5000) + ".toml"}},
	} {
		for k, v := range tc.env {
			t.Setenv(k, v)
		}
		code, _, _ := run(t, tc.args...)
		if code < 0 || code > 4 {
			t.Errorf("%v %v: exit %d outside the frozen set", tc.env, tc.args, code)
		}
		for k := range tc.env {
			t.Setenv(k, "")
		}
	}
	if _, err := os.Lstat(canary); err == nil {
		t.Fatal("an argument or environment value was executed as a shell command")
	}
	if _, err := os.Lstat(filepath.Join(dir, "a.nfo")); err != nil {
		t.Fatal("a scan removed a file")
	}
}

func TestExitCodesFollowVerdict(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	pass := write(t, filepath.Join(dir, "p", "a.nfo"), "nfo\n")
	warn := write(t, filepath.Join(dir, "w", "a.xyz"), "x\n")
	block := write(t, filepath.Join(dir, "b", "a.url"), "x\n")
	fail := write(t, filepath.Join(dir, "f", "a.srt"), "\x89PNG\r\n\x1a\n")
	for _, tc := range []struct {
		path string
		code int
		word string
	}{
		{pass, 0, "PASS"},
		{warn, 1, "WARN"},
		{fail, 3, "FAIL"},
		{block, 4, "BLOCK"},
	} {
		code, out, _ := run(t, "scan", tc.path)
		if code != tc.code || !strings.HasPrefix(out, fmt.Sprintf("%-5s %s\n", tc.word, tc.path)) {
			t.Errorf("%s: code %d, output %q", tc.path, code, out)
		}
	}
	// The worst file decides, in any order.
	code, out, _ := run(t, "scan", pass, block, warn)
	if code != 4 || !strings.HasSuffix(out, "BLOCK: 3 file(s) BLOCK=1 PASS=1 WARN=1\n") {
		t.Errorf("mixed: code %d output %q", code, out)
	}
	// A missing path is an error and a FAIL.
	code, _, _ = run(t, "scan", filepath.Join(dir, "missing"))
	if code != 3 {
		t.Errorf("missing path: code %d", code)
	}
}

// Guarantee 8: --dry-run changes nothing anywhere.
func TestDryRunTouchesNothing(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "x\n")
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	q := filepath.Join(t.TempDir(), "q")
	if code, _, _ := run(t, "--dry-run", "clean", "--remove-blocked-sidecars", dir); code > 1 {
		t.Errorf("dry clean exit %d", code)
	}
	if _, err := os.Lstat(url); err != nil {
		t.Error("dry-run clean removed the blocked sidecar")
	}
	code, out, _ := run(t, "--dry-run", "scan", "--quarantine", q, dir)
	if code != 4 {
		t.Errorf("dry scan exit %d", code)
	}
	if _, err := os.Lstat(q); err == nil {
		t.Error("dry-run scan created the quarantine directory")
	}
	if _, err := os.Lstat(url); err != nil {
		t.Error("dry-run scan moved the blocked sidecar")
	}
	if strings.Contains(out, "moved to") {
		t.Errorf("dry run claims to have moved a file:\n%s", out)
	}
	if runtime.GOOS != "windows" {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 2 {
			t.Errorf("directory changed: %d entries", len(entries))
		}
	}
}

// A file whose name carries an escape sequence, a carriage return, a bidi
// override and a newline that spells a verdict line cannot reshape the
// terminal report: every command prints it as one line with visible
// escapes, while --json carries the raw name. The quarantine path in a
// QUARANTINED message and the run-level error for a vanished path go
// through the same writer.
func TestHumanReportEscapesHostileNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("control characters are not allowed in file names on windows")
	}
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	name := "\x1b[2K\rPASS  /forged\nBLOCK /forged‮" + "fni.nfo"
	p := write(t, filepath.Join(dir, name), "nfo\n")
	escaped := report.Sanitize(p)
	if escaped == p || strings.ContainsAny(escaped, "\x1b\r\n‮") {
		t.Fatalf("test setup: %q", escaped)
	}
	for _, args := range [][]string{
		{"scan", dir},
		{"--verbose", "scan", dir},
		{"--dry-run", "clean", dir},
		{"--dry-run", "ingest", "--remove-blocked-sidecars", dir},
		{"--dry-run", "--verbose", "remux", dir},
	} {
		code, out, errs := run(t, args...)
		// The bidi override makes the file BLOCK (BIDI_NAME); the clean
		// dry run over a sidecar never reaches that finding.
		if code != 4 && code != 0 {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errs)
		}
		for _, l := range append(lines(out), lines(errs)...) {

			for _, r := range l {
				if r < 0x20 && r != '\t' || r == 0x7f || r == 0x202e {
					t.Errorf("%v: raw %U in %q", args, r, l)
				}
			}
			if strings.HasPrefix(l, "PASS  /forged") || strings.HasPrefix(l, "BLOCK /forged") {
				t.Errorf("%v: forged verdict line %q", args, l)
			}
		}
		if !strings.Contains(out, " "+escaped+"\n") {
			t.Errorf("%v: no escaped line for the file:\n%s", args, out)
		}
	}
	// The JSON report keeps the raw name.
	_, out, _ := run(t, "--json", "scan", dir)
	var doc struct {
		Files []struct{ Path string } `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Files) != 1 || doc.Files[0].Path != p {
		t.Errorf("JSON path %q, want %q", doc.Files, p)
	}
}
