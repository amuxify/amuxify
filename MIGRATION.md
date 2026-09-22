# Migrating from amuxify 0.1.x

0.2.0 replaces the Bash scripts with one Go binary. The scripts still live under
`legacy/` and can be run from there, but they receive no further changes.

## Command mapping

| 0.1.x | 0.2.0 |
|---|---|
| `amux-scan <dir>` | `amuxify --profile archive scan <dir>` |
| `amux-scan-all <dir>` | `amuxify --profile archive scan <dir>` (`text` data tags are allowed by every profile) |
| `amux-scan-all --deep` | `amuxify scan --verify full` |
| `amux-scan-all --clamav` | `amuxify scan --clamav`, or `safety.clamav = "required"` in a profile |
| `amux-remux <dir>` | `amuxify --profile archive remux <dir>` |
| `amux-remux --dry-run` | `amuxify --dry-run remux` |
| `amux-clean <dir>` | `amuxify --profile archive clean <dir>` |
| `AMUXIFY_UND_POLICY=drop\|english` | `languages.und = "drop"` or `"assume:eng"` in the profile |

The `amux-*` names are installed as shims that print a deprecation line on
stderr and run the mapped command. They will be removed in 0.4.

## Behaviour changes

**Default policy is `homelab`, not the 0.1.x rules.** The 0.1.x rules are the
`archive` profile. Pass `--profile archive` or set `AMUXIFY_PROFILE=archive`
in your environment to keep the old behaviour.

| Area | 0.1.x | 0.2.0 `homelab` | 0.2.0 `archive` |
|---|---|---|---|
| Languages | English only | all kept | English only |
| Untagged (`und`) tracks | interactive prompt | kept | dropped |
| Chapters | dropped | kept | dropped |
| Attachments | always BLOCK | fonts kept with text subs, cover art kept, everything else BLOCK | fonts and covers dropped, everything else BLOCK |
| Commentary audio | kept | kept | dropped |
| Track titles | dropped | kept | dropped |
| Links in tags, chapters, subtitles | not detected | WARN | FAIL |
| MP4 purchase atoms (`ownr`, `apID`, `purd`, ...) | not detected | WARN | FAIL |
| Executable permission bit | BLOCK with `--strict-permissions` | WARN | FAIL |
| Symlink anywhere in tree | whole run aborts | that file skipped with WARN | same |
| Hard-linked source with `--in-place` | not handled | skipped with WARN | same |
| MKV content in a `.mp4` name | passes | FAIL `EXT_MISMATCH` | same |
| `mkvmerge -J` exit 1 (warnings) | failed the file (fixed in 0.1.1) | logged as WARN | same |
| MKV writer | ffmpeg | mkvmerge | mkvmerge |
| Non-MKV input to remux | refused | converted to MKV | converted to MKV |
| macOS xattr | all removed | only `com.apple.*` | same |

**Why mkvmerge instead of ffmpeg for the MKV.** ffmpeg drops IETF language tags,
does not carry Dolby Vision and HDR10+ configuration into Matroska reliably, and
cannot preserve attachments. mkvmerge is the reference Matroska writer.

**Exit codes are now stable.** 0 PASS, 1 WARN, 2 usage, 3 FAIL, 4 BLOCK, 130
interrupted. 0.1.x used 0, 1 and 2 with different meanings per command.

## Output location

`amuxify remux` still writes to `<root>__remuxed/` beside the input by default.
Use `--output <dir>` to choose, or `--in-place` to replace sources after
verification (the source's extension becomes `.mkv`).

## Reproducing the 0.1.x workflow exactly

```sh
export AMUXIFY_PROFILE=archive
amuxify scan ~/purchases && amuxify remux ~/purchases && amuxify clean ~/purchases__remuxed
```
