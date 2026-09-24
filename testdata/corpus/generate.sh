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

# H.264 + AAC with a keyframe every half second, three seconds long. Every other
# file in the corpus has a single keyframe at the start, which cannot tell a trim
# that landed on the right keyframe from one that landed on the only keyframe.
# -g 12 -keyint_min 12 fixes the GOP at twelve frames of the 24 fps content and
# -sc_threshold 0 stops a scene cut from inserting one anywhere else.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" -t 3 \
	-c:v libx264 -crf 40 -pix_fmt yuv420p -g 12 -keyint_min 12 -sc_threshold 0 \
	-c:a aac -b:a 32k "${EXACT[@]}" \
	h264-gop12.mp4

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

# The Matroska family. The two mkv files carry the same H.264 and AAC bitstreams the
# mp4 files carry, so a remux between the families can be compared against a source
# that is byte-identical in everything but its container.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" "${ENCODE[@]}" "${EXACT[@]}" \
	h264-aac.mkv

"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" -t 3 \
	-c:v libx264 -crf 40 -pix_fmt yuv420p -g 12 -keyint_min 12 -sc_threshold 0 \
	-c:a aac -b:a 32k "${EXACT[@]}" \
	h264-gop12.mkv

# WebM, one file per codec the format allows. ffmpeg 9 here has no libvorbis, so the
# Vorbis file uses the built-in encoder, which is marked experimental and so needs
# -strict -2, and which refuses anything but stereo, hence -ac 2. It is byte-stable
# across runs all the same, which is what the corpus needs of it.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" -t 2 \
	-c:v libvpx -b:v 100k -c:a vorbis -strict -2 -ac 2 -b:a 32k "${EXACT[@]}" \
	vp8-vorbis.webm

"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" -t 2 \
	-c:v libvpx-vp9 -b:v 100k -c:a libopus -b:a 32k "${EXACT[@]}" \
	vp9-opus.webm

# SVT-AV1 prints its own configuration banner to stderr whatever ffmpeg's log level
# is, so this one command is noisy. -preset 12 is its fastest, which is what keeps a
# corpus regeneration quick; it maps down to preset 11 at this size.
"${FF[@]}" "${VIDEO[@]}" "${AUDIO[@]}" -t 2 \
	-c:v libsvtav1 -preset 12 -crf 50 -c:a libopus -b:a 32k "${EXACT[@]}" \
	av1-opus.webm

# Goldens. The mapping is the judgment, so it is spelled out rather than hidden in a
# helper: duration and bitrate come from the container, which knows the muxed size;
# framerate is the average, not the nominal rate, so a variable-framerate file gets an
# honest number; rotation is the display matrix negated into clockwise degrees.
#
# Codec names follow the scheme pkg/sprocket documents: one of the short names below,
# or else the sample entry's own four-character code, or, in Matroska, the CodecID
# string. ffprobe's codec_name agrees with the short names and disagrees everywhere
# else, calling ProRes "prores" where the container says "apco", so anything off the
# list is read from codec_tag_string.
GOLDEN='
	["h264", "hevc", "av1", "vp8", "vp9", "aac", "mp3", "opus", "vorbis"] as $short
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

# The golden is named after the whole media file, extension included, because two
# containers carry the same content under the same stem: h264-aac.mp4 and
# h264-aac.mkv.
media=(*.mp4 *.mov *.mkv *.webm)
for file in "${media[@]}"; do
	ffprobe -v error -print_format json -show_format -show_streams "$file" |
		jq "$GOLDEN" >"$file.json"
done

echo "wrote ${#media[@]} media files, $(du -sh . | cut -f1) total"
