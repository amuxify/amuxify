//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/testutil"
)

// Guarantee 2 under parallel jobs: an interrupt during a --jobs 4 remux
// starts no further file, lets the files already running finish or abort,
// and leaves no temp file under the output directory or beside the sources.
// The signal is sent the moment the first verdict streams, while up to three
// other remuxes are still running.
func TestJobsInterruptLeavesNoTemp(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			const files = 12
			for i := 0; i < files; i++ {
				name := filepath.Join(dir, "ep"+string(rune('a'+i))+".mov")
				if err := os.Rename(testutil.Copy(t, "sample.mov"), name); err != nil {
					t.Fatal(err)
				}
			}
			destDir := dir
			args := []string{"--jobs", "4", "remux"}
			if inPlace {
				args = append(args, "--in-place")
			} else {
				destDir = filepath.Join(t.TempDir(), "out")
				args = append(args, "--output", destDir)
			}
			// Any verdict line names a file under dir; the first one fires
			// the signal.
			w := &interruptWriter{marker: "  " + dir + string(filepath.Separator)}
			var errb bytes.Buffer
			code := Main(append(args, dir), w, &errb)
			out := w.buf.String()
			if !w.fired {
				t.Fatalf("no verdict streamed; output:\n%s", out)
			}
			if w.err != nil {
				t.Fatalf("kill: %v", w.err)
			}
			// The run stops: at most the four files that had started report,
			// the root records the cancellation as an error and the exit is
			// 3, exactly as a sequential remux interrupted between two files
			// behaves today.
			verdicts := 0
			for _, line := range strings.Split(out, "\n") {
				if strings.Contains(line, "  "+dir+string(filepath.Separator)) {
					verdicts++
				}
			}
			if verdicts == 0 || verdicts > 4 {
				t.Errorf("%d verdicts streamed after the interrupt, want 1 to 4:\n%s", verdicts, out)
			}
			if code != 3 {
				t.Errorf("exit %d, want 3\n%s%s", code, out, errb.String())
			}
			if !strings.Contains(out, "ERROR "+dir+": context canceled\n") {
				t.Errorf("the interrupt is not reported:\n%s%s", out, errb.String())
			}
			for _, root := range []string{dir, destDir} {
				entries, err := os.ReadDir(root)
				if err != nil {
					if inPlace || root == dir {
						t.Fatal(err)
					}
					continue
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".amuxify-") {
						t.Errorf("temp file %s left in %s", e.Name(), root)
					}
				}
			}
			// Every source that did not finish is still there, untouched.
			sources := 0
			for i := 0; i < files; i++ {
				if _, err := os.Lstat(filepath.Join(dir, "ep"+string(rune('a'+i))+".mov")); err == nil {
					sources++
				}
			}
			if sources < files-4 {
				t.Errorf("%d of %d sources remain; the run went on after the interrupt", sources, files)
			}
		})
	}
}
