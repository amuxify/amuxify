package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain re-executes the test binary as a helper tool. The Runner strips the
// environment of every child it starts, so the helper mode travels in argv:
// the Runner is told to run the tool with "-amuxify-helper <mode>" among its
// arguments, and every other argument is handed to the mode in order.
// AMUXIFY_TEST_HELPER=<mode> is honoured as well for running the binary by
// hand.
func TestMain(m *testing.M) {
	mode := os.Getenv("AMUXIFY_TEST_HELPER")
	var args []string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-amuxify-helper" && i+1 < len(os.Args) && mode == "" {
			mode = os.Args[i+1]
			i++
			continue
		}
		args = append(args, os.Args[i])
	}
	if mode == "" {
		os.Exit(m.Run())
	}
	switch mode {
	case "sleep":
		time.Sleep(5 * time.Second)
		os.Exit(0)
	case "echo":
		for _, a := range args {
			fmt.Printf("%s\n", a)
		}
		os.Exit(0)
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		os.Exit(0)
	case "exit3":
		fmt.Fprintln(os.Stderr, "helper failing on purpose")
		os.Exit(3)
	case "forkexit", "forkhang":
		// Start a grandchild that inherits this process's stdout and
		// stderr and sleeps, so the pipes stay open after this process is
		// gone. forkexit then exits at once; forkhang sleeps as well so
		// the Runner has to kill it on timeout while the grandchild still
		// holds the pipes. Neither is reaped: that is the point.
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(98)
		}
		gc := osexec.Command(self, "-amuxify-helper", "sleep")
		gc.Stdout = os.Stdout
		gc.Stderr = os.Stderr
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(98)
		}
		fmt.Println("parent wrote this")
		if mode == "forkhang" {
			time.Sleep(5 * time.Second)
		}
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	os.Exit(99)
}

// helperTool registers the running test binary under a tool name and returns
// the argv prefix that selects the helper mode.
func helperTool(t *testing.T, tool, mode string) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv("AMUXIFY_"+strings.ToUpper(tool), self)
	return []string{"-amuxify-helper", mode}
}

// Guarantee 4: every ffmpeg and ffprobe call carries the protocol whitelist.
func TestFFGuardPrependsWhitelist(t *testing.T) {
	user := []string{"-i", "in.mkv", "-f", "null", "-"}
	got := ffGuard(FFmpeg, user)
	want := append([]string{"-nostdin", "-hide_banner", "-protocol_whitelist", "file,pipe"}, user...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ffmpeg guard:\n got %q\nwant %q", got, want)
	}
	got = ffGuard(FFprobe, user)
	want = append([]string{"-hide_banner", "-protocol_whitelist", "file,pipe"}, user...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ffprobe guard:\n got %q\nwant %q", got, want)
	}
	if ProtocolWhitelist != "file,pipe" {
		t.Fatalf("ProtocolWhitelist = %q", ProtocolWhitelist)
	}
	// The whitelist must never widen to a network or device protocol.
	for _, p := range strings.Split(ProtocolWhitelist, ",") {
		switch p {
		case "file", "pipe":
		default:
			t.Fatalf("protocol %q is not allowed in the whitelist", p)
		}
	}
}

// The guard is applied by RunWithTimeout itself, not left to callers, and it
// comes first so no later argument is read before it.
func TestRunAppliesGuardToFFmpegFamily(t *testing.T) {
	for tool, guard := range map[string][]string{
		FFmpeg:  {"-nostdin", "-hide_banner", "-protocol_whitelist", "file,pipe"},
		FFprobe: {"-hide_banner", "-protocol_whitelist", "file,pipe"},
	} {
		prefix := helperTool(t, tool, "echo")
		r := &Runner{}
		res, err := r.RunWithTimeout(context.Background(), 10*time.Second, tool, append(prefix, "-i", "http://evil.example/x")...)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(res.Stdout), "\n"), "\n")
		want := append(append([]string{}, guard...), "-i", "http://evil.example/x")
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s argv:\n got %q\nwant %q", tool, got, want)
		}
	}
	// Other tools get no ffmpeg flags.
	prefix := helperTool(t, MKVMerge, "echo")
	r := &Runner{}
	res, err := r.RunWithTimeout(context.Background(), 10*time.Second, MKVMerge, append(prefix, "-J", "x.mkv")...)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "-J\nx.mkv" {
		t.Fatalf("mkvmerge argv: %q", got)
	}
}

// noLocale clears the locale variables so the fallback locale is chosen.
func noLocale(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
	}
}

// Guarantee 4: dynamic-loader injection variables never reach a tool.
func TestCleanEnvDropsLDPreload(t *testing.T) {
	noLocale(t)
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/evil.dylib")
	t.Setenv("LD_LIBRARY_PATH", "/tmp")
	t.Setenv("DYLD_LIBRARY_PATH", "/tmp")
	t.Setenv("FFREPORT", "file=/tmp/x.log:level=32")
	t.Setenv("AV_LOG_FORCE_COLOR", "1")
	t.Setenv("MKVTOOLNIX_DEBUG", "1")
	t.Setenv("PERL5OPT", "-Mevil")
	t.Setenv("PATH", "/usr/bin")
	env := cleanEnv()
	keys := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		keys[k] = v
	}
	for _, bad := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH", "FFREPORT", "AV_LOG_FORCE_COLOR", "MKVTOOLNIX_DEBUG", "PERL5OPT"} {
		if _, ok := keys[bad]; ok {
			t.Errorf("%s leaked into the tool environment", bad)
		}
	}
	// The locale is pinned to a UTF-8 variant: plain "C" makes mkvmerge
	// truncate a path at the first non-ASCII byte.
	if keys["LANG"] != "C.UTF-8" || keys["LC_CTYPE"] != "C.UTF-8" || keys["LC_MESSAGES"] != "C" {
		t.Errorf("locale not pinned to C.UTF-8 with English messages: %v", env)
	}
	if _, ok := keys["LC_ALL"]; ok {
		t.Errorf("LC_ALL must stay unset so LC_MESSAGES=C can take effect: %v", env)
	}
	if keys["PATH"] != "/usr/bin" {
		t.Errorf("PATH not forwarded: %v", env)
	}
	allowed := map[string]bool{"LANG": true, "LC_CTYPE": true, "LC_MESSAGES": true, "PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "SystemRoot": true, "USERPROFILE": true}
	for k := range keys {
		if !allowed[k] {
			t.Errorf("unexpected variable %s in the tool environment", k)
		}
	}
}

// The clean environment is what the child actually receives.
func TestRunUsesCleanEnv(t *testing.T) {
	noLocale(t)
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/evil.dylib")
	t.Setenv("AMUXIFY_SECRET_CANARY", "leaked")
	prefix := helperTool(t, "envtool", "env")
	r := &Runner{}
	res, err := r.RunWithTimeout(context.Background(), 10*time.Second, "envtool", prefix...)
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	for _, bad := range []string{"LD_PRELOAD=", "DYLD_INSERT_LIBRARIES=", "AMUXIFY_SECRET_CANARY=", "AMUXIFY_ENVTOOL="} {
		if strings.Contains(out, bad) {
			t.Errorf("child environment contains %s:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "LC_CTYPE=C.UTF-8\n") || !strings.Contains(out, "LANG=C.UTF-8\n") || !strings.Contains(out, "LC_MESSAGES=C\n") || strings.Contains(out, "LC_ALL=") {
		t.Errorf("child environment lacks the C.UTF-8 locale:\n%s", out)
	}
}

// needTool mirrors testutil.Need for this package, which testutil imports:
// the test skips when the tool is missing and fails under
// AMUXIFY_REQUIRE_TOOLS=1.
func needTool(t *testing.T, tool string) *Runner {
	t.Helper()
	r := &Runner{}
	if !r.Have(tool) {
		if os.Getenv("AMUXIFY_REQUIRE_TOOLS") == "1" {
			t.Fatalf("%s is required (AMUXIFY_REQUIRE_TOOLS=1)", tool)
		}
		t.Skipf("skipping: %s not installed", tool)
	}
	return r
}

// fixture returns the path of a generated corpus file. The corpus is found
// through AMUXIFY_FIXTURES, as testutil.Fixtures does; without it the test
// skips, or fails when tools are required.
func fixture(t *testing.T, name string) string {
	t.Helper()
	dir := os.Getenv("AMUXIFY_FIXTURES")
	if dir == "" {
		if os.Getenv("AMUXIFY_REQUIRE_TOOLS") == "1" {
			t.Fatal("AMUXIFY_FIXTURES must point at the generated corpus (make fixtures)")
		}
		t.Skip("skipping: AMUXIFY_FIXTURES unset")
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return p
}

// mkvmerge must see the whole path when it lies under a directory whose name
// holds non-ASCII bytes, including bidi and zero-width characters. Under the
// plain "C" locale mkvmerge truncates the argument at the first non-ASCII
// byte and reports the file as missing; the pinned C.UTF-8 locale keeps it
// whole.
func TestRunPassesNonASCIIPathsToMkvmerge(t *testing.T) {
	r := needTool(t, MKVMerge)
	src := fixture(t, "clean.mkv")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, dirName := range []string{"Épisode 1 – 日本語", "\u202e\u200bbidi zero\u200cwidth", "émoji 🎬 folder"} {
		dir := filepath.Join(t.TempDir(), dirName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "clean ünicode.mkv")
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := r.RunWithTimeout(context.Background(), time.Minute, MKVMerge, "-J", path)
		if err != nil {
			t.Fatalf("%q: %v", dirName, err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("%q: mkvmerge -J exit %d\nstdout: %s\nstderr: %s", dirName, res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(string(res.Stdout), `"recognized": true`) && !strings.Contains(string(res.Stdout), `"recognized":true`) {
			t.Fatalf("%q: container not recognized:\n%s", dirName, res.Stdout)
		}
	}
}

func TestPathHonoursOverride(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg-here")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_FFMPEG", fake)
	r := &Runner{}
	got, err := r.Path(FFmpeg)
	if err != nil || got != fake {
		t.Fatalf("override: got %q, %v", got, err)
	}
	if !r.Have(FFmpeg) {
		t.Fatal("Have false with a valid override")
	}

	// A copy of the running test binary is a real executable and resolves.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfBytes, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(dir, "real-tool")
	if err := os.WriteFile(real, selfBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_FFMPEG", real)
	if got, err := (&Runner{}).Path(FFmpeg); err != nil || got != real {
		t.Fatalf("copy of the test binary: got %q, %v", got, err)
	}

	r2 := &Runner{}
	missing := filepath.Join(dir, "does-not-exist")
	t.Setenv("AMUXIFY_FFMPEG", missing)
	if _, err := r2.Path(FFmpeg); err == nil {
		t.Fatal("missing override path resolved")
	} else if !strings.Contains(err.Error(), "AMUXIFY_FFMPEG="+missing) {
		t.Fatalf("error does not name the override: %v", err)
	}
	if r2.Have(FFmpeg) {
		t.Fatal("Have true with a missing override")
	}
}

// An override that names something other than an executable regular file
// is refused up front, so doctor and Have never report a tool as present
// that could not actually be run.
func TestPathOverrideMustBeExecutableFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a-directory")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain-file")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, path, want string
	}{
		{"directory", sub, "not a regular file"},
		{"non-executable file", plain, "not executable"},
		{"missing path", filepath.Join(dir, "missing"), "no such file"},
	}
	for _, tc := range cases {
		if tc.name == "non-executable file" && runtime.GOOS == "windows" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AMUXIFY_FFPROBE", tc.path)
			r := &Runner{}
			got, err := r.Path(FFprobe)
			if err == nil {
				t.Fatalf("override %s resolved to %q", tc.path, got)
			}
			if !strings.Contains(err.Error(), "AMUXIFY_FFPROBE="+tc.path) {
				t.Errorf("error does not name the override: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if r.Have(FFprobe) {
				t.Error("Have true for an unusable override")
			}
			if _, err := r.RunWithTimeout(context.Background(), time.Second, FFprobe, "-version"); err == nil {
				t.Error("RunWithTimeout ran an unusable override")
			}
		})
	}
}

// An override is a path, never a command line: shell metacharacters and
// embedded arguments are not interpreted.
func TestPathOverrideIsNotAShellCommand(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "ffprobe")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hostile := range []string{
		real + " -version",
		real + "; touch " + filepath.Join(dir, "pwned"),
		"$(touch " + filepath.Join(dir, "pwned") + ")",
		"`id`",
	} {
		t.Setenv("AMUXIFY_FFPROBE", hostile)
		r := &Runner{}
		if p, err := r.Path(FFprobe); err == nil {
			t.Errorf("hostile override %q resolved to %q", hostile, p)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("an override was executed through a shell")
	}
}

func TestUnknownToolIsErrNotFound(t *testing.T) {
	r := &Runner{}
	_, err := r.Path("amuxify-no-such-tool-" + fmt.Sprint(os.Getpid()))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
	res, err := r.RunWithTimeout(context.Background(), time.Second, "amuxify-no-such-tool-"+fmt.Sprint(os.Getpid()))
	if err == nil || res != nil {
		t.Fatalf("RunWithTimeout on a missing tool: res=%v err=%v", res, err)
	}
}

// Guarantee 4: a hung tool is killed and reported, never waited on forever.
func TestRunTimeoutReturnsTimedOut(t *testing.T) {
	prefix := helperTool(t, "sleeptool", "sleep")
	r := &Runner{}
	start := time.Now()
	res, err := r.RunWithTimeout(context.Background(), 200*time.Millisecond, "sleeptool", prefix...)
	if err == nil {
		t.Fatal("no error after the timeout")
	}
	if res == nil || !res.TimedOut {
		t.Fatalf("result does not report the timeout: %+v", res)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error text: %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("RunWithTimeout waited %v for a 200ms timeout", time.Since(start))
	}
}

// A cancelled parent context stops the tool too.
func TestRunHonoursContextCancel(t *testing.T) {
	prefix := helperTool(t, "sleeptool", "sleep")
	r := &Runner{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := r.RunWithTimeout(ctx, 0, "sleeptool", prefix...)
	if time.Since(start) > 4*time.Second {
		t.Fatalf("waited %v after cancel", time.Since(start))
	}
	// A killed child is an error, never a completed run with an exit code
	// that a caller might read as a verdict.
	if err == nil {
		t.Fatalf("cancelled run reported no error: %+v", res)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v does not wrap context.Canceled", err)
	}
	if res == nil || res.ExitCode == 0 {
		t.Fatalf("cancelled run carries a success exit code: %+v", res)
	}
	if res.TimedOut {
		t.Fatal("cancellation reported as a timeout")
	}
}

// Arguments reach the tool verbatim, one argv entry each: there is no shell
// in between that could split on spaces or expand metacharacters.
func TestRunPassesArgumentsVerbatim(t *testing.T) {
	prefix := helperTool(t, "echotool", "echo")
	dir := t.TempDir()
	hostile := []string{
		"a b",
		"; touch " + filepath.Join(dir, "pwned") + " ;",
		"$(touch " + filepath.Join(dir, "pwned2") + ")",
		"`id`",
		"file:///etc/passwd",
		"-i",
		"",
		"|",
		"\u202e.exe",
		"ünïcödé",
	}
	r := &Runner{}
	res, err := r.RunWithTimeout(context.Background(), 10*time.Second, "echotool", append(prefix, hostile...)...)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	got := strings.Split(strings.TrimSuffix(string(res.Stdout), "\n"), "\n")
	if !reflect.DeepEqual(got, hostile) {
		t.Fatalf("argv mangled:\n got %q\nwant %q", got, hostile)
	}
	for _, p := range []string{"pwned", "pwned2"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err == nil {
			t.Fatalf("argument %s was interpreted by a shell", p)
		}
	}
}

func TestNonZeroExitIsNotAnError(t *testing.T) {
	prefix := helperTool(t, "failtool", "exit3")
	r := &Runner{}
	res, err := r.RunWithTimeout(context.Background(), 10*time.Second, "failtool", prefix...)
	if err != nil {
		t.Fatalf("non-zero exit surfaced as error: %v", err)
	}
	if res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("result: %+v", res)
	}
	if !strings.Contains(string(res.Stderr), "on purpose") {
		t.Fatalf("stderr not captured: %q", res.Stderr)
	}
}

func TestRunRecordsCmdlineAndTrace(t *testing.T) {
	prefix := helperTool(t, "echotool", "echo")
	var traced []string
	r := &Runner{Trace: func(s string) { traced = append(traced, s) }}
	res, err := r.Run(context.Background(), "echotool", append(prefix, "x")...)
	if err != nil {
		t.Fatal(err)
	}
	if len(traced) != 1 || traced[0] != res.Cmdline {
		t.Fatalf("trace %q, cmdline %q", traced, res.Cmdline)
	}
	if !strings.HasSuffix(res.Cmdline, " -amuxify-helper echo x") {
		t.Fatalf("cmdline: %q", res.Cmdline)
	}
}

// The override variable name is derived from the tool name in upper case, so
// a tool name cannot smuggle in a different variable.
func TestOverrideNameIsUpperCasedToolName(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "tool")
	if err := os.WriteFile(fake, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_MKVPROPEDIT", fake)
	r := &Runner{}
	if p, err := r.Path(MKVPropedit); err != nil || p != fake {
		t.Fatalf("mkvpropedit override: %q %v", p, err)
	}
	names := []string{FFmpeg, FFprobe, MKVMerge, MKVPropedit, MKVExtract, ExifTool, ClamScan}
	sort.Strings(names)
	for _, n := range names {
		if strings.ToLower(n) != n || strings.ContainsAny(n, " /\\;$") {
			t.Errorf("tool name %q is not a plain lower-case word", n)
		}
	}
}

// The locale handed to the tools is the caller's own UTF-8 locale when one
// is set, because that one exists on the host, and C.UTF-8 otherwise. It
// reaches the child as LANG and LC_CTYPE only; LC_MESSAGES is pinned to C so
// the tool messages amuxify matches on stay English, and LC_ALL is never
// set because it would override that pin. A value that is not shaped like
// a locale name is never forwarded, however it mentions UTF-8, so nothing
// but those three variables can come out of the caller's locale settings.
func TestLocaleSelection(t *testing.T) {
	cases := []struct {
		name   string
		lcAll  string
		lcType string
		lang   string
		want   string
	}{
		{"nothing set", "", "", "", "C.UTF-8"},
		{"only LANG", "", "", "en_US.UTF-8", "en_US.UTF-8"},
		{"only LANG utf8 spelling", "", "", "en_US.utf8", "en_US.utf8"},
		{"only LANG upper case", "", "", "DE_DE.UTF8", "DE_DE.UTF8"},
		{"LC_ALL wins over LANG", "de_DE.UTF-8", "", "fr_FR.UTF-8", "de_DE.UTF-8"},
		{"LC_CTYPE wins over LANG", "", "ja_JP.UTF-8", "fr_FR.UTF-8", "ja_JP.UTF-8"},
		{"LC_ALL not UTF-8 falls through to LANG", "C", "", "en_GB.UTF-8", "en_GB.UTF-8"},
		{"all set but none UTF-8", "C", "POSIX", "en_US.ISO-8859-1", "C.UTF-8"},
		{"LANG C", "", "", "C", "C.UTF-8"},
		{"modifier", "", "", "sr_RS.UTF-8@latin", "sr_RS.UTF-8@latin"},
		{"shell metacharacters", "", "", "en_US.UTF-8; touch /tmp/pwned", "C.UTF-8"},
		{"command substitution", "", "", "$(id).UTF-8", "C.UTF-8"},
		{"newline injecting a variable", "", "", "en_US.UTF-8\nLD_PRELOAD=/tmp/evil.so", "C.UTF-8"},
		{"equals sign", "", "", "UTF-8=LD_PRELOAD", "C.UTF-8"},
		{"leading space", "", "", " en_US.UTF-8", "C.UTF-8"},
		{"quotes", "", "", "'en_US.UTF-8'", "C.UTF-8"},
		{"slash", "", "", "../UTF-8", "C.UTF-8"},
		{"over long", "", "", strings.Repeat("a", 60) + ".UTF-8", "C.UTF-8"},
		{"hostile LC_ALL falls through to LANG", "en_US.UTF-8`id`", "", "en_US.UTF-8", "en_US.UTF-8"},
		{"non-ascii", "", "", "en_US.UTF-8\u00e9", "C.UTF-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LC_ALL", tc.lcAll)
			t.Setenv("LC_CTYPE", tc.lcType)
			t.Setenv("LANG", tc.lang)
			t.Setenv("PATH", "/usr/bin")
			if got := Locale(); got != tc.want {
				t.Fatalf("Locale() = %q, want %q", got, tc.want)
			}
			env := cleanEnv()
			var lang, lcType, lcMessages int
			for _, kv := range env {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "LANG":
					lang++
				case "LC_CTYPE":
					lcType++
				case "LC_MESSAGES":
					lcMessages++
					if v != "C" {
						t.Errorf("LC_MESSAGES=%q, want C so tool messages stay English", v)
					}
				case "LC_ALL":
					t.Errorf("LC_ALL forwarded, which would override LC_MESSAGES: %q", kv)
				}
				if k == "LANG" || k == "LC_CTYPE" {
					if v != tc.want {
						t.Errorf("%s=%q, want %q", k, v, tc.want)
					}
				}
				if strings.ContainsAny(kv, "\n;`$' ") || strings.Contains(kv, "LD_PRELOAD") || strings.Contains(kv, "touch") {
					t.Errorf("hostile bytes reached the tool environment: %q", kv)
				}
			}
			if lang != 1 || lcType != 1 || lcMessages != 1 {
				t.Errorf("LANG appears %d times, LC_CTYPE %d times and LC_MESSAGES %d times: %v", lang, lcType, lcMessages, env)
			}
			allowed := map[string]bool{"LANG": true, "LC_CTYPE": true, "LC_MESSAGES": true, "PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "SystemRoot": true, "USERPROFILE": true}
			for _, kv := range env {
				if k, _, _ := strings.Cut(kv, "="); !allowed[k] {
					t.Errorf("unexpected variable in the tool environment: %q", kv)
				}
			}
		})
	}
}

// A child sees the caller's UTF-8 locale, and a hostile LANG cannot
// smuggle a second variable into the child.
func TestRunForwardsCallerUTF8Locale(t *testing.T) {
	prefix := helperTool(t, "envtool", "env")
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", "en_US.UTF-8")
	r := &Runner{}
	res, err := r.RunWithTimeout(context.Background(), 10*time.Second, "envtool", prefix...)
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "LC_CTYPE=en_US.UTF-8\n") || !strings.Contains(out, "LANG=en_US.UTF-8\n") || !strings.Contains(out, "LC_MESSAGES=C\n") || strings.Contains(out, "LC_ALL=") {
		t.Errorf("child environment lacks the caller's locale or the English message pin:\n%s", out)
	}
	t.Setenv("LANG", "en_US.UTF-8\nLD_PRELOAD=/tmp/evil.so")
	res, err = r.RunWithTimeout(context.Background(), 10*time.Second, "envtool", prefix...)
	if err != nil {
		t.Fatal(err)
	}
	out = string(res.Stdout)
	if strings.Contains(out, "LD_PRELOAD") || !strings.Contains(out, "LC_CTYPE=C.UTF-8\n") {
		t.Errorf("hostile LANG reached the child:\n%s", out)
	}
}

// A child that hands its stdout to a grandchild and then exits, or is
// killed on timeout while the grandchild lives on, must not hold the run
// open for as long as the grandchild keeps the pipe. WaitDelay bounds the
// wait; the run returns within the delay and never reports success for
// output it could not be sure was complete.
func TestWaitDelayReleasesPipeHeldByGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("inherited pipe semantics differ on Windows")
	}
	for _, mode := range []string{"forkexit", "forkhang"} {
		t.Run(mode, func(t *testing.T) {
			prefix := helperTool(t, "forker", mode)
			r := &Runner{WaitDelay: 500 * time.Millisecond}
			start := time.Now()
			res, err := r.RunWithTimeout(context.Background(), time.Second, "forker", prefix...)
			took := time.Since(start)
			if took > 3*time.Second {
				t.Fatalf("run held for %s by the grandchild's pipe", took)
			}
			if err == nil {
				t.Fatalf("run reported success although its pipes had to be forced shut: %+v", res)
			}
			if mode == "forkhang" && !res.TimedOut {
				t.Errorf("timeout not reported: %v", err)
			}
			if !strings.Contains(string(res.Stdout), "parent wrote this") {
				t.Errorf("output written before the pipe was released is missing: %q", res.Stdout)
			}
		})
	}
}

// Without a WaitDelay the same grandchild would hold the run for its whole
// life; the default is a few seconds so a stuck run cannot last forever.
func TestWaitDelayDefault(t *testing.T) {
	r := &Runner{}
	if d := r.waitDelay(); d != DefaultWaitDelay || d <= 0 || d > 30*time.Second {
		t.Fatalf("default wait delay %s", d)
	}
	r.WaitDelay = time.Second
	if r.waitDelay() != time.Second {
		t.Fatal("explicit wait delay ignored")
	}
}

type countingWriter struct{ b []byte }

func (c *countingWriter) Write(p []byte) (int, error) { c.b = append(c.b, p...); return len(p), nil }

// RunStreaming hands stdout to the writer and leaves Result.Stdout empty,
// still collects stderr and the exit code, refuses a nil writer, and
// surfaces a failing writer as an error rather than a truncated success.
func TestRunStreaming(t *testing.T) {
	prefix := helperTool(t, "streamer", "echo")
	r := &Runner{}
	var w countingWriter
	res, err := r.RunStreaming(context.Background(), 10*time.Second, "streamer", &w, append(prefix, "one", "two")...)
	if err != nil {
		t.Fatal(err)
	}
	if string(w.b) != "one\ntwo\n" || res.Stdout != nil || res.ExitCode != 0 {
		t.Fatalf("writer got %q, Result.Stdout %q, exit %d", w.b, res.Stdout, res.ExitCode)
	}
	if _, err := r.RunStreaming(context.Background(), 10*time.Second, "streamer", nil, prefix...); err == nil {
		t.Fatal("nil writer accepted")
	}
	prefix = helperTool(t, "failer", "exit3")
	res, err = r.RunStreaming(context.Background(), 10*time.Second, "failer", &w, prefix...)
	if err != nil || res.ExitCode != 3 || !strings.Contains(string(res.Stderr), "failing on purpose") {
		t.Fatalf("exit %d err %v stderr %q", res.ExitCode, err, res.Stderr)
	}
	prefix = helperTool(t, "streamer", "echo")
	if _, err := r.RunStreaming(context.Background(), 10*time.Second, "streamer", failingWriter{}, append(prefix, "x")...); err == nil {
		t.Fatal("write failure was not reported")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// Workers of a parallel run resolve tools through one Runner at the same
// time; the path cache is filled and read under a lock, so the race
// detector sees no unsynchronised access and every caller gets the same
// answer. A hostile override is checked by every caller, not only the first.
func TestPathConcurrentCallers(t *testing.T) {
	helperTool(t, "shared", "echo")
	r := &Runner{}
	var wg sync.WaitGroup
	paths := make([]string, 16)
	errs := make([]error, 16)
	for i := range paths {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				paths[i], errs[i] = r.Path("shared")
				if errs[i] != nil {
					return
				}
				_ = r.Have("no-such-tool-anywhere")
			}
		}(i)
	}
	wg.Wait()
	for i := range paths {
		if errs[i] != nil || paths[i] != paths[0] || paths[i] == "" {
			t.Errorf("caller %d: %q %v, first %q", i, paths[i], errs[i], paths[0])
		}
	}
}
