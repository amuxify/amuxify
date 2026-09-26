# Changelog

All notable changes to amuxify will be documented in this file.

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
  `ghcr.io/amuxify/amuxify`, checksum-verified `install.sh`.

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