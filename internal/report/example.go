package report

import "time"

// Example returns a deterministic report that exercises every field. It is
// the golden fixture and the example in docs/report.md.
//
// The report describes an ingest run started by the sabnzbd adapter over
// three files and one path that vanished before it could be scanned. The
// messages, actions and info keys are the ones the real commands emit, so
// the example stays representative of a live run.
func Example() *Summary {
	s := NewSummary("amuxify", "0.3.0", "ingest", "homelab")
	s.Hook = &HookInfo{
		Adapter:  "sabnzbd",
		Event:    "pp status 0",
		Label:    "Sample.Movie.2026.1080p",
		Category: "movies",
		FailOn:   "FAIL",
		ExitCode: 1,
	}
	s.Started = time.Date(2026, time.September, 22, 10, 0, 0, 0, time.UTC)

	movie := FileResult{Path: "/srv/incoming/movie.mkv", Duration: 812 * time.Millisecond}
	movie.Add(Finding{
		Code:     "LINK_IN_TAG",
		Severity: Warn,
		Message:  "1 link(s) in metadata: tag COMMENT: http://x.example",
		Detail:   "tag COMMENT: http://x.example",
	})
	movie.Addf("HASH_OK", Pass, "2 stream(s) verified identical to source")
	movie.Addf("PLACED", Pass, "written and verified")
	movie.Addf("ROUTE", Pass, "remux: #1 default flag false -> true")
	movie.Output = "/srv/incoming__remuxed/movie.mkv"
	movie.Actions = []string{
		"keep #0 video h264 und: primary video",
		"keep #1 audio aac eng default: language eng",
	}
	movie.Info = map[string]string{"container": "matroska", "kind": "matroska", "route": "remux"}
	s.Append(movie)

	nfo := FileResult{Path: "/srv/incoming/movie.nfo", Duration: 3 * time.Millisecond}
	nfo.Addf("SIDECAR_OK", Pass, "allowed sidecar .nfo")
	nfo.Addf("NFO_KODI", Pass, "Kodi movie nfo")
	nfo.Addf("NOTHING_TO_CLEAN", Pass, "sidecar; nothing to clean")
	nfo.Addf("ROUTE", Pass, "skip: sidecar")
	nfo.Info = map[string]string{"route": "skip"}
	s.Append(nfo)

	exe := FileResult{Path: "/srv/incoming/readme.exe", Duration: 1 * time.Millisecond}
	exe.Addf("SIDECAR_BLOCKED", Block, "blocked sidecar type .exe")
	exe.Addf("ROUTE", Pass, "skip: sidecar")
	exe.Info = map[string]string{"route": "skip"}
	s.Append(exe)

	s.Error("/srv/incoming/gone.mkv: lstat /srv/incoming/gone.mkv: no such file or directory")
	s.Finished = s.Started.Add(4 * time.Second)
	return s
}
