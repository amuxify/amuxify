package cli

import "path/filepath"

// quarantineFlag is an optional-value flag. A bare --quarantine means
// <state-dir>/quarantine, --quarantine=DIR names a directory and
// --quarantine=off (or =false) turns it off. The standard flag package treats
// the argument after a bool-style flag as positional, so the directory form
// must use "=".
type quarantineFlag struct {
	set bool
	dir string
}

func (q *quarantineFlag) String() string {
	if q == nil || !q.set {
		return ""
	}
	if q.dir != "" {
		return q.dir
	}
	return "state-dir"
}

func (q *quarantineFlag) IsBoolFlag() bool { return true }

func (q *quarantineFlag) Set(v string) error {
	switch v {
	case "", "true":
		q.set, q.dir = true, ""
	case "false", "off", "0":
		q.set, q.dir = false, ""
	default:
		q.set, q.dir = true, v
	}
	return nil
}

// resolve returns the quarantine directory: "" when unset, the named
// directory, or <state-dir>/quarantine for the bare form.
func (q *quarantineFlag) resolve(stateDir string) string {
	if !q.set {
		return ""
	}
	if q.dir != "" {
		return q.dir
	}
	return filepath.Join(stateDir, "quarantine")
}
