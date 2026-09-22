// Package policy loads profiles and turns a probed file into a Decision:
// which tracks to keep, which flags to set, what to strip. Everything here is
// pure and unit-testable; no tool runs.
package policy

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

//go:embed profiles/*.toml
var builtin embed.FS

// Profile is the full policy document.
type Profile struct {
	Name        string `toml:"-"`
	Description string `toml:"description"`
	Languages   struct {
		Keep           []string `toml:"keep"`
		PreferOriginal bool     `toml:"prefer_original"`
		Und            string   `toml:"und"` // keep | drop | assume:<lang>
	} `toml:"languages"`
	Audio struct {
		DropCommentary bool `toml:"drop_commentary"`
		KeepFirst      bool `toml:"keep_first"` // keep only the first matching audio track
	} `toml:"audio"`
	Subtitles struct {
		KeepForced        bool   `toml:"keep_forced"`
		DropSDHDuplicates bool   `toml:"drop_sdh_duplicates"`
		ScanLinks         bool   `toml:"scan_links"`
		Links             string `toml:"links"` // warn | fail
	} `toml:"subtitles"`
	Chapters struct {
		Keep bool `toml:"keep"`
	} `toml:"chapters"`
	Attachments struct {
		Fonts    string `toml:"fonts"`     // keep | drop | keep_if_text_subs
		CoverArt string `toml:"cover_art"` // keep | drop
		Other    string `toml:"other"`     // block | drop
	} `toml:"attachments"`
	Metadata struct {
		StripTitle       bool   `toml:"strip_title"`
		StripTags        bool   `toml:"strip_tags"`
		StripTrackTitles bool   `toml:"strip_track_titles"`
		StripProvenance  bool   `toml:"strip_provenance"`
		Links            string `toml:"links"`      // warn | fail
		Provenance       string `toml:"provenance"` // warn | fail
	} `toml:"metadata"`
	Streams struct {
		AllowDataTags []string `toml:"allow_data_tags"`
	} `toml:"streams"`
	Verify struct {
		Tier       string `toml:"tier"` // quick | full | none
		StreamHash bool   `toml:"stream_hash"`
	} `toml:"verify"`
	Safety struct {
		RefuseRoot      bool   `toml:"refuse_root"`
		Hardlinks       string `toml:"hardlinks"`        // skip | break | copy
		ExecPermissions string `toml:"exec_permissions"` // ignore | warn | fail
		ClamAV          string `toml:"clamav"`           // off | optional | required
	} `toml:"safety"`
	Sidecars struct {
		Allow []string `toml:"allow"`
		Block []string `toml:"block"`
	} `toml:"sidecars"`
}

// Default is the profile used when none is named.
const Default = "homelab"

// Names lists the built-in profiles.
func Names() []string {
	entries, _ := fs.ReadDir(builtin, "profiles")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
	}
	sort.Strings(names)
	return names
}

// Source returns the raw TOML of a built-in profile.
func Source(name string) ([]byte, error) {
	b, err := builtin.ReadFile("profiles/" + name + ".toml")
	if err != nil {
		return nil, fmt.Errorf("unknown built-in profile %q (have: %s)", name, strings.Join(Names(), ", "))
	}
	return b, nil
}

// Load resolves a profile by built-in name or by path to a TOML file. A
// path is anything containing a slash or ending in .toml.
func Load(nameOrPath string) (*Profile, error) {
	if nameOrPath == "" {
		nameOrPath = Default
	}
	var raw []byte
	var err error
	name := nameOrPath
	if strings.ContainsAny(nameOrPath, `/\`) || strings.HasSuffix(nameOrPath, ".toml") {
		raw, err = os.ReadFile(nameOrPath)
		if err != nil {
			return nil, err
		}
	} else {
		raw, err = Source(nameOrPath)
		if err != nil {
			return nil, err
		}
	}
	p, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", nameOrPath, err)
	}
	p.Name = name
	return p, nil
}

// Parse decodes TOML into a Profile on top of the homelab defaults, so a
// user file only has to state what differs.
func Parse(raw []byte) (*Profile, error) {
	base, _ := Source(Default)
	p := &Profile{}
	if _, err := toml.Decode(string(base), p); err != nil {
		return nil, fmt.Errorf("built-in defaults: %w", err)
	}
	md, err := toml.Decode(string(raw), p)
	if err != nil {
		return nil, err
	}
	if undec := md.Undecoded(); len(undec) > 0 {
		keys := make([]string, 0, len(undec))
		for _, k := range undec {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func oneOf(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s: %q is not one of %s", field, v, strings.Join(allowed, "|"))
}

// Validate checks enumerations.
func (p *Profile) Validate() error {
	if len(p.Languages.Keep) == 0 {
		return fmt.Errorf("languages.keep must list at least one language or \"*\"")
	}
	if u := p.Languages.Und; u != "keep" && u != "drop" && !strings.HasPrefix(u, "assume:") {
		return fmt.Errorf("languages.und: %q must be keep, drop or assume:<lang>", u)
	}
	checks := []error{
		oneOf("subtitles.links", p.Subtitles.Links, "warn", "fail"),
		oneOf("attachments.fonts", p.Attachments.Fonts, "keep", "drop", "keep_if_text_subs"),
		oneOf("attachments.cover_art", p.Attachments.CoverArt, "keep", "drop"),
		oneOf("attachments.other", p.Attachments.Other, "block", "drop"),
		oneOf("metadata.links", p.Metadata.Links, "warn", "fail"),
		oneOf("metadata.provenance", p.Metadata.Provenance, "warn", "fail"),
		oneOf("verify.tier", p.Verify.Tier, "quick", "full", "none"),
		oneOf("safety.hardlinks", p.Safety.Hardlinks, "skip", "break", "copy"),
		oneOf("safety.exec_permissions", p.Safety.ExecPermissions, "ignore", "warn", "fail"),
		oneOf("safety.clamav", p.Safety.ClamAV, "off", "optional", "required"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}
	return nil
}

// KeepsAllLanguages reports whether the language filter is a wildcard.
func (p *Profile) KeepsAllLanguages() bool {
	for _, l := range p.Languages.Keep {
		if l == "*" {
			return true
		}
	}
	return false
}
