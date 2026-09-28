# Design notes

## Why this exists

The first amuxify was three Bash scripts written for one macOS machine and one
problem: media bought or downloaded from untrusted storefronts arrived with
tracker URLs in tags, purchase identifiers in MP4 atoms, fonts that were not
fonts, and sometimes appended archives. The scripts verified, stripped and
remuxed, and refused anything suspicious. The engineering was careful (temp
then rename, stream hashing, protocol whitelist) but the policy was personal:
English only, chapters dropped, every attachment blocked, interactive prompts.

0.2 keeps the careful part and makes the policy a document. The Bash scripts
are not carried in this tree; they are in the `v0.1.1` tag.

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
- `internal/ingest` composes scan, remux and clean into one in-place pass with one result per file; the routing decision is a pure function.
- `internal/hook` maps a download client's or media manager's environment to an ingest job and a run verdict to that caller's exit code; it runs no tools.
- `internal/testutil` locates tools and fixtures for tests and skips cleanly when they are missing.

## Decision flow for remux

scan → refuse on BLOCK, refuse on FAIL unless `--force` → `policy.Decide`
(tracks, flags, languages, attachments, chapters, strips) → mkvmerge into a
temp file → mkvpropedit for provenance fields → probe the output → structural
assertions (counts, chapters, tags, title, muxing app, HDR) → per-stream hash
equality (packet hash, or decoded-frame hash across container families) → head
and tail decode (or full) → fsync → place without clobber, or replace in place
preserving identity.

The HDR assertion compares more than a label. The prober reads the colour
primaries, transfer characteristic, matrix coefficients and range, the
mastering display chromaticity and luminance, the content light levels and
the Dolby Vision configuration record from ffprobe's stream fields and side
data, and fills any gap from the `mkvmerge -J` track properties, into one
normalised `probe.Color` per video stream. Every number is validated as it is
read; a value that is not a finite number, is negative, or is beyond what the
format can express is recorded as malformed instead of trusted. The verifier
compares the source and output `Color` of every kept video stream and reports
each value that was lost, gained or changed in one `HDR_LOST` finding for the
first stream that differs. The comparison exists because this signalling lives
in the container header, outside the packets that the stream hashes cover, and
a muxer can drop or alter it without changing a hash.

The remuxer feeds that view back into the mkvmerge command line. mkvmerge's
MP4 reader carries the colour primaries, transfer and matrix from the `colr`
box, but not its range flag, and it does not read the mastering display
(`mdcv`) and content light (`clli`) boxes at all, so a plain remux of an HDR10
MP4 produces a Matroska file without them and the assertion fails it as
`HDR_LOST`. For every kept video track the argument builder therefore names
the range, the chromaticity and white point coordinates, the luminance bounds
and the content light levels with mkvmerge's colour options whenever the
source's ffprobe view holds a value that mkvmerge's own `-J` view of the same
track does not. A value mkvmerge already reads is left to mkvmerge, so a
Matroska source gets no colour options at all. Each number is written as a
plain decimal with at most twelve fractional digits, because mkvmerge rejects
exponent notation and misparses longer decimals, and only when that decimal
reads back within the verifier's tolerance; otherwise the group is left out
and the assertion reports the loss rather than passing a substitute. The
Dolby Vision configuration record has no mkvmerge option. mkvmerge 102 carries
the `dvcC` box from an MP4 source into a Matroska block addition mapping on
its own, the `dovi.mp4` fixture pins that, and an older mkvmerge that drops it
fails the file as `HDR_LOST` instead of passing silently.

## Decision flow for ingest

ingest is scan plus one of remux or clean, never both on the same file, and a
hard-linked file is never edited in place. After scan, a media file that is not
BLOCK (and not FAIL unless `--force` is given) goes through `policy.Decide` and
then the routing rules below, checked in this order. The first matching rule
wins; the route and its reasons are recorded as the `ROUTE` finding.

| # | Condition | Route | Findings added | Reason text |
|---|---|---|---|---|
| 1 | the decision contains a finding of severity FAIL or worse (`NO_VIDEO`) and no `--force` | skip | the decision's findings | `decision failed` |
| 2 | the scan verdict is FAIL and `--force` is given | remux | none here; remux adds them | `scan verdict FAIL; rebuilt because --force was given` |
| 3 | more than one hard link and the file is audio or subtitle | skip | `HARDLINKED` WARN plus the decision's findings | `hard-linked audio or subtitle file` |
| 4 | more than one hard link and the hard-link mode is `skip` | skip | `HARDLINKED` WARN plus the decision's findings | `hard-linked` |
| 5 | more than one hard link and the mode is `break` or `copy` | remux | none | `hard-linked; rebuilt rather than edited in place (safety.hardlinks = <mode>)` |
| 6 | the file is audio or subtitle | clean | the decision's findings | `audio or subtitle container; metadata only` |
| 7 | the extension is not `mkv` or the container is not Matroska | remux | none | `container <name> is rebuilt as Matroska` |
| 8 | scan reported `MKV_WARNING` | remux | none | `mkvmerge reported warnings; rebuilding` |
| 9 | tracks, flags, languages, attachments or chapters differ from the decision | remux | none | the list of differences |
| 10 | otherwise | clean | the decision's findings | `tracks, flags, attachments and chapters already match the profile` |

The clean route decodes the file first (head and tail for `quick`, everything for
`full`) because mkvpropedit edits headers in place; a file that does not decode is
`DECODE_FAIL` and left untouched. The remux route does not decode the input twice:
the scanner runs without a verify tier and the remuxer verifies its output. The
same file can take different routes under different profiles; `multi.mkv` in the
fixture corpus is cleaned under `homelab`, while under `archive` its embedded links
are a failure, so it is refused unless `--force` is given, and then it is rebuilt.

## Why not ffmpeg for Matroska

ffmpeg's Matroska muxer drops IETF language tags, reorders and sometimes drops
Dolby Vision and HDR10+ configuration records, cannot carry attachments through
a stream copy, and rewrites chapter structure. mkvmerge is the reference
implementation. ffmpeg still does the MP4, MOV and AVI metadata rewrite for
`clean` because those are its native containers.

## Roadmap

0.3: `ingest`, hook adapters for SABnzbd, NZBGet, Sonarr and Radarr, frozen
report schema `amuxify.report/1`, Kodi NFO awareness, a test for every
guarantee. 0.4 (this release): HDR and Dolby Vision assertions, `--jobs` for
parallel processing, `watch` for set-ups where the hook cannot run inside the
client container, ClamAV polish, and the fixes carried over from 0.3. 0.5: the
module path moves to `amuxify.com/amuxify`, served by a static page on the
project site, so the import path no longer names the hosting provider; macOS
notarization once an Apple developer account exists, which also removes the
quarantine step from the Homebrew cask. 0.6: the website redone, with these
docs rendered on amuxify.com from the files in this directory, a better
design, and a landing page that describes the local workflow first (download
or buy, scan, clean or remux, then move to the library) and the download-client
hooks second; no changes to the binary. A native Windows build is not planned:
the guarantees rest on POSIX file identity, and Windows users run the Docker
image through Docker Desktop or the Linux binary under WSL (see
[install.md](install.md)). 1.0: fixture matrix complete, exit codes and the
report schema declared stable, `legacy/` removed.
