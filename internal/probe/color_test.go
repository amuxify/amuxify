package probe

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/testutil"
)

// hdr10Stream is what ffprobe prints for the hdr10.mkv fixture's video
// stream, trimmed to the fields the probe reads.
const hdr10Stream = `{"index":0,"codec_name":"h264","codec_type":"video","width":320,"height":240,
"pix_fmt":"yuv420p","color_range":"tv","color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020",
"side_data_list":[
 {"side_data_type":"Content light level metadata","max_content":1000,"max_average":400},
 {"side_data_type":"Mastering display metadata","red_x":"11878269/16777216","red_y":"4898947/16777216","green_x":"11408507/67108864",
  "green_y":"13371441/16777216","blue_x":"8791261/67108864","blue_y":"12348031/268435456","white_point_x":"10492471/33554432",
  "white_point_y":"689963/2097152","min_luminance":"209800/2098000053","max_luminance":"1000/1"}]}`

// doviStream is a Dolby Vision profile 8.1 stream as ffprobe prints it; the
// record cannot be built with the fixture tools, so it is written by hand.
const doviStream = `{"index":0,"codec_name":"hevc","codec_type":"video","color_range":"tv","color_space":"bt2020nc",
"color_transfer":"smpte2084","color_primaries":"bt2020",
"side_data_list":[
 {"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,"dv_profile":8,"dv_level":6,
  "rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":1},
 {"side_data_type":"Content light level metadata","max_content":1000,"max_average":400}]}`

func probeJSON(t *testing.T, stream string) *MediaInfo {
	t.Helper()
	var fp ffprobeOut
	if err := json.Unmarshal([]byte(`{"streams":[`+stream+`],"format":{"format_name":"matroska,webm"}}`), &fp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return fromFFprobe("x.mkv", &fp)
}

func TestColorFromFFprobeHDR10(t *testing.T) {
	m := probeJSON(t, hdr10Stream)
	s := m.Streams[0]
	if strings.Join(s.HDR, "+") != "hdr10" {
		t.Fatalf("labels %v", s.HDR)
	}
	c := s.Color
	if c.Primaries != "bt2020" || c.Transfer != "smpte2084" || c.Matrix != "bt2020nc" || c.Range != "tv" {
		t.Fatalf("description %+v", c)
	}
	if c.Light == nil || c.Light.MaxCLL != 1000 || c.Light.MaxFALL != 400 {
		t.Fatalf("content light %+v", c.Light)
	}
	md := c.Mastering
	if md == nil || !md.HasPrimaries || !md.HasLuminance {
		t.Fatalf("mastering %+v", md)
	}
	for name, got := range map[string][2]float64{
		"red_x": {md.RedX, 0.708}, "red_y": {md.RedY, 0.292}, "green_x": {md.GreenX, 0.170}, "green_y": {md.GreenY, 0.797},
		"blue_x": {md.BlueX, 0.131}, "blue_y": {md.BlueY, 0.046}, "white_x": {md.WhiteX, 0.3127}, "white_y": {md.WhiteY, 0.3290},
		"min_luminance": {md.MinLuminance, 0.0001}, "max_luminance": {md.MaxLuminance, 1000},
	} {
		if !closeEnough(got[0], got[1]) {
			t.Errorf("%s = %v want %v", name, got[0], got[1])
		}
	}
	if len(c.Malformed) != 0 {
		t.Fatalf("malformed %v", c.Malformed)
	}
	want := "primaries=bt2020 transfer=smpte2084 matrix=bt2020nc range=tv red_x=0.708 red_y=0.292 green_x=0.17 green_y=0.797 " +
		"blue_x=0.131 blue_y=0.046 white_x=0.3127 white_y=0.329 min_luminance=0.0001 max_luminance=1000 max_cll=1000 max_fall=400"
	if got := c.String(); got != want {
		t.Fatalf("String()\n got %s\nwant %s", got, want)
	}
	if c.DolbyVision != nil {
		t.Fatal("no Dolby Vision record was given")
	}
}

func TestColorFromFFprobeDolbyVision(t *testing.T) {
	s := probeJSON(t, doviStream).Streams[0]
	if strings.Join(s.HDR, "+") != "hdr10+dovi" {
		t.Fatalf("labels %v", s.HDR)
	}
	want := &DolbyVision{Profile: 8, Level: 6, RPU: true, EL: false, BL: true, BLSignalCompatibilityID: 1}
	if !reflect.DeepEqual(s.Color.DolbyVision, want) {
		t.Fatalf("dolby vision %+v want %+v", s.Color.DolbyVision, want)
	}
	if !strings.Contains(s.Color.String(), "dv_profile=8 dv_level=6 dv_rpu=1 dv_el=0 dv_bl=1 dv_bl_signal_compatibility_id=1") {
		t.Fatalf("String() %s", s.Color.String())
	}
}

func TestColorSDRIsEmpty(t *testing.T) {
	s := probeJSON(t, `{"index":0,"codec_type":"video","codec_name":"h264"}`).Streams[0]
	if len(s.HDR) != 0 || !s.Color.Empty() || s.Color.String() != "" {
		t.Fatalf("SDR stream carries %v %q", s.HDR, s.Color.String())
	}
	// The names ffprobe prints for unsignalled values fold to absent.
	s = probeJSON(t, `{"index":0,"codec_type":"video","color_primaries":"unknown","color_transfer":"unspecified","color_space":"reserved"}`).Streams[0]
	if !s.Color.Empty() {
		t.Fatalf("unspecified values kept: %q", s.Color.String())
	}
}

// Hostile ffprobe output: every case must parse without a panic, must not
// fail the probe, and must produce a Color whose text carries no byte from
// the input. A value that is rejected is named in Malformed so it is never
// confused with an absent one.
func TestColorHostileFFprobeJSON(t *testing.T) {
	side := func(entries string) string {
		return `{"index":0,"codec_type":"video","color_transfer":"smpte2084","side_data_list":` + entries + `}`
	}
	md := func(fields string) string {
		return side(`[{"side_data_type":"Mastering display metadata",` + fields + `}]`)
	}
	allCoords := `"red_x":"0.708","red_y":"0.292","green_x":"0.17","green_y":"0.797","blue_x":"0.131","blue_y":"0.046","white_point_x":"0.3127","white_point_y":"0.329"`
	cases := []struct {
		name      string
		stream    string
		malformed []string
		check     func(t *testing.T, s Stream)
	}{
		{"missing side data", `{"index":0,"codec_type":"video","color_transfer":"smpte2084"}`, nil, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Mastering != nil || c.Light != nil || c.DolbyVision != nil {
				t.Fatal("metadata from nowhere")
			}
		}},
		{"null side data", side(`null`), nil, nil},
		{"side data is an object", side(`{"side_data_type":"Mastering display metadata","max_luminance":"1000/1"}`), nil, nil},
		{"side data is a number", side(`42`), nil, nil},
		{"side data holds non objects", side(`[1,"x",null,[],{"side_data_type":"Content light level metadata","max_content":1,"max_average":1}]`), nil, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Light == nil || c.Light.MaxCLL != 1 {
				t.Fatalf("the well formed entry after the odd ones was lost: %+v", c.Light)
			}
		}},
		{"type is not a string", side(`[{"side_data_type":7,"max_content":1,"max_average":1}]`), nil, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Light != nil {
				t.Fatal("entry without a usable type was used")
			}
		}},
		{"type with control characters", side(`[{"side_data_type":"\u0000Mastering display\u001b[31m metadata\u202e","max_luminance":"1000/1","min_luminance":"0/1"}]`), nil, func(t *testing.T, s Stream) {
			// The type string is only matched, never stored.
			c := s.Color
			if c.Mastering == nil || !c.Mastering.HasLuminance {
				t.Fatalf("luminance not read: %+v", c.Mastering)
			}
		}},
		{"duplicated entries keep the first", side(`[{"side_data_type":"Content light level metadata","max_content":1000,"max_average":400},{"side_data_type":"Content light level metadata","max_content":1,"max_average":1}]`), nil, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Light == nil || c.Light.MaxCLL != 1000 || c.Light.MaxFALL != 400 {
				t.Fatalf("second entry overrode the first: %+v", c.Light)
			}
		}},
		{"huge luminance number", md(`"min_luminance":"0/1","max_luminance":1e300`), []string{"luminance", "max_luminance"}, nil},
		{"luminance beyond float64", md(`"min_luminance":"0/1","max_luminance":1e999`), []string{"luminance", "max_luminance"}, nil},
		{"rational beyond float64", md(`"min_luminance":"0/1","max_luminance":"1e999/1"`), []string{"luminance", "max_luminance"}, nil},
		{"NaN string", md(`"min_luminance":"NaN","max_luminance":"1000/1"`), []string{"luminance", "min_luminance"}, nil},
		{"Inf string", md(`"min_luminance":"0/1","max_luminance":"+Inf"`), []string{"luminance", "max_luminance"}, nil},
		{"infinity spelled out", md(`"min_luminance":"0/1","max_luminance":"infinity"`), []string{"luminance", "max_luminance"}, nil},
		{"hexadecimal float", md(`"min_luminance":"0/1","max_luminance":"0x1p10"`), []string{"luminance", "max_luminance"}, nil},
		{"zero denominator", md(`"min_luminance":"1/0","max_luminance":"1000/1"`), []string{"luminance", "min_luminance"}, nil},
		{"negative luminance", md(`"min_luminance":"-1/1","max_luminance":"1000/1"`), []string{"luminance", "min_luminance"}, nil},
		{"only one luminance", md(`"max_luminance":"1000/1"`), []string{"luminance"}, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Mastering != nil {
				t.Fatalf("half a luminance range was kept: %+v", c.Mastering)
			}
		}},
		{"chromaticity above one", md(`"red_x":"1.5",` + strings.TrimPrefix(allCoords, `"red_x":"0.708",`)), []string{"red_x"}, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Mastering != nil {
				t.Fatalf("an invalid set of primaries was kept: %+v", c.Mastering)
			}
		}},
		{"negative chromaticity", md(`"red_x":"-0.1",` + strings.TrimPrefix(allCoords, `"red_x":"0.708",`)), []string{"red_x"}, nil},
		{"partial chromaticity", md(`"red_x":"0.708","red_y":"0.292"`), []string{"chromaticity_coordinates"}, nil},
		{"chromaticity of the wrong type", md(`"red_x":{"a":1},` + strings.TrimPrefix(allCoords, `"red_x":"0.708",`)), []string{"red_x"}, nil},
		{"chromaticity as an array", md(`"red_x":[0.7],` + strings.TrimPrefix(allCoords, `"red_x":"0.708",`)), []string{"red_x"}, nil},
		{"chromaticity as a boolean", md(`"red_x":true,` + strings.TrimPrefix(allCoords, `"red_x":"0.708",`)), []string{"red_x"}, nil},
		{"overlong rational", md(`"min_luminance":"` + strings.Repeat("1", 70) + `/1","max_luminance":"1000/1"`), []string{"luminance", "min_luminance"}, nil},
		{"rational with junk", md(`"min_luminance":"1/1; rm -rf /","max_luminance":"1000/1"`), []string{"luminance", "min_luminance"}, nil},
		{"content light as strings", side(`[{"side_data_type":"Content light level metadata","max_content":"1000","max_average":"400"}]`), nil, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Light == nil || c.Light.MaxCLL != 1000 {
				t.Fatalf("numeric strings are what ffprobe prints for some fields: %+v", c.Light)
			}
		}},
		{"content light not integral", side(`[{"side_data_type":"Content light level metadata","max_content":1000.5,"max_average":400}]`), []string{"max_cll"}, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Light != nil {
				t.Fatalf("a fractional level was kept: %+v", c.Light)
			}
		}},
		{"content light above 16 bits", side(`[{"side_data_type":"Content light level metadata","max_content":65536,"max_average":400}]`), []string{"max_cll"}, nil},
		{"content light negative", side(`[{"side_data_type":"Content light level metadata","max_content":1000,"max_average":-1}]`), []string{"max_fall"}, nil},
		{"content light missing field", side(`[{"side_data_type":"Content light level metadata","max_content":1000}]`), nil, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Light != nil {
				t.Fatalf("half a content light level was kept: %+v", c.Light)
			}
		}},
		{"dolby vision profile out of range", side(`[{"side_data_type":"DOVI configuration record","dv_profile":128,"dv_level":6,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":1}]`), []string{"dv_profile"}, func(t *testing.T, s Stream) {
			c := s.Color
			if c.DolbyVision != nil {
				t.Fatalf("an invalid record was kept: %+v", c.DolbyVision)
			}
			if strings.Join(s.HDR, "+") != "hdr10+dovi" {
				t.Fatalf("labels %v", s.HDR)
			}
		}},
		{"dolby vision flag not boolean", side(`[{"side_data_type":"DOVI configuration record","dv_profile":8,"dv_level":6,"rpu_present_flag":2,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":1}]`), []string{"dv_rpu"}, nil},
		{"dolby vision missing field", side(`[{"side_data_type":"DOVI configuration record","dv_profile":8}]`), []string{"dv_bl", "dv_bl_signal_compatibility_id", "dv_el", "dv_level", "dv_rpu"}, nil},
		{"dolby vision wrong types", side(`[{"side_data_type":"DOVI configuration record","dv_profile":"eight","dv_level":null,"rpu_present_flag":true,"el_present_flag":[],"bl_present_flag":{},"dv_bl_signal_compatibility_id":"1/0"}]`), []string{"dv_bl", "dv_bl_signal_compatibility_id", "dv_el", "dv_level", "dv_profile", "dv_rpu"}, nil},
		{"description with control characters", `{"index":0,"codec_type":"video","color_primaries":"bt2020\u001b[31m","color_transfer":"smpte2084\n","color_space":"\u202ebt2020nc","color_range":"tv;reboot"}`, []string{"matrix", "primaries", "range", "transfer"}, func(t *testing.T, s Stream) {
			c := s.Color
			if c.Primaries != "" || c.Transfer != "" || c.Matrix != "" || c.Range != "" {
				t.Fatalf("hostile names kept: %+v", c)
			}
		}},
		{"overlong description", `{"index":0,"codec_type":"video","color_primaries":"` + strings.Repeat("a", 40) + `"}`, []string{"primaries"}, nil},
		{"deeply nested side data", side(strings.Repeat("[", 200) + strings.Repeat("]", 200)), nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := probeJSON(t, tc.stream)
			if len(m.Streams) != 1 {
				t.Fatalf("%d streams", len(m.Streams))
			}
			c := m.Streams[0].Color
			if !reflect.DeepEqual(c.Malformed, tc.malformed) {
				t.Fatalf("malformed %v want %v", c.Malformed, tc.malformed)
			}
			for _, r := range c.String() {
				if unicode.IsControl(r) || !unicode.IsPrint(r) || r == '[' || r == ';' {
					t.Fatalf("String() carries %q: %q", r, c.String())
				}
			}
			if tc.check != nil {
				tc.check(t, m.Streams[0])
			}
		})
	}
}

func TestHDRLabelsDeduplicated(t *testing.T) {
	side := json.RawMessage(`[{"side_data_type":"DOVI configuration record"},{"side_data_type":"DOVI configuration record"},{"side_data_type":"HDR Dynamic Metadata SMPTE2094-40 (HDR10+)"},{"side_data_type":"Dolby Vision RPU Data"}]`)
	if got := strings.Join(hdrLabels("smpte2084", side), "+"); got != "hdr10+dovi+hdr10plus" {
		t.Fatalf("labels %s", got)
	}
	if got := hdrLabels("bt709", nil); len(got) != 0 {
		t.Fatalf("SDR labels %v", got)
	}
}

func mkvProps(t *testing.T, props string) Color {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(props), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return colorFromMkv(raw)
}

// mkvmerge -J prints the American spelling; the documentation lists the
// British one. Both are read and give the same normalised values ffprobe
// gives, so a track probed through either tool compares equal.
func TestColorFromMkvmerge(t *testing.T) {
	american := `{"chromaticity_coordinates":"0.708,0.292,0.17,0.797,0.131,0.046","color_matrix_coefficients":9,"color_primaries":9,
"color_range":1,"color_transfer_characteristics":16,"max_content_light":1000,"max_frame_light":400,"max_luminance":1000.0,
"min_luminance":9.999999747378752e-05,"white_color_coordinates":"0.3127,0.329","codec_id":"V_MPEGH/ISO/HEVC"}`
	british := strings.NewReplacer("color_", "colour_").Replace(american)
	ff := probeJSON(t, hdr10Stream).Streams[0].Color
	for name, props := range map[string]string{"american": american, "british": british} {
		c := mkvProps(t, props)
		if diffs := ColorDiff(&ff, &c); len(diffs) != 0 {
			t.Errorf("%s spelling differs from ffprobe's view: %v", name, diffs)
		}
		if len(c.Malformed) != 0 {
			t.Errorf("%s: malformed %v", name, c.Malformed)
		}
	}
	c := mkvProps(t, `{"color_transfer_characteristics":18,"color_primaries":9,"color_matrix_coefficients":9,"color_range":2}`)
	if c.Transfer != "arib-std-b67" || c.Primaries != "bt2020" || c.Matrix != "bt2020nc" || c.Range != "pc" || c.Mastering != nil || c.Light != nil {
		t.Fatalf("hlg %+v", c)
	}
	// Unspecified and reserved code points fold to absent, like ffprobe's names.
	c = mkvProps(t, `{"color_transfer_characteristics":2,"color_primaries":2,"color_matrix_coefficients":2,"color_range":0}`)
	if !c.Empty() {
		t.Fatalf("unspecified codes kept: %q", c.String())
	}
}

func TestColorHostileMkvmergeJSON(t *testing.T) {
	cases := []struct {
		name      string
		props     string
		malformed []string
		empty     bool
	}{
		{"no colour at all", `{"language":"eng"}`, nil, true},
		{"code as string", `{"color_primaries":"9"}`, nil, false},
		{"code as word", `{"color_primaries":"bt2020"}`, []string{"primaries"}, false},
		{"code negative", `{"color_transfer_characteristics":-16}`, []string{"transfer"}, false},
		{"code huge", `{"color_transfer_characteristics":1e300}`, []string{"transfer"}, false},
		{"code fractional", `{"color_matrix_coefficients":9.5}`, []string{"matrix"}, false},
		{"code unknown", `{"color_primaries":200}`, nil, true},
		{"five coordinates", `{"chromaticity_coordinates":"0.708,0.292,0.17,0.797,0.131","white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"seven coordinates", `{"chromaticity_coordinates":"0.708,0.292,0.17,0.797,0.131,0.046,0.5","white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"coordinate NaN", `{"chromaticity_coordinates":"NaN,0.292,0.17,0.797,0.131,0.046","white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"coordinate above one", `{"chromaticity_coordinates":"1.708,0.292,0.17,0.797,0.131,0.046","white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"coordinates with control characters", `{"chromaticity_coordinates":"0.708,0.292,0.17,0.797,0.131,0.046\u0000","white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"coordinates as array", `{"chromaticity_coordinates":[0.708,0.292,0.17,0.797,0.131,0.046],"white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"coordinates overlong", `{"chromaticity_coordinates":"` + strings.Repeat("0.1,", 100) + `0.1","white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"white point missing", `{"chromaticity_coordinates":"0.708,0.292,0.17,0.797,0.131,0.046"}`, []string{"white_coordinates"}, false},
		{"white point alone", `{"white_color_coordinates":"0.3127,0.329"}`, []string{"chromaticity_coordinates"}, false},
		{"white point one value", `{"chromaticity_coordinates":"0.708,0.292,0.17,0.797,0.131,0.046","white_color_coordinates":"0.3127"}`, []string{"white_coordinates"}, false},
		{"luminance negative", `{"max_luminance":1000.0,"min_luminance":-1}`, []string{"luminance"}, false},
		{"luminance absurd", `{"max_luminance":1e20,"min_luminance":0}`, []string{"luminance"}, false},
		{"luminance beyond float64", `{"max_luminance":1e999,"min_luminance":0}`, []string{"luminance"}, false},
		{"luminance string", `{"max_luminance":"bright","min_luminance":0}`, []string{"luminance"}, false},
		{"luminance half", `{"max_luminance":1000}`, []string{"luminance"}, false},
		{"content light fractional", `{"max_content_light":1000.5,"max_frame_light":400}`, []string{"max_cll"}, false},
		{"content light too large", `{"max_content_light":1000,"max_frame_light":70000}`, []string{"max_fall"}, false},
		{"content light half", `{"max_content_light":1000}`, []string{"max_fall"}, false},
		{"content light null", `{"max_content_light":null,"max_frame_light":400}`, []string{"max_cll"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mkvProps(t, tc.props)
			if !reflect.DeepEqual(c.Malformed, tc.malformed) {
				t.Fatalf("malformed %v want %v (%q)", c.Malformed, tc.malformed, c.String())
			}
			if c.Empty() != tc.empty {
				t.Fatalf("empty %v want %v (%q)", c.Empty(), tc.empty, c.String())
			}
			for _, r := range c.String() {
				if unicode.IsControl(r) || !unicode.IsPrint(r) {
					t.Fatalf("String() carries %q", r)
				}
			}
		})
	}
}

// A wrong type in a colour property must not fail the whole mkvmerge
// identification, which the typed properties would.
func TestMkvTrackPropsKeepRawOnOddColourTypes(t *testing.T) {
	var mk mkvOut
	err := json.Unmarshal([]byte(`{"container":{"recognized":true,"supported":true,"type":"Matroska"},
"tracks":[{"id":0,"type":"video","codec":"HEVC","properties":{"language":"eng","default_track":true,"color_primaries":"nine","max_luminance":{"x":1}}}]}`), &mk)
	if err != nil {
		t.Fatalf("identification failed on an odd colour type: %v", err)
	}
	p := mk.Tracks[0].Properties
	if p.Language != "eng" || !p.Default {
		t.Fatalf("typed fields lost: %+v", p.mkvTrackFields)
	}
	c := colorFromMkv(p.raw)
	if !reflect.DeepEqual(c.Malformed, []string{"luminance", "primaries"}) {
		t.Fatalf("malformed %v", c.Malformed)
	}
}

// ffprobe's view wins; mkvmerge fills what it left out, and a malformed
// field seen by either tool stays malformed.
func TestColorMergeFillsOnlyMissing(t *testing.T) {
	c := Color{Primaries: "bt709", Mastering: &MasteringDisplay{HasLuminance: true, MinLuminance: 0.01, MaxLuminance: 500}}
	mk := Color{Primaries: "bt2020", Transfer: "smpte2084", Malformed: []string{"max_cll"},
		Mastering: &MasteringDisplay{HasPrimaries: true, RedX: 0.708, RedY: 0.292, GreenX: 0.17, GreenY: 0.797, BlueX: 0.131, BlueY: 0.046, WhiteX: 0.3127, WhiteY: 0.329, HasLuminance: true, MinLuminance: 0, MaxLuminance: 1000},
		Light:     &ContentLight{MaxCLL: 1, MaxFALL: 1}}
	c.merge(mk)
	if c.Primaries != "bt709" || c.Transfer != "smpte2084" {
		t.Fatalf("description %+v", c)
	}
	if !c.Mastering.HasPrimaries || c.Mastering.RedX != 0.708 || c.Mastering.MaxLuminance != 500 || c.Mastering.MinLuminance != 0.01 {
		t.Fatalf("mastering %+v", c.Mastering)
	}
	if c.Light == nil || c.Light.MaxCLL != 1 || !reflect.DeepEqual(c.Malformed, []string{"max_cll"}) {
		t.Fatalf("light %+v malformed %v", c.Light, c.Malformed)
	}
	c.merge(mk)
	if !reflect.DeepEqual(c.Malformed, []string{"max_cll"}) {
		t.Fatalf("malformed duplicated: %v", c.Malformed)
	}
}

func TestColorDiff(t *testing.T) {
	hdr := probeJSON(t, hdr10Stream).Streams[0].Color
	dovi := probeJSON(t, doviStream).Streams[0].Color
	clone := func(c Color) Color {
		b, _ := json.Marshal(c)
		var out Color
		_ = json.Unmarshal(b, &out)
		return out
	}
	cases := []struct {
		name string
		src  Color
		out  func() Color
		want []string
	}{
		{"identical", hdr, func() Color { return clone(hdr) }, nil},
		{"both empty", Color{}, func() Color { return Color{} }, nil},
		{"rational rounding is tolerated", hdr, func() Color {
			c := clone(hdr)
			c.Mastering.RedX = 0.708
			c.Mastering.MinLuminance = 0.0001
			return c
		}, nil},
		{"transfer lost", hdr, func() Color { c := clone(hdr); c.Transfer = ""; return c }, []string{"transfer lost (was smpte2084)"}},
		{"primaries changed", hdr, func() Color { c := clone(hdr); c.Primaries = "bt709"; return c }, []string{"primaries changed from bt2020 to bt709"}},
		{"matrix and range changed", hdr, func() Color { c := clone(hdr); c.Matrix = "bt709"; c.Range = "pc"; return c },
			[]string{"matrix changed from bt2020nc to bt709", "range changed from tv to pc"}},
		{"mastering display lost", hdr, func() Color { c := clone(hdr); c.Mastering = nil; return c }, []string{"mastering display metadata lost"}},
		{"content light lost", hdr, func() Color { c := clone(hdr); c.Light = nil; return c }, []string{"content light level lost"}},
		{"max luminance changed", hdr, func() Color { c := clone(hdr); c.Mastering.MaxLuminance = 4000; return c }, []string{"max_luminance changed from 1000 to 4000"}},
		{"min luminance doubled", hdr, func() Color { c := clone(hdr); c.Mastering.MinLuminance = 0.0002; return c }, []string{"min_luminance changed from 0.0001 to 0.0002"}},
		{"chromaticity changed", hdr, func() Color { c := clone(hdr); c.Mastering.WhiteY = 0.3; return c }, []string{"white_y changed from 0.329 to 0.3"}},
		{"luminance half lost", hdr, func() Color { c := clone(hdr); c.Mastering.HasLuminance = false; return c },
			[]string{"min_luminance lost (was 0.0001)", "max_luminance lost (was 1000)"}},
		{"max cll changed", hdr, func() Color { c := clone(hdr); c.Light.MaxCLL = 999; return c }, []string{"max_cll changed from 1000 to 999"}},
		{"sdr output gained hdr", Color{}, func() Color { return clone(hdr) }, []string{
			"mastering display metadata gained", "content light level gained",
			"primaries gained (now bt2020)", "transfer gained (now smpte2084)", "matrix gained (now bt2020nc)", "range gained (now tv)"}},
		{"dolby vision lost", dovi, func() Color { c := clone(dovi); c.DolbyVision = nil; return c }, []string{"dolby vision configuration lost"}},
		{"dolby vision profile changed", dovi, func() Color { c := clone(dovi); c.DolbyVision.Profile = 5; return c }, []string{"dv_profile changed from 8 to 5"}},
		{"dolby vision level changed", dovi, func() Color { c := clone(dovi); c.DolbyVision.Level = 9; return c }, []string{"dv_level changed from 6 to 9"}},
		{"dolby vision flags changed", dovi, func() Color { c := clone(dovi); c.DolbyVision.RPU = false; c.DolbyVision.EL = true; return c },
			[]string{"dv_rpu changed from 1 to 0", "dv_el changed from 0 to 1"}},
		{"dolby vision compatibility id changed", dovi, func() Color { c := clone(dovi); c.DolbyVision.BLSignalCompatibilityID = 4; return c },
			[]string{"dv_bl_signal_compatibility_id changed from 1 to 4"}},
		{"malformed source, absent output", Color{Malformed: []string{"max_luminance"}}, func() Color { return Color{} }, []string{"max_luminance lost (was malformed)"}},
		{"malformed on both sides agrees", Color{Malformed: []string{"max_luminance"}}, func() Color { return Color{Malformed: []string{"max_luminance"}} }, nil},
		{"nil pointers", Color{}, func() Color { return Color{} }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.out()
			got := ColorDiff(&tc.src, &out)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diff\n got %q\nwant %q", got, tc.want)
			}
		})
	}
	if d := ColorDiff(nil, nil); len(d) != 0 {
		t.Fatalf("nil diff %v", d)
	}
	if d := ColorDiff(nil, &hdr); len(d) == 0 {
		t.Fatal("nil source against hdr output reported nothing")
	}
}

func TestNumberRejectsNonFinite(t *testing.T) {
	for _, s := range []string{`"NaN"`, `"nan"`, `"Inf"`, `"-Inf"`, `"infinity"`, `"1/0"`, `"0/0"`, `"1e999"`, `1e999`, `-1e999`, `"0x10"`, `"1_000"`, `""`, `"/"`, `"1/"`, `"/1"`, `"1/2/3"`, `true`, `null`, `[]`, `{}`} {
		if v, ok := number(json.RawMessage(s), -math.MaxFloat64, math.MaxFloat64); ok {
			t.Errorf("number(%s) accepted %v", s, v)
		}
	}
	for _, s := range []string{`1`, `"1"`, `"1/1"`, `"209800/2098000053"`, `1.5e2`, `"-0"`, `"1e-5"`} {
		if _, ok := number(json.RawMessage(s), -1, 1e6); !ok {
			t.Errorf("number(%s) rejected", s)
		}
	}
}

// The fixtures built with mkvmerge's colour options come back through
// ffprobe and mkvmerge -J with the values the script set.
func TestProbeHDRFixtures(t *testing.T) {
	r := testutil.Need(t, exec.FFprobe, exec.MKVMerge)
	root := testutil.Fixtures(t)
	p := &Prober{Runner: r, Timeout: time.Minute}
	m, err := p.Probe(context.Background(), root+"/hdr10.mkv")
	if err != nil {
		t.Fatal(err)
	}
	v := m.StreamsOf("video")
	if len(v) != 1 || strings.Join(v[0].HDR, "+") != "hdr10" {
		t.Fatalf("hdr10.mkv video %+v", v)
	}
	want := probeJSON(t, hdr10Stream).Streams[0].Color
	if diffs := ColorDiff(&want, &v[0].Color); len(diffs) != 0 {
		t.Fatalf("hdr10.mkv: %v\n%s", diffs, v[0].Color.String())
	}
	m, err = p.Probe(context.Background(), root+"/hlg.mkv")
	if err != nil {
		t.Fatal(err)
	}
	v = m.StreamsOf("video")
	if len(v) != 1 || strings.Join(v[0].HDR, "+") != "hlg" {
		t.Fatalf("hlg.mkv video %+v", v)
	}
	c := v[0].Color
	if c.Transfer != "arib-std-b67" || c.Primaries != "bt2020" || c.Matrix != "bt2020nc" || c.Range != "tv" || c.Mastering != nil || c.Light != nil || len(c.Malformed) != 0 {
		t.Fatalf("hlg.mkv: %s", c.String())
	}
	m, err = p.Probe(context.Background(), root+"/clean.mkv")
	if err != nil {
		t.Fatal(err)
	}
	if v = m.StreamsOf("video"); len(v[0].HDR) != 0 || !v[0].Color.Empty() {
		t.Fatalf("clean.mkv reports colour: %v %s", v[0].HDR, v[0].Color.String())
	}
}
