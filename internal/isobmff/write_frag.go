package isobmff

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"time"
)

// fragMovieTimescale is the movie timescale a fragmented output declares. Every
// duration in its mvhd and tkhd boxes is on it, and a millisecond tick is fine
// enough for a movie header and coarse enough that nothing overflows.
const fragMovieTimescale = 1000

// maxFragmentSamples bounds what one fragment holds, and so what this writer
// holds at once: one descriptor per sample, no payload. A source that never
// signals a boundary is broken into fragments of this many samples rather than
// buffering the whole file.
//
// ponytail: a flat ceiling rather than a byte or time budget. A caller that
// wants fragments of a given length signals the boundaries itself, which is
// what the Matroska driver does with its source's clusters.
const maxFragmentSamples = 1 << 13

// fragmentBrand is the compatible brand a fragmented file carries on top of its
// target's own. It is what ffmpeg writes for -movflags frag_keyframe+empty_moov,
// and it says the file may describe its samples with movie fragments rather
// than with the movie header's tables.
const fragmentBrand = "iso6"

// FragTrack is one track of a fragmented output: what it carries and how to
// describe it, with no samples in it. It is the shape both a Matroska source
// and, later, an MPEG-TS one can fill in, since neither has a sample table to
// hand over.
type FragTrack struct {
	// ID is the track_ID the output gives it, which every sample names. It must
	// be nonzero and unique across the tracks of one file.
	ID uint32
	// Handler is "vide" or "soun".
	Handler string
	// Timescale is the track's media timescale in ticks per second. Every
	// sample time and duration of this track is on it.
	Timescale uint32
	// Codec is the stable short name, as SampleEntry.Codec documents. It
	// decides the sample entry's four-character code.
	Codec string
	// Config is the codec configuration as the ISOBMFF family stores it: the
	// avcC, hvcC, or av1C record verbatim, the raw AudioSpecificConfig for aac,
	// or the Opus identification header for opus. It is nil for a codec that
	// needs none.
	Config []byte
	// Width and Height are the stored dimensions, for a video track.
	Width, Height uint32
	// Channels and SampleRate describe an audio track.
	Channels   uint16
	SampleRate uint32
	// EditDelay is how far the track's media timeline runs ahead of the
	// movie's, in media ticks, written as a one-entry edit list. A stream whose
	// frames are decoded in a different order from the one they are shown in
	// has to be decoded ahead of time, and an ISOBMFF decode time cannot be
	// negative, so the presentation is pushed forward instead and the edit list
	// takes it back off. It is zero for a track that needs no such lead.
	EditDelay uint64
}

// FragSample is one sample of the stream WriteFragmented is driven by. It holds
// no bytes: the payload stays in the source reader until the writer copies it.
type FragSample struct {
	// Track is the FragTrack.ID this sample belongs to.
	Track uint32
	// Decode is the sample's decode time on its track's media timeline. It is
	// what the fragment's tfdt states for the first sample of each track.
	Decode uint64
	// Duration is how long the sample occupies the decode timeline, in media
	// ticks.
	Duration uint32
	// Composition is when the sample is shown relative to when it is decoded,
	// in media ticks. It is usually zero or positive; a negative value makes
	// the fragment's trun the signed version of the box.
	Composition int32
	// Sync reports whether the sample decodes on its own.
	Sync bool
	// Fragment marks the first sample of a new fragment. The writer starts a
	// moof here, so a caller signals its own natural boundaries: a source
	// cluster, a segment, or a time budget.
	Fragment bool
	// Offset and Size locate the sample's bytes in the reader passed to
	// WriteFragmented.
	Offset, Size int64
}

// NextFragSample yields the samples of a fragmented output in decode order,
// interleaved across tracks however the source stores them, and reports ok
// false once the stream is spent. It is a function rather than an interface
// because the two producers, a Matroska demuxer today and an MPEG-TS one later,
// share nothing but this call.
type NextFragSample func() (FragSample, bool, error)

// Sample flags as a trun stores them. ISO/IEC 14496-12 8.8.3.1: a sync sample
// depends on nothing, and anything else depends on something and is not a sync
// sample.
const (
	syncSampleFlags    = 0x02000000
	nonSyncSampleFlags = 0x01000000 | sampleNonSync
)

// WriteFragmented writes a stream of samples into w as a fragmented file of the
// target container: an ftyp, then a moov whose sample tables are empty and
// whose mvex declares one trex per track, then a moof and an mdat per fragment.
// Nothing is re-encoded and no sample is looked at.
//
// It exists because a source with no sample table cannot be written any other
// way in one pass. A Matroska file states its frames cluster by cluster and
// nothing up front says how many there are or how large each one is, so a
// header-first plain MP4 would mean buffering every sample's size and offset,
// unbounded in the length of the input, or reading the source twice. A
// fragmented file needs neither: each fragment describes its own samples, and
// the cost of building one is the descriptors of the samples in it.
//
// r is where the payload comes from, and next yields the samples in decode
// order. duration is how long the movie runs, which goes in the mvhd and every
// tkhd so that probing the output reports it; the mdhd durations are left at
// zero, which is what a fragmented file declares and what makes a reader add
// the fragments up instead.
//
// A fragment ends where the next sample says it begins, and at
// maxFragmentSamples whatever the source says. Each one is built in memory,
// which is bounded by that, and its payload is then copied through one buffer
// this function owns, so a multi-gigabyte source costs the same heap as a small
// one.
//
// A codec the target cannot hold returns ErrIncompatibleCodec, a codec with no
// sample entry here returns ErrUnsupportedSource, a target this package does
// not write returns ErrUnsupportedTarget, and a sample that declares a
// negative or four-gigabyte size returns ErrMalformed.
func WriteFragmented(w io.Writer, r io.ReaderAt, target Target, duration time.Duration, tracks []FragTrack, next NextFragSample) error {
	brands, ok := targetBrands[target]
	if !ok {
		return fmt.Errorf("%w: %q is not a container this writer produces", ErrUnsupportedTarget, target)
	}
	if len(tracks) == 0 {
		return fmt.Errorf("%w: it carries no video or audio track", ErrUnsupportedSource)
	}
	for _, t := range tracks {
		if !CodecFits(t.Codec, target) {
			return fmt.Errorf("%w: track %d carries %s, which does not fit in %s",
				ErrIncompatibleCodec, t.ID, t.Codec, target)
		}
	}

	moov, err := fragMoov(tracks, duration)
	if err != nil {
		return err
	}
	// A copy, because the brand list behind the table is shared and appending
	// in place would leave the next writer with this file's brands.
	brands.compatible = append(slices.Clone(brands.compatible), fragmentBrand)
	if err := writeAll(w, ftypBox(brands), moov); err != nil {
		return err
	}

	var (
		buf      = make([]byte, copyBufferSize)
		samples  = make([]FragSample, 0, 64)
		pending  FragSample
		held     bool
		fragment fragmentWriter
		sequence uint32
	)
	for {
		samples = samples[:0]
		if held {
			samples, held = append(samples, pending), false
		}
		for len(samples) < maxFragmentSamples {
			s, ok, err := next()
			if err != nil {
				return err
			}
			if !ok {
				break
			}
			if s.Size < 0 || s.Size > math.MaxUint32 {
				return fmt.Errorf("%w: track %d offers a sample of %d bytes", ErrMalformed, s.Track, s.Size)
			}
			if s.Fragment && len(samples) > 0 {
				pending, held = s, true
				break
			}
			samples = append(samples, s)
		}
		if len(samples) == 0 {
			return nil
		}
		sequence++
		if err := fragment.write(w, r, tracks, samples, sequence, buf); err != nil {
			return err
		}
	}
}

// fragmentWriter holds the buffers one fragment is built in, reused across
// fragments so that a long file allocates what a single fragment costs.
type fragmentWriter struct {
	box     boxWriter
	offsets []int    // the position in the moof of each traf's trun data offset
	order   []uint32 // the track each of those belongs to, in moof order
	bytes   []int64  // the payload each of those tracks contributes
}

// write builds one moof, patches its data offsets now that its size is known,
// and streams the fragment's payload into the mdat behind it.
//
// The offsets are why the moof is built in memory: a trun states where its
// samples begin relative to the moof, so the moof has to be finished before
// the first byte of payload can be placed. One fragment's descriptors is the
// whole of what that costs.
func (f *fragmentWriter) write(w io.Writer, r io.ReaderAt, tracks []FragTrack, samples []FragSample, sequence uint32, buf []byte) error {
	f.box.buf = f.box.buf[:0]
	f.offsets, f.order, f.bytes = f.offsets[:0], f.order[:0], f.bytes[:0]

	b := &f.box
	start := b.open("moof")
	mfhd := b.full("mfhd", 0, 0)
	b.u32(sequence)
	b.close(mfhd)
	for _, t := range tracks {
		at, payload, ok := b.traf(t, samples)
		if !ok {
			continue
		}
		f.offsets = append(f.offsets, at)
		f.order = append(f.order, t.ID)
		f.bytes = append(f.bytes, payload)
	}
	b.close(start)

	var total int64
	for _, n := range f.bytes {
		total += n
	}
	at := int64(len(b.buf)) + mdatHeaderSize
	for i, offset := range f.offsets {
		// A trun's data offset is a signed 32-bit field, so a fragment can
		// place at most 2 GiB of payload ahead of its last track's run.
		//
		// ponytail: refused rather than split. Breaking such a fragment into
		// several is the upgrade, and no real cluster comes near the limit.
		if at > math.MaxInt32 {
			return fmt.Errorf("%w: a fragment of over %d bytes, beyond what a trun data offset can reach",
				ErrUnsupportedSource, int64(math.MaxInt32))
		}
		binary.BigEndian.PutUint32(b.buf[offset:], uint32(at))
		at += f.bytes[i]
	}

	if err := writeAll(w, b.buf, mdatHeader(total)); err != nil {
		return err
	}
	for _, id := range f.order {
		for _, s := range samples {
			if s.Track != id {
				continue
			}
			copied, err := io.CopyBuffer(w, io.NewSectionReader(r, s.Offset, s.Size), buf)
			if err != nil {
				return fmt.Errorf("copying %d bytes of payload at %d: %w", s.Size, s.Offset, err)
			}
			if copied != s.Size {
				return fmt.Errorf("%w: copied %d of the %d bytes at %d", ErrTruncated, copied, s.Size, s.Offset)
			}
		}
	}
	return nil
}

// traf writes one track's fragment: the header, the base decode time, and the
// run of samples. It reports the position of the trun's data offset field,
// which only the finished moof can fill in, and how many bytes of payload the
// run accounts for. A track with no sample in this fragment gets no traf.
// ISO/IEC 14496-12 8.8.6.
func (b *boxWriter) traf(t FragTrack, samples []FragSample) (offsetAt int, payload int64, ok bool) {
	var (
		count  uint32
		base   uint64
		first  = true
		shifts bool
		signed bool
	)
	for _, s := range samples {
		if s.Track != t.ID {
			continue
		}
		if first {
			base, first = s.Decode, false
		}
		count++
		payload += s.Size
		shifts = shifts || s.Composition != 0
		signed = signed || s.Composition < 0
	}
	if count == 0 {
		return 0, 0, false
	}

	start := b.open("traf")
	// The samples live in the mdat behind this moof, which is what
	// default-base-is-moof says and what every writer in practice means.
	// ISO/IEC 14496-12 8.8.7.
	tfhd := b.full("tfhd", 0, tfhdDefaultBaseIsMoof)
	b.u32(t.ID)
	b.close(tfhd)
	// The 64-bit form unconditionally: a long file's decode time overflows 32
	// bits on a fine timescale, and four bytes a fragment is not a cost.
	tfdt := b.full("tfdt", 1, 0)
	b.u64(base)
	b.close(tfdt)

	flags := uint32(trunDataOffset | trunSampleDuration | trunSampleSize | trunSampleFlags)
	var version uint8
	if shifts {
		flags |= trunSampleCompOffset
		// A negative offset is only expressible in version 1, the same signed
		// form the trim path writes its ctts in.
		if signed {
			version = 1
		}
	}
	trun := b.full("trun", version, flags)
	b.u32(count)
	offsetAt = len(b.buf)
	b.u32(0) // data offset, patched once the moof size is known
	for _, s := range samples {
		if s.Track != t.ID {
			continue
		}
		b.u32(s.Duration)
		b.u32(uint32(s.Size))
		if s.Sync {
			b.u32(syncSampleFlags)
		} else {
			b.u32(nonSyncSampleFlags)
		}
		if shifts {
			b.u32(uint32(s.Composition))
		}
	}
	b.close(trun)
	b.close(start)
	return offsetAt, payload, true
}

// fragMoov builds the movie header of a fragmented file: the same tracks a
// plain file declares, with empty sample tables, plus the mvex that says the
// samples are described by the fragments behind it.
func fragMoov(tracks []FragTrack, duration time.Duration) ([]byte, error) {
	movie := durationToTicks(duration, fragMovieTimescale)

	var b boxWriter
	start := b.open("moov")
	b.fragMvhd(tracks, movie)
	for _, t := range tracks {
		if err := b.fragTrak(t, movie); err != nil {
			return nil, err
		}
	}
	mvex := b.open("mvex")
	for _, t := range tracks {
		// Every default is zero, because every trun states its samples in full.
		// ISO/IEC 14496-12 8.8.3.
		trex := b.full("trex", 0, 0)
		b.u32(t.ID)
		b.u32(1) // default sample description index
		b.u32(0) // default sample duration
		b.u32(0) // default sample size
		b.u32(0) // default sample flags
		b.close(trex)
	}
	b.close(mvex)
	b.close(start)
	return b.buf, nil
}

// fragMvhd writes the movie header. ISO/IEC 14496-12 8.2.2.
func (b *boxWriter) fragMvhd(tracks []FragTrack, duration uint64) {
	start := b.full("mvhd", 1, 0)
	b.u64(0) // creation time
	b.u64(0) // modification time
	b.u32(fragMovieTimescale)
	b.u64(duration)
	b.u32(one16)  // rate, 1.0
	b.u16(0x0100) // volume, full
	b.u16(0)      // reserved
	b.zeros(8)    // reserved
	for _, v := range matrixIdentity {
		b.u32(uint32(v))
	}
	b.zeros(24) // pre_defined
	var next uint32
	for _, t := range tracks {
		next = max(next, t.ID+1)
	}
	b.u32(next)
	b.close(start)
}

// fragTrak writes one track: its header, the edit list that hides its decode
// lead where it has one, and a media box whose sample tables are empty.
func (b *boxWriter) fragTrak(t FragTrack, duration uint64) error {
	// The track is enabled and part of the movie.
	const enabledInMovie = 0x000003

	start := b.open("trak")
	tkhd := b.full("tkhd", 1, enabledInMovie)
	b.u64(0) // creation time
	b.u64(0) // modification time
	b.u32(t.ID)
	b.u32(0) // reserved
	b.u64(duration)
	b.zeros(8) // reserved
	b.u16(0)   // layer
	b.u16(0)   // alternate group
	if t.Handler == "soun" {
		b.u16(0x0100) // volume, full
	} else {
		b.u16(0)
	}
	b.u16(0) // reserved
	// Matroska carries no display matrix, and neither does MPEG-TS, so the
	// output is written unrotated. Rotation is lost writing into this family
	// from either of them, which the sprocket documentation states.
	for _, v := range matrixIdentity {
		b.u32(uint32(v))
	}
	b.u32(t.Width << 16)
	b.u32(t.Height << 16)
	b.close(tkhd)

	if t.EditDelay > 0 {
		b.fragEdts(t, duration)
	}
	if err := b.fragMdia(t); err != nil {
		return err
	}
	b.close(start)
	return nil
}

// fragEdts writes the one-entry edit list that takes a track's decode lead back
// off its presentation. ISO/IEC 14496-12 8.6.6.
func (b *boxWriter) fragEdts(t FragTrack, duration uint64) {
	edts := b.open("edts")
	elst := b.full("elst", 1, 0)
	b.u32(1) // entry count
	b.u64(duration)
	b.u64(t.EditDelay)
	b.u32(one16) // rate, 1.0
	b.close(elst)
	b.close(edts)
}

// fragMdia writes the media box: the media header, the handler, and a sample
// table with nothing in it.
func (b *boxWriter) fragMdia(t FragTrack) error {
	start := b.open("mdia")

	// A media duration of zero is what a fragmented file declares: the samples
	// are in the fragments, so a reader adds those up rather than trusting a
	// number written before any of them existed. The movie header does carry
	// the duration, because the source states it up front.
	mdhd := b.full("mdhd", 1, 0)
	b.u64(0) // creation time
	b.u64(0) // modification time
	b.u32(t.Timescale)
	b.u64(0) // duration
	b.u16(undLanguage)
	b.u16(0) // pre_defined
	b.close(mdhd)

	b.hdlr(t.Handler)

	minf := b.open("minf")
	b.mediaHeader(t.Handler)
	b.dinf()
	if err := b.fragStbl(t); err != nil {
		return err
	}
	b.close(minf)

	b.close(start)
	return nil
}

// fragStbl writes the sample table of a fragmented track: the sample
// description, and then the four tables a reader expects to find, all empty.
func (b *boxWriter) fragStbl(t FragTrack) error {
	start := b.open("stbl")

	stsd := b.full("stsd", 0, 0)
	b.u32(1) // entry count
	if err := b.sampleEntry(t); err != nil {
		return err
	}
	b.close(stsd)

	for _, typ := range [...]string{"stts", "stsc", "stsz", "stco"} {
		empty := b.full(typ, 0, 0)
		if typ == "stsz" {
			b.u32(0) // sample size, not constant
		}
		b.u32(0) // entry count
		b.close(empty)
	}

	b.close(start)
	return nil
}

// entryFormats maps a codec short name onto the four-character sample entry
// code a fragmented output describes it with. A codec that is not here has no
// sample entry this writer can build, whatever the compatibility table allows,
// which is the same shape the Matroska writer's codec identifier table has.
var entryFormats = map[string]string{
	"h264": "avc1",
	"hevc": "hvc1",
	"av1":  "av01",
	"vp8":  "vp08",
	"vp9":  "vp09",
	"aac":  "mp4a",
	"opus": "Opus",
}

// sampleEntry writes one stsd entry: the fixed fields the handler decides, then
// the codec configuration box. ISO/IEC 14496-12 12.1.3 and 12.2.3.
func (b *boxWriter) sampleEntry(t FragTrack) error {
	format, ok := entryFormats[t.Codec]
	if !ok {
		return fmt.Errorf("%w: track %d carries %s, which has no sample entry here",
			ErrUnsupportedSource, t.ID, t.Codec)
	}

	start := b.open(format)
	b.zeros(6) // reserved
	b.u16(1)   // data reference index
	if t.Handler == "soun" {
		b.zeros(8) // version, revision, and vendor, all QuickTime leftovers
		b.u16(max(t.Channels, 1))
		b.u16(16) // sample size in bits
		b.u16(0)  // pre_defined
		b.u16(0)  // reserved
		b.u32(t.SampleRate << 16)
	} else {
		b.zeros(16) // pre_defined and reserved
		b.u16(uint16(min(t.Width, math.MaxUint16)))
		b.u16(uint16(min(t.Height, math.MaxUint16)))
		b.u32(0x00480000) // horizontal resolution, 72 dpi
		b.u32(0x00480000) // vertical resolution, 72 dpi
		b.u32(0)          // reserved
		b.u16(1)          // frame count, one picture per sample
		b.zeros(32)       // compressor name, left empty
		b.u16(0x0018)     // depth, colour with no alpha
		b.u16(0xffff)     // pre_defined, -1
	}
	if err := b.codecConfig(t); err != nil {
		return err
	}
	b.close(start)
	return nil
}

// codecConfig writes the configuration box a sample entry carries. Three of the
// video codecs store the same record the source already holds, so it goes in
// verbatim; the other three are built here from the form the source keeps them
// in, which is the inverse of what the Matroska writer does on the way out.
func (b *boxWriter) codecConfig(t FragTrack) error {
	record := func(typ string) error {
		if len(t.Config) == 0 {
			return fmt.Errorf("%w: track %d carries %s with no configuration record",
				ErrUnsupportedSource, t.ID, t.Codec)
		}
		start := b.open(typ)
		b.raw(t.Config)
		b.close(start)
		return nil
	}

	switch t.Codec {
	case "h264":
		return record("avcC")
	case "hevc":
		return record("hvcC")
	case "av1":
		return record("av1C")
	case "vp8", "vp9":
		b.vpcC()
		return nil
	case "aac":
		return b.esds(t)
	case "opus":
		return b.dOps(t)
	}
	return fmt.Errorf("%w: track %d carries %s, which has no configuration record here",
		ErrUnsupportedSource, t.ID, t.Codec)
}

// vpcC writes a VP codec configuration record. Matroska stores none for VP8 or
// VP9, and nothing in this library reads one back, so what goes out is the
// minimum the box has to hold: 8-bit 4:2:0 with the colour description left
// unspecified, which is what a decoder falls back to anyway.
// ISO/IEC 14496-15 and the VP Codec ISO Media File Format Binding.
//
// ponytail: a fixed record rather than one derived from the bitstream. Reading
// the profile and bit depth out of a VP9 uncompressed header is the upgrade,
// and it wants a keyframe parse this writer deliberately does not do.
func (b *boxWriter) vpcC() {
	const (
		// bitDepth 8 in the high nibble, chroma subsampling 4:2:0 colocated in
		// the next three bits, and the full range flag clear.
		depthAndChroma = 8<<4 | 1<<1
		// 2 is "unspecified" for all three colour fields. ISO/IEC 23091-2.
		unspecified = 2
	)
	start := b.full("vpcC", 1, 0)
	b.raw([]byte{0, 0, depthAndChroma, unspecified, unspecified, unspecified})
	b.u16(0) // codec initialization data size
	b.close(start)
}

// Descriptor field values used by the esds this writer builds.
// ISO/IEC 14496-1 7.2.6.6 and ISO/IEC 14496-3.
const (
	// objectTypeAAC is the objectTypeIndication for MPEG-4 audio.
	objectTypeAAC = 0x40
	// streamTypeAudio is the stream type 0x05 in its top six bits, with
	// upStream clear and the reserved bit set.
	streamTypeAudio = 0x15
	// slConfigMP4 is the predefined SL packet header configuration for MP4.
	slConfigMP4 = 0x02
	tagSLConfig = 0x06
)

// esds writes the elementary stream descriptor an mp4a entry carries. Matroska
// stores an AAC track's AudioSpecificConfig raw, which is exactly the decoder
// specific information this wraps it back into.
func (b *boxWriter) esds(t FragTrack) error {
	if len(t.Config) == 0 {
		return fmt.Errorf("%w: track %d carries AAC with no AudioSpecificConfig",
			ErrUnsupportedSource, t.ID)
	}

	config := descriptor(tagDecoderSpecific, t.Config)
	decoder := descriptor(tagDecoderConfig, concatBytes(
		[]byte{objectTypeAAC, streamTypeAudio, 0, 0, 0}, // object type, stream type, buffer size
		make([]byte, 8), // maximum and average bitrate, both unstated
		config,
	))
	stream := descriptor(tagESDescr, concatBytes(
		[]byte{byte(t.ID >> 8), byte(t.ID), 0}, // ES_ID and flags
		decoder,
		descriptor(tagSLConfig, []byte{slConfigMP4}),
	))

	start := b.full("esds", 0, 0)
	b.raw(stream)
	b.close(start)
	return nil
}

// dOps writes the Opus specific box, which holds the identification header's
// fields with the magic dropped and the multi-byte ones big-endian rather than
// little. It is the inverse of what the Matroska writer builds on the way out.
// RFC 7845 section 5.1.
func (b *boxWriter) dOps(t FragTrack) error {
	// "OpusHead", a version byte, the channel count, a 16 bit pre-skip, a 32
	// bit input sample rate, a 16 bit output gain, and the mapping family.
	const opusHeadSize = 19
	head := t.Config
	if len(head) < opusHeadSize || string(head[:8]) != "OpusHead" {
		return fmt.Errorf("%w: track %d carries Opus with a %d byte identification header, need at least %d",
			ErrUnsupportedSource, t.ID, len(head), opusHeadSize)
	}

	start := b.open("dOps")
	b.raw([]byte{0, head[9]}) // version, output channel count
	b.u16(binary.LittleEndian.Uint16(head[10:]))
	b.u32(binary.LittleEndian.Uint32(head[12:]))
	b.u16(binary.LittleEndian.Uint16(head[16:]))
	b.raw(head[18:19]) // channel mapping family
	// A mapping family other than zero is followed by the channel mapping
	// table, which is the same bytes in both records.
	b.raw(head[opusHeadSize:])
	b.close(start)
	return nil
}

// descriptor wraps a body in one tag-length-value descriptor. The length is
// base-128, seven bits per byte, high bit set to continue, which is what
// readDescriptor reads back.
func descriptor(tag byte, body []byte) []byte {
	out := []byte{tag}
	size := len(body)
	for shift := 21; shift > 0; shift -= 7 {
		if size>>shift != 0 {
			out = append(out, byte((size>>shift)&0x7f)|0x80)
		}
	}
	out = append(out, byte(size&0x7f))
	return append(out, body...)
}

// concatBytes joins parts into one slice, which keeps the descriptor nesting
// above readable.
func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}
