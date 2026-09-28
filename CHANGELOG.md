# Changelog

All notable changes to amuxify will be documented in this file.

## 0.4.0

### Added

- HDR and Dolby Vision assertions. The remux verifier reads the colour
  primaries, transfer characteristic, matrix coefficients and range, the
  mastering display chromaticity and luminance, the content light levels and
  the Dolby Vision configuration record of every kept video stream from both
  ffprobe and `mkvmerge -J`, and compares them between the source and the
  output. A value that was lost, gained or changed fails the file with
  `HDR_LOST`, names the value, and deletes the output, as a stream hash
  mismatch does. An SDR source whose output gained HDR signalling fails the
  same way. Chromaticity and luminance are compared with a tolerance of one
  part in a million, which covers the single-precision rounding of the
  Matroska header and nothing wider. Every number is validated before it is
  trusted, so a malformed probe result is recorded as malformed rather than
  compared, and a clean value from one tool never stands in for a malformed
  one from the other. The scan `HDR` finding lists the values it read. The
  fixture corpus gains an HDR10 and an HLG file.
- `--jobs <n>`: `scan`, `remux`, `clean` and `ingest` process up to n files at
  the same time, from 1 to 64. The default of 1 behaves exactly as before and
  a value outside the range is a usage error. Each file's lines are printed
  the moment that file finishes, so they appear in completion order, while
  the JSON report and the count line keep walk order. A parallel run makes
  the same decisions as a sequential one: two names of one inode, two sources
  that would rebuild to one output and two blocked files that would
  quarantine under one name are never processed at the same time, and a
  run-wide claim set makes sure that a collision between workers leaves
  exactly one output and reports `OUTPUT_EXISTS` or a failed quarantine for
  the other. `remux` scans every file of a tree before it rebuilds any,
  whatever the job count, so the second name of a hard-linked pair is
  reported with the links it had when the run began. An interrupt starts no
  further file, waits for the files already running, and leaves no temp file
  behind. The hook adapters accept the flag and always process one file at a
  time, because the download client decides how many scripts run at once.
- `amuxify watch <dir>`: polls one directory and runs `ingest` on each
  regular file once its size, modification time and identity have stayed
  unchanged for `--settle` (default 30s), checking every `--interval`
  (default 5s). It takes the ingest flags, uses the same walker, quarantine
  exclusion and symlink rules, and hands files to `ingest` one at a time. A
  file is ingested once per version; a file that grows, is rewritten or is
  replaced under the same name is ingested again once it settles, including
  a file amuxify itself rebuilt. Right before each ingest the path is checked
  again to be the same regular file, still inside the watched directory and
  reached through real directories. Under `--json` every pass that did
  something writes one `amuxify.report/1` document on one line. On SIGINT or
  SIGTERM the watcher finishes the file it is working on, with the tool that
  is rebuilding or editing it left to run to its end under its usual timeout,
  starts no further file and exits 0. `--once` makes one pass, waits one
  settle window, makes a second pass and exits with the worst verdict, so
  `--settle 0s` is the cron form. The quarantine directory, a directory
  inside it and a symlink are refused as the watched directory, and
  `--verify none` is refused. The Docker compose example gains a watcher
  service with a `stop_grace_period`.
- ClamAV: `doctor` adds an informational `clamav-db` row under `clamscan`
  with the signature database version, date and age, suggests `freshclam`
  when the signatures are more than a week old or absent, and exits 2 when
  the active profile requires the scan and no database has been downloaded.
  The `CLAMAV_INFECTED` and `CLAMAV_ERROR` details carry only the lines of
  clamscan output that name the scanned file, at most eight of them, passed
  through the report sanitiser; the verdict comes from the exit status alone,
  so text in the output cannot change it. ClamAV scans run one at a time
  whatever the job count, because every `clamscan` start loads the whole
  signature database. docs/profiles.md and docs/install.md gained ClamAV
  sections.
- The tool runner keeps at most 16 MiB of what any tool prints to each of
  standard output and standard error, so a flooding tool cannot grow the
  process, and a cut output is never taken for the whole: a text subtitle
  track longer than the bound is reported as not fully checked with
  `LINK_IN_SUBS` at the profile's link severity, and a cut ffprobe or
  `mkvmerge -J` document names the cut as the reason the file is unparseable.

### Changed

- Under `safety.clamav = required` (the strict profile) a scanner that gives
  no verdict, because it timed out, could not start or exited with an error,
  is `FAIL CLAMAV_ERROR` and the file is not probed or imported. It was
  `WARN CLAMAV_ERROR`, which let a short `--timeout` or a clamscan without a
  signature database turn a required scan into a pass. Under an optional
  scan it stays a warning and the file is still probed and verified.
- clamscan runs under the same `--timeout` as every other tool, with a
  default of 30 minutes; a scan that runs past the deadline is killed and
  reported as `CLAMAV_ERROR`.
- The SABnzbd adapter accepts exactly the argument forms SABnzbd produces.
  The job is read from the environment whenever `SAB_COMPLETE_DIR` is set,
  and `SAB_PP_STATUS` must be set with it; a job without a status now exits
  2 where it was treated as successful. Positional parameters are accepted
  only as SABnzbd's seven or eight parameters in SABnzbd's order, and the
  status must not be empty. A lone directory, any other count, and a
  directory written in front of an older SABnzbd's seven parameters are
  usage errors that name the argument; a bare `--quarantine` followed by a
  stray argument says to write `--quarantine=<dir>`. The eighth parameter,
  the failure URL, is never inspected, because SABnzbd copies it from the
  indexer's `X-DNZB-Failure` header and a check on its value would let an
  indexer have its jobs refused before the scan. Operators with
  `script_can_fail` on should note that a job started with
  `SAB_COMPLETE_DIR` but no `SAB_PP_STATUS` now fails as a usage error.
- The `LINK_IN_TAG` finding lists tag hits in a fixed order instead of the
  order the tag map happened to be visited in.
- The Homebrew cask clears the macOS quarantine attribute through a
  `postflight_steps` stanza instead of the `postflight` block that Homebrew 7
  reports as deprecated.
- The documentation states that a native Windows build is not planned,
  because the safety guarantees rest on POSIX file identity; Windows users
  run the Docker image through Docker Desktop or the Linux binary under WSL.

### Fixed

- A named pipe planted at a path amuxify opens could stall a run for as long
  as the planter liked, because opening a pipe waits for a peer. Every open
  of an input file, a sidecar, the quarantine source and destination, and
  the temp file after an external tool has written to it is now made without
  blocking and without following a link, and the entry is refused unless it
  is a regular file. A pipe under a media or sidecar name is reported as
  `FAIL UNREADABLE` by scan, remux and ingest and as `FAIL CLEAN_FAIL` by
  clean, and a pipe swapped onto the temp name after mkvmerge or ffmpeg
  returns is caught before any further read of that name. The flush before
  placement goes through the descriptor held since the temp file was created
  rather than through its name.
- On Linux the in-place remux set the output's modification time through
  `/proc/self/fd`, so in a container without `/proc` the file kept a fresh
  time and the identity guarantee failed silently. The time is now set with
  `utimensat` on the descriptor first, then through `/proc/self/fd`, and by
  the file's own name only after a check that the name still leads to the
  open file, with a call that never follows a symbolic link. An in-place
  remux or MP4 rewrite whose modification time cannot be set now fails
  before the rename, with the source left under its own name. Ownership
  remains best effort.
- Guarantees 2, 3 and 6 in docs/safety.md are backed by new adversarial
  tests: a cancelled mkvmerge and an interrupted watcher leave no temp file,
  pipes at every name a run opens, and the timestamp fallbacks.

## 0.3.0

### Added

- `amuxify ingest`: scan, then rebuild into a verified MKV or clean in place, one
  pass per file, always in place, with `--force`, `--hardlinks`, `--verify`,
  `--quarantine`, `--remove-blocked-sidecars` and `--original-language`. A
  hard-linked file is never edited in place. New finding `ROUTE`.
- `amuxify hook sabnzbd | nzbget | sonarr | radarr`: adapters that read the
  caller's environment, run ingest and exit the caller's way (0/1 or 93/94/95),
  with `--fail-on warn|fail|block`, `--category` and `--json-out`. The Sonarr and
  Radarr Test button runs a tool check. Wrapper scripts live under
  `contrib/hooks/` and in the Docker image.
- `--quarantine` without a value uses `<state-dir>/quarantine` for `ingest` and
  the hook adapters; `scan --quarantine <dir>` is unchanged.
- Kodi NFO awareness: allowed `.nfo` sidecars are classified (`NFO_KODI`,
  `NFO_TEXT`); links outside scraper and artwork fields are `LINK_IN_SIDECAR`
  (WARN or FAIL per `metadata.links`). NFO files are never modified.
- Report schema `amuxify.report/1` with a `schema` field and an optional `hook`
  object; golden and reflection tests freeze it; `docs/report.md` states the
  compatibility policy.
- A named test for every guarantee in docs/safety.md, and integration tests over
  the fixture corpus that skip when tools are missing and fail in CI.
- Hardening: every directory in a quarantine or output path is checked for
  symlinks before a file is placed under it; the tools run with a UTF-8 locale
  and English messages so that their output is read the same everywhere; decode verification checks
  the streams ffprobe reported rather than a fixed set; cleaning an MP4 twice
  leaves it unchanged; a tool named by a `tools.*` override must be executable
  or `doctor` reports it; a cancelled or timed-out run reports the cancellation
  as such instead of a tool failure; `amuxify help` lists the global flags;
  `ingest` skips a symlink with a warning and never follows it.
- The terminal report escapes control characters (other than tab) and Unicode
  bidirectional, zero-width and line separator controls in paths, messages,
  details and the hook adapters' own lines as `\xNN` or `\uNNNN`, so a file
  name cannot forge or overwrite a line. The JSON report carries the raw value.

### Changed

- JSON: `started` and `finished` are UTC with whole seconds; `files`, `findings`
  and `errors` are `[]` rather than absent or `null`; `profile` is always present.
- `docs/report.md` code table corrected to match the code (`HARDLINKED` is PASS in
  scan, `NO_VIDEO` and `NO_AUDIO` are FAIL, `QUARANTINED` is BLOCK on success,
  `XATTR` warns on failure, `DOUBLE_EXT` fires on a blocked penultimate extension).

### Fixed

- The `--timeout` help text listed the wrong probe default.
- Review fixes. An in-place rebuild places its output through the same
  no-clobber path as every other write and never overwrites a file at the
  destination. An `ingest` run whose root is a single file quarantines that
  file correctly. A quarantine move across devices copies the file safely
  instead of failing or leaving two copies. A directory that cannot be read
  fails the run instead of being silently skipped. A hook run
  under `--json` writes exactly one JSON document to stdout and its own log
  lines to stderr, reports an interruption on stderr, and refuses a positional
  argument the adapter does not take, with a hint when a directory was written
  after a bare `--quarantine`. The SABnzbd adapter accepts exactly seven or
  eight positional parameters, so a directory written after a bare
  `--quarantine` in front of SABnzbd's own parameters is refused with the same
  hint instead of being silently ignored. The NZBGet wrapper honours `AMUXIFY_PROFILE`
  behind its own `NZBPO_PROFILE` option like the other wrappers. The release
  workflow tags the image `0.3.0` as well as `v0.3.0`, and the derived
  `Dockerfile.sabnzbd` installs the wrapper under
  `/usr/local/share/amuxify/hooks/`, where a `/config` mount cannot hide it.
- Verification fixes. An in-place rebuild checks, right before it replaces or
  removes the source, that the file at that path is still the one it rebuilt
  and verified, and refuses with `REMUX_FAIL` when the file was swapped while
  mkvmerge ran; the temp file is created by amuxify itself and checked after
  mkvmerge returns and again before placement, so a symlink planted at the
  temp name is never followed. The entry placed at the destination is checked
  to be the file the run built, and the ownership, mode and time copy onto an
  in-place output, in `remux` and in the MP4 rewrite of `clean`, goes through
  the open descriptor of that file rather than through its name, so a symlink
  swapped onto the temp name after the last check cannot have its target's
  mode or time rewritten; a failed replacement in the MP4 rewrite is now
  reported as `CLEAN_FAIL` instead of being lost. A dry run of `remux`
  predicts the `OUTPUT_EXISTS` collision two files of one run produce when
  they rebuild to the same destination, as an `ingest` dry run already did,
  and the prediction treats `Ep.mkv` and `ep.mkv` as one entry on a
  case-insensitive filesystem. A placement failure in output mode that is not
  an existing destination is reported as `REMUX_FAIL` with the reason instead
  of `OUTPUT_EXISTS`. A quarantine move on the same filesystem now refuses a
  source that is not a regular file and checks that the entry it placed is the
  very file it started with, so a symlink swapped in during the move can never
  leave a hard link to its target in the quarantine tree; an entry that fails
  that check is removed only when it is not the last name of a file. A
  quarantine directory that sits inside the scanned tree is skipped by the
  walk, and a path that is the quarantine directory or lies inside it is
  refused as a usage error before anything runs, so a file is quarantined once
  rather than moved a level deeper on every run; both checks compare
  directories by identity, so another spelling or letter case of the same
  directory does not defeat them. The human report sanitiser and the
  `BIDI_NAME` check now cover every Unicode format character, including U+061C
  and the tag characters, rather than a fixed list of code points.
- The temp file that a rebuild or an MP4 rewrite creates stays open until the
  run's last identity check has passed. An open descriptor keeps the inode
  allocated, so a filesystem that reuses a freed inode number at once, as ext4
  does, cannot give that number to a file swapped onto the temp name, which
  would otherwise pass as the run's own.

## 0.2.0

Rewrite in Go. One binary, four built-in profiles, every common container as
input, mkvmerge as the only MKV writer. The repository moved to
`github.com/amuxify/amuxify` (module path `github.com/amuxify/amuxify`).

### Added

- `amuxify scan | remux | clean | doctor | profile` subcommands with shared
  `--profile`, `--json`, `--dry-run`, `--verbose`, `--quiet`, `--timeout`,
  `--state-dir`, `--allow-root`, `--trace` flags.
- Profiles as TOML: `homelab` (default), `anime`, `archive` (the 0.1.x rules),
  `strict`. Custom profiles overlay the defaults; unknown keys are rejected.
- Input containers: MKV, WebM, MP4, M4V, MOV, AVI, MPEG-TS, M2TS, MPG, VOB, FLV.
- Link detection in titles, tags, chapter names, attachment names and text
  subtitle tracks (`LINK_IN_TAG`, `LINK_IN_SUBS`).
- MP4 provenance atom detection (`PURCHASE_ATOM`): `ownr`, `apID`, `purd`,
  `xid `, `cprt`, `©too`, `©enc`, XMP boxes and others; `clean` removes them
  and re-checks the result.
- Attachment policy: fonts allowed with text subtitles, cover art by profile,
  attachment payloads sniffed for executables and archives (`ATTACH_EXEC`).
- Sidecar policy with allow and block lists (`SIDECAR_BLOCKED`, `SIDECAR_UNKNOWN`).
- Truncation detection for MP4 (`TRUNCATED`), HDR and Dolby Vision preservation
  check after remux (`HDR_LOST`).
- Hard-link handling (`safety.hardlinks = skip | break | copy`), per-file
  symlink skipping, root refusal, owner/mode/mtime preservation in place.
- Stable exit codes 0/1/2/3/4/130 and a JSON report.
- `doctor` with version floors (MKVToolNix 50, ffmpeg 4.4).
- Static release binaries for Linux and macOS, Docker image at
  `ghcr.io/amuxify/amuxify`, Homebrew cask in `amuxify/homebrew-tap`,
  checksum-verified `install.sh`.

### Changed

- Default policy keeps all languages, chapters and fonts; strips titles, tags
  and provenance. The 0.1.x English-only behaviour is `--profile archive`.
- Untagged-language tracks never prompt; `languages.und` decides.
- Extended-attribute removal is limited to `user.*` and `com.apple.*`.

### Removed

- The `amux-scan`, `amux-scan-all`, `amux-remux` and `amux-clean` commands.
  The command mapping is in the README; the scripts remain in the `v0.1.1` tag.
- ffmpeg as MKV writer; `--strict-permissions`, `--allow-data-tag`, `--deep`
  flags (now profile keys and `--verify full`); the exiftool hard dependency;
  `ulimit`/`gtimeout` requirements.


## 0.1.1

Safety release. No media-policy changes.

### Fixed

- `amux-remux`: untagged-track prompts no longer hang forever when stdin is not a terminal. Without a terminal the decision comes from `AMUXIFY_UND_POLICY` (`drop` by default, or `english`). Closed stdin during a prompt exits 130 instead of spinning.
- `amux-remux`: `mkvmerge -J` exit status 1 (warnings) no longer fails output verification; only status 2 or empty output does.
- `amux-clean`: explicit exit status (1 when any file failed, 0 otherwise) and INT/TERM handling.
- All commands: arguments after `--` are honoured.

### Documentation

- README documents exit codes and unattended runs.

## 0.1.0

Initial release.

### Added

- `amux-scan` for strict pre-ingestion media safety scanning.
- `amux-scan-all` with strict permissions and legacy `text` data-stream allowance.
- Optional full FFmpeg decode verification with `--deep`.
- Optional ClamAV scanning with `--clamav`.
- Suspicious extension, MIME, symlink, attachment, data-stream, filename-spoofing and polyglot checks.
- `amux-remux` for conservative MKV archival remuxing.
- Stream-copy preservation of retained video, English audio and English subtitles.
- Interactive handling of untagged audio and subtitle streams.
- Removal of attachments, data streams, chapters, tags, track titles and old Segment UID.
- Container muxing/writing application sanitization.
- SHA-256 verification of retained video, audio and subtitle streams.
- Beginning/end decode verification before output finalization.
- `amux-clean` for writable metadata and macOS extended-attribute cleanup.
- Recursive directory processing.
- Dry-run support.
- Public `--help` and `--version` interfaces.
- Makefile-based setup, testing, installation and release checks.
