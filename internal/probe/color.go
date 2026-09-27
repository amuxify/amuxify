package probe

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Color is the colour and HDR signalling of one video stream in one
// normalised form, whichever tool reported it. ffprobe reports the colour
// description as names and the static HDR metadata as side data; mkvmerge
// -J reports the Matroska track header's numeric codes and floats. Both are
// folded into this struct so a source and an output can be compared
// field by field after a remux. A field that is absent in every tool's
// output is left at its zero value and the pointers stay nil; a field whose
// value could not be trusted (not a number, NaN, infinite, negative where
// that is impossible, or beyond any magnitude the format can express) is
// never stored and its name is listed in Malformed instead, so that a
// malformed value in the source and a missing value in the output still
// compare as different.
type Color struct {
	Primaries string `json:"primaries,omitempty"` // ffprobe's name, for example bt2020
	Transfer  string `json:"transfer,omitempty"`  // for example smpte2084 or arib-std-b67
	Matrix    string `json:"matrix,omitempty"`    // for example bt2020nc
	Range     string `json:"range,omitempty"`     // tv or pc

	Mastering   *MasteringDisplay `json:"mastering_display,omitempty"`
	Light       *ContentLight     `json:"content_light,omitempty"`
	DolbyVision *DolbyVision      `json:"dolby_vision,omitempty"`

	Malformed []string `json:"malformed,omitempty"` // field names whose value was rejected
}

// MasteringDisplay is the SMPTE ST 2086 mastering display metadata: the
// chromaticity of the three primaries and the white point in CIE 1931 xy,
// and the display's luminance range in cd/m². The two groups are signalled
// independently, so each has its own presence flag.
type MasteringDisplay struct {
	HasPrimaries bool    `json:"has_primaries"`
	RedX         float64 `json:"red_x,omitempty"`
	RedY         float64 `json:"red_y,omitempty"`
	GreenX       float64 `json:"green_x,omitempty"`
	GreenY       float64 `json:"green_y,omitempty"`
	BlueX        float64 `json:"blue_x,omitempty"`
	BlueY        float64 `json:"blue_y,omitempty"`
	WhiteX       float64 `json:"white_x,omitempty"`
	WhiteY       float64 `json:"white_y,omitempty"`
	HasLuminance bool    `json:"has_luminance"`
	MinLuminance float64 `json:"min_luminance,omitempty"`
	MaxLuminance float64 `json:"max_luminance,omitempty"`
}

// ContentLight is the CTA-861.3 content light level: the maximum content
// light level and the maximum frame-average light level in cd/m².
type ContentLight struct {
	MaxCLL  int `json:"max_cll"`
	MaxFALL int `json:"max_fall"`
}

// DolbyVision is the Dolby Vision configuration record ffprobe reports as
// stream side data.
type DolbyVision struct {
	Profile                 int  `json:"profile"`
	Level                   int  `json:"level"`
	RPU                     bool `json:"rpu"`
	EL                      bool `json:"el"`
	BL                      bool `json:"bl"`
	BLSignalCompatibilityID int  `json:"bl_signal_compatibility_id"`
}

// Bounds on parsed numbers. A chromaticity coordinate is a CIE xy value and
// lies in [0, 1]. A luminance is in cd/m²; the PQ curve peaks at 10000 and
// nothing a display can master exceeds maxLuminanceBound. Content light
// levels are 16-bit unsigned in every container. The Dolby Vision fields
// are 7, 6, 1 and 4 bit unsigned in the configuration record.
const (
	maxLuminanceBound = 100000.0
	maxContentLight   = 65535
	maxDVProfile      = 127
	maxDVLevel        = 63
	maxDVCompatID     = 15
	maxEnumLen        = 32
)

// Empty reports whether no colour or HDR field was read at all.
func (c *Color) Empty() bool {
	return c == nil || (c.Primaries == "" && c.Transfer == "" && c.Matrix == "" && c.Range == "" &&
		c.Mastering == nil && c.Light == nil && c.DolbyVision == nil && len(c.Malformed) == 0)
}

// colorField is one flattened field of a Color, used to describe and compare.
type colorField struct {
	name  string
	str   string
	num   float64
	float bool // a measured value, compared with a tolerance
}

// fields flattens c into an ordered list. The chromaticity and luminance
// values are marked as floats so that comparisons can apply a tolerance;
// integers and names compare exactly. Every field carries the string shown
// to users.
func (c *Color) fields() []colorField {
	if c == nil {
		return nil
	}
	var out []colorField
	str := func(name, v string) {
		if v != "" {
			out = append(out, colorField{name: name, str: v})
		}
	}
	num := func(name string, v float64) {
		out = append(out, colorField{name: name, str: formatNum(v), num: v, float: true})
	}
	integer := func(name string, v int) {
		out = append(out, colorField{name: name, str: strconv.Itoa(v)})
	}
	boolean := func(name string, v bool) {
		n := 0
		if v {
			n = 1
		}
		integer(name, n)
	}
	str("primaries", c.Primaries)
	str("transfer", c.Transfer)
	str("matrix", c.Matrix)
	str("range", c.Range)
	if m := c.Mastering; m != nil {
		if m.HasPrimaries {
			num("red_x", m.RedX)
			num("red_y", m.RedY)
			num("green_x", m.GreenX)
			num("green_y", m.GreenY)
			num("blue_x", m.BlueX)
			num("blue_y", m.BlueY)
			num("white_x", m.WhiteX)
			num("white_y", m.WhiteY)
		}
		if m.HasLuminance {
			num("min_luminance", m.MinLuminance)
			num("max_luminance", m.MaxLuminance)
		}
	}
	if l := c.Light; l != nil {
		integer("max_cll", l.MaxCLL)
		integer("max_fall", l.MaxFALL)
	}
	if d := c.DolbyVision; d != nil {
		integer("dv_profile", d.Profile)
		integer("dv_level", d.Level)
		boolean("dv_rpu", d.RPU)
		boolean("dv_el", d.EL)
		boolean("dv_bl", d.BL)
		integer("dv_bl_signal_compatibility_id", d.BLSignalCompatibilityID)
	}
	for _, m := range c.Malformed {
		out = append(out, colorField{name: m, str: "malformed"})
	}
	return out
}

// String lists every present field as name=value pairs separated by
// spaces, in a fixed order, for a finding's detail. Values are numbers and
// names that passed validation, so the text never carries bytes from the
// tool output.
func (c *Color) String() string {
	var parts []string
	for _, f := range c.fields() {
		parts = append(parts, f.name+"="+f.str)
	}
	return strings.Join(parts, " ")
}

// ColorDiff lists, in plain words, every field that differs between the
// colour signalling of a source stream and of its remuxed copy: fields the
// output lost, fields it gained, and fields whose value changed. The
// chromaticity and luminance values are compared with a small tolerance,
// because a value stored as a fixed-point fraction in one container and as
// a float in another comes back from ffprobe as two rationals that differ
// in the eighth digit; a difference of one part in a thousand (or one
// millionth absolute, for values near zero) is reported. Names, light
// levels and the Dolby Vision fields must match exactly. An empty result
// means the two agree.
func ColorDiff(src, out *Color) []string {
	var diffs []string
	groups := []struct {
		name         string
		inSrc, inOut bool
		prefixes     []string
	}{
		{"mastering display metadata", src != nil && src.Mastering != nil, out != nil && out.Mastering != nil,
			[]string{"red_", "green_", "blue_", "white_", "min_luminance", "max_luminance"}},
		{"content light level", src != nil && src.Light != nil, out != nil && out.Light != nil, []string{"max_cll", "max_fall"}},
		{"dolby vision configuration", src != nil && src.DolbyVision != nil, out != nil && out.DolbyVision != nil, []string{"dv_"}},
	}
	skip := func(name string) bool {
		for _, g := range groups {
			if g.inSrc == g.inOut {
				continue
			}
			for _, p := range g.prefixes {
				if strings.HasPrefix(name, p) {
					return true
				}
			}
		}
		return false
	}
	for _, g := range groups {
		switch {
		case g.inSrc && !g.inOut:
			diffs = append(diffs, g.name+" lost")
		case !g.inSrc && g.inOut:
			diffs = append(diffs, g.name+" gained")
		}
	}
	sf, of := src.fields(), out.fields()
	byName := map[string]colorField{}
	for _, f := range of {
		byName[f.name] = f
	}
	seen := map[string]bool{}
	for _, s := range sf {
		seen[s.name] = true
		if skip(s.name) {
			continue
		}
		o, ok := byName[s.name]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s lost (was %s)", s.name, s.str))
		case s.float && o.float:
			if !closeEnough(s.num, o.num) {
				diffs = append(diffs, fmt.Sprintf("%s changed from %s to %s", s.name, s.str, o.str))
			}
		case s.str != o.str:
			diffs = append(diffs, fmt.Sprintf("%s changed from %s to %s", s.name, s.str, o.str))
		}
	}
	for _, o := range of {
		if !seen[o.name] && !skip(o.name) {
			diffs = append(diffs, fmt.Sprintf("%s gained (now %s)", o.name, o.str))
		}
	}
	return diffs
}

func closeEnough(a, b float64) bool {
	d := math.Abs(a - b)
	return d <= 1e-6 || d <= 1e-3*math.Max(math.Abs(a), math.Abs(b))
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// colorSetter accumulates a Color while parsing one tool's output. Each
// set method validates its value and records a rejected one in Malformed.
type colorSetter struct {
	c         Color
	malformed map[string]bool
}

func (s *colorSetter) reject(name string) {
	if s.malformed == nil {
		s.malformed = map[string]bool{}
	}
	s.malformed[name] = true
}

func (s *colorSetter) done() Color {
	for name := range s.malformed {
		s.c.Malformed = append(s.c.Malformed, name)
	}
	sort.Strings(s.c.Malformed)
	return s.c
}

// enum accepts a colour description name from ffprobe: a short token of
// letters, digits, dots and dashes. Anything else came from a tool that was
// not ffprobe or from a corrupt stream and is rejected. The names ffprobe
// uses for unspecified values are folded to the empty string, because the
// Matroska header and the codec bitstream both express "not signalled" that
// way and mkvmerge writes or omits the element depending on the input.
func (s *colorSetter) enum(name, v string) string {
	switch v {
	case "", "unknown", "unspecified", "reserved":
		return ""
	}
	if len(v) > maxEnumLen {
		s.reject(name)
		return ""
	}
	for _, r := range v {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' || r == '+'
		if !ok {
			s.reject(name)
			return ""
		}
	}
	return strings.ToLower(v)
}

// number parses a JSON value that should hold a number: a JSON number, or
// a string holding a decimal number or a rational "num/den" as ffprobe
// prints for AVRational fields. NaN, infinities, a zero denominator and
// anything outside [lo, hi] are rejected.
func number(raw json.RawMessage, lo, hi float64) (float64, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return 0, false
	}
	if strings.HasPrefix(t, "\"") {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return 0, false
		}
		t = strings.TrimSpace(str)
	}
	if len(t) > 64 {
		return 0, false
	}
	var v float64
	if num, den, ok := strings.Cut(t, "/"); ok {
		n, err1 := parseFinite(num)
		d, err2 := parseFinite(den)
		if err1 != nil || err2 != nil || d == 0 {
			return 0, false
		}
		v = n / d
	} else {
		n, err := parseFinite(t)
		if err != nil {
			return 0, false
		}
		v = n
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v < lo || v > hi {
		return 0, false
	}
	return v, true
}

// parseFinite parses a decimal number and refuses the textual forms of NaN
// and infinity that strconv accepts, as well as hexadecimal and underscored
// forms, so that only what a tool prints as a plain number is accepted.
func parseFinite(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || r == '.' || r == '-' || r == '+' || r == 'e' || r == 'E') {
			return 0, fmt.Errorf("not a plain number")
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("not finite")
	}
	return v, nil
}

// integer parses a JSON value that should hold a whole number in [0, hi].
func integer(raw json.RawMessage, hi int) (int, bool) {
	v, ok := number(raw, 0, float64(hi))
	if !ok || v != math.Trunc(v) {
		return 0, false
	}
	return int(v), true
}

// colorFromFFprobe reads the colour description and the stream side data of
// one ffprobe stream. side is the raw side_data_list; a value of any other
// shape than an array of objects is ignored entry by entry, so one odd
// entry never hides the others. The first entry of each kind wins, so a
// duplicated entry cannot override the values of the first.
func colorFromFFprobe(primaries, transfer, matrix, rng string, side json.RawMessage) Color {
	var s colorSetter
	s.c.Primaries = s.enum("primaries", primaries)
	s.c.Transfer = s.enum("transfer", transfer)
	s.c.Matrix = s.enum("matrix", matrix)
	s.c.Range = s.enum("range", rng)
	for _, sd := range sideDataEntries(side) {
		switch t := sideDataType(sd); {
		case strings.Contains(t, "mastering display") && s.c.Mastering == nil:
			s.c.Mastering = s.mastering(sd)
		case strings.Contains(t, "content light") && s.c.Light == nil:
			s.c.Light = s.contentLight(sd)
		case strings.Contains(t, "dovi configuration") && s.c.DolbyVision == nil:
			s.c.DolbyVision = s.dolbyVision(sd)
		}
	}
	return s.done()
}

// sideDataEntries decodes side_data_list leniently: a missing or malformed
// list is empty, and an element that is not an object is skipped.
func sideDataEntries(raw json.RawMessage) []map[string]json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	var out []map[string]json.RawMessage
	for _, it := range items {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(it, &m); err != nil || m == nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// sideDataType returns the lower-cased side_data_type of an entry, or the
// empty string when it is missing or not a string.
func sideDataType(sd map[string]json.RawMessage) string {
	var t string
	if err := json.Unmarshal(sd["side_data_type"], &t); err != nil {
		return ""
	}
	return strings.ToLower(t)
}

// hdrLabels derives the short labels the scan reports (hdr10, hlg, dovi,
// hdr10plus) from the transfer characteristic and the side data types, each
// at most once.
func hdrLabels(transfer string, side json.RawMessage) []string {
	labels := addLabel(nil, transferLabel(transfer))
	for _, sd := range sideDataEntries(side) {
		t := sideDataType(sd)
		if strings.Contains(t, "dovi") || strings.Contains(t, "dolby vision") {
			labels = addLabel(labels, "dovi")
		}
		if strings.Contains(t, "hdr10+") || strings.Contains(t, "dynamic hdr") {
			labels = addLabel(labels, "hdr10plus")
		}
	}
	return labels
}

// transferLabel names the HDR family a transfer characteristic implies.
func transferLabel(transfer string) string {
	switch transfer {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	}
	return ""
}

// addLabel appends l unless it is empty or already present. The transfer
// label is kept first, as fromFFprobe orders them.
func addLabel(labels []string, l string) []string {
	if l == "" {
		return labels
	}
	for _, have := range labels {
		if have == l {
			return labels
		}
	}
	if l == "hdr10" || l == "hlg" {
		return append([]string{l}, labels...)
	}
	return append(labels, l)
}

func (s *colorSetter) mastering(sd map[string]json.RawMessage) *MasteringDisplay {
	m := &MasteringDisplay{}
	coords := []struct {
		key string
		dst *float64
	}{
		{"red_x", &m.RedX}, {"red_y", &m.RedY}, {"green_x", &m.GreenX}, {"green_y", &m.GreenY},
		{"blue_x", &m.BlueX}, {"blue_y", &m.BlueY}, {"white_point_x", &m.WhiteX}, {"white_point_y", &m.WhiteY},
	}
	present, valid := 0, 0
	for _, c := range coords {
		raw, ok := sd[c.key]
		if !ok {
			continue
		}
		present++
		v, ok := number(raw, 0, 1)
		if !ok {
			s.reject(strings.Replace(c.key, "white_point", "white", 1))
			continue
		}
		*c.dst = v
		valid++
	}
	m.HasPrimaries = valid == len(coords)
	if present > 0 && present < len(coords) {
		// Only a complete set of eight coordinates describes a display; a
		// partial one is recorded as malformed so it is never mistaken for
		// no metadata.
		s.reject("chromaticity_coordinates")
	}
	lumPresent, lumValid := 0, 0
	if raw, ok := sd["min_luminance"]; ok {
		lumPresent++
		if v, ok := number(raw, 0, maxLuminanceBound); ok {
			m.MinLuminance = v
			lumValid++
		} else {
			s.reject("min_luminance")
		}
	}
	if raw, ok := sd["max_luminance"]; ok {
		lumPresent++
		if v, ok := number(raw, 0, maxLuminanceBound); ok {
			m.MaxLuminance = v
			lumValid++
		} else {
			s.reject("max_luminance")
		}
	}
	m.HasLuminance = lumPresent == 2 && lumValid == 2
	if lumPresent > 0 && !m.HasLuminance {
		s.reject("luminance")
	}
	if !m.HasPrimaries && !m.HasLuminance {
		return nil
	}
	return m
}

func (s *colorSetter) contentLight(sd map[string]json.RawMessage) *ContentLight {
	l := &ContentLight{}
	ok1, ok2 := false, false
	if raw, ok := sd["max_content"]; ok {
		l.MaxCLL, ok1 = integer(raw, maxContentLight)
		if !ok1 {
			s.reject("max_cll")
		}
	}
	if raw, ok := sd["max_average"]; ok {
		l.MaxFALL, ok2 = integer(raw, maxContentLight)
		if !ok2 {
			s.reject("max_fall")
		}
	}
	if !ok1 || !ok2 {
		return nil
	}
	return l
}

func (s *colorSetter) dolbyVision(sd map[string]json.RawMessage) *DolbyVision {
	d := &DolbyVision{}
	ok := true
	get := func(key, name string, hi int) int {
		raw, present := sd[key]
		if !present {
			ok = false
			s.reject(name)
			return 0
		}
		v, good := integer(raw, hi)
		if !good {
			ok = false
			s.reject(name)
		}
		return v
	}
	d.Profile = get("dv_profile", "dv_profile", maxDVProfile)
	d.Level = get("dv_level", "dv_level", maxDVLevel)
	d.RPU = get("rpu_present_flag", "dv_rpu", 1) == 1
	d.EL = get("el_present_flag", "dv_el", 1) == 1
	d.BL = get("bl_present_flag", "dv_bl", 1) == 1
	d.BLSignalCompatibilityID = get("dv_bl_signal_compatibility_id", "dv_bl_signal_compatibility_id", maxDVCompatID)
	if !ok {
		return nil
	}
	return d
}

// ITU-T H.273 code points as ffprobe names them, for the numeric codes
// mkvmerge -J reports from the Matroska track header.
var (
	h273Primaries = map[int]string{
		1: "bt709", 4: "bt470m", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 8: "film",
		9: "bt2020", 10: "smpte428", 11: "smpte431", 12: "smpte432", 22: "jedec-p22",
	}
	h273Transfer = map[int]string{
		1: "bt709", 4: "bt470m", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 8: "linear",
		9: "log100", 10: "log316", 11: "iec61966-2-4", 12: "bt1361e", 13: "iec61966-2-1",
		14: "bt2020-10", 15: "bt2020-12", 16: "smpte2084", 17: "smpte428", 18: "arib-std-b67",
	}
	h273Matrix = map[int]string{
		0: "gbr", 1: "bt709", 4: "fcc", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 8: "ycgco",
		9: "bt2020nc", 10: "bt2020c", 11: "smpte2085", 12: "chroma-derived-nc", 13: "chroma-derived-c", 14: "ictcp",
	}
	mkvRange = map[int]string{1: "tv", 2: "pc"}
)

// colorFromMkv reads the colour track properties of one mkvmerge -J track.
// Both the British spelling of the JSON keys that mkvmerge documents and
// the American one it prints are accepted. Codes that H.273 leaves
// unspecified (2) or that are not in the tables are folded to the empty
// string, as are reserved code points, which is what ffprobe does with
// them.
func colorFromMkv(props map[string]json.RawMessage) Color {
	var s colorSetter
	get := func(keys ...string) (json.RawMessage, bool) {
		for _, k := range keys {
			if raw, ok := props[k]; ok && len(raw) > 0 && string(raw) != "null" {
				return raw, true
			}
		}
		return nil, false
	}
	code := func(name string, table map[int]string, keys ...string) string {
		raw, ok := get(keys...)
		if !ok {
			return ""
		}
		n, ok := integer(raw, 255)
		if !ok {
			s.reject(name)
			return ""
		}
		return table[n]
	}
	s.c.Primaries = code("primaries", h273Primaries, "colour_primaries", "color_primaries")
	s.c.Transfer = code("transfer", h273Transfer, "colour_transfer_characteristics", "color_transfer_characteristics")
	s.c.Matrix = code("matrix", h273Matrix, "colour_matrix_coefficients", "color_matrix_coefficients")
	s.c.Range = code("range", mkvRange, "colour_range", "color_range")

	m := &MasteringDisplay{}
	if raw, ok := get("chromaticity_coordinates"); ok {
		vals, good := floatList(raw, 6)
		if !good {
			s.reject("chromaticity_coordinates")
		} else {
			m.RedX, m.RedY, m.GreenX, m.GreenY, m.BlueX, m.BlueY = vals[0], vals[1], vals[2], vals[3], vals[4], vals[5]
			if raw, ok := get("white_colour_coordinates", "white_color_coordinates"); ok {
				wv, good := floatList(raw, 2)
				if !good {
					s.reject("white_coordinates")
				} else {
					m.WhiteX, m.WhiteY = wv[0], wv[1]
					m.HasPrimaries = true
				}
			} else {
				s.reject("white_coordinates")
			}
		}
	} else if _, ok := get("white_colour_coordinates", "white_color_coordinates"); ok {
		s.reject("chromaticity_coordinates")
	}
	minRaw, hasMin := get("min_luminance")
	maxRaw, hasMax := get("max_luminance")
	if hasMin || hasMax {
		minV, okMin := number(minRaw, 0, maxLuminanceBound)
		maxV, okMax := number(maxRaw, 0, maxLuminanceBound)
		if hasMin && hasMax && okMin && okMax {
			m.MinLuminance, m.MaxLuminance, m.HasLuminance = minV, maxV, true
		} else {
			s.reject("luminance")
		}
	}
	if m.HasPrimaries || m.HasLuminance {
		s.c.Mastering = m
	}
	cllRaw, hasCLL := get("max_content_light")
	fallRaw, hasFALL := get("max_frame_light")
	if hasCLL || hasFALL {
		cll, okCLL := integer(cllRaw, maxContentLight)
		fall, okFALL := integer(fallRaw, maxContentLight)
		if hasCLL && hasFALL && okCLL && okFALL {
			s.c.Light = &ContentLight{MaxCLL: cll, MaxFALL: fall}
		} else {
			if !hasCLL || !okCLL {
				s.reject("max_cll")
			}
			if !hasFALL || !okFALL {
				s.reject("max_fall")
			}
		}
	}
	return s.done()
}

// floatList parses mkvmerge's comma separated coordinate string into
// exactly n values in [0, 1].
func floatList(raw json.RawMessage, n int) ([]float64, bool) {
	var str string
	if err := json.Unmarshal(raw, &str); err != nil || len(str) > 256 {
		return nil, false
	}
	parts := strings.Split(str, ",")
	if len(parts) != n {
		return nil, false
	}
	out := make([]float64, n)
	for i, p := range parts {
		v, err := parseFinite(p)
		if err != nil || v < 0 || v > 1 {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// merge fills the fields of c that ffprobe left empty from the Matroska
// track header mkvmerge reported. ffprobe stays the primary source because
// it reads the same header and the codec's own signalling, and both the
// source and the output are probed the same way; mkvmerge adds what ffprobe
// omitted. A malformed field stays malformed whichever tool saw it.
func (c *Color) merge(mk Color) {
	if c.Primaries == "" {
		c.Primaries = mk.Primaries
	}
	if c.Transfer == "" {
		c.Transfer = mk.Transfer
	}
	if c.Matrix == "" {
		c.Matrix = mk.Matrix
	}
	if c.Range == "" {
		c.Range = mk.Range
	}
	if mk.Mastering != nil {
		if c.Mastering == nil {
			c.Mastering = &MasteringDisplay{}
		}
		if !c.Mastering.HasPrimaries && mk.Mastering.HasPrimaries {
			p := *mk.Mastering
			c.Mastering.HasPrimaries = true
			c.Mastering.RedX, c.Mastering.RedY = p.RedX, p.RedY
			c.Mastering.GreenX, c.Mastering.GreenY = p.GreenX, p.GreenY
			c.Mastering.BlueX, c.Mastering.BlueY = p.BlueX, p.BlueY
			c.Mastering.WhiteX, c.Mastering.WhiteY = p.WhiteX, p.WhiteY
		}
		if !c.Mastering.HasLuminance && mk.Mastering.HasLuminance {
			c.Mastering.HasLuminance = true
			c.Mastering.MinLuminance, c.Mastering.MaxLuminance = mk.Mastering.MinLuminance, mk.Mastering.MaxLuminance
		}
	}
	if c.Light == nil && mk.Light != nil {
		l := *mk.Light
		c.Light = &l
	}
	if len(mk.Malformed) > 0 {
		seen := map[string]bool{}
		for _, m := range c.Malformed {
			seen[m] = true
		}
		for _, m := range mk.Malformed {
			if !seen[m] {
				c.Malformed = append(c.Malformed, m)
				seen[m] = true
			}
		}
		sort.Strings(c.Malformed)
	}
}
