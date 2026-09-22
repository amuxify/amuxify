package sniff

import "testing"

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
