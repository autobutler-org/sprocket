# Test corpus

Small media files with a committed golden probe result each, so every parser and
every public function has something to test against.

Every file is two seconds of the same synthetic content: a 128x72 `testsrc2` pattern
at 24 fps and a 440 Hz `sine` tone. 128x72 is not square on purpose, so a rotated
file is distinguishable from its stored dimensions. Two files are exceptions:
`prores-pcm.mov` is a quarter of a second, because ProRes is an intra codec and costs
orders of magnitude more per frame than the rest, and `h264-gop12.mp4` is three
seconds, because a trim needs room for more than one keyframe. The eleven media files
total 227,105 bytes; the directory including the goldens and the script is 300 KB on
disk.

## Files

| File | What it covers |
| --- | --- |
| `h264-aac.mp4` | H.264 + AAC in mp4, and the `moov`-at-the-end case, which is the mp4 muxer's default |
| `h264-aac-faststart.mp4` | the same content with `moov` moved to the front |
| `h264-gop12.mp4` | three seconds with a keyframe every twelve frames, at 0, 0.5, 1.0, 1.5, 2.0, and 2.5 seconds: the file a trim can snap to a keyframe other than the first |
| `hevc-aac-8bit.mov` | HEVC + AAC in mov, 8-bit, `hvc1` sample entry |
| `hevc-aac-10bit.mov` | HEVC + AAC in mov, 10-bit (`main10`, `yuv420p10le`) |
| `rotate-90.mp4` | a `tkhd` matrix of 90 degrees clockwise |
| `rotate-180.mp4` | a `tkhd` matrix of 180 degrees |
| `rotate-270.mp4` | a `tkhd` matrix of 270 degrees clockwise |
| `no-audio.mp4` | video with no audio track |
| `fragmented.mp4` | fragmented mp4: an empty `moov` followed by `moof`/`mdat` pairs |
| `prores-pcm.mov` | ProRes proxy + 16-bit PCM in mov: the pair no mp4 can hold, which the remux compatibility table has to refuse |

Later waves add mkv, webm, and MPEG-TS, when those containers land.

## Generating

`generate.sh` in this directory writes every file, and `make corpus` runs it. It
needs ffmpeg, ffprobe, and jq; it was last run against ffmpeg 9.0.2. Read the script
for the exact flags, one command per file.

ffmpeg is a contributor's tool. Nothing in the test path invokes the script and CI
installs no media tooling. The files and the goldens are committed data, regenerated
only on purpose.

Output is byte-stable: two consecutive runs produce identical files. That comes from
`-fflags +bitexact -flags +bitexact -map_metadata -1`, which strips encoder version
strings, creation timestamps, and the writing-application tag.

## Goldens

Each media file has a golden JSON of the same name. The fields are the probe result
that [#5](https://github.com/autobutler-org/sprocket/issues/5) defines:

| Field | Meaning |
| --- | --- |
| `duration` | seconds, float, from the container |
| `width`, `height` | the stored dimensions, before rotation is applied |
| `video_codec` | codec short name |
| `audio_codec` | codec short name, empty when there is no audio track |
| `bitrate` | bits per second, from the container's declared rate |
| `framerate` | frames per second, the average rate rather than the nominal one |
| `rotation` | clockwise degrees, one of 0, 90, 180, and 270 |

Codec names are lowercase short names: `h264`, `hevc`, `aac`, `opus`, `av1`, `vp9`.
That is the scheme #5 adopts as part of the public contract, and anything off that
list is named by its four-character sample entry code instead. ffprobe's `codec_name`
agrees with the short names and disagrees everywhere else, calling ProRes `prores`
where the container says `apco`, so the script reads `codec_tag_string` for anything
that is not one of the short names.

Two more mappings from ffprobe's output are worth spelling out. `framerate` is
`avg_frame_rate`, not `r_frame_rate`, so a variable-framerate file gets an honest
number instead of a nominal one. `rotation` is the video stream's display matrix
rotation negated: ffprobe reports a counter-clockwise angle in the range
(-180, 180], and the golden reports clockwise degrees normalized to [0, 360), which
matches the `rotate` tag convention that phones write.

`fragmented.mp4` reports a duration of 2.083333 rather than 2.0. The fragmented
layout carries no edit list, so the trailing AAC frame is not trimmed away. That is a
property of the file, not an error in the golden.

## License

Every file here is generated from synthetic lavfi sources by `generate.sh`. No
downloaded footage. It is released under the repository license, MIT No Attribution;
see [LICENSE](../../LICENSE).
