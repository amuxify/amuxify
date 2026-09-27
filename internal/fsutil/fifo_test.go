//go:build unix

package fsutil

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A named pipe planted at a name the run opens is the one entry that can
// stall the process for good: open(2) on a FIFO waits for a peer that the
// planter never has to supply. Every open of a name this run did not
// create through a descriptor it already holds must therefore return at
// once and refuse the pipe (guarantee 3, which covers every entry swapped
// onto a name). These tests plant a FIFO at each such name and require
// both the refusal and the return within a few seconds.

// fifoWait is how long a call that must not block is allowed to take. It
// is generous so a loaded CI machine does not fail it, and still far below
// the forever a hang would take.
const fifoWait = 5 * time.Second

// within runs fn and fails when it has not returned after fifoWait. fn
// reports through its error, never through t, because it runs on its own
// goroutine.
func within(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(fifoWait):
		t.Fatalf("%s did not return within %s: it is blocked on the named pipe", what, fifoWait)
		return nil
	}
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
}

// holdReader opens the pipe for reading without blocking and keeps it open
// until the test ends, so that a write-only open of the pipe succeeds
// rather than failing with ENXIO; the refusal must then come from the
// check on the descriptor.
func holdReader(t *testing.T, fifo string) {
	t.Helper()
	fd, err := syscall.Open(fifo, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
}

func nonblockFlag(t *testing.T, f *os.File) bool {
	t.Helper()
	flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFL, 0)
	if e != 0 {
		t.Fatal(e)
	}
	return flags&syscall.O_NONBLOCK != 0
}

func TestOpenRegularRefusesNamedPipeAndEveryOtherKind(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a.mkv")
	mkfifo(t, fifo)
	target := filepath.Join(dir, "target.mkv")
	writeFile(t, target, "real", 0o644)
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	subdir := filepath.Join(dir, "d.mkv")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A socket path must fit sockaddr_un, which the test directory's name
	// may not; the socket is bound under a short name of its own.
	sockDir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "s.mkv")
	for _, tc := range []struct{ name, path, want string }{
		{"named pipe without a writer", fifo, "named pipe"},
		{"symlink", link, "symlink"},
		{"socket", sock, "socket"},
		{"directory", subdir, "directory"},
		{"device", os.DevNull, "device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.path == sock {
				l, err := net.Listen("unix", sock)
				if err != nil {
					t.Skipf("unix sockets unavailable: %v", err)
				}
				defer l.Close()
			}
			err := within(t, "OpenRegular", func() error {
				f, err := OpenRegular(tc.path)
				if f != nil {
					_ = f.Close()
					return errors.New("returned a file")
				}
				return err
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal naming the %s", err, tc.want)
			}
		})
	}
	t.Run("named pipe with a writer", func(t *testing.T) {
		// A pipe whose planter is writing opens at once even without
		// O_NONBLOCK; the descriptor check is what refuses it.
		fd, err := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.Close(fd)
		err = within(t, "OpenRegular", func() error {
			f, err := OpenRegular(fifo)
			if f != nil {
				_ = f.Close()
				return errors.New("returned a file")
			}
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "named pipe") {
			t.Fatalf("got %v, want a refusal naming the pipe", err)
		}
	})
	t.Run("regular file", func(t *testing.T) {
		f, err := OpenRegular(target)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if nonblockFlag(t, f) {
			t.Error("the file was left in non-blocking mode")
		}
		if b, err := io.ReadAll(f); err != nil || string(b) != "real" {
			t.Errorf("read %q %v", b, err)
		}
	})
	t.Run("missing", func(t *testing.T) {
		if _, err := OpenRegular(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("got %v, want not-exist", err)
		}
	})
}

func TestOpenOwnRefusesNamedPipe(t *testing.T) {
	for _, withReader := range []bool{false, true} {
		name := "without a reader"
		if withReader {
			name = "with a reader"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, ".amuxify-a.mkv.tmp")
			own, err := CreateTemp(tmp)
			if err != nil {
				t.Fatal(err)
			}
			defer own.Close()
			if err := os.Remove(tmp); err != nil {
				t.Fatal(err)
			}
			mkfifo(t, tmp)
			if withReader {
				holdReader(t, tmp)
			}
			err = within(t, "OpenOwn", func() error {
				f, err := OpenOwn(tmp, own.Info())
				if f != nil {
					_ = f.Close()
					return errors.New("returned a file")
				}
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "named pipe") {
				t.Fatalf("got %v, want a refusal naming the pipe", err)
			}
			if err := own.Sync(); err != nil {
				t.Errorf("the held handle no longer syncs: %v", err)
			}
		})
	}
}

// The in-place replacement is the last name-based open of the temp file;
// a pipe swapped onto the name is refused and the destination keeps its
// bytes, mode and time.
func TestReplaceInPlaceOwnRefusesNamedPipeAtTemp(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "a.mkv")
	writeFile(t, dest, "original", 0o600)
	stamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(dest, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	tmp := TempName(dest)
	own, err := CreateTemp(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, tmp)
	holdReader(t, tmp)
	err = within(t, "ReplaceInPlaceOwn", func() error { return ReplaceInPlaceOwn(tmp, dest, own.Info()) })
	if err == nil || !strings.Contains(err.Error(), "named pipe") {
		t.Fatalf("got %v, want a refusal naming the pipe", err)
	}
	if readFile(t, dest) != "original" {
		t.Error("destination replaced")
	}
	mode, mtime := identityOfPath(t, dest)
	if mode != 0o600 || !mtime.Equal(stamp) {
		t.Errorf("destination identity changed: %o %v", mode, mtime)
	}
	fi, err := os.Lstat(tmp)
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the planted pipe was removed or replaced: %v", err)
	}
}

// A pipe left at the temp name before the run is a leftover: it is removed
// and the run's own regular file takes the name. Nothing opens the pipe.
func TestCreateTempReplacesPlantedPipe(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".amuxify-a.mkv.tmp")
	mkfifo(t, tmp)
	var own *Temp
	err := within(t, "CreateTemp", func() (err error) {
		own, err = CreateTemp(tmp)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	if !own.Info().Mode().IsRegular() {
		t.Fatalf("created %v, want a regular file", own.Info().Mode())
	}
	now, err := os.Lstat(tmp)
	if err != nil || !now.Mode().IsRegular() || !os.SameFile(own.Info(), now) {
		t.Fatalf("the name does not lead to the created file: %v", err)
	}
}

func TestFsyncRefusesNamedPipe(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "a.mkv")
	mkfifo(t, fifo)
	err := within(t, "Fsync", func() error { return Fsync(fifo) })
	if err == nil || !strings.Contains(err.Error(), "named pipe") {
		t.Fatalf("got %v, want a refusal naming the pipe", err)
	}
}

// Temp.Sync goes through the handle held since the creation: it flushes
// what a tool wrote into the file through its name, and it does not open
// the name again, so a pipe swapped onto it changes nothing.
func TestTempSyncNeverOpensTheName(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".amuxify-a.mkv.tmp")
	own, err := CreateTemp(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	writeFile(t, tmp, "written by the tool", 0o644)
	if err := own.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, tmp)
	if err := within(t, "Temp.Sync", own.Sync); err != nil {
		t.Fatalf("sync after the swap: %v", err)
	}
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
	if err := own.Sync(); err == nil {
		t.Error("sync on a closed handle returned nil")
	}
	var nilTemp *Temp
	if err := nilTemp.Sync(); err == nil {
		t.Error("sync on a nil handle returned nil")
	}
}

// The quarantine move: a pipe as the source is never moved, a pipe at the
// destination is never replaced or opened, on one filesystem and across
// two, and a pipe swapped onto the destination while the copy runs is
// refused when the copy is read back.
func TestMoveNoClobberRefusesNamedPipes(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		from, to := t.TempDir(), t.TempDir()
		src := filepath.Join(from, "a.mkv")
		mkfifo(t, src)
		dest := filepath.Join(to, "a.mkv")
		err := within(t, "MoveNoClobber", func() error { return MoveNoClobber(src, dest) })
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("got %v", err)
		}
		if exists(dest) {
			t.Error("destination created")
		}
		if fi, err := os.Lstat(src); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("the pipe at the source was removed or replaced: %v", err)
		}
	})
	for _, cross := range []bool{false, true} {
		name := "destination on the same filesystem"
		if cross {
			name = "destination across filesystems"
		}
		t.Run(name, func(t *testing.T) {
			if cross {
				forceCrossDevice(t)
			}
			from, to := t.TempDir(), t.TempDir()
			src := filepath.Join(from, "a.mkv")
			writeFile(t, src, "payload", 0o644)
			dest := filepath.Join(to, "a.mkv")
			mkfifo(t, dest)
			err := within(t, "MoveNoClobber", func() error { return MoveNoClobber(src, dest) })
			if !errors.Is(err, ErrExists) {
				t.Fatalf("got %v, want ErrExists", err)
			}
			if readFile(t, src) != "payload" {
				t.Error("source changed")
			}
			if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("the pipe at the destination was removed or replaced: %v", err)
			}
		})
	}
	t.Run("destination swapped for a pipe during the copy", func(t *testing.T) {
		forceCrossDevice(t)
		from, to := t.TempDir(), t.TempDir()
		src := filepath.Join(from, "a.mkv")
		writeFile(t, src, "payload", 0o644)
		dest := filepath.Join(to, "a.mkv")
		orig := CopyData
		CopyData = func(dst io.Writer, r io.Reader) (int64, error) {
			n, err := io.Copy(dst, r)
			if err := os.Remove(dest); err != nil {
				return n, err
			}
			if err := syscall.Mkfifo(dest, 0o600); err != nil {
				return n, err
			}
			return n, err
		}
		t.Cleanup(func() { CopyData = orig })
		err := within(t, "MoveNoClobber", func() error { return MoveNoClobber(src, dest) })
		if err == nil || !strings.Contains(err.Error(), "named pipe") {
			t.Fatalf("got %v, want a refusal naming the pipe", err)
		}
		if readFile(t, src) != "payload" {
			t.Error("source removed or changed although the copy was never verified")
		}
		if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("the pipe swapped onto the destination was removed although it is not this run's file: %v", err)
		}
	})
	t.Run("source swapped for a pipe during the copy", func(t *testing.T) {
		forceCrossDevice(t)
		from, to := t.TempDir(), t.TempDir()
		src := filepath.Join(from, "a.mkv")
		writeFile(t, src, "payload", 0o644)
		dest := filepath.Join(to, "a.mkv")
		orig := CopyData
		CopyData = func(dst io.Writer, r io.Reader) (int64, error) {
			n, err := io.Copy(dst, r)
			if err := os.Remove(src); err != nil {
				return n, err
			}
			if err := syscall.Mkfifo(src, 0o600); err != nil {
				return n, err
			}
			return n, err
		}
		t.Cleanup(func() { CopyData = orig })
		err := within(t, "MoveNoClobber", func() error { return MoveNoClobber(src, dest) })
		if err == nil || !strings.Contains(err.Error(), "changed during the move") {
			t.Fatalf("got %v, want a refusal", err)
		}
		if exists(dest) {
			t.Error("copy kept although the source changed")
		}
		if fi, err := os.Lstat(src); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("the pipe swapped onto the source was removed: %v", err)
		}
	})
}
