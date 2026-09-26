# Changelog

All notable changes to amuxify will be documented in this file.

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
  after a bare `--quarantine`. The NZBGet wrapper honours `AMUXIFY_PROFILE`
  behind its own `NZBPO_PROFILE` option like the other wrappers. The release
  workflow tags the image `0.3.0` as well as `v0.3.0`, and the derived
  `Dockerfile.sabnzbd` installs the wrapper under
  `/usr/local/share/amuxify/hooks/`, where a `/config` mount cannot hide it.
  A quarantine move on the same filesystem now refuses a source that is not
  a regular file and checks that the entry it placed is the very file it
  started with, so a symlink swapped in during the move can never leave a
  hard link to its target in the quarantine tree; an entry that fails that
  check is removed only when it is not the last name of a file. A
  quarantine directory that sits inside the scanned tree is skipped by the
  walk, and a path that is the quarantine directory or lies inside it is
  refused as a usage error before anything runs, so a file is quarantined
  once rather than moved a level deeper on every run; both checks compare
  directories by identity, so another spelling or letter case of the same
  directory does not defeat them. The human report sanitiser and the
  `BIDI_NAME` check now cover every Unicode format character, including
  U+061C and the tag characters, rather than a fixed list of code points.

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
