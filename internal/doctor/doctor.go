// Package doctor reports whether this machine can run amuxify: which tools
// are present, whether they meet the version floors, whether the profile
// loads, and whether the environment is safe.
package doctor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/report"
)

// Check is one line of the doctor report.
type Check struct {
	Name     string          `json:"name"`
	Status   report.Severity `json:"status"`
	Detail   string          `json:"detail"`
	Required bool            `json:"required"`
}

type floor struct {
	tool     string
	required bool
	min      []int
	note     string
}

var floors = []floor{
	{exec.MKVMerge, true, []int{50, 0}, "MKVToolNix 50 or newer for --no-date and IETF language tags"},
	{exec.MKVPropedit, true, []int{50, 0}, "ships with MKVToolNix"},
	{exec.MKVExtract, false, []int{50, 0}, "optional: attachment content sniffing"},
	{exec.FFmpeg, true, []int{4, 4}, "4.4 minimum, 5.0 or newer recommended"},
	{exec.FFprobe, true, []int{4, 4}, "ships with ffmpeg"},
	{exec.ExifTool, false, nil, "optional: deep metadata reports"},
	{exec.ClamScan, false, nil, "optional: safety.clamav = optional|required"},
}

var verRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// Run executes all checks.
func Run(ctx context.Context, r *exec.Runner, profileName, stateDir string) ([]Check, report.Severity) {
	var checks []Check
	worst := report.Pass
	add := func(c Check) {
		checks = append(checks, c)
		if c.Status > worst {
			worst = c.Status
		}
	}
	for _, f := range floors {
		path, err := r.Path(f.tool)
		if err != nil {
			st := report.Warn
			if f.required {
				st = report.Usage
			}
			add(Check{Name: f.tool, Status: st, Detail: "not found (" + f.note + ")", Required: f.required})
			continue
		}
		line, err := r.Version(ctx, f.tool)
		if err != nil {
			add(Check{Name: f.tool, Status: report.Warn, Detail: path + ": cannot read version: " + err.Error(), Required: f.required})
			continue
		}
		v, ok := parseVersion(line)
		switch {
		case f.min == nil:
			add(Check{Name: f.tool, Status: report.Pass, Detail: fmt.Sprintf("%s (%s)", path, line), Required: f.required})
		case !ok:
			add(Check{Name: f.tool, Status: report.Warn, Detail: fmt.Sprintf("%s: unparseable version %q; need %s", path, line, f.note), Required: f.required})
		case less(v, f.min):
			add(Check{Name: f.tool, Status: report.Usage, Detail: fmt.Sprintf("%s: %s is older than %s", path, line, f.note), Required: f.required})
		default:
			add(Check{Name: f.tool, Status: report.Pass, Detail: fmt.Sprintf("%s (%s)", path, line), Required: f.required})
		}
	}
	if p, err := policy.Load(profileName); err != nil {
		add(Check{Name: "profile", Status: report.Usage, Detail: err.Error(), Required: true})
	} else {
		add(Check{Name: "profile", Status: report.Pass, Detail: p.Name + ": " + p.Description, Required: true})
		if p.Safety.ClamAV == "required" && !r.Have(exec.ClamScan) {
			add(Check{Name: "clamav", Status: report.Usage, Detail: "profile requires clamscan but it is not installed", Required: true})
		}
	}
	if fsutil.IsRoot() {
		add(Check{Name: "user", Status: report.Warn, Detail: "running as root; files created in a library will be root-owned. Use --allow-root only when intended."})
	} else {
		add(Check{Name: "user", Status: report.Pass, Detail: fmt.Sprintf("uid %d", os.Geteuid())})
	}
	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			add(Check{Name: "state-dir", Status: report.Warn, Detail: stateDir + ": " + err.Error()})
		} else if f, err := os.CreateTemp(stateDir, ".probe-*"); err != nil {
			add(Check{Name: "state-dir", Status: report.Warn, Detail: stateDir + ": not writable"})
		} else {
			f.Close()
			os.Remove(f.Name())
			add(Check{Name: "state-dir", Status: report.Pass, Detail: stateDir})
		}
	}
	tmp := os.TempDir()
	if f, err := os.CreateTemp(tmp, ".amuxify-*"); err != nil {
		add(Check{Name: "tmpdir", Status: report.Usage, Detail: tmp + ": not writable", Required: true})
	} else {
		f.Close()
		os.Remove(f.Name())
		add(Check{Name: "tmpdir", Status: report.Pass, Detail: tmp})
	}
	return checks, worst
}

func parseVersion(line string) ([]int, bool) {
	m := verRe.FindStringSubmatch(line)
	if m == nil {
		return nil, false
	}
	var v []int
	for _, s := range m[1:] {
		if s == "" {
			v = append(v, 0)
			continue
		}
		n, _ := strconv.Atoi(s)
		v = append(v, n)
	}
	return v, true
}

func less(a, b []int) bool {
	for i := range b {
		ai := 0
		if i < len(a) {
			ai = a[i]
		}
		if ai < b[i] {
			return true
		}
		if ai > b[i] {
			return false
		}
	}
	return false
}

// Format renders checks for a terminal.
func Format(checks []Check) string {
	var sb strings.Builder
	for _, c := range checks {
		st := c.Status.String()
		if c.Status == report.Usage {
			st = "MISSING"
		}
		fmt.Fprintf(&sb, "%-8s %-12s %s\n", st, c.Name, c.Detail)
	}
	return sb.String()
}
