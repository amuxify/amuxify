package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// hookEnv replaces the environment the hook adapters read for the rest of
// the test. Only these entries are visible; the process environment is not.
func hookEnv(t *testing.T, kv ...string) {
	t.Helper()
	orig := environ
	environ = func() []string { return append([]string(nil), kv...) }
	t.Cleanup(func() { environ = orig })
}

// jobEnv is the environment each caller sets for a finished download at p.
func jobEnv(adapter, p string) []string {
	switch adapter {
	case "sabnzbd":
		return []string{"SAB_COMPLETE_DIR=" + p, "SAB_FINAL_NAME=Job", "SAB_CAT=tv", "SAB_PP_STATUS=0", "SAB_VERSION=4.3.0"}
	case "nzbget":
		return []string{"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_STATUS=SUCCESS/ALL", "NZBPP_DIRECTORY=" + p, "NZBPP_NZBNAME=Job", "NZBPP_CATEGORY=tv"}
	case "sonarr":
		return []string{"sonarr_eventtype=Download", "sonarr_episodefile_path=" + p, "sonarr_series_title=Show", "sonarr_release_title=Show.S01E01"}
	case "radarr":
		return []string{"radarr_eventtype=Download", "radarr_moviefile_path=" + p, "radarr_movie_title=Movie"}
	}
	panic("unknown adapter " + adapter)
}

var adapters = []string{"sabnzbd", "nzbget", "sonarr", "radarr"}

// lines splits output into lines without the trailing empty one.
func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// checkNZBGetOutput asserts what NZBGet requires of stdout and stderr: every
// line carries a tag, and the only line that begins with [NZB] after the tag
// is the genuine MARK=BAD, which is the last line and appears only when
// expected.
func checkNZBGetOutput(t *testing.T, out, errs string, markBad bool) {
	t.Helper()
	ls := lines(out)
	for i, l := range ls {
		if !strings.HasPrefix(l, "[INFO] ") {
			t.Errorf("stdout line without [INFO] tag: %q", l)
			continue
		}
		body := strings.TrimPrefix(l, "[INFO] ")
		if len(body) >= 5 && strings.EqualFold(body[:5], "[NZB]") {
			if body != "[NZB] MARK=BAD" || i != len(ls)-1 || !markBad {
				t.Errorf("unexpected control line %q at %d of %d (mark bad expected: %v)", l, i, len(ls), markBad)
			}
		}
	}
	if markBad && (len(ls) == 0 || ls[len(ls)-1] != "[INFO] [NZB] MARK=BAD") {
		t.Errorf("MARK=BAD is not the last stdout line:\n%s", out)
	}
	for _, l := range lines(errs) {
		if !strings.HasPrefix(l, "[ERROR] ") {
			t.Errorf("stderr line without [ERROR] tag: %q", l)
		}
	}
}

// noControlLines asserts that a caller other than NZBGet never sees an NZBGet
// command on stdout, and that no output line was forged by a name.
func noControlLines(t *testing.T, out string) {
	t.Helper()
	for _, l := range lines(out) {
		if strings.HasPrefix(l, "[NZB]") || strings.HasPrefix(l, "[INFO] ") || strings.HasPrefix(l, "[ERROR] ") {
			t.Errorf("NZBGet-style line for another caller: %q", l)
		}
	}
}

func TestHookUsage(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	none := testutil.Profile(t, "[verify]\ntier=\"none\"\n")
	// q is the directory a wrapper wrote after a bare --quarantine, which
	// must never be created by a refused invocation.
	q := filepath.Join(t.TempDir(), "q")
	for _, tc := range []struct {
		name string
		env  []string
		args []string
		code int
		want string
	}{
		{"no adapter", nil, []string{"hook"}, 2, "hook: adapter required: sabnzbd, nzbget, sonarr, radarr"},
		{"unknown adapter", nil, []string{"hook", "bogus"}, 2, `hook: unknown adapter "bogus"`},
		{"adapter with a newline", nil, []string{"hook", "sonarr\n"}, 2, "unknown adapter"},
		{"sabnzbd without env or args", nil, []string{"hook", "sabnzbd"}, 2, "hook sabnzbd: not started by SABnzbd"},
		{"nzbget without env", nil, []string{"hook", "nzbget"}, 94, "[ERROR] amuxify: hook nzbget: not started by NZBGet"},
		{"nzbget without env but with args", nil, []string{"hook", "nzbget", dir}, 94, "[ERROR] amuxify: hook nzbget: unexpected argument \"" + dir + "\"; the adapter reads the job from the environment"},
		{"nzbget with a directory after a bare --quarantine", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--quarantine", dir}, 94, "[ERROR] amuxify: hook nzbget: unexpected argument \"" + dir + "\"; the adapter reads the job from the environment; if it was meant as the quarantine directory write --quarantine=" + dir},
		{"sonarr with a directory after a bare --quarantine", jobEnv("sonarr", dir), []string{"hook", "sonarr", "--quarantine", dir}, 2, "hook sonarr: unexpected argument \"" + dir + "\"; the adapter reads the job from the environment; if it was meant as the quarantine directory write --quarantine=" + dir},
		{"radarr with a stray argument", jobEnv("radarr", dir), []string{"hook", "radarr", "extra"}, 2, "hook radarr: unexpected argument \"extra\"; the adapter reads the job from the environment\n"},
		{"sabnzbd with one positional", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--quarantine", dir}, 2, "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 1 beginning with \"" + dir + "\"; if it was meant as the quarantine directory write --quarantine=" + dir},
		{"sabnzbd with six positionals", nil, []string{"hook", "sabnzbd", dir, "n", "c", "1", "tv", "g"}, 2, "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 6 beginning with \"" + dir + "\"\n"},
		{"sabnzbd with nine positionals from the environment form", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--quarantine", q, dir, "n", "c", "1", "tv", "g", "0", ""}, 2, "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with \"" + q + "\"; if it was meant as the quarantine directory write --quarantine=" + q + "\n"},
		{"sabnzbd with nine positionals from the argument form", nil, []string{"hook", "sabnzbd", "--quarantine", q, dir, "n", "c", "1", "tv", "g", "0", ""}, 2, "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with \"" + q + "\"; if it was meant as the quarantine directory write --quarantine=" + q + "\n"},
		{"sabnzbd with nine positionals and no bare --quarantine", nil, []string{"hook", "sabnzbd", q, dir, "n", "c", "1", "tv", "g", "0", ""}, 2, "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with \"" + q + "\"\n"},
		{"sabnzbd with ten positionals names no stray", nil, []string{"hook", "sabnzbd", "--quarantine", q, dir, "n", "c", "1", "tv", "g", "0", "", "x"}, 2, "got 10 beginning with \"" + q + "\"\n"},
		{"sabnzbd with nine positionals and a bare path as the failure URL names no stray", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--quarantine", q, dir, "n", "c", "1", "tv", "g", "0", "/"}, 2, "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with \"" + q + "\"\n"},
		{"sabnzbd with an empty status among seven", nil, []string{"hook", "sabnzbd", dir, "n", "c", "1", "tv", "g", ""}, 2, "hook sabnzbd: SABnzbd's seventh parameter, the post-processing status, is empty\n"},
		{"sabnzbd with an empty status among eight", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", dir, "n", "c", "1", "tv", "g", "", ""}, 2, "hook sabnzbd: SABnzbd's seventh parameter, the post-processing status, is empty\n"},
		{"sabnzbd with an empty status and a bare --quarantine names no argument", nil, []string{"hook", "sabnzbd", "--quarantine", dir, "n", "c", "1", "tv", "g", "", ""}, 2, "hook sabnzbd: SABnzbd's seventh parameter, the post-processing status, is empty\n"},
		{"sabnzbd env without SAB_PP_STATUS", []string{"SAB_COMPLETE_DIR=" + dir}, []string{"hook", "sabnzbd"}, 2, "hook sabnzbd: not started by SABnzbd: SAB_COMPLETE_DIR is set but SAB_PP_STATUS is not\n"},
		{"sabnzbd env with an empty SAB_PP_STATUS", []string{"SAB_COMPLETE_DIR=" + dir, "SAB_PP_STATUS="}, []string{"hook", "sabnzbd"}, 2, "hook sabnzbd: not started by SABnzbd: SAB_COMPLETE_DIR is set but SAB_PP_STATUS is not\n"},
		{"sonarr without env", nil, []string{"hook", "sonarr"}, 2, "hook sonarr: not started by Sonarr: sonarr_eventtype is not set"},
		{"radarr without env", nil, []string{"hook", "radarr"}, 2, "hook radarr: not started by Radarr: radarr_eventtype is not set"},
		{"sonarr download without a path", []string{"sonarr_eventtype=Download"}, []string{"hook", "sonarr"}, 2, "event Download without sonarr_episodefile_path"},
		{"radarr download without a path", []string{"radarr_eventtype=Download"}, []string{"hook", "radarr"}, 2, "event Download without radarr_moviefile_path"},
		{"bad fail-on", []string{"sonarr_eventtype=Test"}, []string{"hook", "sonarr", "--fail-on", "never"}, 2, `--fail-on must be warn, fail or block, not "never"`},
		{"empty fail-on", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--fail-on", ""}, 2, "--fail-on must be warn, fail or block"},
		{"bad fail-on nzbget", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--fail-on", "pass"}, 94, "[ERROR] amuxify: hook nzbget: --fail-on must be warn, fail or block"},
		{"nzbget with --json", jobEnv("nzbget", dir), []string{"--json", "hook", "nzbget"}, 94, "[ERROR] amuxify: hook nzbget: --json is not supported because NZBGet logs stdout line by line; use --json-out <file>"},
		{"nzbget with --json after the adapter", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--json"}, 94, "--json is not supported"},
		{"--force is not a hook flag", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--force"}, 2, "flag provided but not defined: -force"},
		{"--original-language is not a hook flag", jobEnv("sonarr", dir), []string{"hook", "sonarr", "--original-language", "jpn"}, 2, "flag provided but not defined: -original-language"},
		{"unknown flag", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--quarantine-everything"}, 2, "flag provided but not defined"},
		{"unknown flag nzbget", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--bogus"}, 94, "[ERROR] flag provided but not defined: -bogus"},
		{"verify none from the flag", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--verify", "none"}, 2, "verify tier none is refused (from --verify)"},
		{"verify none from the flag nzbget", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--verify=none"}, 94, "[ERROR] amuxify: ingest: in-place writes require verification; verify tier none is refused (from --verify)"},
		{"verify none from the flag sonarr", jobEnv("sonarr", dir), []string{"hook", "sonarr", "--verify", "NONE"}, 2, "--verify must be quick, full or none"},
		{"verify none from the profile", jobEnv("sabnzbd", dir), []string{"--profile", none, "hook", "sabnzbd"}, 2, "verify tier none is refused (from profile"},
		{"verify none from the profile after the adapter", jobEnv("radarr", dir), []string{"hook", "radarr", "--profile", none}, 2, "verify tier none is refused (from profile"},
		{"verify none from the profile nzbget", jobEnv("nzbget", dir), []string{"--profile", none, "hook", "nzbget"}, 94, "[ERROR] amuxify: ingest: in-place writes require verification; verify tier none is refused (from profile"},
		{"verify none from the profile under dry-run", jobEnv("sabnzbd", dir), []string{"--dry-run", "--profile", none, "hook", "sabnzbd"}, 2, "verify tier none is refused"},
		{"bad verify", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--verify", "bogus"}, 2, "hook sabnzbd: --verify must be quick, full or none"},
		{"bad hardlinks", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--hardlinks", "follow"}, 2, "hook sabnzbd: --hardlinks must be skip, break or copy"},
		{"bad category glob", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--category", "["}, 2, "hook sabnzbd: --category:"},
		{"bad category glob nzbget", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--category", "tv["}, 94, "[ERROR] amuxify: hook nzbget: --category:"},
		{"bad profile", jobEnv("sabnzbd", dir), []string{"--profile", "../../etc/passwd", "hook", "sabnzbd"}, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEnv(t, tc.env...)
			code, out, errs := run(t, tc.args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d\nstdout: %s\nstderr: %s", code, tc.code, out, errs)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("stderr %q does not contain %q", errs, tc.want)
			}
			if out != "" {
				t.Errorf("stdout %q for a usage error", out)
			}
			if len(tc.args) > 1 && tc.args[len(tc.args)-1] != "nzbget" && !strings.Contains(strings.Join(tc.args, " "), "nzbget") {
				noControlLines(t, out)
			} else if strings.Contains(strings.Join(tc.args, " "), "nzbget") {
				checkNZBGetOutput(t, out, errs, false)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(dir, "a.nfo")); err != nil {
		t.Error("a usage error touched the download")
	}
	if _, err := os.Lstat(q); err == nil {
		t.Error("a refused invocation created the quarantine directory")
	}
	// A usage error is never a verdict: the block file stays and no hook
	// ever runs ingest with tier none, whatever the flag order.
	url := write(t, filepath.Join(dir, "b.url"), "x\n")
	hookEnv(t, jobEnv("sabnzbd", dir)...)
	if code, _, _ := run(t, "hook", "sabnzbd", "--remove-blocked-sidecars", "--verify", "none"); code != 2 {
		t.Errorf("exit %d", code)
	}
	if _, err := os.Lstat(url); err != nil {
		t.Error("ingest ran with tier none")
	}
}

// Guarantee 10 under the hook: the process refuses to modify files as root
// under every adapter, in that adapter's usage convention, and the Sonarr
// and Radarr connection test fails so the misconfiguration is visible in
// their test dialog. --dry-run needs no writer and is allowed.
func TestHookRefusesRoot(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "x\n")
	asUser(t, 0)
	for _, tc := range []struct {
		env  []string
		args []string
		code int
	}{
		{jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--remove-blocked-sidecars"}, 2},
		{jobEnv("nzbget", dir), []string{"hook", "nzbget", "--remove-blocked-sidecars"}, 94},
		{jobEnv("sonarr", dir), []string{"hook", "sonarr"}, 2},
		{jobEnv("radarr", dir), []string{"hook", "radarr"}, 2},
		{[]string{"sonarr_eventtype=Test"}, []string{"hook", "sonarr"}, 1},
		{[]string{"radarr_eventtype=Test"}, []string{"hook", "radarr"}, 1},
	} {
		hookEnv(t, tc.env...)
		code, out, errs := run(t, tc.args...)
		if code != tc.code || !strings.Contains(errs, "refusing to modify files as root") {
			t.Errorf("%v: exit %d stderr %q", tc.args, code, errs)
		}
		if tc.args[1] == "nzbget" {
			checkNZBGetOutput(t, out, errs, false)
		} else {
			noControlLines(t, out)
		}
		if tc.code == 1 && !strings.Contains(out, "test failed") {
			t.Errorf("%v: test failure not on stdout: %q", tc.args, out)
		}
	}
	if _, err := os.Lstat(url); err != nil {
		t.Fatal("a root run removed the block file")
	}
	// Under --dry-run the root refusal does not apply, and nothing changes.
	hookEnv(t, jobEnv("sabnzbd", dir)...)
	code, _, errs := run(t, "--dry-run", "hook", "sabnzbd", "--remove-blocked-sidecars")
	if code != 1 || strings.Contains(errs, "root") {
		t.Errorf("dry run as root: exit %d stderr %q", code, errs)
	}
	if _, err := os.Lstat(url); err != nil {
		t.Fatal("dry run removed the block file")
	}
}

// Every exit code in the table, for every adapter, verdict and --fail-on,
// through Main. The expected values are written out here rather than taken
// from the hook package so the table under test is independent.
func TestHookExitCodeMatrix(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	files := map[string]string{
		"PASS":  write(t, filepath.Join(dir, "p", "a.nfo"), "nfo\n"),
		"WARN":  write(t, filepath.Join(dir, "w", "a.xyz"), "x\n"),
		"FAIL":  write(t, filepath.Join(dir, "f", "a.srt"), "\x89PNG\r\n\x1a\n"),
		"BLOCK": write(t, filepath.Join(dir, "b", "a.url"), "x\n"),
	}
	verdicts := []string{"PASS", "WARN", "FAIL", "BLOCK"}
	type row map[string]int // verdict -> exit code
	matrix := map[string]map[string]row{
		"sabnzbd": {
			"warn":  {"PASS": 0, "WARN": 1, "FAIL": 1, "BLOCK": 1},
			"fail":  {"PASS": 0, "WARN": 0, "FAIL": 1, "BLOCK": 1},
			"block": {"PASS": 0, "WARN": 0, "FAIL": 0, "BLOCK": 1},
		},
		"nzbget": {
			"warn":  {"PASS": 93, "WARN": 94, "FAIL": 94, "BLOCK": 94},
			"fail":  {"PASS": 93, "WARN": 93, "FAIL": 94, "BLOCK": 94},
			"block": {"PASS": 93, "WARN": 93, "FAIL": 93, "BLOCK": 94},
		},
	}
	matrix["sonarr"], matrix["radarr"] = matrix["sabnzbd"], matrix["sabnzbd"]
	before := tree(t, dir)
	for _, a := range adapters {
		for _, failOn := range []string{"", "warn", "fail", "block", "BLOCK"} {
			for _, v := range verdicts {
				p := files[v]
				want := matrix[a][strings.ToLower(failOn)][v]
				if failOn == "" {
					want = matrix[a]["fail"][v]
				}
				args := []string{"hook", a}
				if failOn != "" {
					args = append(args, "--fail-on", failOn)
				}
				hookEnv(t, jobEnv(a, p)...)
				code, out, errs := run(t, args...)
				if code != want {
					t.Errorf("%s %s fail-on %q: exit %d, want %d\nstdout: %s\nstderr: %s", a, v, failOn, code, want, out, errs)
				}
				prefix := ""
				if a == "nzbget" {
					prefix = "[INFO] "
					checkNZBGetOutput(t, out, errs, v == "BLOCK")
				} else {
					noControlLines(t, out)
					noControlLines(t, errs)
				}
				if !strings.Contains(out, prefix+fmt.Sprintf("%-5s %s\n", v, p)) {
					t.Errorf("%s %s: no verdict line:\n%s", a, v, out)
				}
				if !strings.HasPrefix(out, prefix+"amuxify hook "+a+": ") {
					t.Errorf("%s %s: no start line:\n%s", a, v, out)
				}
				final := prefix + fmt.Sprintf("amuxify: %s, 1 file(s)\n", v)
				if a == "sabnzbd" {
					if !strings.HasSuffix(out, final) {
						t.Errorf("sabnzbd %s: final line missing:\n%s", v, out)
					}
				} else if strings.Contains(out, "amuxify: "+v+", 1 file(s)") {
					t.Errorf("%s %s: SABnzbd's final line printed:\n%s", a, v, out)
				}
				if a == "sonarr" || a == "radarr" {
					// The arrs log stderr as errors, so the tail goes there
					// only when the caller is told the import failed.
					if want == 1 && !strings.Contains(errs, v+": 1 file(s)") {
						t.Errorf("%s %s fail-on %q: failure tail not on stderr: %q", a, v, failOn, errs)
					}
					if want == 0 && errs != "" {
						t.Errorf("%s %s fail-on %q: stderr %q on success", a, v, failOn, errs)
					}
				}
			}
		}
	}
	unchanged(t, before, tree(t, dir))
	// --quiet drops the text report and SABnzbd's final line, but never the
	// start line, the verdict-driven exit code or NZBGet's control line.
	hookEnv(t, jobEnv("sabnzbd", files["BLOCK"])...)
	if code, out, _ := run(t, "--quiet", "hook", "sabnzbd"); code != 1 || out != "amuxify hook sabnzbd: Job (pp status 0)\n" {
		t.Errorf("quiet sabnzbd: exit %d stdout %q", code, out)
	}
	hookEnv(t, jobEnv("nzbget", files["BLOCK"])...)
	code, out, errs := run(t, "--quiet", "hook", "nzbget")
	if code != 94 || out != "[INFO] amuxify hook nzbget: Job (status SUCCESS/SUCCESS/ALL)\n[INFO] [NZB] MARK=BAD\n" {
		t.Errorf("quiet nzbget: exit %d stdout %q", code, out)
	}
	checkNZBGetOutput(t, out, errs, true)
	hookEnv(t, jobEnv("nzbget", files["FAIL"])...)
	if code, out, _ := run(t, "--quiet", "hook", "nzbget"); code != 94 || out != "[INFO] amuxify hook nzbget: Job (status SUCCESS/SUCCESS/ALL)\n" {
		t.Errorf("quiet nzbget FAIL: exit %d stdout %q", code, out)
	}
}

// A skipped job does nothing: the adapter prints one line, exits with the
// caller's nothing-to-do code and never touches the download, whatever the
// other flags say.
func TestHookSABnzbdSkip(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "x\n")
	before := tree(t, dir)
	for _, tc := range []struct {
		name string
		env  []string
		args []string
		code int
		line string
	}{
		{"sabnzbd pp status 1", []string{"SAB_COMPLETE_DIR=" + dir, "SAB_PP_STATUS=1", "SAB_FAIL_MSG=Unpack failed"}, []string{"hook", "sabnzbd", "--fail-on", "warn", "--remove-blocked-sidecars"}, 0,
			"amuxify hook sabnzbd: skipping, post-processing status 1; the files are not usable: Unpack failed\n"},
		{"sabnzbd pp status 3 by argv", nil, []string{"hook", "sabnzbd", dir, "j.nzb", "j", "1", "tv", "g", "3", ""}, 0,
			"amuxify hook sabnzbd: skipping, post-processing status 3; the files are not usable\n"},
		{"sabnzbd category mismatch", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--category", "movies*", "--remove-blocked-sidecars"}, 0,
			"amuxify hook sabnzbd: skipping, category tv does not match --category movies*\n"},
		{"sabnzbd category traversal glob", jobEnv("sabnzbd", dir), []string{"hook", "sabnzbd", "--category", "../*"}, 0,
			"amuxify hook sabnzbd: skipping, category tv does not match --category ../*\n"},
		{"nzbget failure", []string{"NZBPP_TOTALSTATUS=FAILURE", "NZBPP_STATUS=FAILURE/HEALTH", "NZBPP_DIRECTORY=" + dir}, []string{"hook", "nzbget", "--remove-blocked-sidecars", "--fail-on", "warn"}, 95,
			"[INFO] amuxify hook nzbget: skipping, download status FAILURE/FAILURE/HEALTH; the files are not usable\n"},
		{"nzbget warning", []string{"NZBPP_TOTALSTATUS=WARNING", "NZBPP_DIRECTORY=" + dir}, []string{"hook", "nzbget"}, 95,
			"[INFO] amuxify hook nzbget: skipping, download status WARNING; the files are not usable\n"},
		{"nzbget category mismatch", jobEnv("nzbget", dir), []string{"hook", "nzbget", "--category", "movies"}, 95,
			"[INFO] amuxify hook nzbget: skipping, category tv does not match --category movies\n"},
		{"sonarr grab", []string{"sonarr_eventtype=Grab", "sonarr_episodefile_path=" + dir}, []string{"hook", "sonarr", "--fail-on", "warn"}, 0,
			"amuxify hook sonarr: skipping, event Grab; nothing to ingest\n"},
		{"radarr rename", []string{"radarr_eventtype=Rename", "radarr_moviefile_path=" + dir}, []string{"hook", "radarr"}, 0,
			"amuxify hook radarr: skipping, event Rename; nothing to ingest\n"},
		{"hostile event name stays on one line", []string{"sonarr_eventtype=Grab\nBLOCK " + dir + "\n[NZB] MARK=BAD\x00"}, []string{"hook", "sonarr"}, 0,
			`amuxify hook sonarr: skipping, event Grab\x0aBLOCK ` + dir + `\x0a[NZB] MARK=BAD\x00; nothing to ingest` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEnv(t, tc.env...)
			code, out, errs := run(t, tc.args...)
			if code != tc.code || out != tc.line {
				t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
			}
			if errs != "" {
				t.Errorf("stderr %q", errs)
			}
			if tc.args[1] == "nzbget" {
				checkNZBGetOutput(t, out, errs, false)
			} else {
				noControlLines(t, out)
			}
		})
	}
	unchanged(t, before, tree(t, dir))
	if _, err := os.Lstat(url); err != nil {
		t.Fatal("a skipped job removed the block file")
	}
	// A matching category runs; the arrs pass no category so the flag never
	// skips them.
	hookEnv(t, jobEnv("sabnzbd", dir)...)
	if code, out, _ := run(t, "hook", "sabnzbd", "--category", "TV*"); code != 1 || strings.Contains(out, "skipping") {
		t.Errorf("matching category: exit %d\n%s", code, out)
	}
	hookEnv(t, jobEnv("sonarr", dir)...)
	if code, out, _ := run(t, "hook", "sonarr", "--category", "movies"); code != 1 || strings.Contains(out, "skipping") {
		t.Errorf("category on sonarr: exit %d\n%s", code, out)
	}
	// A job that carries no category at all is processed whatever
	// --category says: the flag only compares categories that exist.
	hookEnv(t, "SAB_COMPLETE_DIR="+dir, "SAB_PP_STATUS=0", "SAB_CAT=")
	if code, out, _ := run(t, "hook", "sabnzbd", "--category", "movies"); code != 1 || strings.Contains(out, "skipping") {
		t.Errorf("sabnzbd without a category: exit %d\n%s", code, out)
	}
	hookEnv(t, "NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY="+dir)
	if code, out, _ := run(t, "hook", "nzbget", "--category", "movies"); code != 94 || strings.Contains(out, "skipping") {
		t.Errorf("nzbget without a category: exit %d\n%s", code, out)
	}
}

func TestHookSonarrEvents(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	b := write(t, filepath.Join(dir, "b.nfo"), "nfo\n")
	t.Run("test event with a missing tool fails visibly", func(t *testing.T) {
		// A misconfigured container, as seen from the arrs' test button:
		// the failure is exit 1 with the reason on both streams, and the
		// hook checks mkvpropedit itself because setup does not.
		for _, tool := range []string{"MKVPROPEDIT", "FFMPEG", "MKVMERGE"} {
			t.Setenv("AMUXIFY_"+tool, filepath.Join(t.TempDir(), "missing"))
			for _, a := range []string{"sonarr", "radarr"} {
				hookEnv(t, a+"_eventtype=Test", a+"_episodefile_path="+dir, a+"_moviefile_path="+dir)
				code, out, errs := run(t, "hook", a)
				if code != 1 || !strings.HasPrefix(out, "amuxify hook "+a+": test failed: ") || out != errs || !strings.Contains(out, strings.ToLower(tool)) {
					t.Errorf("%s without %s: exit %d\nstdout: %s\nstderr: %s", a, tool, code, out, errs)
				}
				if strings.Contains(out, "ready") {
					t.Errorf("%s: ready line without %s:\n%s", a, tool, out)
				}
			}
			testutil.Stubs(t)
		}
	})
	t.Run("download with paths", func(t *testing.T) {
		hookEnv(t, "sonarr_eventtype=Download", "sonarr_episodefile_paths="+a+"|"+b+"|", "sonarr_series_title=Show", "sonarr_release_title=Show.S01E01E02")
		code, out, errs := run(t, "hook", "sonarr")
		if code != 0 || errs != "" {
			t.Errorf("exit %d stderr %q", code, errs)
		}
		if !strings.HasPrefix(out, "amuxify hook sonarr: Show: Show.S01E01E02 (Download)\n") || !strings.Contains(out, "PASS  "+a+"\n") || !strings.Contains(out, "PASS  "+b+"\n") {
			t.Errorf("output:\n%s", out)
		}
	})
	t.Run("download without a label names the path", func(t *testing.T) {
		hookEnv(t, "radarr_eventtype=Download", "radarr_moviefile_path="+a)
		code, out, _ := run(t, "hook", "radarr")
		if code != 0 || !strings.HasPrefix(out, "amuxify hook radarr: "+a+" (Download)\n") {
			t.Errorf("exit %d output:\n%s", code, out)
		}
	})
	t.Run("original language reaches ingest as data", func(t *testing.T) {
		canary := filepath.Join(t.TempDir(), "pwned")
		for _, lang := range []string{"jpn", "$(touch " + canary + ")", "-o " + canary, strings.Repeat("x", 100000)} {
			hookEnv(t, "sonarr_eventtype=Download", "sonarr_episodefile_path="+a, "sonarr_series_originallanguage="+lang)
			code, _, errs := run(t, "--profile", "anime", "hook", "sonarr")
			if code != 0 {
				t.Errorf("%.20q: exit %d %s", lang, code, errs)
			}
		}
		if _, err := os.Lstat(canary); err == nil {
			t.Fatal("an original language value ran a command")
		}
	})
	t.Run("warn never fails an import by default", func(t *testing.T) {
		w := write(t, filepath.Join(t.TempDir(), "a.xyz"), "x\n")
		for _, a := range []string{"sonarr", "radarr"} {
			hookEnv(t, jobEnv(a, w)...)
			code, out, errs := run(t, "hook", a)
			if code != 0 || errs != "" || !strings.Contains(out, "WARN  "+w) {
				t.Errorf("%s: exit %d stderr %q\n%s", a, code, errs, out)
			}
		}
	})
	t.Run("other events are ignored", func(t *testing.T) {
		for _, ev := range []string{"Grab", "Rename", "EpisodeFileDelete", "SeriesDelete", "HealthIssue", "ApplicationUpdate", "ManualInteractionRequired", "download", "TEST"} {
			hookEnv(t, "sonarr_eventtype="+ev, "sonarr_episodefile_path="+a)
			code, out, _ := run(t, "hook", "sonarr")
			if code != 0 || !strings.HasPrefix(out, "amuxify hook sonarr: skipping, event "+ev+"; nothing to ingest\n") {
				t.Errorf("%s: exit %d\n%s", ev, code, out)
			}
		}
	})
}

// The connection test with real tools: exit 0 and one ready line naming the
// build, the profile and both tool versions.
func TestHookTestEventWithTools(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	ready := regexp.MustCompile(`^amuxify ` + regexp.QuoteMeta(Version) + `: hook (sonarr|radarr) ready \(profile homelab; ffmpeg: [^;\n]+; mkvmerge: [^;\n]+\)\n$`)
	for _, a := range []string{"sonarr", "radarr"} {
		hookEnv(t, a+"_eventtype=Test")
		code, out, errs := run(t, "hook", a)
		if code != 0 || errs != "" || !ready.MatchString(out) {
			t.Errorf("%s: exit %d stdout %q stderr %q", a, code, out, errs)
		}
		// The event takes precedence over any path variable and runs no
		// ingest.
		hookEnv(t, a+"_eventtype=Test", a+"_episodefile_path=/nonexistent", a+"_moviefile_path=/nonexistent")
		if code, out, _ := run(t, "hook", a); code != 0 || strings.Contains(out, "nonexistent") {
			t.Errorf("%s: exit %d\n%s", a, code, out)
		}
	}
	// A profile that cannot load fails the test with exit 1, on both
	// streams, because the arrs show stderr in the test dialog.
	hookEnv(t, "sonarr_eventtype=Test")
	code, out, errs := run(t, "--profile", "no-such-profile", "hook", "sonarr")
	if code != 1 || !strings.HasPrefix(out, "amuxify hook sonarr: test failed: ") || out != errs {
		t.Errorf("bad profile: exit %d stdout %q stderr %q", code, out, errs)
	}
}

func TestHookNZBGetSidecarTree(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	t.Run("blocked sidecar removed and the job marked bad", func(t *testing.T) {
		dir := t.TempDir()
		url := write(t, filepath.Join(dir, "x.url"), "[InternetShortcut]\nURL=http://x\n")
		nfo := write(t, filepath.Join(dir, "sub", "x.nfo"), "notes\n")
		hookEnv(t, jobEnv("nzbget", dir)...)
		code, out, errs := run(t, "--verbose", "hook", "nzbget", "--remove-blocked-sidecars")
		if code != 94 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		checkNZBGetOutput(t, out, errs, true)
		for _, want := range []string{"[INFO] amuxify hook nzbget: Job (status SUCCESS/SUCCESS/ALL)\n", "[INFO] BLOCK " + url + "\n", "[INFO] PASS  " + nfo + "\n", "SIDECAR_REMOVED", "[INFO] BLOCK: 2 file(s) BLOCK=1 PASS=1\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in:\n%s", want, out)
			}
		}
		if _, err := os.Lstat(url); err == nil {
			t.Error("x.url still exists")
		}
		if _, err := os.Lstat(nfo); err != nil {
			t.Error("x.nfo removed")
		}
	})
	t.Run("dry run touches nothing", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "x.url"), "x\n")
		write(t, filepath.Join(dir, "x.nfo"), "notes\n")
		state := t.TempDir()
		before := tree(t, dir)
		hookEnv(t, jobEnv("nzbget", dir)...)
		code, out, errs := run(t, "--dry-run", "--state-dir", state, "hook", "nzbget", "--remove-blocked-sidecars", "--quarantine")
		if code != 94 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		checkNZBGetOutput(t, out, errs, true)
		unchanged(t, before, tree(t, dir))
		if _, err := os.Lstat(filepath.Join(state, "quarantine")); err == nil {
			t.Error("dry run created the quarantine directory")
		}
	})
	t.Run("quarantine under the state dir", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "sub", "empty.mkv"), "")
		state := t.TempDir()
		hookEnv(t, jobEnv("nzbget", dir)...)
		code, out, errs := run(t, "--state-dir", state, "hook", "nzbget", "--quarantine")
		if code != 94 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		checkNZBGetOutput(t, out, errs, true)
		if _, err := os.Lstat(filepath.Join(state, "quarantine", "sub", "empty.mkv")); err != nil {
			t.Errorf("not quarantined: %v", err)
		}
	})
	t.Run("a file name cannot forge a control line", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("newlines are not allowed in file names on windows")
		}
		dir := t.TempDir()
		names := []string{"x\n[NZB] DIRECTORY=/etc.nfo", "y\n[NZB] MARK=BAD.nfo", "\n[nzb] NZBPR_amuxify=1.nfo", strings.Repeat("z", 200) + "\n[NZB] MARK=BAD.nfo"}
		for _, n := range names {
			write(t, filepath.Join(dir, n), "notes\n")
		}
		hookEnv(t, "NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY="+dir, "NZBPP_NZBNAME=Job\n[NZB] MARK=BAD", "NZBPP_CATEGORY=tv\n[NZB] DIRECTORY=/")
		code, out, errs := run(t, "--verbose", "hook", "nzbget")
		if code != 93 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		// Every file is PASS, so no line at all may be a command.
		checkNZBGetOutput(t, out, errs, false)
		if strings.Contains(out, "[INFO] [NZB]") {
			t.Errorf("control line forged:\n%s", out)
		}
		if !strings.HasPrefix(out, `[INFO] amuxify hook nzbget: Job\x0a[NZB] MARK=BAD (status SUCCESS)`+"\n") {
			t.Errorf("start line not kept on one line:\n%s", out)
		}
		// The names with a newline are printed as one escaped line each.
		if strings.Count(out, `\x0a[NZB] `) != 4 || strings.Contains(out, "\n[NZB]") || strings.Contains(out, "\n[nzb]") {
			t.Errorf("file names with a newline were not escaped:\n%s", out)
		}
	})
	t.Run("trace lines stay tagged", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "x.nfo"), "notes\n")
		hookEnv(t, jobEnv("nzbget", dir)...)
		code, out, errs := run(t, "--trace", "--verbose", "hook", "nzbget")
		if code != 93 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		checkNZBGetOutput(t, out, errs, false)
	})
}

// decodeReport parses one JSON report and checks the envelope.
func decodeReport(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var doc map[string]interface{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	var nulls []string
	walkNulls("$", doc, &nulls)
	if len(nulls) != 0 {
		t.Errorf("null values at %v", nulls)
	}
	if doc["schema"] != "amuxify.report/1" {
		t.Errorf("schema %v", doc["schema"])
	}
	if doc["command"] != "ingest" {
		t.Errorf("command %v", doc["command"])
	}
	return doc
}

func TestHookReportHasHookBlock(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	write(t, filepath.Join(dir, "b.xyz"), "x\n")
	for _, a := range adapters {
		t.Run(a, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "report.json")
			hookEnv(t, jobEnv(a, dir)...)
			code, stdout, errs := run(t, "hook", a, "--json-out", out, "--fail-on", "warn")
			if code != map[string]int{"nzbget": 94}[a]+map[string]int{"nzbget": 0, "sabnzbd": 1, "sonarr": 1, "radarr": 1}[a] {
				t.Errorf("exit %d\n%s%s", code, stdout, errs)
			}
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			doc := decodeReport(t, data)
			hook, _ := doc["hook"].(map[string]interface{})
			if hook == nil {
				t.Fatalf("no hook object:\n%s", data)
			}
			wantLabel, wantCat := "Job", "tv"
			wantEvent := "pp status 0"
			switch a {
			case "nzbget":
				wantEvent = "status SUCCESS/SUCCESS/ALL"
			case "sonarr":
				wantLabel, wantCat, wantEvent = "Show: Show.S01E01", "", "Download"
			case "radarr":
				wantLabel, wantCat, wantEvent = "Movie", "", "Download"
			}
			if hook["adapter"] != a || hook["event"] != wantEvent || hook["fail_on"] != "WARN" || int(hook["exit_code"].(float64)) != code {
				t.Errorf("hook object %v (exit %d)", hook, code)
			}
			if got, _ := hook["label"].(string); got != wantLabel {
				t.Errorf("label %q, want %q", got, wantLabel)
			}
			if got, _ := hook["category"].(string); got != wantCat {
				t.Errorf("category %q, want %q", got, wantCat)
			}
			if _, ok := hook["label"]; ok && wantLabel == "" {
				t.Error("empty label present")
			}
			if _, ok := hook["category"]; ok && wantCat == "" {
				t.Error("empty category present")
			}
			if doc["verdict"] != "WARN" {
				t.Errorf("verdict %v", doc["verdict"])
			}
			if fi, _ := os.Stat(out); fi != nil && fi.Mode().Perm()&0o022 != 0 {
				t.Errorf("report mode %v", fi.Mode())
			}
			// stdout keeps the log lines, not the document.
			if strings.HasPrefix(stdout, "{") || strings.Contains(stdout, `"schema"`) {
				t.Errorf("JSON on stdout with --json-out:\n%s", stdout)
			}
		})
	}
	t.Run("--json on stdout", func(t *testing.T) {
		// Under --json stdout is exactly one JSON document, nothing before
		// it and nothing after it; the adapter's start line and SABnzbd's
		// final line go to stderr instead, as documented.
		for _, a := range []string{"sabnzbd", "sonarr", "radarr"} {
			hookEnv(t, jobEnv(a, dir)...)
			code, stdout, errs := run(t, "--json", "hook", a)
			if code != 0 {
				t.Errorf("%s: exit %d %s", a, code, errs)
			}
			if !strings.HasPrefix(stdout, "{\n") || !strings.HasSuffix(stdout, "\n}\n") || strings.Count(stdout, "\n{") != 0 {
				t.Fatalf("%s: stdout is not a single document:\n%s", a, stdout)
			}
			doc := decodeReport(t, []byte(stdout))
			hook, _ := doc["hook"].(map[string]interface{})
			if hook["adapter"] != a || hook["fail_on"] != "FAIL" || int(hook["exit_code"].(float64)) != 0 {
				t.Errorf("%s: hook object %v", a, hook)
			}
			want := "amuxify hook " + a + ": "
			if !strings.HasPrefix(errs, want) {
				t.Errorf("%s: start line not first on stderr: %q", a, errs)
			}
			if a == "sabnzbd" && !strings.HasSuffix(errs, "\namuxify: WARN, 2 file(s)\n") {
				t.Errorf("sabnzbd: final line not last on stderr: %q", errs)
			}
			if a != "sabnzbd" && lines(errs)[len(lines(errs))-1] != strings.TrimSuffix(lines(errs)[0], "") && strings.Contains(errs, "file(s)") {
				t.Errorf("%s: a count line on stderr for a passing run: %q", a, errs)
			}
		}
		// A skipped job under --json writes nothing to stdout and the
		// skipping line to stderr.
		hookEnv(t, "sonarr_eventtype=Grab", "sonarr_episodefile_path="+dir)
		code, stdout, errs := run(t, "--json", "hook", "sonarr")
		if code != 0 || stdout != "" || errs != "amuxify hook sonarr: skipping, event Grab; nothing to ingest\n" {
			t.Errorf("skipped under --json: exit %d stdout %q stderr %q", code, stdout, errs)
		}
		// --json-out keeps the log lines on stdout, because stdout does not
		// carry the document then.
		hookEnv(t, jobEnv("sabnzbd", dir)...)
		out := filepath.Join(t.TempDir(), "r.json")
		if code, stdout, errs := run(t, "hook", "sabnzbd", "--json-out", out); code != 0 || !strings.HasPrefix(stdout, "amuxify hook sabnzbd: Job (pp status 0)\n") || !strings.HasSuffix(stdout, "\namuxify: WARN, 2 file(s)\n") || errs != "" {
			t.Errorf("--json-out: exit %d stdout %q stderr %q", code, stdout, errs)
		}
	})

	t.Run("a plain ingest has no hook object", func(t *testing.T) {
		code, stdout, _ := run(t, "--json", "ingest", dir)
		if code != 1 {
			t.Errorf("exit %d", code)
		}
		doc := decodeReport(t, []byte(stdout))
		if _, ok := doc["hook"]; ok {
			t.Error("hook object on a plain ingest")
		}
	})
}

// Guarantee 1 for --json-out: an existing file is never overwritten, a
// symlink is never followed, and the failure changes neither the exit code
// nor stdout.
func TestHookJSONOutNeverOverwrites(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	scratch := t.TempDir()
	existing := write(t, filepath.Join(scratch, "existing.json"), "precious\n")
	target := write(t, filepath.Join(scratch, "target.json"), "precious target\n")
	link := filepath.Join(scratch, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	dangling := filepath.Join(scratch, "dangling.json")
	if err := os.Symlink(filepath.Join(scratch, "never-created.json"), dangling); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(scratch, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := tree(t, scratch)
	for _, tc := range []struct {
		name, path string
	}{
		{"existing file", existing},
		{"symlink to a file", link},
		{"dangling symlink", dangling},
		{"directory", subdir},
		{"missing parent", filepath.Join(scratch, "no", "such", "dir", "r.json")},
		{"the download itself", filepath.Join(dir, "a.nfo")},
		{"empty path", ""},
		{"NUL in the path", "a\x00b.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, a := range []string{"sabnzbd", "nzbget"} {
				hookEnv(t, jobEnv(a, dir)...)
				args := []string{"hook", a}
				if tc.path != "" || tc.name == "empty path" {
					args = append(args, "--json-out", tc.path)
				}
				code, out, errs := run(t, args...)
				want := map[string]int{"sabnzbd": 0, "nzbget": 93}[a]
				if code != want {
					t.Errorf("%s: exit %d, want %d\n%s%s", a, code, want, out, errs)
				}
				if tc.path != "" && !strings.Contains(errs, "amuxify: json-out: ") {
					t.Errorf("%s: no json-out error on stderr: %q", a, errs)
				}
				if a == "nzbget" {
					checkNZBGetOutput(t, out, errs, false)
				}
			}
		})
	}
	unchanged(t, before, tree(t, scratch))
	for p, want := range map[string]string{existing: "precious\n", target: "precious target\n"} {
		if got, _ := os.ReadFile(p); string(got) != want {
			t.Errorf("%s changed to %q", p, got)
		}
	}
	if _, err := os.Lstat(filepath.Join(scratch, "never-created.json")); err == nil {
		t.Error("the dangling symlink's target was created")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.nfo")); string(got) != "nfo\n" {
		t.Error("the download was overwritten with the report")
	}
	// A second run with the same path fails the same way and keeps the first
	// report.
	fresh := filepath.Join(scratch, "fresh.json")
	hookEnv(t, jobEnv("sabnzbd", dir)...)
	if code, _, errs := run(t, "hook", "sabnzbd", "--json-out", fresh); code != 0 || errs != "" {
		t.Fatalf("first write: exit %d stderr %q", code, errs)
	}
	first, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errs := run(t, "hook", "sabnzbd", "--json-out", fresh); code != 0 || !strings.Contains(errs, "json-out") {
		t.Fatalf("second write: exit %d stderr %q", code, errs)
	}
	if second, _ := os.ReadFile(fresh); !bytes.Equal(first, second) {
		t.Error("the second run replaced the first report")
	}
}

// Everything a caller can put in the environment or on the command line is
// data: no value is executed, split, expanded or interpreted as a flag, the
// exit code stays inside the adapter's set, and a dry run changes nothing.
func TestHookHostileEnvironment(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	base := t.TempDir()
	dir := filepath.Join(base, "dl")
	nfo := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	write(t, filepath.Join(dir, "a.url"), "x\n")
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(dir, linked); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "pwned")
	before := tree(t, base)
	values := []string{
		dir + "; touch " + canary,
		"$(touch " + canary + ")",
		"`touch " + canary + "`",
		"-rf",
		"--quarantine=" + canary,
		"--remove-blocked-sidecars",
		dir + "\nBLOCK /forged\n[NZB] MARK=BAD",
		dir + "\r[INFO] PASS  /forged",
		"a\x00b",
		"‮" + dir,
		"​",
		"relative/dir",
		"..",
		dir + "/../../../../../../../../nonexistent",
		linked,
		nfo,
		"",
		" ",
		"/nonexistent-" + strings.Repeat("x", 100),
		strings.Repeat("x", 200000),
		dir + strings.Repeat("/", 5000),
	}
	envFor := func(a, v string) []string {
		switch a {
		case "sabnzbd":
			return []string{"SAB_COMPLETE_DIR=" + v, "SAB_FINAL_NAME=" + v, "SAB_CAT=" + v, "SAB_PP_STATUS=0", "SAB_FAIL_MSG=" + v}
		case "nzbget":
			return []string{"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY=" + v, "NZBPP_FINALDIR=" + v, "NZBPP_NZBNAME=" + v, "NZBPP_CATEGORY=" + v, "NZBPP_STATUS=" + v}
		case "sonarr":
			return []string{"sonarr_eventtype=Download", "sonarr_episodefile_paths=" + v + "|" + v, "sonarr_series_title=" + v, "sonarr_release_title=" + v, "sonarr_series_originallanguage=" + v}
		}
		return []string{"radarr_eventtype=Download", "radarr_moviefile_path=" + v, "radarr_movie_title=" + v, "radarr_movie_originallanguage=" + v}
	}
	allowed := map[string]map[int]bool{
		"sabnzbd": {0: true, 1: true, 2: true},
		"nzbget":  {93: true, 94: true, 95: true},
		"sonarr":  {0: true, 1: true, 2: true},
		"radarr":  {0: true, 1: true, 2: true},
	}
	for _, a := range adapters {
		for _, v := range values {
			hookEnv(t, envFor(a, v)...)
			code, out, errs := run(t, "--dry-run", "hook", a, "--remove-blocked-sidecars", "--quarantine", "--fail-on", "warn")
			if !allowed[a][code] {
				t.Errorf("%s %.30q: exit %d outside the adapter's set\n%s%s", a, v, code, out, errs)
			}
			if a == "nzbget" {
				checkNZBGetOutput(t, out, errs, strings.Contains(out, "BLOCK: "))
			} else {
				// amuxify itself never writes an NZBGet command for another
				// caller; the report writer echoes the value, so only an
				// exact command line would be one.
				for _, l := range append(lines(out), lines(errs)...) {
					if l == "[NZB] MARK=BAD" || strings.HasPrefix(l, "[INFO] amuxify") {
						t.Errorf("%s %.30q: NZBGet-style line %q", a, v, l)
					}
				}
			}
			// The hook's own line stays one line: the start line is first
			// and complete, and the forged verdict inside the value cannot
			// come out as the second line.
			ls := lines(out)
			if out == "" && (code == 2 || code == 94) {
				continue // a usage error: the value was refused before the start line
			}
			if len(ls) == 0 || !strings.HasPrefix(strings.TrimPrefix(ls[0], "[INFO] "), "amuxify hook "+a+": ") || strings.ContainsAny(ls[0], "\r\x00") {
				t.Errorf("%s %.30q: no complete start line: %q", a, v, out)
			}
			if len(ls) > 1 {
				second := strings.TrimPrefix(ls[1], "[INFO] ")
				if strings.HasPrefix(second, "BLOCK /forged") || strings.HasPrefix(second, "PASS  /forged") {
					t.Errorf("%s %.30q: the value split the start line: %q", a, v, ls[:2])
				}
			}
		}
		// Hostile event names and statuses.
		for _, ev := range []string{"Download; touch " + canary, "$(touch " + canary + ")", "Download\n", "\x00", strings.Repeat("D", 100000)} {
			hookEnv(t, a+"_eventtype="+ev, "SAB_COMPLETE_DIR="+dir, "SAB_PP_STATUS="+ev, "NZBPP_TOTALSTATUS="+ev, "NZBPP_DIRECTORY="+dir, "sonarr_episodefile_path="+dir, "radarr_moviefile_path="+dir)
			code, out, errs := run(t, "--dry-run", "hook", a)
			if !allowed[a][code] {
				t.Errorf("%s event %.20q: exit %d\n%s%s", a, ev, code, out, errs)
			}
			if a == "nzbget" {
				checkNZBGetOutput(t, out, errs, false)
			}
		}
	}
	unchanged(t, before, tree(t, base))
	if _, err := os.Lstat(canary); err == nil {
		t.Fatal("an environment value was executed or used as a flag")
	}
	for _, p := range []string{"/nonexistent-" + strings.Repeat("x", 100), filepath.Join(base, "quarantine")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
}

// Command-line injection: positional arguments and flag-shaped values after
// them are data for SABnzbd's argv form, extra arguments to the other
// adapters are ignored, and --fail-on can never turn BLOCK into a success.
func TestHookArgumentInjection(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "x\n")
	q := filepath.Join(t.TempDir(), "q")
	before := tree(t, dir)
	t.Run("flags after positionals are not flags", func(t *testing.T) {
		hookEnv(t)
		code, out, _ := run(t, "hook", "sabnzbd", dir, "--remove-blocked-sidecars", "--quarantine="+q, "1", "tv", "g", "0", "")
		if code != 1 || !strings.Contains(out, "BLOCK "+url) {
			t.Errorf("exit %d\n%s", code, out)
		}
		// The second positional (nzb name) became the label, not a flag.
		if !strings.HasPrefix(out, "amuxify hook sabnzbd: --quarantine="+q+" (pp status 0)\n") {
			t.Errorf("start line:\n%s", out)
		}
	})
	t.Run("a directory named like a flag after --", func(t *testing.T) {
		hookEnv(t)
		code, out, errs := run(t, "hook", "sabnzbd", "--", "--quarantine="+q, "n", "c", "1", "tv", "g", "0", "")
		if code != 1 || !strings.Contains(out, "ERROR --quarantine="+q+": ") || !strings.HasPrefix(out, "amuxify hook sabnzbd: c (pp status 0)\n") {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
	})
	t.Run("extra arguments to the arrs and nzbget are refused", func(t *testing.T) {
		for _, a := range []string{"sonarr", "radarr", "nzbget"} {
			hookEnv(t, jobEnv(a, dir)...)
			code, out, errs := run(t, "hook", a, "/etc", "--remove-blocked-sidecars", "--verify=none")
			want := 2
			if a == "nzbget" {
				want = 94
			}
			if code != want || out != "" {
				t.Errorf("%s: exit %d stdout %q", a, code, out)
			}
			if !strings.Contains(errs, `unexpected argument "/etc"`) {
				t.Errorf("%s: stderr %q does not name the argument", a, errs)
			}
			if strings.Contains(errs, "verify tier none") {
				t.Errorf("%s: the run got past the positional check: %q", a, errs)
			}
		}
	})
	t.Run("a hostile positional is refused before anything runs", func(t *testing.T) {
		canary := filepath.Join(t.TempDir(), "pwned")
		hostile := []string{"; rm -rf /", "$(touch " + canary + ")", "`touch " + canary + "`", "--quarantine=" + q, "\n[NZB] MARK=BAD", "-", "--"}
		for _, a := range adapters {
			for _, v := range hostile {
				hookEnv(t, jobEnv(a, dir)...)
				code, out, errs := run(t, "hook", a, "--quarantine", "--remove-blocked-sidecars", "--", v)
				want := 2
				if a == "nzbget" {
					want = 94
				}
				if code != want || out != "" {
					t.Errorf("%s %q: exit %d stdout %q stderr %q", a, v, code, out, errs)
				}
				// The argument is quoted on one stderr line with the hint.
				if ls := lines(errs); len(ls) != 1 || !strings.Contains(ls[0], fmt.Sprintf("%q", v)) || !strings.Contains(ls[0], "--quarantine="+report.Sanitize(v)) {
					t.Errorf("%s %q: stderr %q", a, v, errs)
				}
				if a == "nzbget" {
					checkNZBGetOutput(t, out, errs, false)
				}
			}
		}
		if _, err := os.Lstat(canary); err == nil {
			t.Fatal("a positional argument was executed")
		}
		if _, err := os.Lstat(url); err != nil {
			t.Fatal("a refused invocation removed the block file")
		}
	})
	t.Run("SABnzbd's eight parameters still pass", func(t *testing.T) {
		hookEnv(t)
		code, out, _ := run(t, "--dry-run", "hook", "sabnzbd", dir, "n", "c", "1", "tv", "g", "0", "")
		if code != 1 || !strings.Contains(out, "BLOCK "+url) {
			t.Errorf("exit %d\n%s", code, out)
		}
	})
	t.Run("SABnzbd's seven parameters from an older version still pass", func(t *testing.T) {
		hookEnv(t)
		if code, out, _ := run(t, "--dry-run", "hook", "sabnzbd", dir, "n", "c", "1", "tv", "g", "0"); code != 1 || !strings.Contains(out, "BLOCK "+url) {
			t.Errorf("seven parameters: exit %d\n%s", code, out)
		}
	})
	t.Run("a directory after a bare --quarantine in front of SABnzbd's parameters is refused", func(t *testing.T) {
		// The natural wrapper edit `hook sabnzbd --quarantine /q "$@"` puts
		// the directory first and SABnzbd's eight parameters after it. The
		// count is one too many, so the run is refused before the
		// environment is read, the hint names the directory, and the block
		// file is neither quarantined under q nor under the state
		// directory.
		sab := []string{dir, "n", "c", "1", "tv", "g", "0", ""}
		state := t.TempDir()
		for _, tc := range []struct {
			name string
			env  []string
		}{
			{"environment form", jobEnv("sabnzbd", dir)},
			{"argument form", nil},
		} {
			hookEnv(t, tc.env...)
			args := append([]string{"--state-dir", state, "hook", "sabnzbd", "--quarantine", "--remove-blocked-sidecars", q}, sab...)
			code, out, errs := run(t, args...)
			if code != 2 || out != "" {
				t.Errorf("%s: exit %d stdout %q stderr %q", tc.name, code, out, errs)
			}
			want := "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with " + fmt.Sprintf("%q", q) + "; if it was meant as the quarantine directory write --quarantine=" + q + "\n"
			if ls := lines(errs); len(ls) != 1 || !strings.HasSuffix(errs, want) {
				t.Errorf("%s: stderr %q, want a single line ending %q", tc.name, errs, want)
			}
			if _, err := os.Lstat(url); err != nil {
				t.Errorf("%s: the block file was moved or removed", tc.name)
			}
			if _, err := os.Lstat(q); err == nil {
				t.Errorf("%s: the refused directory was created", tc.name)
			}
			if _, err := os.Lstat(filepath.Join(state, "quarantine")); err == nil {
				t.Errorf("%s: the state quarantine directory was created", tc.name)
			}
		}
	})
	t.Run("a directory after a bare --quarantine in front of an older SABnzbd's seven parameters is refused", func(t *testing.T) {
		// The same wrapper edit against a SABnzbd that passes seven
		// parameters and no environment yields exactly eight positionals,
		// which the count alone accepts, and the parser would ingest the
		// stray directory as the completed one. The tell is args[1]: SABnzbd
		// never puts a directory second, so the shifted shape is refused
		// whatever stands at args[0], an existing directory, a symlink to
		// one, or a path the wrapper has not created yet. The run is refused
		// before anything runs, the message names the directory found second
		// and the hint names the stray path as written, the block file
		// stays, nothing is written under the stray directory or its symlink
		// target, a missing one is not created, and no state quarantine
		// directory appears. The fixture is this subtest's own copy so a
		// regression here cannot cascade into later subtests through the
		// shared a.url.
		fdir := t.TempDir()
		furl := write(t, filepath.Join(fdir, "a.url"), "x\n")
		fbefore := tree(t, fdir)
		sab := []string{fdir, "n", "c", "1", "tv", "g", "0"}
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		missing := filepath.Join(t.TempDir(), "q")
		check := func(t *testing.T, name, qdir, state string) {
			t.Helper()
			if _, err := os.Lstat(furl); err != nil {
				t.Errorf("%s: the block file was moved or removed", name)
			}
			unchanged(t, fbefore, tree(t, fdir))
			switch qdir {
			case missing:
				if _, err := os.Lstat(missing); err == nil {
					t.Errorf("%s: the missing directory was created", name)
				}
			case link:
				if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Errorf("%s: the symlink was replaced or removed: %v", name, err)
				}
				if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
					t.Errorf("%s: the symlink target was written to: %v %v", name, entries, err)
				}
			default:
				if entries, err := os.ReadDir(qdir); err != nil || len(entries) != 0 {
					t.Errorf("%s: the stray directory was written to: %v %v", name, entries, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(state, "quarantine")); err == nil {
				t.Errorf("%s: the state quarantine directory was created", name)
			}
		}
		for _, tc := range []struct {
			name string
			qdir string
		}{
			{"existing directory", t.TempDir()},
			{"symlink to a directory", link},
			{"not yet created", missing},
		} {
			state := t.TempDir()
			hookEnv(t)
			args := append([]string{"--state-dir", state, "hook", "sabnzbd", "--quarantine", "--remove-blocked-sidecars", tc.qdir}, sab...)
			code, out, errs := run(t, args...)
			if code != 2 || out != "" {
				t.Errorf("%s: exit %d stdout %q stderr %q", tc.name, code, out, errs)
			}
			want := "hook sabnzbd: " + fmt.Sprintf("%q", fdir) + " is a directory where SABnzbd's second parameter, the original NZB name, belongs; a directory in front of SABnzbd's parameters is not read as one of them; if it was meant as the quarantine directory write --quarantine=" + tc.qdir + "\n"
			if ls := lines(errs); len(ls) != 1 || !strings.HasSuffix(errs, want) {
				t.Errorf("%s: stderr %q, want a single line ending %q", tc.name, errs, want)
			}
			check(t, tc.name, tc.qdir, state)
		}
		// With SAB_COMPLETE_DIR set the job comes from the environment and
		// nothing on disk is inspected: the same eight positionals are the
		// right count, the environment's directory is scanned and the stray
		// one is neither ingested nor written to. A SABnzbd that sets the
		// environment passes eight parameters, so this shape only arises when
		// a wrapper also dropped one; the job is still scanned rather than
		// refused, because a refusal here would rest on the NZB name, which
		// the indexer chooses.
		for _, tc := range []struct {
			name string
			qdir string
		}{
			{"existing directory, environment form", t.TempDir()},
			{"symlink to a directory, environment form", link},
			{"not yet created, environment form", missing},
		} {
			state := t.TempDir()
			hookEnv(t, jobEnv("sabnzbd", fdir)...)
			args := append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--quarantine", "--remove-blocked-sidecars", tc.qdir}, sab...)
			code, out, errs := run(t, args...)
			if code != 1 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: Job (pp status 0)\n") || !strings.Contains(out, "BLOCK "+furl+"\n") {
				t.Errorf("%s: exit %d\n%s%s", tc.name, code, out, errs)
			}
			if strings.Contains(out, tc.qdir) {
				t.Errorf("%s: the stray directory was scanned:\n%s", tc.name, out)
			}
			check(t, tc.name, tc.qdir, state)
		}
	})
	t.Run("SABnzbd's genuine parameters pass with a bare --quarantine", func(t *testing.T) {
		// The second parameter is an NZB file name, so the shifted-shape
		// check leaves the documented eight and the older seven alone even
		// when the bare flag is set. The run reaches ingest and reports the
		// block file.
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"eight parameters", []string{dir, "Show.S01E01.nzb", "c", "1", "tv", "g", "0", ""}},
			{"seven parameters", []string{dir, "Show.S01E01.nzb", "c", "1", "tv", "g", "0"}},
		} {
			hookEnv(t)
			args := append([]string{"--dry-run", "hook", "sabnzbd", "--quarantine"}, tc.args...)
			code, out, errs := run(t, args...)
			if code != 1 || !strings.Contains(out, "BLOCK "+url) || !strings.HasPrefix(out, "amuxify hook sabnzbd: c (pp status 0)\n") {
				t.Errorf("%s: exit %d\n%s%s", tc.name, code, out, errs)
			}
		}
	})

	t.Run("fail-on cannot lower BLOCK", func(t *testing.T) {
		for _, a := range adapters {
			for _, failOn := range []string{"block", "BLOCK", " block "} {
				hookEnv(t, jobEnv(a, dir)...)
				code, out, errs := run(t, "--dry-run", "hook", a, "--fail-on", failOn)
				want := 1
				if a == "nzbget" {
					want = 94
					checkNZBGetOutput(t, out, errs, true)
				}
				if code != want {
					t.Errorf("%s --fail-on %q: exit %d\n%s", a, failOn, code, out)
				}
			}
			// Repeating the flag takes the last value, still no lower.
			hookEnv(t, jobEnv(a, dir)...)
			if code, _, _ := run(t, "--dry-run", "hook", a, "--fail-on", "warn", "--fail-on", "block"); code != map[string]int{"nzbget": 94}[a]+map[string]int{"nzbget": 0, "sabnzbd": 1, "sonarr": 1, "radarr": 1}[a] {
				t.Errorf("%s repeated --fail-on: exit %d", a, code)
			}
		}
	})
	unchanged(t, before, tree(t, dir))
	if _, err := os.Lstat(q); err == nil {
		t.Error("a positional argument created the quarantine directory")
	}
}

// The SABnzbd adapter's accepted argument forms, tried with hostile values.
// The environment form is read whenever SAB_COMPLETE_DIR is set, positionals
// are only SABnzbd's seven or eight parameters, flags go anywhere before the
// positionals, and every value that gets through is data. The symlinked
// directory is guarantee 3 in docs/safety.md; the rest is the untrusted
// caller paragraph that follows the ten guarantees there.
func TestHookSABnzbdArgumentForms(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "x\n")
	before := tree(t, dir)
	sab := func(d string, rest ...string) []string {
		return append([]string{d, "Show.S01E01.nzb", "Show", "1", "tv", "alt.binaries", "0", ""}, rest...)
	}
	t.Run("flags are recognised in any position before the positionals", func(t *testing.T) {
		state := t.TempDir()
		q := filepath.Join(t.TempDir(), "q")
		for _, args := range [][]string{
			append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--fail-on", "block", "--quarantine=" + q, "--remove-blocked-sidecars"}, sab(dir)...),
			append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q, "--fail-on=block", "--verify", "quick"}, sab(dir)...),
			append([]string{"--dry-run", "hook", "sabnzbd", "--remove-blocked-sidecars", "--state-dir", state, "--quarantine=" + q}, sab(dir)...),
			append([]string{"--dry-run", "hook", "sabnzbd", "--quarantine", "--quarantine=" + q}, sab(dir)...),
			append([]string{"--dry-run", "hook", "sabnzbd", "--quarantine=" + q, "--quarantine"}, sab(dir)...),
			append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--fail-on", "block", "--"}, sab(dir)...),
		} {
			hookEnv(t)
			code, out, errs := run(t, args...)
			if code != 1 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: Show (pp status 0)\n") || !strings.Contains(out, "BLOCK "+url) {
				t.Errorf("%q: exit %d\n%s%s", args, code, out, errs)
			}
			if _, err := os.Lstat(q); err == nil {
				t.Errorf("%q: the quarantine directory was created under --dry-run", args)
			}
		}
	})
	t.Run("a directory named --quarantine", func(t *testing.T) {
		// After -- the name is a positional. Alone it is not SABnzbd's
		// shape, so it is refused and never entered; as SABnzbd's first
		// parameter it is the completed directory and is scanned like any
		// other. With a bare --quarantine set, the hint spells the flag
		// form even for this name.
		base := t.TempDir()
		named := write(t, filepath.Join(base, "--quarantine", "b.url"), "x\n")
		t.Chdir(base)
		state := t.TempDir()
		hookEnv(t)
		code, out, errs := run(t, "--state-dir", state, "hook", "sabnzbd", "--", "--quarantine")
		if code != 2 || out != "" || !strings.HasSuffix(errs, `hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 1 beginning with "--quarantine"`+"\n") {
			t.Errorf("alone: exit %d stdout %q stderr %q", code, out, errs)
		}
		hookEnv(t)
		code, out, errs = run(t, "--state-dir", state, "hook", "sabnzbd", "--quarantine", "--", "--quarantine")
		if code != 2 || out != "" || !strings.HasSuffix(errs, "; if it was meant as the quarantine directory write --quarantine=--quarantine\n") {
			t.Errorf("after a bare --quarantine: exit %d stdout %q stderr %q", code, out, errs)
		}
		hookEnv(t)
		code, out, errs = run(t, append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--"}, sab("--quarantine")...)...)
		if code != 1 || errs != "" || !strings.Contains(out, "BLOCK "+named) {
			t.Errorf("as the completed directory: exit %d\n%s%s", code, out, errs)
		}
		if _, err := os.Lstat(named); err != nil {
			t.Error("the block file was moved or removed")
		}
	})
	t.Run("a completed directory with shell metacharacters and newlines", func(t *testing.T) {
		canary := filepath.Join(t.TempDir(), "pwned")
		name := "a; touch " + canary + " $(touch " + canary + ")\nBLOCK /forged\r[NZB] MARK=BAD"
		hostile := filepath.Join(t.TempDir(), name)
		hurl := write(t, filepath.Join(hostile, "a.url"), "x\n")
		hbefore := tree(t, hostile)
		state := t.TempDir()
		for _, tc := range []struct {
			name string
			env  []string
			args []string
		}{
			{"environment form", jobEnv("sabnzbd", hostile), []string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--remove-blocked-sidecars"}},
			{"seven parameters", nil, append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--remove-blocked-sidecars"}, sab(hostile)[:7]...)},
			{"eight parameters", nil, append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--remove-blocked-sidecars"}, sab(hostile)...)},
		} {
			hookEnv(t, tc.env...)
			code, out, errs := run(t, tc.args...)
			if code != 1 || errs != "" || !strings.Contains(out, "BLOCK "+report.Sanitize(hurl)) {
				t.Errorf("%s: exit %d\n%s%s", tc.name, code, out, errs)
			}
			ls := lines(out)
			if len(ls) == 0 || !strings.HasPrefix(ls[0], "amuxify hook sabnzbd: ") || strings.ContainsAny(out, "\r\x00") {
				t.Errorf("%s: start line or raw control characters: %q", tc.name, out)
			}
			for _, l := range ls {
				if l == "BLOCK /forged" || l == "[NZB] MARK=BAD" {
					t.Errorf("%s: the directory name forged a line: %q", tc.name, l)
				}
			}
			noControlLines(t, out)
			unchanged(t, hbefore, tree(t, hostile))
		}
		if _, err := os.Lstat(canary); err == nil {
			t.Fatal("the directory name was executed")
		}
	})
	t.Run("a directory after SABnzbd's eight parameters is one too many", func(t *testing.T) {
		// The wrapper edit `hook sabnzbd --quarantine "$@" /q` against a
		// current SABnzbd makes nine, refused on count. With a bare
		// --quarantine the hint names the stray directory, which is last,
		// and never SABnzbd's job directory, which is first: a user who
		// followed a hint naming the job directory would make a downloaded
		// release tree the quarantine root. When the indexer's failure URL
		// is a bare path or a number the shape cannot be told and no hint is
		// printed, also when the wrapper's stray argument is empty, as
		// `hook sabnzbd --quarantine "$@" "$EXTRA"` gives with EXTRA unset:
		// an empty last argument has the failure URL's shape, and only the
		// status slot then tells the two readings apart.
		stray := t.TempDir()
		for _, env := range [][]string{jobEnv("sabnzbd", dir), nil} {
			for _, failURL := range []string{"", "https://indexer/report/1"} {
				for _, bare := range []bool{false, true} {
					hookEnv(t, env...)
					args := []string{"hook", "sabnzbd", "--remove-blocked-sidecars"}
					if bare {
						args = append(args, "--quarantine")
					}
					eight := sab(dir)
					eight[7] = failURL
					code, out, errs := run(t, append(append(args, eight...), stray)...)
					want := "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with " + fmt.Sprintf("%q", dir)
					if bare {
						want += "; if it was meant as the quarantine directory write --quarantine=" + stray
					}
					if code != 2 || out != "" || !strings.HasSuffix(errs, want+"\n") {
						t.Errorf("env=%v url=%q bare=%v: exit %d stdout %q stderr %q", env != nil, failURL, bare, code, out, errs)
					}
					if strings.Contains(errs, "--quarantine="+dir) {
						t.Errorf("env=%v url=%q bare=%v: the hint names the job directory: %q", env != nil, failURL, bare, errs)
					}
				}
			}
			for _, failURL := range []string{"/", "0", "-1"} {
				for _, last := range []string{stray, ""} {
					hookEnv(t, env...)
					eight := sab(dir)
					eight[7] = failURL
					code, out, errs := run(t, append(append([]string{"hook", "sabnzbd", "--quarantine"}, eight...), last)...)
					want := "hook sabnzbd: expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with " + fmt.Sprintf("%q", dir) + "\n"
					if code != 2 || out != "" || !strings.HasSuffix(errs, want) {
						t.Errorf("env=%v url=%q last=%q: exit %d stdout %q stderr %q", env != nil, failURL, last, code, out, errs)
					}
					if strings.Contains(errs, "--quarantine=") {
						t.Errorf("env=%v url=%q last=%q: a hint was printed: %q", env != nil, failURL, last, errs)
					}
				}
			}
		}
		if entries, err := os.ReadDir(stray); err != nil || len(entries) != 0 {
			t.Errorf("the stray directory was written to: %v %v", entries, err)
		}
	})
	t.Run("a directory after an older SABnzbd's seven parameters is the failure URL", func(t *testing.T) {
		// The wrapper edit `hook sabnzbd "$@" /q` against a SABnzbd that
		// passes seven parameters makes eight, and the directory stands where
		// the failure URL belongs. That position is never inspected, because
		// SABnzbd fills it from the indexer's X-DNZB-Failure header and an
		// existing directory there must not refuse a job (see the next
		// subtest). So the job's own directory is scanned, the stray one is
		// ignored as the failure URL, never entered and never written to, and
		// a bare --quarantine prints no hint because nothing was refused.
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		missingURL := filepath.Join(t.TempDir(), "not-a-url")
		for _, stray := range []string{t.TempDir(), link, missingURL} {
			for _, bare := range []bool{false, true} {
				hookEnv(t)
				args := []string{"--dry-run", "hook", "sabnzbd", "--remove-blocked-sidecars"}
				if bare {
					args = append(args, "--quarantine")
				}
				code, out, errs := run(t, append(append(args, sab(dir)[:7]...), stray)...)
				if code != 1 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: Show (pp status 0)\n") || !strings.Contains(out, "BLOCK "+url+"\n") {
					t.Errorf("stray %s bare=%v: exit %d\n%s%s", stray, bare, code, out, errs)
				}
				if strings.Contains(out, stray) {
					t.Errorf("stray %s bare=%v: the failure URL was scanned:\n%s", stray, bare, out)
				}
			}
		}
		if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
			t.Errorf("the symlink target was written to: %v %v", entries, err)
		}
		if _, err := os.Lstat(missingURL); err == nil {
			t.Error("the missing path was created")
		}
		unchanged(t, before, tree(t, dir))
	})
	t.Run("an indexer's failure URL never refuses the job", func(t *testing.T) {
		// SABnzbd passes the X-DNZB-Failure header of the NZB response as
		// the eighth parameter and as SAB_FAILURE_URL, unchanged. An indexer
		// that sends "/" there must not make the hook exit 2, because under
		// SABnzbd's default script_can_fail=off a usage error leaves the job
		// successful and Sonarr or Radarr import the release unscanned. Every
		// value here is passed with a genuine job in both forms; each run
		// prints the start line and scans the job directory, and the value is
		// never opened.
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		state := t.TempDir()
		hostile := []string{"/", ".", "..", "/tmp", dir, dir + "/", cwd, "file:///", "//", "--quarantine", "-", "\n", " ", "\x00"}
		for _, v := range hostile {
			for _, tc := range []struct {
				name string
				env  []string
				n    int
			}{
				{"environment form with eight parameters", append(jobEnv("sabnzbd", dir), "SAB_FAILURE_URL="+v), 8},
				{"eight parameters", nil, 8},
			} {
				hookEnv(t, tc.env...)
				args := sab(dir)[:tc.n]
				args[7] = v
				code, out, errs := run(t, append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--quarantine", "--remove-blocked-sidecars"}, args...)...)
				if code != 1 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: ") || !strings.Contains(out, "BLOCK "+url+"\n") {
					t.Errorf("%s, failure URL %q: exit %d\n%s%s", tc.name, v, code, out, errs)
				}
				if strings.Contains(errs, "eighth parameter") || strings.Contains(errs, "usage") {
					t.Errorf("%s, failure URL %q: refused: %q", tc.name, v, errs)
				}
			}
		}
		unchanged(t, before, tree(t, dir))
	})
	t.Run("an NZB named like a directory in the working directory never refuses the job", func(t *testing.T) {
		// The second parameter is the original NZB name, which the indexer
		// chooses, and the shifted-shape check must not resolve it against
		// the script's working directory: a subdirectory of that name would
		// otherwise refuse every job served under it, which under SABnzbd's
		// default script_can_fail=off leaves the job successful and unscanned.
		// A bare name is never looked up, so the seven- and eight-parameter
		// jobs are scanned, with and without a bare --quarantine, and the
		// subdirectory is neither entered nor written to. A directory named
		// with a path in that place is still the shifted shape.
		cwd := t.TempDir()
		named := filepath.Join(cwd, "Show.S01E01.nzb")
		if err := os.MkdirAll(filepath.Join(cwd, "rel", "dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(named, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(cwd)
		state := t.TempDir()
		for _, n := range []int{7, 8} {
			for _, bare := range []bool{false, true} {
				hookEnv(t)
				args := []string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--remove-blocked-sidecars"}
				if bare {
					args = append(args, "--quarantine")
				}
				code, out, errs := run(t, append(args, sab(dir)[:n]...)...)
				if code != 1 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: Show (pp status 0)\n") || !strings.Contains(out, "BLOCK "+url+"\n") {
					t.Errorf("%d parameters bare=%v: exit %d\n%s%s", n, bare, code, out, errs)
				}
				if strings.Contains(out, named) || strings.Contains(errs, "second parameter") {
					t.Errorf("%d parameters bare=%v: the NZB name was looked up:\n%s%s", n, bare, out, errs)
				}
			}
		}
		if entries, err := os.ReadDir(named); err != nil || len(entries) != 0 {
			t.Errorf("the directory named like the NZB was written to: %v %v", entries, err)
		}
		hookEnv(t)
		shifted := append([]string{filepath.Join(t.TempDir(), "q"), filepath.Join("rel", "dir")}, sab(dir)[1:7]...)
		code, out, errs := run(t, append([]string{"--dry-run", "--state-dir", state, "hook", "sabnzbd"}, shifted...)...)
		if code != 2 || out != "" || !strings.Contains(errs, fmt.Sprintf("%q", filepath.Join("rel", "dir"))+" is a directory where SABnzbd's second parameter") {
			t.Errorf("relative directory second: exit %d stdout %q stderr %q", code, out, errs)
		}
		unchanged(t, before, tree(t, dir))
	})
	t.Run("seven parameters under the environment form are refused", func(t *testing.T) {
		// A SABnzbd that sets SAB_COMPLETE_DIR passes eight parameters, so
		// seven means a flag that takes a value stands before "$@" in the
		// wrapper and swallowed the job directory. With `--category "$@"`
		// the job directory becomes the category and the failure URL lands
		// in the status slot, so a job whose indexer set one would run and
		// then be skipped as a category mismatch; with `--state-dir "$@"`
		// and a bare --quarantine the state directory would be the release
		// tree and BLOCK files would be quarantined into it. Both are
		// refused before anything runs, the message says what happened, no
		// hint names a value, and nothing is created under the job.
		jdir := t.TempDir()
		jurl := write(t, filepath.Join(jdir, "a.url"), "x\n")
		jbefore := tree(t, jdir)
		rest := []string{"Show.nzb", "Show", "1", "tv", "alt.binaries", "0"}
		want := `hook sabnzbd: SABnzbd set SAB_COMPLETE_DIR and passes eight parameters, got 7 beginning with "Show.nzb"; a flag that takes a value written before "$@" in the wrapper swallows the first one` + "\n"
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"--category swallows the directory", append(append([]string{"hook", "sabnzbd", "--category", jdir}, rest...), "https://indexer/fail/1")},
			{"--category swallows the directory and the indexer set no failure URL", append(append([]string{"hook", "sabnzbd", "--category", jdir}, rest...), "")},
			{"--state-dir swallows the directory under a bare --quarantine", append(append([]string{"hook", "sabnzbd", "--state-dir", jdir, "--quarantine", "--remove-blocked-sidecars"}, rest...), "https://indexer/fail/1")},
			{"--json-out swallows the directory", append(append([]string{"hook", "sabnzbd", "--json-out", jdir}, rest...), "")},
		} {
			hookEnv(t, jobEnv("sabnzbd", jdir)...)
			code, out, errs := run(t, tc.args...)
			if code != 2 || out != "" || !strings.HasSuffix(errs, want) {
				t.Errorf("%s: exit %d stdout %q stderr %q", tc.name, code, out, errs)
			}
			if strings.Contains(errs, "--quarantine=") {
				t.Errorf("%s: a hint was printed: %q", tc.name, errs)
			}
			if _, err := os.Lstat(jurl); err != nil {
				t.Errorf("%s: the block file was moved or removed", tc.name)
			}
			unchanged(t, jbefore, tree(t, jdir))
		}
		if _, err := os.Lstat(filepath.Join(jdir, "quarantine")); err == nil {
			t.Error("a quarantine directory was created inside the release tree")
		}
		// The same seven without the environment are an older SABnzbd's
		// call and are scanned.
		hookEnv(t)
		code, out, errs := run(t, append([]string{"--dry-run", "hook", "sabnzbd", jdir}, rest...)...)
		if code != 1 || errs != "" || !strings.Contains(out, "BLOCK "+jurl+"\n") {
			t.Errorf("positional form: exit %d\n%s%s", code, out, errs)
		}
	})
	t.Run("status -1 with a directory that does not exist", func(t *testing.T) {
		// A failed job is skipped on the status alone. The directory is
		// never stat'ed or created, and neither is the quarantine directory:
		// the skip decision comes before every filesystem check.
		missing := filepath.Join(t.TempDir(), "gone")
		q := filepath.Join(t.TempDir(), "q")
		state := t.TempDir()
		for _, tc := range []struct {
			name string
			env  []string
			args []string
		}{
			{"environment form", []string{"SAB_COMPLETE_DIR=" + missing, "SAB_PP_STATUS=-1", "SAB_FAIL_MSG=Download failed"}, []string{"--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q, "--remove-blocked-sidecars", "--fail-on", "warn"}},
			{"seven parameters", nil, append([]string{"--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q, "--fail-on", "warn"}, missing, "j.nzb", "j", "1", "tv", "g", "-1")},
			{"eight parameters", nil, append([]string{"--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q}, missing, "j.nzb", "j", "1", "tv", "g", "-1", "https://indexer/fail")},
		} {
			hookEnv(t, tc.env...)
			code, out, errs := run(t, tc.args...)
			if code != 0 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: skipping, post-processing status -1; the files are not usable") {
				t.Errorf("%s: exit %d stdout %q stderr %q", tc.name, code, out, errs)
			}
			for _, p := range []string{missing, q, filepath.Join(state, "quarantine")} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s: %s was created", tc.name, p)
				}
			}
		}
	})
	t.Run("SAB_COMPLETE_DIR is a symlink", func(t *testing.T) {
		// Guarantee 3: the link is reported and never followed. The block
		// file behind it is neither found nor quarantined, and the link
		// itself stays a link.
		target := t.TempDir()
		turl := write(t, filepath.Join(target, "a.url"), "x\n")
		tbefore := tree(t, target)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		q := filepath.Join(t.TempDir(), "q")
		state := t.TempDir()
		for _, tc := range []struct {
			name string
			env  []string
			args []string
		}{
			{"environment form", jobEnv("sabnzbd", link), []string{"--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q, "--remove-blocked-sidecars", "--fail-on", "warn"}},
			{"eight parameters", nil, append([]string{"--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q, "--remove-blocked-sidecars", "--fail-on", "warn"}, sab(link)...)},
		} {
			hookEnv(t, tc.env...)
			code, out, errs := run(t, tc.args...)
			if code != 1 || !strings.Contains(out, "WARN  "+link) || !strings.Contains(out, "SYMLINK") || strings.Contains(out, "BLOCK") || strings.Contains(out, turl) {
				t.Errorf("%s: exit %d\n%s%s", tc.name, code, out, errs)
			}
			if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s: the symlink was replaced or removed: %v", tc.name, err)
			}
			unchanged(t, tbefore, tree(t, target))
			if _, err := os.Lstat(q); err == nil {
				t.Errorf("%s: the quarantine directory was created", tc.name)
			}
		}
	})
	t.Run("SAB_COMPLETE_DIR is the quarantine root", func(t *testing.T) {
		// Guarantee 10: the quarantine directory never lies inside the tree
		// it serves. The job is refused before anything runs, in every
		// spelling: the root itself, a subdirectory of it, a symlink to it,
		// and the root with a trailing slash.
		q := t.TempDir()
		qurl := write(t, filepath.Join(q, "sub", "a.url"), "x\n")
		qbefore := tree(t, q)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(q, link); err != nil {
			t.Fatal(err)
		}
		state := t.TempDir()
		for _, root := range []string{q, filepath.Join(q, "sub"), link, q + "/", q + "///", filepath.Join(q, "sub", "..", "sub")} {
			for _, env := range [][]string{jobEnv("sabnzbd", root), nil} {
				hookEnv(t, env...)
				args := []string{"--state-dir", state, "hook", "sabnzbd", "--quarantine=" + q, "--remove-blocked-sidecars"}
				if env == nil {
					args = append(args, sab(root)...)
				}
				code, out, errs := run(t, args...)
				if code != 2 || out != "" || !strings.Contains(errs, "the quarantine directory must lie outside the tree it serves") {
					t.Errorf("root %s env=%v: exit %d stdout %q stderr %q", root, env != nil, code, out, errs)
				}
			}
		}
		unchanged(t, qbefore, tree(t, q))
		if _, err := os.Lstat(qurl); err != nil {
			t.Error("the block file inside the quarantine directory was moved")
		}
	})
	t.Run("SAB_COMPLETE_DIR with a trailing slash", func(t *testing.T) {
		// SABnzbd does not add one, but a wrapper might. The path is cleaned
		// before it is scanned, so the report names the file without a
		// doubled separator and the run behaves as without the slash.
		state := t.TempDir()
		for _, root := range []string{dir + "/", dir + "//"} {
			for _, env := range [][]string{jobEnv("sabnzbd", root), nil} {
				hookEnv(t, env...)
				args := []string{"--dry-run", "--state-dir", state, "hook", "sabnzbd", "--remove-blocked-sidecars"}
				if env == nil {
					args = append(args, sab(root)...)
				}
				code, out, errs := run(t, args...)
				if code != 1 || errs != "" || !strings.Contains(out, "BLOCK "+url+"\n") || strings.Contains(out, dir+"//") {
					t.Errorf("root %q env=%v: exit %d\n%s%s", root, env != nil, code, out, errs)
				}
			}
		}
	})
	t.Run("environment values with NUL and control characters", func(t *testing.T) {
		// Every value is data. A status that is not exactly 0 skips the job
		// and is echoed sanitised on one line; a NUL or escape sequence in
		// the name or the category cannot break the start line or reach the
		// terminal raw. The exit code stays within SABnzbd's set.
		state := t.TempDir()
		for _, v := range []string{"0\x00", "0\n", "\x000", "0\x1b[2J", "\x1b]0;pwned\x07", "0\r", "-1\n[NZB] MARK=BAD"} {
			hookEnv(t, "SAB_COMPLETE_DIR="+dir, "SAB_PP_STATUS="+v, "SAB_FAIL_MSG=x\x00y\x1b[31m")
			code, out, errs := run(t, "--state-dir", state, "hook", "sabnzbd", "--remove-blocked-sidecars")
			if code != 0 || errs != "" || !strings.HasPrefix(out, "amuxify hook sabnzbd: skipping, post-processing status "+report.Sanitize(v)+"; the files are not usable: x\\x00y\\x1b[31m\n") {
				t.Errorf("status %q: exit %d stdout %q stderr %q", v, code, out, errs)
			}
			if len(lines(out)) != 1 || strings.ContainsAny(out, "\x00\x1b\r\x07") {
				t.Errorf("status %q: raw control characters or extra lines: %q", v, out)
			}
			noControlLines(t, out)
		}
		for _, v := range []string{"Show\x00Name", "Show\x1b[2J", "\x1b]0;pwned\x07", "Show\nBLOCK /forged", "Show\r[NZB] MARK=BAD", "\x7f"} {
			hookEnv(t, "SAB_COMPLETE_DIR="+dir, "SAB_PP_STATUS=0", "SAB_FINAL_NAME="+v, "SAB_CAT="+v)
			code, out, errs := run(t, "--dry-run", "--state-dir", state, "hook", "sabnzbd", "--category", "Show*", "--remove-blocked-sidecars")
			if code != 0 && code != 1 {
				t.Errorf("name %q: exit %d\n%s%s", v, code, out, errs)
			}
			ls := lines(out)
			if len(ls) == 0 || !strings.HasPrefix(ls[0], "amuxify hook sabnzbd: ") || strings.ContainsAny(out, "\x00\x1b\r\x07\x7f") {
				t.Errorf("name %q: start line or raw control characters: %q", v, out)
			}
			for _, l := range ls {
				if l == "BLOCK /forged" || l == "[NZB] MARK=BAD" {
					t.Errorf("name %q: the value forged a line: %q", v, l)
				}
			}
			noControlLines(t, out)
		}
	})
	unchanged(t, before, tree(t, dir))
	if _, err := os.Lstat(url); err != nil {
		t.Fatal("the block file was moved or removed")
	}
}

// The shipped wrapper scripts are executable POSIX sh that parses, with the
// documented settings blocks.
func TestContribHooks(t *testing.T) {
	root := filepath.Join("..", "..", "contrib", "hooks")
	sh, shErr := osexec.LookPath("sh")
	for _, tc := range []struct {
		name     string
		contains []string
		absent   []string
	}{
		{"amuxify-sabnzbd.sh", []string{`exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook sabnzbd "$@"`, "Config, Special"}, []string{"Config, Switches"}},
		{"amuxify-nzbget.sh", []string{
			"### NZBGET POST-PROCESSING SCRIPT",
			"### OPTIONS",
			"#Profile=homelab",
			"#FailOn=fail",
			`exec amuxify --profile "${NZBPO_PROFILE:-${AMUXIFY_PROFILE:-homelab}}" hook nzbget --fail-on "${NZBPO_FAILON:-fail}"`,
		}, []string{"eval"}},
		{"amuxify-sonarr.sh", []string{`exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook sonarr`, "On Import, On Upgrade and On Import Complete"}, []string{"eval"}},
		{"amuxify-radarr.sh", []string{`exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook radarr`, "On Import and On Upgrade."}, []string{"eval"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, tc.name)
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			s := string(data)
			if !strings.HasPrefix(s, "#!/bin/sh\n") {
				t.Errorf("first line %q", strings.SplitN(s, "\n", 2)[0])
			}
			if strings.Contains(s, "\r") {
				t.Error("carriage returns in the script")
			}
			for _, c := range tc.contains {
				if !strings.Contains(s, c) {
					t.Errorf("missing %q", c)
				}
			}
			for _, c := range tc.absent {
				if strings.Contains(s, c) {
					t.Errorf("contains %q", c)
				}
			}
			if n := strings.Count(s, "### NZBGET POST-PROCESSING SCRIPT"); n > 0 && n != 2 {
				t.Error("the NZBGet signature block is not closed")
			}
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 != 0o111 {
				t.Errorf("mode %v is not executable by everyone", fi.Mode())
			}
			if shErr != nil {
				t.Skip("sh not found")
			}
			if out, err := osexec.Command(sh, "-n", p).CombinedOutput(); err != nil {
				t.Errorf("sh -n: %v\n%s", err, out)
			}
			// Every wrapper honours AMUXIFY_PROFILE the same way: it is
			// the fallback for the profile, with homelab behind it, on the
			// one exec line. NZBGet's own option comes first there.
			if strings.Count(s, `${AMUXIFY_PROFILE:-homelab}`) != 1 || strings.Count(s, "--profile \"") != 1 {
				t.Errorf("AMUXIFY_PROFILE is not passed through exactly once:\n%s", s)
			}
			execLine := ""
			for _, l := range strings.Split(s, "\n") {
				if strings.HasPrefix(l, "exec ") {
					execLine = l
				}
			}
			if !strings.Contains(execLine, `${AMUXIFY_PROFILE:-homelab}`) {
				t.Errorf("the exec line does not read AMUXIFY_PROFILE: %q", execLine)
			}
			if shErr == nil {
				// Run the exec line with a fake amuxify on PATH and see
				// which profile it receives: AMUXIFY_PROFILE from the
				// environment, and for NZBGet the NZBPO_PROFILE option
				// first.
				bin := t.TempDir()
				fake := filepath.Join(bin, "amuxify")
				if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				for _, tc := range []struct {
					env  []string
					want string
				}{
					{nil, "homelab"},
					{[]string{"AMUXIFY_PROFILE=anime"}, "anime"},
					{[]string{"AMUXIFY_PROFILE=anime", "NZBPO_PROFILE=strict"}, map[bool]string{true: "strict", false: "anime"}[tc.name == "amuxify-nzbget.sh"]},
				} {
					cmd := osexec.Command(sh, p)
					cmd.Env = append([]string{"PATH=" + bin}, tc.env...)
					out, err := cmd.Output()
					if err != nil {
						t.Fatalf("%v: %v", tc.env, err)
					}
					args := strings.Split(strings.TrimSpace(string(out)), "\n")
					if len(args) < 2 || args[0] != "--profile" || args[1] != tc.want {
						t.Errorf("%v: the wrapper passed %q, want --profile %s", tc.env, args, tc.want)
					}
				}
			}
		})
	}
	t.Run("Dockerfile.sabnzbd", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join(root, "Dockerfile.sabnzbd"))
		if err != nil {
			t.Fatal(err)
		}
		// The image pulls the release named in VERSION, so a bump that
		// forgets the Dockerfile fails here as well as in release-check.
		version, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
		if err != nil {
			t.Fatal(err)
		}
		image := "ghcr.io/amuxify/amuxify:" + strings.TrimSpace(string(version))
		// The wrapper is installed outside /config, so a bind mount of
		// /config cannot hide it; the docs say how to point SABnzbd at it.
		for _, c := range []string{"FROM lscr.io/linuxserver/sabnzbd:", "COPY --from=" + image + " /usr/local/bin/amuxify /usr/local/bin/amuxify", "/usr/share/amuxify/hooks/amuxify-sabnzbd.sh /usr/local/share/amuxify/hooks/"} {
			if !strings.Contains(string(data), c) {
				t.Errorf("missing %q", c)
			}
		}
		if strings.Contains(string(data), "/config/") {
			t.Errorf("the image writes under /config, which a bind mount hides:\n%s", data)
		}
		docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "hooks.md"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(docs), string(data)) {
			t.Error("docs/hooks.md does not show contrib/hooks/Dockerfile.sabnzbd verbatim")
		}
		if strings.Contains(string(docs), "/config/scripts/\n") {
			t.Error("docs/hooks.md still copies a wrapper into /config/scripts/")
		}
	})

	t.Run("packaging ships the scripts", func(t *testing.T) {
		df, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(df), "COPY contrib/hooks /usr/share/amuxify/hooks") {
			t.Error("Dockerfile does not copy contrib/hooks")
		}
		gr, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gr), "contrib/hooks/*") {
			t.Error(".goreleaser.yaml does not ship contrib/hooks")
		}
	})
}

// The adapters over the real corpus: SABnzbd and NZBGet see the BLOCK from
// the polyglot as a failure, every file gets its line, and a dry run changes
// nothing.
func TestHookDryRunOnFixtures(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	corpus := testutil.CopyTree(t)
	before := tree(t, corpus)
	files := 0
	for p, v := range before {
		if v != "dir" && filepath.Base(p) != ".DS_Store" {
			files++
		}
	}
	countVerdicts := func(out, prefix string) int {
		n := 0
		for _, l := range lines(out) {
			l = strings.TrimPrefix(l, prefix)
			if strings.HasPrefix(l, "PASS  ") || strings.HasPrefix(l, "WARN  ") || strings.HasPrefix(l, "FAIL  ") || strings.HasPrefix(l, "BLOCK ") {
				n++
			}
		}
		return n
	}
	t.Run("sabnzbd", func(t *testing.T) {
		report := filepath.Join(t.TempDir(), "r.json")
		hookEnv(t, "SAB_COMPLETE_DIR="+corpus, "SAB_FINAL_NAME=corpus", "SAB_CAT=tv", "SAB_PP_STATUS=0")
		code, out, errs := run(t, "--dry-run", "hook", "sabnzbd", "--remove-blocked-sidecars", "--hardlinks", "break", "--quarantine", "--json-out", report)
		if code != 1 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		if !strings.HasPrefix(out, "amuxify hook sabnzbd: corpus (pp status 0)\n") || !strings.HasSuffix(out, "\namuxify: BLOCK, "+fmt.Sprint(files)+" file(s)\n") {
			t.Errorf("start or final line:\n%s", out)
		}
		if n := countVerdicts(out, ""); n != files {
			t.Errorf("%d verdict lines for %d files", n, files)
		}
		if !strings.Contains(out, "BLOCK "+filepath.Join(corpus, "polyglot.mkv")+"\n") {
			t.Errorf("polyglot.mkv not BLOCK:\n%s", out)
		}
		noControlLines(t, out)
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		doc := decodeReport(t, data)
		hook, _ := doc["hook"].(map[string]interface{})
		if hook["adapter"] != "sabnzbd" || int(hook["exit_code"].(float64)) != 1 || doc["verdict"] != "BLOCK" {
			t.Errorf("hook %v verdict %v", hook, doc["verdict"])
		}
		if n, _ := doc["files"].([]interface{}); len(n) != files {
			t.Errorf("%d files in the report", len(n))
		}
	})
	t.Run("nzbget", func(t *testing.T) {
		hookEnv(t, "NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY="+corpus, "NZBPP_NZBNAME=corpus", "NZBPP_CATEGORY=tv")
		code, out, errs := run(t, "--dry-run", "hook", "nzbget", "--remove-blocked-sidecars", "--hardlinks", "break", "--quarantine")
		if code != 94 {
			t.Errorf("exit %d\n%s%s", code, out, errs)
		}
		checkNZBGetOutput(t, out, errs, true)
		if !strings.HasPrefix(out, "[INFO] amuxify hook nzbget: corpus (status SUCCESS)\n") {
			t.Errorf("start line:\n%s", out)
		}
		if n := countVerdicts(out, "[INFO] "); n != files {
			t.Errorf("%d verdict lines for %d files", n, files)
		}
	})
	t.Run("sonarr and radarr on one file", func(t *testing.T) {
		conforming := filepath.Join(corpus, "conforming.mkv")
		polyglot := filepath.Join(corpus, "polyglot.mkv")
		for _, a := range []string{"sonarr", "radarr"} {
			hookEnv(t, jobEnv(a, conforming)...)
			code, out, errs := run(t, "--dry-run", "hook", a)
			if code != 0 || errs != "" || !strings.Contains(out, "PASS  "+conforming+"\n") {
				t.Errorf("%s conforming: exit %d stderr %q\n%s", a, code, errs, out)
			}
			hookEnv(t, jobEnv(a, polyglot)...)
			code, out, errs = run(t, "--dry-run", "hook", a, "--fail-on", "block")
			if code != 1 || !strings.Contains(out, "BLOCK "+polyglot+"\n") || !strings.Contains(errs, "BLOCK: 1 file(s)") {
				t.Errorf("%s polyglot: exit %d stderr %q\n%s", a, code, errs, out)
			}
		}
	})
	unchanged(t, before, tree(t, corpus))
}
