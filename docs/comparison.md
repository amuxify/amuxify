# How amuxify compares

amuxify is a gate, not a pipeline. It does one deterministic pass and proves
the result. Tools that do more (transcoding, library management) or less (strip
tags without verifying) sit beside it, not against it.

| | amuxify | nudebomb / mkvstrip / radarr-striptracks | PlexCleaner | Tdarr / Unmanic / FileFlows | mediachecker, video-check, integv |
|---|---|---|---|---|---|
| Purpose | verify, sanitize, normalize, prove | drop unwanted language tracks | normalize MKVs for Plex | transcode and re-encode pipelines | integrity checks |
| Writes MKV with | mkvmerge | mkvmerge / mkvpropedit | mkvmerge, ffmpeg, HandBrake | ffmpeg, HandBrake | none |
| Per-stream hash equality source vs output | yes | no | no | no | n/a |
| Decode verification | head and tail, or full | no | optional full | optional | full |
| Executable and polyglot detection | yes | no | no | no | no |
| Attachment payload sniffing | yes | no | no | no | no |
| Links in tags, chapters, subtitles | yes | no | no | no | no |
| MP4 purchase atoms | yes | no | no | no | no |
| Never overwrite, temp+rename, symlink refusal | yes | varies | partial | partial | n/a |
| Runs as a hook with stable exit codes | yes | some | no | no | some |
| Transcoding | never | no | yes (optional) | yes | no |
| GUI | no | no | no | yes | no |

Use nudebomb-style tools when you only want to prune languages and trust the
source. Use Tdarr-style tools when you want to re-encode. Put amuxify before
either when the source is not trusted.
