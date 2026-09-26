# Changelog

All notable changes to amuxify will be documented in this file.

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