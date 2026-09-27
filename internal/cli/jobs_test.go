package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// --jobs takes a whole number from 1 to 64, before or after the command;
// anything else is a usage error that exits 2 without touching a file. The
// hook adapters check it the same way and answer with their own usage code.
func TestJobsUsageErrors(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	cases := []struct {
		args []string
		msg  string
		code int
	}{
		{[]string{"--jobs", "0", "scan", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"--jobs=0", "scan", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"scan", "--jobs", "0", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"--jobs", "-1", "scan", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"--jobs", "65", "scan", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"--jobs", "abc", "scan", dir}, `invalid value "abc" for flag -jobs`, 2},
		{[]string{"scan", "--jobs", "abc", dir}, `invalid value "abc" for flag -jobs`, 2},
		{[]string{"--jobs", "1.5", "scan", dir}, `invalid value "1.5" for flag -jobs`, 2},
		{[]string{"--jobs", "", "scan", dir}, `invalid value "" for flag -jobs`, 2},
		{[]string{"--jobs", "4; rm -rf /", "scan", dir}, "invalid value", 2},
		{[]string{"--jobs", "0", "doctor"}, "--jobs must be between 1 and 64", 2},
		{[]string{"remux", "--jobs", "999999999999999999999", dir}, "invalid value", 2},
		{[]string{"clean", "--jobs", "0", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"ingest", "--jobs", "0", dir}, "--jobs must be between 1 and 64", 2},
		{[]string{"hook", "sabnzbd", "--jobs", "0"}, "--jobs must be between 1 and 64", 2},
		{[]string{"hook", "nzbget", "--jobs", "0"}, "--jobs must be between 1 and 64", 94},
	}
	for _, tc := range cases {
		before := tree(t, dir)
		code, out, errb := run(t, tc.args...)
		if code != tc.code {
			t.Errorf("%v: exit %d, want %d (stderr %q)", tc.args, code, tc.code, errb)
		}
		if !strings.Contains(errb, tc.msg) {
			t.Errorf("%v: stderr %q does not contain %q", tc.args, errb, tc.msg)
		}
		if strings.Contains(out, "PASS") || strings.Contains(out, "file(s)") {
			t.Errorf("%v: a report was written on a usage error:\n%s", tc.args, out)
		}
		unchanged(t, before, tree(t, dir))
	}
	// The valid edges work, before and after the command, and a count far
	// above the number of files is fine.
	for _, args := range [][]string{
		{"--jobs", "1", "scan", dir},
		{"--jobs", "64", "scan", dir},
		{"scan", "--jobs=64", dir},
		{"--jobs", "2", "scan", "--jobs", "64", dir},
	} {
		if code, out, errb := run(t, args...); code != 0 || !strings.Contains(out, "PASS: 1 file(s)") {
			t.Errorf("%v: exit %d\n%s%s", args, code, out, errb)
		}
	}
}

// The help page lists --jobs with its value placeholder.
func TestJobsInHelp(t *testing.T) {
	_, out, errb := run(t, "help")
	if !strings.Contains(out+errb, "  --jobs <n>              files to process at the same time, 1 to 64; hook always uses 1\n") {
		t.Errorf("help does not list --jobs <n>:\n%s%s", out, errb)
	}
}

// blocks splits streamed human output into per-file blocks: a verdict line
// and the indented lines under it. It fails the test on an indented line
// that does not follow a verdict line, which is what interleaved output from
// two workers looks like.
func blocks(t *testing.T, out string) (map[string][]string, []string) {
	t.Helper()
	byPath := map[string][]string{}
	var order []string
	var cur string
	for _, l := range lines(out) {
		if l == "" {
			break
		}
		switch {
		case strings.HasPrefix(l, "PASS  ") || strings.HasPrefix(l, "WARN  ") || strings.HasPrefix(l, "FAIL  ") || strings.HasPrefix(l, "BLOCK "):
			cur = l[6:]
			if _, dup := byPath[cur]; dup {
				t.Errorf("verdict line for %s appears twice", cur)
			}
			byPath[cur] = []string{l}
			order = append(order, cur)
		case strings.HasPrefix(l, "      "):
			if cur == "" {
				t.Errorf("indented line before any verdict line: %q", l)
				continue
			}
			byPath[cur] = append(byPath[cur], l)
		default:
			t.Errorf("unexpected line %q", l)
		}
	}
	return byPath, order
}

// decodeJSON decodes one report document and checks its schema.
func decodeJSON(t *testing.T, out string) map[string]interface{} {
	t.Helper()
	var doc map[string]interface{}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if doc["schema"] != report.SchemaID {
		t.Errorf("schema %v", doc["schema"])
	}
	return doc
}

// stripTimes removes the fields that legitimately differ between two runs.
func stripTimes(doc map[string]interface{}) {
	delete(doc, "started")
	delete(doc, "finished")
	for _, f := range doc["files"].([]interface{}) {
		delete(f.(map[string]interface{}), "duration_ms")
	}
}

// A parallel run over the corpus prints every file's block whole, with no
// line of another file inside it, and its JSON report equals the sequential
// run's apart from the timestamps, so --jobs changes when a file is reported
// and never what is reported. A dry run changes nothing either way.
func TestJobsParallelMatchesSequential(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"scan", []string{"scan"}, 4},
		{"scan verbose", []string{"--verbose", "scan"}, 4},
		{"ingest dry run", []string{"--dry-run", "ingest", "--force", "--remove-blocked-sidecars", "--hardlinks", "break", "--quarantine"}, 4},
		{"remux dry run", []string{"--dry-run", "remux", "--output"}, 4},
		{"clean dry run", []string{"--dry-run", "clean"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			corpus := testutil.CopyTree(t)
			args := append([]string(nil), tc.args...)
			if args[len(args)-1] == "--output" {
				args = append(args, filepath.Join(t.TempDir(), "out"))
			}
			args = append(args, corpus)
			before := tree(t, corpus)
			seqCode, seqOut, seqErr := run(t, append([]string{"--jobs", "1"}, args...)...)
			parCode, parOut, parErr := run(t, append([]string{"--jobs", "4"}, args...)...)
			unchanged(t, before, tree(t, corpus))
			if seqCode != tc.code || parCode != tc.code {
				t.Fatalf("exit %d and %d, want %d\n%s%s", seqCode, parCode, tc.code, seqErr, parErr)
			}
			seqBlocks, seqOrder := blocks(t, seqOut)
			parBlocks, parOrder := blocks(t, parOut)
			if len(parOrder) != len(seqOrder) || len(parBlocks) != len(seqBlocks) {
				t.Fatalf("%d blocks in parallel, %d sequential\n%s", len(parOrder), len(seqOrder), parOut)
			}
			for p, want := range seqBlocks {
				if got := parBlocks[p]; !reflect.DeepEqual(got, want) {
					t.Errorf("block for %s differs:\n%s\nwant:\n%s", p, strings.Join(got, "\n"), strings.Join(want, "\n"))
				}
			}
			// The sequential run streams in walk order; the parallel one
			// in completion order, which is some permutation of it.
			for i := 1; i < len(seqOrder); i++ {
				if seqOrder[i] < seqOrder[i-1] {
					t.Errorf("sequential run out of walk order: %s after %s", seqOrder[i], seqOrder[i-1])
				}
			}
			// The tails match exactly.
			seqTail := seqOut[strings.LastIndex(seqOut, "\n\n"):]
			parTail := parOut[strings.LastIndex(parOut, "\n\n"):]
			if seqTail != parTail {
				t.Errorf("tail %q, sequential %q", parTail, seqTail)
			}

			_, seqJSON, _ := run(t, append([]string{"--json", "--jobs", "1"}, args...)...)
			_, parJSON, _ := run(t, append([]string{"--json", "--jobs", "4"}, args...)...)
			seqDoc := decodeJSON(t, seqJSON)
			parDoc := decodeJSON(t, parJSON)
			stripTimes(seqDoc)
			stripTimes(parDoc)
			if !reflect.DeepEqual(seqDoc, parDoc) {
				a, _ := json.MarshalIndent(seqDoc, "", "  ")
				b, _ := json.MarshalIndent(parDoc, "", "  ")
				t.Errorf("JSON differs between --jobs 1 and --jobs 4:\n%s\n---\n%s", a, b)
			}
			unchanged(t, before, tree(t, corpus))
		})
	}
}

// Guarantee 1 from the command line: several sources that rebuild to one
// destination, remuxed with --jobs 4, leave exactly one output, the others
// report OUTPUT_EXISTS with the same message a sequential run gives, and no
// temp file is left. Repeated, because a race that shows only sometimes is
// still a race.
func TestJobsSameDestinationRace(t *testing.T) {
	needTools(t)
	asUser(t, 1000)
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			for round := 0; round < 3; round++ {
				dir := t.TempDir()
				for name, as := range map[string]string{"sample.mov": "ep.mov", "sample.webm": "ep.webm", "sample.avi": "ep.avi", "sample.ts": "ep.ts"} {
					if err := os.Rename(testutil.Copy(t, name), filepath.Join(dir, as)); err != nil {
						t.Fatal(err)
					}
				}
				destDir := dir
				args := []string{"--json", "--jobs", "4", "remux"}
				if inPlace {
					args = append(args, "--in-place")
				} else {
					destDir = filepath.Join(t.TempDir(), "out")
					args = append(args, "--output", destDir)
				}
				code, out, errb := run(t, append(args, dir)...)
				if code != 3 {
					t.Fatalf("round %d: exit %d, want 3\n%s%s", round, code, out, errb)
				}
				files := parseRemuxDoc(t, out)
				if len(files) != 4 {
					t.Fatalf("round %d: %d files", round, len(files))
				}
				placed, exists := 0, 0
				dest := filepath.Join(destDir, "ep.mkv")
				for _, f := range files {
					if _, _, ok := f.finding("PLACED"); ok && f.Output == dest {
						placed++
						continue
					}
					sev, msg, ok := f.finding("OUTPUT_EXISTS")
					if ok && sev == "FAIL" && msg == dest+" already exists" && f.Output == "" {
						exists++
						continue
					}
					t.Errorf("round %d: %s: %+v", round, filepath.Base(f.Path), f)
				}
				if placed != 1 || exists != 3 || filepath.Base(files[0].Path) != "ep.avi" {
					t.Errorf("round %d: %d placed, %d OUTPUT_EXISTS, first %s", round, placed, exists, files[0].Path)
				}
				if _, _, ok := files[0].finding("PLACED"); !ok {
					t.Errorf("round %d: the first file in walk order did not win: %+v", round, files[0])
				}
				entries, err := os.ReadDir(destDir)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".amuxify-") {
						t.Errorf("round %d: temp file %s left behind", round, e.Name())
					}
				}
			}
		})
	}
}

// Guarantee 1 for quarantine from the command line: two roots holding a
// blocked file of the same name are scanned with --jobs 4 and the second is
// refused with "destination already exists"; the quarantine holds one file
// and the loser stays where it was. Repeated, because a race that shows
// only sometimes is still a race.
func TestJobsSameQuarantineNameRace(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	for round := 0; round < 10; round++ {
		q := filepath.Join(t.TempDir(), "q")
		a := write(t, filepath.Join(t.TempDir(), "a", "x.url"), "[InternetShortcut]\nURL=http://a\n")
		b := write(t, filepath.Join(t.TempDir(), "b", "x.url"), "[InternetShortcut]\nURL=http://b\n")
		code, out, errb := run(t, "--json", "--jobs", "4", "scan", "--quarantine", q, a, b)
		if code != 4 {
			t.Fatalf("round %d: exit %d\n%s%s", round, code, out, errb)
		}
		doc := decodeJSON(t, out)
		moved, failed := 0, 0
		for _, f := range doc["files"].([]interface{}) {
			for _, fd := range f.(map[string]interface{})["findings"].([]interface{}) {
				m := fd.(map[string]interface{})
				if m["code"] != "QUARANTINED" {
					continue
				}
				switch m["message"] {
				case "moved to " + filepath.Join(q, "x.url"):
					moved++
				case "quarantine failed: destination already exists":
					failed++
				default:
					t.Errorf("round %d: %v", round, m)
				}
			}
		}
		if moved != 1 || failed != 1 {
			t.Errorf("round %d: %d moved, %d failed", round, moved, failed)
		}
		got, err := os.ReadFile(filepath.Join(q, "x.url"))
		if err != nil || !strings.Contains(string(got), "URL=http://a") {
			t.Errorf("round %d: quarantine holds %q, %v; the first root's file should win", round, got, err)
		}
		if _, err := os.Lstat(a); err == nil {
			t.Errorf("round %d: the moved file is still at %s", round, a)
		}
		if _, err := os.Lstat(b); err != nil {
			t.Errorf("round %d: the refused file is gone from %s", round, b)
		}
	}
}

// The hook adapters run with one job whatever --jobs says: the flag is
// accepted, checked, and ignored, and the run behaves like a sequential
// ingest.
func TestHookIgnoresJobs(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	write(t, filepath.Join(dir, "b.nfo"), "nfo\n")
	hookEnv(t, jobEnv("sabnzbd", dir)...)
	code, out, errb := run(t, "--jobs", "64", "hook", "sabnzbd")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	_, order := blocks(t, out[strings.Index(out, "\n")+1:])
	if len(order) != 2 || order[0] != filepath.Join(dir, "a.nfo") || order[1] != filepath.Join(dir, "b.nfo") {
		t.Errorf("hook streamed %v, want walk order", order)
	}
	if !strings.Contains(out, "amuxify: "+report.Pass.String()+", 2 file(s)") {
		t.Errorf("summary line missing:\n%s", out)
	}
}
