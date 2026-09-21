# sprocket

A Go library that probes, thumbnails, trims, and remuxes video files without ffmpeg.

## Status

Nothing is implemented yet. The module is empty and the API is undecided. Work is
tracked in [the epic, #13](https://github.com/autobutler-org/sprocket/issues/13).

## What it does

Four operations:

- Probe: duration, width, height, video codec, audio codec, bitrate, framerate, rotation.
- Thumbnail: the keyframe nearest a requested time, decoded to an `image.Image`.
- Trim: cut a range at keyframe boundaries by stream copy.
- Remux: move the same streams into a different container. MOV to MP4 is the headline case.

These are container operations. A container is bookkeeping: boxes, sample tables,
timestamps, offsets. The bulk of ffmpeg is codecs, and this library re-encodes
nothing, so it needs no encoder and needs a decoder for one purpose only.

Planned container support, in priority order: the ISOBMFF family (mp4, mov, m4v, 3gp,
3g2), then Matroska and WebM, then MPEG-TS. Planned keyframe decoding: HEVC and H.264.
VP9 has no known pure-Go path and will return the unsupported-codec error.

## What it does not do

- No re-encoding, scaling, filters, or quality reduction of any kind. That is the line
  that keeps this from becoming an ffmpeg rewrite.
- No exact-timestamp frame extraction. Thumbnails snap to keyframes.
- No streaming protocols. This library reads files.
- No AVI, WMV, ASF, FLV, OGV, or MPEG-PS.

## Guarantees

- No cgo and no external binaries. Decoder backends may be compiled to WASM and run
  under wazero, which keeps the single-binary and cross-compile properties.
- Streaming with bounded memory. Inputs are user-controlled and unbounded, so the API
  takes an `io.ReaderAt` plus a size, or an `io.ReadSeeker`, never the file as a
  `[]byte`. Payload moves with `io.Copy` over byte ranges. Anything that must be read
  whole, a `moov` box or one keyframe sample, is capped by a limit this library picks
  and checks, never by a size the file declares.
- Cross-compiles to linux/arm64 and darwin/arm64. CI builds both.

## Package layout

The root package `sprocket` holds the public API: the four operations, their result
types, `CanRemux`, and the typed errors. Container parsers and muxers live under
`internal/` until their API settles and there is a reason to expose them.

## Development

Needs GNU Make 3.82 or newer. macOS ships 3.81, so use `gmake` there
(`brew install make`).

The lint tools are pinned in `tools/go.mod`, a separate module, so the library's
`go.mod` lists only what the library imports.

```sh
make check        # lint
make test         # tests and coverage
make build/cross  # linux/arm64 and darwin/arm64, CGO off
make help         # every target
```

## License

MIT No Attribution. See [LICENSE](LICENSE).
