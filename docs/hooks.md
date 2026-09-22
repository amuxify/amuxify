# Hooks and unattended runs

amuxify never prompts and always exits with a stable code, so it can be called
from any post-processing script. Native adapters (`amuxify hook sabnzbd`,
`nzbget`, `sonarr`, `radarr`) arrive in 0.3; until then use the exit code.

Exit codes: 0 PASS, 1 WARN, 2 usage error, 3 FAIL, 4 BLOCK, 130 interrupted.
`--json` gives the full report on stdout; `--quiet` suppresses the text report.

## SABnzbd post-processing script

```sh
#!/bin/sh
# SABnzbd passes the completed directory in SAB_COMPLETE_DIR.
# Exit 0 = success, anything else marks the job failed.
amuxify --profile homelab --quiet remux --in-place "$SAB_COMPLETE_DIR"
rc=$?
case $rc in
  0|1) exit 0 ;;   # PASS or WARN: let Sonarr/Radarr import
  *)   echo "amuxify verdict $rc, see log"; exit 1 ;;
esac
```

## NZBGet post-processing script

```sh
#!/bin/sh
# NZBGet: 93 = success, 94 = failure, 95 = none.
amuxify --quiet remux --in-place "$NZBPP_DIRECTORY"
case $? in 0|1) exit 93 ;; *) exit 94 ;; esac
```

## Sonarr and Radarr custom script (On Import)

```sh
#!/bin/sh
# sonarr_episodefile_path / radarr_moviefile_path point at the imported file.
f="${sonarr_episodefile_path:-$radarr_moviefile_path}"
lang="${sonarr_series_originallanguage:-$radarr_movie_originallanguage}"
amuxify --quiet --json remux --in-place --original-language "$lang" "$f" > /var/log/amuxify/last.json
```

Sonarr and Radarr ignore the exit code of an On Import script, so use scan in a
Before Import step if you want to reject files, or run amuxify in the download
client instead so the verdict gates the import.

## cron or systemd timer over an incoming folder

```sh
amuxify scan --quarantine /srv/media/quarantine /srv/media/incoming || true
amuxify remux --output /srv/media/ready /srv/media/incoming
```

`--quarantine` moves BLOCK files into a mirrored tree under the given directory
so they stop being picked up, without deleting anything.

## Docker with the linuxserver.io arr images

Mount the same paths at the same locations and run amuxify as a sidecar with
the same PUID and PGID:

```sh
docker run --rm -u 1000:1000 -v /srv/media:/srv/media \
  ghcr.io/nxame/amuxify remux --in-place /srv/media/incoming
```

Hard-linked files (most torrent setups) are skipped in place by default so
seeding continues. Set `safety.hardlinks = "copy"` to write a cleaned copy
elsewhere, or `"break"` to replace only the library's name.
