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
| `amuxify doctor` | Check tools, version floors, profile and environment | nothing |
| `amuxify profile [show <name>]` | List or print built-in profiles | nothing |

Input containers: MKV, WebM, MP4, M4V, MOV, AVI, MPEG-TS, M2TS, MPG, VOB, FLV.
Output is always Matroska written by mkvmerge. ffmpeg never writes an MKV.

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
[releases page](https://github.com/nxame/amuxify/releases) with SHA-256 sums.

```sh
# script: downloads, verifies the checksum, installs to /usr/local/bin
curl -fsSL https://raw.githubusercontent.com/nxame/amuxify/main/install.sh | sh

# Homebrew
brew install nxame/tap/amuxify

# Docker (run as the uid that owns the library, never root)
docker run --rm -u 1000:1000 -v /srv/media/incoming:/data ghcr.io/nxame/amuxify scan /data

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
8. `--verify none` is refused together with `--in-place`.
9. `BLOCK` cannot be overridden by any flag.
10. Refuses to modify files as root unless `--allow-root`.

Each guarantee has a test. Details in [docs/safety.md](docs/safety.md).

## Unattended use

amuxify never prompts. Untagged-language tracks follow `languages.und` in the
profile (`keep`, `drop`, or `assume:<lang>`). Hook adapters for SABnzbd, NZBGet,
Sonarr and Radarr arrive in 0.3; until then call `amuxify scan` or
`amuxify remux --in-place` from your post-processing script and branch on the
exit code. Examples in [docs/hooks.md](docs/hooks.md).

## Upgrading from 0.1.x

The Bash scripts are frozen under `legacy/`. `amux-scan`, `amux-scan-all`,
`amux-remux` and `amux-clean` are shims that call `amuxify --profile archive`
for one release. Behaviour changes are listed in [MIGRATION.md](MIGRATION.md).

## Documentation

- [docs/install.md](docs/install.md), [docs/profiles.md](docs/profiles.md), [docs/safety.md](docs/safety.md)
- [docs/hooks.md](docs/hooks.md), [docs/report.md](docs/report.md), [docs/comparison.md](docs/comparison.md)
- [docs/design.md](docs/design.md), [MIGRATION.md](MIGRATION.md), [CHANGELOG.md](CHANGELOG.md)
- [SECURITY.md](SECURITY.md), [CONTRIBUTING.md](CONTRIBUTING.md)

## License

BSD-3-Clause. See [LICENSE](LICENSE).
