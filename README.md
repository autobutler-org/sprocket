# sprocket

A Go library that probes, thumbnails, trims, and remuxes video files without ffmpeg.

## Status

`Probe` works on the ISOBMFF family: mp4, mov, m4v, 3gp, and 3g2. Thumbnail, Trim,
and Remux are not written yet, and neither is Matroska, WebM, or MPEG-TS support.
Work is tracked in [the epic, #13](https://github.com/autobutler-org/sprocket/issues/13).

Keyframe decoding covers HEVC, and H.264 behind the `h264` build tag. The tag is
there because H.264 is patent encumbered: Via LA's active AVC list still holds a
patent mapped to core decoding that runs to November 2030 in the US, so opting in
is a decision to take deliberately rather than one that arrives with `go get`.
Build with `-tags h264` to compile the decoder in; without it an H.264 sample
returns the unsupported-codec error naming the tag. That expiry is also when the
decision gets looked at again.

```go
file, err := os.Open("clip.mp4")
if err != nil {
	return err
}
defer file.Close()

stat, err := file.Stat()
if err != nil {
	return err
}

info, err := sprocket.Probe(file, stat.Size()) // *os.File is an io.ReaderAt
if err != nil {
	return err
}
fmt.Println(info.Duration, info.Width, info.Height, info.VideoCodec, info.Rotation)
```

`Probe` reads box headers and the movie header only, never the media payload, so a
4 GiB file costs a few kilobytes of reads. Codec names are a documented, stable
scheme, and so are the bitrate and frame rate rules; see the package documentation.
Input that is not a container this library reads returns `ErrUnsupportedContainer`
with no partial result, and headers that are malformed or cut short return
`ErrCorrupt`.

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
3g2), then Matroska and WebM, then MPEG-TS. Keyframe decoding covers HEVC and, behind
the `h264` build tag, H.264. VP9 has no known pure-Go path and returns the
unsupported-codec error.

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

`pkg/sprocket` holds the public API: the four operations, their result types,
`CanRemux`, and the typed errors. Container parsers and muxers live under
`internal/` until their API settles and there is a reason to expose them. No Go
files sit at the module root.

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

There are two builds of this library, with and without the `h264` build tag, so
those targets each run twice and both configurations have to pass.

## License

MIT No Attribution. See [LICENSE](LICENSE).

Dependencies are listed in [THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md) with
their licenses and the notices they carry. One of them, the HEVC decoder, puts it
this way: "This software implements a decoder and an encoder. It gives you no special
rights on the HEVC patents. HEVC is covered by patents held by several pools and by
unpooled holders; if you distribute or use this software you may need a licence from
them." No software license grants patent rights in HEVC or H.264, whoever wrote the
decoder, so the same goes for sprocket.

H.264 is in the same position, and its decoder carries no patent notice of its own,
so this is the only one sprocket's users get. AVC is covered by patents licensed through
Via LA's AVC Patent Portfolio License and by holders outside that pool; if you
distribute or use this software you may need a license from them. Via LA's published
fee schedule counts a decoder on its own as one unit at the same rate as an encoder,
so decoding is not a lighter position than encoding. That is what the `h264` build
tag is for: nothing about H.264 is compiled in unless you ask for it.
