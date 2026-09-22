#!/usr/bin/env bash
# Regenerate the test corpus and its golden probe results.
#
# ffmpeg and ffprobe are contributor tools. Nothing in the test path or in CI runs
# this script; the files it writes are committed and only regenerated on purpose.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

for tool in ffmpeg ffprobe jq; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "$tool is not installed. Run 'brew install ffmpeg jq' (or your platform's equivalent) first." >&2
		exit 1
	fi
done

# Every file is two seconds of the same synthetic content: a 128x72 test pattern at
# 24 fps and a 440 Hz tone. 128x72 is deliberately not square, so a rotated file is
# distinguishable from its stored dimensions.
VIDEO=(-f lavfi -i "testsrc2=size=128x72:rate=24")
AUDIO=(-f lavfi -i "sine=frequency=440:sample_rate=48000")
ENCODE=(-t 2 -c:v libx264 -crf 40 -pix_fmt yuv420p -c:a aac -b:a 32k)

# Keep the output free of anything that changes between runs or between ffmpeg
# builds: encoder version strings, creation timestamps, the writing-application tag.
EXACT=(-fflags +bitexact -flags +bitexact -map_metadata -1)

FF=(ffmpeg -y -hide_banner -loglevel error)

# H.264 + AAC in mp4. The baseline, and the moov-at-the-end case: the mp4 muxer
# writes moov last unless told otherwise.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" "${ENCODE[@]}" "${EXACT[@]}" \
	h264-aac.mp4

# The same content with moov moved to the front.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" "${ENCODE[@]}" "${EXACT[@]}" \
	-movflags +faststart h264-aac-faststart.mp4

# HEVC + AAC in mov, 8-bit. hvc1 rather than hev1 is what Apple tools write.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" "${ENCODE[@]}" "${EXACT[@]}" \
	-c:v libx265 -profile:v main -pix_fmt yuv420p -tag:v hvc1 hevc-aac-8bit.mov

# HEVC + AAC in mov, 10-bit.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" "${ENCODE[@]}" "${EXACT[@]}" \
	-c:v libx265 -profile:v main10 -pix_fmt yuv420p10le -tag:v hvc1 hevc-aac-10bit.mov

# Video with no audio track.
"${FF[@]}" "${VIDEO[@]}" "${ENCODE[@]}" -an "${EXACT[@]}" \
	no-audio.mp4

# Fragmented mp4. frag_keyframe+empty_moov over +dash: it is the plain fragmented
# layout, an empty moov followed by moof/mdat pairs, without the sidx and the
# one-track-per-file rules dash adds on top.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" "${ENCODE[@]}" "${EXACT[@]}" \
	-movflags +frag_keyframe+empty_moov fragmented.mp4

# ProRes video and PCM audio in mov: the pair that has no valid representation in
# an MP4, which is what the remux compatibility table has to refuse. ProRes is an
# intra codec and costs orders of magnitude more per frame than the rest of the
# corpus, so this one is a quarter of a second at the proxy profile.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" -t 0.25 \
	-c:v prores_ks -profile:v 0 -pix_fmt yuv422p10le -c:a pcm_s16le "${EXACT[@]}" \
	prores-pcm.mov

# Rotation, as a tkhd matrix on the same content. -display_rotation is an input-only
# option in ffmpeg 9 and takes a counter-clockwise angle, so the files are produced by
# remuxing the baseline with the negated angle; the goldens report clockwise degrees.
# -metadata:s:v rotate= is the other candidate and writes nothing at all here.
for angle in 90 180 270; do
	"${FF[@]}" -display_rotation:v "-${angle}" -i h264-aac.mp4 \
		-c copy "${EXACT[@]}" "rotate-${angle}.mp4"
done

# Goldens. The mapping is the judgment, so it is spelled out rather than hidden in a
# helper: duration and bitrate come from the container, which knows the muxed size;
# framerate is the average, not the nominal rate, so a variable-framerate file gets an
# honest number; rotation is the display matrix negated into clockwise degrees.
#
# Codec names follow the scheme pkg/sprocket documents: one of the short names below,
# or else the sample entry's own four-character code. ffprobe's codec_name agrees with
# the short names and disagrees everywhere else, calling ProRes "prores" where the
# container says "apco", so anything off the list is read from codec_tag_string.
GOLDEN='
	["h264", "hevc", "av1", "vp8", "vp9", "aac", "mp3", "opus"] as $short
	| def codec($s): if ($short | index($s.codec_name)) then $s.codec_name else $s.codec_tag_string end;
	(.streams[] | select(.codec_type == "video")) as $v
	| ([.streams[] | select(.codec_type == "audio")] | first) as $a
	| ([$v.side_data_list // [] | .[] | select(.rotation != null) | .rotation] | first // 0) as $ccw
	| {
		duration: (.format.duration | tonumber),
		width: $v.width,
		height: $v.height,
		video_codec: codec($v),
		audio_codec: (if $a then codec($a) else "" end),
		bitrate: (.format.bit_rate | tonumber | floor),
		framerate: ($v.avg_frame_rate | split("/") | (.[0] | tonumber) / (.[1] | tonumber)),
		rotation: ((((-($ccw | round)) % 360) + 360) % 360),
	}
'

media=(*.mp4 *.mov)
for file in "${media[@]}"; do
	ffprobe -v error -print_format json -show_format -show_streams "$file" |
		jq "$GOLDEN" >"${file%.*}.json"
done

echo "wrote ${#media[@]} media files, $(du -sh . | cut -f1) total"
