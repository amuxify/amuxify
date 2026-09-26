# Report format and finding codes

Every amuxify command writes the same report. Without `--json` the report is
printed for a terminal, one line per file as soon as that file is finished,
followed by the run-level errors and a count line. With `--json` nothing is
printed until the run is over and then exactly one JSON object followed by a
newline is written to stdout. Errors and diagnostics go to stderr in both
modes, so stdout can be piped straight into a parser. `--quiet` prints nothing
and leaves only the exit code.

This is the report of an `amuxify ingest` run started by the SABnzbd hook
adapter. It is the file `internal/report/testdata/report-golden.json`, which a
test compares byte for byte with what the code writes, so what you see here is
exactly what a script receives.

```json
{
  "schema": "amuxify.report/1",
  "tool": "amuxify",
  "version": "0.3.0",
  "command": "ingest",
  "profile": "homelab",
  "hook": {
    "adapter": "sabnzbd",
    "event": "pp status 0",
    "label": "Sample.Movie.2026.1080p",
    "category": "movies",
    "fail_on": "FAIL",
    "exit_code": 1
  },
  "started": "2026-09-22T10:00:00Z",
  "finished": "2026-09-22T10:00:04Z",
  "verdict": "BLOCK",
  "counts": {
    "BLOCK": 1,
    "PASS": 1,
    "WARN": 1
  },
  "files": [
    {
      "path": "/srv/incoming/movie.mkv",
      "verdict": "WARN",
      "findings": [
        {
          "code": "LINK_IN_TAG",
          "severity": "WARN",
          "message": "1 link(s) in metadata: tag COMMENT: http://x.example",
          "detail": "tag COMMENT: http://x.example"
        },
        {
          "code": "HASH_OK",
          "severity": "PASS",
          "message": "2 stream(s) verified identical to source"
        },
        {
          "code": "PLACED",
          "severity": "PASS",
          "message": "written and verified"
        },
        {
          "code": "ROUTE",
          "severity": "PASS",
          "message": "remux: #1 default flag false -\u003e true"
        }
      ],
      "output": "/srv/incoming__remuxed/movie.mkv",
      "actions": [
        "keep #0 video h264 und: primary video",
        "keep #1 audio aac eng default: language eng"
      ],
      "info": {
        "container": "matroska",
        "kind": "matroska",
        "route": "remux"
      },
      "duration_ms": 812
    },
    {
      "path": "/srv/incoming/movie.nfo",
      "verdict": "PASS",
      "findings": [
        {
          "code": "SIDECAR_OK",
          "severity": "PASS",
          "message": "allowed sidecar .nfo"
        },
        {
          "code": "NFO_KODI",
          "severity": "PASS",
          "message": "Kodi movie nfo"
        },
        {
          "code": "NOTHING_TO_CLEAN",
          "severity": "PASS",
          "message": "sidecar; nothing to clean"
        },
        {
          "code": "ROUTE",
          "severity": "PASS",
          "message": "skip: sidecar"
        }
      ],
      "info": {
        "route": "skip"
      },
      "duration_ms": 3
    },
    {
      "path": "/srv/incoming/readme.exe",
      "verdict": "BLOCK",
      "findings": [
        {
          "code": "SIDECAR_BLOCKED",
          "severity": "BLOCK",
          "message": "blocked sidecar type .exe"
        },
        {
          "code": "ROUTE",
          "severity": "PASS",
          "message": "skip: sidecar"
        }
      ],
      "info": {
        "route": "skip"
      },
      "duration_ms": 1
    }
  ],
  "errors": [
    "/srv/incoming/gone.mkv: lstat /srv/incoming/gone.mkv: no such file or directory"
  ]
}
```

## Stability

The report schema is `amuxify.report/1` and is stable from 0.3.0 on. Within
schema 1, amuxify will only ever add fields, add `info` keys and add finding
codes. It will not rename or remove a field, change a field's type, change the
meaning or spelling of a verdict token, or reassign a finding code. Message
and detail text are written for people and may change in any release; parse
the code and the severity, not the message. Timestamps are RFC 3339 in UTC
with whole seconds. Arrays are present and empty rather than absent or null.
The optional `hook` object is present only for runs started by `amuxify
hook`. A consumer that ignores unknown fields, unknown `info` keys and unknown
codes keeps working across every 0.x release. If a breaking change ever
becomes necessary the value becomes `amuxify.report/2` and both forms are
documented for at least one minor release. The example above is
`internal/report/testdata/report-golden.json`, which a test compares byte for
byte.

Two tests guard this promise. `TestFieldSetFrozen` in `internal/report` pins
the name, type and optionality of every field in declaration order, so a
change to the wire format cannot land by accident. `TestCodesDocumented` in
`internal/cli` parses the code base and this page and fails when a finding
code exists in one but not the other.

## Verdicts and exit codes

A verdict is one of five upper-case tokens. The process exit code is the
numeric value of the run verdict, and a worse verdict always wins: a run with
one BLOCK file exits 4 even when every other file passed, and a run-level
error raises the verdict to at least FAIL but never lowers it.

| Token | Exit code | Meaning |
|---|---|---|
| `PASS` | 0 | Nothing to report. |
| `WARN` | 1 | Something worth a look, but the file is usable and, for ingest, was placed. |
| `USAGE` | 2 | Bad arguments or an unusable environment. As a finding severity it appears only in `doctor --json`. |
| `FAIL` | 3 | The file failed a check or a step failed; nothing was written for it. |
| `BLOCK` | 4 | The file is dangerous. It is never modified or placed and no flag, profile key or environment variable lowers this. |

An interrupted run (SIGINT or SIGTERM) exits 130 when it had nothing worse to
report.

## Fields

The top-level object has these keys, in this order. The `command` value is one
of `scan`, `remux`, `clean`, `ingest` or `doctor`.

| Key | Type | Always present | Meaning |
|---|---|---|---|
| `schema` | string | yes | The schema identifier, `amuxify.report/1`. Check this first. |
| `tool` | string | yes | Always `amuxify`. |
| `version` | string | yes | The amuxify version that wrote the report. |
| `command` | string | yes | `scan`, `remux`, `clean`, `ingest` or `doctor`. |
| `profile` | string | yes | The profile name or path, or an empty string when none applied. |
| `hook` | object | only for `amuxify hook` runs | The adapter that started the run; see below. |
| `started` | string | yes | RFC 3339 UTC with whole seconds, for example `2026-09-22T10:00:00Z`. |
| `finished` | string | yes | Same form; never earlier than `started`. |
| `verdict` | string | yes | The run verdict, the worst of every file and error. |
| `counts` | object | yes | One key per verdict token that occurred, mapped to the number of files with that verdict. Empty when there were no files. |
| `files` | array | yes | One entry per file in the order the files finished. Empty when there were none. |
| `errors` | array of strings | yes | Run-level problems not tied to one file, such as a path that could not be listed. Empty when there were none. |

The `hook` object has these keys. `label` and `category` are omitted when the
adapter did not receive them.

| Key | Type | Always present | Meaning |
|---|---|---|---|
| `adapter` | string | yes | The adapter name: `sabnzbd`, `nzbget`, `sonarr` or `radarr`. |
| `event` | string | yes | The event the adapter was called for, as the downloader phrased it. |
| `label` | string | no | The job or release name the downloader passed. |
| `category` | string | no | The download category the downloader passed. |
| `fail_on` | string | yes | The verdict at or above which the hook exits non-zero. |
| `exit_code` | integer | yes | The exit code the hook returned to the downloader. |

Each entry in `files` has these keys.

| Key | Type | Always present | Meaning |
|---|---|---|---|
| `path` | string | yes | The input path as it was given or found. For `doctor` it is the check name. |
| `verdict` | string | yes | The worst severity among the file's findings. |
| `findings` | array | yes | Every observation about the file, in the order they were made. Empty when there were none. |
| `output` | string | no | Where the result was written, when a file was written. |
| `actions` | array of strings | no | What remux did or would do to each track, attachment and the chapters. |
| `info` | object of strings | no | Facts about the file, such as `container`, `kind`, `nlink`, `hdr` and `route`. New keys may appear in any release. |
| `duration_ms` | integer | yes | How long the file took, in whole milliseconds. |

Each finding has these keys.

| Key | Type | Always present | Meaning |
|---|---|---|---|
| `code` | string | yes | A fixed upper-case token from the table below. |
| `severity` | string | yes | One of the five verdict tokens. |
| `message` | string | yes | One line for people. Its wording may change. |
| `detail` | string | no | Supporting lines, separated by `\n`, for example each link that was found. |

## Strings and encoding

Paths, messages and details are written exactly as amuxify saw them, with the
escaping JSON requires. Quotes, backslashes and control characters, including
a NUL byte, an escape sequence or a Unicode bidi control in a file name, are
escaped and never break the document. The characters `<`, `>` and `&` are
written as `<`, `>` and `&`, which every JSON parser decodes to
the plain character; the example above shows this in a `ROUTE` message. A file
name that is not valid UTF-8 has each invalid byte replaced with U+FFFD, so
the document is always valid UTF-8. Such a path cannot be recovered byte for
byte from the JSON report; the terminal form writes the name's raw bytes. Map
keys in `counts` and `info` are sorted, so the same run always produces the
same bytes.

## Doctor

`amuxify doctor --json` writes the same envelope. `command` is `doctor`, each
check is one entry in `files` whose `path` is the check name, and each entry
has one finding whose code is the upper-cased check name, with `duration_ms`
0 and no `info`. The severity `USAGE` marks a required tool that is missing or
too old, an invalid profile or an unwritable temporary directory; it appears
as a finding severity and as the run verdict only here. The `STATE-DIR` check
is present only when a state directory is known, which is the case unless
`--state-dir` was set to an empty string.

## Codes

Codes are frozen from 0.2.0 on. A new code may be added in a minor release;
an existing one is never renamed or reassigned. Three codes are declared but
never emitted and are marked reserved. The "Where" column names the command
that adds the code itself. An ingest run scans every file and then remuxes or
cleans it, so any code from `scan`, `remux` or `clean` can appear in an
`ingest` report as well; the column says `ingest` only for codes ingest adds
on its own.

| Code | Severity | Where | Meaning |
|---|---|---|---|
| `SYMLINK` | WARN | scan, clean | The path is a symbolic link and was skipped. |
| `EMPTY_FILE` | BLOCK | scan | The file has zero bytes. |
| `UNREADABLE` | FAIL | scan | The file cannot be opened. |
| `BIDI_NAME` | BLOCK | scan | The name contains a Unicode bidi or zero-width control. |
| `DOUBLE_EXT` | WARN | scan | The extension before the last one is on the sidecar block list, as in `x.exe.mkv`. |
| `EXEC_PERM` | WARN or FAIL | scan | The executable bit is set on a media file; the profile decides the severity. |
| `EXT_MISMATCH` | FAIL | scan | The content type does not match the extension. |
| `EXT_UNKNOWN` | reserved | scan | Declared, never emitted. |
| `DANGEROUS_CONTENT` | BLOCK | scan | An executable, script, HTML page or archive where media was expected. |
| `POLYGLOT` | BLOCK | scan | An archive or executable signature in the head or tail of the file. |
| `UNPARSEABLE` | FAIL | scan | ffprobe or the MP4 walker cannot read the file. |
| `TRUNCATED` | FAIL | scan | An MP4 box declares more bytes than the file has, or there is no moov box. |
| `NO_VIDEO`, `NO_AUDIO` | FAIL | scan, remux | A stream type the container should carry is missing. |
| `DATA_STREAM` | BLOCK | scan | A data stream with a tag outside `streams.allow_data_tags`. |
| `ATTACH_BLOCKED` | BLOCK | scan | An attachment type the profile blocks. |
| `ATTACH_EXEC` | BLOCK | scan | An attachment payload that is executable or an archive. |
| `ATTACH_DROP` | PASS | scan | An attachment that policy will remove; remux lists the removal under `actions`. |
| `ATTACH_OK` | PASS | scan | An attachment that is allowed. |
| `MKV_ERROR` | FAIL | scan | `mkvmerge -J` exited 2. |
| `MKV_WARNING` | WARN | scan | `mkvmerge -J` exited 1. |
| `PURCHASE_ATOM` | WARN or FAIL | scan, clean | Identifying MP4 atoms; the profile decides the severity, and clean reports FAIL when they survive a rewrite. |
| `PROVENANCE_INFO` | PASS | scan | Encoder and muxer fingerprints; shown in verbose output only. |
| `LINK_IN_TAG` | WARN or FAIL | scan | A URL or domain in the title, tags, chapters or attachment names; `metadata.links` decides the severity. |
| `LINK_IN_SUBS` | WARN or FAIL | scan | A URL or domain in a text subtitle track; `subtitles.links` decides the severity. |
| `DECODE_FAIL` | FAIL | scan, remux, ingest | ffmpeg reported errors while decoding. |
| `NO_DURATION` | WARN | scan | The container reports no duration. |
| `CLAMAV_INFECTED` | BLOCK | scan | clamscan reported a match. |
| `CLAMAV_ERROR` | WARN | scan | clamscan could not run or failed. |
| `CLAMAV_MISSING` | FAIL | scan | The profile requires clamscan and it is not installed. |
| `HARDLINKED` | PASS (scan, remux copy) or WARN (remux skip or break, clean skip, ingest) | scan, remux, clean, ingest | More than one link to the inode; scan records the count in `info.nlink`. |
| `SIDECAR_BLOCKED` | BLOCK | scan | A sidecar extension on the block list. |
| `SIDECAR_UNKNOWN` | WARN | scan | A sidecar extension on neither list. |
| `SIDECAR_OK` | PASS | scan | An allowed sidecar. |
| `SIDECAR_REMOVED` | PASS | clean | A blocked sidecar was removed under `--remove-blocked-sidecars`. |
| `NFO_KODI` | PASS | scan | A Kodi metadata nfo or a scraper URL nfo. |
| `NFO_TEXT` | PASS | scan | A plain text nfo that Kodi ignores. |
| `LINK_IN_SIDECAR` | WARN or FAIL | scan | A link in an nfo outside the Kodi artwork and scraper fields; `metadata.links` decides the severity. |
| `UND_TRACK` | PASS | scan, remux | A track without a language tag. |
| `AUDIO_FALLBACK` | WARN | remux | Policy would have dropped every audio track, so all were kept. |
| `VIDEO_EXTRA` | WARN | remux, ingest | A secondary video stream was dropped. |
| `HDR` | PASS | scan | HDR10, HLG, Dolby Vision or HDR10+ was detected. |
| `HDR_LOST` | FAIL | remux | The output lost HDR or Dolby Vision signalling. |
| `QUARANTINED` | BLOCK, or WARN when the move failed | scan | The file was moved under `--quarantine`. |
| `REFUSED` | FAIL or BLOCK | remux, ingest | The scan verdict prevented the remux; the severity is the scan verdict. |
| `SKIPPED` | PASS or WARN | remux, clean, ingest | There was nothing to do for this file; the message says why. |
| `ROUTE` | PASS | ingest | The route taken, `remux`, `clean` or `skip`, and why. |
| `OUTPUT_EXISTS` | FAIL | remux | The destination already exists; it is never overwritten. |
| `UNSUPPORTED_INPUT` | FAIL | remux | mkvmerge cannot read this container. |
| `REMUX_FAIL` | FAIL | remux | mkvmerge exited 2 or a later step failed. |
| `REMUX_WARNING` | WARN | remux | mkvmerge exited 1. |
| `HASH_MISMATCH` | FAIL | remux, clean | A stream hash comparison failed; the output was deleted and the source is untouched. |
| `HASH_OK` | PASS | remux | Every kept stream hashed identical to the source. |
| `VERIFY_FAIL` | FAIL | remux | The output structure differs from the decision. |
| `PLACED` | PASS | remux | The output was written and verified. |
| `DRY_RUN` | PASS | remux, clean | What would have been done under `--dry-run`. |
| `METADATA` | PASS | clean | Metadata was removed or rewritten. |
| `XATTR` | PASS, or WARN when an attribute could not be removed | clean | Extended attributes were removed. |
| `NOTHING_TO_CLEAN` | PASS | clean | The file was already clean or carries no writable metadata. |
| `CLEAN_FAIL` | FAIL | clean | A cleaning step failed; the source is untouched. |
| `MKVMERGE`, `MKVPROPEDIT`, `MKVEXTRACT`, `FFMPEG`, `FFPROBE`, `EXIFTOOL`, `CLAMSCAN`, `LOCALE`, `PROFILE`, `CLAMAV`, `USER`, `STATE-DIR`, `TMPDIR` | PASS, WARN or USAGE | doctor | One row per check; the message is the check's detail line. |
| `TRACK`, `ATTACHMENT` | reserved | remux | Declared, never emitted. |
