#!/usr/bin/env bash
#
# amux-clean
#
# Removes:
#   - macOS extended attributes, including quarantine and Finder metadata
#   - EXIF, XMP, IPTC and other metadata writable by ExifTool
#   - writable Matroska tags/title metadata via MKVToolNix
#
# Does not re-encode media.
#
set -Eo pipefail
IFS=$'\n\t'

PROGRAM_NAME="${AMUXIFY_COMMAND:-amux-clean}"
PROGRAM_VERSION="${AMUXIFY_VERSION:-dev}"

DRY_RUN=0
TARGET=""

usage() {
  cat >&2 <<EOF
Usage: $PROGRAM_NAME [--dry-run] /path/to/file-or-directory

Remove filesystem and writable container metadata from supported media files.

Options:
  --dry-run    Show what would be cleaned without modifying files.
  -h, --help   Show this help.

Behavior:
  - Recursively processes directories.
  - Clears macOS extended attributes.
  - Removes writable metadata with ExifTool.
  - Cleans writable Matroska tags/titles with MKVToolNix.
  - Ignores .DS_Store.
  - Does not re-encode media.
EOF
}

die_usage() {
  usage
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --version)
      printf '%s %s\n' "$PROGRAM_NAME" "$PROGRAM_VERSION"
      exit 0
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "Unknown option: $1" >&2
      die_usage
      ;;
    *)
      [[ -z "$TARGET" ]] || {
        echo "Only one target may be specified." >&2
        die_usage
      }
      TARGET="$1"
      shift
      ;;
  esac
done

[[ -n "$TARGET" ]] || die_usage

if [[ ! -e "$TARGET" ]]; then
  echo "Not found: $TARGET" >&2
  exit 2
fi

for cmd in find xattr exiftool mkvpropedit; do
  command -v "$cmd" >/dev/null 2>&1 || {
    echo "Missing dependency: $cmd" >&2
    exit 2
  }
done

CHECKED=0
CLEANED=0
FAILED=0
UNSUPPORTED=0

clean_file() {
  local path="$1"
  local exif_output
  local exif_status

  if [[ "$(basename "$path")" == ".DS_Store" ]]; then
    return
  fi

  CHECKED=$((CHECKED + 1))

  if [[ ! -f "$path" ]]; then
    return
  fi

  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "WOULD CLEAN: $path"
    return
  fi

  echo "CLEANING: $path"

  # Remove macOS extended attributes:
  # quarantine, Finder tags, downloaded-from metadata, resource forks, etc.
  if ! xattr -c -- "$path" 2>/dev/null; then
    echo "  ERROR: could not remove extended attributes" >&2
    FAILED=$((FAILED + 1))
    return
  fi

  # MKV/Matroska requires MKVToolNix because ExifTool cannot write MKV files.
  if [[ "$path" == *.[mM][kK][vV] ]]; then
    if ! mkvpropedit "$path" --tags all: --edit info --delete title; then
      echo "  ERROR: mkvpropedit failed" >&2
      FAILED=$((FAILED + 1))
      return
    fi

    CLEANED=$((CLEANED + 1))
    return
  fi

  # Remove all metadata ExifTool can safely rewrite.
  # Unsupported formats are left unchanged.
  exif_output="$(exiftool -all= -overwrite_original -ignoreMinorErrors -- "$path" 2>&1)"
  exif_status=$?

  if [[ "$exif_status" -ne 0 ]]; then
    if grep -qiE 'nothing to write|unsupported file type|does not support writing' <<<"$exif_output"; then
      echo "  NOTE: ExifTool cannot rewrite metadata for this format"
      UNSUPPORTED=$((UNSUPPORTED + 1))
    else
      echo "  ERROR: ExifTool failed" >&2
      printf '  %s\n' "$exif_output" >&2
      FAILED=$((FAILED + 1))
      return
    fi
  fi

  rm -f -- "${path}_original"

  CLEANED=$((CLEANED + 1))
}

if [[ -f "$TARGET" ]]; then
  clean_file "$TARGET"
else
  while IFS= read -r -d '' path; do
    clean_file "$path"
  done < <(find "$TARGET" -type f ! -name '.DS_Store' -print0)
fi

echo
echo "Checked:              $CHECKED"
echo "Cleaned:              $CLEANED"
echo "ExifTool unsupported: $UNSUPPORTED"
echo "Failed:               $FAILED"

[[ "$FAILED" -eq 0 ]]