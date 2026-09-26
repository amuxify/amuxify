// Package report defines verdicts, findings, and the exit-code contract that
// every amuxify subcommand shares. The tokens here are part of the public
// interface: hook scripts and log parsers depend on them staying fixed.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Severity orders verdicts from best to worst. The numeric values are the
// process exit codes, except Usage which is reserved for argument errors.
type Severity int

const (
	Pass  Severity = 0
	Warn  Severity = 1
	Usage Severity = 2
	Fail  Severity = 3
	Block Severity = 4
)

func (s Severity) String() string {
	switch s {
	case Pass:
		return "PASS"
	case Warn:
		return "WARN"
	case Usage:
		return "USAGE"
	case Fail:
		return "FAIL"
	case Block:
		return "BLOCK"
	}
	return fmt.Sprintf("SEVERITY(%d)", int(s))
}

// MarshalText makes Severity serialise as its token in JSON.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText accepts the token form.
func (s *Severity) UnmarshalText(b []byte) error {
	switch strings.ToUpper(string(b)) {
	case "PASS":
		*s = Pass
	case "WARN":
		*s = Warn
	case "USAGE":
		*s = Usage
	case "FAIL":
		*s = Fail
	case "BLOCK":
		*s = Block
	default:
		return fmt.Errorf("unknown severity %q", string(b))
	}
	return nil
}

// ExitCode maps a verdict to the process exit status. Precedence is
// BLOCK(4) > FAIL(3) > USAGE(2) > WARN(1) > PASS(0).
func ExitCode(s Severity) int { return int(s) }

// Worst returns the more severe of two verdicts.
func Worst(a, b Severity) Severity {
	if b > a {
		return b
	}
	return a
}

// Finding is one observation about one file. Code is a fixed ASCII token.
type Finding struct {
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Detail   string   `json:"detail,omitempty"`
}

// FileResult is the outcome for one input path.
type FileResult struct {
	Path     string            `json:"path"`
	Verdict  Severity          `json:"verdict"`
	Findings []Finding         `json:"findings"`
	Output   string            `json:"output,omitempty"`
	Actions  []string          `json:"actions,omitempty"`
	Info     map[string]string `json:"info,omitempty"`
	Duration time.Duration     `json:"-"`
	Millis   int64             `json:"duration_ms"`
}

// Add records a finding and raises the verdict when needed.
func (r *FileResult) Add(f Finding) {
	r.Findings = append(r.Findings, f)
	r.Verdict = Worst(r.Verdict, f.Severity)
}

// Addf is a convenience wrapper around Add.
func (r *FileResult) Addf(code string, sev Severity, format string, args ...interface{}) {
	r.Add(Finding{Code: code, Severity: sev, Message: fmt.Sprintf(format, args...)})
}

// Has reports whether a finding with the given code exists.
func (r *FileResult) Has(code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// SchemaID identifies the report shape. It changes only for a breaking change.
const SchemaID = "amuxify.report/1"

// HookInfo records the adapter that started an ingest run (hook runs only).
type HookInfo struct {
	Adapter  string `json:"adapter"`
	Event    string `json:"event"`
	Label    string `json:"label,omitempty"`
	Category string `json:"category,omitempty"`
	FailOn   string `json:"fail_on"`
	ExitCode int    `json:"exit_code"`
}

// Summary is the run-level report. Its JSON form is the wire format that
// docs/report.md documents and that TestFieldSetFrozen pins: fields are only
// ever added, and the arrays and maps are present and empty rather than
// absent or null.
type Summary struct {
	Schema   string         `json:"schema"`
	Tool     string         `json:"tool"`
	Version  string         `json:"version"`
	Command  string         `json:"command"`
	Profile  string         `json:"profile"`
	Hook     *HookInfo      `json:"hook,omitempty"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Verdict  Severity       `json:"verdict"`
	Counts   map[string]int `json:"counts"`
	Files    []FileResult   `json:"files"`
	Errors   []string       `json:"errors"`
}

// now returns the current time in UTC with whole seconds, so timestamps
// marshal as RFC 3339 with a Z suffix and no fraction.
func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

// NewSummary starts a report for one run.
func NewSummary(tool, version, command, profile string) *Summary {
	return &Summary{Schema: SchemaID, Tool: tool, Version: version, Command: command, Profile: profile,
		Started: now(), Counts: map[string]int{}, Files: []FileResult{}, Errors: []string{}}
}

// Append records a file result and updates counts and the run verdict. A
// result without findings is stored with an empty findings slice so that it
// marshals as an empty array.
func (s *Summary) Append(r FileResult) {
	r.Millis = r.Duration.Milliseconds()
	if r.Findings == nil {
		r.Findings = []Finding{}
	}
	if s.Counts == nil {
		s.Counts = map[string]int{}
	}
	s.Files = append(s.Files, r)
	s.Counts[r.Verdict.String()]++
	s.Verdict = Worst(s.Verdict, r.Verdict)
}

// Error records a run-level error (not tied to one file) and marks FAIL.
func (s *Summary) Error(msg string) {
	s.Errors = append(s.Errors, msg)
	s.Verdict = Worst(s.Verdict, Fail)
}

// Close stamps the finish time.
func (s *Summary) Close() { s.Finished = now() }

// MarshalJSON normalises the collections so that a Summary assembled by hand,
// without NewSummary or Append, still marshals with present, empty arrays and
// maps rather than null. The caller's slices are never modified.
func (s Summary) MarshalJSON() ([]byte, error) {
	type plain Summary
	p := plain(s)
	if p.Counts == nil {
		p.Counts = map[string]int{}
	}
	if p.Errors == nil {
		p.Errors = []string{}
	}
	files := make([]FileResult, len(p.Files))
	for i, f := range p.Files {
		if f.Findings == nil {
			f.Findings = []Finding{}
		}
		files[i] = f
	}
	p.Files = files
	return json.Marshal(p)
}

// WriteJSON emits the machine-readable form: one indented object followed by
// a newline.
func (s *Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteHuman emits the terminal form: one line per file, then findings
// indented, then a count line and the run verdict.
func (s *Summary) WriteHuman(w io.Writer, verbose bool) {
	for _, f := range s.Files {
		f.WriteHuman(w, verbose)
	}
	s.WriteHumanTail(w)
}

// WriteHuman prints one file's verdict line and its findings. Used both for
// the final report and for streaming a result as soon as the file is done.
// Every value that came from a file or a caller goes through Sanitize, so
// the line structure of the terminal form is amuxify's own: a file name
// cannot end a line early, forge a verdict line or hide characters.
func (f *FileResult) WriteHuman(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "%-5s %s\n", f.Verdict, Sanitize(f.Path))
	for _, fd := range f.Findings {
		if fd.Severity == Pass && !verbose {
			continue
		}
		fmt.Fprintf(w, "      %-5s %-18s %s\n", fd.Severity, fd.Code, Sanitize(fd.Message))
		if verbose && fd.Detail != "" {
			for _, line := range strings.Split(fd.Detail, "\n") {
				fmt.Fprintf(w, "            %s\n", Sanitize(line))
			}
		}
	}
	if f.Output != "" {
		fmt.Fprintf(w, "      -> %s\n", Sanitize(f.Output))
	}
	if verbose {
		for _, a := range f.Actions {
			fmt.Fprintf(w, "      * %s\n", Sanitize(a))
		}
	}
}

// WriteHumanTail prints run-level errors and the count line.
func (s *Summary) WriteHumanTail(w io.Writer) {
	for _, e := range s.Errors {
		fmt.Fprintf(w, "ERROR %s\n", Sanitize(e))
	}
	keys := make([]string, 0, len(s.Counts))
	for k := range s.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, s.Counts[k]))
	}
	fmt.Fprintf(w, "\n%s: %d file(s) %s\n", s.Verdict, len(s.Files), strings.Join(parts, " "))
}

// Sanitize returns s with every character that could reshape terminal or
// log output replaced by a visible escape: a control character other than
// tab (0x00-0x1F, 0x7F and the C1 range 0x80-0x9F) becomes \xNN, and every
// Unicode format character (general category Cf: the bidirectional
// controls, zero-width characters, the byte order mark, the soft hyphen,
// the tag characters and the other invisible ones) and the line and
// paragraph separators become \uNNNN, or \UNNNNNNNN above U+FFFF so the
// escape cannot be confused with a shorter one followed by a hex digit. The
// human report writers and the hook log lines use it, so a file name
// carrying an escape sequence, a carriage return, a newline or a bidi
// override cannot overwrite, split or reorder a line. Bytes that are not
// valid UTF-8 pass through unchanged, as the terminal form promises. The
// JSON form is untouched: it carries the raw value with JSON escaping.
func Sanitize(s string) string {
	i := 0
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !(r == utf8.RuneError && n == 1) && sanitized(r) {
			break
		}
		i += n
	}
	if i >= len(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	b.WriteString(s[:i])
	for i < len(s) {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteByte(s[i])
		case !sanitized(r):
			b.WriteString(s[i : i+n])
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0xFFFF:
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
		i += n
	}
	return b.String()
}

// sanitized reports whether Sanitize replaces r: the C0 and C1 controls and
// DEL except tab, every format character (unicode.Cf, which holds the bidi
// controls, the zero-width characters, the byte order mark, the soft
// hyphen, the tag characters and the rest of the invisible ones), and the
// line and paragraph separators. The category test, rather than a list of
// code points, is what keeps a newly noticed invisible character from
// slipping through.
func sanitized(r rune) bool {
	switch {
	case r == '\t':
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	}
	return unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029
}
