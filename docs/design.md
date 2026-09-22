# Design notes

## Why this exists

The first amuxify was three Bash scripts written for one macOS machine and one
problem: media bought or downloaded from untrusted storefronts arrived with
tracker URLs in tags, purchase identifiers in MP4 atoms, fonts that were not
fonts, and sometimes appended archives. The scripts verified, stripped and
remuxed, and refused anything suspicious. The engineering was careful (temp
then rename, stream hashing, protocol whitelist) but the policy was personal:
English only, chapters dropped, every attachment blocked, interactive prompts.

0.2 keeps the careful part and makes the policy a document.

## Principles

1. **Gate, not pipeline.** One pass, one verdict, no re-encoding, no library
   mutation beyond the file being processed.
2. **Prove, do not trust.** mkvmerge is excellent, but the output is still
   hashed against the source stream by stream and decoded at both ends.
3. **Security is a verdict, not a log line.** BLOCK is its own exit code and
   nothing overrides it.
4. **Never prompt.** Every decision has a profile key. A hook run behaves
   exactly like a terminal run.
5. **The tools do the media work.** mkvmerge writes Matroska, ffmpeg hashes and
   decodes. amuxify decides, verifies and reports. No media parsing beyond
   magic bytes and an MP4 box walker that reads, never writes.
6. **Platform-independent by construction.** Pure Go with three small
   dependencies, static binaries, no vendor CLIs in the build.

## Package map

- `cmd/amuxify` entry point.
- `internal/cli` flag parsing and subcommand dispatch.
- `internal/report` verdicts, findings, exit codes, human and JSON output.
- `internal/exec` tool lookup, timeouts, environment, ffmpeg guard flags.
- `internal/sniff` magic-byte detection.
- `internal/fsutil` temp names, no-clobber placement, in-place replacement.
- `internal/mp4` read-only ISO BMFF walker for provenance and truncation.
- `internal/probe` ffprobe and `mkvmerge -J` merged into one `MediaInfo`.
- `internal/policy` profiles and the pure `Decide()` function.
- `internal/verify` stream hashes and decode checks.
- `internal/scan`, `internal/remux`, `internal/clean`, `internal/doctor`.
- `legacy/` the frozen Bash scripts; `test/` the differential test.

## Decision flow for remux

scan → refuse on BLOCK, refuse on FAIL unless `--force` → `policy.Decide`
(tracks, flags, languages, attachments, chapters, strips) → mkvmerge into a
temp file → mkvpropedit for provenance fields → probe the output → structural
assertions (counts, chapters, tags, title, muxing app, HDR) → per-stream hash
equality (packet hash, or decoded-frame hash across container families) → head
and tail decode (or full) → fsync → place without clobber, or replace in place
preserving identity.

## Why not ffmpeg for Matroska

ffmpeg's Matroska muxer drops IETF language tags, reorders and sometimes drops
Dolby Vision and HDR10+ configuration records, cannot carry attachments through
a stream copy, and rewrites chapter structure. mkvmerge is the reference
implementation. ffmpeg still does the MP4, MOV and AVI metadata rewrite for
`clean` because those are its native containers.

## Roadmap

0.3: `ingest` (scan + remux + clean in one call), hook adapters for SABnzbd,
NZBGet, Sonarr and Radarr, frozen JSON schema. 0.4: Windows, macOS
notarization, parallel full verification. 1.0: fixture matrix complete,
`legacy/` removed.
