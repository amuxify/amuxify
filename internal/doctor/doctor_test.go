package doctor

import "testing"

func TestParseVersion(t *testing.T) {
	cases := map[string][]int{
		"mkvmerge v102.0 ('Truth') 64-bit": {102, 0, 0},
		"ffmpeg version 9.0.1 Copyright":   {9, 0, 1},
		"ffmpeg version n7.1-3":            {7, 1, 0},
		"ffmpeg version 4.4.2-0ubuntu0.22": {4, 4, 2},
		"13.55":                            {13, 55, 0},
	}
	for in, want := range cases {
		got, ok := parseVersion(in)
		if !ok || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("%q: got %v ok=%v want %v", in, got, ok, want)
		}
	}
	if !less([]int{4, 3, 9}, []int{4, 4}) || less([]int{5, 0}, []int{4, 4}) || less([]int{50, 0}, []int{50, 0}) {
		t.Fatal("less wrong")
	}
	if _, ok := parseVersion("ffmpeg version N-112345-gabcdef"); ok {
		t.Fatal("git build should be unparseable")
	}
}
