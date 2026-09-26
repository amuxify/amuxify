#!/usr/bin/env bash
# Generates the amuxify test corpus with ffmpeg lavfi sources and mkvmerge.
# Usage: testdata/gen-fixtures.sh <outdir>
set -euo pipefail
out="${1:?outdir}"
mkdir -p "$out"
cd "$out"
ff() { ffmpeg -nostdin -hide_banner -loglevel error -y "$@"; }

# Base elementary streams: 4 seconds of video, two audio tracks, a subtitle.
ff -f lavfi -i testsrc2=size=320x240:rate=25 -t 4 -c:v libx264 -preset ultrafast -pix_fmt yuv420p video.h264
ff -f lavfi -i sine=frequency=440:sample_rate=48000 -t 4 -c:a aac audio_eng.m4a
ff -f lavfi -i sine=frequency=660:sample_rate=48000 -t 4 -c:a aac audio_und.m4a
ff -f lavfi -i sine=frequency=880:sample_rate=48000 -t 4 -c:a aac audio_ger.m4a
printf '1\n00:00:00,000 --> 00:00:02,000\nHello from https://example.com/tracker\n\n2\n00:00:02,000 --> 00:00:04,000\nClean line\n' > subs_links.srt
printf '1\n00:00:00,000 --> 00:00:02,000\nHello\n\n' > subs_clean.srt
printf 'CHAPTER01=00:00:00.000\nCHAPTER01NAME=Intro\nCHAPTER02=00:00:02.000\nCHAPTER02NAME=Visit www.example.org now\n' > chapters.txt
printf '<?xml version="1.0"?>\n<!DOCTYPE Tags SYSTEM "matroskatags.dtd">\n<Tags><Tag><Simple><Name>COMMENT</Name><String>ripped by someone http://evil.example/x</String></Simple><Simple><Name>PURCHASER</Name><String>buyer@example.com</String></Simple></Tag></Tags>\n' > tags.xml
printf 'MZ this is not really a font' > fake.ttf
printf '\x7fELF\x02\x01\x01\x00payload' > payload.bin
python3 - <<'PY'
# A tiny valid TrueType-ish header so sniff sees a font
open('real.ttf','wb').write(b'\x00\x01\x00\x00' + b'\x00'*60)
PY

# 1. Multi-language MKV: eng+und+ger audio, srt with link, chapters with link, tags, title, font attachment.
mkvmerge -q -o multi.mkv --title "Ripped by X - https://example.net" --global-tags tags.xml --chapters chapters.txt \
  --attach-file real.ttf \
  video.h264 \
  --language 0:eng --track-name 0:"English commentary" audio_eng.m4a \
  --language 0:und audio_und.m4a \
  --language 0:ger --track-name 0:"Deutsch" audio_ger.m4a \
  --language 0:eng subs_links.srt >/dev/null || true

# 2. Clean MKV: eng audio, clean subs, no title.
mkvmerge -q -o clean.mkv video.h264 --language 0:eng audio_eng.m4a --language 0:eng subs_clean.srt >/dev/null || true

# 3. MKV with an executable attachment.
mkvmerge -q -o exe_attach.mkv --attach-file payload.bin video.h264 --language 0:eng audio_eng.m4a >/dev/null || true

# 4. MKV with a fake font attachment (font name, PE content).
mkvmerge -q -o fake_font.mkv --attach-file fake.ttf video.h264 --language 0:eng audio_eng.m4a --language 0:eng subs_clean.srt >/dev/null || true

# 5. MP4 with purchase-style atoms and a title.
ff -i video.h264 -i audio_eng.m4a -c copy -metadata title="Bought Movie" -metadata comment="https://store.example/receipt" \
   -metadata:s:a:0 language=eng -movflags +faststart -f mp4 purchased.mp4
# 6. MOV and M4V variants, AVI, TS, WebM, MPG.
ff -i video.h264 -i audio_eng.m4a -c copy -metadata:s:a:0 language=eng -f mov sample.mov
ff -i video.h264 -i audio_eng.m4a -c copy -f mp4 sample.m4v
ff -f lavfi -i testsrc2=size=320x240:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 4 -c:v mpeg4 -c:a mp3 -f avi sample.avi
ff -i video.h264 -i audio_eng.m4a -c copy -f mpegts sample.ts
ff -f lavfi -i testsrc2=size=320x240:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 4 -c:v libvpx -c:a libopus -f webm sample.webm
ff -f lavfi -i testsrc2=size=320x240:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 -t 4 -c:v mpeg2video -c:a mp2 -f vob sample.mpg

# 7. Truncated MP4 (moov at end, cut off).
ff -i video.h264 -i audio_eng.m4a -c copy -f mp4 full.mp4
head -c 20000 full.mp4 > truncated.mp4
rm full.mp4
# 8. Polyglot: MKV with ZIP appended.
cp clean.mkv polyglot.mkv
python3 -c "import zipfile,sys; z=zipfile.ZipFile('x.zip','w'); z.writestr('a.txt','x'); z.close()"
cat x.zip >> polyglot.mkv
rm x.zip
# 9. Extension mismatch: MKV named .mp4.
cp clean.mkv mislabeled.mp4
# 10. Text file named .mkv.
printf 'not a video\n' > text.mkv
# 11. Empty file.
: > empty.mkv
# 12. Sidecars.
printf 'nfo text' > sample.nfo
printf '[InternetShortcut]\nURL=http://evil.example\n' > sample.url
printf 'echo hi\n' > run.sh
cp clean.mkv "exec_perm.mkv"; chmod +x exec_perm.mkv
# 13. Bidi filename.
cp clean.mkv "$(printf 'movie\xe2\x80\xaevkm.mkv')"
# 14. Hardlinked pair.
cp clean.mkv hard_a.mkv; ln hard_a.mkv hard_b.mkv
# 15. Symlink.
ln -s clean.mkv link.mkv
# 16. Double extension.
cp clean.mkv "double.mkv.exe"

rm -f video.h264 audio_*.m4a subs_*.srt chapters.txt tags.xml fake.ttf payload.bin real.ttf
echo "fixtures written to $out"
