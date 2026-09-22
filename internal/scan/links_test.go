package scan

import "testing"

func TestFindLinks(t *testing.T) {
	got := FindLinks("Ripped by GROUP - visit https://example.com/x?y=1 or www.tracker.to and EXAMPLE.NET. Enjoy 5.1 audio, x264 S01E02.")
	want := []string{"https://example.com/x?y=1", "www.tracker.to", "EXAMPLE.NET"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if len(FindLinks("Episode 12 - The Return (Director's Cut) 1080p 5.1 DTS-HD")) != 0 {
		t.Fatal("false positive on plain title")
	}
}

func TestBidi(t *testing.T) {
	if bidiChars("movie‮vkm.exe") == "" || bidiChars("movie.mkv") != "" {
		t.Fatal("bidi detection wrong")
	}
}
