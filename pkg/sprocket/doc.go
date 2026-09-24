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
// All four operations are implemented for the ISOBMFF family: mp4, mov, m4v,
// 3gp, and 3g2. Keyframe decoding covers HEVC, and H.264 behind the h264 build
// tag. Matroska, WebM, and MPEG-TS are not supported yet. See
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
// # Trim
//
// Trim cuts a range out of a file and writes it as a file of the same family,
// copying the samples across as byte ranges. Nothing is decoded and nothing is
// re-encoded, which is what fixes where a cut can begin.
//
// The start snaps back to the keyframe at or before it. Frames between two
// keyframes are coded against their neighbours, so a cut that began between
// them would have to re-encode everything up to the next one, and re-encoding
// is a non-goal. Trim returns the time the output actually begins at, on the
// source's timeline, and that is the number to label the result with. A
// negative start reads as zero, a start past the end of the file takes the last
// keyframe, and a file whose first keyframe is not its first frame begins at
// that first keyframe rather than failing: the nearest range that decodes beats
// no range at all.
//
// The end has no such constraint and takes the last sample at or before it. An
// end past the end of the file keeps the rest of it, and an end at or before
// the start keeps the single sample the start snapped to, so a cut always holds
// a frame.
//
// Every track is cut against the same window, and each lands on its own sample
// boundaries. An audio frame is not a video frame: at 48 kHz an AAC frame is
// about 21 milliseconds, so an audio track may carry up to one frame of sound
// from before the video's first frame, and up to one after its last. Trim keeps
// the frame that straddles the start rather than the one after it, since a
// track that began late would be silent where the video is not, and that frame
// of lead is the residual error against the video: closing it exactly would
// mean re-encoding the audio. A track that declares keyframes of its own, which
// audio rarely does, snaps back to one the way the video track does. A file
// with no audio, or with several audio tracks, is cut the same way; a timecode
// or subtitle track is dropped, as it is by Remux.
//
// # Trim timestamps
//
// The output's timestamps are rewritten, not shifted with an edit list, and the
// source's edit list is dropped rather than carried across.
//
// The sample tables make this nearly free. stts holds durations rather than
// absolute times, so cutting it at the first kept sample leaves that sample
// decoding at zero with no arithmetic at all. A slice of a run table is still a
// run table, which keeps the writer's shape: it copies tables rather than
// rebuilding them, and memory stays flat whatever the length of the cut.
//
// ctts takes one adjustment. Its offsets say when a sample is displayed
// relative to when it is decoded, and a video track's first frame usually
// carries a couple of frames of encoder delay, which the source's edit list was
// there to hide. Dropping the list without touching the offsets would show the
// first frame that late while an audio track, which carries no ctts at all,
// started at zero, so the cut would lead its audio by the difference. Instead
// the span's earliest composition time is subtracted from every offset, which
// puts the first displayed frame at zero and leaves the timing between samples
// exactly as it was. Where reordering then drives an offset below zero the
// table is written in its signed form, which is what version 1 of the box is
// for.
//
// The tradeoff behind rewriting rather than writing an edit list is worth
// stating. An edit list preserves the source's timing exactly and lets a player
// trim the pre-roll itself; rewriting survives players that ignore edit lists,
// which many do. Rewriting wins here because the pre-roll question does not
// arise: the output already starts on a keyframe, so there is nothing before
// its first frame to trim away, and the normalized offsets carry the rest.
//
// What is left is the sample grid. An audio track's first frame is the one the
// cut landed on rather than the instant it asked for, so its sound sits up to
// one frame, about 21 milliseconds for AAC at 48 kHz, from where the source had
// it. Closing that would mean re-encoding. Probe reports the output's own
// duration, which is the samples it kept rather than the window that was asked
// for.
//
// A fragmented source is not trimmed, for the reason Remux is not: its samples
// are described by its moof boxes rather than by the movie header.
//
// # Remux
//
// Remux moves the streams of an MP4-family file into another container of the
// same family: mp4, m4v, or 3gp. Nothing is decoded and nothing is re-encoded.
// The sample payload is copied verbatim as byte ranges, and the sample
// descriptions, edit lists, and display matrices are copied verbatim too, so
// the output probes and thumbnails to what the input did. Only video and audio
// tracks come across; a timecode or subtitle track is dropped.
//
// The output is header-first. The source's sample tables already give every
// sample's size, so the output's chunk offsets are known before a byte of
// payload is written, and the ftyp, the moov, and then the mdat go out in that
// order. Probing the result therefore costs a read of its head, and a player
// can start on it before it has the whole file.
//
// Each track's samples are written as one chunk, one track after another,
// rather than interleaved by time. That is valid and it keeps the writer
// honest about memory, but a player streaming a long file has to seek between
// the video and the audio. Interleaving is the upgrade, and it changes nothing
// a caller can see.
//
// A fragmented source is not remuxed. Its samples are described by its moof
// boxes rather than by the movie header, and the writer builds its output from
// the movie header, so a fragmented input returns ErrUnsupportedContainer.
//
// # Remux compatibility
//
// Not every stream fits every container: a MOV carrying ProRes video or PCM
// audio has no valid representation in a normal MP4. The table below is the
// whole answer, and it is an allowlist, so a codec that does not appear is
// refused everywhere. CanRemux and Remux read the same table, so the answer a
// caller is given ahead of time and the answer a remux acts on cannot drift.
//
//	codec   containers it may be written into
//	h264    mp4, m4v, 3gp
//	hevc    mp4, m4v, 3gp
//	av1     mp4
//	vp9     mp4
//	aac     mp4, m4v, 3gp
//	mp3     mp4, m4v
//	opus    mp4
//	alac    mp4, m4v
//	ac-3    mp4, m4v
//	ec-3    mp4, m4v
//	fLaC    mp4
//	samr    3gp
//	sawb    3gp
//
// alac, ac-3, and ec-3 are on the list because all three are registered for
// the MP4 family: alac through Apple's own registration, and the two Dolby
// formats through ETSI TS 102 366 Annex F.
//
// What the table refuses, and why it is worth naming, is what a MOV can carry
// and an MP4 cannot: ProRes, stored as apch, apcn, apcs, apco, or ap4h, and
// uncompressed audio, stored as sowt, twos, lpcm, in24, in32, fl32, fl64, raw,
// NONE, ulaw, or alaw. Those arrive here under their four-character codes, as
// "Codec names" describes, and none of them is on the list. Remuxing one
// returns ErrIncompatible naming the codec and the container that refused it,
// and CanRemux reports false for the same pair.
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
