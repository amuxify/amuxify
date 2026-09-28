# Hooks and unattended runs

amuxify never prompts and always exits with a stable code, so it can be called
from any post-processing script. `amuxify hook sabnzbd`, `nzbget`, `sonarr` and
`radarr` go one step further: each adapter reads the environment its caller
sets, runs `ingest` on the finished download and exits the way that caller
expects, so the wrapper script is one line.

Exit codes: 0 PASS, 1 WARN, 2 usage error, 3 FAIL, 4 BLOCK, 130 interrupted.
`--json` gives the full report on stdout; `--quiet` suppresses the text report.
Under `--json` stdout carries exactly one JSON document and nothing else: the
adapter's own lines (the start line, the skipping line and SABnzbd's closing
count line) move to stderr. The hook adapters add `--json-out <file>`, which
writes the JSON report to a file while stdout keeps the lines the caller logs.
A skipped run (a status that is not successful, an event that carries no
files, or a category that does not match `--category`) never starts ingest:
it writes only the skipping line, to stdout normally and to stderr under
`--json`, exits 0 (95 for NZBGet), and produces no JSON document and no
`--json-out` file either way.

## The adapters

| Adapter | What it reads | When it skips | Exit codes |
|---|---|---|---|
| `hook sabnzbd` | `SAB_COMPLETE_DIR` (the directory to ingest), `SAB_PP_STATUS`, `SAB_FINAL_NAME`, `SAB_CAT`, `SAB_FAIL_MSG`; without the variables, SABnzbd's seven or eight positional parameters (completed directory first, post-processing status seventh; see the accepted forms below) | `SAB_PP_STATUS` is not `0`, or the category does not match `--category`; exits 0 | 0 when the verdict is below `--fail-on`, 1 at or above it, 2 for a usage error, 130 when interrupted |
| `hook nzbget` | `NZBPP_TOTALSTATUS`, `NZBPP_FINALDIR` or `NZBPP_DIRECTORY` (the first non-empty one), `NZBPP_NZBNAME`, `NZBPP_CATEGORY`, `NZBPP_STATUS` | `NZBPP_TOTALSTATUS` is not `SUCCESS`, or the category does not match `--category`; exits 95 | 93 when the verdict is below `--fail-on`, 94 at or above it, 95 when skipped, 94 for a usage error or an interruption |
| `hook sonarr` | `sonarr_eventtype`, `sonarr_episodefile_path` or `sonarr_episodefile_paths`, `sonarr_series_originallanguage`, `sonarr_series_title`, `sonarr_release_title` | every event other than `Download` and `Test`; exits 0 | 0 when the verdict is below `--fail-on`, 1 at or above it, 2 for a usage error, 130 when interrupted |
| `hook radarr` | `radarr_eventtype`, `radarr_moviefile_path`, `radarr_movie_originallanguage`, `radarr_movie_title` | every event other than `Download` and `Test`; exits 0 | as `hook sonarr` |

A BLOCK verdict produces the failure code for every `--fail-on` value. Each
adapter prints one line `amuxify hook <adapter>: <label> (<event>)` before the
per-file report, or `amuxify hook <adapter>: skipping, <reason>` when there is
nothing to do. Under `--json` both lines go to stderr instead, so that stdout
is the JSON document alone. A skipped run ends with that line: no report is
built, so stdout stays empty under `--json` and `--json-out` writes no file.
When the run is interrupted the adapter writes
`amuxify hook <adapter>: interrupted` to stderr and exits with the caller's
interruption code (130, or 94 for NZBGet). When the environment is missing
altogether (for example `SAB_COMPLETE_DIR` unset and no arguments, or
`sonarr_eventtype` unset) the adapter reports that it was not started by that
program and exits with the usage code.

The hook flags are `--fail-on warn|fail|block` (default `fail`), `--category
<glob>`, `--json-out <file>` and the ingest flags `--verify`, `--hardlinks`,
`--quarantine` and `--remove-blocked-sidecars`. There is no `--force`, because
an unattended run never overrides a FAIL, and no `--original-language`, because
Sonarr and Radarr supply it from the series or movie record. Quarantine is off
by default for every adapter. The JSON report of a hook run carries a `hook`
object with the adapter, event, label, category, `fail_on` and `exit_code`; see
[report.md](report.md).

## Which one to use

Run the hook in the download client when you can. SABnzbd and NZBGet call the
script before Sonarr or Radarr import the download, so a FAIL or BLOCK verdict
fails the job and nothing reaches the library. The Sonarr and Radarr adapters
run after the import; they sanitise the file that is already in the library and
cannot block the import, because Sonarr and Radarr only look at the exit code
of the Test event. Use them when the download client is not yours to configure,
or for imports that do not come through SABnzbd or NZBGet. When no hook can
run at all, because amuxify cannot be installed next to the client, `amuxify
watch` polls the download folder from its own container or from the host; see
[Watching a folder](#watching-a-folder).

## SABnzbd

The wrapper is `contrib/hooks/amuxify-sabnzbd.sh`:

```sh
#!/bin/sh
# SABnzbd: put this file in the scripts folder and assign it per category.
# Enable "script_can_fail" in Config, Special to let exit 1 fail the job.
exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook sabnzbd "$@"
```

Copy it into the scripts folder that SABnzbd's configuration names, make it
executable, and assign it to each category that should be gated (Config,
Categories). SABnzbd runs the script after unpacking, with the completed
directory in `SAB_COMPLETE_DIR` and the post-processing status in
`SAB_PP_STATUS`. A status other than `0` means the download is not usable
(failed verification, unpack or repair); the adapter prints a skipping line
that quotes `SAB_FAIL_MSG` when SABnzbd set it, and exits 0 so the failure
stays SABnzbd's own.

### Accepted forms

The adapter reads the job in one of two forms, and refuses everything else
with exit code 2 and a message that names the problem.

The environment form applies whenever `SAB_COMPLETE_DIR` is set, which every
supported SABnzbd does. The adapter then also requires `SAB_PP_STATUS`; a
job with the directory but no status is refused rather than assumed
successful. `SAB_FINAL_NAME`, `SAB_CAT` and `SAB_FAIL_MSG` are optional. When
the environment form applies the positional parameters are not read, but
their count is still checked, since SABnzbd passes both and a wrong count
means the wrapper line is wrong. Nothing in the positional parameters is
looked up on disk in this form.

The positional form applies when `SAB_COMPLETE_DIR` is not set, and accepts
exactly the parameters SABnzbd passes to a script, in SABnzbd's order: the
final directory of the job, the name of the original NZB file, the clean job
name, the indexer's report number, the category, the newsgroup, the
post-processing status, and, from the SABnzbd version that added it, the
failure URL. Seven parameters are read as an older SABnzbd's call and eight
as a current one's. The status must not be empty in either form. The
directory comes from the first parameter, the label from the third (or the
second when the third is empty), the category from the fifth and the status
from the seventh; the others are ignored.

No other count of positional arguments is accepted. In particular a lone
directory is not a way to name the directory to ingest: `hook sabnzbd
/downloads/job` is a usage error, and so are six, nine or more parameters.
A wrapper that writes `hook sabnzbd /q "$@"` or `hook sabnzbd "$@" /q`
against a current SABnzbd gives nine arguments and is refused on count. In
the positional form one more shape is refused: a directory in front of an
older SABnzbd's seven parameters, which has the right count but would put
the added directory where the job's own belongs. SABnzbd's second parameter
is an NZB file name, so an existing directory in that place shows that a
directory was added in front, and the job is refused rather than the wrong
directory scanned. The first parameter is not required to exist, since a
job directory may be gone by the time the script runs.

The eighth parameter, the failure URL, is never inspected in either form.
SABnzbd copies it from the `X-DNZB-Failure` header of the indexer's NZB
response, so it is the one parameter an indexer writes, and a check that
refused the job on its value, for instance because it named an existing
directory, would let an indexer have every job it serves refused before the
scan. A directory written after an older SABnzbd's seven parameters is
therefore read as the failure URL and ignored, and the job's own directory
is still scanned.

When a run is refused because of its arguments, the message names the
count or the directory found in the wrong place. When a bare `--quarantine`
is set and the arguments show which one the wrapper added, the message also
says to write `--quarantine=<dir>` with that argument, because that is the
usual reason for a stray directory. The hint only ever names a value the
wrapper added, never one of SABnzbd's own parameters: with nine arguments
the added directory is the first when the last is empty or a URL, as
SABnzbd's failure URL is, and the last when the eighth is, and no hint is
printed when neither reading fits.

Flags such as `--quarantine=DIR`, `--fail-on`, `--category` and
`--remove-blocked-sidecars` are recognised in any position before the first
positional parameter, and the global flags such as `--profile` and
`--dry-run` may go before `hook` or in the same position as the hook flags.
After the first positional parameter, or after `--`, every argument is a
parameter, so a directory named like a flag is a directory. Every value in
either form is carried as data: nothing is split, expanded or executed,
control characters are escaped in the log lines, and a directory that is a
symlink is reported as `WARN SYMLINK` and never followed.

SABnzbd only fails a job on a non-zero script exit when its `script_can_fail`
setting is on; turn it on. With it, every non-zero exit fails the job: 1 for a
verdict at or above `--fail-on`, 2 for a usage error and 130 for an
interruption. SABnzbd reports a failed job to Sonarr and Radarr as a failed
download, and they blocklist the release and search for another one. That is
what you want for a BLOCK, which means an executable payload, a polyglot or a
blocked sidecar. If a damaged or noisy file should not cost you the release,
pass `--fail-on block` on the `exec` line: a FAIL then leaves the job
successful, the file is imported and its findings stay in the log.

SABnzbd shows the last line the script printed next to the exit code, so the
adapter always ends its output with one line of the form

```
amuxify: FAIL, 3 file(s)
```

which SABnzbd displays as `Exit(1): amuxify: FAIL, 3 file(s)`. The line is
omitted under `--quiet`. Under `--json` it goes to stderr together with the
start line, so that stdout holds the JSON document alone; use `--json-out
<file>` when you want the report as a file and the log lines on stdout.

`--category <glob>` limits the adapter to jobs whose `SAB_CAT` matches the
pattern (case-insensitive, `path.Match` syntax, so `tv*` matches `tv` and
`tv-4k`). Other jobs print a skipping line and exit 0. A job that carries no
category at all is processed regardless of the flag. Assigning the script per
category in SABnzbd does the same job without the flag; the flag is for one
script shared by several categories.

## NZBGet

The wrapper is `contrib/hooks/amuxify-nzbget.sh`. The signature block is
mandatory: NZBGet reads it to recognise a post-processing script and to show
the options in its web interface, which arrive as `NZBPO_PROFILE` and
`NZBPO_FAILON`.

```sh
#!/bin/sh

##############################################################################
### NZBGET POST-PROCESSING SCRIPT                                          ###

# Verify and sanitise the finished download with amuxify.
#
# amuxify scans every file, rebuilds video into verified MKV or cleans it in
# place, and fails the job when a file fails or is blocked.

##############################################################################
### OPTIONS                                                                ###

# Profile (homelab, anime, archive, strict, or a path to a TOML file).
#Profile=homelab

# Verdict that fails the job (warn, fail, block).
#FailOn=fail

### NZBGET POST-PROCESSING SCRIPT                                          ###
##############################################################################

exec amuxify --profile "${NZBPO_PROFILE:-${AMUXIFY_PROFILE:-homelab}}" hook nzbget --fail-on "${NZBPO_FAILON:-fail}"
```

Copy it into the directory that NZBGet's `ScriptDir` setting names and make it
executable. NZBGet lists the scripts it found under Settings, Extension
Scripts, and each category has its own list of the extensions that run for it;
tick the script there for every category that should be gated. The `NZBPO_*`
options are the wrapper's business: amuxify never reads them.

NZBGet requires exit codes of its own: 93 means the post-processing succeeded,
94 that it failed, 95 that the script had nothing to do. The adapter never
returns anything else. A verdict below `--fail-on` is 93, a verdict at or above
it is 94, a download whose `NZBPP_TOTALSTATUS` is not `SUCCESS` is 95, and a
usage error or an interruption is 94 with an `[ERROR]` line explaining it.

NZBGet reads the script's stdout line by line and takes a leading `[INFO]`,
`[WARNING]`, `[ERROR]`, `[DETAIL]` or `[DEBUG]` tag as the log level of that
line. The adapter therefore prefixes every stdout line with `[INFO] ` and every
stderr line with `[ERROR] `, so the per-file verdict lines land in the NZBGet
log at the level you expect. Two consequences: `--json` is a usage error for
this adapter (the message says to use `--json-out <file>`), and `--trace` lines
would appear as `[ERROR]` lines in the log, so run `amuxify ingest --trace` by
hand when you need to see the tool command lines. A stdout line that would
begin with `[NZB]` for any other reason, for example a file name that contains
a newline, is written as `[INFO] (not a command) [NZB] ...`, so only the
adapter itself can hand NZBGet a command.

When the run verdict is BLOCK the adapter prints `[NZB] MARK=BAD` as its last
stdout line, whatever `--fail-on` says. NZBGet then marks the download as bad,
and Sonarr and Radarr treat a bad download as a failed one: they blocklist the
release and search for another. A WARN or FAIL verdict never prints that line,
so a file that is merely damaged or noisy fails the job without making the arr
re-grab the release; a BLOCK, which means an executable payload, a polyglot or
a blocked sidecar, is exactly the case where a replacement is wanted.

To keep a JSON report of each run, add `--json-out` with a path that is new for
each run, for example `--json-out "/reports/$NZBPP_NZBID.json"`, to the `exec`
line; the log lines are unaffected. An existing file is never overwritten.

## Sonarr and Radarr

The wrappers are `contrib/hooks/amuxify-sonarr.sh` and
`contrib/hooks/amuxify-radarr.sh`:

```sh
#!/bin/sh
# Sonarr: Settings, Connect, Custom Script. Leave Arguments empty.
# Tick On Import, On Upgrade and On Import Complete. Test runs a tool check.
exec amuxify --profile "${AMUXIFY_PROFILE:-homelab}" hook sonarr
```

The Radarr wrapper says Radarr and `hook radarr`, and its comment ticks On
Import and On Upgrade. Add the script under Settings, Connect, Custom Script,
point Path at the wrapper and leave Arguments empty; the adapter reads
everything from the environment. Tick On Import and On Upgrade, and in Sonarr
also On Import Complete, which arrives with every file of a multi-episode
import in `sonarr_episodefile_paths`. Every other event (Grab, Rename, health
events, application update and so on) prints a skipping line and exits 0.

The Test button runs a tool check. The adapter loads the profile and looks for
ffmpeg, ffprobe, mkvmerge and mkvpropedit, prints
`amuxify <version>: hook sonarr ready (profile <name>; ffmpeg: <version line>; mkvmerge: <version line>)`
and exits 0, or prints `amuxify hook sonarr: test failed: <error>` on stdout and
stderr and exits 1 so Sonarr shows the failure. Sonarr and Radarr inspect the exit code only for
Test; for an import the file is already in the library whatever the verdict.

Sonarr passes the original language of the series in
`sonarr_series_originallanguage` and Radarr the movie's in
`radarr_movie_originallanguage`. The adapter hands it to the profile's
`languages.prefer_original`, so the first audio track in that language becomes
the default track, the same as `ingest --original-language` would.

Torrent imports are usually hard links: the library name and the seeding name
point at the same bytes. Under the default `safety.hardlinks = "skip"` a
hard-linked video file is left alone with `WARN HARDLINKED`, because editing it
in place would change the seeding copy too. Pass `--hardlinks break` on the
`exec` line to rebuild the library copy into a new file and replace only the
library's name, leaving the seeding copy untouched. Hard-linked audio and
subtitle files are always skipped, since those are only ever edited in place.

When a non-MKV file is rebuilt its extension changes to `.mkv`. Sonarr and
Radarr keep the old path in their database until the next series or movie
refresh, so run a rescan (or enable the periodic refresh) after enabling the
hook. If you would rather keep the container, run `amuxify hook` from the
download client instead, where the rename happens before import.

Sonarr and Radarr log a script's stdout at Debug level and its stderr at Error
level, so the per-file report is visible when the log level is set to Debug.
Routine output never goes to stderr; when the verdict reaches `--fail-on` the
adapter writes the count line (`FAIL: 3 file(s) FAIL=1 PASS=2`) to stderr as
well, so a failed ingest shows up in the arr's log at Error level without
raising the log level.

## Options

`--fail-on warn|fail|block` (default `fail`) chooses the verdict from which the
caller sees a failure. BLOCK is a failure under every value.

| `--fail-on` | Caller sees success | Caller sees failure |
|---|---|---|
| `warn` | PASS | WARN, FAIL, BLOCK |
| `fail` (default) | PASS, WARN | FAIL, BLOCK |
| `block` | PASS, WARN, FAIL | BLOCK |

`--quarantine` moves files that scan as BLOCK into a mirrored tree under a
directory, before anything else is done with the download. `--quarantine=DIR`
names the directory; a bare `--quarantine` uses `<state-dir>/quarantine`
(see [install.md](install.md) for the state directory); `--quarantine=off`
turns it off again. The directory form must use `=`, because the flag also
works without a value. A directory written after a bare `--quarantine` is not
read as its value: `hook sonarr`, `hook radarr` and `hook nzbget` take no
positional arguments at all, and `hook sabnzbd` takes none or SABnzbd's seven or
eight parameters, so the stray word is a usage error, and the message says to write
`--quarantine=<dir>` instead. Nothing runs before that check. Quarantine is
cleared under `--dry-run`. The quarantine directory must lie outside the
paths being processed: a path that is the quarantine directory or lies inside
it is refused as a usage error before anything runs, and a quarantine
directory that sits inside the tree is skipped by the walk, so a file that
was quarantined by an earlier run is never scanned or moved again. Both
checks recognise the directory by what it is, not by how it is written, so a
trailing slash, a `..` component, a relative path, a symlink or, on a
case-insensitive filesystem, a different letter case names the same
directory.

`--remove-blocked-sidecars` deletes sidecar files whose extension is on the
profile's block list (`SIDECAR_BLOCKED`). Without it they are reported and left
in place. Nothing else is ever deleted.

`--verify quick|full|none` overrides the profile's `verify.tier`. `none` is
refused: ingest writes in place and requires verification, and the refusal
names the flag or the profile that asked for it.

`--hardlinks skip|break|copy` overrides the profile's `safety.hardlinks`:
`skip` leaves a hard-linked file alone with `WARN HARDLINKED`, `break` rebuilds
it and replaces only this name, `copy` writes the rebuilt file to the output
tree and leaves the original alone.

`--category <glob>` (SABnzbd and NZBGet) acts only on jobs whose category
matches; the match is case-insensitive and uses `path.Match` patterns. A job
that carries no category is processed regardless of the flag, and Sonarr and
Radarr never pass one, so the flag is ignored for them.

`--json-out <file>` writes the JSON report to a new file in addition to whatever
the global flags print. The file must not exist yet: amuxify never overwrites
it and never follows a symlink in its place, so give each run its own path. A
write failure is reported on stderr as `amuxify: json-out: <error>` and does
not change the exit code.

The global flags apply before the subcommand as everywhere, and are accepted
after it as well: `--profile`, `--json`, `--dry-run`, `--verbose`, `--quiet`,
`--timeout`, `--state-dir`, `--allow-root`, `--trace` and `--jobs`. A hook
accepts `--jobs` but always processes one file at a time, whatever value is
given: the download client decides how many scripts run at once, and one
script that fans out would take the machine away from the others. `--dry-run`
reports what each file would get (`ROUTE` under `--verbose` shows the plan)
and changes nothing. `AMUXIFY_PROFILE` sets the default profile; every
wrapper passes it through with `homelab` as the fallback, so setting that
variable in the client's environment is enough to switch profiles without
editing the script. The NZBGet wrapper puts its own
`NZBPO_PROFILE` option first and falls back to `AMUXIFY_PROFILE` and then
`homelab`, like the others.

## cron or systemd timer over an incoming folder

```sh
amuxify scan --quarantine /srv/media/quarantine /srv/media/incoming || true
amuxify ingest /srv/media/incoming
```

`--quarantine` moves BLOCK files into a mirrored tree under the given directory
so they stop being picked up, without deleting anything. The directory may sit
inside the incoming tree, as above; the walk skips it. `ingest` then rebuilds
or cleans what is left, in place, and exits with the worst verdict of the run.

A timer runs ingest over the whole folder at fixed times and has no way to
tell a finished download from one that is still being written. When the
folder receives files at any time, `amuxify watch` below does the same work
and waits for each file to stop changing first.

## Watching a folder

```
amuxify [global flags] watch [--interval 5s] [--settle 30s] [--once] [ingest flags] <dir>
```

`watch` polls one directory and runs `ingest` on each file once it has stayed
unchanged for the settle window. It takes the ingest flags (`--quarantine`,
`--profile` as a global flag, `--verify`, `--hardlinks`, `--force`, `--dry-run`,
`--remove-blocked-sidecars`) and treats each file exactly as `ingest` would,
with the same walker, the same quarantine exclusion and the same symlink rules.
As with `ingest` and the hooks, the quarantine directory is written as
`--quarantine=DIR`; a bare `--quarantine` followed by a directory leaves that
directory as a stray positional argument and the watcher refuses to start.
Files are handed to `ingest` one at a time, so `--jobs` is accepted and has no
effect under `watch`, as it has none under a hook. It uses polling only, so it
works on any filesystem, including network shares and Docker bind mounts, where
change notification is unreliable or absent.

Use `watch` when the hook cannot run inside the download client, for example
when the client is a stock container you do not want to rebuild, or when the
files arrive by other means (an rsync target, an SFTP drop, a USB copy). Prefer
the hook when it is available: the hook knows when the download is complete
and can fail the job, while a watcher can only infer completion from the file
having stopped changing, and it cannot stop the client from importing the file
in the meantime.

Every pass walks the directory and notes the size, modification time and
identity (device and inode) of every regular file. A file is ingested once it
has been seen unchanged in all three for at least `--settle`, and then only
once for that content: it is not ingested again on the next pass. A file that
changes later, by growing, being rewritten or being replaced by a different
file under the same name, is a new version and is ingested again once it has
settled. That includes a file amuxify itself rebuilt or cleaned: the watcher
does not try to tell its own writes from someone else's, so the rewritten file
is looked at once more after it settles, and that second ingest finds nothing
left to change. That second look is a full ingest, with the probe and the
decode check the first one ran, so a file the watcher rebuilds or edits costs
roughly twice the processing of the same file under `ingest`. The watcher
accepts that cost rather than trusting its own writes, because a change from
someone else that lands while amuxify is writing would otherwise be
missed. A file that is still growing is never ingested. Right before
each ingest the watcher checks the path again: it must be the same regular
file it decided on, still inside the watched directory, reached through real
directories and not through a symlink. A file that vanished before its turn
is dropped without a report. Symlinks are listed but never followed, and each
one is noticed once on stderr unless `--quiet` is given. A subdirectory that
cannot be read is reported once, and again only when the error changes. The
watcher keeps only the files present at the last pass in memory, so a folder
that files pass through does not grow its footprint.

Per-file verdicts stream in the same human format as `ingest`, as each file
finishes, and a pass that ingested at least one file ends with the usual count
line. Under `--json` each such pass writes one complete `amuxify.report/1`
document on a single line, with `command` set to `ingest`, so a consumer reads
newline-delimited reports and can parse each line as it arrives; the watcher's
own lines go to stderr. A pass that ingested nothing and met no new error
prints nothing, so a quiet folder stays quiet; an unreadable subdirectory is
reported once, with a count line or a document of its own, when it appears or
changes. The command starts with one line naming the directory, the interval
and the settle window.

Exit behaviour: without `--once` the command runs until it is interrupted
(SIGINT or SIGTERM). On an interrupt it finishes the file it is working on,
reports it, starts no further file, prints `amuxify watch: interrupted,
stopped` and exits 0, whatever the verdicts were. The file in progress is
finished for real: a tool that is rebuilding or editing it is left to run to
its end, bounded only by its usual timeout, so an interrupt never leaves a
half-written header behind. No temporary file is left behind, because a file
in progress is either finished or its temporary output is removed, as in
every other command. With `--once` the command makes one
pass, waits one settle window, makes a second pass that ingests what has
settled, and exits with the worst verdict of the run using the usual codes (0
PASS, 1 WARN, 3 FAIL, 4 BLOCK, 130 interrupted). A `--once` run therefore
takes at least `--settle`; `--settle 0s` ingests everything present in a single
pass, which is the cron case from above with the same command line as the
continuous watcher.

`watch` refuses the quarantine directory itself, or a directory inside it, as
the watched directory, and refuses a symlink in place of the directory, both
before any tool runs. A quarantine directory inside the watched tree is not
entered, exactly as with `ingest`, so a blocked file that was moved there is
not picked up again. `--verify none` is refused. The root guard is unchanged:
`watch` refuses to run as root unless `--allow-root` is given.

A compose set-up runs the watcher as its own service over the volume the
download client writes to. The download client is any stock image; the watcher
is the plain amuxify image, run as the uid and gid that own the folder:

```yaml
services:
  sabnzbd:
    image: lscr.io/linuxserver/sabnzbd:latest
    environment:
      PUID: "1000"
      PGID: "1000"
    volumes:
      - /srv/media/downloads:/downloads

  amuxify:
    image: ghcr.io/amuxify/amuxify
    user: "1000:1000"
    command: >
      watch --interval 10s --settle 60s
      --quarantine=/downloads/quarantine --remove-blocked-sidecars
      /downloads/complete
    volumes:
      - /srv/media/downloads:/downloads
    restart: unless-stopped
    stop_grace_period: 30m
```

The watcher sees the folder the client writes to, waits until each file has
been unchanged for a minute, and then verifies and sanitises it in place; a
BLOCK file is moved under `/downloads/quarantine`, which the walk never
enters. Pick a settle window longer than the longest pause your client makes
between two writes to one file; the default of 30 seconds is enough for a
client that writes a download in one go, while a client that assembles a file
from parts may pause longer while it repairs or unpacks. With `--json` the
service's stdout is one report document per line, so `docker compose logs -f
amuxify` or a log shipper can follow it. The container stops with exit code 0
when compose sends its stop signal, once the file in progress is finished.
Set `stop_grace_period` longer than the longest time a single file takes to
process, because after that period Docker sends SIGKILL, which ends the
watcher and its tool at once like a crash would: a rebuild leaves its
temporary file behind, which the next pass ignores, and an in-place header
edit can be cut short. Compose's default of ten seconds is far too short for
a remux.

## Docker

The `ghcr.io/amuxify/amuxify` image carries the four wrappers under
`/usr/share/amuxify/hooks/`. The release archives carry them under
`contrib/hooks/`.

A hook has to run inside the container that calls it, so the simplest layout is
a derived image: start from the client's image, add ffmpeg and MKVToolNix, and
copy the binary and the wrapper out of the amuxify image. The wrapper goes
under `/usr/local/share/amuxify/hooks/`, outside `/config`, so that mounting
`/config` from the host (which every linuxserver.io image expects) does not
hide it. This is `contrib/hooks/Dockerfile.sabnzbd`:

```Dockerfile
FROM lscr.io/linuxserver/sabnzbd:latest
RUN apk add --no-cache ffmpeg mkvtoolnix
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/local/bin/amuxify /usr/local/bin/amuxify
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/share/amuxify/hooks/amuxify-sabnzbd.sh /usr/local/share/amuxify/hooks/
```

The same four lines work for the other three images; change the base image and
the wrapper name:

```Dockerfile
FROM lscr.io/linuxserver/nzbget:latest
RUN apk add --no-cache ffmpeg mkvtoolnix
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/local/bin/amuxify /usr/local/bin/amuxify
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/share/amuxify/hooks/amuxify-nzbget.sh /usr/local/share/amuxify/hooks/
```

```Dockerfile
FROM lscr.io/linuxserver/sonarr:latest
RUN apk add --no-cache ffmpeg mkvtoolnix
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/local/bin/amuxify /usr/local/bin/amuxify
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/share/amuxify/hooks/amuxify-sonarr.sh /usr/local/share/amuxify/hooks/
```

```Dockerfile
FROM lscr.io/linuxserver/radarr:latest
RUN apk add --no-cache ffmpeg mkvtoolnix
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/local/bin/amuxify /usr/local/bin/amuxify
COPY --from=ghcr.io/amuxify/amuxify:0.4.0 /usr/share/amuxify/hooks/amuxify-radarr.sh /usr/local/share/amuxify/hooks/
```

Then point the client at the wrapper where the image put it. In SABnzbd set
the scripts folder (Config, Folders) to `/usr/local/share/amuxify/hooks` and
assign `amuxify-sabnzbd.sh` per category; in NZBGet set `ScriptDir` to
`/usr/local/share/amuxify/hooks`; in Sonarr and Radarr give the Custom Script
the path `/usr/local/share/amuxify/hooks/amuxify-sonarr.sh` or
`amuxify-radarr.sh`. If the client already has a scripts folder under
`/config` that you want to keep, copy or symlink the wrapper into it from
the container, or copy it from `contrib/hooks/` in the release archive into the
mounted folder on the host. Run the derived container with the `PUID` and
`PGID` of the user that owns the library, as you would the plain image, so
that amuxify writes files with the right owner and does not refuse to run as
root.

When you cannot rebuild the client's image, run amuxify as a sidecar: a second
container with the same paths mounted at the same locations, started by a cron
job or a systemd timer on the host rather than by the client:

```sh
docker run --rm -u 1000:1000 -v /srv/media:/srv/media \
  ghcr.io/amuxify/amuxify ingest /srv/media/incoming
```

Pass `-u` with the uid and gid that own the library, never root.

Hard-linked files (most torrent setups) are skipped in place by default so
seeding continues. Set `safety.hardlinks = "copy"` to write a cleaned copy
elsewhere, or `"break"` to replace only the library's name.

## Troubleshooting

- The Sonarr or Radarr Test button fails. The message names the missing tool
  or the profile error. Run `amuxify doctor` inside the same container, as the
  same user, and fix what it reports; the Test runs the same checks.
- Nothing happens when a download finishes. Look for the skipping line in the
  client's log. For Sonarr and Radarr, check that On Import (and On Import
  Complete) is ticked; other events are skipped by design. For SABnzbd and
  NZBGet, check that the script is assigned to the job's category and that
  `--category`, if given, matches it. A SABnzbd job with `SAB_PP_STATUS` other
  than `0`, or an NZBGet job whose total status is not `SUCCESS`, is skipped
  because its files are not usable.
- A file was renamed to `.mkv` and Sonarr or Radarr lost track of it. The
  rebuild changed the extension; run a rescan of the series or movie, or enable
  the periodic refresh, and the new path is picked up. Running the hook from
  the download client avoids this, because the rename happens before import.
