//go:build linux

package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run on Linux only, where futimes needs a fallback for a
// system without /proc. The CI run on ubuntu is the confirming run; on
// macOS they are compiled by GOOS=linux go vet and not executed.

// noDescriptorCalls makes both descriptor calls fail for the duration of
// the test: the raw utimensat as a kernel or seccomp profile without it
// would, and the /proc form as a container without /proc would.
func noDescriptorCalls(t *testing.T) {
	t.Helper()
	origFd, origProc := futimensFd, futimesProc
	futimensFd = func(int, *[2]syscall.Timespec) error { return syscall.ENOSYS }
	futimesProc = func(int, []syscall.Timeval) error { return syscall.ENOENT }
	t.Cleanup(func() { futimensFd, futimesProc = origFd, origProc })
}

func openForIdentity(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := OpenOwn(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

var (
	oldStamp = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	newStamp = time.Date(2010, 11, 12, 13, 14, 15, 0, time.UTC)
)

// The descriptor call the kernel provides sets the times without /proc.
func TestFutimensFdSetsTimesWithoutProc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.mkv")
	writeFile(t, path, "x", 0o644)
	if err := os.Chtimes(path, oldStamp, oldStamp); err != nil {
		t.Fatal(err)
	}
	f := openForIdentity(t, path)
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(newStamp.UnixNano()), syscall.NsecToTimespec(newStamp.UnixNano())}
	if err := utimensatFd(int(f.Fd()), &ts); err != nil {
		t.Fatalf("utimensat on the descriptor: %v", err)
	}
	if _, mtime := identityOfPath(t, path); !mtime.Equal(newStamp) {
		t.Fatalf("mtime %v, want %v", mtime, newStamp)
	}
}

// With /proc missing, futimes still sets the times: first through the raw
// descriptor call, and when that is refused too, by the file's name.
func TestFutimesFallsBackWithoutProc(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"proc missing", func(t *testing.T) {
			orig := futimesProc
			futimesProc = func(int, []syscall.Timeval) error { return syscall.ENOENT }
			t.Cleanup(func() { futimesProc = orig })
		}},
		{"proc missing and utimensat refused", noDescriptorCalls},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			path := filepath.Join(t.TempDir(), "a.mkv")
			writeFile(t, path, "x", 0o644)
			if err := os.Chtimes(path, oldStamp, oldStamp); err != nil {
				t.Fatal(err)
			}
			f := openForIdentity(t, path)
			if err := futimes(f, time.Now(), newStamp); err != nil {
				t.Fatalf("futimes: %v", err)
			}
			if _, mtime := identityOfPath(t, path); !mtime.Equal(newStamp) {
				t.Fatalf("mtime %v, want %v", mtime, newStamp)
			}
		})
	}
}

// The name-based fallback is the one place a name is used after the file
// was opened, so it must refuse a name that no longer leads to that file:
// another file renamed onto it, a symlink planted at it, or nothing at
// all. The entry now at the name keeps its own times, and so does the file
// that was opened.
func TestFutimesPathFallbackRefusesSwappedFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(t *testing.T, path, victim string)
		want string
	}{
		{"other file renamed onto the name", func(t *testing.T, path, victim string) {
			if err := os.Rename(victim, path); err != nil {
				t.Fatal(err)
			}
		}, "no longer names the file"},
		{"hard link to another file", func(t *testing.T, path, victim string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(victim, path); err != nil {
				t.Fatal(err)
			}
		}, "no longer names the file"},
		{"symlink planted at the name", func(t *testing.T, path, victim string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, path); err != nil {
				t.Fatal(err)
			}
		}, "symlink"},
		{"name removed", func(t *testing.T, path, victim string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noDescriptorCalls(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "a.mkv")
			writeFile(t, path, "opened", 0o644)
			if err := os.Chtimes(path, oldStamp, oldStamp); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(dir, "victim.mkv")
			writeFile(t, victim, "victim", 0o600)
			if err := os.Chtimes(victim, oldStamp, oldStamp); err != nil {
				t.Fatal(err)
			}
			f := openForIdentity(t, path)
			tc.swap(t, path, victim)
			err := futimes(f, time.Now(), newStamp)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal saying %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "through /proc: no such file") {
				t.Errorf("the error does not say that /proc was missing: %v", err)
			}
			if fi, err := f.Stat(); err != nil || !fi.ModTime().Equal(oldStamp) {
				t.Errorf("the opened file's time changed through the name: %v %v", fi.ModTime(), err)
			}
			now, err := os.Lstat(path)
			if err == nil && now.Mode()&os.ModeSymlink == 0 && !now.ModTime().Equal(oldStamp) {
				t.Errorf("the entry swapped onto the name got the new time: %v", now.ModTime())
			}
			if vfi, err := os.Lstat(victim); err == nil && !vfi.ModTime().Equal(oldStamp) {
				t.Errorf("the victim got the new time: %v", vfi.ModTime())
			}
		})
	}
}

// The path fallback is never taken for a name that leads to something
// other than a regular file, and it is only reached when both descriptor
// calls failed: with the descriptor call working, no name is ever used,
// so a swap after the open changes nothing about which file gets the time.
func TestFutimesDescriptorCallIgnoresTheName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.mkv")
	writeFile(t, path, "opened", 0o644)
	if err := os.Chtimes(path, oldStamp, oldStamp); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim.mkv")
	writeFile(t, victim, "victim", 0o600)
	if err := os.Chtimes(victim, oldStamp, oldStamp); err != nil {
		t.Fatal(err)
	}
	f := openForIdentity(t, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	if err := futimes(f, time.Now(), newStamp); err != nil {
		t.Fatalf("futimes: %v", err)
	}
	if fi, err := f.Stat(); err != nil || !fi.ModTime().Equal(newStamp) {
		t.Errorf("the opened file did not get the time: %v %v", fi.ModTime(), err)
	}
	if vfi, err := os.Lstat(victim); err != nil || !vfi.ModTime().Equal(oldStamp) {
		t.Errorf("the victim behind the planted link got the time: %v %v", vfi.ModTime(), err)
	}
}
