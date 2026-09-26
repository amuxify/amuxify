package sniff

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBytes(t *testing.T) {
	cases := map[string]Kind{
		"\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01\x42\x82\x88matroska": Matroska,
		"\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01\x42\x82\x84webm":     WebM,
		"\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom":                                     MP4,
		"RIFF\x00\x00\x00\x00AVI LIST":                                                         AVI,
		"MZ\x90\x00\x03":                                                                       Executable,
		"\x7fELF\x02\x01":                                                                      Executable,
		"PK\x03\x04\x14\x00":                                                                   Archive,
		"%PDF-1.4":                                                                             PDF,
		"#!/bin/sh\necho hi":                                                                   Script,
		"1\n00:00:01,000 --> 00:00:02,000\nHello\n":                                            Text,
		"<!DOCTYPE html><html>":                                                                HTML,
		"":                                                                                     Empty,
	}
	for in, want := range cases {
		if got := Bytes([]byte(in)).Kind; got != want {
			t.Errorf("%q: got %s want %s", in[:min(len(in), 12)], got, want)
		}
	}
	ts := make([]byte, 188*4)
	for i := 0; i < 4; i++ {
		ts[i*188] = 0x47
	}
	if Bytes(ts).Kind != MPEGTS {
		t.Error("ts not detected")
	}
}

// A scene NFO is CP437 box drawing, which is not valid UTF-8. It must sniff
// as Unknown, never as anything Dangerous, so the sidecar gate lets it
// through and never mistakes it for an executable.
func TestCP437BodyIsUnknownNotDangerous(t *testing.T) {
	body := []byte("\xdb\xdb\xdb\xdb\xdb\xdb\r\n\xb0\xb1\xb2 SAMPLE.GROUP \xb2\xb1\xb0\r\n\r\nRelease notes.\r\n")
	res := Bytes(body)
	if res.Kind != Unknown {
		t.Fatalf("CP437 body: got %s want %s", res.Kind, Unknown)
	}
	if res.Kind.Dangerous() {
		t.Fatal("CP437 body classified as dangerous")
	}
	// The same body with a shebang or a PE header in front is still caught.
	if got := Bytes(append([]byte("#!/bin/sh\n"), body...)).Kind; got != Script {
		t.Fatalf("shebang before CP437: got %s", got)
	}
	if got := Bytes(append([]byte("MZ\x90\x00"), body...)).Kind; got != Executable {
		t.Fatalf("MZ before CP437: got %s", got)
	}
}

// HTML detection is case-insensitive: an upper-case doctype or tag is still
// HTML and still dangerous.
func TestHTMLUppercaseDoctype(t *testing.T) {
	cases := []string{
		"<!DOCTYPE HTML PUBLIC \"-//W3C//DTD HTML 4.01//EN\">\n<HTML><BODY>x</BODY></HTML>",
		"<!doctype HTML>",
		"<HTML><head></head></HTML>",
		"<Html>",
		"plain text first\n\n<SCRIPT>alert(1)</SCRIPT>",
		"\xef\xbb\xbf<!DOCTYPE html>",
		"1\n00:00:01,000 --> 00:00:02,000\n<script src=\"https://evil.example/x.js\"></script>\n",
	}
	for _, in := range cases {
		res := Bytes([]byte(in))
		if res.Kind != HTML {
			t.Errorf("%q: got %s want %s", in[:min(len(in), 24)], res.Kind, HTML)
		}
		if !res.Kind.Dangerous() {
			t.Errorf("%q: HTML not dangerous", in[:min(len(in), 24)])
		}
	}
	// Angle brackets in ordinary subtitles or Kodi NFO are not HTML.
	for _, in := range []string{
		"<movie><title>Sample</title><plot>Plain plot.</plot></movie>\n",
		"1\n00:00:01,000 --> 00:00:02,000\n<i>Hello</i>\n",
		"<b>bold</b> is not a page",
	} {
		if got := Bytes([]byte(in)).Kind; got != Text {
			t.Errorf("%q: got %s want %s", in, got, Text)
		}
	}
}

// Anything starting with "#!" is a script no matter what follows.
func TestShebangIsScript(t *testing.T) {
	cases := []string{
		"#!/bin/sh\nrm -rf /\n",
		"#!/usr/bin/env python3\nimport os\n",
		"#!/bin/bash",
		"#!",
		"#!\xdb\xdb\xdb",
		"#! /usr/bin/perl -w\n",
	}
	for _, in := range cases {
		res := Bytes([]byte(in))
		if res.Kind != Script {
			t.Errorf("%q: got %s want %s", in, res.Kind, Script)
		}
		if !res.Kind.Dangerous() {
			t.Errorf("%q: script not dangerous", in)
		}
	}
	// A comment line that is not a shebang stays text.
	for _, in := range []string{"# not a shebang\n", " #!/bin/sh\n", "\xef\xbb\xbf#!/bin/sh\n"} {
		if got := Bytes([]byte(in)).Kind; got == Script {
			t.Errorf("%q: false positive shebang", in)
		}
	}
}

// Hostile bodies never reach a Dangerous verdict by accident and never panic.
func TestHostileBodiesDoNotPanicOrMisclassify(t *testing.T) {
	cases := map[string]Kind{
		"\x00":                         Unknown,
		"\x00\x00\x00\x00":             Unknown,
		"text\x00with nul":             Unknown,
		"\xef\xbb\xbf":                 Text, // bare UTF-8 BOM
		"M":                            Text,
		"P":                            Text,
		"MZ":                           Executable,
		"PK":                           Text,
		"PK\x03\x04":                   Archive,
		"\x7fELF":                      Executable,
		"%PDF-":                        PDF,
		"%PDF":                         Text,
		"\x1f\x8b\x08":                 Archive,
		"\xca\xfe\xba\xbe":             Executable,
		"RIFF\x00\x00\x00\x00WEBPVP8 ": Image,
	}
	for in, want := range cases {
		if got := Bytes([]byte(in)).Kind; got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	for _, in := range []string{"\xff\xfe\x00\x00", "\xff\xff\xff\xff", "\x7fEL", "\xff\xfeh\x00i\x00"} {
		if got := Bytes([]byte(in)).Kind; got.Dangerous() {
			t.Errorf("%q: classified dangerous as %s", in, got)
		}
	}
	// Long inputs: 64 KiB of each byte value.
	for b := 0; b < 256; b++ {
		buf := make([]byte, 1<<16)
		for i := range buf {
			buf[i] = byte(b)
		}
		_ = Bytes(buf)
	}
	// Every single byte and every pair of bytes classify without panicking.
	for a := 0; a < 256; a++ {
		_ = Bytes([]byte{byte(a)})
		for b := 0; b < 256; b += 17 {
			_ = Bytes([]byte{byte(a), byte(b)})
		}
	}
}

// A file that is exactly a valid container header followed by an executable
// is still reported by its header; the appended payload is the scanner's
// polyglot check to catch, so sniff must not silently hide the header.
func TestHeaderWinsOverTrailingPayload(t *testing.T) {
	ebml := "\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01\x42\x82\x88matroska"
	if got := Bytes([]byte(ebml + "MZ\x90\x00")).Kind; got != Matroska {
		t.Fatalf("EBML then MZ: got %s", got)
	}
	if got := Bytes([]byte("MZ\x90\x00" + ebml)).Kind; got != Executable {
		t.Fatalf("MZ then EBML: got %s", got)
	}
}

func TestFileReadsHeaderOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.bin")
	body := make([]byte, 64<<10)
	copy(body, "\x1a\x45\xdf\xa3")
	copy(body[len(body)-4:], "MZ\x90\x00")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := File(p)
	if err != nil || res.Kind != Matroska {
		t.Fatalf("File: %s %v", res.Kind, err)
	}
	if _, err := File(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file: no error")
	}
	if _, err := File(dir); err == nil {
		t.Fatal("directory: no error")
	}
}

func TestExtensionKindsCoverSidecarsAndMedia(t *testing.T) {
	if k := ExtensionKinds("nfo"); len(k) != 1 || k[0] != Text {
		t.Fatalf("nfo kinds %v", k)
	}
	if k := ExtensionKinds("mkv"); len(k) != 1 || k[0] != Matroska {
		t.Fatalf("mkv kinds %v", k)
	}
	if k := ExtensionKinds("xyz-unknown"); k != nil {
		t.Fatalf("unknown extension kinds %v", k)
	}
	for _, k := range []Kind{Executable, Archive, PDF, HTML, Script} {
		if !k.Dangerous() {
			t.Errorf("%s not dangerous", k)
		}
	}
	for _, k := range []Kind{Matroska, MP4, Text, Image, Unknown, Empty} {
		if k.Dangerous() {
			t.Errorf("%s dangerous", k)
		}
	}
}
