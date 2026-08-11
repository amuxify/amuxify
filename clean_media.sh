#!/usr/bin/env bash

#
# clean-media.sh
#
# Removes:
# - macOS extended attributes, including quarantine and Finder metadata
# - EXIF, XMP, IPTC and other metadata writable by ExifTool
#
# Usage:
# cleanmedia /path/to/file-or-directory
# cleanmedia --dry-run /path/to/file-or-directory
#

set -uo pipefail
IFS=$'\n\t'

DRY_RUN=0
TARGET=""

usage() {
echo "Usage: $0 [--dry-run] /path/to/file-or-directory" >&2
exit 2
}

while [[ $# -gt 0 ]]; do
case "$1" in
--dry-run)
DRY_RUN=1
shift
;;
-h|--help)
usage
;;
-*)
echo "Unknown option: $1" >&2
usage
;;
*)
[[ -z "$TARGET" ]] || {
echo "Only one target may be specified." >&2
usage
}
TARGET="$1"
shift
;;
esac
done

[[ -n "$TARGET" && -e "$TARGET" ]] || usage

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

# Remove every macOS extended attribute:
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
# ExifTool leaves unsupported file formats unchanged.

exif_output="$(
exiftool \
-all= \
-overwrite_original \
-ignoreMinorErrors \
-- "$path" 2>&1
)"
exif_status=$?

if [[ "$exif_status" -ne 0 ]]; then
if grep -qiE \
'nothing to write|unsupported file type|does not support writing' \
<<<"$exif_output"; then
echo "  NOTE: ExifTool cannot rewrite metadata for this format"
UNSUPPORTED=$((UNSUPPORTED + 1))
else
echo "  ERROR: ExifTool failed" >&2
printf '  %s\n' "$exif_output" >&2
FAILED=$((FAILED + 1))
return
fi
fi

# ExifTool can create this backup suffix without -overwrite_original.

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
echo "Checked:             $CHECKED"
echo "Cleaned:             $CLEANED"
echo "ExifTool unsupported: $UNSUPPORTED"
echo "Failed:              $FAILED"

[[ "$FAILED" -eq 0 ]]