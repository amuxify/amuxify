# Safety model

amuxify treats every input as hostile until proven otherwise, and treats your
library as something it must never damage. These are the invariants. Each one
is enforced in code and covered by a test; a violation is a bug, report it via
SECURITY.md.

## Verdicts

- **PASS** nothing to report.
- **WARN** the file is usable; something you should know about (a link in a
  tag, a link in an NFO, an untagged track, an unknown sidecar, a hard link
  that an in-place edit would break).
- **FAIL** integrity: the file cannot be parsed, is truncated, is not what its
  extension claims, or an output could not be verified. `remux --force` will
  still attempt a FAIL file; the output is verified as strictly as any other.
- **BLOCK** security: an executable or archive payload, a polyglot, a bidi
  control character in the name, a blocked sidecar type, a positive antivirus
  hit. No flag overrides BLOCK.

## The ten guarantees

1. **Never overwrite.** Output is placed with a link-then-unlink that fails if
   the destination exists. A collision is `FAIL OUTPUT_EXISTS`.
   Test: `fsutil.TestPlaceNoClobberRefusesExisting`, `remux.TestOutputExistsBeforeAnyTool`.
2. **Temp, fsync, rename.** Output is written as `.amuxify-<name>.tmp` in the
   destination directory, fsynced, then renamed. Same filesystem always; a
   crash leaves at most a temp file, which the next run ignores. Only a name
   of exactly that shape is ignored: a file that borrows the `.amuxify-`
   prefix without the `.tmp` suffix is scanned like any other.
   Test: `fsutil.TestTempNameIsHiddenSibling`, `remux.TestRemuxWritesViaTempAndPlaces`.
3. **Symlinks are never followed.** Each symlink is reported `WARN SYMLINK` and
   skipped. A symlink somewhere in a tree never aborts the run (0.1.x did).
   A symlink swapped onto the temp name during a run is refused before the
   output takes its source's identity, which is copied through the open
   descriptor rather than the name, and before the output is placed. The
   temp file is held open from its creation until the last of those checks,
   so its inode number cannot be freed and handed to a file swapped onto the
   name, as ext4 would do at once.
   A job directory that a download client hands over as a symlink is the
   same case: the hook reports the link and never enters the target.
   Test: `scan.TestSymlinkSkippedNotFollowed`, `fsutil.TestCopyIdentityToNeverFollowsSymlinkAtFormerName`,
   `fsutil.TestCreateTempPinsInode`, `remux.TestTempSwappedBeforePlacementRefused`,
   `clean.TestMp4RewriteRefusesSwappedTemp`,
   `cli.TestHookSABnzbdArgumentForms/SAB_COMPLETE_DIR_is_a_symlink`.
4. **ffmpeg cannot reach the network or devices.** Every ffmpeg and ffprobe call
   is started with `-protocol_whitelist file,pipe`, `-nostdin`, a clean
   environment, and a timeout. A crafted playlist or subtitle cannot make
   ffmpeg open a URL.
   Test: `exec.TestFFGuardPrependsWhitelist`, `exec.TestCleanEnvDropsLDPreload`.
5. **Streams are proven identical.** After remux, every kept stream is hashed
   in the source and in the output with `ffmpeg -f streamhash` (SHA-256 of
   packets). When the source container frames packets differently from
   Matroska (MPEG-TS, MPEG-PS, AVI), decoded frames are compared instead. Any
   difference deletes the output and fails the file.
   Test: `remux.TestHashMismatchDeletesOutput`.
6. **In place preserves identity.** `--in-place` copies mode, owner, group and
   modification time from the source to the output before the rename.
   Test: `fsutil.TestReplaceInPlacePreservesModeAndMtime`, `remux.TestInPlacePreservesIdentity`.
7. **Extended attributes are scoped.** Only `user.*` (Linux, FreeBSD) and
   `com.apple.*` (macOS) are removed. ACLs, SELinux labels, capabilities and
   `security.*` are never touched.
   Test: `clean.TestStripXattrsOnlyListedNamespaces`.
8. **No unverified in-place writes.** `--verify none` with `--in-place` is a
   usage error, and `ingest` and every hook adapter refuse verify tier `none`
   whether it comes from the flag or from the profile.
   Test: `remux.TestInPlaceVerifyNoneRefused`, `ingest.TestVerifyNoneRefusedFromFlagAndProfile`.
9. **BLOCK is final.** No flag, profile key or environment variable turns a
   BLOCK into anything else.
   Test: `remux.TestBlockRefusedEvenWithForce`, `ingest.TestBlockRefusedEvenWithForce`, `scan.TestBlockIsNeverLowered`.
10. **No root by accident.** Modifying commands refuse to run as uid 0 unless
    `--allow-root`, because a hook container running as root would leave
    root-owned files in the library.
    Test: `cli.TestSetupRefusesRoot`.

`ingest` and the hook adapters are compositions of scan, remux and clean and
add no write path of their own; a file is rebuilt by the same remux code,
edited by the same mkvpropedit call `clean` uses, or left alone, and a
hard-linked file is never edited in place. Every guarantee above applies to
them unchanged. A hook never turns a client job into a failed one for a WARN
or FAIL verdict unless `--fail-on` says so, and BLOCK remains final.

Everything a hook receives from its caller is untrusted, and some of it
comes from further away: SABnzbd passes the indexer's `X-DNZB-Failure`
header as its eighth parameter unchanged. Positional arguments are accepted
only in the shapes the caller is documented to produce (for SABnzbd, none or
its seven or eight parameters, with a directory added in front of an older
SABnzbd's seven refused rather than scanned in place of the job's own), a
job whose status is missing is refused rather than assumed successful, a job
directory that is or lies inside the quarantine directory is refused before
anything runs, and every value is carried as data: nothing is split,
expanded or executed, and control characters never reach the log raw. No
value an indexer can write is ever a reason to refuse a job, because a
refusal is a usage exit and SABnzbd's default settings leave a job
successful on one; the failure URL is never inspected, and the hint that
names a stray argument never names one of SABnzbd's own.
Test: `hook.TestCheckSABnzbdArgs`, `hook.TestSABnzbdFailureURLNeverRefuses`,
`hook.TestParseKeepsHostileValuesAsData`,
`cli.TestHookSABnzbdArgumentForms`, `cli.TestHookArgumentInjection`,
`cli.TestHookEnvironmentInjection`.

## What scan looks at

Filename: bidi controls and every other Unicode format character (the
zero-width and other invisible ones), double extensions, executable bit.
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
not rewrite or delete `.nfo` files because of their contents, does not strip
music tags unless `clean --strip-audio-tags` asks for it, does not phone home,
and keeps no journal. Keep originals until you have looked at the output.
