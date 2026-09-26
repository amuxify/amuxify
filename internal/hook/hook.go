// Package hook maps a download client's or media manager's environment to an
// ingest job and a run verdict to that caller's exit code. It runs no tools.
package hook

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/amuxify/amuxify/internal/report"
)

// Adapter names the program that started the hook.
type Adapter string

// The known adapters.
const (
	SABnzbd Adapter = "sabnzbd"
	NZBGet  Adapter = "nzbget"
	Sonarr  Adapter = "sonarr"
	Radarr  Adapter = "radarr"
)

// Adapters returns the names in the order above.
func Adapters() []string {
	return []string{string(SABnzbd), string(NZBGet), string(Sonarr), string(Radarr)}
}

// Valid reports whether a is a known adapter.
func Valid(a Adapter) bool {
	switch a {
	case SABnzbd, NZBGet, Sonarr, Radarr:
		return true
	}
	return false
}

// Job is what the adapter derived from its caller.
type Job struct {
	Adapter  Adapter
	Paths    []string // what to ingest, as given by the caller
	Original string   // original language hint or ""
	Label    string   // job or title name for the first log line
	Category string   // SAB_CAT or NZBPP_CATEGORY; "" for the arrs
	Event    string   // raw status or event type for the log line
	Skip     string   // non-empty: reason this invocation does nothing
	Test     bool     // Sonarr and Radarr connection test
}

// Parse reads the caller's environment ("K=V" entries as os.Environ returns
// them) and positional arguments. An error means the process was not started
// the way the adapter expects. The values are taken as data: nothing in them
// is interpreted, and a path is handed to ingest exactly as the caller gave
// it.
func Parse(a Adapter, env []string, args []string) (Job, error) {
	switch a {
	case SABnzbd:
		return parseSABnzbd(env, args)
	case NZBGet:
		return parseNZBGet(env)
	case Sonarr:
		return parseArr(Sonarr, env)
	case Radarr:
		return parseArr(Radarr, env)
	}
	return Job{}, fmt.Errorf("unknown adapter %q", string(a))
}

func parseSABnzbd(env []string, args []string) (Job, error) {
	j := Job{Adapter: SABnzbd}
	dir, _ := Lookup(env, "SAB_COMPLETE_DIR")
	status := "0"
	switch {
	case dir != "":
		if v, ok := Lookup(env, "SAB_PP_STATUS"); ok && v != "" {
			status = v
		}
		j.Label, _ = Lookup(env, "SAB_FINAL_NAME")
		j.Category, _ = Lookup(env, "SAB_CAT")
	case len(args) > 0:
		// SABnzbd passes eight positional parameters: complete dir, nzb
		// name, clean job name, indexer report number, category, group,
		// post-processing status, failure URL.
		dir = args[0]
		if len(args) > 2 && args[2] != "" {
			j.Label = args[2]
		} else if len(args) > 1 {
			j.Label = args[1]
		}
		if len(args) > 4 {
			j.Category = args[4]
		}
		if len(args) > 6 && args[6] != "" {
			status = args[6]
		}
	default:
		return j, errors.New("not started by SABnzbd: SAB_COMPLETE_DIR is not set and no arguments were given")
	}
	j.Paths = []string{dir}
	j.Event = "pp status " + status
	if status != "0" {
		j.Skip = fmt.Sprintf("post-processing status %s; the files are not usable", status)
		if msg, _ := Lookup(env, "SAB_FAIL_MSG"); msg != "" {
			j.Skip += ": " + msg
		}
	}
	return j, nil
}

func parseNZBGet(env []string) (Job, error) {
	j := Job{Adapter: NZBGet}
	total, ok := Lookup(env, "NZBPP_TOTALSTATUS")
	if !ok || total == "" {
		return j, errors.New("not started by NZBGet: NZBPP_TOTALSTATUS is not set (NZBGet 13 or newer is required)")
	}
	dir, _ := Lookup(env, "NZBPP_FINALDIR")
	if dir == "" {
		dir, _ = Lookup(env, "NZBPP_DIRECTORY")
	}
	if dir == "" {
		return j, errors.New("not started by NZBGet: neither NZBPP_FINALDIR nor NZBPP_DIRECTORY is set")
	}
	j.Paths = []string{dir}
	j.Label, _ = Lookup(env, "NZBPP_NZBNAME")
	j.Category, _ = Lookup(env, "NZBPP_CATEGORY")
	st := total
	if s, _ := Lookup(env, "NZBPP_STATUS"); s != "" {
		st += "/" + s
	}
	j.Event = "status " + st
	if total != "SUCCESS" {
		j.Skip = fmt.Sprintf("download status %s; the files are not usable", st)
	}
	return j, nil
}

// parseArr handles Sonarr and Radarr, whose variables differ only in the
// prefix and in the names of the path and title fields.
func parseArr(a Adapter, env []string) (Job, error) {
	j := Job{Adapter: a}
	prefix := string(a) + "_"
	program := "Sonarr"
	pathKey, pathsKey := "sonarr_episodefile_path", "sonarr_episodefile_paths"
	langKey, titleKey := "sonarr_series_originallanguage", "sonarr_series_title"
	if a == Radarr {
		program = "Radarr"
		pathKey, pathsKey = "radarr_moviefile_path", ""
		langKey, titleKey = "radarr_movie_originallanguage", "radarr_movie_title"
	}
	event, ok := Lookup(env, prefix+"eventtype")
	if !ok || event == "" {
		return j, fmt.Errorf("not started by %s: %seventtype is not set", program, prefix)
	}
	j.Event = event
	switch event {
	case "Test":
		j.Test = true
		return j, nil
	case "Download":
	default:
		j.Skip = fmt.Sprintf("event %s; nothing to ingest", event)
		return j, nil
	}
	if p, ok := Lookup(env, pathKey); ok && p != "" {
		j.Paths = append(j.Paths, p)
	} else if ps, ok := Lookup(env, pathsKey); pathsKey != "" && ok {
		for _, p := range strings.Split(ps, "|") {
			if p != "" {
				j.Paths = append(j.Paths, p)
			}
		}
	}
	if len(j.Paths) == 0 {
		if pathsKey != "" {
			return j, fmt.Errorf("event Download without %s or %s", pathKey, pathsKey)
		}
		return j, fmt.Errorf("event Download without %s", pathKey)
	}
	j.Original, _ = Lookup(env, langKey)
	title, _ := Lookup(env, titleKey)
	release, _ := Lookup(env, prefix+"release_title")
	switch {
	case title != "" && release != "":
		j.Label = title + ": " + release
	case title != "":
		j.Label = title
	default:
		j.Label = release
	}
	return j, nil
}

// Outcome is how the invocation ended.
type Outcome int

// The outcomes ExitCode distinguishes.
const (
	Ran         Outcome = iota // ingest ran; verdict is meaningful
	Skipped                    // nothing to do
	UsageError                 // bad flags, Parse error, tools missing
	Interrupted                // ctx cancelled during ingest
)

// failed reports whether the caller must see a failure. BLOCK is a failure
// under every fail-on value, whatever value the caller managed to pass.
func failed(verdict, failOn report.Severity) bool {
	return verdict == report.Block || verdict >= failOn
}

// ExitCode maps the outcome to the caller's convention. SABnzbd, Sonarr and
// Radarr use 0 for success, 1 for failure and 2 for a usage error, and 130
// when interrupted. NZBGet reads 93 as success, 94 as failure and 95 as
// nothing to do, and never sees any other value from amuxify: a usage error
// or an interruption is reported as 94. An unknown adapter follows the
// SABnzbd convention.
func ExitCode(a Adapter, verdict, failOn report.Severity, o Outcome) int {
	if a == NZBGet {
		switch o {
		case Ran:
			if failed(verdict, failOn) {
				return 94
			}
			return 93
		case Skipped:
			return 95
		}
		return 94
	}
	switch o {
	case Ran:
		if failed(verdict, failOn) {
			return 1
		}
		return 0
	case Skipped:
		return 0
	case UsageError:
		return int(report.Usage)
	}
	return 130
}

// MarkBad is the line that tells NZBGet to mark the download as bad.
const MarkBad = "[NZB] MARK=BAD"

// ControlLines are printed on stdout last. Only NZBGet has any: MarkBad when
// the verdict is BLOCK, so a WARN or FAIL never makes the caller blocklist
// and re-grab a release.
func ControlLines(a Adapter, verdict report.Severity) []string {
	if a == NZBGet && verdict == report.Block {
		return []string{MarkBad}
	}
	return nil
}

// ParseFailOn accepts warn, fail or block (case-insensitive).
func ParseFailOn(s string) (report.Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "warn":
		return report.Warn, nil
	case "fail":
		return report.Fail, nil
	case "block":
		return report.Block, nil
	}
	return report.Fail, fmt.Errorf("--fail-on must be warn, fail or block, not %q", s)
}

// MatchCategory reports whether category matches the glob (path.Match,
// case-insensitive). An empty glob matches everything. A malformed glob is
// an error, never a match.
func MatchCategory(glob, category string) (bool, error) {
	if glob == "" {
		return true, nil
	}
	ok, err := path.Match(strings.ToLower(glob), strings.ToLower(category))
	if err != nil {
		return false, fmt.Errorf("%q: %v", glob, err)
	}
	return ok, nil
}

// Lookup returns the value of key in env and whether it was present. The
// first entry wins when a key is repeated, as os.Getenv does.
func Lookup(env []string, key string) (string, bool) {
	if key == "" {
		return "", false
	}
	for _, kv := range env {
		if len(kv) > len(key) && kv[len(key)] == '=' && kv[:len(key)] == key {
			return kv[len(key)+1:], true
		}
	}
	return "", false
}
