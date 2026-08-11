# amuxify

Strict MKV media-ingest tools for macOS.

`amuxify` scans, sanitizes, remuxes, and verifies media before archival storage while preserving retained video/audio streams without re-encoding.

> Use at your own risk. Keep original media until you have independently verified the generated output.

## Commands

- `scanmedia`
- `scanmediaAll`
- `remuxmedia`
- `cleanmedia`

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

This accepts names such as `s1e1`, `S01E01`, or `s01e02` and normalizes them to `S01E01.mkv`, `S01E02.mkv`, etc.

### 1. Scan source

`scanmediaAll ./dir`

Expect: no `BLOCKED` files.

### 2. Preview remux

`remuxmedia --dry-run ./dir`

Expect: shows what will be kept/dropped and may ask about untagged English audio/subtitles.

### 3. Remux

`remuxmedia ./dir`

Expect: creates `./dir__remuxed` and ends with `PASSED`.

### 4. Clean remuxed files

`cleanmedia ./dir__remuxed`

Expect: metadata and xattrs cleaned.

### 5. Final scan

`scanmediaAll ./dir__remuxed`

Expect: clean pass. These are the files intended for archival/NAS copy.

### Optional full decode verification

`scanmediaAll --deep ./dir__remuxed`

## License

BSD 3-Clause License.
