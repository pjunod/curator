#!/usr/bin/env bash
# Regenerates the mediainfo prober corpus (internal/domain/mediainfo/testdata).
#
# Fixtures are real container headers produced by ffmpeg and then truncated to
# a header window -- the first N KiB of an MKV or a faststart MP4 parses
# identically to the whole file for everything the prober reads (ADR 0013 §2).
# One MKV is deliberately left whole; see mkv-720p-h264-aac-whole.mkv below.
# ffmpeg is a DEVELOPMENT tool only: monarr itself never shells out to it.
#
# Usage: scripts/gen-mediainfo-fixtures.sh   (needs ffmpeg on PATH)
set -euo pipefail

out="$(cd "$(dirname "$0")/.." && pwd)/internal/domain/mediainfo/testdata"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$out"

# v <name> <size> <pixfmt> <vcodec> <extra video args...>
src() { ffmpeg -hide_banner -loglevel error -f lavfi -i "testsrc2=size=$1:rate=24:duration=1" -f lavfi -i "sine=frequency=440:duration=1"; }

gen() { # gen <outfile> <ffmpeg args...>
  local f="$1"; shift
  ffmpeg -hide_banner -loglevel error -y \
    -f lavfi -i "testsrc2=size=${SIZE}:rate=24:duration=${DUR:-1}" \
    -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=${DUR:-1}" \
    "$@" "$tmp/$f"
}

trunc() { # trunc <src> <destname> <kib>
  head -c "$(( $3 * 1024 ))" "$tmp/$1" > "$out/$2"
  printf '%s original-size=%s\n' "$2" "$(stat -c%s "$tmp/$1")" >> "$out/SIZES.txt"
}
copy() { cp "$tmp/$1" "$out/$2"; }

: > "$out/SIZES.txt"

SIZE=1920x1080 gen a.mkv -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a ac3 -ac 6 -f matroska
trunc a.mkv mkv-1080p-h264-ac3.mkv 64

SIZE=3840x2160 gen b.mkv -c:v libx265 -preset ultrafast -pix_fmt yuv420p10le \
  -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc \
  -strict -2 -c:a truehd -ac 6 -f matroska
trunc b.mkv mkv-2160p-hevc-hdr10-truehd.mkv 64

SIZE=1920x1080 gen c.mkv -c:v libx265 -preset ultrafast -pix_fmt yuv420p10le \
  -color_primaries bt2020 -color_trc arib-std-b67 -colorspace bt2020nc \
  -c:a eac3 -ac 6 -f matroska
trunc c.mkv mkv-1080p-hevc-hlg-eac3.mkv 64

SIZE=1920x800 gen d.mkv -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
  -strict -2 -c:a dca -ac 6 -f matroska
trunc d.mkv mkv-1080p-scope-h264-dts.mkv 64

SIZE=720x576 gen e.mkv -c:v mpeg2video -flags +ilme+ildct -top 1 -pix_fmt yuv420p \
  -c:a ac3 -ac 2 -f matroska
trunc e.mkv mkv-576i-mpeg2-ac3.mkv 64

SIZE=1280x720 gen f.mkv -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -ac 2 -f matroska
trunc f.mkv mkv-720p-h264-aac.mkv 64

# The two fixtures that are WHOLE rather than header windows.
#
# Every other MKV here is the first 64 KiB of a real file, which parses
# identically for everything the prober reads -- and, since monarr learned to
# compare a Matroska Segment's declared length against the bytes on disk, is
# also correctly reported as TRUNCATED. That is a true statement about those
# fixtures and useful coverage in its own right, but it leaves nothing to
# stand in for an ordinary intact file. crf 51 keeps a whole one in the same
# size class as the windows. Two of them, at the resolutions the app-level
# probe tests assert.
SIZE=1280x720 gen fw.mkv -c:v libx264 -preset ultrafast -crf 51 -pix_fmt yuv420p -c:a aac -ac 2 -f matroska
copy fw.mkv mkv-720p-h264-aac-whole.mkv
SIZE=1920x1080 gen aw.mkv -c:v libx264 -preset ultrafast -crf 51 -pix_fmt yuv420p -c:a ac3 -ac 6 -f matroska
copy aw.mkv mkv-1080p-h264-ac3-whole.mkv

SIZE=1920x1080 gen g.mkv -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a flac -ac 6 -f matroska
trunc g.mkv mkv-1080p-h264-flac.mkv 64

SIZE=1920x1080 DUR=0.5 gen h.mkv -c:v libaom-av1 -cpu-used 8 -pix_fmt yuv420p -c:a libopus -ac 2 -strict -2 -f matroska
trunc h.mkv mkv-1080p-av1-opus.mkv 64

SIZE=1920x1080 gen i.mp4 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -ac 2 -movflags +faststart -f mp4
trunc i.mp4 mp4-1080p-h264-aac-faststart.mp4 64

SIZE=1920x1080 DUR=0.05 gen j.mp4 -c:v libx264 -preset ultrafast -crf 51 -pix_fmt yuv420p -c:a aac -ac 2 -f mp4
copy j.mp4 mp4-1080p-h264-aac-moov-at-end.mp4

SIZE=3840x2160 gen k.mp4 -c:v libx265 -preset ultrafast -pix_fmt yuv420p10le -tag:v hvc1 \
  -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc \
  -c:a eac3 -ac 6 -movflags +faststart -f mp4
trunc k.mp4 mp4-2160p-hevc-hdr10-eac3.mp4 64

SIZE=854x480 gen l.mp4 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a ac3 -ac 2 -movflags +faststart -f mp4
trunc l.mp4 mp4-480p-h264-ac3.mp4 64

# An AVI: a container the native prober deliberately does not deep-parse.
SIZE=1280x720 gen m.avi -c:v mpeg4 -pix_fmt yuv420p -c:a ac3 -ac 2 -f avi
trunc m.avi avi-720p-mpeg4-ac3.avi 32

ls -la "$out"
