# Profiles

A profile is a TOML document that answers every policy question amuxify asks.
Four are built in. A custom profile is a file with only the keys you want to
change; everything else comes from `homelab`. Unknown keys and invalid values
are rejected at load time, so a typo cannot silently widen a policy.

```sh
amuxify profile                  # list
amuxify profile show archive     # print the TOML
amuxify profile check my.toml    # validate
amuxify --profile my.toml scan .
export AMUXIFY_PROFILE=archive   # default for this shell
```

Precedence: command-line flags > profile > built-in `homelab` defaults.

## Built-in profiles

| | `homelab` (default) | `anime` | `archive` | `strict` |
|---|---|---|---|---|
| For | Jellyfin, Plex, Kodi libraries | fansub releases with ASS subtitles and fonts | English-only archive of purchased media (the 0.1.x rules) | archive plus full decode and antivirus |
| `languages.keep` | `["*"]` | `["*"]` | `["eng"]` | `["eng"]` |
| `languages.und` | keep | keep | drop | drop |
| `audio.drop_commentary` | no | yes | yes | yes |
| `chapters.keep` | yes | yes | no | no |
| `attachments.fonts` | keep if text subs | keep | drop | drop |
| `attachments.cover_art` | keep | keep | drop | drop |
| `metadata.strip_track_titles` | no | no | yes | yes |
| `metadata.links`, `provenance` | warn | warn | fail | fail |
| `safety.exec_permissions` | warn | warn | fail | fail |
| `safety.clamav` | off | off | optional | required |
| `verify.tier` | quick | quick | quick | full |
| sidecars | nfo, txt, images allowed | same plus ttf, otf, ttc | nfo, txt, html blocked | same as archive |

## Keys

### `[languages]`

- `keep` — list of languages to keep for audio and subtitles. `["*"]` keeps
  all. Accepts ISO 639-1, 639-2/B, 639-2/T and common names (`en`, `eng`,
  `english`, `ger`, `deu`, `german`, `ja`, `jpn` ...). Canonical form is ISO 639-2/B.
- `prefer_original` — when the caller passes `--original-language` (or the
  Sonarr and Radarr hook adapters supply it from the series or movie record),
  the first audio track in that language becomes the default track.
- `und` — what to do with tracks that have no language tag: `keep`, `drop`, or
  `assume:<lang>` which tags them (for example `assume:eng`) and then applies
  `keep`. amuxify never prompts.

The video track is never dropped for language. If the policy would remove every
audio track, all audio is kept and the file gets `WARN AUDIO_FALLBACK`; a file
is never silently made silent.

### `[audio]`

- `drop_commentary` — drop audio tracks whose title matches the word
  "commentary" or "commentaries" (word-bounded, case-insensitive). Titles like
  "Director's Commentary" match; a film titled "The Commentator" does not.
- `keep_first` — keep only the first audio track that passes the language rule.

### `[subtitles]`

- `keep_forced` — keep forced-flag subtitle tracks even when their language is
  not in `keep` (they carry the foreign-dialogue translations).
- `drop_sdh_duplicates` — when a language has both a normal and an SDH track,
  drop the SDH one.
- `scan_links` — extract text subtitle tracks and look for URLs and domains.
- `links` — `warn` or `fail` when a link is found in subtitle text
  (`LINK_IN_SUBS`). amuxify reports; it does not rewrite subtitle text.

### `[chapters]`

- `keep` — carry chapters into the output. Chapter names are scanned for links.

### `[attachments]`

- `fonts` — `keep`, `drop`, or `keep_if_text_subs` (keep only when the file has
  an SSA/ASS/SRT/WebVTT track that could reference them). Font payloads are
  sniffed; a "font" that is a PE, ELF, Mach-O or archive is `BLOCK ATTACH_EXEC`.
- `cover_art` — `keep` or `drop` for JPEG, PNG and WebP attachments.
- `other` — `block` (default; the file is `BLOCK ATTACH_BLOCKED`) or `drop`
  (remux removes it and reports `ATTACH_DROP`).

### `[metadata]`

- `strip_title` — clear the container title.
- `strip_tags` — remove global and track tags (statistics tags are regenerated
  by mkvmerge only when this is off).
- `strip_track_titles` — clear track names.
- `strip_provenance` — clear muxing and writing application, segment date, MP4
  `ilst` purchase and identifier atoms, XMP boxes.
- `links` — `warn` or `fail` for links found in title, tags, chapter names or
  attachment names (`LINK_IN_TAG`) and in `.nfo` sidecars (`LINK_IN_SIDECAR`).
- `provenance` — `warn` or `fail` when identifying atoms are present
  (`PURCHASE_ATOM`). Plain encoder fingerprints are reported at PASS level as
  `PROVENANCE_INFO` and only shown with `--verbose`.

### `[streams]`

- `allow_data_tags` — data-stream codec tags that are not `BLOCK DATA_STREAM`.
  `tmcd` (QuickTime timecode) and `text` (legacy tx3g) are allowed by default.

### `[verify]`

- `tier` — `quick` (decode first and last 15 seconds), `full` (decode
  everything), `none`. `none` is refused with `remux --in-place`.
- `stream_hash` — after remux, hash every kept stream in source and output and
  require equality. Off only for experiments; the safety guarantee depends on it.

### `[safety]`

- `refuse_root` — refuse to modify files as root unless `--allow-root`.
- `hardlinks` — for a source with more than one hard link when writing in
  place: `skip` (WARN and leave it, torrent-safe), `break` (replace this name
  only), `copy` (write to the output tree instead).
- `exec_permissions` — `ignore`, `warn` or `fail` when a media file has the
  executable bit. FAT and exFAT mounts show every file as executable; use
  `ignore` there.
- `clamav` — `off`, `optional` (scan when clamscan is installed), `required`
  (`doctor` fails without it; infected files are `BLOCK CLAMAV_INFECTED`).
  See the section on ClamAV below for what each value means at run time.

### ClamAV

When the scan is on, every media file is handed to `clamscan` after the
content checks and before it is probed. The verdict comes from the exit
status of clamscan alone: exit 0 is clean, exit 1 is `BLOCK CLAMAV_INFECTED`,
and any other exit, a scanner that cannot start or one that runs past the
timeout is `WARN CLAMAV_ERROR`, after which the file is probed and verified
like any other. What clamscan printed is kept only as the lines that name the
scanned file, cut to a few lines and passed through the report sanitiser, so
a scanner's output can neither raise nor lower a verdict nor reshape a
report line.

The three values of `safety.clamav` and the `scan --clamav` flag combine as
follows.

| `safety.clamav` | without `--clamav` | with `--clamav` | clamscan missing |
|---|---|---|---|
| `off` | ⏭️ not run | ▶️ run | ⏭️ carries on |
| `optional` | ▶️ run | ▶️ run | ⏭️ carries on |
| `required` | ▶️ run | ▶️ run | ⛔ `FAIL CLAMAV_MISSING` |

`--clamav` turns an `off` profile into an optional scan for that run; it never
lowers a `required` profile, and no flag turns a `required` profile's missing
scanner into a pass. With `required` the file is failed before it is probed,
`ingest` and the hook adapters refuse it even with `--force`, and `doctor`
exits 2. The clamscan call runs under the run's `--timeout` when one is
given and under a 30 minute limit otherwise.

### `[sidecars]`

Files in the scanned tree that are not media.

- `allow` — extensions reported as `PASS SIDECAR_OK`.
- `block` — extensions reported as `BLOCK SIDECAR_BLOCKED`; `clean
  --remove-blocked-sidecars` deletes them.
- Anything else is `WARN SIDECAR_UNKNOWN`.

An allowed `.nfo` is classified as Kodi XML, Kodi URL, mixed or plain text
(`NFO_KODI`, `NFO_TEXT`). Scraper URLs and artwork URLs are never reported as
links. amuxify never rewrites an NFO file.

## Example custom profile

```toml
# ~/.config/amuxify/mine.toml: homelab, but German and English only, no commentary
description = "German and English, no commentary"

[languages]
keep = ["deu", "eng"]
und = "assume:eng"

[audio]
drop_commentary = true
```
