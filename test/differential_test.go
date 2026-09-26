// Package test holds the differential test: the frozen 0.1.x Bash scripts
// under legacy/ and the Go binary must agree on PASS versus flagged for every
// fixture where 0.1.x semantics still apply. Run with AMUXIFY_DIFFTEST=1.
package test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type summary struct {
	Files []struct {
		Path    string `json:"path"`
		Verdict string `json:"verdict"`
	} `json:"files"`
}

func TestDifferential(t *testing.T) {
	if os.Getenv("AMUXIFY_DIFFTEST") == "" {
		t.Skip("set AMUXIFY_DIFFTEST=1 to run (needs ffmpeg, mkvtoolnix, python3)")
	}
	for _, tool := range []string{"ffmpeg", "mkvmerge", "mkvpropedit", "python3", "bash"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	root, _ := filepath.Abs("..")
	bin := filepath.Join(root, "bin", "amuxify")
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("build first: make build (%v)", err)
	}
	fx := t.TempDir()
	gen := exec.Command(filepath.Join(root, "testdata", "gen-fixtures.sh"), fx)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("fixtures: %v\n%s", err, out)
	}

	// Go verdicts for the whole tree, archive profile (closest to 0.1.x).
	cmd := exec.Command(bin, "--profile", "archive", "--json", "scan", "--verify", "none", fx)
	out, _ := cmd.Output()
	var s summary
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	goVerdict := map[string]string{}
	for _, f := range s.Files {
		goVerdict[filepath.Base(f.Path)] = f.Verdict
	}

	// Expected outcome per fixture. legacy: 0.1.x exits 1 when a file is
	// BLOCKED. goFlag: Go verdict is FAIL or BLOCK under archive. Where they
	// differ the reason is a documented behaviour change (see MIGRATION.md).
	type want struct{ legacy, goFlag bool }
	cases := map[string]want{
		"clean.mkv":      {false, false},
		"exe_attach.mkv": {true, true},
		"fake_font.mkv":  {true, true},
		"polyglot.mkv":   {true, true},
		"text.mkv":       {true, true},
		"empty.mkv":      {true, true},
		"truncated.mp4":  {true, true},
		"exec_perm.mkv":  {true, true},
		"sample.avi":     {false, false},
		"sample.ts":      {false, false},
		"sample.webm":    {false, false},
		"sample.mov":     {false, false},
		// Legacy blocks every attachment; Go drops the font by policy and
		// instead fails the file for links in tags and subtitles.
		"multi.mkv": {true, true},
		// Legacy does not compare extension with content for MKV-in-.mp4.
		"mislabeled.mp4": {false, true},
	}
	legacy := filepath.Join(root, "legacy", "libexec", "scan-media.sh")
	for name, w := range cases {
		path := filepath.Join(fx, name)
		lc := exec.Command("bash", legacy, "--strict-permissions", "--allow-data-tag", "text", path)
		lc.Env = append(os.Environ(), "AMUXIFY_COMMAND=amux-scan-all", "AMUXIFY_VERSION=0.1.1")
		lout, lerr := lc.CombinedOutput()
		if legacyFlag := lerr != nil; legacyFlag != w.legacy {
			t.Errorf("legacy %s: flagged=%v want %v\n%s", name, legacyFlag, w.legacy, lout)
		}
		gv := goVerdict[name]
		if goFlag := gv == "FAIL" || gv == "BLOCK"; goFlag != w.goFlag {
			t.Errorf("go %s: verdict %s (flagged=%v) want flagged=%v", name, gv, goFlag, w.goFlag)
		}
	}
	// Remux both ways and compare the kept track layout of clean.mkv.
	goOut := filepath.Join(fx, "go_out")
	rc := exec.Command(bin, "--profile", "archive", "remux", "--output", goOut, filepath.Join(fx, "clean.mkv"))
	if o, err := rc.CombinedOutput(); err != nil {
		t.Fatalf("go remux: %v\n%s", err, o)
	}
	ident := func(p string) string {
		o, err := exec.Command("mkvmerge", "-J", p).Output()
		if err != nil && len(o) == 0 {
			t.Fatalf("mkvmerge -J %s: %v", p, err)
		}
		var j struct {
			Tracks []struct {
				Type  string `json:"type"`
				Props struct {
					Lang string `json:"language"`
				} `json:"properties"`
			} `json:"tracks"`
		}
		_ = json.Unmarshal(o, &j)
		var parts []string
		for _, tr := range j.Tracks {
			parts = append(parts, tr.Type+":"+tr.Props.Lang)
		}
		return strings.Join(parts, ",")
	}
	legacyRemux := filepath.Join(root, "legacy", "libexec", "remux-media.sh")
	lr := exec.Command("bash", legacyRemux, filepath.Join(fx, "clean.mkv"))
	lr.Env = append(os.Environ(), "AMUXIFY_COMMAND=amux-remux", "AMUXIFY_VERSION=0.1.1")
	lr.Stdin = nil
	if o, err := lr.CombinedOutput(); err != nil {
		t.Fatalf("legacy remux: %v\n%s", err, o)
	}
	legacyOut := filepath.Join(fx+"__remuxed", "clean.mkv")
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(fx), "*__remuxed", "clean.mkv"))
	if len(matches) > 0 {
		legacyOut = matches[0]
	}
	if _, err := os.Stat(legacyOut); err != nil {
		t.Fatalf("legacy remux produced no output at %s", legacyOut)
	}
	if a, b := ident(filepath.Join(goOut, "clean.mkv")), ident(legacyOut); a != b {
		t.Errorf("track layout differs: go=%s legacy=%s", a, b)
	}
}
