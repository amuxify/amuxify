package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		if !strings.Contains(out, "QUARANTINED") || !strings.Contains(out, "moved to "+dest) || strings.Contains(out, "quarantine failed") {
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
