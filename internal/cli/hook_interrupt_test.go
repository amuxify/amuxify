//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/testutil"
)

// interruptWriter sends the test process SIGINT the moment a marker line is
// written, then waits so the signal is delivered before the next file.
type interruptWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marker string
	fired  bool
	err    error
}

func (w *interruptWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if !w.fired && strings.Contains(w.buf.String(), w.marker) {
		w.fired = true
		w.err = syscall.Kill(os.Getpid(), syscall.SIGINT)
		time.Sleep(500 * time.Millisecond)
	}
	return len(p), nil
}

// An interrupted run exits 130 for SABnzbd, Sonarr and Radarr and 94 for
// NZBGet, whatever the verdict so far, and the report records that code.
func TestHookInterrupted(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	b := write(t, filepath.Join(dir, "b.nfo"), "nfo\n")
	c := write(t, filepath.Join(dir, "c.nfo"), "nfo\n")
	for _, tc := range []struct {
		adapter string
		env     []string
		code    int
	}{
		{"sabnzbd", jobEnv("sabnzbd", dir), 130},
		{"nzbget", jobEnv("nzbget", dir), 94},
		{"sonarr", []string{"sonarr_eventtype=Download", "sonarr_episodefile_paths=" + a + "|" + b + "|" + c}, 130},
		{"radarr", jobEnv("radarr", dir), 130},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "r.json")
			hookEnv(t, tc.env...)
			w := &interruptWriter{marker: "PASS  " + a + "\n"}
			var errb bytes.Buffer
			code := Main([]string{"hook", tc.adapter, "--json-out", report}, w, &errb)
			out := w.buf.String()
			if !w.fired {
				t.Fatalf("marker line never streamed; output:\n%s", out)
			}
			if w.err != nil {
				t.Fatalf("kill: %v", w.err)
			}
			if code != tc.code {
				t.Fatalf("exit %d, want %d\n%s%s", code, tc.code, out, errb.String())
			}
			// The caller's log says why the job failed: one line on stderr,
			// which NZBGet shows as an [ERROR] line.
			want := "amuxify hook " + tc.adapter + ": interrupted\n"
			if tc.adapter == "nzbget" {
				want = "[ERROR] " + want
			}
			if !strings.Contains(errb.String(), want) {
				t.Errorf("stderr %q lacks %q", errb.String(), want)
			}
			if strings.Contains(out, "interrupted") {
				t.Errorf("the interrupted line went to stdout:\n%s", out)
			}

			if strings.Contains(out, "PASS  "+c+"\n") {
				t.Errorf("the run went on after the interrupt:\n%s", out)
			}
			data, err := os.ReadFile(report)
			if err != nil {
				t.Fatal(err)
			}
			doc := decodeReport(t, data)
			hook, _ := doc["hook"].(map[string]interface{})
			if int(hook["exit_code"].(float64)) != tc.code {
				t.Errorf("hook.exit_code %v", hook["exit_code"])
			}
			if tc.adapter == "nzbget" {
				checkNZBGetOutput(t, out, errb.String(), false)
			} else {
				noControlLines(t, out)
			}
		})
	}
}
