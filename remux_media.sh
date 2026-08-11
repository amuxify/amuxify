#!/usr/bin/env bash
#
# remux_media.sh — Conservative MKV archive remuxer/sanitizer.
#
# Goals:
#   - MKV input only (single file OR directory tree).
#   - Preserve the primary/default video stream with stream copy (NO video re-encode).
#   - Preserve English audio streams only with stream copy (NO audio re-encode).
#   - Preserve all English SRT subtitles.
#   - Convert English ASS/SSA subtitles to plain SRT.
#   - For untagged ASS/SSA subtitles, show a dialogue sample and ask whether
#     they should be treated as English. The choice can be remembered for
#     the rest of this run only.
#   - Drop non-English audio/subtitles, attachments, data streams, chapters,
#     global/track tags, track titles, old Segment UID and old container metadata.
#   - Verify the output before it is finalized.
#   - Never overwrite an existing destination file.
#   - Refuse symlinks.
#
# IMPORTANT — PROVENANCE LIMITATION
#
# This tool removes/rebuilds CONTAINER-level provenance.
#
# Retained video/audio streams are intentionally stream-copied and are therefore
# not re-encoded. Encoder-identifying information embedded INSIDE the encoded
# bitstream may survive this operation. For example, HEVC produced by x265 can
# contain x265 version/build/options information inside codec headers/SEI data.
#
# Therefore this script MUST NOT be described as guaranteeing removal of all
# bitstream-level provenance while stream-copy mode is used.
#
# Matroska also requires MuxingApp/WritingApp elements. This script clears their
# values after remuxing so tool/version fingerprints are minimized, but the
# mandatory elements themselves may still exist as empty strings.
#
# Usage:
#   remux_media.sh /path/to/file.mkv
#   remux_media.sh /path/to/directory
#   remux_media.sh --dry-run /path/to/directory
#
# Output:
#   Directory input:
#     /path/Season01 -> /path/Season01__remuxed/...
#
#   Single-file input:
#     /path/movies/Episode.mkv -> /path/movies__remuxed/Episode.mkv
#
set -Euo pipefail
IFS=$'\n\t'

DRY_RUN=0
TARGET=""

SESSION_UND_SUB_POLICY=""   # english | drop | empty
SESSION_UND_AUDIO_POLICY="" # english | drop | empty

TOTAL=0
PROCESSED=0
SUCCEEDED=0
FAILED=0
SKIPPED=0

VIDEO_KEPT=0
VIDEO_DROPPED=0
AUDIO_KEPT=0
AUDIO_DROPPED=0
SUB_SRT_KEPT=0
SUB_ASS_CONVERTED=0
SUB_DROPPED=0
NO_SUB_HINTS=0

CURRENT_TMP_OUTPUT=""
CURRENT_WORKDIR=""

FFPROBE_TIMEOUT_SECS=60
FFMPEG_TIMEOUT_SECS=3600

TIMEOUT_BIN=""
if command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
elif command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
fi

usage() {
  cat >&2 <<EOF
Usage: $0 [--dry-run] /path/to/file-or-directory

  --dry-run   Inspect, plan and prompt, but do not create output files.

Rules:
  - MKV only
  - primary/default video only
  - English audio only
  - English SRT kept
  - English ASS/SSA converted to SRT
  - untagged ASS/SSA prompts for a decision
  - no overwrite
  - symlinks are refused
EOF
  exit 2
}

cleanup_current() {
  if [[ -n "$CURRENT_TMP_OUTPUT" && -e "$CURRENT_TMP_OUTPUT" ]]; then
    rm -f -- "$CURRENT_TMP_OUTPUT" 2>/dev/null || true
  fi
  CURRENT_TMP_OUTPUT=""

  if [[ -n "$CURRENT_WORKDIR" && -d "$CURRENT_WORKDIR" ]]; then
    rm -rf -- "$CURRENT_WORKDIR" 2>/dev/null || true
  fi
  CURRENT_WORKDIR=""
}

on_interrupt() {
  printf '\nInterrupted. Cleaning temporary files.\n' >&2
  cleanup_current
  exit 130
}

trap on_interrupt INT TERM
trap cleanup_current EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      ;;
    --)
      shift
      break
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage
      ;;
    *)
      if [[ -n "$TARGET" ]]; then
        echo "Only one input target is supported per run." >&2
        usage
      fi
      TARGET="$1"
      shift
      ;;
  esac
done

[[ -n "$TARGET" ]] || usage

for cmd in find ffprobe ffmpeg mkvmerge mkvpropedit perl mktemp basename dirname mkdir mv rm grep sed head sort tr wc; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing dependency: $cmd" >&2
    case "$cmd" in
      ffprobe|ffmpeg)
        echo "On macOS: brew install ffmpeg" >&2
        ;;
      mkvmerge|mkvpropedit)
        echo "On macOS: brew install mkvtoolnix" >&2
        ;;
    esac
    exit 2
  fi
done

if [[ ! -e "$TARGET" && ! -L "$TARGET" ]]; then
  echo "Not found: $TARGET" >&2
  exit 2
fi

if [[ -L "$TARGET" ]]; then
  echo "REFUSED: input target is a symlink: $TARGET" >&2
  exit 1
fi

safe_ffprobe() {
  local secs="$1"
  shift

  if [[ -n "$TIMEOUT_BIN" ]]; then
    "$TIMEOUT_BIN" --foreground "$secs" ffprobe -protocol_whitelist file "$@"
  else
    ffprobe -protocol_whitelist file "$@"
  fi
}

safe_ffmpeg() {
  local secs="$1"
  shift

  if [[ -n "$TIMEOUT_BIN" ]]; then
    "$TIMEOUT_BIN" --foreground "$secs" ffmpeg -protocol_whitelist file,pipe "$@"
  else
    ffmpeg -protocol_whitelist file,pipe "$@"
  fi
}

abs_dir() {
  (cd "$1" 2>/dev/null && pwd -P)
}

abs_path_any() {
  local p="$1"
  local d
  local b
  local rd

  if [[ -d "$p" ]]; then
    abs_dir "$p"
    return
  fi

  d="$(dirname "$p")"
  b="$(basename "$p")"
  rd="$(abs_dir "$d")" || return 1
  printf '%s/%s\n' "$rd" "$b"
}

lowercase() {
  LC_ALL=C printf '%s' "$1" | LC_ALL=C tr '[:upper:]' '[:lower:]'
}

is_english_lang() {
  local lang
  lang="$(lowercase "${1:-}")"

  case "$lang" in
    eng|en|en-*|eng-*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

is_und_lang() {
  local lang
  lang="$(lowercase "${1:-}")"

  case "$lang" in
    ""|und|unknown|unk)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

stream_field() {
  local file="$1"
  local idx="$2"
  local entry="$3"

  safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -select_streams "$idx" -show_entries "stream=$entry" -of default=noprint_wrappers=1:nokey=1 -- "$file" 2>/dev/null | head -n1
}

stream_tag() {
  local file="$1"
  local idx="$2"
  local tag="$3"

  safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -select_streams "$idx" -show_entries "stream_tags=$tag" -of default=noprint_wrappers=1:nokey=1 -- "$file" 2>/dev/null | head -n1
}

stream_disposition() {
  local file="$1"
  local idx="$2"
  local disp="$3"

  safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -select_streams "$idx" -show_entries "stream_disposition=$disp" -of default=noprint_wrappers=1:nokey=1 -- "$file" 2>/dev/null | head -n1
}

list_stream_indexes() {
  local file="$1"
  local type="$2"

  safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -select_streams "$type" -show_entries stream=index -of default=noprint_wrappers=1:nokey=1 -- "$file" 2>/dev/null
}

format_disposition() {
  local def="${1:-0}"
  local forced="${2:-0}"

  if [[ "$def" == "1" && "$forced" == "1" ]]; then
    printf 'default+forced'
  elif [[ "$def" == "1" ]]; then
    printf 'default'
  elif [[ "$forced" == "1" ]]; then
    printf 'forced'
  else
    printf '0'
  fi
}

show_sub_sample() {
  local srt="$1"

  perl -ne '
    next if /^\s*$/;
    next if /^\d+\s*$/;
    next if /^\d\d:\d\d:\d\d[,.]\d+\s+-->\s+\d\d:\d\d:\d\d[,.]\d+/;
    s/<font\b[^>]*>//gi;
    s#</font>##gi;
    s/<\/?(?:i|b|u)>//gi;
    print;
  ' "$srt" | head -n 6
}

clean_srt_font_tags() {
  local srt="$1"
  perl -0pi -e 's/<font\b[^>]*>//gi; s#</font>##gi' "$srt"
}

prompt_und_subtitle() {
  local file="$1"
  local codec="$2"
  local sample_file="$3"
  local answer

  if [[ "$SESSION_UND_SUB_POLICY" == "english" ]]; then
    return 0
  fi

  if [[ "$SESSION_UND_SUB_POLICY" == "drop" ]]; then
    return 1
  fi

  echo
  echo "Untagged subtitle found"
  echo "  File:     $(basename "$file")"
  echo "  Codec:    $codec"
  echo "  Language: und"
  echo
  echo "Sample:"
  show_sub_sample "$sample_file" | sed 's/^/  /'
  echo

  while true; do
    printf '[y] English this one  [a] English all untagged ASS/SSA this run  [n] drop this one  [d] drop all untagged ASS/SSA this run  [q] quit: '
    IFS= read -r answer

    case "$(lowercase "$answer")" in
      y)
        return 0
        ;;
      a)
        SESSION_UND_SUB_POLICY="english"
        return 0
        ;;
      n)
        return 1
        ;;
      d)
        SESSION_UND_SUB_POLICY="drop"
        return 1
        ;;
      q)
        echo "Quit requested."
        exit 130
        ;;
      *)
        echo "Please enter y, a, n, d or q."
        ;;
    esac
  done
}

prompt_und_audio() {
  local file="$1"
  local idx="$2"
  local codec="$3"
  local channels="$4"
  local rate="$5"
  local answer

  if [[ "$SESSION_UND_AUDIO_POLICY" == "english" ]]; then
    return 0
  fi

  if [[ "$SESSION_UND_AUDIO_POLICY" == "drop" ]]; then
    return 1
  fi

  echo
  echo "Untagged audio found"
  echo "  File:        $(basename "$file")"
  echo "  Stream:      #$idx"
  echo "  Codec:       $codec"
  echo "  Channels:    ${channels:-unknown}"
  echo "  Sample rate: ${rate:-unknown}"
  echo "  Language:    und"
  echo

  while true; do
    printf '[y] English this one  [a] English all untagged audio this run  [n] drop this one  [d] drop all untagged audio this run  [q] quit: '
    IFS= read -r answer

    case "$(lowercase "$answer")" in
      y)
        return 0
        ;;
      a)
        SESSION_UND_AUDIO_POLICY="english"
        return 0
        ;;
      n)
        return 1
        ;;
      d)
        SESSION_UND_AUDIO_POLICY="drop"
        return 1
        ;;
      q)
        echo "Quit requested."
        exit 130
        ;;
      *)
        echo "Please enter y, a, n, d or q."
        ;;
    esac
  done
}

extract_sub_to_srt() {
  local input="$1"
  local idx="$2"
  local codec="$3"
  local out="$4"

  if [[ "$codec" == "subrip" || "$codec" == "srt" ]]; then
    safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -i "$input" -map "0:$idx" -c:s copy -f srt "$out"
  else
    safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -i "$input" -map "0:$idx" -c:s srt -f srt "$out"
  fi

  clean_srt_font_tags "$out"
}

stream_copy_hash() {
  local file="$1"
  local idx="$2"

  safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -i "$file" -map "0:$idx" -c copy -f streamhash -hash sha256 - 2>/dev/null | sed -n 's/.*SHA256=//p' | head -n1
}

quick_decode_verify() {
  local file="$1"

  safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -xerror -i "$file" -t 15 -map 0:v:0 -map 0:a? -f null - >/dev/null 2>&1 || return 1

  safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -xerror -sseof -15 -i "$file" -map 0:v:0 -map 0:a? -f null - >/dev/null 2>&1 || return 1

  return 0
}

print_plan_header() {
  local file="$1"
  echo
  echo "------------------------------------------------------------"
  echo "[$PROCESSED/$TOTAL] $(basename "$file")"
  echo "------------------------------------------------------------"
}

process_file() {
  local input="$1"
  local output="$2"

  local -a video_indexes=()
  local -a audio_indexes=()
  local -a subtitle_indexes=()
  local -a keep_audio_indexes=()
  local -a keep_audio_disps=()
  local -a keep_sub_files=()
  local -a keep_sub_disps=()
  local -a keep_sub_source_indexes=()
  local -a keep_sub_source_codecs=()
  local -a ffargs=()
  local -a prop_args=()
  local -a out_video_indexes=()
  local -a out_audio_indexes=()
  local -a out_sub_indexes=()

  local idx
  local codec
  local lang
  local def
  local forced
  local disp
  local channels
  local rate
  local title
  local primary_video=""
  local video_default=""
  local source_audio_count=0
  local source_sub_count=0
  local keep_sub_count=0
  local duration
  local rel_tmp
  local subtmp
  local subnum=0
  local input_no
  local i
  local src_hash
  local dst_hash
  local verify_json
  local mux_app
  local write_app
  local remux_failed=0

  PROCESSED=$((PROCESSED + 1))
  print_plan_header "$input"

  if [[ -e "$output" || -L "$output" ]]; then
    echo "SKIPPED: destination already exists:"
    echo "  $output"
    SKIPPED=$((SKIPPED + 1))
    return 0
  fi

  while IFS= read -r idx; do
    [[ -n "$idx" ]] && video_indexes+=("$idx")
  done < <(list_stream_indexes "$input" v)

  while IFS= read -r idx; do
    [[ -n "$idx" ]] && audio_indexes+=("$idx")
  done < <(list_stream_indexes "$input" a)

  while IFS= read -r idx; do
    [[ -n "$idx" ]] && subtitle_indexes+=("$idx")
  done < <(list_stream_indexes "$input" s)

  if [[ ${#video_indexes[@]} -eq 0 ]]; then
    echo "FAILED: no video stream found."
    FAILED=$((FAILED + 1))
    return 1
  fi

  # Primary video = first default video, otherwise first video.
  primary_video="${video_indexes[0]}"
  for idx in "${video_indexes[@]}"; do
    def="$(stream_disposition "$input" "$idx" default || true)"
    if [[ "$def" == "1" ]]; then
      primary_video="$idx"
      video_default="1"
      break
    fi
  done

  codec="$(stream_field "$input" "$primary_video" codec_name || true)"
  echo "VIDEO"
  echo "  KEEP  #$primary_video $codec (primary/default, stream copy)"

  if [[ ${#video_indexes[@]} -gt 1 ]]; then
    for idx in "${video_indexes[@]}"; do
      if [[ "$idx" != "$primary_video" ]]; then
        codec="$(stream_field "$input" "$idx" codec_name || true)"
        echo "  DROP  #$idx $codec (secondary video)"
        VIDEO_DROPPED=$((VIDEO_DROPPED + 1))
      fi
    done
  fi

  VIDEO_KEPT=$((VIDEO_KEPT + 1))

  echo
  echo "AUDIO"

  source_audio_count=${#audio_indexes[@]}

  if [[ "$source_audio_count" -eq 0 ]]; then
    echo "  HINT: source contains no audio streams."
  else
    for idx in "${audio_indexes[@]}"; do
      codec="$(stream_field "$input" "$idx" codec_name || true)"
      lang="$(stream_tag "$input" "$idx" language || true)"
      channels="$(stream_field "$input" "$idx" channels || true)"
      rate="$(stream_field "$input" "$idx" sample_rate || true)"
      def="$(stream_disposition "$input" "$idx" default || true)"
      forced="$(stream_disposition "$input" "$idx" forced || true)"
      disp="$(format_disposition "$def" "$forced")"

      if is_english_lang "$lang"; then
        keep_audio_indexes+=("$idx")
        keep_audio_disps+=("$disp")
        echo "  KEEP  #$idx $codec ${channels:-?}ch language=${lang:-eng} (stream copy)"
        AUDIO_KEPT=$((AUDIO_KEPT + 1))
      elif is_und_lang "$lang"; then
        if prompt_und_audio "$input" "$idx" "$codec" "$channels" "$rate"; then
          keep_audio_indexes+=("$idx")
          keep_audio_disps+=("$disp")
          echo "  KEEP  #$idx $codec untagged -> treat as English (stream copy)"
          AUDIO_KEPT=$((AUDIO_KEPT + 1))
        else
          echo "  DROP  #$idx $codec language=und"
          AUDIO_DROPPED=$((AUDIO_DROPPED + 1))
        fi
      else
        echo "  DROP  #$idx $codec language=$lang"
        AUDIO_DROPPED=$((AUDIO_DROPPED + 1))
      fi
    done

    if [[ ${#keep_audio_indexes[@]} -eq 0 ]]; then
      echo "FAILED: source has audio, but no English audio was retained."
      echo "        Refusing to create a silent archive copy."
      FAILED=$((FAILED + 1))
      return 1
    fi
  fi

  CURRENT_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/remuxmedia.XXXXXX")"

  echo
  echo "SUBTITLES"

  source_sub_count=${#subtitle_indexes[@]}

  if [[ "$source_sub_count" -eq 0 ]]; then
    echo "  HINT: no subtitles found; proceeding without subtitles."
    NO_SUB_HINTS=$((NO_SUB_HINTS + 1))
  else
    for idx in "${subtitle_indexes[@]}"; do
      codec="$(stream_field "$input" "$idx" codec_name || true)"
      lang="$(stream_tag "$input" "$idx" language || true)"
      def="$(stream_disposition "$input" "$idx" default || true)"
      forced="$(stream_disposition "$input" "$idx" forced || true)"
      disp="$(format_disposition "$def" "$forced")"

      subnum=$((subnum + 1))
      subtmp="$CURRENT_WORKDIR/sub_${subnum}.srt"

      case "$codec" in
        subrip|srt)
          if is_english_lang "$lang"; then
            if extract_sub_to_srt "$input" "$idx" "$codec" "$subtmp"; then
              keep_sub_files+=("$subtmp")
              keep_sub_disps+=("$disp")
              keep_sub_source_indexes+=("$idx")
              keep_sub_source_codecs+=("$codec")
              echo "  KEEP  #$idx SRT language=${lang:-eng}"
              SUB_SRT_KEPT=$((SUB_SRT_KEPT + 1))
            else
              echo "FAILED: could not extract SRT subtitle #$idx."
              FAILED=$((FAILED + 1))
              cleanup_current
              return 1
            fi
          else
            echo "  DROP  #$idx SRT language=${lang:-und}"
            SUB_DROPPED=$((SUB_DROPPED + 1))
          fi
          ;;

        ass|ssa)
          if ! extract_sub_to_srt "$input" "$idx" "$codec" "$subtmp"; then
            echo "FAILED: could not convert subtitle #$idx ($codec) to SRT."
            FAILED=$((FAILED + 1))
            cleanup_current
            return 1
          fi

          if is_english_lang "$lang"; then
            keep_sub_files+=("$subtmp")
            keep_sub_disps+=("$disp")
            keep_sub_source_indexes+=("$idx")
            keep_sub_source_codecs+=("$codec")
            echo "  CONVERT #$idx ${codec} language=$lang -> SRT"
            SUB_ASS_CONVERTED=$((SUB_ASS_CONVERTED + 1))
          elif is_und_lang "$lang"; then
            if prompt_und_subtitle "$input" "$codec" "$subtmp"; then
              keep_sub_files+=("$subtmp")
              keep_sub_disps+=("$disp")
              keep_sub_source_indexes+=("$idx")
              keep_sub_source_codecs+=("$codec")
              echo "  CONVERT #$idx ${codec} language=und -> English SRT"
              SUB_ASS_CONVERTED=$((SUB_ASS_CONVERTED + 1))
            else
              echo "  DROP  #$idx ${codec} language=und"
              rm -f -- "$subtmp"
              SUB_DROPPED=$((SUB_DROPPED + 1))
            fi
          else
            echo "  DROP  #$idx ${codec} language=$lang"
            rm -f -- "$subtmp"
            SUB_DROPPED=$((SUB_DROPPED + 1))
          fi
          ;;

        *)
          if is_english_lang "$lang"; then
            echo "  DROP  #$idx codec=$codec language=$lang (not SRT/ASS/SSA; no OCR conversion)"
          else
            echo "  DROP  #$idx codec=$codec language=${lang:-und}"
          fi
          SUB_DROPPED=$((SUB_DROPPED + 1))
          ;;
      esac
    done

    keep_sub_count=${#keep_sub_files[@]}

    if [[ "$keep_sub_count" -eq 0 ]]; then
      echo "  HINT: no English subtitle retained; proceeding without subtitles."
      NO_SUB_HINTS=$((NO_SUB_HINTS + 1))
    fi
  fi

  echo
  echo "SANITIZE"
  echo "  DROP attachments"
  echo "  DROP data streams"
  echo "  DROP chapters"
  echo "  DROP global/track tags"
  echo "  DROP track titles"
  echo "  DROP old Segment UID"
  echo "  CLEAR muxing/writing application values"
  echo "  RETAG retained English audio/subtitles as eng"

  echo
  echo "OUTPUT"
  echo "  $output"

  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo
    echo "DRY RUN: no output created."
    cleanup_current
    return 0
  fi

  mkdir -p -- "$(dirname "$output")"

  CURRENT_TMP_OUTPUT="$(dirname "$output")/.$(basename "$output").remuxing.tmp.mkv"

  if [[ -e "$CURRENT_TMP_OUTPUT" || -L "$CURRENT_TMP_OUTPUT" ]]; then
    echo "FAILED: temporary output already exists:"
    echo "  $CURRENT_TMP_OUTPUT"
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  duration="$(safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 -- "$input" 2>/dev/null || true)"

  ffargs=(-nostdin -hide_banner -v error -nostats -progress pipe:1 -stats_period 1 -i "$input")

  for subtmp in "${keep_sub_files[@]}"; do
    ffargs+=(-i "$subtmp")
  done

  ffargs+=(-map "0:$primary_video")

  for idx in "${keep_audio_indexes[@]}"; do
    ffargs+=(-map "0:$idx")
  done

  input_no=1
  for subtmp in "${keep_sub_files[@]}"; do
    ffargs+=(-map "$input_no:0")
    input_no=$((input_no + 1))
  done

  ffargs+=(-c:v copy -c:a copy -c:s copy -map_metadata -1 -map_chapters -1 -metadata title=)

  ffargs+=(-metadata:s:v:0 language= -metadata:s:v:0 title= -disposition:v:0 default)

  i=0
  while [[ "$i" -lt ${#keep_audio_indexes[@]} ]]; do
    ffargs+=("-metadata:s:a:$i" language=eng "-metadata:s:a:$i" title= "-disposition:a:$i" "${keep_audio_disps[$i]}")
    i=$((i + 1))
  done

  i=0
  while [[ "$i" -lt ${#keep_sub_files[@]} ]]; do
    ffargs+=("-metadata:s:s:$i" language=eng "-metadata:s:s:$i" title= "-disposition:s:$i" "${keep_sub_disps[$i]}")
    i=$((i + 1))
  done

  ffargs+=(-f matroska "$CURRENT_TMP_OUTPUT")

  echo
  echo "REMUXING"

  # Parse FFmpeg progress in a subshell. The entire pipeline fails if FFmpeg
  # fails because pipefail is enabled.
  if ! safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" "${ffargs[@]}" |
    (
      current_us=""
      speed_num=""

      while IFS='=' read -r key value; do
        case "$key" in
          out_time_us)
            current_us="$value"
            ;;
          speed)
            speed_num="${value%x}"
            speed_num="$(printf '%s' "$speed_num" | tr -d ' ')"
            ;;
          progress)
            if [[ -n "$current_us" && "$current_us" =~ ^[0-9]+$ && "$duration" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
              awk -v current_us="$current_us" -v total="$duration" -v speed="${speed_num:-0}" '
                BEGIN {
                  current = current_us / 1000000
                  pct = (current / total) * 100
                  if (pct < 0) pct = 0
                  if (pct > 100) pct = 100

                  if (speed + 0 > 0) {
                    remain = (total - current) / speed
                    if (remain < 0) remain = 0
                    printf "\r  Remuxing: %5.1f%% | speed: %sx | ETA: %02d:%02d   ",
                      pct, speed, int(remain / 60), int(remain) % 60
                  } else {
                    printf "\r  Remuxing: %5.1f%%   ", pct
                  }
                }
              '
            fi

            if [[ "$value" == "end" ]]; then
              printf '\n'
            fi
            ;;
        esac
      done
    )
  then
    echo
    echo "FAILED: FFmpeg remux failed."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  echo "Cleaning Matroska metadata..."

  prop_args=("$CURRENT_TMP_OUTPUT" --tags all: --chapters "" --edit info --delete title --delete date --delete segment-uid --set muxing-application= --set writing-application=)

  prop_args+=(--edit track:v1 --delete name --set language=und)

  i=1
  while [[ "$i" -le ${#keep_audio_indexes[@]} ]]; do
    prop_args+=(--edit "track:a$i" --delete name --set language=eng)
    i=$((i + 1))
  done

  i=1
  while [[ "$i" -le ${#keep_sub_files[@]} ]]; do
    prop_args+=(--edit "track:s$i" --delete name --set language=eng)
    i=$((i + 1))
  done

  if ! mkvpropedit "${prop_args[@]}" >/dev/null; then
    echo "FAILED: mkvpropedit metadata cleanup failed."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  echo "Verifying container..."

  if ! safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_format -show_streams -- "$CURRENT_TMP_OUTPUT" >/dev/null 2>&1; then
    echo "FAILED: ffprobe cannot parse remuxed output."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if ! verify_json="$(mkvmerge -J "$CURRENT_TMP_OUTPUT" 2>/dev/null)"; then
    echo "FAILED: mkvmerge cannot identify remuxed output."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if ! printf '%s\n' "$verify_json" | grep -Eq '"attachments"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]'; then
    echo "FAILED: verification found attachments."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if ! printf '%s\n' "$verify_json" | grep -Eq '"chapters"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]'; then
    echo "FAILED: verification found chapters."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if ! printf '%s\n' "$verify_json" | grep -Eq '"global_tags"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]'; then
    echo "FAILED: verification found global tags."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if ! printf '%s\n' "$verify_json" | grep -Eq '"track_tags"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]'; then
    echo "FAILED: verification found track tags."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if printf '%s\n' "$verify_json" | grep -q '"track_name"'; then
    echo "FAILED: verification found one or more track names."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if printf '%s\n' "$verify_json" | grep -q '"segment_uid"'; then
    echo "FAILED: verification found a Segment UID."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  mux_app="$(printf '%s\n' "$verify_json" | perl -ne 'if (/"muxing_application"\s*:\s*"([^"]*)"/) { print $1; exit }')"
  write_app="$(printf '%s\n' "$verify_json" | perl -ne 'if (/"writing_application"\s*:\s*"([^"]*)"/) { print $1; exit }')"

  if [[ -n "$mux_app" || -n "$write_app" ]]; then
    echo "FAILED: muxing/writing application fingerprint is not empty."
    echo "  muxing_application:  ${mux_app:-<empty>}"
    echo "  writing_application: ${write_app:-<empty>}"
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  while IFS= read -r idx; do
    [[ -n "$idx" ]] && out_video_indexes+=("$idx")
  done < <(list_stream_indexes "$CURRENT_TMP_OUTPUT" v)

  while IFS= read -r idx; do
    [[ -n "$idx" ]] && out_audio_indexes+=("$idx")
  done < <(list_stream_indexes "$CURRENT_TMP_OUTPUT" a)

  while IFS= read -r idx; do
    [[ -n "$idx" ]] && out_sub_indexes+=("$idx")
  done < <(list_stream_indexes "$CURRENT_TMP_OUTPUT" s)

  if [[ ${#out_video_indexes[@]} -ne 1 ]]; then
    echo "FAILED: expected exactly 1 video stream; found ${#out_video_indexes[@]}."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if [[ ${#out_audio_indexes[@]} -ne ${#keep_audio_indexes[@]} ]]; then
    echo "FAILED: audio stream count mismatch."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if [[ ${#out_sub_indexes[@]} -ne ${#keep_sub_files[@]} ]]; then
    echo "FAILED: subtitle stream count mismatch."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  # Ensure output contains no attachment/data stream types.
  if safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_entries stream=codec_type -of default=noprint_wrappers=1:nokey=1 -- "$CURRENT_TMP_OUTPUT" 2>/dev/null | grep -Eq '^(attachment|data)$'; then
    echo "FAILED: output contains attachment/data stream."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  echo "Verifying video/audio stream-copy hashes..."

  src_hash="$(stream_copy_hash "$input" "$primary_video" || true)"
  dst_hash="$(stream_copy_hash "$CURRENT_TMP_OUTPUT" "${out_video_indexes[0]}" || true)"

  if [[ -z "$src_hash" || -z "$dst_hash" || "$src_hash" != "$dst_hash" ]]; then
    echo "FAILED: video stream hash mismatch; refusing output."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  i=0
  while [[ "$i" -lt ${#keep_audio_indexes[@]} ]]; do
    src_hash="$(stream_copy_hash "$input" "${keep_audio_indexes[$i]}" || true)"
    dst_hash="$(stream_copy_hash "$CURRENT_TMP_OUTPUT" "${out_audio_indexes[$i]}" || true)"

    if [[ -z "$src_hash" || -z "$dst_hash" || "$src_hash" != "$dst_hash" ]]; then
      echo "FAILED: audio stream hash mismatch for retained audio #${keep_audio_indexes[$i]}."
      FAILED=$((FAILED + 1))
      cleanup_current
      return 1
    fi

    i=$((i + 1))
  done

  echo "Verifying beginning/end decode..."

  if ! quick_decode_verify "$CURRENT_TMP_OUTPUT"; then
    echo "FAILED: output failed beginning/end decode verification."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if command -v xattr >/dev/null 2>&1; then
    xattr -c -- "$CURRENT_TMP_OUTPUT" >/dev/null 2>&1 || true
  fi

  echo "Finalizing..."

  if [[ -e "$output" || -L "$output" ]]; then
    echo "FAILED: destination appeared during processing; refusing overwrite."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  if ! mv -- "$CURRENT_TMP_OUTPUT" "$output"; then
    echo "FAILED: could not finalize output."
    FAILED=$((FAILED + 1))
    cleanup_current
    return 1
  fi

  CURRENT_TMP_OUTPUT=""
  cleanup_current

  echo "PASS: $output"
  SUCCEEDED=$((SUCCEEDED + 1))
  return 0
}

TARGET_ABS="$(abs_path_any "$TARGET")" || {
  echo "Cannot resolve input path: $TARGET" >&2
  exit 2
}

INPUT_IS_DIR=0
if [[ -d "$TARGET_ABS" ]]; then
  INPUT_IS_DIR=1
fi

INPUT_ROOT=""
OUTPUT_ROOT=""
declare -a FILES=()
declare -a OUTPUTS=()

if [[ "$INPUT_IS_DIR" -eq 1 ]]; then
  INPUT_ROOT="$TARGET_ABS"
  OUTPUT_ROOT="$(dirname "$INPUT_ROOT")/$(basename "$INPUT_ROOT")__remuxed"

  # Refuse any symlink anywhere in the source tree.
  first_link="$(find "$INPUT_ROOT" -type l -print -quit 2>/dev/null || true)"
  if [[ -n "$first_link" ]]; then
    echo "REFUSED: symlink found inside input tree:" >&2
    echo "  $first_link" >&2
    exit 1
  fi

  while IFS= read -r -d '' f; do
    FILES+=("$f")
  done < <(find "$INPUT_ROOT" -type f ! -name '.DS_Store' \( -iname '*.mkv' \) -print0)

  TOTAL=${#FILES[@]}

  if [[ "$TOTAL" -eq 0 ]]; then
    echo "No MKV files found under: $INPUT_ROOT"
    exit 0
  fi

  for f in "${FILES[@]}"; do
    rel="${f#"$INPUT_ROOT"/}"
    OUTPUTS+=("$OUTPUT_ROOT/$rel")
  done
else
  ext="$(lowercase "${TARGET_ABS##*.}")"
  if [[ "$ext" != "mkv" ]]; then
    echo "REFUSED: v1 supports MKV input only: $TARGET_ABS" >&2
    exit 1
  fi

  INPUT_ROOT="$(dirname "$TARGET_ABS")"
  OUTPUT_ROOT="$(dirname "$INPUT_ROOT")/$(basename "$INPUT_ROOT")__remuxed"
  FILES+=("$TARGET_ABS")
  OUTPUTS+=("$OUTPUT_ROOT/$(basename "$TARGET_ABS")")
  TOTAL=1
fi

echo "Input:  $TARGET_ABS"
echo "Output: $OUTPUT_ROOT"
echo "Files:  $TOTAL"

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "Mode:   DRY RUN"
fi

i=0
while [[ "$i" -lt "$TOTAL" ]]; do
  if ! process_file "${FILES[$i]}" "${OUTPUTS[$i]}"; then
    :
  fi
  i=$((i + 1))
done

echo
echo "----------- REMUX SUMMARY -----------"
printf 'Files discovered:       %d\n' "$TOTAL"
printf 'Remuxed successfully:   %d\n' "$SUCCEEDED"
printf 'Skipped existing:       %d\n' "$SKIPPED"
printf 'Failed:                 %d\n' "$FAILED"
echo
printf 'Primary video kept:     %d\n' "$VIDEO_KEPT"
printf 'Secondary video dropped:%d\n' "$VIDEO_DROPPED"
printf 'English audio kept:     %d\n' "$AUDIO_KEPT"
printf 'Other audio dropped:    %d\n' "$AUDIO_DROPPED"
echo
printf 'English SRT kept:       %d\n' "$SUB_SRT_KEPT"
printf 'ASS/SSA -> SRT:         %d\n' "$SUB_ASS_CONVERTED"
printf 'Subtitles dropped:      %d\n' "$SUB_DROPPED"
printf 'No-English-sub hints:   %d\n' "$NO_SUB_HINTS"
echo

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "DRY RUN COMPLETE: no output files were created."
elif [[ "$FAILED" -ne 0 ]]; then
  echo "COMPLETED WITH FAILURES."
else
  echo "PASSED: all created outputs passed verification."
fi

[[ "$FAILED" -ne 0 ]] && exit 1
exit 0
