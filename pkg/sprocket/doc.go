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
// 3gp, and 3g2.
//
// So are Matroska and WebM, in both directions. An MP4-family file goes into an
// mkv or a webm, and a Matroska or WebM file goes into an mp4, an m4v, or a 3gp,
// where the output is fragmented for the reason "Fragmented output" gives.
// MPEG-TS is not supported at all.
//
// Keyframe decoding covers HEVC, VP8, and AV1, and H.264 behind the h264 build
// tag. VP9 has no pure-Go decoder and returns ErrUnsupportedCodec. See
// https://github.com/autobutler-org/sprocket/issues/13.
//
// # Matroska and WebM
//
// One parser covers both, and so does the writer: they differ by the document
// type in their header and by what codecs they are allowed to carry. Three
// things about the family are worth knowing before reading a result from one.
//
// Rotation is always 0. Matroska has no display matrix, so there is nothing for
// the container to say about orientation and this library does not guess. The
// Projection element can express a roll, but it belongs to the spherical video
// signaling rather than to the orientation flag a phone writes, and nothing
// outside 360 degree video sets it, so it is not read.
//
// Frame rate comes from what the file declares where it declares anything.
// Matroska has no sample table, so there is no count of frames to divide by the
// duration without reading the whole file. A track that states a
// DefaultDuration, which every muxer writes, reports the rate that implies,
// which is nominal rather than measured: a variable-framerate file is described
// by whatever its muxer nominated. A track that states none is measured by
// counting the blocks across the file, which costs a walk of the cluster
// headers when the file is opened and is bounded to about four hours of
// one-second clusters.
//
// Duration is what the file declares and nothing else. The ISOBMFF side falls
// back to its longest track where the movie header says zero; there is no
// equivalent here short of reading to the last block, so a Matroska file that
// declares no duration reports zero, and with it a bitrate and a measured frame
// rate of zero.
//
// Writing one is single-pass. The segment declares an unknown size and the seek
// index goes at the end, both of which the format allows, so the header can go
// out before anything after it is known. Each cluster does declare its size,
// which costs holding one cluster's block descriptors, a few thousand at most
// and no payload, while it is measured. Clusters run about a second and begin
// on a keyframe where there is one to begin on.
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
// chroma planes in the stream's own range and matrix. The matrix and the range
// the stream signals are used when it signals them: BT.601, BT.709, and
// BT.2020, in studio or full range. Video that signals no matrix is read as
// BT.601 studio range, luma 16 to 235 and chroma 16 to 240, at every size,
// which is what ffmpeg does, so a thumbnail matches what every ffmpeg-based
// tool renders. Measured against ffmpeg's render of the same keyframe, at
// 128x72 and at 3840x2160, signaled and unsignaled, HEVC and AV1, the mean
// absolute error is under 0.5 of 255 on every channel. H.264 is always read as
// unsignaled, since its decoder does not report what the stream signals. The
// primaries and the transfer are not applied, so HDR video is not tone mapped.
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
// Trim cuts a range out of a file and writes it as a file of any container
// Remux can write, copying the samples across as byte ranges. Nothing is
// decoded and nothing is re-encoded, which is what fixes where a cut can
// begin.
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
// A Matroska source is cut the same way, against a window of timestamps rather
// than a range of sample numbers, since a file with no sample table has no
// sample numbers to name. The start snaps back to the keyframe the Cues index
// or a cluster walk finds at or before it, every other track begins at its own
// last block at or before that, and each track is rebased onto its own first
// block, so every track begins at zero with the same one-frame residual the
// MP4 family has. Writing one into the MP4 family produces a fragmented file,
// exactly as a remux does.
//
// A fragmented source is not trimmed, for the reason Remux does not remux one:
// its samples are described by its moof boxes rather than by the movie header.
//
// # Remux
//
// Remux moves the streams of a file into another container: mp4, m4v, 3gp,
// mkv, or webm, from a source of either family. Nothing is decoded and nothing
// is re-encoded. The sample payload is copied verbatim as byte ranges, and the
// codec configuration is carried across, so the output probes and thumbnails
// to what the input did. Only video and audio tracks come across; a timecode or
// subtitle track is dropped.
//
// The output is header-first whichever pairing it is, so probing the result
// costs a read of its head and a player can start on it before it has the whole
// file. From an MP4-family source the sample tables already give every sample's
// size, so the chunk offsets are known before a byte of payload is written and
// the ftyp, the moov, and then the mdat go out in that order. From a Matroska
// source there is no such table, and "Fragmented output" is how the header
// still goes first.
//
// Each track's samples are written as one chunk, one track after another,
// rather than interleaved by time. That is valid and it keeps the writer
// honest about memory, but a player streaming a long file has to seek between
// the video and the audio. Interleaving is the upgrade, and it changes nothing
// a caller can see.
//
// An MP4-family source into a Matroska output takes the same route: the samples are
// copied as byte ranges out of the source and the codec configuration records
// are carried across, either verbatim, since Matroska adopted the same records
// the MP4 family uses, or converted where the two families store the same
// fields differently. AAC is one of those: Matroska stores the raw
// AudioSpecificConfig, which lives inside the source's esds. Opus is the other:
// Matroska stores the identification header whose body a dOps box holds. Going
// the other way runs the same conversions backwards, so an AAC track gets its
// esds rebuilt and an Opus track its dOps. VP8 and VP9 are the one loss:
// Matroska carries no vpcC, so the output gets the minimum the box has to hold,
// 8-bit 4:2:0 with the colour description left unspecified.
//
// Rotation does not survive a Matroska file in either direction. The format has
// no display matrix, so there is nothing to carry into or out of a tkhd, and an
// MP4 written from one is written unrotated.
//
// A Matroska or WebM source into another Matroska container is a copy of the
// source's own clusters: the blocks move across as bytes, keyframe flags and
// lacing included, and the cluster boundaries are the ones the source chose.
// Into the MP4 family it is the fragmented writer, described next.
//
// A fragmented source is not remuxed. Its samples are described by its moof
// boxes rather than by the movie header, and the writer builds its output from
// the movie header, so a fragmented input returns ErrUnsupportedContainer.
//
// # Fragmented output
//
// An MP4 written from a Matroska or WebM source is a fragmented one: an ftyp,
// then a moov whose sample tables are empty and whose mvex declares the tracks,
// then a moof and an mdat for each of the source's clusters. The ftyp carries
// the iso6 brand alongside the target's own, which is what says the file may
// describe its samples that way.
//
// It is fragmented because Matroska has no up-front index. A cluster carries
// its blocks and each block its own timestamp, and nothing before the media
// says how many frames a track holds or how large each one is, so a writer
// cannot put a moov in front of the payload without reading the whole file
// first. Writing the index at the end would keep the output a plain MP4 at the
// cost of buffering every sample's size and offset, unbounded in the length of
// the input, or of reading the source twice. A fragmented file needs neither:
// each fragment describes its own samples, and building one costs a cluster's
// block headers, which is the bound the Matroska writer already works to. It is
// also progressively playable, and it is what every streaming pipeline emits.
// Nothing downstream has to change either, because the demuxer here already
// reads fragmented files: the output probes, thumbnails, and trims through code
// that was already there.
//
// Two numbers have to be derived on the way, because Matroska does not state
// them.
//
// A block's duration is the step to the next block on the same track, which
// costs one cluster of lookahead. The last block of the file falls back to the
// track's DefaultDuration, or to the previous block's duration where the track
// declares none.
//
// A block's decode time is the harder one. Matroska stores blocks in decode
// order and stamps each with the time it is shown; an MP4 wants a decode time
// and an offset from it to the presentation. Within a fragment the timestamps
// are sorted: the frames of a group are shown in some permutation of the same
// set of times, so the i-th block in storage order is decoded at the i-th
// smallest timestamp. A reordered stream then has to be decoded before its
// first frame is shown, which would want a decode time below zero, and an MP4
// cannot express one. The presentation is pushed forward by that lead instead
// and a one-entry edit list takes it back off, which is what every muxer does
// and what keeps Probe and Thumbnail reporting the times the source stated. The
// lead is measured over the first fragment each track appears in and rounded up
// to a whole frame; a later fragment that reorders more deeply drives a
// composition offset negative, and the signed form of the trun carries that.
// Signed offsets alone, with no edit list, would say the same thing more
// simply, but ffmpeg shifts a track with negative offsets later by the deepest
// one, so a file written that way plays late there.
//
// One thing does not come across. A laced block, which packs several frames
// into one to save a header each, has no equivalent in an MP4, where a sample
// is a frame. Rather than take the block apart, the writer returns
// ErrUnsupportedContainer naming the lacing. A Matroska output keeps such a
// block as it stands.
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
//	h264    mp4, m4v, 3gp, mkv
//	hevc    mp4, m4v, 3gp, mkv
//	av1     mp4, mkv, webm
//	vp8     mkv, webm
//	vp9     mp4, mkv, webm
//	aac     mp4, m4v, 3gp, mkv
//	mp3     mp4, m4v, mkv
//	opus    mp4, mkv, webm
//	vorbis  mkv, webm
//	alac    mp4, m4v, mkv
//	ac-3    mp4, m4v, mkv
//	ec-3    mp4, m4v, mkv
//	fLaC    mp4, mkv
//	samr    3gp
//	sawb    3gp
//
// alac, ac-3, and ec-3 are on the list because all three are registered for
// the MP4 family: alac through Apple's own registration, and the two Dolby
// formats through ETSI TS 102 366 Annex F.
//
// Matroska takes nearly everything, because its codec identifiers are an open
// registry rather than a fixed set of four-character codes. WebM is the strict
// one: the format allows VP8, VP9, and AV1 video with Vorbis or Opus audio and
// nothing else, so an H.264 file returns ErrIncompatible for it.
//
// The table speaks for the codecs and not for the source. A pairing it allows
// can still fail at Remux for a reason that is not about the codec: a
// fragmented source, a laced Matroska block, or a source whose codec
// configuration this library cannot translate into the target's own form. Those
// return ErrUnsupportedContainer naming what stopped them. The last of those is
// the narrowest gap today: an MP4 written from a Matroska source can describe
// h264, hevc, av1, vp8, vp9, aac, and opus, and the other rows of the mp4
// column have no sample entry built for them yet.
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
//	h264, hevc, av1, vp8, vp9, aac, mp3, opus, vorbis
//
// Anything else comes through as its four-character sample entry code exactly as
// the file stores it, case included: "ac-3", "ec-3", "fLaC", "alac". A Matroska
// file with a codec off the list reports its CodecID string instead, such as
// "V_QUICKTIME". Those are readable enough to log or to match on, but they are
// not yet promised to stay as they are; the nine names above are. The list grows as containers and
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
// Info.FrameRate is an average for an ISOBMFF file: the video track's sample
// count over its media duration, taken from the sample tables. A
// variable-framerate file therefore reports the rate it actually ran at rather
// than a nominal one, and a file whose nominal rate is a lie reports the truth.
// It is 0 when there is no video track, no video sample, or no duration.
//
// A Matroska or WebM file has no sample table, so the rule there is the one
// "Matroska and WebM" describes: the declared DefaultDuration where the track
// states one, and a measured count of blocks where it does not.
package sprocket
