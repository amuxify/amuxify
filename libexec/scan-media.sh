#!/usr/bin/env bash
#
# amux-scan
#
# Pre-ingestion safety scanner for media files and directories.
#
# Purpose:
#   Validate media before archival/NAS ingestion.
#
# This is a heuristic safety gate, not a sandbox. It reduces risk from
# malformed, misnamed or suspicious files but cannot guarantee that crafted
# media is harmless. Treat it as one layer of defense.
#
set -Eo pipefail
IFS=$'\n\t'

PROGRAM_NAME="${AMUXIFY_COMMAND:-amux-scan}"
PROGRAM_VERSION="${AMUXIFY_VERSION:-dev}"

trap 'printf "\nInterrupted.\n" >&2; exit 130' INT TERM

# ---------------------------------------------------------------------------
# Config / argument parsing
# ---------------------------------------------------------------------------
QUARANTINE_DIR=""
JSON_OUTPUT=0
DEEP_SCAN=0
STRICT_PERMISSIONS=0
CLAMAV_SCAN=0
TARGETS=()

# codec_tag_string values treated as known-benign "data" streams (never
# applies to "attachment" streams — those stay blocked unconditionally).
# tmcd (QuickTime timecode) is trusted by default: it's a fixed 4-byte
# integer with no room for a payload, structurally incapable of carrying
# anything. Add more via --allow-data-tag only after you've manually
# inspected what a tag actually contains — see usage text.
ALLOWED_DATA_TAGS=("tmcd")

usage() {
  cat >&2 <<EOF
Usage: $PROGRAM_NAME [OPTIONS] /path/to/file-or-directory [more-targets...]

Scan media files before archival or NAS ingestion.

Targets may be individual files, directories, or shell-expanded globs.
Directories are scanned recursively.

Options:
  --quarantine DIR
      Move blocked files into DIR while preserving relative paths.

  --deep
      Fully decode media with FFmpeg instead of only validating the
      container and streams. Much slower, but can detect corruption
      or truncation that ffprobe alone may miss.

  --strict-permissions
      Block files with the executable permission bit set.

  --clamav
      Scan each file with ClamAV.

  --allow-data-tag TAG
      Allow an additional embedded data-stream codec tag.
      May be specified more than once. "tmcd" is allowed by default.
      Attachments are always blocked.

  --json
      Emit the report as JSON instead of human-readable text.

  -h, --help
      Show this help.

Results:
  OK       File passed the enabled checks.
  WARNING  Review recommended, but the file is not blocked.
  BLOCKED  File failed the safety policy.

Examples:
  $PROGRAM_NAME ./movies
  $PROGRAM_NAME --deep ./movies
  $PROGRAM_NAME movie.mkv
EOF
}

die_usage() {
  usage
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --quarantine)
      [[ $# -ge 2 ]] || die_usage
      QUARANTINE_DIR="$2"
      shift 2
      ;;
    --json)
      JSON_OUTPUT=1
      shift
      ;;
    --deep)
      DEEP_SCAN=1
      shift
      ;;
    --strict-permissions)
      STRICT_PERMISSIONS=1
      shift
      ;;
    --clamav)
      CLAMAV_SCAN=1
      shift
      ;;
    --allow-data-tag)
      [[ $# -ge 2 ]] || die_usage
      ALLOWED_DATA_TAGS+=("$2")
      shift 2
      ;;
    --version)
      printf '%s %s\n' "$PROGRAM_NAME" "$PROGRAM_VERSION"
      exit 0
      ;;      
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    -*)
      echo "Unknown option: $1" >&2
      die_usage
      ;;
    *)
      TARGETS+=("$1")
      shift
      ;;
  esac
done

if [[ $# -gt 0 ]]; then
  TARGETS+=("$@")
fi

if [[ ${#TARGETS[@]} -eq 0 ]]; then
  die_usage
fi

for t in "${TARGETS[@]}"; do
  if [[ ! -d "$t" && ! -f "$t" && ! -L "$t" ]]; then
    echo "Not found: $t" >&2
    exit 2
  fi
done

# ---------------------------------------------------------------------------
# Dependency check
# ---------------------------------------------------------------------------
for cmd in find file ffprobe basename dirname mkdir mv grep awk; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Missing dependency: $cmd" >&2
    echo "On macOS: brew install ffmpeg (file/find/basename/grep are preinstalled)" >&2
    exit 2
  fi
done

if [[ "$DEEP_SCAN" -eq 1 ]] && ! command -v ffmpeg >/dev/null 2>&1; then
  echo "Missing dependency: ffmpeg (required for --deep)" >&2
  echo "On macOS: brew install ffmpeg" >&2
  exit 2
fi

if [[ "$CLAMAV_SCAN" -eq 1 ]] && ! command -v clamscan >/dev/null 2>&1; then
  echo "Missing dependency: clamscan (required for --clamav)" >&2
  echo "On macOS: brew install clamav && freshclam" >&2
  exit 2
fi

HAS_XATTR=0
command -v xattr >/dev/null 2>&1 && HAS_XATTR=1

# ---------------------------------------------------------------------------
# Hardening for ffprobe/ffmpeg calls themselves.
#
# ffprobe/ffmpeg are large C parsers with a long CVE history — running them
# over attacker-supplied bytes makes them part of the attack surface, not
# just a validation tool. Three constraints on every call against $path:
#   - protocol_whitelist file : blocks embedded http/https/concat/subfile/
#                               rtmp/etc. references from being followed.
#                               A crafted file can otherwise cause ffprobe to
#                               make outbound requests or read other local
#                               files during what looks like passive
#                               validation (an SSRF/local-file-read vector).
#   - timeout                 : bounds runtime against malformed inputs that
#                               trigger pathological parsing loops.
#   - ulimit -v               : bounds memory against declared-huge
#                               dimensions/stream counts (decompression-bomb
#                               style headers).
# ---------------------------------------------------------------------------
FFPROBE_TIMEOUT_SECS=60
FFMPEG_TIMEOUT_SECS=600
MEM_LIMIT_KB=2097152   # 2 GB virtual memory cap per ffprobe/ffmpeg call

TIMEOUT_BIN=""
if command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
elif command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
else
  echo "NOTE: no 'timeout'/'gtimeout' found — ffprobe/ffmpeg calls run unbounded." >&2
  echo "      On macOS: brew install coreutils (provides gtimeout)" >&2
fi

safe_ffprobe() {
  local secs="$1"
  shift

  (
    ulimit -v "$MEM_LIMIT_KB" 2>/dev/null

    if [[ -n "$TIMEOUT_BIN" ]]; then
      "$TIMEOUT_BIN" --foreground "$secs" ffprobe -protocol_whitelist file "$@"
    else
      ffprobe -protocol_whitelist file "$@"
    fi
  )
}

safe_ffmpeg() {
  local secs="$1"
  shift

  (
    ulimit -v "$MEM_LIMIT_KB" 2>/dev/null

    if [[ -n "$TIMEOUT_BIN" ]]; then
      "$TIMEOUT_BIN" --foreground "$secs" ffmpeg -protocol_whitelist file,pipe "$@"
    else
      ffmpeg -protocol_whitelist file,pipe "$@"
    fi
  )
}

# ---------------------------------------------------------------------------
# Portable absolute-path resolution.
# ---------------------------------------------------------------------------
abs_path() {
  local target_dir

  target_dir="$(cd "$1" 2>/dev/null && pwd -P)" || return 1
  printf '%s' "$target_dir"
}

abs_path_any() {
  local p="$1"
  local dir
  local base
  local resolved_dir

  if [[ -d "$p" && ! -L "$p" ]]; then
    abs_path "$p"
  else
    dir="$(dirname "$p")"
    base="$(basename "$p")"
    resolved_dir="$(abs_path "$dir")" || return 1
    printf '%s/%s' "$resolved_dir" "$base"
  fi
}

# Resolve every target up front.
RESOLVED_TARGETS_ABS=()
RESOLVED_TARGETS_IS_FILE=()
RESOLVED_TARGETS_IS_SYMLINK=()

for t in "${TARGETS[@]}"; do
  t_abs="$(abs_path_any "$t")" || {
    echo "Cannot resolve path: $t" >&2
    exit 2
  }

  t_is_file=0
  [[ -f "$t" && ! -d "$t" ]] && t_is_file=1

  t_is_symlink=0
  [[ -L "$t" ]] && t_is_symlink=1

  RESOLVED_TARGETS_ABS+=("$t_abs")
  RESOLVED_TARGETS_IS_FILE+=("$t_is_file")
  RESOLVED_TARGETS_IS_SYMLINK+=("$t_is_symlink")
done

# ---------------------------------------------------------------------------
# Quarantine directory setup + nesting validation.
# ---------------------------------------------------------------------------
QUARANTINE_ABS=""

if [[ -n "$QUARANTINE_DIR" ]]; then
  mkdir -p "$QUARANTINE_DIR" || {
    echo "Cannot create quarantine dir: $QUARANTINE_DIR" >&2
    exit 2
  }

  QUARANTINE_ABS="$(abs_path "$QUARANTINE_DIR")" || {
    echo "Cannot resolve quarantine path: $QUARANTINE_DIR" >&2
    exit 2
  }

  for i in "${!RESOLVED_TARGETS_ABS[@]}"; do
    [[ "${RESOLVED_TARGETS_IS_FILE[$i]}" -eq 1 ]] && continue

    t_abs="${RESOLVED_TARGETS_ABS[$i]}"

    case "$QUARANTINE_ABS" in
      "$t_abs"|"$t_abs"/*)
        echo "Quarantine directory must NOT be inside a target directory" >&2
        echo "  target:     $t_abs" >&2
        echo "  quarantine: $QUARANTINE_ABS" >&2
        exit 2
        ;;
    esac

    case "$t_abs" in
      "$QUARANTINE_ABS"|"$QUARANTINE_ABS"/*)
        echo "Target directory must NOT be inside the quarantine directory" >&2
        echo "  target:     $t_abs" >&2
        echo "  quarantine: $QUARANTINE_ABS" >&2
        exit 2
        ;;
    esac
  done
fi

# ---------------------------------------------------------------------------
# State
# ---------------------------------------------------------------------------
CHECKED=0
FAILED=0

declare -a BLOCKED_PATHS=()
declare -a BLOCKED_REASONS=()
declare -a WARNING_PATHS=()
declare -a WARNING_REASONS=()

VIDEO_EXTS='mkv|mp4|m4v|mov|avi|webm|mpg|mpeg|ts|m2ts|flv|wmv'
AUDIO_EXTS='mp3|flac|wav|aac|m4a|ogg|opus'
SUB_EXTS='srt|ass|ssa|vtt|sub'
FFPROBE_SUB_EXTS='srt|ass|ssa|vtt'
DANGEROUS_EXTS='exe|scr|com|msi|bat|cmd|ps1|vbs|js|jar|app|dmg|pkg|sh|command|py|pl|rb|bin|deb|rpm|apk|iso|img'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
json_escape() {
  local s="$1"
  local out=""
  local c
  local i
  local len
  local ord
  local esc

  len=${#s}

  for (( i=0; i<len; i++ )); do
    c="${s:i:1}"

    case "$c" in
      '"')
        out+='\"'
        ;;
      '\')
        out+='\\'
        ;;
      $'\t')
        out+='\t'
        ;;
      $'\n')
        out+='\n'
        ;;
      $'\r')
        out+='\r'
        ;;
      *)
        printf -v ord '%d' "'$c" 2>/dev/null || ord=32

        if (( ord < 0x20 )); then
          printf -v esc '\\u%04x' "$ord"
          out+="$esc"
        else
          out+="$c"
        fi
        ;;
    esac
  done

  printf '%s' "$out"
}

log_ok() {
  local kind="$1"
  local path="$2"
  local note="${3:-}"

  if [[ "$JSON_OUTPUT" -eq 0 ]]; then
    echo "${CHECKED}. OK $kind: $path"
    [[ -n "$note" ]] && echo "        $note"
  fi
}

log_warn() {
  local reason="$1"
  local path="$2"
  local detail="${3:-}"

  WARNING_PATHS+=("$path")
  WARNING_REASONS+=("$reason")

  if [[ "$JSON_OUTPUT" -eq 0 ]]; then
    echo "${CHECKED}. WARNING $reason: $path"
    [[ -n "$detail" ]] && echo "        $detail"
  fi
}

log_block() {
  local reason="$1"
  local path="$2"
  local detail="${3:-}"

  FAILED=1
  BLOCKED_PATHS+=("$path")
  BLOCKED_REASONS+=("$reason")

  if [[ "$JSON_OUTPUT" -eq 0 ]]; then
    echo "${CHECKED}. BLOCKED $reason: $path"
    [[ -n "$detail" ]] && echo "        $detail"
  fi

  if [[ -n "$QUARANTINE_DIR" ]]; then
    quarantine_file "$path"
  fi
}

quarantine_file() {
  local path="$1"
  local rel
  local dest_dir

  if [[ "$CURRENT_TARGET_IS_FILE" -eq 1 ]]; then
    dest_dir="$QUARANTINE_DIR"
  else
    rel="${path#"$CURRENT_TARGET_ABS"/}"
    dest_dir="$QUARANTINE_DIR/$(dirname "$rel")"
  fi

  mkdir -p "$dest_dir" 2>/dev/null

  if mv -n -- "$path" "$dest_dir/" 2>/dev/null; then
    [[ "$JSON_OUTPUT" -eq 0 ]] && echo "        -> quarantined to $dest_dir/$(basename "$path")"
  else
    [[ "$JSON_OUTPUT" -eq 0 ]] && echo "        -> WARNING: quarantine move failed" >&2
  fi
}

# ---------------------------------------------------------------------------
# Polyglot / trailing-data detection.
# ---------------------------------------------------------------------------
check_polyglot() {
  local path="$1"
  local tail_hit

  tail_hit="$(tail -c 70000 -- "$path" 2>/dev/null | LC_ALL=C grep -aF -o -e $'PK\x03\x04' -e $'PK\x05\x06' -e $'Rar!\x1a\x07' -e $'7z\xbc\xaf\x27\x1c' -e $'\x7fELF' 2>/dev/null | head -n1)"

  if [[ -n "$tail_hit" ]]; then
    printf 'archive/executable signature near end of file (possible appended/smuggled payload)'
    return 0
  fi

  if LC_ALL=C grep -aF -q -- 'This program cannot be run in DOS mode' "$path" 2>/dev/null; then
    printf 'embedded PE executable signature (DOS stub string) found in file'
    return 0
  fi

  return 1
}

# ---------------------------------------------------------------------------
# Per-file scan.
# ---------------------------------------------------------------------------
scan_file() {
  local path="$1"

  name="$(basename "$path")"
  lower="$(LC_ALL=C printf '%s' "$name" | LC_ALL=C tr '[:upper:]' '[:lower:]')"
  ext="${lower##*.}"

  if [[ "$name" == ".DS_Store" ]]; then
    return
  fi

  CHECKED=$((CHECKED + 1))

  if LC_ALL=C printf '%s' "$name" | LC_ALL=C grep -aE -q $'\xe2\x80\xaa|\xe2\x80\xab|\xe2\x80\xac|\xe2\x80\xad|\xe2\x80\xae|\xe2\x80\x8e|\xe2\x80\x8f|\xe2\x81\xa6|\xe2\x81\xa7|\xe2\x81\xa8|\xe2\x81\xa9'; then
    log_block "filename contains Unicode bidi-override/invisible characters (possible extension-spoofing)" "$path"
    return
  fi

  if [[ ! -s "$path" ]]; then
    log_block "empty file" "$path"
    return
  fi

  if [[ ! -r "$path" ]]; then
    log_block "unreadable file" "$path"
    return
  fi

  if [[ "$CLAMAV_SCAN" -eq 1 ]]; then
    local clam_out
    local clam_rc

    [[ "$JSON_OUTPUT" -eq 0 ]] && echo "        ClamAV scanning..."

    if [[ "$JSON_OUTPUT" -eq 0 ]]; then
      clamscan --verbose -- "$path"
      clam_rc=$?
      clam_out=""
    else
      clam_out="$(clamscan --no-summary -i -- "$path" 2>/dev/null)"
      clam_rc=$?
    fi

    if [[ "$clam_rc" -eq 1 ]]; then
      log_block "malware signature detected (ClamAV)" "$path" "$clam_out"
      return
    elif [[ "$clam_rc" -ge 2 ]]; then
      log_warn "ClamAV scan error (not blocking — check clamscan/freshclam)" "$path"
    fi
  fi

  if [[ -x "$path" ]]; then
    if [[ "$STRICT_PERMISSIONS" -eq 1 ]]; then
      log_block "executable permission set" "$path"
      return
    else
      log_warn "executable permission set (not blocking — use --strict-permissions to enforce; FAT/exFAT can mark all files executable)" "$path"
    fi
  fi

  if [[ "$ext" =~ ^($DANGEROUS_EXTS)$ ]]; then
    log_block "dangerous/script extension" "$path"
    return
  fi

  category=""

  if [[ "$ext" =~ ^($VIDEO_EXTS)$ ]]; then
    category="video"
  elif [[ "$ext" =~ ^($AUDIO_EXTS)$ ]]; then
    category="audio"
  elif [[ "$ext" =~ ^($SUB_EXTS)$ ]]; then
    category="subtitle"
  else
    log_block "unsupported extension" "$path"
    return
  fi

  mime="$(file -b --mime-type -- "$path" 2>/dev/null || echo "unknown")"

  from_internet=0

  if [[ "$HAS_XATTR" -eq 1 ]]; then
    if xattr -p com.apple.quarantine -- "$path" >/dev/null 2>&1; then
      from_internet=1
    fi
  fi

  note=""
  [[ "$from_internet" -eq 1 ]] && note="note: downloaded from internet (Gatekeeper quarantine flag set)"

  if [[ "$category" == "subtitle" ]]; then
    sub_size_before="$(wc -c < "$path" | tr -d ' ')"
    sub_size_after="$(LC_ALL=C tr -d '\000' < "$path" | wc -c | tr -d ' ')"

    if [[ "$sub_size_before" != "$sub_size_after" ]]; then
      log_block "binary content in subtitle file (NUL byte found)" "$path"
      return
    fi

    case "$mime" in
      text/*|application/x-subrip)
        :
        ;;
      application/octet-stream)
        :
        ;;
      *)
        log_block "invalid subtitle MIME type" "$path" "MIME: $mime"
        return
        ;;
    esac

    if [[ "$ext" =~ ^($FFPROBE_SUB_EXTS)$ ]]; then
      if ! safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_entries format=format_name -of default=noprint_wrappers=1:nokey=1 -- "$path" >/dev/null 2>&1; then
        log_block "unparseable subtitle content" "$path"
        return
      fi
    fi

    log_ok "subtitle" "$path" "$note"
    return
  fi

  if ! safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_entries format=format_name -of default=noprint_wrappers=1:nokey=1 -- "$path" >/dev/null 2>&1; then
    log_block "invalid/unparseable media container" "$path"
    return
  fi

  stream_info="$(safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_entries stream=index,codec_type,codec_name,codec_tag_string -of csv=p=0 -- "$path" 2>/dev/null)"

  expected_type="video"
  [[ "$category" == "audio" ]] && expected_type="audio"

  has_expected_stream=0
  local -a bad_stream_lines=()

  while IFS=',' read -r s_index s_codec s_type s_tag; do
    [[ -z "$s_type" ]] && continue

    [[ "$s_type" == "$expected_type" ]] && has_expected_stream=1

    if [[ "$s_type" == "attachment" ]]; then
      bad_stream_lines+=("stream #$s_index attachment (codec=$s_codec tag=$s_tag)")
    elif [[ "$s_type" == "data" ]]; then
      local tag_is_allowed=0
      local allowed_tag

      for allowed_tag in "${ALLOWED_DATA_TAGS[@]}"; do
        [[ "$s_tag" == "$allowed_tag" ]] && {
          tag_is_allowed=1
          break
        }
      done

      if [[ "$tag_is_allowed" -eq 0 ]]; then
        bad_stream_lines+=("stream #$s_index data (codec=$s_codec tag=$s_tag) — not in ALLOWED_DATA_TAGS, use --allow-data-tag $s_tag if you've verified it's safe")
      fi
    fi
  done <<< "$stream_info"

  if [[ "$has_expected_stream" -eq 0 ]]; then
    log_block "no expected $category stream found in container" "$path"
    return
  fi

  if [[ ${#bad_stream_lines[@]} -gt 0 ]]; then
    local detail

    detail="$(printf '%s; ' "${bad_stream_lines[@]}")"
    log_block "embedded attachment/data stream present in container" "$path" "$detail"
    return
  fi

  local poly_reason

  if poly_reason="$(check_polyglot "$path")"; then
    log_block "$poly_reason" "$path"
    return
  fi

  case "$mime" in
    video/*)
      if [[ "$category" != "video" ]]; then
        log_block "MIME/extension mismatch (video content, $category extension)" "$path" "MIME: $mime"
        return
      fi
      ;;
    audio/*)
      if [[ "$category" != "audio" ]]; then
        log_block "MIME/extension mismatch (audio content, $category extension)" "$path" "MIME: $mime"
        return
      fi
      ;;
    application/octet-stream)
      :
      ;;
    *)
      log_block "unexpected MIME type" "$path" "MIME: $mime"
      return
      ;;
  esac

  if [[ "$DEEP_SCAN" -eq 1 ]]; then
    local duration
    local current_us
    local current_time
    local speed_num
    local calc
    local percent
    local pos_h
    local pos_m
    local pos_s
    local eta_m
    local eta_s

    duration="$(safe_ffprobe "$FFPROBE_TIMEOUT_SECS" -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 -- "$path" 2>/dev/null || true)"

    if ! [[ "$duration" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
      duration=""
    fi

    if [[ "$JSON_OUTPUT" -eq 0 ]]; then
      printf '\n----------- DEEP DECODING ENTIRE FILE -----------\n'

      if ! safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -nostats -xerror -progress pipe:1 -stats_period 1 -i "$path" -f null - |
        (
          current_us=""
          current_time=""
          speed_num=""

          while IFS='=' read -r key value; do
            case "$key" in
              out_time_us)
                current_us="$value"
                ;;
              out_time)
                current_time="$value"
                ;;
              speed)
                speed_num="${value%x}"
                speed_num="$(printf '%s' "$speed_num" | tr -d ' ')"
                ;;
              progress)
                if [[ -n "$speed_num" && "$speed_num" != "N/A" ]]; then
                  calc=""

                  if [[ -n "$current_us" && "$current_us" =~ ^[0-9]+$ ]]; then
                    calc="$(awk -v current_us="$current_us" -v total="$duration" -v speed="$speed_num" '
                      BEGIN {
                        current = current_us / 1000000

                        if (speed + 0 <= 0) exit

                        ph = int(current / 3600)
                        pm = int(current / 60) % 60
                        ps = int(current) % 60

                        if (total + 0 > 0) {
                          remaining = (total - current) / speed
                          if (remaining < 0) remaining = 0

                          pct = (current / total) * 100
                          if (pct > 100) pct = 100
                          if (pct < 0) pct = 0

                          em = int(remaining / 60)
                          es = int(remaining) % 60

                          printf "%.1f %d %d %d %d %d\n", pct, ph, pm, ps, em, es
                        } else {
                          printf "NA %d %d %d 0 0\n", ph, pm, ps
                        }
                      }'
                    )"
                  elif [[ -n "$current_time" ]]; then
                    calc="$(awk -v stamp="$current_time" -v total="$duration" -v speed="$speed_num" '
                      BEGIN {
                        split(stamp, t, ":")
                        current = (t[1] * 3600) + (t[2] * 60) + t[3]

                        if (speed + 0 <= 0) exit

                        ph = int(current / 3600)
                        pm = int(current / 60) % 60
                        ps = int(current) % 60

                        if (total + 0 > 0) {
                          remaining = (total - current) / speed
                          if (remaining < 0) remaining = 0

                          pct = (current / total) * 100
                          if (pct > 100) pct = 100
                          if (pct < 0) pct = 0

                          em = int(remaining / 60)
                          es = int(remaining) % 60

                          printf "%.1f %d %d %d %d %d\n", pct, ph, pm, ps, em, es
                        } else {
                          printf "NA %d %d %d 0 0\n", ph, pm, ps
                        }
                      }'
                    )"
                  fi

                  if [[ -n "$calc" ]]; then
                    IFS=' ' read -r percent pos_h pos_m pos_s eta_m eta_s <<< "$calc"

                    if [[ "$percent" == "NA" ]]; then
                      printf '\r        Decoded position: %02d:%02d:%02d | speed: %sx | ETA: unavailable   ' "$pos_h" "$pos_m" "$pos_s" "$speed_num"
                    else
                      printf '\r        Decoded: %5.1f%% | position: %02d:%02d:%02d | speed: %sx | ETA: %02d:%02d   ' "$percent" "$pos_h" "$pos_m" "$pos_s" "$speed_num" "$eta_m" "$eta_s"
                    fi
                  fi
                fi

                if [[ "$value" == "end" ]]; then
                  printf '\n'
                fi
                ;;
            esac
          done
        )
      then
        printf '\n'
        log_block "failed full decode (truncated/corrupt media or decode timeout)" "$path"
        return
      fi
    else
      if ! safe_ffmpeg "$FFMPEG_TIMEOUT_SECS" -nostdin -hide_banner -v error -nostats -xerror -i "$path" -f null - >/dev/null 2>&1; then
        log_block "failed full decode (truncated/corrupt media or decode timeout)" "$path"
        return
      fi
    fi
  fi

  log_ok "$category" "$path" "$note"
}

# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------
for i in "${!RESOLVED_TARGETS_ABS[@]}"; do
  CURRENT_TARGET_ABS="${RESOLVED_TARGETS_ABS[$i]}"
  CURRENT_TARGET_IS_FILE="${RESOLVED_TARGETS_IS_FILE[$i]}"
  CURRENT_TARGET_IS_SYMLINK="${RESOLVED_TARGETS_IS_SYMLINK[$i]}"

  if [[ "$CURRENT_TARGET_IS_SYMLINK" -eq 1 ]]; then
    CHECKED=$((CHECKED + 1))
    log_block "symlink not permitted" "$CURRENT_TARGET_ABS"
    continue
  fi

  if [[ "$JSON_OUTPUT" -eq 0 ]]; then
    echo "Scanning: $CURRENT_TARGET_ABS"
    echo
  fi

  if [[ "$CURRENT_TARGET_IS_FILE" -eq 1 ]]; then
    scan_file "$CURRENT_TARGET_ABS"
  else
    while IFS= read -r -d '' link; do
      CHECKED=$((CHECKED + 1))
      log_block "symlink not permitted" "$link"
    done < <(find "$CURRENT_TARGET_ABS" -type l -print0 2>/dev/null)

    while IFS= read -r -d '' path; do
      scan_file "$path"
    done < <(find "$CURRENT_TARGET_ABS" -type f ! -name '.DS_Store' -print0)
  fi
done

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------
if [[ "$JSON_OUTPUT" -eq 1 ]]; then
  {
    echo "{"
    echo '  "targets": ['

    for i in "${!RESOLVED_TARGETS_ABS[@]}"; do
      sep=","
      [[ "$i" -eq $((${#RESOLVED_TARGETS_ABS[@]} - 1)) ]] && sep=""

      printf '    "%s"%s\n' "$(json_escape "${RESOLVED_TARGETS_ABS[$i]}")" "$sep"
    done

    echo '  ],'
    printf '  "checked": %d,\n' "$CHECKED"
    printf '  "deep_scan": %s,\n' "$([[ "$DEEP_SCAN" -eq 1 ]] && echo true || echo false)"
    printf '  "strict_permissions": %s,\n' "$([[ "$STRICT_PERMISSIONS" -eq 1 ]] && echo true || echo false)"
    printf '  "failed": %s,\n' "$([[ "$FAILED" -ne 0 ]] && echo true || echo false)"

    echo '  "blocked": ['

    for i in "${!BLOCKED_PATHS[@]}"; do
      sep=","
      [[ "$i" -eq $((${#BLOCKED_PATHS[@]} - 1)) ]] && sep=""

      printf '    {"path": "%s", "reason": "%s"}%s\n' "$(json_escape "${BLOCKED_PATHS[$i]}")" "$(json_escape "${BLOCKED_REASONS[$i]}")" "$sep"
    done

    echo '  ],'
    echo '  "warnings": ['

    for i in "${!WARNING_PATHS[@]}"; do
      sep=","
      [[ "$i" -eq $((${#WARNING_PATHS[@]} - 1)) ]] && sep=""

      printf '    {"path": "%s", "reason": "%s"}%s\n' "$(json_escape "${WARNING_PATHS[$i]}")" "$(json_escape "${WARNING_REASONS[$i]}")" "$sep"
    done

    echo '  ]'
    echo "}"
  }
else
  echo
  echo "Checked: $CHECKED files"

  if [[ ${#WARNING_PATHS[@]} -gt 0 ]]; then
    echo
    echo "Warnings (non-blocking):"

    for i in "${!WARNING_PATHS[@]}"; do
      echo "  [${WARNING_REASONS[$i]}] ${WARNING_PATHS[$i]}"
    done
  fi

  if [[ "$FAILED" -ne 0 ]]; then
    echo
    echo "Summary of blocked files:"

    for i in "${!BLOCKED_PATHS[@]}"; do
      echo "  [${BLOCKED_REASONS[$i]}] ${BLOCKED_PATHS[$i]}"
    done

    echo
    echo "FAILED: suspicious or unsupported files found."
  else
    echo "PASSED: all files are valid media or subtitles."
  fi
fi

[[ "$FAILED" -ne 0 ]] && exit 1
exit 0