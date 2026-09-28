// Package hook maps a download client's or media manager's environment to an
// ingest job and a run verdict to that caller's exit code. It runs no tools.
package hook

import (
	"errors"
	"fmt"
	"net/url"
	"os"
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

// SABnzbd's positional parameters, in the order SABnzbd passes them to a
// post-processing script. Versions before the failure URL was added pass
// the first seven; current versions pass all eight.
const (
	sabDir      = iota // the final directory of the job
	sabNZBName         // the name of the original NZB file
	sabJobName         // the clean job name, as shown in the queue
	sabReport          // the indexer's report number
	sabCategory        // the user-defined category
	sabGroup           // the newsgroup
	sabStatus          // the post-processing status: 0 is success
	sabFailURL         // the failure URL, or empty
)

// SABnzbdParams and SABnzbdParamsWithoutURL are the two counts of
// positional arguments the SABnzbd adapter accepts besides none at all.
const (
	SABnzbdParams           = sabFailURL + 1
	SABnzbdParamsWithoutURL = sabStatus + 1
)

// SABnzbdArgError says why positional arguments are not SABnzbd's
// parameters. Arg is the argument a wrapper added, when the values alone
// tell which one that is, so a caller can say how it should have been
// written; it is empty otherwise. It never names a value SABnzbd itself
// passed.
type SABnzbdArgError struct {
	Arg    string
	Reason string
}

func (e *SABnzbdArgError) Error() string { return e.Reason }

// CheckSABnzbdArgs decides whether args are SABnzbd's positional parameters,
// given the environment ("K=V" entries) the script was started with. The
// accepted forms are exactly these: no arguments at all, the seven
// parameters an older SABnzbd passes, or the eight a current one passes.
// Any other count is refused. The seventh parameter, the status, must not
// be empty in either form: the adapter only ever runs on a status of
// exactly 0 and refuses to guess when the wrapper dropped it.
//
// When SAB_COMPLETE_DIR is set the job is read from the environment, and
// the count and the status are all that is checked. Without it the job is
// read from the positionals, and one more shape is refused: a directory
// written in front of an older SABnzbd's seven parameters, which has the
// right count and would otherwise be ingested in place of the job's own
// directory, in silence. The tell is the second parameter. SABnzbd's is
// the name of the original NZB file, never a directory, so an existing
// directory there, or a symlink to one, means a directory was written in
// front. The first parameter is not inspected, because the stray directory
// may be a symlink or may not exist yet and either would let the shifted
// shape through.
//
// No other position is looked up on disk, and none at all in the
// environment form. In particular the eighth parameter, the failure URL, is
// never inspected: SABnzbd copies it from the X-DNZB-Failure header of the
// indexer's NZB response, so if an existing directory there were a tell, an
// indexer could send "/" and have every job it serves refused before the
// scan. A directory a wrapper writes after seven parameters is therefore
// read as the failure URL and ignored, which still leaves the job's own
// directory scanned. Every value is otherwise taken as data.
func CheckSABnzbdArgs(env, args []string) error {
	switch len(args) {
	case 0:
		return nil
	case SABnzbdParamsWithoutURL, SABnzbdParams:
	default:
		return &SABnzbdArgError{Arg: strayArg(args), Reason: fmt.Sprintf(
			"expected no positional arguments or SABnzbd's seven or eight parameters, got %d beginning with %q", len(args), args[0])}
	}
	if dir, _ := Lookup(env, "SAB_COMPLETE_DIR"); dir == "" && isDir(args[sabNZBName]) {
		return &SABnzbdArgError{Arg: args[sabDir], Reason: fmt.Sprintf(
			"%q is a directory where SABnzbd's second parameter, the original NZB name, belongs; a directory in front of SABnzbd's parameters is not read as one of them", args[sabNZBName])}
	}
	if args[sabStatus] == "" {
		return &SABnzbdArgError{Reason: "SABnzbd's seventh parameter, the post-processing status, is empty"}
	}
	return nil
}

// strayArg names the argument a wrapper added to a wrong count when the
// values alone tell where it is: the only argument when there is one, and
// for nine, one more than SABnzbd's eight, the first or the last. SABnzbd's
// eighth parameter is empty or a URL and its seventh, the status, is
// neither, so a last argument of that shape means the stray is first, and
// an eighth of that shape means the stray is last. Any other count, and a
// nine whose values fit neither reading, names nothing. Nothing is looked
// up on disk, so no value SABnzbd itself passed can be named: an indexer
// that puts a bare path in the failure URL only costs the hint.
func strayArg(args []string) string {
	switch len(args) {
	case 1:
		return args[0]
	case SABnzbdParams + 1:
		switch {
		case isFailURL(args[SABnzbdParams]):
			return args[0]
		case isFailURL(args[sabFailURL]):
			return args[SABnzbdParams]
		}
	}
	return ""
}

// isFailURL reports whether s has the shape of SABnzbd's eighth parameter:
// empty, or a URL with a scheme and a host.
func isFailURL(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// isDir reports whether path names an existing directory, following a
// symlink to one. A NUL in the path is an error from Stat, so such a value
// is simply not a directory.
func isDir(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// parseSABnzbd reads the job from the environment when SAB_COMPLETE_DIR is
// set, which is how SABnzbd 2 and later start a script, and otherwise from
// the positional parameters. The positional arguments are checked first in
// either case: SABnzbd sets both, so a wrong count is a wrapper mistake
// whichever form the job would then be read from. In the environment form
// SAB_PP_STATUS must be set, as SABnzbd always does alongside
// SAB_COMPLETE_DIR; a job whose status is missing is refused rather than
// assumed successful.
func parseSABnzbd(env []string, args []string) (Job, error) {
	j := Job{Adapter: SABnzbd}
	if err := CheckSABnzbdArgs(env, args); err != nil {
		return j, err
	}
	dir, _ := Lookup(env, "SAB_COMPLETE_DIR")
	var status string
	switch {
	case dir != "":
		status, _ = Lookup(env, "SAB_PP_STATUS")
		if status == "" {
			return j, errors.New("not started by SABnzbd: SAB_COMPLETE_DIR is set but SAB_PP_STATUS is not")
		}
		j.Label, _ = Lookup(env, "SAB_FINAL_NAME")
		j.Category, _ = Lookup(env, "SAB_CAT")
	case len(args) > 0:
		dir = args[sabDir]
		if args[sabJobName] != "" {
			j.Label = args[sabJobName]
		} else {
			j.Label = args[sabNZBName]
		}
		j.Category = args[sabCategory]
		status = args[sabStatus]
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
