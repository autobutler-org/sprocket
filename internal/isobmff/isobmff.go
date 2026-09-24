// Package isobmff parses the ISO base media file format family: mp4, mov, m4v,
// 3gp, and 3g2. One parser covers all five, which differ by brand rather than by
// structure.
//
// Parse takes an io.ReaderAt and a size and never reads the file whole. It walks
// the top-level boxes by header, reads the moov (and each moof in turn) into
// memory under a cap this package chooses, and leaves the media payload on disk.
// Sample tables stay in their on-disk form: lookups walk the compact runs rather
// than expanding to a slice per sample, so a file with millions of samples costs
// what its tables cost.
//
// Times come in two shapes and the distinction matters. Fields named in ticks
// (MovieDuration, MediaDuration, Edit.Duration) are integers on the timescale
// named next to them: the movie timescale from mvhd, or a track's media
// timescale from mdhd. Methods that return a time.Duration have already done the
// conversion. Rotation is clockwise degrees, matching what phones write, and
// Width and Height are the stored dimensions, before rotation.
package isobmff

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
)

// Size caps. Everything this package reads whole is bounded by one of these
// rather than by a size the file declares.
const (
	// maxMoovBytes bounds the movie header. A two-hour file with a sample every
	// frame has a moov of a few megabytes; 64 MiB is generous for anything real
	// and still a bound.
	maxMoovBytes = 64 << 20
	// maxMoofBytes bounds one movie fragment header. Fragments are seconds
	// long, so this is generous by the same margin.
	maxMoofBytes = 16 << 20
	// maxFtypBytes bounds the brand list.
	maxFtypBytes = 4 << 10
	// maxSampleBytes bounds one sample read. A 4K intra frame runs to a few
	// hundred kilobytes.
	maxSampleBytes = 32 << 20
)

// Fixed-point units used by the tkhd matrix. ISO/IEC 14496-12 8.3.2.
const (
	one16 = 1 << 16 // 1.0 in 16.16
	one30 = 1 << 30 // 1.0 in 2.30
)

// matrixIdentity is the unrotated transformation matrix.
var matrixIdentity = [9]int32{one16, 0, 0, 0, one16, 0, 0, 0, one30}

// topLevelStart are the box types a file in this family may begin with.
// Rejecting anything else keeps an unrelated binary from parsing by accident.
var topLevelStart = map[string]bool{
	"ftyp": true, "styp": true, "moov": true, "mdat": true,
	"free": true, "skip": true, "wide": true, "pnot": true, "pdin": true,
}

// File is a parsed ISOBMFF file. It keeps the reader it was parsed from, so it
// stays valid only as long as that reader does.
type File struct {
	// MajorBrand and CompatibleBrands come from ftyp, empty when there is none.
	MajorBrand       string
	CompatibleBrands []string
	// Timescale is the movie timescale in ticks per second, from mvhd.
	Timescale uint32
	// MovieDuration is the mvhd duration in Timescale ticks. A fragmented file
	// declares zero here; use Duration, which adds the fragments up.
	MovieDuration uint64
	// Tracks are the trak boxes in file order.
	Tracks []*Track
	// Fragmented reports whether the file carries mvex or any moof.
	Fragmented bool

	r     io.ReaderAt
	size  int64
	moofs []moofRange // every moof, in file order
}

// Edit is one edit list entry. ISO/IEC 14496-12 8.6.6.
type Edit struct {
	// Duration is in the movie timescale.
	Duration uint64
	// MediaTime is the track media time this edit starts at, in the track's
	// media timescale, or -1 for an empty edit that simply delays the track.
	MediaTime int64
	// Rate is the playback rate, 1.0 for normal playback.
	Rate float64
}

// SampleEntry is a track's first stsd entry: the codec and, where the codec has
// one, its configuration record.
type SampleEntry struct {
	// Format is the four-character sample entry code, such as "avc1".
	Format string
	// Codec is the stable short name: h264, hevc, av1, vp9, aac, opus, mp3.
	// An unrecognized format reports its own four-character code.
	Codec string
	// Width and Height are the stsd dimensions, set for a video track only.
	// Prefer the track's own Width and Height, which are what a player uses.
	Width, Height uint16
	// ChannelCount and SampleRate are set for an audio track only.
	ChannelCount uint16
	SampleRate   uint32
	// Config is the raw avcC, hvcC, av1C, vpcC, or dOps record, aliasing the
	// parsed moov. Treat it as read-only; it is nil when the entry carries none.
	Config []byte
	// NALLengthSize is the width in bytes of the length prefix on each NAL unit
	// in a sample, 1 through 4, for avcC and hvcC. Zero for everything else.
	NALLengthSize int
	// ObjectType is the esds objectTypeIndication, zero when there is no esds.
	ObjectType byte
}

// Track is one trak box: its header, its media timeline, and its sample tables.
type Track struct {
	// ID is the tkhd track_ID.
	ID uint32
	// Handler is the hdlr type, "vide" for video and "soun" for audio.
	Handler string
	// Timescale is this track's media timescale in ticks per second, from mdhd.
	Timescale uint32
	// MediaDuration is the mdhd duration in Timescale ticks. A fragmented track
	// declares zero here; use Duration, which adds the fragments up.
	MediaDuration uint64
	// Language is the mdhd language as ISO 639-2/T, "und" when unspecified.
	Language string
	// Width and Height are the tkhd display dimensions, truncated from 16.16
	// fixed point, before Rotation is applied.
	Width, Height uint32
	// Matrix is the tkhd transformation matrix as stored: a, b, u, c, d, v, x,
	// y, w, where a through d and x and y are 16.16 and u, v, and w are 2.30.
	Matrix [9]int32
	// Rotation is clockwise degrees: 0, 90, 180, or 270.
	Rotation int
	// RotationKnown reports whether Matrix was one of the four standard
	// rotations. When it is false Rotation is 0, which is the safe answer for a
	// matrix that also flips or shears, but the caller can tell it apart from a
	// genuine identity matrix.
	RotationKnown bool
	// Edits is the edit list, empty when the track has none.
	Edits []Edit
	// Entry is the track's first sample description.
	Entry SampleEntry

	// edts and stsd are those two box bodies as stored, aliasing the parsed
	// moov. The writer copies them verbatim rather than rebuilding them, which
	// is what keeps an edit list and a codec configuration intact through a
	// remux whatever they carry. Both are nil when the track has none.
	edts, stsd []byte

	tables    sampleTables
	defaults  trackDefaults
	fragments []trackFragment
	fragDur   uint64
	fragBytes int64
	fragCount uint32
}

// Parse reads the structure of an ISOBMFF file. It reads box headers, the moov,
// and every moof, and nothing else; media payload is left where it is.
func Parse(r io.ReaderAt, size int64) (*File, error) {
	if size < boxHeaderSize {
		return nil, fmt.Errorf("%w: %d bytes is too short to hold a box", ErrNotISOBMFF, size)
	}

	// The first box decides whether this is worth walking at all, which keeps an
	// unrelated binary from parsing by accident.
	var head [boxHeaderSize]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return nil, fmt.Errorf("%w: reading the first box header: %w", ErrNotISOBMFF, err)
	}
	if typ := string(head[4:]); !topLevelStart[typ] {
		return nil, fmt.Errorf("%w: it begins with a %q box", ErrNotISOBMFF, typ)
	}

	f := &File{r: r, size: size}
	var (
		moovOff, moovSize int64
		foundMoov         bool
	)
	if err := scanTop(r, size, func(h header, off int64) error {
		switch h.typ {
		case "ftyp", "styp":
			return f.readFtyp(off+h.hdrSize, h.size-h.hdrSize)
		case "moov":
			moovOff, moovSize, foundMoov = off+h.hdrSize, h.size-h.hdrSize, true
		case "moof":
			f.moofs = append(f.moofs, moofRange{start: off, payload: off + h.hdrSize, size: h.size - h.hdrSize})
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if !foundMoov {
		return nil, fmt.Errorf("%w: no moov box", ErrNotISOBMFF)
	}
	moov, err := readWhole(r, moovOff, moovSize, maxMoovBytes, "moov")
	if err != nil {
		return nil, err
	}
	if err := f.parseMoov(moov); err != nil {
		return nil, err
	}
	if len(f.Tracks) == 0 {
		return nil, fmt.Errorf("%w: the moov declares no track", ErrMalformed)
	}
	if len(f.moofs) > 0 {
		f.Fragmented = true
		if err := f.parseFragments(); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// Duration reports the movie duration. A fragmented file, whose mvhd duration is
// zero, reports its longest track instead.
func (f *File) Duration() time.Duration {
	if f.MovieDuration > 0 && f.Timescale > 0 {
		return ticksToDuration(f.MovieDuration, f.Timescale)
	}
	var longest time.Duration
	for _, t := range f.Tracks {
		longest = max(longest, t.Duration())
	}
	return longest
}

// VideoTrack returns the first track with a vide handler, or nil.
func (f *File) VideoTrack() *Track { return f.trackByHandler("vide") }

// AudioTrack returns the first track with a soun handler, or nil.
func (f *File) AudioTrack() *Track { return f.trackByHandler("soun") }

func (f *File) trackByHandler(handler string) *Track {
	for _, t := range f.Tracks {
		if t.Handler == handler {
			return t
		}
	}
	return nil
}

func (f *File) trackByID(id uint32) *Track {
	for _, t := range f.Tracks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Duration reports the track's media duration.
func (t *Track) Duration() time.Duration {
	ticks := t.MediaDuration
	if ticks == 0 {
		ticks = t.fragDur
	}
	return ticksToDuration(ticks, t.Timescale)
}

// SampleCount reports how many samples the track holds, across fragments.
func (t *Track) SampleCount() uint32 { return t.tables.count + t.fragCount }

// SampleBytes reports the total size of the track's samples. It is what a
// per-track bitrate is derived from.
func (t *Track) SampleBytes() int64 { return t.tables.totalBytes() + t.fragBytes }

// FrameRate reports samples per second averaged over the track: an honest
// number for variable-framerate content rather than a nominal rate. It is 0 for
// a track with no duration.
func (t *Track) FrameRate() float64 {
	seconds := t.Duration().Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(t.SampleCount()) / seconds
}

// EmptyEditDuration reports the leading empty edit, in the movie timescale. It
// is the delay phone footage commonly carries before its first sample; zero when
// the track has no edit list or its first edit is a real one.
func (t *Track) EmptyEditDuration() uint64 {
	if len(t.Edits) == 0 || t.Edits[0].MediaTime != -1 {
		return 0
	}
	return t.Edits[0].Duration
}

// mediaTicks maps a presentation time on the movie timeline to this track's
// media timeline, honoring the leading empty edit and the media time the first
// real edit starts at.
func (t *Track) mediaTicks(at time.Duration, movieTimescale uint32) uint64 {
	at -= ticksToDuration(t.EmptyEditDuration(), movieTimescale)
	ticks := durationToTicks(at, t.Timescale)
	for _, e := range t.Edits {
		if e.MediaTime >= 0 {
			return ticks + uint64(e.MediaTime)
		}
	}
	return ticks
}

// movieTime maps a media composition time back onto the movie timeline, the
// inverse of mediaTicks. A time inside the part of the media the edit list
// trims away reports zero, which is where the edit puts it.
func (t *Track) movieTime(ticks uint64, movieTimescale uint32) time.Duration {
	for _, e := range t.Edits {
		if e.MediaTime >= 0 {
			if ticks < uint64(e.MediaTime) {
				ticks = uint64(e.MediaTime)
			}
			ticks -= uint64(e.MediaTime)
			break
		}
	}
	return ticksToDuration(ticks, t.Timescale) + ticksToDuration(t.EmptyEditDuration(), movieTimescale)
}

// readFtyp records the brands. ISO/IEC 14496-12 4.3.
func (f *File) readFtyp(off, size int64) error {
	body, err := readWhole(f.r, off, size, maxFtypBytes, "ftyp")
	if err != nil {
		return err
	}
	if len(body) < 8 {
		return fmt.Errorf("%w: ftyp is %d bytes", ErrNotISOBMFF, len(body))
	}
	f.MajorBrand = strings.TrimRight(string(body[:4]), " ")
	for off := 8; off+4 <= len(body); off += 4 {
		f.CompatibleBrands = append(f.CompatibleBrands, strings.TrimRight(string(body[off:off+4]), " "))
	}
	return nil
}

func (f *File) parseMoov(body []byte) error {
	trex := map[uint32]trackDefaults{}
	if err := walk(body, 1, func(typ string, child []byte) error {
		switch typ {
		case "mvhd":
			return f.parseMvhd(child)
		case "trak":
			t, err := parseTrak(child, 2)
			if err != nil {
				return err
			}
			f.Tracks = append(f.Tracks, t)
		case "mvex":
			f.Fragmented = true
			return parseMvex(child, 2, trex)
		}
		return nil
	}); err != nil {
		return err
	}
	// mvex comes after the traks in every file that follows the spec's box
	// order, but nothing enforces that, so the defaults are applied afterwards.
	for id, d := range trex {
		if t := f.trackByID(id); t != nil {
			t.defaults = d
		}
	}
	return nil
}

// parseMvhd reads the movie header. ISO/IEC 14496-12 8.2.2.
func (f *File) parseMvhd(body []byte) error {
	version, _, rest, ok := fullBoxVersion(body)
	if !ok {
		return fmt.Errorf("%w: mvhd is %d bytes", ErrMalformed, len(body))
	}
	need := 16
	if version == 1 {
		need = 28
	}
	if len(rest) < need {
		return fmt.Errorf("%w: mvhd v%d body is %d bytes, need %d", ErrMalformed, version, len(rest), need)
	}
	if version == 1 {
		f.Timescale, f.MovieDuration = be32(rest[16:]), be64(rest[20:])
	} else {
		f.Timescale, f.MovieDuration = be32(rest[8:]), uint64(be32(rest[12:]))
	}
	return nil
}

func parseTrak(body []byte, depth int) (*Track, error) {
	t := &Track{}
	err := walk(body, depth, func(typ string, child []byte) error {
		switch typ {
		case "tkhd":
			return t.parseTkhd(child)
		case "edts":
			t.edts = child
			return walk(child, depth+1, func(typ string, e []byte) error {
				if typ == "elst" {
					return t.parseElst(e)
				}
				return nil
			})
		case "mdia":
			return t.parseMdia(child, depth+1)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// A trak with no media header has no timeline, so nothing can be asked of
	// it. Rejecting it here also stops a tree of nested traks from parsing into
	// a track made of zeroes.
	if t.Timescale == 0 {
		return nil, fmt.Errorf("%w: track %d has no media timescale", ErrMalformed, t.ID)
	}
	return t, nil
}

// parseTkhd reads the track header. ISO/IEC 14496-12 8.3.2.
func (t *Track) parseTkhd(body []byte) error {
	version, _, rest, ok := fullBoxVersion(body)
	if !ok {
		return fmt.Errorf("%w: tkhd is %d bytes", ErrMalformed, len(body))
	}
	head := 20
	if version == 1 {
		head = 32
	}
	// The header is followed by 16 bytes of reserved, layer, alternate group,
	// and volume fields, then the 36-byte matrix and the 16.16 dimensions.
	const tail = 16 + 36 + 8
	if len(rest) < head+tail {
		return fmt.Errorf("%w: tkhd v%d body is %d bytes, need %d", ErrMalformed, version, len(rest), head+tail)
	}
	if version == 1 {
		t.ID = be32(rest[16:])
	} else {
		t.ID = be32(rest[8:])
	}
	matrix := rest[head+16:]
	for i := range t.Matrix {
		t.Matrix[i] = int32(be32(matrix[i*4:]))
	}
	t.Rotation, t.RotationKnown = rotationFromMatrix(t.Matrix)
	t.Width = be32(matrix[36:]) >> 16
	t.Height = be32(matrix[40:]) >> 16
	return nil
}

// rotationFromMatrix maps the four standard tkhd matrices to clockwise degrees.
// Only the 2x2 rotation part is considered: the translation in x and y moves the
// picture without turning it. Anything else, including a flip or a shear, is
// reported as unrecognized rather than guessed at.
func rotationFromMatrix(m [9]int32) (int, bool) {
	a, b, c, d := m[0], m[1], m[3], m[4]
	switch {
	case a == one16 && b == 0 && c == 0 && d == one16:
		return 0, true
	case a == 0 && b == one16 && c == -one16 && d == 0:
		return 90, true
	case a == -one16 && b == 0 && c == 0 && d == -one16:
		return 180, true
	case a == 0 && b == -one16 && c == one16 && d == 0:
		return 270, true
	}
	return 0, false
}

// parseElst reads the edit list. ISO/IEC 14496-12 8.6.6.
func (t *Track) parseElst(body []byte) error {
	version, _, rest, ok := fullBoxVersion(body)
	if !ok || len(rest) < 4 {
		return fmt.Errorf("%w: elst is %d bytes", ErrMalformed, len(body))
	}
	count := be32(rest)
	width := 12
	if version == 1 {
		width = 20
	}
	table, err := entries(rest[4:], count, width)
	if err != nil {
		return err
	}
	t.Edits = make([]Edit, 0, count)
	for off := 0; off+width <= len(table); off += width {
		e := Edit{}
		if version == 1 {
			e.Duration, e.MediaTime = be64(table[off:]), int64(be64(table[off+8:]))
		} else {
			e.Duration, e.MediaTime = uint64(be32(table[off:])), int64(int32(be32(table[off+4:])))
		}
		e.Rate = float64(int32(be32(table[off+width-4:]))) / one16
		t.Edits = append(t.Edits, e)
	}
	return nil
}

func (t *Track) parseMdia(body []byte, depth int) error {
	var minf []byte
	if err := walk(body, depth, func(typ string, child []byte) error {
		switch typ {
		case "mdhd":
			return t.parseMdhd(child)
		case "hdlr":
			// The hdlr inside minf names the data handler, not the media, so
			// only the one directly under mdia is read.
			if _, _, rest, ok := fullBoxVersion(child); ok && len(rest) >= 8 {
				t.Handler = strings.TrimRight(string(rest[4:8]), " \x00")
			}
		case "minf":
			minf = child
		}
		return nil
	}); err != nil {
		return err
	}
	// minf is parsed second so the handler is known by the time stsd needs it,
	// whatever order the boxes appear in.
	return walk(minf, depth+1, func(typ string, child []byte) error {
		if typ == "stbl" {
			return t.parseStbl(child, depth+2)
		}
		return nil
	})
}

// parseMdhd reads the media header. ISO/IEC 14496-12 8.4.2.
func (t *Track) parseMdhd(body []byte) error {
	version, _, rest, ok := fullBoxVersion(body)
	if !ok {
		return fmt.Errorf("%w: mdhd is %d bytes", ErrMalformed, len(body))
	}
	need := 20
	if version == 1 {
		need = 32
	}
	if len(rest) < need {
		return fmt.Errorf("%w: mdhd v%d body is %d bytes, need %d", ErrMalformed, version, len(rest), need)
	}
	if version == 1 {
		t.Timescale, t.MediaDuration = be32(rest[16:]), be64(rest[20:])
		t.Language = decodeLanguage(be16(rest[28:]))
	} else {
		t.Timescale, t.MediaDuration = be32(rest[8:]), uint64(be32(rest[12:]))
		t.Language = decodeLanguage(be16(rest[16:]))
	}
	return nil
}

// decodeLanguage unpacks the three five-bit characters of an ISO 639-2/T code.
// QuickTime writes 0x7fff for unspecified, which falls out as "und" here.
func decodeLanguage(packed uint16) string {
	var out [3]byte
	for i := range out {
		c := byte((packed>>(10-5*i))&0x1f) + 0x60
		if c < 'a' || c > 'z' {
			return "und"
		}
		out[i] = c
	}
	return string(out[:])
}

// ticksToDuration converts a count on a timescale to wall time without
// overflowing on a large tick count.
func ticksToDuration(ticks uint64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	ts := uint64(timescale)
	whole := ticks / ts
	if whole > uint64(time.Duration(1<<62)/time.Second) {
		return time.Duration(1 << 62)
	}
	return time.Duration(whole)*time.Second + time.Duration((ticks%ts)*uint64(time.Second)/ts)
}

// durationToTicks converts wall time to a count on a timescale, clamping a
// negative time to zero.
func durationToTicks(d time.Duration, timescale uint32) uint64 {
	if d <= 0 || timescale == 0 {
		return 0
	}
	ts := uint64(timescale)
	return uint64(d/time.Second)*ts + uint64(d%time.Second)*ts/uint64(time.Second)
}

func be16(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
