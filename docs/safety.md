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
   Under `--jobs` every worker claims its destination in a run-wide set
   before it creates anything, so two sources that rebuild to one name give
   exactly one output and `OUTPUT_EXISTS` for the other, and two blocked
   files that would quarantine under one name give one move and one
   `quarantine failed`. The claim is given back whenever the destination is
   left empty, including when a placed output is taken away again because
   its source changed, so the claim set never refuses a name that is free
   on disk.
   Test: `fsutil.TestPlaceNoClobberRefusesExisting`, `remux.TestOutputExistsBeforeAnyTool`,
   `pool.TestClaimsExactlyOneWinner`, `remux.TestClaimExactlyOneWinner`,
   `remux.TestClaimGuardsConcurrentRemuxScanned`, `remux.TestParallelSameDestinationOnce`,
   `remux.TestClaimReleasedWhenNothingPlaced`, `remux.TestClaimReleasedWhenPlacedOutputRemovedAgain`,
   `scan.TestQuarantineClaimRefusesBeforeDisk`, `scan.TestQuarantineSameNameFromTwoRootsOnce`,
   `cli.TestJobsSameDestinationRace`.
2. **Temp, fsync, rename.** Output is written as `.amuxify-<name>.tmp` in the
   destination directory, fsynced, then renamed. Same filesystem always; a
   crash leaves at most a temp file, which the next run ignores. Only a name
   of exactly that shape is ignored: a file that borrows the `.amuxify-`
   prefix without the `.tmp` suffix is scanned like any other.
   An interrupt under `--jobs` starts no further file, waits for the files
   already running, and leaves no temp file behind them either.
   A cancelled context, whether from a tool timeout or from an interrupt,
   kills the tool and removes the temp file. `watch` does not pass its
   interrupt into the file in progress: that file is finished and the pass
   stops before the next one, so a stop signal never kills a tool mid-write.
   Test: `fsutil.TestTempNameIsHiddenSibling`,
   `remux.TestRemuxWritesViaTempAndPlaces`,
   `pool.TestRunCancelStartsNothingNew`, `cli.TestJobsInterruptLeavesNoTemp`,
   `remux.TestFailedRemuxLeavesNothing` (a timed-out mkvmerge leaves nothing),
   `remux.TestCancelledMidMkvmergeLeavesNoTemp` (a cancel while mkvmerge writes
   the temp file removes it and leaves the source untouched),
   `watch.TestCancelStopsBetweenFiles` (the file in progress is handed a context
   the cancel does not reach), `cli.TestWatchInterruptFinishesFileInProgress`
   (SIGINT while mkvmerge runs under `watch`: the file is finished and placed,
   no further file is started, no temp file is left).
3. **Symlinks are never followed.** Each symlink is reported `WARN SYMLINK` and
   skipped. A symlink somewhere in a tree never aborts the run (0.1.x did).
   A symlink swapped onto the temp name during a run is refused before the
   output takes its source's identity, which is copied through the open
   descriptor rather than the name, and before the output is placed. The
   temp file is held open from its creation until the last of those checks,
   so its inode number cannot be freed and handed to a file swapped onto the
   name, as ext4 would do at once.
   The same holds for a named pipe, which is worse than a symlink in one way:
   opening it waits for a peer that a planter never has to supply, so a pipe
   at a name the run opens could park the process for good. Every open of a
   name amuxify did not create through a descriptor it still holds (an input
   file, a sidecar, the quarantine source and destination, the temp name
   after an external tool wrote to it) is made without following a link and
   without blocking, and the entry is refused unless it turns out to be a
   regular file. A pipe under a media or sidecar name is skipped: scan,
   remux and ingest report it as `FAIL UNREADABLE`, and clean reports it as
   `FAIL CLEAN_FAIL`. A pipe swapped onto the temp name after mkvmerge or
   ffmpeg returns is caught before any further read of that name. While
   the tool itself has the name, only its timeout bounds the wait, and the
   run reports the timeout as a failed remux or clean. The flush before
   placement goes through the descriptor held since the temp file was
   created, never through the name.
    `watch` lists symlinks without following them, refuses a directory that
   was replaced by a symlink between two passes, and checks the whole path
   again right before each ingest.
   Test: `scan.TestSymlinkSkippedNotFollowed`,
   `fsutil.TestCopyIdentityToNeverFollowsSymlinkAtFormerName`,
   `fsutil.TestCreateTempPinsInode`,
   `remux.TestTempSwappedBeforePlacementRefused`,
   `clean.TestMp4RewriteRefusesSwappedTemp`,
   `fsutil.TestOpenRegularRefusesNamedPipeAndEveryOtherKind`,
   `fsutil.TestOpenOwnRefusesNamedPipe`,
   `fsutil.TestReplaceInPlaceOwnRefusesNamedPipeAtTemp`,
   `fsutil.TestCreateTempReplacesPlantedPipe`,
   `fsutil.TestFsyncRefusesNamedPipe`, `fsutil.TestTempSyncNeverOpensTheName`,
   `fsutil.TestMoveNoClobberRefusesNamedPipes`,
   `sniff.TestFileRefusesNamedPipeAndSymlink`,
   `mp4.TestParseRefusesNamedPipeAndSymlink`, `scan.TestNamedPipeInputRefused`,
   `scan.TestScanReadersRefuseNamedPipe`,
   `scan.TestQuarantineRefusesNamedPipeAtDestination`,
   `scan.TestAttachmentSwappedForPipeDoesNotBlock`,
   `clean.TestNamedPipeInputRefused`, `clean.TestMp4RewriteRefusesPipeAtTemp`,
   `remux.TestNamedPipeInputRefused`,
   `remux.TestTempSwappedForPipeAfterMkvmerge`,
   `remux.TestTakeIdentityRefusesPipeAtTemp`,
   `remux.TestMkvmergeBlockedOnPipeIsKilled`,
   `ingest.TestNamedPipeInputRefused`,
   `cli.TestHookSABnzbdArgumentForms/SAB_COMPLETE_DIR_is_a_symlink`,
   `watch.TestSymlinkIsSkippedAndNoticedOnce`,
   `watch.TestDirectoryReplacedBySymlinkIsNotFollowed`,
   `watch.TestRecheckRefusesEveryChange`,
   `watch.TestRootSwappedForSymlinkIsRefused`, `cli.TestWatchSymlinkIsSkipped`.
4. **ffmpeg cannot reach the network or devices.** Every ffmpeg and ffprobe call
   is started with `-protocol_whitelist file,pipe`, `-nostdin`, a clean
   environment, and a timeout. A crafted playlist or subtitle cannot make
   ffmpeg open a URL. Every tool, clamscan included, runs under the same
   environment and timeout, and the runner keeps a bounded amount of what a
   tool prints, so a tool that hangs is killed and a tool that floods its
   output cannot grow the process. A bounded output is never mistaken for
   the whole: a probe cut at the bound is an unparseable file with the cut
   named as the reason, and a text subtitle track cut at the bound is
   reported `LINK_IN_SUBS` as not fully checked, so a track padded past the
   bound cannot hide a link behind it. What clamscan printed reaches the
   report only as the lines about the scanned file, walked in place so the
   memory taken is bounded by the detail and not by the output, cut to a
   fixed size and passed through the report sanitiser; the verdict comes
   from its exit status alone.
   Test: `exec.TestFFGuardPrependsWhitelist`, `exec.TestCleanEnvDropsLDPreload`,
   `exec.TestRunTimeoutReturnsTimedOut`, `exec.TestRunCapsOutput`,
   `scan.TestSubtitleTrackLongerThanRunnerKeeps`, `scan.TestProbeOutputLongerThanRunnerKeeps`,
   `scan.TestClamscanHangIsKilledAtTimeout`, `scan.TestClamscanOutputBounded`,
   `scan.TestClamDetailMemoryBounded`,
   `scan.TestClamscanHostileOutputSanitised`, `scan.TestClamscanArgumentsVerbatim`.
5. **Streams are proven identical.** After remux, every kept stream is hashed
   in the source and in the output with `ffmpeg -f streamhash` (SHA-256 of
   packets). When the source container frames packets differently from
   Matroska (MPEG-TS, MPEG-PS, AVI), decoded frames are compared instead. Any
   difference deletes the output and fails the file. The colour and HDR
   signalling of every kept video stream is held to the same standard,
   because it lives in the container header rather than in the packets and
   a muxer can drop or alter it without changing a single hash: the colour
   primaries, transfer characteristic, matrix coefficients and range, the
   mastering display chromaticity and luminance, the content light levels
   and the Dolby Vision configuration record are read from the source and
   from the output, and any value that was lost, gained or changed is
   `FAIL HDR_LOST` with the value named; the output is deleted like any
   other verification failure. An SDR file is compared as strictly, so an
   output that gained signalling fails too. Names, light levels and the
   Dolby Vision fields must match exactly. The chromaticity and luminance
   values are compared with a tolerance of one part in a million, which is
   about sixteen times the rounding of the single precision floats the
   Matroska header stores them in and is the only reason two readings of
   one value can differ; a value nudged by a tenth of a percent is reported
   as changed. Every number read from a tool is validated before it is
   compared: a value that is not a number, not finite, negative, or beyond
   what the format can express is recorded as malformed rather than
   trusted, and a malformed value in the source that is missing in the
   output is still a difference. A field that either tool rejected stays
   malformed when the two tools' readings are combined, so a clean value
   from the other tool never stands in for it. The values mkvmerge does not
   read from an MP4 source (the range flag, the mastering display and the
   content light levels) are passed to it on the command line from the
   source's ffprobe reading, so an HDR10 MP4 comes through as a complete
   HDR10 Matroska file rather than failing on every run; the assertion is
   not relaxed for it, and a mkvmerge that still loses a value fails the
   file.
   Test: `remux.TestHashMismatchDeletesOutput`, `remux.TestHDRPropertyStrippedDeletesOutput`,
   `remux.TestSDRGainedHDRFails`, `remux.TestHdrDiffDolbyVision`,
   `remux.TestRemuxKeepsHDRProperties`,
   `remux.TestMkvmergeArgsCarryColourMkvmergeDrops`, `probe.TestCloseEnough`,
   `probe.TestColorMergeKeepsRejectedFieldMalformed`,
   `probe.TestColorHostileFFprobeJSON`, `probe.TestColorHostileMkvmergeJSON`,
   `probe.TestProbeKeepsMkvmergeColourView`.
6. **In place preserves identity.** `--in-place` copies mode, owner, group and
   modification time from the source to the output before the rename. The
   copy goes through the descriptor of the file this run created, so it
   cannot land on anything swapped onto the temp name. On Linux the
   modification time is set with `utimensat` on that descriptor, which does
   not need `/proc`; the `/proc/self/fd` form the Go standard library uses
   is the second attempt, and only when both are refused is the file's own
   name used, after a check that the name still leads to the open file and
   with a call that never follows a symlink. A mode or a modification time
   that cannot be set is an error, not a best-effort step: the run stops
   before the rename, the source keeps its name, and the file is reported
   as a failed replacement. The same happens when the name-based fallback
   finds the temp name swapped, so an output that would have carried the
   wrong identity is never placed. Only ownership is best effort, because
   a non-root user cannot give a file away.
   Test: `fsutil.TestReplaceInPlacePreservesModeAndMtime`, `remux.TestInPlacePreservesIdentity`,
   `fsutil.TestReplaceInPlaceOwnFailsWhenTimeCannotBeSet`,
   `fsutil.TestFutimesFallsBackWithoutProc`, `fsutil.TestFutimesPathFallbackRefusesSwappedFile`,
   `fsutil.TestFutimesDescriptorCallIgnoresTheName`,
   `fsutil.TestReplaceInPlaceOwnRefusesSwapDuringPathFallback` (the last four Linux only).
7. **Extended attributes are scoped.** Only `user.*` (Linux, FreeBSD) and
   `com.apple.*` (macOS) are removed. ACLs, SELinux labels, capabilities and
   `security.*` are never touched.
   Test: `clean.TestStripXattrsOnlyListedNamespaces`.
8. **No unverified in-place writes.** `--verify none` with `--in-place` is a
   usage error, and `ingest`, `watch` and every hook adapter refuse verify
   tier `none` whether it comes from the flag or from the profile.
   Test: `remux.TestInPlaceVerifyNoneRefused`, `ingest.TestVerifyNoneRefusedFromFlagAndProfile`,
   `cli.TestWatchUsage`.
9. **BLOCK is final.** No flag, profile key or environment variable turns a
   BLOCK into anything else. A positive antivirus hit is a BLOCK from the
   exit status of clamscan, whatever the scanner printed, and `--clamav`
   never lowers a profile that requires the scan. A profile that requires
   the scan never passes a file that was not scanned: a scanner that is
   missing, that was killed at the timeout, that could not start or that
   exited without a verdict fails the file before it is probed, and
   `ingest` and the hook adapters refuse it.
   Test: `remux.TestBlockRefusedEvenWithForce`,
   `ingest.TestBlockRefusedEvenWithForce`, `scan.TestBlockIsNeverLowered`,
   `scan.TestClamscanInfectedBlocks`,
   `scan.TestClamAVFlagForcesAndNeverDowngrades`,
   `scan.TestClamscanErrorFailsUnderRequiredProfile`,
   `ingest.TestClamscanInfectedRefusedEvenWithForce`,
   `ingest.TestClamscanMissingRefusesMedia`,
   `ingest.TestClamscanErrorRefusedUnderStrict`,
   `cli.TestClamscanInfectedBlocksEverywhere`,
   `cli.TestClamscanMissingWithStrictProfile`,
   `cli.TestClamscanErrorFailsStrictEverywhere`,
   `doctor.TestClamscanDatabaseRequiredByProfile`.
10. **No root by accident.** Modifying commands refuse to run as uid 0 unless
    `--allow-root`, because a hook container running as root would leave
    root-owned files in the library.
    Test: `cli.TestSetupRefusesRoot`, `cli.TestWatchRefusesRoot`.

`ingest`, `watch` and the hook adapters are compositions of scan, remux and
clean and add no write path of their own; a file is rebuilt by the same remux
code, edited by the same mkvpropedit call `clean` uses, or left alone, and a
hard-linked file is never edited in place. Every guarantee above applies to
them unchanged. `watch` adds only a decision about when to hand a file to
ingest: a file is handed over once it has been seen unchanged in size,
modification time and identity for the settle window, and is checked again
right before the hand-over (`watch.TestGrowingFileIsNotIngested`,
`watch.TestSwappedFileIsNew`, `watch.TestChangedDuringIngestIsIngestedAgain`).
The quarantine directory is excluded from the walk as in ingest and refused
as the watched directory (`cli.TestWatchQuarantineAndSidecarWiring`), and a
hostile file name reaches the terminal escaped (`cli.TestWatchEscapesHostileNames`). A hook never turns a client job into a failed one for a WARN
or FAIL verdict unless `--fail-on` says so, and BLOCK remains final.

Everything a hook receives from its caller is untrusted, and some of it
comes from further away: SABnzbd passes the indexer's `X-DNZB-Failure`
header as its eighth parameter unchanged, and the second parameter is the
name the indexer gave the NZB. Positional arguments are accepted only in the
shapes the caller is documented to produce (for SABnzbd, none or its seven
or eight parameters, with a directory added in front of an older SABnzbd's
seven refused rather than scanned in place of the job's own, and seven under
a SABnzbd that sets the environment, which always passes eight, refused as a
wrapper flag that swallowed the job directory), a job whose status is
missing is refused rather than assumed successful, a job directory that is
or lies inside the quarantine directory is refused before anything runs,
and every value is carried as data: nothing is split, expanded or executed,
and control characters never reach the log raw. No value an indexer can
write is ever a reason to refuse a job, because a refusal is a usage exit
and SABnzbd's default settings leave a job successful on one; the failure
URL is never inspected, a bare NZB name is never looked up on disk, and the
hint that names a stray argument never names one of SABnzbd's own.
   Test: `hook.TestCheckSABnzbdArgs`, `hook.TestSABnzbdFailureURLNeverRefuses`,
`hook.TestParseKeepsHostileValuesAsData`,
`cli.TestHookSABnzbdArgumentForms`, `cli.TestHookArgumentInjection`,
`cli.TestHookHostileEnvironment`.

`--jobs` changes how many files are in flight, not what happens to any one of
them. The worker pool never runs two names of one inode at the same time, nor
two sources that map to one output or quarantine name; such files run one
after the other in walk order, so a parallel run makes the same decisions as
a sequential one. A name is compared in lower case, and a name that holds a
character outside ASCII is keyed to its directory as well, so two spellings
of one accented name that a filesystem such as APFS folds into one entry are
also run one after the other. `remux` scans every file of a tree before it
rebuilds any, whatever the job count, so the scan findings describe the tree
as the run found it: the second name of a hard-linked pair is reported with
the links it had before the first name was rebuilt in place. Paths given on
the command line are still processed one after the other, and a file that
quarantines to a name an earlier root already took meets that file on disk.
The hook adapters and `watch` always use one job, because the download
client, or the watcher's own polling, decides how many files are handed over
at once. Whatever the job count, at most one `clamscan` process runs at a
time, because each one loads the whole signature database; a worker whose
file is due for it waits its turn.
   Test: `pool.TestRunKeysSerialise`, `pool.TestRunChainedKeysComplete`,
`remux.TestParallelHardLinksSerialised`, `remux.TestSerialKeysOnlyMediaGetDestKeys`,
`scan.TestScanPathParallelOrderAndCancel`, `scan.TestClamScanRunsOneAtATime`,
`exec.TestPathConcurrentCallers`, `cli.TestJobsParallelMatchesSequential`,
`cli.TestJobsSameQuarantineNameAcrossRoots`, `cli.TestJobsUsageErrors`,
`cli.TestHookIgnoresJobs`, `pool.TestPathKeys`,
`remux.TestParallelNormalisationCollisionSerialised`,
`scan.TestSerialKeysNormalisationSharesDirectoryKey`,
`pool.TestRunCancelWakesWorkersParkedOnKey`,
`pool.TestRunDuplicateKeysOnOneItem`.

## What scan looks at

Filename: bidi controls and every other Unicode format character (the
zero-width and other invisible ones), double extensions, executable bit.
Container: magic bytes versus extension, executable or archive signatures in the
first and last MiB (polyglots), truncated MP4, ffprobe and `mkvmerge -J`
parseability with warnings tolerated. Streams: no video, no audio, unknown or
data streams outside the allow list, attachments by policy with payload
sniffing, HDR and Dolby Vision presence with the colour description, the
mastering display and content light values and the Dolby Vision profile listed
in the finding's detail. Metadata: links in title, tags, chapter
names, attachment names, text subtitle tracks; MP4 purchase and identifier
atoms; XMP boxes. Optional: ClamAV, full decode.

## What amuxify does not do

It does not rewrite subtitle text, does not transcode, does not rename, does
not delete media (only blocked sidecars, and only with an explicit flag), does
not rewrite or delete `.nfo` files because of their contents, does not strip
music tags unless `clean --strip-audio-tags` asks for it, does not phone home,
and keeps no journal. Keep originals until you have looked at the output.
