# Test corpus

Small media files with a committed golden probe result each, so every parser and
every public function has something to test against.

Every file is two seconds of the same synthetic content: a 128x72 `testsrc2` pattern
at 24 fps and a 440 Hz `sine` tone. 128x72 is not square on purpose, so a rotated
file is distinguishable from its stored dimensions. Three files are exceptions:
`prores-pcm.mov` is a quarter of a second, because ProRes is an intra codec and costs
orders of magnitude more per frame than the rest, and `h264-gop12.mp4` and
`h264-gop12.mkv` are three seconds, because a trim needs room for more than one
keyframe. The sixteen media files total 369,832 bytes; the directory including the
goldens and the script is 472 KB on disk.

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
| `h264-aac.mkv` | H.264 + AAC in Matroska, the same bitstreams `h264-aac.mp4` carries, so a remux between the two families can be compared against a source that differs only in its container |
| `h264-gop12.mkv` | the Matroska twin of `h264-gop12.mp4`: three seconds with a keyframe every twelve frames, so the `Cues` index holds more than one entry |
| `vp8-vorbis.webm` | VP8 + Vorbis in WebM |
| `vp9-opus.webm` | VP9 + Opus in WebM: the video codec with no decoder here, which has to probe correctly and return the typed unsupported-codec error from the thumbnail path |
| `av1-opus.webm` | AV1 + Opus in WebM |

A later wave adds MPEG-TS, when that container lands.

## Generating

`generate.sh` in this directory writes every file, and `make corpus` runs it. It
needs ffmpeg, ffprobe, and jq; it was last run against ffmpeg 9.0.2. That build has
no libvorbis, so `vp8-vorbis.webm` uses ffmpeg's own Vorbis encoder, which is marked
experimental and needs `-strict -2` and stereo input; it is byte-stable all the same.
SVT-AV1 prints its configuration banner to stderr whatever the log level is, so the
AV1 command is noisy. Read the script
for the exact flags, one command per file.

ffmpeg is a contributor's tool. Nothing in the test path invokes the script and CI
installs no media tooling. The files and the goldens are committed data, regenerated
only on purpose.

Output is byte-stable: two consecutive runs produce identical files. That comes from
`-fflags +bitexact -flags +bitexact -map_metadata -1`, which strips encoder version
strings, creation timestamps, and the writing-application tag.

## Goldens

Each media file has a golden JSON named after the whole file, extension included:
`h264-aac.mp4.json` and `h264-aac.mkv.json`, since the same content is carried under
the same stem in two containers. The fields are the probe result
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

Codec names are lowercase short names: `h264`, `hevc`, `av1`, `vp8`, `vp9`, `aac`,
`mp3`, `opus`, and `vorbis`. That is the scheme #5 adopts as part of the public
contract, and anything off that list is named by its four-character sample entry
code, or in Matroska by its `CodecID` string, instead. ffprobe's `codec_name`
agrees with the short names and disagrees everywhere else, calling ProRes `prores`
where the container says `apco`, so the script reads `codec_tag_string` for anything
that is not one of the short names.

Two more mappings from ffprobe's output are worth spelling out. `framerate` is
`avg_frame_rate`, not `r_frame_rate`, so a variable-framerate file gets an honest
number instead of a nominal one. `rotation` is the video stream's display matrix
rotation negated: ffprobe reports a counter-clockwise angle in the range
(-180, 180], and the golden reports clockwise degrees normalized to [0, 360), which
matches the `rotate` tag convention that phones write.

The Matroska and WebM files report a duration a little over their two or three
seconds, because Matroska stores the duration the muxer measured rather than the
movie duration an mvhd declares, and that measurement runs to the end of the last
audio frame. Matroska carries no display matrix, so every one of them reports a
rotation of 0.

`fragmented.mp4` reports a duration of 2.083333 rather than 2.0. The fragmented
layout carries no edit list, so the trailing AAC frame is not trimmed away. That is a
property of the file, not an error in the golden.

## License

Every file here is generated from synthetic lavfi sources by `generate.sh`. No
downloaded footage. It is released under the repository license, MIT No Attribution;
see [LICENSE](../../LICENSE).
