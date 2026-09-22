package isobmff

import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"
)

// Target names a container this package can write. One writer covers all
// three: they differ by the brands in their ftyp and by what CodecTargets lets
// into them, not by structure.
type Target string

// The containers Write produces.
const (
	TargetMP4 Target = "mp4"
	TargetM4V Target = "m4v"
	Target3GP Target = "3gp"
)

// brandSet is one target's ftyp: the major brand that says what the file is,
// and the compatible brands that say what a reader may treat it as.
type brandSet struct {
	major      string
	compatible []string
}

// targetBrands is the ftyp each target gets. ISO/IEC 14496-12 4.3,
// ISO/IEC 14496-14 4 for mp4, and 3GPP TS 26.244 for 3gp.
var targetBrands = map[Target]brandSet{
	TargetMP4: {major: "isom", compatible: []string{"isom", "iso2", "avc1", "mp41"}},
	TargetM4V: {major: "M4V ", compatible: []string{"M4V ", "M4A ", "mp42", "isom"}},
	Target3GP: {major: "3gp4", compatible: []string{"3gp4", "isom", "mp41"}},
}

// CodecTargets is the codec and container compatibility table: which of this
// package's codec short names each target may carry. It is the whole answer,
// and it is deliberately an allowlist. A codec that is not listed cannot be
// written into any of these containers, which is what keeps the MOV-only
// codecs out of an MP4 without naming every one of them:
//
//   - ProRes, stored as apch, apcn, apcs, apco, or ap4h.
//   - Uncompressed and lightly compressed PCM, stored as sowt, twos, lpcm,
//     in24, in32, fl32, fl64, raw, NONE, ulaw, or alaw.
//   - Anything else a QuickTime file may carry that the MP4 registration
//     authority has no entry for.
//
// Both CanRemux and the muxer read this, so the answer a caller is given and
// the answer a remux acts on cannot drift apart.
//
// alac, ac-3, and ec-3 are on the list: all three are registered for the MP4
// family, alac through Apple's own registration and the two Dolby formats
// through ETSI TS 102 366 Annex F.
var CodecTargets = map[string][]Target{
	"h264": {TargetMP4, TargetM4V, Target3GP},
	"hevc": {TargetMP4, TargetM4V, Target3GP},
	"av1":  {TargetMP4},
	"vp9":  {TargetMP4},
	"aac":  {TargetMP4, TargetM4V, Target3GP},
	"mp3":  {TargetMP4, TargetM4V},
	"opus": {TargetMP4},
	"alac": {TargetMP4, TargetM4V},
	"ac-3": {TargetMP4, TargetM4V},
	"ec-3": {TargetMP4, TargetM4V},
	"fLaC": {TargetMP4},
	"samr": {Target3GP},
	"sawb": {Target3GP},
}

// CodecFits reports whether a codec short name may be written into a target.
// An empty codec is a track the file does not have, which blocks nothing, and
// a target this package does not write fits nothing.
func CodecFits(codec string, target Target) bool {
	if _, ok := targetBrands[target]; !ok {
		return false
	}
	if codec == "" {
		return true
	}
	return slices.Contains(CodecTargets[codec], target)
}

// copyBufferSize is the one buffer the payload copy uses, whatever the file's
// size. It is large enough that a chunk moves in a handful of reads and small
// enough that holding it costs nothing worth measuring.
const copyBufferSize = 256 << 10

// mdatHeaderSize is the header the payload box always gets: a declared size of
// 1, the type, and a 64-bit largesize. Writing the 64-bit form unconditionally
// costs eight bytes and removes a branch that only a file over four gigabytes
// would ever take, which is a branch no test can reach.
const mdatHeaderSize = largeBoxHeaderSize

// Write writes src into w as a file of the target container: an ftyp, then a
// moov holding every track's tables, then one mdat with the sample payload
// copied verbatim. Nothing is re-encoded and no sample is looked at.
//
// The header goes first. The source's own tables give every sample's size, so
// the output's chunk offsets are known before a byte of payload moves, and the
// result is progressively playable. Payload moves as byte ranges through one
// buffer this function owns, so a multi-gigabyte source costs the same as a
// small one.
//
// Only video and audio tracks are carried over; a timecode or subtitle track
// is dropped. A fragmented source, or one with no video or audio track,
// returns ErrUnsupportedSource. A codec the target cannot hold returns
// ErrIncompatibleCodec, and a target this package does not write returns
// ErrUnsupportedTarget.
//
// ponytail: every track's samples go into one chunk, laid out one track after
// another. That is valid and it is header-first, but it is not interleaved, so
// a player streaming a long file has to seek between the video and the audio.
// Interleaving by time is the upgrade: group samples into chunks of a second
// or so and emit a chunk per track in turn, which turns the single stsc and
// stco entry per track into one per chunk and nothing else.
//
// ponytail: Trim (#10) wants the same writer over a sample range per track
// rather than the whole track. It is not free here: stts, ctts, stss, and stsz
// are copied out of the source verbatim, and a range means slicing every one
// of them, splitting the run tables at the cut. Add the range when Trim lands.
func Write(w io.Writer, src *File, target Target) error {
	brands, ok := targetBrands[target]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedTarget, target)
	}
	if src.Fragmented {
		return fmt.Errorf("%w: the source is fragmented, and its samples are described by moof boxes rather than by the moov's tables", ErrUnsupportedSource)
	}

	tracks := make([]*Track, 0, len(src.Tracks))
	for _, t := range src.Tracks {
		if t.Handler != "vide" && t.Handler != "soun" {
			continue
		}
		if !CodecFits(t.Entry.Codec, target) {
			return fmt.Errorf("%w: track %d carries %s, which does not fit in %s",
				ErrIncompatibleCodec, t.ID, t.Entry.Codec, target)
		}
		tracks = append(tracks, t)
	}
	if len(tracks) == 0 {
		return fmt.Errorf("%w: it carries no video or audio track", ErrUnsupportedSource)
	}

	payload := make([]int64, len(tracks))
	var total int64
	for i, t := range tracks {
		n, err := t.tables.payloadBytes()
		if err != nil {
			return err
		}
		payload[i], total = n, total+n
	}

	ftyp := ftypBox(brands)
	moov, offsetAt := buildMoov(src, tracks)
	at := int64(len(ftyp)) + int64(len(moov)) + mdatHeaderSize
	for i := range tracks {
		if offsetAt[i] >= 0 {
			binary.BigEndian.PutUint64(moov[offsetAt[i]:], uint64(at))
		}
		at += payload[i]
	}

	if err := writeAll(w, ftyp, moov, mdatHeader(total)); err != nil {
		return err
	}

	buf := make([]byte, copyBufferSize)
	for _, t := range tracks {
		if err := copyPayload(w, src.r, &t.tables, buf); err != nil {
			return err
		}
	}
	return nil
}

// writeAll writes each part in turn, reporting the first failure.
func writeAll(w io.Writer, parts ...[]byte) error {
	for _, part := range parts {
		if _, err := w.Write(part); err != nil {
			return fmt.Errorf("writing the output: %w", err)
		}
	}
	return nil
}

// ftypBox builds the file type box for a target. ISO/IEC 14496-12 4.3.
func ftypBox(brands brandSet) []byte {
	var b boxWriter
	start := b.open("ftyp")
	b.raw(fourcc(brands.major))
	b.u32(0) // minor version
	for _, brand := range brands.compatible {
		b.raw(fourcc(brand))
	}
	b.close(start)
	return b.buf
}

// mdatHeader builds the payload box header for a given payload size.
func mdatHeader(payload int64) []byte {
	head := binary.BigEndian.AppendUint32(nil, 1)
	head = append(head, "mdat"...)
	return binary.BigEndian.AppendUint64(head, uint64(payload+mdatHeaderSize))
}

// copyPayload copies a track's samples into w in the order the source stores
// them. Chunks that sit back to back in the source join into one
// io.CopyBuffer, so a track the source already laid out contiguously moves in
// a single copy, and every copy shares the buffer the caller owns.
func copyPayload(w io.Writer, r io.ReaderAt, s *sampleTables, buf []byte) error {
	var run byteRange
	flush := func() error {
		if run.size == 0 {
			return nil
		}
		n, err := io.CopyBuffer(w, io.NewSectionReader(r, run.offset, run.size), buf)
		if err != nil {
			return fmt.Errorf("copying %d bytes of payload at %d: %w", run.size, run.offset, err)
		}
		if n != run.size {
			return fmt.Errorf("%w: copied %d of the %d bytes at %d", ErrTruncated, n, run.size, run.offset)
		}
		run = byteRange{}
		return nil
	}

	if err := s.eachRange(func(next byteRange) error {
		if run.size > 0 && run.offset+run.size == next.offset {
			run.size += next.size
			return nil
		}
		if err := flush(); err != nil {
			return err
		}
		run = next
		return nil
	}); err != nil {
		return err
	}
	return flush()
}

// buildMoov writes the movie header and every track. It returns the moov and,
// per track, the position in it of that track's chunk offset field, which only
// the finished layout can fill in. A track with no samples has no offset field
// and reports -1.
func buildMoov(src *File, tracks []*Track) (moov []byte, offsetAt []int) {
	var b boxWriter
	start := b.open("moov")
	b.mvhd(src, tracks)
	offsetAt = make([]int, len(tracks))
	for i, t := range tracks {
		offsetAt[i] = b.trak(src, t)
	}
	b.close(start)
	return b.buf, offsetAt
}

// mvhd writes the movie header. ISO/IEC 14496-12 8.2.2.
func (b *boxWriter) mvhd(f *File, tracks []*Track) {
	start := b.full("mvhd", 1, 0)
	b.u64(0) // creation time
	b.u64(0) // modification time
	b.u32(movieTimescale(f))
	b.u64(movieDuration(f))
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

// movieTimescale is the source's movie timescale, or a sane one when it
// declares none, since every duration in the output is measured on it.
func movieTimescale(f *File) uint32 {
	if f.Timescale == 0 {
		return 1000
	}
	return f.Timescale
}

// movieDuration is the mvhd duration to write. A source that declares none
// falls back to the same longest-track answer File.Duration gives, so the
// output probes to what the input probed to.
func movieDuration(f *File) uint64 {
	if f.MovieDuration > 0 {
		return f.MovieDuration
	}
	return durationToTicks(f.Duration(), movieTimescale(f))
}

// trak writes one track and returns the position of its chunk offset field.
func (b *boxWriter) trak(f *File, t *Track) int {
	start := b.open("trak")
	b.tkhd(f, t)
	// The edit list is copied rather than rebuilt: it is what makes the output
	// start where the input started, and a writer that re-derived it would be
	// a second chance to get the priming delay wrong.
	if len(t.edts) > 0 {
		edts := b.open("edts")
		b.raw(t.edts)
		b.close(edts)
	}
	at := b.mdia(t)
	b.close(start)
	return at
}

// tkhd writes the track header, matrix and all, so rotation survives the trip.
// ISO/IEC 14496-12 8.3.2.
func (b *boxWriter) tkhd(f *File, t *Track) {
	// The track is enabled and part of the movie.
	const enabledInMovie = 0x000003

	start := b.full("tkhd", 1, enabledInMovie)
	b.u64(0) // creation time
	b.u64(0) // modification time
	b.u32(t.ID)
	b.u32(0) // reserved
	b.u64(trackDuration(f, t))
	b.zeros(8) // reserved
	b.u16(0)   // layer
	b.u16(0)   // alternate group
	if t.Handler == "soun" {
		b.u16(0x0100) // volume, full
	} else {
		b.u16(0)
	}
	b.u16(0) // reserved
	for _, v := range t.Matrix {
		b.u32(uint32(v))
	}
	b.u32(t.Width << 16)
	b.u32(t.Height << 16)
	b.close(start)
}

// trackDuration is the track's duration on the movie timescale, which is what
// tkhd stores. An edit list already measures there, so its entries are summed
// where there is one; otherwise the media duration is converted.
func trackDuration(f *File, t *Track) uint64 {
	var edited uint64
	for _, e := range t.Edits {
		edited += e.Duration
	}
	if edited > 0 {
		return edited
	}
	return durationToTicks(t.Duration(), movieTimescale(f))
}

// mdia writes the media box and returns the position of the chunk offset field.
func (b *boxWriter) mdia(t *Track) int {
	start := b.open("mdia")
	b.mdhd(t)
	b.hdlr(t)
	at := b.minf(t)
	b.close(start)
	return at
}

// mdhd writes the media header. ISO/IEC 14496-12 8.4.2.
func (b *boxWriter) mdhd(t *Track) {
	start := b.full("mdhd", 1, 0)
	b.u64(0) // creation time
	b.u64(0) // modification time
	b.u32(t.Timescale)
	b.u64(t.MediaDuration)
	b.u16(packLanguage(t.Language))
	b.u16(0) // pre_defined
	b.close(start)
}

// undLanguage is "und" packed the way mdhd stores a language.
const undLanguage = 0x55c4

// packLanguage packs a three-letter ISO 639-2/T code into the five-bits-per-
// character form mdhd uses. Anything else is written as undetermined.
func packLanguage(code string) uint16 {
	if len(code) != 3 {
		return undLanguage
	}
	var packed uint16
	for i := range 3 {
		c := code[i]
		if c < 'a' || c > 'z' {
			return undLanguage
		}
		packed |= uint16(c-0x60) << (10 - 5*i)
	}
	return packed
}

// hdlr writes the handler reference with an empty name.
// ISO/IEC 14496-12 8.4.3.
func (b *boxWriter) hdlr(t *Track) {
	start := b.full("hdlr", 0, 0)
	b.u32(0) // pre_defined
	b.raw(fourcc(t.Handler))
	b.zeros(12)      // reserved
	b.raw([]byte{0}) // the name, empty
	b.close(start)
}

// minf writes the media information box and returns the position of the chunk
// offset field. ISO/IEC 14496-12 8.4.4.
func (b *boxWriter) minf(t *Track) int {
	start := b.open("minf")

	if t.Handler == "soun" {
		smhd := b.full("smhd", 0, 0)
		b.u16(0) // balance, centred
		b.u16(0) // reserved
		b.close(smhd)
	} else {
		// The flags of a vmhd are always 1. ISO/IEC 14496-12 12.1.2.
		vmhd := b.full("vmhd", 0, 1)
		b.u16(0)   // graphics mode, copy
		b.zeros(6) // opcolor
		b.close(vmhd)
	}

	// The media lives in this file, which is what a dref of one self-contained
	// url entry says. ISO/IEC 14496-12 8.7.2.
	dinf := b.open("dinf")
	dref := b.full("dref", 0, 0)
	b.u32(1)
	url := b.full("url ", 0, 1)
	b.close(url)
	b.close(dref)
	b.close(dinf)

	at := b.stbl(t)
	b.close(start)
	return at
}

// fourcc pads a name out to the four bytes the format stores it in.
func fourcc(name string) []byte {
	out := []byte("    ")
	copy(out, name)
	return out
}

// boxWriter builds a box tree in memory. A box is opened with a placeholder
// size, its children are appended, and closing it patches the size in, so the
// whole tree is written in one pass with no counting ahead.
type boxWriter struct{ buf []byte }

// open starts a box and returns the position to close it at.
func (b *boxWriter) open(typ string) int {
	start := len(b.buf)
	b.buf = append(b.buf, 0, 0, 0, 0)
	b.buf = append(b.buf, typ...)
	return start
}

// full starts a FullBox, writing its version and flags. ISO/IEC 14496-12 4.2.
func (b *boxWriter) full(typ string, version uint8, flags uint32) int {
	start := b.open(typ)
	b.u32(uint32(version)<<24 | flags&0x00ffffff)
	return start
}

// close patches a box's size now that its payload is written.
func (b *boxWriter) close(start int) {
	binary.BigEndian.PutUint32(b.buf[start:], uint32(len(b.buf)-start))
}

func (b *boxWriter) u16(v uint16) { b.buf = binary.BigEndian.AppendUint16(b.buf, v) }
func (b *boxWriter) u32(v uint32) { b.buf = binary.BigEndian.AppendUint32(b.buf, v) }
func (b *boxWriter) u64(v uint64) { b.buf = binary.BigEndian.AppendUint64(b.buf, v) }
func (b *boxWriter) raw(p []byte) { b.buf = append(b.buf, p...) }
func (b *boxWriter) zeros(n int)  { b.buf = append(b.buf, make([]byte, n)...) }
