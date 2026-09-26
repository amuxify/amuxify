# Report format and finding codes

`--json` writes one object on stdout:

```json
{
  "tool": "amuxify",
  "version": "0.2.0",
  "command": "scan",
  "profile": "homelab",
  "started": "2026-09-22T10:00:00Z",
  "finished": "2026-09-22T10:00:04Z",
  "verdict": "WARN",
  "counts": {"PASS": 3, "WARN": 1},
  "files": [
    {
      "path": "/srv/incoming/movie.mkv",
      "verdict": "WARN",
      "findings": [
        {"code": "LINK_IN_TAG", "severity": "WARN", "message": "1 link(s) in metadata: tag COMMENT: http://x.example", "detail": "tag COMMENT: http://x.example"}
      ],
      "output": "/srv/incoming__remuxed/movie.mkv",
      "actions": ["keep #0 video h264 und: primary video", "..."],
      "info": {"container": "matroska"},
      "duration_ms": 812
    }
  ],
  "errors": []
}
```

Codes and verdict tokens are frozen from 0.2.0 on; new codes may be added in
minor versions, existing ones are never renamed or reassigned. The schema is
declared stable in 0.3.0.

## Codes

| Code | Severity | Where | Meaning |
|---|---|---|---|
| `SYMLINK` | WARN | scan, clean | symlink skipped |
| `EMPTY_FILE` | BLOCK | scan | zero bytes |
| `UNREADABLE` | FAIL | scan | cannot open |
| `BIDI_NAME` | BLOCK | scan | Unicode bidi or zero-width control in the name |
| `DOUBLE_EXT` | WARN | scan | media extension hidden before another extension |
| `EXEC_PERM` | WARN/FAIL | scan | executable bit on a media file (profile decides) |
| `EXT_MISMATCH` | FAIL | scan | content type does not match the extension |
| `EXT_UNKNOWN` | WARN | scan | extension not in the media or sidecar lists |
| `DANGEROUS_CONTENT` | BLOCK | scan | executable, script, HTML or archive where media was expected |
| `POLYGLOT` | BLOCK | scan | archive or executable signature in the head or tail |
| `UNPARSEABLE` | FAIL | scan | ffprobe or the MP4 walker cannot read the file |
| `TRUNCATED` | FAIL | scan | MP4 box declares more bytes than the file has, or no moov |
| `NO_VIDEO`, `NO_AUDIO` | WARN | scan, remux | missing stream type |
| `DATA_STREAM` | BLOCK | scan | data stream with a tag outside `streams.allow_data_tags` |
| `ATTACH_BLOCKED` | BLOCK | scan | attachment type the profile blocks |
| `ATTACH_EXEC` | BLOCK | scan | attachment payload is executable or an archive |
| `ATTACH_DROP` | PASS | scan, remux | attachment will be removed by policy |
| `ATTACH_OK` | PASS | scan | attachment allowed |
| `MKV_ERROR` | FAIL | scan | `mkvmerge -J` exit 2 |
| `MKV_WARNING` | WARN | scan | `mkvmerge -J` exit 1 |
| `PURCHASE_ATOM` | WARN/FAIL | scan, clean | identifying MP4 atoms (profile decides) |
| `PROVENANCE_INFO` | PASS | scan | encoder and muxer fingerprints (verbose only) |
| `LINK_IN_TAG` | WARN/FAIL | scan | URL or domain in title, tags, chapters, attachment names |
| `LINK_IN_SUBS` | WARN/FAIL | scan | URL or domain in a text subtitle track |
| `DECODE_FAIL` | FAIL | scan, remux | ffmpeg reported errors decoding |
| `NO_DURATION` | WARN | scan | container reports no duration |
| `CLAMAV_INFECTED` | BLOCK | scan | clamscan match |
| `CLAMAV_ERROR`, `CLAMAV_MISSING` | WARN/FAIL | scan | clamscan problem |
| `HARDLINKED` | WARN | scan, remux, clean | more than one link to the inode |
| `SIDECAR_BLOCKED` | BLOCK | scan | sidecar extension on the block list |
| `SIDECAR_UNKNOWN` | WARN | scan | sidecar extension on neither list |
| `SIDECAR_OK`, `SIDECAR_REMOVED` | PASS | scan, clean | |
| `UND_TRACK` | PASS/WARN | scan, remux | track without a language tag |
| `AUDIO_FALLBACK` | WARN | remux | policy would drop all audio; all kept |
| `HDR` | PASS | scan | HDR10, HLG, Dolby Vision or HDR10+ detected |
| `HDR_LOST` | FAIL | remux | output lost HDR or DV signalling |
| `QUARANTINED` | PASS | scan | moved under `--quarantine` |
| `REFUSED` | FAIL/BLOCK | remux | scan verdict prevented remux |
| `OUTPUT_EXISTS` | FAIL | remux | destination already exists |
| `UNSUPPORTED_INPUT` | FAIL | remux | mkvmerge cannot read this container |
| `REMUX_FAIL`, `REMUX_WARNING` | FAIL/WARN | remux | mkvmerge exit 2 / exit 1 |
| `HASH_MISMATCH`, `HASH_OK` | FAIL/PASS | remux, clean | stream hash comparison |
| `VERIFY_FAIL` | FAIL | remux | output structure differs from the decision |
| `PLACED`, `DRY_RUN` | PASS | remux, clean | |
| `METADATA`, `XATTR`, `NOTHING_TO_CLEAN`, `CLEAN_FAIL` | PASS/FAIL | clean | |
