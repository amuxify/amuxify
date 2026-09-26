// Package testutil locates tools and fixtures for tests and skips cleanly when
// they are missing. Set AMUXIFY_REQUIRE_TOOLS=1 to fail instead of skipping.
package testutil

import (
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
)

// required reports whether AMUXIFY_REQUIRE_TOOLS=1 turns skips into failures.
func required() bool {
	v := strings.TrimSpace(os.Getenv("AMUXIFY_REQUIRE_TOOLS"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// missing skips the test, or fails it under AMUXIFY_REQUIRE_TOOLS=1.
func missing(t testing.TB, what string) {
	t.Helper()
	if required() {
		t.Fatalf("%s is required (AMUXIFY_REQUIRE_TOOLS=1)", what)
	}
	t.Skipf("skipping: %s (AMUXIFY_REQUIRE_TOOLS unset)", what)
}

// Need skips the test unless every named tool resolves through exec.Runner,
// so AMUXIFY_<TOOL> overrides apply. With AMUXIFY_REQUIRE_TOOLS=1 a missing
// tool fails the test. It returns a Runner with a 2 minute timeout.
func Need(t testing.TB, tools ...string) *exec.Runner {
	t.Helper()
	r := &exec.Runner{Timeout: 2 * time.Minute}
	for _, tool := range tools {
		if !r.Have(tool) {
			missing(t, tool+" not installed")
		}
	}
	return r
}

var (
	fixOnce sync.Once
	fixDir  string
	fixErr  error
)

// Fixtures returns the directory holding the generated corpus. It honours
// AMUXIFY_FIXTURES; otherwise it runs testdata/gen-fixtures.sh once per test
// binary (sync.Once) into a directory under os.TempDir() and skips (or fails
// under AMUXIFY_REQUIRE_TOOLS=1) when ffmpeg, mkvmerge, mkvpropedit or python3
// are missing or the script exits non-zero. The script is located relative
// to the calling package by walking up to the directory that holds go.mod.
func Fixtures(t testing.TB) string {
	t.Helper()
	if v := os.Getenv("AMUXIFY_FIXTURES"); v != "" {
		abs, err := filepath.Abs(v)
		if err != nil {
			t.Fatalf("AMUXIFY_FIXTURES=%q: %v", v, err)
		}
		if fi, err := os.Stat(filepath.Join(abs, "clean.mkv")); err != nil || fi.IsDir() {
			t.Fatalf("AMUXIFY_FIXTURES=%q does not hold a generated corpus (clean.mkv missing); run make fixtures", v)
		}
		return abs
	}
	fixOnce.Do(func() { fixDir, fixErr = generate() })
	if fixErr != nil {
		missing(t, fixErr.Error())
	}
	return fixDir
}

// generate runs the fixture script into a fresh temp directory.
func generate() (string, error) {
	r := &exec.Runner{Timeout: 10 * time.Minute}
	for _, tool := range []string{exec.FFmpeg, exec.MKVMerge, exec.MKVPropedit} {
		if !r.Have(tool) {
			return "", fmt.Errorf("%s not installed", tool)
		}
	}
	if _, err := osexec.LookPath("python3"); err != nil {
		return "", fmt.Errorf("python3 not installed")
	}
	if _, err := osexec.LookPath("bash"); err != nil {
		return "", fmt.Errorf("bash not installed")
	}
	root, err := moduleRoot()
	if err != nil {
		return "", err
	}
	script := filepath.Join(root, "testdata", "gen-fixtures.sh")
	if _, err := os.Stat(script); err != nil {
		return "", fmt.Errorf("fixture script: %v", err)
	}
	dir, err := os.MkdirTemp("", "amuxify-fixtures-")
	if err != nil {
		return "", err
	}
	// The script's own tool lookups go through PATH, so the AMUXIFY_<TOOL>
	// overrides are prepended as directories for the duration of the run.
	cmd := osexec.Command("bash", script, dir)
	cmd.Env = append(os.Environ(), "PATH="+pathWithOverrides(r))
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("gen-fixtures.sh failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

// pathWithOverrides prepends the directories of overridden tools to PATH.
func pathWithOverrides(r *exec.Runner) string {
	var dirs []string
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract} {
		if p, err := r.Path(tool); err == nil && os.Getenv("AMUXIFY_"+strings.ToUpper(tool)) != "" {
			dirs = append(dirs, filepath.Dir(p))
		}
	}
	dirs = append(dirs, os.Getenv("PATH"))
	return strings.Join(dirs, string(os.PathListSeparator))
}

// moduleRoot walks up from the calling package's directory to go.mod.
func moduleRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot locate the testutil source file")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

// Copy copies one fixture (a path relative to Fixtures) into t.TempDir(),
// preserving the relative path, and returns the absolute copy path.
func Copy(t testing.TB, rel string) string {
	t.Helper()
	src := filepath.Join(Fixtures(t), filepath.FromSlash(rel))
	dst := filepath.Join(t.TempDir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copy fixture %s: %v", rel, err)
	}
	return dst
}

// CopyTree copies the whole corpus into t.TempDir() and returns the root.
// Hard links and symlinks are recreated as such so the corpus keeps its
// hard_a/hard_b pair and its link.mkv symlink.
func CopyTree(t testing.TB) string {
	t.Helper()
	src := Fixtures(t)
	dst := filepath.Join(t.TempDir(), "corpus")
	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copy corpus: %v", err)
	}
	return dst
}

func copyTree(src, dst string) error {
	// inode -> first copied path, so a hard-linked pair stays a pair.
	linked := map[string]string{}
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			return os.MkdirAll(target, 0o755)
		case fi.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		key := inodeKey(fi)
		if key != "" {
			if first, ok := linked[key]; ok {
				return os.Link(first, target)
			}
		}
		if err := copyFile(p, target); err != nil {
			return err
		}
		if key != "" {
			linked[key] = target
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(link, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// Stubs points AMUXIFY_FFMPEG, AMUXIFY_FFPROBE, AMUXIFY_MKVMERGE and
// AMUXIFY_MKVPROPEDIT at a harmless executable file created in t.TempDir()
// via t.Setenv, so cli tests that never reach a tool (usage errors,
// sidecar-only trees) run on machines without the tools. The stub is a copy
// of the running test binary (os.Executable), which exists on every platform.
func Stubs(t testing.TB) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	stub := filepath.Join(t.TempDir(), "amuxify-stub-tool")
	if err := copyFile(self, stub); err != nil {
		t.Fatalf("stub tool: %v", err)
	}
	if err := os.Chmod(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit} {
		t.Setenv("AMUXIFY_"+strings.ToUpper(tool), stub)
	}
}

// Profile writes a TOML overlay into t.TempDir() and returns its path.
func Profile(t testing.TB, toml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "profile.toml")
	if err := os.WriteFile(p, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// inodeKey identifies a file's inode so hard-linked pairs can be recreated.
func inodeKey(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
		return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
	}
	return ""
}
