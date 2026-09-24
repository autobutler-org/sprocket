# sprocket

A Go library that probes, thumbnails, trims, and remuxes video files without ffmpeg.

## Status

All four operations work on the ISOBMFF family: mp4, mov, m4v, 3gp, and 3g2, and
on Matroska and WebM, in both directions. An mp4 goes into an mkv or a webm, and
an mkv or a webm goes into an mp4, an m4v, or a 3gp. That last direction writes a
**fragmented** MP4: Matroska has no up-front index, so nothing before the media
says how many frames a track holds or how large each one is, and a `moov` cannot
be placed in front of the payload from a single pass. One `moof` and one `mdat`
per source cluster needs no index and costs a cluster's block headers. See
`Fragmented output` in the package documentation for what that means for the
decode times and the edit list the writer has to derive.

MPEG-TS (`.ts`, and the 192-byte-packet `.m2ts`) probes, thumbnails, and remuxes in
both directions: a TS goes into an mp4, an m4v, or a 3gp, fragmented for the same
reason as Matroska, and an MP4-family file goes into a TS. A transport stream has no
header and no index, so `Probe` scans it, at most 4 MiB from the front and 4 MiB from
the end, and a thumbnail seeks by estimate and reads forward to the keyframe. Trimming
a TS source is not supported yet; trimming an mp4 into a TS is. See `MPEG-TS` in the
package documentation. Work is tracked in
[the epic, #13](https://github.com/autobutler-org/sprocket/issues/13).

Keyframe decoding covers HEVC, VP8, and AV1, and H.264 behind the `h264` build
tag. VP9 has no pure-Go decoder and returns the unsupported-codec error. The tag is
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
4 GiB file costs a few kilobytes of reads; a transport stream, which has no header,
costs at most 8 MiB. Codec names are a documented, stable
scheme, and so are the bitrate and frame rate rules; see the package documentation.
Input that is not a container this library reads returns `ErrUnsupportedContainer`
with no partial result, and headers that are malformed or cut short return
`ErrCorrupt`.

```go
frame, err := sprocket.Thumbnail(file, stat.Size(), 5*time.Second,
	sprocket.ThumbnailOptions{MaxDimension: 320})
switch {
case errors.Is(err, sprocket.ErrUnsupportedCodec):
	return genericIcon() // the file is fine, this library cannot picture it
case err != nil:
	return err
}
fmt.Println(frame.Time) // the keyframe's own time, which is rarely the 5s asked for
return png.Encode(out, frame.Image)
```

`Thumbnail` snaps to a keyframe, so `Frame.Time` is the time of the frame that came
back rather than the one that was asked for. It picks the keyframe nearest the
request, breaking a tie towards the earlier one and answering a time past the end of
the file with the last keyframe. The track's rotation is applied, the planes are
converted to RGB, and `MaxDimension` caps the longer side of the result without ever
upscaling. `Frame.Image` is an `*image.RGBA`.

```go
out, err := os.Create("clip-trimmed.mp4")
if err != nil {
	return err
}
defer out.Close()

start, err := sprocket.Trim(file, stat.Size(), out, sprocket.MP4, 30*time.Second, 45*time.Second)
if err != nil {
	return err
}
fmt.Println(start) // where the cut really begins, which is the keyframe at or before 30s
```

`Trim` copies a range of samples into a new file without re-encoding. The start
snaps back to the keyframe at or before it, because a cut cannot begin mid-GOP
without re-encoding, so the returned time is the one to label the result with;
the end takes the last sample at or before it. Every track is cut against the
same window on its own sample boundaries, so audio lands within one frame of the
video, about 21 ms for AAC at 48 kHz. Timestamps are rewritten rather than
expressed as an edit list: the source's edit list is dropped and the composition
offsets are normalized in its place, so the output is displayed from its first
frame on any player. The package documentation writes out what that trades away.
Payload moves as byte ranges, so
cutting ten seconds out of a 4 GiB file costs the same heap as cutting them out
of a small one.

```go
if !sprocket.CanRemux(info, sprocket.MP4) {
	return fmt.Errorf("this file holds %s, which an mp4 cannot", info.VideoCodec)
}
out, err := os.Create("clip.mp4")
if err != nil {
	return err
}
defer out.Close()

return sprocket.Remux(file, stat.Size(), out, sprocket.MP4)
```

`Remux` moves the same streams into another container of the MP4 family without
re-encoding. The `moov` goes in front of the payload, so the output is progressively
playable and probing it costs a read of its head, and the payload is copied as byte
ranges through a single buffer, so a multi-gigabyte file costs the same heap as a
small one. Sample descriptions, edit lists, and display matrices are copied verbatim,
so the output probes and thumbnails to what the input did.

Not every stream fits every container: a MOV carrying ProRes or PCM has no valid
representation in an MP4. `CanRemux` answers that from a `Probe` result, so a caller
can offer only the targets that will work, and a remux that cannot happen returns
`ErrIncompatible` naming what blocked it. Both read the same table, which the package
documentation writes out in full.

## What it does

Four operations:

- Probe: duration, width, height, video codec, audio codec, bitrate, framerate, rotation.
- Thumbnail: the keyframe nearest a requested time, decoded to an `image.Image`.
- Trim: cut a range at keyframe boundaries by stream copy.
- Remux: move the same streams into a different container. MOV to MP4 is the headline case.

These are container operations. A container is bookkeeping: boxes, sample tables,
timestamps, offsets. The bulk of ffmpeg is codecs, and this library re-encodes
nothing, so it needs no encoder and needs a decoder for one purpose only.

Container support, in the order it was built: the ISOBMFF family (mp4, mov, m4v, 3gp,
3g2), then Matroska and WebM, then MPEG-TS. Keyframe decoding covers HEVC, VP8, AV1
and, behind the `h264` build tag, H.264. VP9 has no known pure-Go path and returns the
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
