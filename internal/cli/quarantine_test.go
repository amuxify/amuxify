package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/testutil"
)

// A quarantine directory inside the tree is handled the same by every
// command that quarantines: the BLOCK file is moved once, and a second run
// over the same tree neither lists nor moves it. The directory is written
// with a trailing slash and a ".." component to show the spelling does not
// matter.
func TestQuarantineInsideTreeIsMovedOnce(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	for _, tc := range []struct {
		name string
		args func(dir, q string) []string
	}{
		{"scan", func(dir, q string) []string { return []string{"scan", "--quarantine", q, dir} }},
		{"ingest", func(dir, q string) []string { return []string{"ingest", "--quarantine=" + q, dir} }},
		{"hook nzbget", func(dir, q string) []string {
			hookEnv(t, jobEnv("nzbget", dir)...)
			return []string{"hook", "nzbget", "--quarantine=" + q}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "downloads")
			write(t, filepath.Join(dir, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n")
			write(t, filepath.Join(dir, "ok.nfo"), "nfo\n")
			// Built by string concatenation: filepath.Join would clean the
			// ".." away before the command saw it.
			q := dir + "/sub/../quarantine/"
			dest := filepath.Join(dir, "quarantine", "sub", "x.url")
			args := tc.args(dir, q)
			_, out, errs := run(t, args...)
			if !strings.Contains(out, "moved to "+dest) {
				t.Fatalf("first run: no move to %s:\n%s%s", dest, out, errs)
			}
			fi, err := os.Lstat(dest)
			if err != nil {
				t.Fatal(err)
			}
			_, out, errs = run(t, args...)
			if strings.Contains(out, "quarantine") || strings.Contains(errs, "quarantine") {
				t.Errorf("second run mentions the quarantine directory:\n%s%s", out, errs)
			}
			if !strings.Contains(out, "ok.nfo") {
				t.Errorf("second run did not list the rest of the tree:\n%s", out)
			}
			if now, err := os.Lstat(dest); err != nil || !os.SameFile(fi, now) {
				t.Errorf("second run touched the quarantined file: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, "quarantine", "quarantine")); err == nil {
				t.Error("second run nested the quarantine directory")
			}
		})
	}
}

// A path that is the quarantine directory or lies inside it is a usage
// error, exit 2 (94 for NZBGet), reported before anything runs: no tool is
// started, nothing under the path changes and no directory is created. The
// relative spelling of both paths is covered through the working directory.
func TestRootInsideQuarantineIsUsageError(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	base := t.TempDir()
	q := filepath.Join(base, "quarantine")
	write(t, filepath.Join(q, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n")
	inside := filepath.Join(q, "sub")
	state := t.TempDir()
	write(t, filepath.Join(state, "quarantine", "x", "y.url"), "[InternetShortcut]\nURL=http://x\n")
	cases := []struct {
		name string
		env  []string
		args []string
		code int
		want string
	}{
		{"scan root is the quarantine", nil, []string{"scan", "--quarantine", q + "/", q}, 2, "scan: " + q + " is the quarantine directory"},
		{"scan root inside the quarantine", nil, []string{"scan", "--quarantine", q, inside + "/../sub"}, 2, "lies inside the quarantine directory " + q},
		{"scan second root inside the quarantine", nil, []string{"scan", "--quarantine", q, base, inside}, 2, "scan: " + inside + " lies inside the quarantine directory"},
		{"ingest root inside the quarantine", nil, []string{"ingest", "--quarantine=" + q, inside}, 2, "ingest: " + inside + " lies inside the quarantine directory"},
		{"ingest bare quarantine under the state dir", nil, []string{"--state-dir", state, "ingest", "--quarantine", filepath.Join(state, "quarantine", "x")}, 2, "ingest: " + filepath.Join(state, "quarantine", "x") + " lies inside the quarantine directory"},
		{"hook sonarr file inside the quarantine", jobEnv("sonarr", filepath.Join(q, "sub", "x.url")), []string{"hook", "sonarr", "--quarantine=" + q}, 2, "hook sonarr: " + filepath.Join(q, "sub", "x.url") + " lies inside the quarantine directory"},
		{"hook nzbget directory is the quarantine", jobEnv("nzbget", q), []string{"hook", "nzbget", "--quarantine=" + q}, 94, "[ERROR] amuxify: hook nzbget: " + q + " is the quarantine directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != nil {
				hookEnv(t, tc.env...)
			}
			before, beforeState := tree(t, base), tree(t, state)
			code, out, errs := run(t, tc.args...)
			if code != tc.code {
				t.Errorf("exit %d, want %d; stdout %q stderr %q", code, tc.code, out, errs)
			}
			if !strings.Contains(errs, tc.want) {
				t.Errorf("stderr %q lacks %q", errs, tc.want)
			}
			if out != "" {
				t.Errorf("stdout %q on a usage error", out)
			}
			unchanged(t, before, tree(t, base))
			unchanged(t, beforeState, tree(t, state))
		})
	}
	t.Run("relative paths", func(t *testing.T) {
		t.Chdir(base)
		before := tree(t, base)
		code, out, errs := run(t, "scan", "--quarantine", "quarantine", "quarantine/sub/")
		if code != 2 || !strings.Contains(errs, "lies inside the quarantine directory") || out != "" {
			t.Errorf("exit %d stdout %q stderr %q", code, out, errs)
		}
		unchanged(t, before, tree(t, base))
	})
	// Under --dry-run no quarantine is in effect and the run is not refused.
	if code, _, errs := run(t, "--dry-run", "scan", "--quarantine", q, inside); code != 4 {
		t.Errorf("dry run: exit %d stderr %q", code, errs)
	}
}
