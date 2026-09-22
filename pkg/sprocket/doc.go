// Package sprocket probes, thumbnails, trims, and remuxes video files.
//
// Probe reports duration, dimensions, codecs, bitrate, framerate, and rotation.
// Thumbnail decodes the keyframe nearest a requested time to an image.Image.
// Trim cuts a range at keyframe boundaries by copying streams. Remux moves the
// same streams into a different container. Trim and Remux never re-encode.
//
// Guarantees:
//
//   - No cgo and no external binaries. Decoder backends may be compiled to
//     WASM and run under wazero, which keeps those properties.
//   - Bounded memory. Inputs are read through an io.ReaderAt or io.ReadSeeker
//     and payload moves with io.Copy over byte ranges, so a multi-gigabyte file
//     costs megabytes of heap. Anything read whole is capped by a limit the
//     library chooses, never by a size the file declares.
//   - Cross-compiles to linux/arm64, darwin/arm64, and the host.
//
// Non-goals: re-encoding, scaling, filters, and quality reduction of any kind;
// exact-timestamp frame extraction, since thumbnails snap to keyframes;
// streaming protocols.
//
// # Status
//
// Probe and Thumbnail are implemented for the ISOBMFF family: mp4, mov, m4v,
// 3gp, and 3g2. Keyframe decoding covers HEVC, and H.264 behind the h264 build
// tag. Trim and Remux are not written yet, and neither is support for
// Matroska, WebM, or MPEG-TS. See
// https://github.com/autobutler-org/sprocket/issues/13.
//
// # Thumbnails
//
// Thumbnail snaps to a keyframe. Decoding forward from one to an arbitrary
// frame is a non-goal, so the frame that comes back is the keyframe nearest the
// requested time and Frame.Time says which one it was. Nearest is measured on
// presentation times; a tie goes to the earlier keyframe, a time past the end
// of the file answers with the last keyframe, and a negative time is read as
// zero. A file with one keyframe answers every request with it.
//
// The frame is decoded, then turned the right way up by the track's rotation,
// then resized if ThumbnailOptions.MaxDimension asked for it. The result is an
// *image.RGBA, so it draws and encodes the way a caller expects without
// knowing what the stream carried.
//
// Getting there means a color conversion, because a decoder hands back luma and
// chroma planes in the stream's own range. Those planes are read as studio
// range, luma 16 to 235 and chroma 16 to 240, since a container does not pass a
// full-range flag through to this library yet and video that carries one is
// rare. The matrix is chosen by picture height, BT.601 below 720 lines and
// BT.709 at or above, which is what players do for video that signals nothing.
// A stream that signals something else is decoded correctly and colored by
// those rules anyway; passing the signalled primaries, transfer, and matrix
// through is work still to do.
//
// MaxDimension caps the longer side of the image, after rotation, preserving
// the aspect ratio. It never upscales: a frame already inside the cap comes
// back at its own size, and so does every frame when MaxDimension is zero.
// Resizing a decoded frame is not re-encoding and not a filter; it is there so
// that asking for a 200 pixel thumbnail does not mean holding a 4K image.
//
// Codecs without a decoder return ErrUnsupportedCodec, which is the signal to
// fall back to a generic icon rather than to fail. VP9 is one of those today,
// and so is H.264 in a build without the h264 build tag. A file with no video
// track returns ErrNoVideo, which is a different thing: nothing is wrong with
// the file, it just holds no picture.
//
// # Codec names
//
// Codec names are lowercase short names. They are part of the public contract
// and callers may switch on them:
//
//	h264, hevc, av1, vp8, vp9, aac, mp3, opus
//
// Anything else comes through as its four-character sample entry code exactly as
// the file stores it, case included: "ac-3", "ec-3", "fLaC", "alac". Those are
// readable enough to log or to match on, but they are not yet promised to stay
// as they are; the eight names above are. The list grows as containers and
// codecs are added, and a name on it never changes meaning.
//
// # Bitrate
//
// Info.Bitrate is the overall rate: the size of the whole file in bits over the
// movie duration, truncated to an integer. It counts container overhead along
// with the media, which is what a player and what ffprobe's format bit_rate
// report. It is 0 when the duration is 0.
//
// A declared bitrate is not consulted. The demuxer does not read btrt or the
// esds maximum bitrate, so there is nothing to prefer or to disagree with yet.
// A declared value is the better source where a file carries one, and this may
// change to prefer it.
//
// # Frame rate
//
// Info.FrameRate is an average: the video track's sample count over its media
// duration, taken from the sample tables. A variable-framerate file therefore
// reports the rate it actually ran at rather than a nominal one, and a file
// whose nominal rate is a lie reports the truth. It is 0 when there is no video
// track, no video sample, or no duration.
package sprocket
