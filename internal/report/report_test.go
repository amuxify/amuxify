package report

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite golden files")

// golden compares got with the file under testdata, rewriting it when the
// -update flag is set.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/report -run %s -update)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func marshalJSON(t *testing.T, s *Summary) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := s.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGolden(t *testing.T) {
	got := marshalJSON(t, Example())
	golden(t, "report-golden.json", got)
	if !json.Valid(got) {
		t.Error("golden output is not valid JSON")
	}
	if !utf8.Valid(got) {
		t.Error("golden output is not valid UTF-8")
	}
	if !bytes.HasSuffix(got, []byte("}\n")) || bytes.Count(got, []byte("\n{")) != 0 {
		t.Error("WriteJSON must emit exactly one object followed by a newline")
	}
}

// TestDocsExampleIsGolden keeps the example in docs/report.md byte for byte
// equal to the golden file, so readers of the docs see the real wire format.
func TestDocsExampleIsGolden(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "report-golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc, append([]byte("```json\n"), want...)) {
		t.Error("docs/report.md does not contain testdata/report-golden.json verbatim inside a json code block")
	}
}

type field struct {
	JSON      string
	Type      string
	OmitEmpty bool
}

func fieldsOf(v interface{}) []field {
	rt := reflect.TypeOf(v)
	var out []field
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out = append(out, field{name, f.Type.String(), strings.Contains(","+opts+",", ",omitempty,")})
	}
	return out
}

// TestFieldSetFrozen pins the JSON name, Go type and omitempty flag of every
// field in declaration order. A rename, removal or reorder fails; an
// addition fails until the list below is edited, which is the deliberate
// friction that keeps docs/report.md and the schema promise in step.
func TestFieldSetFrozen(t *testing.T) {
	cases := []struct {
		name string
		v    interface{}
		want []field
	}{
		{"Summary", Summary{}, []field{
			{"schema", "string", false},
			{"tool", "string", false},
			{"version", "string", false},
			{"command", "string", false},
			{"profile", "string", false},
			{"hook", "*report.HookInfo", true},
			{"started", "time.Time", false},
			{"finished", "time.Time", false},
			{"verdict", "report.Severity", false},
			{"counts", "map[string]int", false},
			{"files", "[]report.FileResult", false},
			{"errors", "[]string", false},
		}},
		{"HookInfo", HookInfo{}, []field{
			{"adapter", "string", false},
			{"event", "string", false},
			{"label", "string", true},
			{"category", "string", true},
			{"fail_on", "string", false},
			{"exit_code", "int", false},
		}},
		{"FileResult", FileResult{}, []field{
			{"path", "string", false},
			{"verdict", "report.Severity", false},
			{"findings", "[]report.Finding", false},
			{"output", "string", true},
			{"actions", "[]string", true},
			{"info", "map[string]string", true},
			{"-", "time.Duration", false},
			{"duration_ms", "int64", false},
		}},
		{"Finding", Finding{}, []field{
			{"code", "string", false},
			{"severity", "report.Severity", false},
			{"message", "string", false},
			{"detail", "string", true},
		}},
	}
	for _, tc := range cases {
		if got := fieldsOf(tc.v); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s fields changed\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

func TestSchemaID(t *testing.T) {
	if SchemaID != "amuxify.report/1" {
		t.Fatalf("SchemaID = %q", SchemaID)
	}
	s := NewSummary("amuxify", "0.3.0", "scan", "homelab")
	if s.Schema != "amuxify.report/1" {
		t.Errorf("Schema = %q", s.Schema)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(marshalJSON(t, s), &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc["schema"]) != `"amuxify.report/1"` {
		t.Errorf("schema on the wire = %s", doc["schema"])
	}
}

// walkNulls collects the JSON paths of every null in a decoded document.
func walkNulls(path string, v interface{}, out *[]string) {
	switch x := v.(type) {
	case nil:
		*out = append(*out, path)
	case map[string]interface{}:
		for k, vv := range x {
			walkNulls(path+"."+k, vv, out)
		}
	case []interface{}:
		for i, vv := range x {
			walkNulls(path+"["+strconv.Itoa(i)+"]", vv, out)
		}
	}
}

func assertNoNulls(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var doc map[string]interface{}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b)
	}
	var nulls []string
	walkNulls("$", doc, &nulls)
	if len(nulls) != 0 {
		t.Errorf("null values at %v\n%s", nulls, b)
	}
	return doc
}

func TestNoNulls(t *testing.T) {
	s := NewSummary("amuxify", "0.3.0", "scan", "")
	b := marshalJSON(t, s)
	doc := assertNoNulls(t, b)
	for _, want := range []string{`"files": []`, `"errors": []`, `"counts": {}`, `"profile": ""`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
	if _, ok := doc["hook"]; ok {
		t.Error("hook must be absent when no adapter started the run")
	}

	s.Append(FileResult{Path: "/x/a.mkv"})
	b = marshalJSON(t, s)
	assertNoNulls(t, b)
	if !bytes.Contains(b, []byte(`"findings": []`)) {
		t.Errorf("a file without findings must marshal an empty findings array\n%s", b)
	}
	for _, absent := range []string{`"output"`, `"actions"`, `"info"`, `"detail"`} {
		if bytes.Contains(b, []byte(absent)) {
			t.Errorf("%s must be omitted when empty\n%s", absent, b)
		}
	}
}

// TestZeroValueSummaryHasNoNulls covers a Summary assembled by hand with a
// nil Counts map, nil Files, nil Errors and a file whose Findings are nil.
// json.Marshal and WriteJSON must both honour the no-null promise.
func TestZeroValueSummaryHasNoNulls(t *testing.T) {
	s := Summary{Files: []FileResult{{Path: "/x/a.mkv"}}}
	if s.Counts != nil || s.Errors != nil || s.Files[0].Findings != nil {
		t.Fatal("test setup: collections must be nil")
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	assertNoNulls(t, b)
	for _, want := range []string{`"counts":{}`, `"errors":[]`, `"findings":[]`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	if s.Counts != nil || s.Errors != nil || s.Files[0].Findings != nil {
		t.Error("marshalling must not modify the caller's Summary")
	}
	var empty Summary
	assertNoNulls(t, marshalJSON(t, &empty))
	if !bytes.Contains(marshalJSON(t, &empty), []byte(`"files": []`)) {
		t.Error("nil Files must marshal as an empty array")
	}
	// Append on a hand-made Summary must not panic on the nil map.
	empty.Append(FileResult{Path: "/x/b.mkv", Verdict: Warn})
	if empty.Counts["WARN"] != 1 || empty.Verdict != Warn {
		t.Errorf("counts %v verdict %v", empty.Counts, empty.Verdict)
	}
}

var rfc3339Seconds = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`)

func TestTimestampsUTCSeconds(t *testing.T) {
	s := NewSummary("amuxify", "0.3.0", "scan", "homelab")
	s.Close()
	var doc struct {
		Started  string `json:"started"`
		Finished string `json:"finished"`
	}
	if err := json.Unmarshal(marshalJSON(t, s), &doc); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"started": doc.Started, "finished": doc.Finished} {
		if !rfc3339Seconds.MatchString(v) {
			t.Errorf("%s = %q is not RFC 3339 UTC with whole seconds", name, v)
		}
	}
	if s.Finished.Before(s.Started) {
		t.Errorf("finished %v before started %v", s.Finished, s.Started)
	}
	if s.Started.Location() != time.UTC || s.Finished.Location() != time.UTC {
		t.Error("timestamps must be UTC")
	}
	if s.Started.Nanosecond() != 0 || s.Finished.Nanosecond() != 0 {
		t.Error("timestamps must be truncated to whole seconds")
	}
	// The example is fixed in time and must render the documented values.
	ex := marshalJSON(t, Example())
	for _, want := range []string{`"started": "2026-09-22T10:00:00Z"`, `"finished": "2026-09-22T10:00:04Z"`} {
		if !bytes.Contains(ex, []byte(want)) {
			t.Errorf("example lacks %s", want)
		}
	}
}

func TestSeverityTokens(t *testing.T) {
	tokens := map[Severity]string{Pass: "PASS", Warn: "WARN", Usage: "USAGE", Fail: "FAIL", Block: "BLOCK"}
	for sev, tok := range tokens {
		if sev.String() != tok {
			t.Errorf("%d.String() = %q, want %q", int(sev), sev.String(), tok)
		}
		b, err := json.Marshal(sev)
		if err != nil || string(b) != `"`+tok+`"` {
			t.Errorf("marshal %s: %s %v", tok, b, err)
		}
		var back Severity
		if err := json.Unmarshal(b, &back); err != nil || back != sev {
			t.Errorf("round trip %s: %v %v", tok, back, err)
		}
		var lower Severity
		if err := lower.UnmarshalText([]byte(strings.ToLower(tok))); err != nil || lower != sev {
			t.Errorf("case-insensitive parse of %q: %v %v", strings.ToLower(tok), lower, err)
		}
		if ExitCode(sev) != int(sev) {
			t.Errorf("ExitCode(%s) = %d", tok, ExitCode(sev))
		}
	}
	for _, code := range []struct {
		sev  Severity
		want int
	}{{Pass, 0}, {Warn, 1}, {Usage, 2}, {Fail, 3}, {Block, 4}} {
		if ExitCode(code.sev) != code.want {
			t.Errorf("ExitCode(%s) = %d, want %d", code.sev, ExitCode(code.sev), code.want)
		}
	}
	// Hostile or malformed tokens are rejected rather than defaulting to PASS.
	for _, bad := range []string{"", "ok", "PASSED", "PASS ", " PASS", "PASS\x00", "P\u200bASS", "\u202ePASS", "BLOCK\n", strings.Repeat("A", 1<<16), "0", "4"} {
		var s Severity = Block
		if err := s.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("token %q was accepted as %v", bad, s)
		} else if s != Block {
			t.Errorf("a failed parse of %q changed the value to %v", bad, s)
		}
	}
	var fromJSON struct {
		Verdict Severity `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(`{"verdict":"root"}`), &fromJSON); err == nil {
		t.Error("an unknown verdict token in JSON must be an error")
	}
	if got := Severity(7).String(); got != "SEVERITY(7)" {
		t.Errorf("unknown severity renders as %q", got)
	}
	if Worst(Pass, Warn) != Warn || Worst(Block, Fail) != Block || Worst(Usage, Usage) != Usage || Worst(Fail, Warn) != Fail {
		t.Error("Worst is not the maximum")
	}
	// The run verdict never goes down: a PASS file after a BLOCK file keeps BLOCK.
	s := NewSummary("amuxify", "0.3.0", "scan", "")
	s.Append(FileResult{Path: "a", Verdict: Block})
	s.Append(FileResult{Path: "b", Verdict: Pass})
	if s.Verdict != Block || s.Counts["BLOCK"] != 1 || s.Counts["PASS"] != 1 {
		t.Errorf("verdict %v counts %v", s.Verdict, s.Counts)
	}
	s.Error("late")
	if s.Verdict != Block {
		t.Error("an error must not lower BLOCK to FAIL")
	}
}

func TestHumanFormats(t *testing.T) {
	ex := Example()
	var verbose, plain, tail bytes.Buffer
	ex.WriteHuman(&verbose, true)
	ex.WriteHuman(&plain, false)
	ex.WriteHumanTail(&tail)
	golden(t, "human-verbose.txt", verbose.Bytes())
	golden(t, "human.txt", plain.Bytes())
	golden(t, "tail.txt", tail.Bytes())
	if !bytes.HasSuffix(plain.Bytes(), tail.Bytes()) {
		t.Error("WriteHuman must end with exactly what WriteHumanTail prints")
	}
	// The frozen line shapes.
	for _, want := range []string{
		"WARN  /srv/incoming/movie.mkv\n",
		"      WARN  LINK_IN_TAG        1 link(s) in metadata: tag COMMENT: http://x.example\n",
		"      -> /srv/incoming__remuxed/movie.mkv\n",
		"BLOCK /srv/incoming/readme.exe\n",
		"ERROR /srv/incoming/gone.mkv: lstat /srv/incoming/gone.mkv: no such file or directory\n",
		"\nBLOCK: 3 file(s) BLOCK=1 PASS=1 WARN=1\n",
	} {
		if !strings.Contains(plain.String(), want) {
			t.Errorf("non-verbose output lacks %q:\n%s", want, plain.String())
		}
	}
	if strings.Contains(plain.String(), "HASH_OK") || strings.Contains(plain.String(), "      * keep") {
		t.Error("non-verbose output must hide PASS findings and actions")
	}
	for _, want := range []string{
		"      PASS  HASH_OK            2 stream(s) verified identical to source\n",
		"            tag COMMENT: http://x.example\n",
		"      * keep #0 video h264 und: primary video\n",
	} {
		if !strings.Contains(verbose.String(), want) {
			t.Errorf("verbose output lacks %q:\n%s", want, verbose.String())
		}
	}
}

func TestExampleContent(t *testing.T) {
	ex := Example()
	if ex.Schema != SchemaID || ex.Tool != "amuxify" || ex.Version != "0.3.0" || ex.Command != "ingest" || ex.Profile != "homelab" {
		t.Errorf("header %+v", *ex)
	}
	if ex.Hook == nil || *ex.Hook != (HookInfo{Adapter: "sabnzbd", Event: "pp status 0", Label: "Sample.Movie.2026.1080p", Category: "movies", FailOn: "FAIL", ExitCode: 1}) {
		t.Errorf("hook %+v", ex.Hook)
	}
	// The run-level error raises the verdict to at least FAIL, but a verdict
	// never goes down, so the BLOCK file keeps the run at BLOCK.
	if ex.Verdict != Block {
		t.Errorf("verdict %v, want BLOCK", ex.Verdict)
	}
	if !reflect.DeepEqual(ex.Counts, map[string]int{"BLOCK": 1, "PASS": 1, "WARN": 1}) {
		t.Errorf("counts %v", ex.Counts)
	}
	if len(ex.Files) != 3 || len(ex.Errors) != 1 {
		t.Fatalf("%d files, %d errors", len(ex.Files), len(ex.Errors))
	}
	want := []struct {
		path    string
		verdict Severity
		codes   []string
		millis  int64
	}{
		{"/srv/incoming/movie.mkv", Warn, []string{"LINK_IN_TAG", "HASH_OK", "PLACED", "ROUTE"}, 812},
		{"/srv/incoming/movie.nfo", Pass, []string{"SIDECAR_OK", "NFO_KODI", "NOTHING_TO_CLEAN", "ROUTE"}, 3},
		{"/srv/incoming/readme.exe", Block, []string{"SIDECAR_BLOCKED", "ROUTE"}, 1},
	}
	for i, w := range want {
		f := ex.Files[i]
		var codes []string
		for _, fd := range f.Findings {
			codes = append(codes, fd.Code)
		}
		if f.Path != w.path || f.Verdict != w.verdict || !reflect.DeepEqual(codes, w.codes) || f.Millis != w.millis {
			t.Errorf("file %d: %s %v %v %d", i, f.Path, f.Verdict, codes, f.Millis)
		}
		if f.Has("REFUSED") {
			t.Errorf("file %d: REFUSED is never added to a sidecar", i)
		}
	}
	if ex.Files[0].Output == "" || len(ex.Files[0].Actions) != 2 || ex.Files[0].Findings[0].Detail == "" {
		t.Error("the first file must carry output, two actions and a detail")
	}
	if !reflect.DeepEqual(ex.Files[0].Info, map[string]string{"container": "matroska", "kind": "matroska", "route": "remux"}) {
		t.Errorf("info %v", ex.Files[0].Info)
	}
	// Two calls are independent values: mutating one never leaks into the other.
	other := Example()
	other.Files[0].Findings[0].Code = "TAMPERED"
	other.Counts["PASS"] = 99
	if Example().Files[0].Findings[0].Code != "LINK_IN_TAG" || Example().Counts["PASS"] != 1 {
		t.Error("Example must return a fresh value on every call")
	}
}

// TestHostileStringsRoundTrip feeds paths and messages with quotes,
// backslashes, control characters, bidi controls and invalid UTF-8 through
// the encoder. The output must always be valid JSON and valid UTF-8, and
// every valid string must come back unchanged.
func TestHostileStringsRoundTrip(t *testing.T) {
	hostile := []string{
		`C:\srv\"quoted".mkv`,
		"/srv/tab\there/new\nline.mkv",
		"/srv/nul\x00byte.mkv",
		"/srv/esc\x1b[31mred\x1b[0m.mkv",
		"/srv/bidi\u202evkm.exe",
		"/srv/zero\u200bwidth\ufeff.mkv",
		"/srv/<script>alert(1)</script>&amp;.mkv",
		"/srv/\u2028line\u2029sep.mkv",
		"/srv/emoji\U0001F600.mkv",
		"/srv/" + strings.Repeat("a", 1<<15) + ".mkv",
		"}\n{\"schema\": \"forged\"}",
	}
	for _, h := range hostile {
		s := NewSummary("amuxify", "0.3.0", "scan", h)
		fr := FileResult{Path: h, Output: h, Actions: []string{h}, Info: map[string]string{h: h}}
		fr.Add(Finding{Code: "LINK_IN_TAG", Severity: Warn, Message: h, Detail: h + "\n" + h})
		s.Append(fr)
		s.Error(h)
		b := marshalJSON(t, s)
		if !json.Valid(b) {
			t.Errorf("%q produced invalid JSON:\n%s", h, b)
			continue
		}
		if !utf8.Valid(b) {
			t.Errorf("%q produced invalid UTF-8", h)
		}
		if bytes.Count(b, []byte("\n{")) != 0 {
			t.Errorf("%q split the output into more than one document", h)
		}
		if bytes.Contains(b, []byte{0}) || bytes.Contains(b, []byte{0x1b}) {
			t.Errorf("%q leaked a raw control byte into the JSON", h)
		}
		var back Summary
		if err := json.Unmarshal(b, &back); err != nil {
			t.Errorf("%q does not decode: %v", h, err)
			continue
		}
		if len(back.Files) != 1 || len(back.Errors) != 1 {
			t.Errorf("%q: %d files %d errors after the round trip", h, len(back.Files), len(back.Errors))
			continue
		}
		f := back.Files[0]
		for what, got := range map[string]string{
			"profile": back.Profile, "path": f.Path, "output": f.Output, "action": f.Actions[0],
			"info value": f.Info[h], "message": f.Findings[0].Message, "detail": f.Findings[0].Detail, "error": back.Errors[0],
		} {
			want := h
			if what == "detail" {
				want = h + "\n" + h
			}
			if got != want {
				t.Errorf("%q: %s came back as %q", h, what, got)
			}
		}
	}
}

// TestInvalidUTF8IsReplaced pins the encoder's behaviour for a path that is
// not valid UTF-8: the output is still valid JSON and valid UTF-8, and each
// invalid byte is replaced with U+FFFD. Consumers are told this in
// docs/report.md; a raw byte can never break the document.
func TestInvalidUTF8IsReplaced(t *testing.T) {
	raw := "/srv/latin1-caf\xe9.mkv"
	if utf8.ValidString(raw) {
		t.Fatal("test setup: the path must be invalid UTF-8")
	}
	s := NewSummary("amuxify", "0.3.0", "scan", "")
	s.Append(FileResult{Path: raw})
	b := marshalJSON(t, s)
	if !json.Valid(b) || !utf8.Valid(b) {
		t.Fatalf("invalid output:\n%s", b)
	}
	var back Summary
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if want := "/srv/latin1-caf\uFFFD.mkv"; back.Files[0].Path != want {
		t.Errorf("path came back as %q, want %q", back.Files[0].Path, want)
	}
}

func TestDetailWithNewlinesIsEscaped(t *testing.T) {
	s := NewSummary("amuxify", "0.3.0", "scan", "")
	fr := FileResult{Path: "/srv/a.mkv"}
	fr.Add(Finding{Code: "LINK_IN_TAG", Severity: Warn, Message: "2 link(s)", Detail: "tag A: http://a.example\r\ntag B: http://b.example\n"})
	s.Append(fr)
	b := marshalJSON(t, s)
	if !bytes.Contains(b, []byte(`"detail": "tag A: http://a.example\r\ntag B: http://b.example\n"`)) {
		t.Errorf("detail is not escaped as a single JSON string:\n%s", b)
	}
	// In the verbose human form each line of the detail is printed indented
	// under its finding; the carriage return is not treated as a separator.
	var out bytes.Buffer
	s.WriteHuman(&out, true)
	if !strings.Contains(out.String(), "            tag A: http://a.example\r\n            tag B: http://b.example\n            \n") {
		t.Errorf("verbose detail lines:\n%q", out.String())
	}
}

// TestMarshalDeterministic marshals the same summary twice and with map
// keys inserted in different orders; the bytes must be identical so that
// diffs and hashes of reports are meaningful.
func TestMarshalDeterministic(t *testing.T) {
	build := func(reverse bool) *Summary {
		s := Example()
		keys := []string{"zeta", "alpha", "mid", "Beta", "_under", "0nine"}
		if reverse {
			for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
		for _, k := range keys {
			s.Files[0].Info[k] = k
			s.Counts[k] = 1
		}
		return s
	}
	a := marshalJSON(t, build(false))
	b := marshalJSON(t, build(true))
	c := marshalJSON(t, build(false))
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Errorf("marshalling is not deterministic\n%s\n---\n%s", a, b)
	}
	if !bytes.Equal(marshalJSON(t, Example()), marshalJSON(t, Example())) {
		t.Error("Example does not marshal to the same bytes twice")
	}
}

// TestAppendCopiesTheResult makes sure a caller that keeps mutating its
// FileResult after Append does not change what is already in the report.
func TestAppendCopiesTheResult(t *testing.T) {
	s := NewSummary("amuxify", "0.3.0", "scan", "")
	fr := FileResult{Path: "/srv/a.mkv", Duration: 1500 * time.Millisecond}
	fr.Addf("HDR", Pass, "HDR10")
	s.Append(fr)
	fr.Path = "/srv/changed.mkv"
	fr.Findings[0].Code = "TAMPERED"
	fr.Verdict = Block
	got := s.Files[0]
	if got.Path != "/srv/a.mkv" || got.Verdict != Pass || got.Millis != 1500 {
		t.Errorf("stored result changed: %+v", got)
	}
	// The findings slice header is copied, so the element mutation above is
	// visible through both; a caller must not touch findings after Append.
	// What matters for the wire format is that Append computed duration_ms
	// from Duration and that Duration itself never reaches the JSON.
	b := marshalJSON(t, s)
	if !bytes.Contains(b, []byte(`"duration_ms": 1500`)) || bytes.Contains(b, []byte(`"Duration"`)) {
		t.Errorf("duration on the wire:\n%s", b)
	}
}

// TestHookOnTheWire checks the hook object's exact key set and that an
// omitted label or category does not appear.
func TestHookOnTheWire(t *testing.T) {
	s := NewSummary("amuxify", "0.3.0", "ingest", "homelab")
	s.Hook = &HookInfo{Adapter: "nzbget", Event: "post-process", FailOn: "BLOCK", ExitCode: 0}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(marshalJSON(t, s), &doc); err != nil {
		t.Fatal(err)
	}
	var hook map[string]interface{}
	if err := json.Unmarshal(doc["hook"], &hook); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hook, map[string]interface{}{"adapter": "nzbget", "event": "post-process", "fail_on": "BLOCK", "exit_code": float64(0)}) {
		t.Errorf("hook object %v", hook)
	}
	// Key order on the wire is the declaration order.
	if !bytes.Contains(marshalJSON(t, s), []byte("\"hook\": {\n    \"adapter\": \"nzbget\",\n    \"event\": \"post-process\",\n    \"fail_on\": \"BLOCK\",\n    \"exit_code\": 0\n  }")) {
		t.Errorf("hook layout:\n%s", marshalJSON(t, s))
	}
}
