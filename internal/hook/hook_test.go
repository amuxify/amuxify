package hook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/report"
)

func TestAdaptersAndValid(t *testing.T) {
	want := []string{"sabnzbd", "nzbget", "sonarr", "radarr"}
	if got := Adapters(); !reflect.DeepEqual(got, want) {
		t.Errorf("Adapters() = %v", got)
	}
	for _, n := range want {
		if !Valid(Adapter(n)) {
			t.Errorf("%s not valid", n)
		}
	}
	for _, n := range []string{"", "SABnzbd", "sabnzbd ", "nzbget\n", "sonarr;", "../sonarr", "‮sonarr", strings.Repeat("a", 100000)} {
		if Valid(Adapter(n)) {
			t.Errorf("%q accepted as an adapter", n)
		}
	}
	if _, err := Parse(Adapter("bogus"), nil, nil); err == nil {
		t.Error("Parse accepted an unknown adapter")
	}
}

func TestLookup(t *testing.T) {
	env := []string{"A=1", "B=", "C=x=y", "A=2", "NOEQ", "=v", "AB=3"}
	cases := []struct {
		key, val string
		ok       bool
	}{
		{"A", "1", true},
		{"B", "", true},
		{"C", "x=y", true},
		{"AB", "3", true},
		{"NOEQ", "", false},
		{"", "", false},
		{"D", "", false},
		{"a", "", false},
	}
	for _, c := range cases {
		v, ok := Lookup(env, c.key)
		if v != c.val || ok != c.ok {
			t.Errorf("Lookup(%q) = %q, %v; want %q, %v", c.key, v, ok, c.val, c.ok)
		}
	}
}

func TestParseSABnzbd(t *testing.T) {
	eight := []string{"/argv/dir", "job.nzb", "Job Name", "12", "movies", "alt.binaries", "0", ""}
	t.Run("env status 0", func(t *testing.T) {
		j, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/dl/Job", "SAB_PP_STATUS=0", "SAB_FINAL_NAME=Job", "SAB_CAT=tv", "SAB_VERSION=4.3"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := Job{Adapter: SABnzbd, Paths: []string{"/dl/Job"}, Label: "Job", Category: "tv", Event: "pp status 0"}
		if !reflect.DeepEqual(j, want) {
			t.Errorf("got %+v\nwant %+v", j, want)
		}
	})
	t.Run("env without optional variables", func(t *testing.T) {
		j, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/dl/Job", "SAB_PP_STATUS=0"}, nil)
		if err != nil || j.Skip != "" || j.Event != "pp status 0" || j.Label != "" || j.Category != "" {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("env without SAB_PP_STATUS is refused, not assumed successful", func(t *testing.T) {
		for _, env := range [][]string{
			{"SAB_COMPLETE_DIR=/dl/Job"},
			{"SAB_COMPLETE_DIR=/dl/Job", "SAB_PP_STATUS="},
		} {
			_, err := Parse(SABnzbd, env, nil)
			if err == nil || err.Error() != "not started by SABnzbd: SAB_COMPLETE_DIR is set but SAB_PP_STATUS is not" {
				t.Errorf("%v: err %v", env, err)
			}
		}
	})
	for _, st := range []string{"1", "2", "3", "-1", "junk"} {
		t.Run("env status "+st, func(t *testing.T) {
			j, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/dl/Job", "SAB_PP_STATUS=" + st, "SAB_FAIL_MSG=unpack failed"}, eight)
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("post-processing status %s; the files are not usable: unpack failed", st)
			if j.Skip != want || j.Event != "pp status "+st || j.Paths[0] != "/dl/Job" {
				t.Errorf("%+v", j)
			}
		})
	}
	t.Run("skip without a fail message", func(t *testing.T) {
		j, _ := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/dl/Job", "SAB_PP_STATUS=2", "SAB_FAIL_MSG="}, nil)
		if j.Skip != "post-processing status 2; the files are not usable" {
			t.Errorf("%q", j.Skip)
		}
	})
	t.Run("argv fallback", func(t *testing.T) {
		j, err := Parse(SABnzbd, []string{"PATH=/bin", "SAB_COMPLETE_DIR="}, eight)
		if err != nil {
			t.Fatal(err)
		}
		want := Job{Adapter: SABnzbd, Paths: []string{"/argv/dir"}, Label: "Job Name", Category: "movies", Event: "pp status 0"}
		if !reflect.DeepEqual(j, want) {
			t.Errorf("got %+v\nwant %+v", j, want)
		}
	})
	t.Run("argv status 1", func(t *testing.T) {
		args := append([]string(nil), eight...)
		args[6] = "1"
		j, err := Parse(SABnzbd, nil, args)
		if err != nil || j.Skip != "post-processing status 1; the files are not usable" || j.Event != "pp status 1" {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("seven parameters from an older SABnzbd", func(t *testing.T) {
		j, err := Parse(SABnzbd, nil, eight[:7])
		if err != nil {
			t.Fatal(err)
		}
		want := Job{Adapter: SABnzbd, Paths: []string{"/argv/dir"}, Label: "Job Name", Category: "movies", Event: "pp status 0"}
		if !reflect.DeepEqual(j, want) {
			t.Errorf("got %+v\nwant %+v", j, want)
		}
	})
	t.Run("the NZB name labels the job when the clean name is empty", func(t *testing.T) {
		args := append([]string(nil), eight...)
		args[2] = ""
		j, err := Parse(SABnzbd, nil, args)
		if err != nil || j.Label != "job.nzb" {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("argv with only the directory is not SABnzbd's shape", func(t *testing.T) {
		_, err := Parse(SABnzbd, nil, []string{"/argv/dir"})
		want := `expected no positional arguments or SABnzbd's seven or eight parameters, got 1 beginning with "/argv/dir"`
		if err == nil || err.Error() != want {
			t.Errorf("err %v", err)
		}
	})
	t.Run("an empty status is refused, not assumed successful", func(t *testing.T) {
		for _, n := range []int{7, 8} {
			args := append([]string(nil), eight[:n]...)
			args[6] = ""
			_, err := Parse(SABnzbd, nil, args)
			if err == nil || err.Error() != "SABnzbd's seventh parameter, the post-processing status, is empty" {
				t.Errorf("%d parameters: err %v", n, err)
			}
		}
	})
	t.Run("neither", func(t *testing.T) {
		_, err := Parse(SABnzbd, []string{"SAB_PP_STATUS=0"}, nil)
		if err == nil || err.Error() != "not started by SABnzbd: SAB_COMPLETE_DIR is not set and no arguments were given" {
			t.Errorf("err %v", err)
		}
	})
	t.Run("env with a wrong argv shape is still a usage error", func(t *testing.T) {
		// SABnzbd sets both, so positionals of the wrong shape are a wrapper
		// mistake even when the environment alone would describe the job.
		_, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/env/dir", "SAB_PP_STATUS=0"}, []string{"/stray/dir"})
		if err == nil || !strings.HasPrefix(err.Error(), "expected no positional arguments") {
			t.Errorf("err %v", err)
		}
	})
	t.Run("env wins over argv", func(t *testing.T) {
		args := append([]string(nil), eight...)
		args[6] = "1"
		j, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/env/dir", "SAB_PP_STATUS=0", "SAB_CAT=tv"}, args)
		if err != nil || j.Paths[0] != "/env/dir" || j.Skip != "" || j.Category != "tv" {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("SAB_STATUS is never read", func(t *testing.T) {
		j, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=/dl/Job", "SAB_PP_STATUS=0", "SAB_STATUS=Failed"}, nil)
		if err != nil || j.Skip != "" {
			t.Errorf("%+v %v", j, err)
		}
	})
}

// CheckSABnzbdArgs is the definition of which positionals are SABnzbd's.
// The stray directory cases matter for safety: a directory added in front
// of or after SABnzbd's own parameters must never be ingested or read as a
// parameter in silence, whatever the wrapper did to put it there.
func TestCheckSABnzbdArgs(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "job.nzb")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	seven := []string{dir, "job.nzb", "Job", "1", "tv", "alt.binaries", "0"}
	eight := append(append([]string(nil), seven...), "")
	with := func(base []string, i int, v string) []string {
		out := append([]string(nil), base...)
		out[i] = v
		return out
	}
	tests := []struct {
		name string
		args []string
		arg  string // the argument a usage message should name, "" for none
		err  string // "" means accepted
	}{
		{"none", nil, "", ""},
		{"seven", seven, "", ""},
		{"eight", eight, "", ""},
		{"eight with a failure URL", with(eight, 7, "https://indexer/report/1"), "", ""},
		{"seven with a missing directory", with(seven, 0, missing), "", ""},
		{"eight with a file where the NZB name goes", with(eight, 1, file), "", ""},
		{"eight with a missing path where the NZB name goes", with(eight, 1, missing), "", ""},
		{"one directory", []string{dir}, dir,
			fmt.Sprintf("expected no positional arguments or SABnzbd's seven or eight parameters, got 1 beginning with %q", dir)},
		{"a directory named --quarantine", []string{"--quarantine"}, "--quarantine",
			`expected no positional arguments or SABnzbd's seven or eight parameters, got 1 beginning with "--quarantine"`},
		{"six", seven[:6], dir,
			fmt.Sprintf("expected no positional arguments or SABnzbd's seven or eight parameters, got 6 beginning with %q", dir)},
		{"nine: a directory before eight", append([]string{dir}, eight...), dir,
			fmt.Sprintf("expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with %q", dir)},
		{"nine: a directory after eight", append(append([]string(nil), eight...), dir), dir,
			fmt.Sprintf("expected no positional arguments or SABnzbd's seven or eight parameters, got 9 beginning with %q", dir)},
		{"a directory before seven", append([]string{missing}, seven...), missing,
			fmt.Sprintf("%q is a directory where SABnzbd's second parameter, the original NZB name, belongs; a directory in front of SABnzbd's parameters is not read as one of them", dir)},
		{"a symlink to a directory before seven", append([]string{missing}, with(seven, 0, link)...), missing,
			fmt.Sprintf("%q is a directory where SABnzbd's second parameter, the original NZB name, belongs; a directory in front of SABnzbd's parameters is not read as one of them", link)},
		{"a directory after seven", append(append([]string(nil), seven...), dir), dir,
			fmt.Sprintf("%q is a directory where SABnzbd's eighth parameter, the failure URL, belongs; a directory after SABnzbd's parameters is not read as one of them", dir)},
		{"a symlink to a directory after seven", append(append([]string(nil), seven...), link), link,
			fmt.Sprintf("%q is a directory where SABnzbd's eighth parameter, the failure URL, belongs; a directory after SABnzbd's parameters is not read as one of them", link)},
		{"seven with an empty status", with(seven, 6, ""), "",
			"SABnzbd's seventh parameter, the post-processing status, is empty"},
		{"eight with an empty status", with(eight, 6, ""), "",
			"SABnzbd's seventh parameter, the post-processing status, is empty"},
		{"a NUL where the NZB name goes is data, not a directory", with(eight, 1, dir+"\x00"), "", ""},
		{"a newline where the failure URL goes is data, not a directory", with(eight, 7, dir+"\n"), "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSABnzbdArgs(tc.args)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %q", tc.args)
			}
			if err.Error() != tc.err {
				t.Errorf("got  %s\nwant %s", err, tc.err)
			}
			var ae *SABnzbdArgError
			if !errors.As(err, &ae) {
				t.Fatalf("%T is not a *SABnzbdArgError", err)
			}
			if ae.Arg != tc.arg {
				t.Errorf("Arg %q, want %q", ae.Arg, tc.arg)
			}
		})
	}
}

func TestParseNZBGet(t *testing.T) {
	t.Run("success with directory", func(t *testing.T) {
		j, err := Parse(NZBGet, []string{"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY=/dl/Job", "NZBPP_NZBNAME=Job", "NZBPP_CATEGORY=tv", "NZBPP_STATUS=SUCCESS/ALL", "NZBPO_PROFILE=strict"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := Job{Adapter: NZBGet, Paths: []string{"/dl/Job"}, Label: "Job", Category: "tv", Event: "status SUCCESS/SUCCESS/ALL"}
		if !reflect.DeepEqual(j, want) {
			t.Errorf("got %+v\nwant %+v", j, want)
		}
	})
	t.Run("finaldir precedence", func(t *testing.T) {
		j, err := Parse(NZBGet, []string{"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY=/dl/Job", "NZBPP_FINALDIR=/dl/final"}, nil)
		if err != nil || !reflect.DeepEqual(j.Paths, []string{"/dl/final"}) {
			t.Errorf("%+v %v", j, err)
		}
		j, err = Parse(NZBGet, []string{"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_FINALDIR=", "NZBPP_DIRECTORY=/dl/Job"}, nil)
		if err != nil || !reflect.DeepEqual(j.Paths, []string{"/dl/Job"}) {
			t.Errorf("empty finaldir: %+v %v", j, err)
		}
	})
	for _, st := range []string{"WARNING", "FAILURE", "DELETED", "success", "SUCCESS "} {
		t.Run("skip "+st, func(t *testing.T) {
			j, err := Parse(NZBGet, []string{"NZBPP_TOTALSTATUS=" + st, "NZBPP_DIRECTORY=/dl/Job", "NZBPP_STATUS=X/Y"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if j.Skip != fmt.Sprintf("download status %s/X/Y; the files are not usable", st) || j.Event != "status "+st+"/X/Y" {
				t.Errorf("%+v", j)
			}
		})
	}
	t.Run("skip without status", func(t *testing.T) {
		j, _ := Parse(NZBGet, []string{"NZBPP_TOTALSTATUS=FAILURE", "NZBPP_DIRECTORY=/dl/Job"}, nil)
		if j.Skip != "download status FAILURE; the files are not usable" {
			t.Errorf("%q", j.Skip)
		}
	})
	t.Run("missing totalstatus", func(t *testing.T) {
		for _, env := range [][]string{{"NZBPP_DIRECTORY=/dl/Job"}, {"NZBPP_DIRECTORY=/dl/Job", "NZBPP_TOTALSTATUS="}} {
			_, err := Parse(NZBGet, env, []string{"/dl/Job"})
			if err == nil || err.Error() != "not started by NZBGet: NZBPP_TOTALSTATUS is not set (NZBGet 13 or newer is required)" {
				t.Errorf("%v: err %v", env, err)
			}
		}
	})
	t.Run("missing both directories", func(t *testing.T) {
		for _, env := range [][]string{{"NZBPP_TOTALSTATUS=SUCCESS"}, {"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY=", "NZBPP_FINALDIR="}} {
			_, err := Parse(NZBGet, env, []string{"/dl/Job"})
			if err == nil || !strings.Contains(err.Error(), "not started by NZBGet") {
				t.Errorf("%v: err %v", env, err)
			}
		}
	})
}

func TestParseSonarr(t *testing.T) {
	t.Run("test", func(t *testing.T) {
		j, err := Parse(Sonarr, []string{"sonarr_eventtype=Test"}, []string{"ignored"})
		if err != nil || !j.Test || j.Skip != "" || j.Event != "Test" || len(j.Paths) != 0 {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("download with path", func(t *testing.T) {
		j, err := Parse(Sonarr, []string{"sonarr_eventtype=Download", "sonarr_episodefile_path=/tv/S/e1.mkv", "sonarr_series_title=S", "sonarr_release_title=S.E01.1080p", "sonarr_series_originallanguage=jpn"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := Job{Adapter: Sonarr, Paths: []string{"/tv/S/e1.mkv"}, Original: "jpn", Label: "S: S.E01.1080p", Event: "Download"}
		if !reflect.DeepEqual(j, want) {
			t.Errorf("got %+v\nwant %+v", j, want)
		}
	})
	t.Run("download with paths", func(t *testing.T) {
		j, err := Parse(Sonarr, []string{"sonarr_eventtype=Download", "sonarr_episodefile_paths=/tv/a.mkv||/tv/b.mkv|"}, nil)
		if err != nil || !reflect.DeepEqual(j.Paths, []string{"/tv/a.mkv", "/tv/b.mkv"}) {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("path wins over paths", func(t *testing.T) {
		j, err := Parse(Sonarr, []string{"sonarr_eventtype=Download", "sonarr_episodefile_path=/tv/a.mkv", "sonarr_episodefile_paths=/tv/b.mkv"}, nil)
		if err != nil || !reflect.DeepEqual(j.Paths, []string{"/tv/a.mkv"}) {
			t.Errorf("%+v %v", j, err)
		}
	})
	t.Run("download with neither", func(t *testing.T) {
		for _, env := range [][]string{
			{"sonarr_eventtype=Download"},
			{"sonarr_eventtype=Download", "sonarr_episodefile_path=", "sonarr_episodefile_paths=||"},
		} {
			_, err := Parse(Sonarr, env, nil)
			if err == nil || err.Error() != "event Download without sonarr_episodefile_path or sonarr_episodefile_paths" {
				t.Errorf("%v: err %v", env, err)
			}
		}
	})
	for _, ev := range []string{"Grab", "Rename", "EpisodeFileDelete", "SeriesAdd", "HealthIssue", "ApplicationUpdate", "ManualInteractionRequired", "download", "Download ", "Test\n", "$(true)", "‮Download", strings.Repeat("D", 100000)} {
		t.Run("skip "+strings.TrimSpace(ev[:min(len(ev), 12)]), func(t *testing.T) {
			j, err := Parse(Sonarr, []string{"sonarr_eventtype=" + ev, "sonarr_episodefile_path=/tv/a.mkv"}, nil)
			if err != nil || j.Test || j.Skip != "event "+ev+"; nothing to ingest" || len(j.Paths) != 0 {
				t.Errorf("%+v %v", j, err)
			}
		})
	}
	t.Run("missing eventtype", func(t *testing.T) {
		for _, env := range [][]string{nil, {"sonarr_eventtype="}, {"SONARR_EVENTTYPE=Download"}, {"radarr_eventtype=Download"}} {
			_, err := Parse(Sonarr, env, nil)
			if err == nil || err.Error() != "not started by Sonarr: sonarr_eventtype is not set" {
				t.Errorf("%v: err %v", env, err)
			}
		}
	})
	t.Run("language passthrough", func(t *testing.T) {
		for _, lang := range []string{"jpn", "eng", "JPN", "ja", "$(id)", strings.Repeat("x", 5000)} {
			j, err := Parse(Sonarr, []string{"sonarr_eventtype=Download", "sonarr_episodefile_path=/tv/a.mkv", "sonarr_series_originallanguage=" + lang}, nil)
			if err != nil || j.Original != lang {
				t.Errorf("%q: %+v %v", lang, j, err)
			}
		}
	})
	t.Run("label composition", func(t *testing.T) {
		for _, c := range []struct{ title, release, want string }{
			{"S", "R", "S: R"},
			{"S", "", "S"},
			{"", "R", "R"},
			{"", "", ""},
		} {
			j, _ := Parse(Sonarr, []string{"sonarr_eventtype=Download", "sonarr_episodefile_path=/tv/a.mkv", "sonarr_series_title=" + c.title, "sonarr_release_title=" + c.release}, nil)
			if j.Label != c.want {
				t.Errorf("%q/%q: label %q", c.title, c.release, j.Label)
			}
		}
	})
}

func TestParseRadarr(t *testing.T) {
	j, err := Parse(Radarr, []string{"radarr_eventtype=Test"}, nil)
	if err != nil || !j.Test {
		t.Errorf("%+v %v", j, err)
	}
	j, err = Parse(Radarr, []string{"radarr_eventtype=Download", "radarr_moviefile_path=/movies/M/m.mkv", "radarr_movie_title=M", "radarr_movie_originallanguage=fra"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := Job{Adapter: Radarr, Paths: []string{"/movies/M/m.mkv"}, Original: "fra", Label: "M", Event: "Download"}
	if !reflect.DeepEqual(j, want) {
		t.Errorf("got %+v\nwant %+v", j, want)
	}
	// Radarr has no _paths variant, and Sonarr's variables mean nothing here.
	_, err = Parse(Radarr, []string{"radarr_eventtype=Download", "radarr_moviefile_paths=/movies/a.mkv", "sonarr_episodefile_path=/tv/a.mkv"}, nil)
	if err == nil || err.Error() != "event Download without radarr_moviefile_path" {
		t.Errorf("err %v", err)
	}
	for _, ev := range []string{"Grab", "Rename", "MovieDelete", "HealthIssue", "download"} {
		j, err := Parse(Radarr, []string{"radarr_eventtype=" + ev, "radarr_moviefile_path=/movies/a.mkv"}, nil)
		if err != nil || j.Skip != "event "+ev+"; nothing to ingest" {
			t.Errorf("%s: %+v %v", ev, j, err)
		}
	}
	if _, err := Parse(Radarr, []string{"sonarr_eventtype=Download"}, nil); err == nil || err.Error() != "not started by Radarr: radarr_eventtype is not set" {
		t.Errorf("err %v", err)
	}
}

// Hostile values are carried as data: a path stays exactly what the caller
// gave, nothing is split, trimmed or expanded, and nothing is executed.
func TestParseKeepsHostileValuesAsData(t *testing.T) {
	for _, v := range []string{
		"/dl/x; rm -rf /",
		"$(touch /tmp/pwned)",
		"`id`",
		"-rf",
		"--quarantine=/",
		"/dl/x\nBLOCK /forged",
		"/dl/x\n[NZB] MARK=BAD",
		"a\x00b",
		"‮/dl/x",
		"​",
		"../../../etc",
		"relative/dir",
		"",
		strings.Repeat("x", 200000),
	} {
		j, err := Parse(SABnzbd, []string{"SAB_COMPLETE_DIR=" + v, "SAB_FINAL_NAME=" + v, "SAB_CAT=" + v, "SAB_PP_STATUS=0"}, nil)
		if v == "" {
			if err == nil {
				t.Error("empty SAB_COMPLETE_DIR with no arguments was accepted")
			}
			continue
		}
		if err != nil || len(j.Paths) != 1 || j.Paths[0] != v || j.Label != v || j.Category != v {
			t.Errorf("%.20q: %+v %v", v, j, err)
		}
		j, err = Parse(NZBGet, []string{"NZBPP_TOTALSTATUS=SUCCESS", "NZBPP_DIRECTORY=" + v, "NZBPP_NZBNAME=" + v}, nil)
		if err != nil || j.Paths[0] != v || j.Label != v {
			t.Errorf("nzbget %.20q: %+v %v", v, j, err)
		}
		j, err = Parse(Sonarr, []string{"sonarr_eventtype=Download", "sonarr_episodefile_path=" + v}, nil)
		if err != nil || j.Paths[0] != v {
			t.Errorf("sonarr %.20q: %+v %v", v, j, err)
		}
	}
	// A flag-shaped positional is a directory name, never a flag, and every
	// hostile value in the positional form is carried as data too.
	for _, v := range []string{"--quarantine=/", "--quarantine", "-rf", "$(id)", "a\x00b", "/dl/x\nBLOCK /forged", "‮/dl/x"} {
		j, err := Parse(SABnzbd, nil, []string{v, v, v, v, v, v, "0", v})
		if err != nil || j.Paths[0] != v || j.Label != v || j.Category != v || j.Skip != "" {
			t.Errorf("%.20q: %+v %v", v, j, err)
		}
	}
	// Positional arguments beyond the eight are not ignored: the shape is
	// not SABnzbd's, so the call is refused before any path is read.
	_, err := Parse(SABnzbd, nil, []string{"--quarantine=/", "n", "c", "1", "cat", "g", "0", "", "extra", "--fail-on", "block"})
	if err == nil || !strings.HasPrefix(err.Error(), "expected no positional arguments or SABnzbd's seven or eight parameters, got 11 ") {
		t.Errorf("err %v", err)
	}
}

func TestExitCodes(t *testing.T) {
	verdicts := []report.Severity{report.Pass, report.Warn, report.Fail, report.Block}
	failOns := []report.Severity{report.Warn, report.Fail, report.Block}
	// success[failOn][verdict] is the caller's view of a Ran outcome.
	success := map[report.Severity]map[report.Severity]bool{
		report.Warn:  {report.Pass: true, report.Warn: false, report.Fail: false, report.Block: false},
		report.Fail:  {report.Pass: true, report.Warn: true, report.Fail: false, report.Block: false},
		report.Block: {report.Pass: true, report.Warn: true, report.Fail: true, report.Block: false},
	}
	type codes struct{ ok, bad, skipped, usage, interrupted int }
	table := map[Adapter]codes{
		SABnzbd: {0, 1, 0, 2, 130},
		NZBGet:  {93, 94, 95, 94, 94},
		Sonarr:  {0, 1, 0, 2, 130},
		Radarr:  {0, 1, 0, 2, 130},
	}
	for a, c := range table {
		for _, fo := range failOns {
			for _, v := range verdicts {
				want := c.bad
				if success[fo][v] {
					want = c.ok
				}
				if got := ExitCode(a, v, fo, Ran); got != want {
					t.Errorf("%s ran %s fail-on %s: %d, want %d", a, v, fo, got, want)
				}
				if got := ExitCode(a, v, fo, Skipped); got != c.skipped {
					t.Errorf("%s skipped %s fail-on %s: %d, want %d", a, v, fo, got, c.skipped)
				}
				if got := ExitCode(a, v, fo, UsageError); got != c.usage {
					t.Errorf("%s usage %s fail-on %s: %d, want %d", a, v, fo, got, c.usage)
				}
				if got := ExitCode(a, v, fo, Interrupted); got != c.interrupted {
					t.Errorf("%s interrupted %s fail-on %s: %d, want %d", a, v, fo, got, c.interrupted)
				}
			}
			// BLOCK is a failure under every fail-on value, including one
			// past the end of the scale.
			for _, bad := range []report.Severity{fo, report.Block + 1, 99} {
				if got := ExitCode(a, report.Block, bad, Ran); got != c.bad {
					t.Errorf("%s BLOCK fail-on %d: %d, want %d", a, bad, got, c.bad)
				}
			}
		}
		// An outcome outside the enumeration is never a success.
		if got := ExitCode(a, report.Pass, report.Fail, Outcome(42)); got != c.interrupted {
			t.Errorf("%s unknown outcome: %d", a, got)
		}
		// NZBGet only ever sees 93, 94 and 95.
		if a == NZBGet {
			for o := Outcome(0); o < 6; o++ {
				for _, v := range verdicts {
					if got := ExitCode(a, v, report.Fail, o); got < 93 || got > 95 {
						t.Errorf("nzbget outcome %d verdict %s: %d", o, v, got)
					}
				}
			}
		}
	}
	// An unknown adapter follows the SABnzbd convention.
	if ExitCode(Adapter("bogus"), report.Block, report.Block, Ran) != 1 || ExitCode(Adapter("bogus"), report.Pass, report.Fail, UsageError) != 2 {
		t.Error("unknown adapter codes")
	}
}

func TestControlLines(t *testing.T) {
	if got := ControlLines(NZBGet, report.Block); !reflect.DeepEqual(got, []string{"[NZB] MARK=BAD"}) {
		t.Errorf("nzbget BLOCK: %v", got)
	}
	for _, v := range []report.Severity{report.Pass, report.Warn, report.Usage, report.Fail} {
		if got := ControlLines(NZBGet, v); got != nil {
			t.Errorf("nzbget %s: %v", v, got)
		}
	}
	for _, a := range []Adapter{SABnzbd, Sonarr, Radarr, Adapter("bogus"), Adapter("NZBGet")} {
		for _, v := range []report.Severity{report.Pass, report.Warn, report.Fail, report.Block} {
			if got := ControlLines(a, v); got != nil {
				t.Errorf("%s %s: %v", a, v, got)
			}
		}
	}
}

func TestParseFailOn(t *testing.T) {
	for in, want := range map[string]report.Severity{"warn": report.Warn, "fail": report.Fail, "block": report.Block, "WARN": report.Warn, "Fail": report.Fail, "BLOCK": report.Block, " block ": report.Block} {
		got, err := ParseFailOn(in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "pass", "never", "usage", "4", "block\x00", "blocked", "fail;block", strings.Repeat("b", 10000)} {
		if _, err := ParseFailOn(in); err == nil {
			t.Errorf("%.20q accepted", in)
		}
	}
}

func TestMatchCategory(t *testing.T) {
	for _, c := range []struct {
		glob, cat string
		want      bool
	}{
		{"tv", "tv", true},
		{"TV", "tv", true},
		{"tv", "TV", true},
		{"tv*", "tv", true},
		{"tv*", "tv-4k", true},
		{"tv*", "movies", false},
		{"movies", "tv", false},
		{"", "tv", true},
		{"", "", true},
		{"*", "", true},
		{"*", "anything", true},
		{"**", "tv", true},
		{"../*", "tv", false},
		{"../*", "../tv", true},
		{"tv", "tv\n", false},
		{"tv", " tv", false},
		{"t?", "tv", true},
		{"[tm]v", "tv", true},
		{"tv", "tv/kids", false},
		{"*", "tv/kids", false},
	} {
		got, err := MatchCategory(c.glob, c.cat)
		if err != nil || got != c.want {
			t.Errorf("MatchCategory(%q, %q) = %v, %v; want %v", c.glob, c.cat, got, err, c.want)
		}
	}
	// A malformed pattern is an error and never a match, whatever the
	// category.
	for _, glob := range []string{"[", "[a-", "tv[", "\\", "[]"} {
		for _, cat := range []string{"tv", "", "[", "anything"} {
			got, err := MatchCategory(glob, cat)
			if err == nil || got {
				t.Errorf("MatchCategory(%q, %q) = %v, %v", glob, cat, got, err)
			}
		}
	}
	// Absurd inputs are handled without panic.
	long := strings.Repeat("*", 5000) + strings.Repeat("a", 5000)
	if _, err := MatchCategory(long, strings.Repeat("a", 5000)); err != nil {
		t.Error(err)
	}
}

func TestPrefixWriter(t *testing.T) {
	var buf bytes.Buffer
	p := NewPrefixWriter(&buf, "[INFO] ")
	for _, s := range []string{"ab", "", "c\nde", "\n", "", "f"} {
		if n, err := p.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := buf.String(); got != "[INFO] abc\n[INFO] de\n" {
		t.Errorf("before flush: %q", got)
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "[INFO] abc\n[INFO] de\n[INFO] f\n" {
		t.Errorf("after flush: %q", got)
	}
	// A second flush and an empty writer produce nothing.
	if err := p.Flush(); err != nil || buf.Len() != len("[INFO] abc\n[INFO] de\n[INFO] f\n") {
		t.Errorf("second flush wrote: %q", buf.String())
	}
	var empty bytes.Buffer
	e := NewPrefixWriter(&empty, "[ERROR] ")
	e.Write(nil)
	e.Write([]byte{})
	e.Flush()
	if empty.Len() != 0 {
		t.Errorf("empty output got a prefix: %q", empty.String())
	}
	// An empty line is still prefixed, as NZBGet expects one tag per line.
	var blank bytes.Buffer
	b := NewPrefixWriter(&blank, "[INFO] ")
	b.Write([]byte("\n\n"))
	if blank.String() != "[INFO] \n[INFO] \n" {
		t.Errorf("blank lines: %q", blank.String())
	}
	// A write error is returned and the failed line is not retried.
	fw := NewPrefixWriter(errWriter{}, "[INFO] ")
	if _, err := fw.Write([]byte("x\n")); err == nil {
		t.Error("write error swallowed")
	}
	if err := fw.Flush(); err != nil {
		t.Errorf("flush after a failed line: %v", err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

// A caller-controlled message can never become a command for NZBGet: any
// line body that begins with the control tag is marked as not a command,
// wherever the newline came from, and only Control writes the real one.
func TestPrefixWriterNeverForgesControlLine(t *testing.T) {
	forged := []string{
		"PASS  /dl/x\n[NZB] MARK=BAD\n",
		"[NZB] MARK=BAD\n",
		"[NZB] DIRECTORY=/etc\n",
		"[NZB] NZBPR_amuxify=1\n",
		"[nzb] mark=bad\n",
		"line\r\n[NZB] MARK=BAD\r\n",
		"[NZB]",
		"[NZB",
	}
	for _, in := range forged {
		var buf bytes.Buffer
		p := NewPrefixWriter(&buf, "[INFO] ")
		// Deliver it byte by byte too, so a split at the tag cannot help.
		for i := range in {
			p.Write([]byte{in[i]})
		}
		p.Flush()
		for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if !strings.HasPrefix(line, "[INFO] ") {
				t.Errorf("%q: unprefixed line %q", in, line)
			}
			body := strings.TrimPrefix(line, "[INFO] ")
			if len(body) >= 5 && strings.EqualFold(body[:5], "[NZB]") {
				t.Errorf("%q: control line forged: %q", in, line)
			}
		}
		if strings.Contains(in, "[NZB] MARK=BAD") && !strings.Contains(buf.String(), "[INFO] (not a command) [NZB] MARK=BAD") {
			t.Errorf("%q: forged line not marked: %q", in, buf.String())
		}
	}
	// The genuine control line goes out verbatim, after any pending text.
	var buf bytes.Buffer
	p := NewPrefixWriter(&buf, "[INFO] ")
	p.Write([]byte("pending"))
	if err := p.Control("[NZB] MARK=BAD"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "[INFO] pending\n[INFO] [NZB] MARK=BAD\n" {
		t.Errorf("control: %q", buf.String())
	}
}

// NZBGet splits overlong lines itself; the writer splits them first so that
// no split point chosen by a hostile name starts a new line with the tag.
func TestPrefixWriterSplitsLongLines(t *testing.T) {
	pad := strings.Repeat("a", MaxLine-3)
	in := pad + "[NZB] MARK=BAD" + strings.Repeat("b", MaxLine) + "\n"
	var buf bytes.Buffer
	p := NewPrefixWriter(&buf, "[INFO] ")
	p.Write([]byte(in))
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines", len(lines))
	}
	var joined strings.Builder
	for _, l := range lines {
		if !strings.HasPrefix(l, "[INFO] ") {
			t.Errorf("unprefixed: %.40q", l)
		}
		body := strings.TrimPrefix(l, "[INFO] ")
		if len(body) > MaxLine || strings.HasPrefix(body, "[NZB]") {
			t.Errorf("bad piece: len %d, starts %.20q", len(body), body)
		}
		joined.WriteString(strings.TrimPrefix(body, neutralized))
	}
	if joined.String() != strings.TrimSuffix(in, "\n") {
		t.Error("the pieces do not reassemble to the input")
	}
	// A partial line that grows past the limit is emitted before Flush, and
	// a split never lands inside a multi-byte character.
	var long bytes.Buffer
	q := NewPrefixWriter(&long, "[INFO] ")
	q.Write([]byte(strings.Repeat("é", MaxLine)))
	if long.Len() == 0 {
		t.Error("overlong partial line was not emitted")
	}
	q.Flush()
	for _, l := range strings.Split(strings.TrimRight(long.String(), "\n"), "\n") {
		if !strings.HasPrefix(l, "[INFO] ") || strings.Contains(l, "�") || !utf8Valid(l) {
			t.Errorf("bad piece: %.40q", l)
		}
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
