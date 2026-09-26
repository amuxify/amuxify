<img src="site/amuxify-logo.png" alt="amuxify logo" width="96" align="right">

# amuxify

**The ingest gate for self-hosted media.** Verify, sanitize, normalize, prove.

amuxify sits between "downloaded or purchased" and "in the library". It runs once
per file, changes nothing it cannot prove is safe, and exits with a verdict your
download client, Sonarr, Radarr or cron job can act on.

```
$ amuxify scan ~/incoming
PASS  /home/me/incoming/Show.S01E01.mkv
WARN  /home/me/incoming/Movie.2019.mkv
      WARN  LINK_IN_TAG        1 link(s) in metadata: tag COMMENT: http://tracker.example/x
BLOCK /home/me/incoming/Movie.2019.Sub.mkv
      BLOCK ATTACH_EXEC        attachment #1 "font.ttf" (font/ttf, 71 KiB) contains executable PE/DOS executable

BLOCK: 3 file(s) BLOCK=1 PASS=1 WARN=1
```

It is not a transcoder, a renamer or a library manager. Streams are never
re-encoded. Every remux is verified by hashing each kept stream against the
source before the output is placed.

> Keep originals until you have checked the output yourself. See [docs/safety.md](docs/safety.md).

## Commands

| Command | What it does | Writes |
|---|---|---|
| `amuxify scan <path>...` | Inspect files and sidecars, report findings, verdict per file | nothing (unless `--quarantine`) |
| `amuxify remux <path>...` | Rebuild any supported container into a sanitized MKV with mkvmerge, verify, place | `<root>__remuxed/` or `--output`, or `--in-place` |
| `amuxify clean <path>...` | Strip metadata, provenance atoms and extended attributes in place, tracks untouched | the file, after stream-hash verification |
| `amuxify ingest <path>...` | Scan, then rebuild into a verified MKV or clean in place, one pass per file | the file in place, after verification |
| `amuxify hook sabnzbd\|nzbget\|sonarr\|radarr` | Run ingest from a download client or media manager script and exit the way that caller expects | as ingest |
| `amuxify doctor` | Check tools, version floors, profile and environment | nothing |
| `amuxify profile [show <name>]` | List or print built-in profiles | nothing |

Input containers: MKV, WebM, MP4, M4V, MOV, AVI, MPEG-TS, M2TS, MPG, VOB, FLV.
Output is always Matroska written by mkvmerge. ffmpeg never writes an MKV.

## Ingest

`amuxify [global flags] ingest [flags] <path>...` is scan, then remux or clean, in one
pass per file, in place. Every file under each path is scanned first. A video file that
is already a Matroska file whose tracks, flags, attachments and chapters match the
profile is cleaned in place with mkvpropedit; any other video file is rebuilt into a
verified MKV that replaces the original. Audio and subtitle files are cleaned in place.
Sidecars are scanned; with `--remove-blocked-sidecars` blocked ones are deleted. A file
that scans as BLOCK is never modified; a file that scans as FAIL is left alone as well
unless `--force` asks for the rebuild anyway. There is no `--output` and no
`--in-place` flag: ingest is in place by definition; `remux --output` is the
non-destructive path and `--dry-run` shows the plan.

## Verdicts and exit status

| Verdict | Exit | Meaning |
|---|---|---|
| `PASS` | 0 | Nothing to report |
| `WARN` | 1 | Something to know about; file is usable |
| usage | 2 | Bad arguments or missing tool |
| `FAIL` | 3 | Integrity problem: unparseable, truncated, hash mismatch, mislabeled |
| `BLOCK` | 4 | Security problem: executable payloads, polyglots, bidi names, blocked sidecars. Never overridable |
| interrupted | 130 | Ctrl-C or SIGTERM; temp files removed |

The worst verdict of the run is the exit code. `--json` prints the full report on
stdout with the same codes; the schema is documented in [docs/report.md](docs/report.md).

## Profiles

A profile is a TOML file. Four are built in; `homelab` is the default.

| Profile | Languages | Chapters | Fonts | Commentary | Links / provenance | Verify |
|---|---|---|---|---|---|---|
| `homelab` | keep all | keep | keep if text subs | keep | warn | quick |
| `anime` | keep all, prefer original | keep | keep | drop | warn | quick |
| `archive` | English only | drop | drop | drop | fail | quick |
| `strict` | English only | drop | drop | drop | fail | full + ClamAV |

```sh
amuxify profile show homelab > my.toml   # edit, then
amuxify --profile ./my.toml remux ~/incoming
```

Every key is documented in [docs/profiles.md](docs/profiles.md). Unknown keys are
rejected so a typo cannot silently widen a policy.

## Install

Binaries for Linux (amd64, arm64, armv7) and macOS (arm64, amd64) are on the
[releases page](https://github.com/amuxify/amuxify/releases) with SHA-256 sums.

```sh
# script: downloads, verifies the checksum, installs to /usr/local/bin
curl -fsSL https://raw.githubusercontent.com/amuxify/amuxify/main/install.sh | sh

# Homebrew
brew install --cask amuxify/tap/amuxify

# Docker (run as the uid that owns the library, never root)
docker run --rm -u 1000:1000 -v /srv/media/incoming:/data ghcr.io/amuxify/amuxify scan /data

# from source
make build && ./bin/amuxify doctor
```

amuxify drives external tools: **MKVToolNix 50+** (mkvmerge, mkvpropedit) and
**ffmpeg 4.4+** (5.0+ recommended). exiftool and clamscan are optional.
`amuxify doctor` tells you what is missing. See [docs/install.md](docs/install.md).

## Safety guarantees

1. Never overwrites an existing path; a collision is a `FAIL`.
2. Writes a temp file beside the destination, fsyncs, then renames. Never across filesystems.
3. Symlinks are skipped per file, never followed, never abort a tree.
4. Every ffmpeg and ffprobe call carries `-protocol_whitelist file,pipe` and a timeout.
5. Every kept stream's SHA-256 matches source and output, or the output is deleted.
6. In-place mode preserves owner, group, mode and mtime.
7. Extended attribute removal touches only `user.*` (Linux) and `com.apple.*` (macOS).
8. `--verify none` is refused together with `--in-place`, and `ingest` and every hook adapter refuse verify tier `none` whether it comes from the flag or from the profile.
9. `BLOCK` cannot be overridden by any flag.
10. Refuses to modify files as root unless `--allow-root`.

Each guarantee has a test; [docs/safety.md](docs/safety.md) names the test next to each one.

## Unattended use

amuxify never prompts. Untagged-language tracks follow `languages.und` in the
profile (`keep`, `drop`, or `assume:<lang>`). `amuxify hook sabnzbd`, `nzbget`,
`sonarr` and `radarr` read the caller's environment, run `ingest` on the finished
download and exit the way that caller expects. Wrapper scripts, the `--fail-on`
option and Docker notes are in [docs/hooks.md](docs/hooks.md).

## Upgrading from 0.2.0

The JSON report gains `schema` (`amuxify.report/1`) and, for hook runs, `hook`;
`started` and `finished` are UTC with whole seconds; `files`, `findings` and
`errors` are always arrays; `profile` is always present. New finding codes:
`ROUTE`, `NFO_KODI`, `NFO_TEXT`, `LINK_IN_SIDECAR`. No flag or code was removed.

## Upgrading from 0.1.x

0.2.0 replaces the Bash scripts with one binary and a different default
policy. The 0.1.x rules live on as `--profile archive`:

| 0.1.x | 0.2.0 |
|---|---|
| `amux-scan <dir>`, `amux-scan-all <dir>` | `amuxify --profile archive scan <dir>` |
| `amux-scan-all --deep` | `amuxify scan --verify full` |
| `amux-remux <dir>` | `amuxify --profile archive remux <dir>` |
| `amux-clean <dir>` | `amuxify --profile archive clean <dir>` |
| `AMUXIFY_UND_POLICY=drop\|english` | `languages.und = "drop"` or `"assume:eng"` in the profile |

Set `AMUXIFY_PROFILE=archive` to keep the old behaviour everywhere. Exit codes
changed to the table above. The scripts themselves are in the `v0.1.1` tag.

## Documentation

- [docs/install.md](docs/install.md), [docs/profiles.md](docs/profiles.md), [docs/safety.md](docs/safety.md)
- [docs/hooks.md](docs/hooks.md), [docs/report.md](docs/report.md), [docs/comparison.md](docs/comparison.md)
- [docs/design.md](docs/design.md), [CHANGELOG.md](CHANGELOG.md)
- [SECURITY.md](SECURITY.md), [CONTRIBUTING.md](CONTRIBUTING.md)

## License

BSD-3-Clause. See [LICENSE](LICENSE).
