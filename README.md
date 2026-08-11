# amuxify

Strict MKV media-ingest tools for macOS.

`amuxify` scans, sanitizes, remuxes, cleans, and verifies media before archival storage while preserving retained video, audio, and subtitle streams without re-encoding.

> Use at your own risk. Keep original media until you have independently verified the generated output.

## Commands

- `amux-scan`
- `amux-scan-all`
- `amux-remux`
- `amux-clean`

## Command overview

### `amux-scan`

Strict media safety scan.

Runs `scan-media.sh` with strict executable-permission checking enabled.

Example:

`amux-scan ./dir`

### `amux-scan-all`

Recommended scan command for the archival workflow.

Runs the same scanner with:

- strict executable-permission checking
- legacy `text` data-stream tags allowed in addition to the default `tmcd`

Example:

`amux-scan-all ./dir`

Optional deep decode:

`amux-scan-all --deep ./dir`

Optional ClamAV scan:

`amux-scan-all --clamav ./dir`

### `amux-remux`

Sanitizes MKV containers while stream-copying retained media.

Policy:

- MKV input only
- primary/default video only
- English audio only
- English subtitles retained in their original codec
- untagged audio/subtitles prompt for a decision
- non-English audio/subtitles dropped
- attachments dropped
- data streams dropped
- chapters dropped
- global and track tags dropped
- track titles dropped
- old Segment UID removed
- muxing/writing application values cleared
- retained video/audio/subtitle streams verified with SHA-256 stream hashes
- beginning and end of output decode-tested
- symlinks refused
- existing output never overwritten

Example:

`amux-remux ./dir`

Preview without creating files:

`amux-remux --dry-run ./dir`

### `amux-clean`

Removes writable filesystem and container metadata.

Behavior:

- recursively processes directories
- clears macOS extended attributes
- removes writable metadata with ExifTool
- cleans writable Matroska tags/titles with MKVToolNix
- ignores `.DS_Store`
- does not re-encode media

Example:

`amux-clean ./dir`

## Installation

### Manual installation

Clone the repository and install into a user-owned prefix:

`make install PREFIX="$HOME/.local"`

Make sure `$HOME/.local/bin` is on your `PATH`.

For zsh on macOS:

`export PATH="$HOME/.local/bin:$PATH"`

Then verify:

`amux-remux --version`

Installed commands:

- `amux-clean`
- `amux-remux`
- `amux-scan`
- `amux-scan-all`

To uninstall:

`make uninstall PREFIX="$HOME/.local"`

### Run without installing

From the repository root:

`make setup`

Then run commands directly:

`./bin/amux-remux --help`

### System-wide installation

The default prefix is `/usr/local`:

`make install`

Depending on directory ownership, this may require elevated permissions:

`sudo make install`

## Workflow

Optional: normalize episode filenames first.

Example:

`Filename (2000) - S01E01 - Title case.mkv`

Rename to:

- `S01E01.mkv`
- `S01E02.mkv`
- `S01E03.mkv`

Run from inside the folder:

`for f in *.mkv; do ep="$(printf '%s\n' "$f" | perl -ne 'if (/[sS](\d{1,2})[eE](\d{1,2})/) { printf "S%02dE%02d", $1, $2 }')"; [[ -n "$ep" ]] && mv -n -- "$f" "$ep.mkv"; done`

This accepts names such as `s1e1`, `S01E01`, or `s01e02` and normalizes them to `S01E01.mkv`, `S01E01.mkv`, or `S01E02.mkv`.

### 1. Scan source

`amux-scan-all ./dir`

Expect: no `BLOCKED` files.

### 2. Preview remux

`amux-remux --dry-run ./dir`

Expect: shows what will be kept or dropped and may prompt about untagged audio or subtitle streams.

### 3. Remux

`amux-remux ./dir`

Expect: creates:

`./dir__remuxed`

The remux process verifies:

- output parses with ffprobe
- output parses with MKVToolNix
- no attachments remain
- no chapters remain
- no global tags remain
- no track tags remain
- no track titles remain
- old Segment UID is removed
- muxing/writing application values are empty
- expected stream counts match
- no attachment/data streams remain
- retained video/audio/subtitle stream hashes match the source
- beginning and end decode successfully

A successful run ends with:

`PASSED: all created outputs passed verification.`

### 4. Clean remuxed files

`amux-clean ./dir__remuxed`

Expect: writable metadata and macOS extended attributes are cleaned.

### 5. Final scan

`amux-scan-all ./dir__remuxed`

Expect: clean pass.

These are the files intended for archival or NAS copy.

### Optional full decode verification

`amux-scan-all --deep ./dir__remuxed`

This fully decodes the media streams and is substantially slower than the normal scan.

## Provenance limitation

`amux-remux` removes and rebuilds container-level provenance.

Retained video, audio, and subtitle streams are stream-copied rather than re-encoded. Encoder-identifying information embedded inside encoded bitstreams may therefore survive the remux.

For example, HEVC produced by x265 may contain x265 build or encoding-option information inside codec headers or SEI data.

`amuxify` should therefore not be considered a bitstream-level provenance removal tool.

## Requirements

Runtime dependencies include:

- FFmpeg / ffprobe
- MKVToolNix
- ExifTool
- Perl

Optional:

- ClamAV for `--clamav`
- GNU coreutils for `gtimeout` on macOS

## Development

Create generated command wrappers and set executable permissions:

`make setup`

Run repository checks:

`make test`

Run release checks:

`make release-check`

Install manually under `/usr/local`:

`make install`

Override the install prefix if needed:

`make install PREFIX=/some/path`

Remove a manual installation:

`make uninstall`

## License

BSD 3-Clause License.