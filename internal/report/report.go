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

// Summary is the run-level report.
type Summary struct {
	Tool     string         `json:"tool"`
	Version  string         `json:"version"`
	Command  string         `json:"command"`
	Profile  string         `json:"profile,omitempty"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Verdict  Severity       `json:"verdict"`
	Counts   map[string]int `json:"counts"`
	Files    []FileResult   `json:"files"`
	Errors   []string       `json:"errors,omitempty"`
}

// NewSummary starts a report for one run.
func NewSummary(tool, version, command, profile string) *Summary {
	return &Summary{Tool: tool, Version: version, Command: command, Profile: profile,
		Started: time.Now(), Counts: map[string]int{}}
}

// Append records a file result and updates counts and the run verdict.
func (s *Summary) Append(r FileResult) {
	r.Millis = r.Duration.Milliseconds()
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
func (s *Summary) Close() { s.Finished = time.Now() }

// WriteJSON emits the machine-readable form.
func (s *Summary) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// WriteHuman emits the terminal form: one line per file, then findings
// indented, then a count line and the run verdict.
func (s *Summary) WriteHuman(w io.Writer, verbose bool) {
	for _, f := range s.Files {
		fmt.Fprintf(w, "%-5s %s\n", f.Verdict, f.Path)
		for _, fd := range f.Findings {
			if fd.Severity == Pass && !verbose {
				continue
			}
			fmt.Fprintf(w, "      %-5s %-18s %s\n", fd.Severity, fd.Code, fd.Message)
			if verbose && fd.Detail != "" {
				for _, line := range strings.Split(fd.Detail, "\n") {
					fmt.Fprintf(w, "            %s\n", line)
				}
			}
		}
		if f.Output != "" {
			fmt.Fprintf(w, "      -> %s\n", f.Output)
		}
		if verbose {
			for _, a := range f.Actions {
				fmt.Fprintf(w, "      * %s\n", a)
			}
		}
	}
	for _, e := range s.Errors {
		fmt.Fprintf(w, "ERROR %s\n", e)
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
