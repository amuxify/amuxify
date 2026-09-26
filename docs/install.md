# Installing amuxify

amuxify is a single static binary that drives external tools. Install the tools
first, then the binary, then run `amuxify doctor`.

## Required tools

| Tool | Minimum | Why |
|---|---|---|
| MKVToolNix (`mkvmerge`, `mkvpropedit`, `mkvextract`) | 50 | the only MKV writer; `--no-date`, IETF language tags, attachment extraction |
| ffmpeg and ffprobe | 4.4 (5.0+ recommended) | probing, stream hashing, decode checks, MP4/AVI metadata rewrite |
| exiftool | any | optional, richer provenance reports |
| clamscan | any | optional, `--clamav` or `safety.clamav = "required"` |

```sh
# Debian / Ubuntu
sudo apt install ffmpeg mkvtoolnix
# Fedora
sudo dnf install ffmpeg mkvtoolnix
# Arch
sudo pacman -S ffmpeg mkvtoolnix-cli
# Alpine
apk add ffmpeg mkvtoolnix
# macOS
brew install ffmpeg mkvtoolnix
```

If a tool lives outside `PATH`, point at it with an environment variable:
`AMUXIFY_FFMPEG`, `AMUXIFY_FFPROBE`, `AMUXIFY_MKVMERGE`, `AMUXIFY_MKVPROPEDIT`,
`AMUXIFY_MKVEXTRACT`, `AMUXIFY_EXIFTOOL`, `AMUXIFY_CLAMSCAN`.

## The binary

### Release binary (Linux, macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/amuxify/amuxify/main/install.sh | sh
# or into your home directory
PREFIX=$HOME/.local sh install.sh
```

The script downloads the archive for your OS and CPU, verifies it against the
`checksums.txt` published with the release, and installs `amuxify` under
`$PREFIX/bin`. Set `VERSION=0.2.0` to pin a version.

### Docker

```sh
docker run --rm -u "$(id -u):$(id -g)" -v /srv/media/incoming:/data \
  ghcr.io/amuxify/amuxify scan /data
```

Always pass `-u` with the uid that owns the library. Without it the container
runs as root and amuxify refuses to modify files (`--allow-root` overrides, but
the resulting files would be root-owned).

The image is Alpine with ffmpeg, MKVToolNix and exiftool. Mount a state
directory at `/state` if you want quarantine and logs to persist.

### From source

```sh
git clone https://github.com/amuxify/amuxify && cd amuxify
make build            # ./bin/amuxify
make install PREFIX=$HOME/.local
```

Go 1.26 or newer.

### Windows

Planned for 0.4. The Go code builds on Windows today (`GOOS=windows go build
./cmd/amuxify`) with tools on `PATH`, but it is untested and unsupported.

## Check

```
$ amuxify doctor
amuxify 0.2.0
PASS     mkvmerge     /usr/bin/mkvmerge (mkvmerge v85.0 ('Nightingale') 64-bit)
PASS     mkvpropedit  /usr/bin/mkvpropedit (...)
PASS     mkvextract   /usr/bin/mkvextract (...)
PASS     ffmpeg       /usr/bin/ffmpeg (ffmpeg version 6.1.1 ...)
PASS     ffprobe      /usr/bin/ffprobe (...)
WARN     exiftool     not found (optional: deep metadata reports)
WARN     clamscan     not found (optional: safety.clamav = optional|required)
PASS     profile      homelab: Keep all languages, chapters and fonts; ...
PASS     user         uid 1000
PASS     state-dir    /home/me/.local/state/amuxify
PASS     tmpdir       /tmp

WARN: usable with warnings
```

Exit code 0 means amuxify is usable, even when optional tools are missing and
reported as `WARN`. Exit code 2 means a required tool is missing or too old, or
the active profile is invalid.

## State directory

`$XDG_STATE_HOME/amuxify` (default `~/.local/state/amuxify`), overridable with
`--state-dir` or `AMUXIFY_STATE_DIR`. Used for quarantine when `--quarantine`
is given without a path in 0.3; today it is only checked by `doctor`.
