package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// A single BLOCK file given as the ingest root, or handed over by Sonarr or
// Radarr, is moved under the quarantine directory by its base name. Before
// review C1 the file itself was used as the scan root, the mirrored path was
// "." and the move was refused as outside the quarantine directory.
func TestIngestSingleFileQuarantine(t *testing.T) {
	testutil.Stubs(t)
	blocked := func(t *testing.T, name string) string {
		t.Helper()
		body := "[InternetShortcut]\nURL=http://x\n"
		if strings.HasSuffix(name, ".mkv") {
			body = ""
		}
		return write(t, filepath.Join(t.TempDir(), "lib", name), body)
	}
	check := func(t *testing.T, code int, out, errb, src, dest string) {
		t.Helper()
		// The human report escapes the bidi character in the name, so the
		// expected path goes through the same sanitiser.
		if !strings.Contains(out, "QUARANTINED") || !strings.Contains(out, "moved to "+report.Sanitize(dest)) || strings.Contains(out, "quarantine failed") {
			t.Errorf("exit %d\n%s%s", code, out, errb)
		}
		if _, err := os.Lstat(dest); err != nil {
			t.Errorf("not in quarantine: %v", err)
		}
		if _, err := os.Lstat(src); err == nil {
			t.Error("source still in place")
		}
		if fi, err := os.Lstat(filepath.Dir(dest)); err != nil || !fi.IsDir() {
			t.Errorf("quarantine root is not a directory: %v", err)
		}
	}
	t.Run("ingest with a named directory", func(t *testing.T) {
		src := blocked(t, "x.url")
		q := filepath.Join(t.TempDir(), "q")
		code, out, errb := run(t, "ingest", "--quarantine="+q, src)
		if code != 4 {
			t.Errorf("exit %d, want 4\n%s%s", code, out, errb)
		}
		check(t, code, out, errb, src, filepath.Join(q, "x.url"))
	})
	t.Run("ingest with the bare flag", func(t *testing.T) {
		src := blocked(t, "empty.mkv")
		state := t.TempDir()
		code, out, errb := run(t, "ingest", "--quarantine", "--state-dir", state, src)
		if code != 4 {
			t.Errorf("exit %d, want 4\n%s%s", code, out, errb)
		}
		check(t, code, out, errb, src, filepath.Join(state, "quarantine", "empty.mkv"))
	})
	for _, adapter := range []string{"sonarr", "radarr"} {
		t.Run("hook "+adapter, func(t *testing.T) {
			src := blocked(t, "movie‮vkm.mkv")
			state := t.TempDir()
			hookEnv(t, jobEnv(adapter, src)...)
			code, out, errb := run(t, "--state-dir", state, "hook", adapter, "--quarantine")
			if code != 1 {
				t.Errorf("exit %d, want 1 for a BLOCK\n%s%s", code, out, errb)
			}
			check(t, code, out, errb, src, filepath.Join(state, "quarantine", "movie‮vkm.mkv"))
		})
	}
	t.Run("hook sonarr with a named directory", func(t *testing.T) {
		src := blocked(t, "x.url")
		q := filepath.Join(t.TempDir(), "q")
		hookEnv(t, jobEnv("sonarr", src)...)
		code, out, errb := run(t, "hook", "sonarr", "--quarantine="+q)
		if code != 1 {
			t.Errorf("exit %d, want 1 for a BLOCK\n%s%s", code, out, errb)
		}
		check(t, code, out, errb, src, filepath.Join(q, "x.url"))
	})
	t.Run("a planted file at the quarantine slot is never overwritten", func(t *testing.T) {
		src := blocked(t, "x.url")
		q := filepath.Join(t.TempDir(), "q")
		planted := write(t, filepath.Join(q, "x.url"), "precious")
		hookEnv(t, jobEnv("radarr", src)...)
		code, out, errb := run(t, "hook", "radarr", "--quarantine="+q)
		if code != 1 || !strings.Contains(out, "quarantine failed") {
			t.Errorf("exit %d\n%s%s", code, out, errb)
		}
		if b, _ := os.ReadFile(planted); string(b) != "precious" {
			t.Errorf("planted file overwritten: %q", b)
		}
		if _, err := os.Lstat(src); err != nil {
			t.Errorf("source removed although the quarantine slot was taken: %v", err)
		}
	})
}

// A directory the process cannot read never turns into PASS with zero
// files: the run carries an ERROR line naming it, the verdict is FAIL and
// the exit code is the failure code of each caller (review C2).
func TestUnreadableDirectoryFailsTheRun(t *testing.T) {
	testutil.Stubs(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	mk := func(t *testing.T, withReadable bool) (string, string) {
		t.Helper()
		root := t.TempDir()
		if withReadable {
			write(t, filepath.Join(root, "a", "ok.nfo"), "nfo\n")
		}
		locked := filepath.Join(root, "locked")
		write(t, filepath.Join(locked, "payload.url"), "[InternetShortcut]\nURL=http://x\n")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		return root, locked
	}
	t.Run("ingest", func(t *testing.T) {
		root, locked := mk(t, true)
		code, out, errb := run(t, "ingest", root)
		if code != 3 || !strings.Contains(out, "ERROR "+root+": cannot read "+locked) || !strings.Contains(out, "FAIL:") {
			t.Errorf("exit %d, want 3 with an ERROR line naming %s\n%s%s", code, locked, out, errb)
		}
		if !strings.Contains(out, filepath.Join(root, "a", "ok.nfo")) {
			t.Errorf("readable file dropped:\n%s", out)
		}
	})
	t.Run("ingest dry run", func(t *testing.T) {
		root, _ := mk(t, false)
		code, out, errb := run(t, "--dry-run", "ingest", root)
		if code != 3 || !strings.Contains(out, "ERROR ") {
			t.Errorf("exit %d, want 3\n%s%s", code, out, errb)
		}
	})
	t.Run("scan", func(t *testing.T) {
		root, _ := mk(t, false)
		code, out, errb := run(t, "scan", root)
		if code != 3 || !strings.Contains(out, "ERROR ") {
			t.Errorf("exit %d, want 3\n%s%s", code, out, errb)
		}
	})
	t.Run("remux dry run keeps the readable files", func(t *testing.T) {
		root, locked := mk(t, true)
		code, out, errb := run(t, "--dry-run", "remux", root)
		if code != 3 || !strings.Contains(out, "ERROR "+root+": cannot read "+locked) {
			t.Errorf("exit %d, want 3 with an ERROR line naming %s\n%s%s", code, locked, out, errb)
		}
		if !strings.Contains(out, filepath.Join(root, "a", "ok.nfo")) {
			t.Errorf("readable file dropped:\n%s", out)
		}
	})
	t.Run("unreadable root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "job")
		write(t, filepath.Join(root, "payload.url"), "x")
		if err := os.Chmod(root, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
		hookEnv(t, jobEnv("nzbget", root)...)
		code, out, errb := run(t, "hook", "nzbget")
		if code != 94 || strings.Contains(out, "PASS") {
			t.Errorf("exit %d, want 94\n%s%s", code, out, errb)
		}
	})
	for _, tc := range []struct {
		adapter string
		want    int
	}{{"sabnzbd", 1}, {"nzbget", 94}} {
		t.Run("hook "+tc.adapter, func(t *testing.T) {
			root, locked := mk(t, true)
			hookEnv(t, jobEnv(tc.adapter, root)...)
			code, out, errb := run(t, "hook", tc.adapter)
			if code != tc.want {
				t.Errorf("exit %d, want %d\n%s%s", code, tc.want, out, errb)
			}
			if !strings.Contains(out+errb, "cannot read "+locked) {
				t.Errorf("no ERROR line naming %s:\n%s%s", locked, out, errb)
			}
			if strings.Contains(out, "amuxify: PASS") || strings.Contains(out, "PASS: ") {
				t.Errorf("run reported PASS:\n%s", out)
			}
			if tc.adapter == "nzbget" {
				checkNZBGetOutput(t, out, errb, false)
			}
		})
	}
}

// The quarantine directory usually lives on another filesystem than the
// download tree (the bare --quarantine form puts it under the state
// directory). A hook run with a BLOCK file still moves it there: the file is
// copied and verified, the source removed, and a file already sitting at the
// quarantine slot is never replaced (review C3, C12, C37).
func TestHookQuarantineAcrossFilesystems(t *testing.T) {
	testutil.Stubs(t)
	orig := fsutil.Place
	fsutil.Place = func(src, dest string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dest, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { fsutil.Place = orig })
	body := "[InternetShortcut]\nURL=http://x\n"
	src := write(t, filepath.Join(t.TempDir(), "lib", "x.url"), body)
	state := t.TempDir()
	dest := filepath.Join(state, "quarantine", "x.url")
	hookEnv(t, jobEnv("sonarr", src)...)
	code, out, errb := run(t, "--state-dir", state, "hook", "sonarr", "--quarantine")
	if code != 1 || !strings.Contains(out, "moved to "+report.Sanitize(dest)) {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if b, err := os.ReadFile(dest); err != nil || string(b) != body {
		t.Fatalf("quarantined copy: %q %v", b, err)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Fatal("source still in the download tree")
	}
	// The copy path creates the file itself with mode 0600; a rename would
	// have kept the source's mode, so this proves the cross-device path ran.
	if fi, err := os.Lstat(dest); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("quarantined copy mode %v, want 0600 from the cross-device copy", fi.Mode().Perm())
	}
	// A second job delivering the same name: the slot is taken, so the move
	// is refused, the first copy is kept and the new file stays in place.
	src = write(t, src, "[InternetShortcut]\nURL=http://second\n")
	hookEnv(t, jobEnv("sonarr", src)...)
	code, out, errb = run(t, "--state-dir", state, "hook", "sonarr", "--quarantine")
	if code != 1 || !strings.Contains(out, "quarantine failed") {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if b, _ := os.ReadFile(dest); string(b) != body {
		t.Fatalf("first quarantined file overwritten: %q", b)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatal("second file removed although it was not quarantined")
	}
}
