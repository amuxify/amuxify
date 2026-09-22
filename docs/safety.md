# Safety model

amuxify treats every input as hostile until proven otherwise, and treats your
library as something it must never damage. These are the invariants. Each one
is enforced in code and covered by a test; a violation is a bug, report it via
SECURITY.md.

## Verdicts

- **PASS** nothing to report.
- **WARN** the file is usable; something you should know about (a link in a
  tag, an untagged track, an unknown sidecar, a hard link).
- **FAIL** integrity: the file cannot be parsed, is truncated, is not what its
  extension claims, or an output could not be verified. `remux --force` will
  still attempt a FAIL file; the output is verified as strictly as any other.
- **BLOCK** security: an executable or archive payload, a polyglot, a bidi
  control character in the name, a blocked sidecar type, a positive antivirus
  hit. No flag overrides BLOCK.

## The ten guarantees

1. **Never overwrite.** Output is placed with a link-then-unlink that fails if
   the destination exists. A collision is `FAIL OUTPUT_EXISTS`.
2. **Temp, fsync, rename.** Output is written as `.amuxify-<name>.tmp` in the
   destination directory, fsynced, then renamed. Same filesystem always; a
   crash leaves at most a temp file, which the next run ignores.
3. **Symlinks are never followed.** Each symlink is reported `WARN SYMLINK` and
   skipped. A symlink somewhere in a tree never aborts the run (0.1.x did).
4. **ffmpeg cannot reach the network or devices.** Every ffmpeg and ffprobe call
   is started with `-protocol_whitelist file,pipe`, `-nostdin`, a clean
   environment, and a timeout. A crafted playlist or subtitle cannot make
   ffmpeg open a URL.
5. **Streams are proven identical.** After remux, every kept stream is hashed
   in the source and in the output with `ffmpeg -f streamhash` (SHA-256 of
   packets). When the source container frames packets differently from
   Matroska (MPEG-TS, MPEG-PS, AVI), decoded frames are compared instead. Any
   difference deletes the output and fails the file.
6. **In place preserves identity.** `--in-place` copies mode, owner, group and
   modification time from the source to the output before the rename.
7. **Extended attributes are scoped.** Only `user.*` (Linux, FreeBSD) and
   `com.apple.*` (macOS) are removed. ACLs, SELinux labels, capabilities and
   `security.*` are never touched.
8. **No unverified in-place writes.** `--verify none` with `--in-place` is a
   usage error.
9. **BLOCK is final.** No flag, profile key or environment variable turns a
   BLOCK into anything else.
10. **No root by accident.** Modifying commands refuse to run as uid 0 unless
    `--allow-root`, because a hook container running as root would leave
    root-owned files in the library.

## What scan looks at

Filename: bidi and zero-width characters, double extensions, executable bit.
Container: magic bytes versus extension, executable or archive signatures in the
first and last MiB (polyglots), truncated MP4, ffprobe and `mkvmerge -J`
parseability with warnings tolerated. Streams: no video, no audio, unknown or
data streams outside the allow list, attachments by policy with payload
sniffing, HDR and Dolby Vision presence. Metadata: links in title, tags, chapter
names, attachment names, text subtitle tracks; MP4 purchase and identifier
atoms; XMP boxes. Optional: ClamAV, full decode.

## What amuxify does not do

It does not rewrite subtitle text, does not transcode, does not rename, does
not delete media (only blocked sidecars, and only with an explicit flag), does
not phone home, and keeps no journal. Keep originals until you have looked at
the output.
