package mpegts

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// PIDs and timing of the streams this writer produces. The PIDs are the ones
// ffmpeg picks, which is as good a convention as any.
const (
	outPMTPID   = 0x1000
	outFirstPID = 0x100
	outProgram  = 1
	// outStart is the timestamp the output's timeline begins at. A decoder
	// wants room for the decode lead of a reordered stream and for the clock
	// reference to run ahead of the first frame, and a second covers both.
	outStart = clockRate
	// pcrLead is how far the program clock reference runs behind the decode
	// time of the frame it rides on, which is the buffering a decoder is given.
	pcrLead = clockRate / 10
	// tableInterval is how often the tables are repeated, at the next keyframe
	// once this much time has passed, so that a player joining mid-file finds
	// them. It is under half a second so that a stream with a keyframe every
	// half second, which is common, gets them in front of every one.
	tableInterval = 400 * time.Millisecond
	// readBufferSize is the one read buffer the sample conversion uses.
	readBufferSize = 64 << 10
)

// outCodec is how one codec is carried: its stream type and PES stream ID.
type outCodec struct {
	streamType byte
	streamID   byte
}

// outCodecs are the codecs this writer can carry, which is the TS column of
// isobmff.CodecTargets. ISO/IEC 13818-1 table 2-34 and ATSC A/52 Annex A.
var outCodecs = map[string]outCodec{
	"h264": {streamH264, 0xe0},
	"hevc": {streamHEVC, 0xe0},
	"aac":  {streamADTS, 0xc0},
	"mp3":  {streamMPEG1Audio, 0xc0},
	"ac-3": {streamAC3, 0xbd},
	"ec-3": {streamEAC3, 0xbd},
}

// Write writes an ISOBMFF source into w as an MPEG transport stream: one
// program, every video and audio track as an elementary stream of it, and the
// samples interleaved by decode time. Nothing is re-encoded.
//
// ranges cuts each track down to a span of its samples, keyed by track ID, as
// isobmff.Write takes it; a nil map writes every track whole.
//
// H.264 and HEVC samples are rewritten from length-prefixed NAL units to Annex
// B start codes, with an access unit delimiter in front and the parameter sets
// from the configuration record in front of each keyframe, which is what lets
// a decoder start at any keyframe. AAC samples get the ADTS header built from
// the AudioSpecificConfig, which limits them to the object types an ADTS
// header can name. MPEG audio and the Dolby formats go in as they are.
//
// The tables go out first and again in front of the next keyframe once 0.4
// seconds have passed. The
// program clock reference rides on the first video stream's PES packets, or
// the first audio stream's when there is no video, a tenth of a second behind
// their decode time. Samples are read one at a time through one buffer, so the
// output costs the same heap whatever the source's length.
//
// A fragmented source returns isobmff.ErrUnsupportedSource. A codec a
// transport stream cannot carry returns isobmff.ErrIncompatibleCodec, and an
// AAC configuration an ADTS header cannot express returns
// isobmff.ErrUnsupportedSource.
func Write(w io.Writer, src *isobmff.File, ranges map[uint32]isobmff.Range) error {
	if src.Fragmented {
		return fmt.Errorf("%w: the source is fragmented, and its samples are described by moof boxes rather than by the moov's tables",
			isobmff.ErrUnsupportedSource)
	}
	tracks, err := outTracks(src, ranges)
	if err != nil {
		return err
	}
	m := &muxer{
		w: w, src: src, tracks: tracks, cc: map[uint16]uint8{},
		read: bufio.NewReaderSize(nil, readBufferSize), buf: make([]byte, readBufferSize),
	}
	return m.run()
}

// outTrack is one track of the output and the cursor the interleave walks it
// with.
type outTrack struct {
	src   *isobmff.Track
	codec outCodec
	pid   uint16
	// params are the parameter sets from the configuration record, each as a
	// NAL unit, which go in front of every keyframe.
	params [][]byte
	// adts is the fixed part of an ADTS header, for an AAC track.
	adts        adtsHeader
	first, last uint32
	next        uint32
	origin      time.Duration
	hevc        bool
}

func outTracks(src *isobmff.File, ranges map[uint32]isobmff.Range) ([]*outTrack, error) {
	var out []*outTrack
	for _, t := range src.Tracks {
		if t.Handler != "vide" && t.Handler != "soun" {
			continue
		}
		if !isobmff.CodecFits(t.Entry.Codec, isobmff.TargetTS) {
			return nil, fmt.Errorf("%w: track %d carries %s, which does not fit in ts",
				isobmff.ErrIncompatibleCodec, t.ID, t.Entry.Codec)
		}
		if t.SampleCount() == 0 {
			continue
		}
		o := &outTrack{
			src: t, codec: outCodecs[t.Entry.Codec], pid: outFirstPID + uint16(len(out)),
			last: t.SampleCount() - 1, hevc: t.Entry.Codec == "hevc",
		}
		// Each video and each MPEG audio stream gets a stream ID of its own
		// kind, counting from the first; the Dolby streams all share private
		// stream 1, which is what the format says for them.
		if o.codec.streamID != 0xbd {
			for _, prior := range out {
				if prior.codec.streamID&0xf0 == o.codec.streamID&0xf0 {
					o.codec.streamID++
				}
			}
		}
		if err := o.configure(); err != nil {
			return nil, err
		}
		if span, cut := ranges[t.ID]; cut {
			if span.Last < span.First || span.Last >= t.SampleCount() {
				return nil, fmt.Errorf("%w: track %d has %d samples, so the range %d..%d is not in it",
					isobmff.ErrMalformed, t.ID, t.SampleCount(), span.First, span.Last)
			}
			o.first, o.last = span.First, span.Last
			sample, err := t.Sample(span.First)
			if err != nil {
				return nil, err
			}
			// Anchored on when the first kept sample is shown, as every other
			// writer's cut is, so that the output's first picture is at zero.
			o.origin = movieTime(t, int64(sample.Composition), src.Timescale)
		}
		o.next = o.first
		out = append(out, o)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: it carries no video or audio track", isobmff.ErrUnsupportedSource)
	}
	return out, nil
}

// configure reads what the track's codec needs out of its sample entry.
func (o *outTrack) configure() error {
	e := o.src.Entry
	switch e.Codec {
	case "h264", "hevc":
		if e.NALLengthSize < 1 || e.NALLengthSize > 4 {
			return fmt.Errorf("%w: track %d has a %d byte NAL length", isobmff.ErrUnsupportedSource, o.src.ID, e.NALLengthSize)
		}
		sets, err := recordParamSets(e.Config, o.hevc)
		if err != nil {
			return fmt.Errorf("%w: track %d: %w", isobmff.ErrUnsupportedSource, o.src.ID, err)
		}
		o.params = sets
	case "aac":
		h, err := adtsFromConfig(e.DecoderConfig)
		if err != nil {
			return fmt.Errorf("%w: track %d: %w", isobmff.ErrUnsupportedSource, o.src.ID, err)
		}
		o.adts = h
	}
	return nil
}

// recordParamSets pulls the parameter sets out of an avcC or hvcC record, each
// as a NAL unit. Every declared length is checked against what is left.
// ISO/IEC 14496-15 5.3.3.1 and 8.3.3.1.
func recordParamSets(record []byte, hevc bool) ([][]byte, error) {
	var sets [][]byte
	take := func(rest []byte) ([]byte, []byte, error) {
		if len(rest) < 2 || int(binary.BigEndian.Uint16(rest)) > len(rest)-2 {
			return nil, nil, fmt.Errorf("%w: a configuration record cut short", ErrMalformed)
		}
		n := int(binary.BigEndian.Uint16(rest))
		return rest[2 : 2+n], rest[2+n:], nil
	}
	var (
		nal []byte
		err error
	)
	if !hevc {
		const header = 6
		if len(record) < header+1 {
			return nil, fmt.Errorf("%w: a %d byte avcC", ErrMalformed, len(record))
		}
		rest := record[header:]
		for range int(record[5] & 0x1f) {
			if nal, rest, err = take(rest); err != nil {
				return nil, err
			}
			sets = append(sets, nal)
		}
		if len(rest) < 1 {
			return nil, fmt.Errorf("%w: an avcC with no PPS count", ErrMalformed)
		}
		count := int(rest[0])
		rest = rest[1:]
		for range count {
			if nal, rest, err = take(rest); err != nil {
				return nil, err
			}
			sets = append(sets, nal)
		}
		return sets, nil
	}

	const header = 23
	if len(record) < header {
		return nil, fmt.Errorf("%w: a %d byte hvcC", ErrMalformed, len(record))
	}
	rest := record[header:]
	for range int(record[header-1]) {
		if len(rest) < 3 {
			return nil, fmt.Errorf("%w: an hvcC array cut short", ErrMalformed)
		}
		count := int(binary.BigEndian.Uint16(rest[1:]))
		rest = rest[3:]
		for range count {
			if nal, rest, err = take(rest); err != nil {
				return nil, err
			}
			sets = append(sets, nal)
		}
	}
	return sets, nil
}

// adtsFromConfig reads the fields an ADTS header repeats out of an
// AudioSpecificConfig. ADTS has two bits for the object type and four for the
// rate index, so an object type past 4, an escaped one, or an explicit rate
// has no ADTS form. ISO/IEC 14496-3 1.6.2.1.
func adtsFromConfig(asc []byte) (adtsHeader, error) {
	if len(asc) < 2 {
		return adtsHeader{}, fmt.Errorf("%w: a %d byte AudioSpecificConfig", ErrMalformed, len(asc))
	}
	object := int(asc[0] >> 3)
	rate := int(asc[0]&0x07)<<1 | int(asc[1]>>7)
	channels := int(asc[1] >> 3 & 0x0f)
	if object < 1 || object > 4 || rate >= len(adtsSampleRates) || channels > 7 {
		return adtsHeader{}, fmt.Errorf("%w: object type %d at rate index %d with %d channels has no ADTS header",
			ErrMalformed, object, rate, channels)
	}
	return adtsHeader{profile: object - 1, rate: rate, channels: channels}, nil
}

// movieTime puts a media time on the movie timeline through the track's edit
// list, as isobmff.Track.MovieTime does, except that a time the edit trims
// away comes out negative rather than clamped to zero. A transport stream can
// state a decode time before the first frame is shown, and has to: the frames
// of a reordered stream are decoded ahead of when they are shown, and clamping
// would stamp several of them with the same decode time.
func movieTime(t *isobmff.Track, ticks int64, movieTimescale uint32) time.Duration {
	for _, e := range t.Edits {
		if e.MediaTime >= 0 {
			ticks -= e.MediaTime
			break
		}
	}
	return scaled(ticks, t.Timescale) + scaled(int64(t.EmptyEditDuration()), movieTimescale)
}

// scaled converts ticks of a timescale to wall time without overflowing.
func scaled(ticks int64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	scale := int64(timescale)
	return time.Duration(ticks/scale*int64(time.Second) + ticks%scale*int64(time.Second)/scale)
}

// muxer is the state of one Write.
type muxer struct {
	w          io.Writer
	src        *isobmff.File
	tracks     []*outTrack
	cc         map[uint16]uint8
	read       *bufio.Reader
	buf        []byte
	lastTables time.Duration
	tables     bool // the tables have gone out once
	pkt        [packetSize]byte
	pes        pesWriter
}

// pending is the sample one track offers the interleave next.
type pending struct {
	track        *outTrack
	sample       isobmff.Sample
	decode, show time.Duration
}

func (m *muxer) run() error {
	pcr := m.tracks[0]
	for _, t := range m.tracks {
		if t.src.Handler == "vide" {
			pcr = t
			break
		}
	}
	m.pes.m = m
	for {
		next, err := m.nextSample()
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		if !m.tables || next.track == pcr && next.sample.Sync && next.decode-m.lastTables >= tableInterval {
			if err := m.writeTables(pcr.pid); err != nil {
				return err
			}
			m.lastTables, m.tables = next.decode, true
		}
		if err := m.writeSample(next, next.track == pcr); err != nil {
			return err
		}
	}
}

// nextSample takes the sample with the earliest decode time from whichever
// track holds it and advances that track's cursor, or returns nil when every
// track is spent. It holds one sample per track, so the interleave costs the
// number of tracks whatever the file's length.
func (m *muxer) nextSample() (*pending, error) {
	var best *pending
	for _, t := range m.tracks {
		if t.next > t.last {
			continue
		}
		s, err := t.src.Sample(t.next)
		if err != nil {
			return nil, err
		}
		found := &pending{
			track: t, sample: s,
			decode: movieTime(t.src, int64(s.Decode), m.src.Timescale) - t.origin,
			show:   movieTime(t.src, int64(s.Composition), m.src.Timescale) - t.origin,
		}
		if best == nil || found.decode < best.decode {
			best = found
		}
	}
	if best != nil {
		best.track.next++
	}
	return best, nil
}

// writeTables writes the program association table and the program map table.
func (m *muxer) writeTables(pcrPID uint16) error {
	pat := []byte{0, outProgram, 0xe0 | outPMTPID>>8, outPMTPID & 0xff}
	if err := m.writeSection(patPID, tablePAT, 1, pat); err != nil {
		return err
	}
	pmt := []byte{0xe0 | byte(pcrPID>>8), byte(pcrPID), 0xf0, 0}
	for _, t := range m.tracks {
		pmt = append(pmt, t.codec.streamType, 0xe0|byte(t.pid>>8), byte(t.pid), 0xf0, 0)
	}
	return m.writeSection(outPMTPID, tablePMT, outProgram, pmt)
}

// writeSection writes one long-form section in one packet: the header, the
// body, and the CRC, padded out with 0xff. ISO/IEC 13818-1 2.4.4.
func (m *muxer) writeSection(pid uint16, table byte, id uint16, body []byte) error {
	sec := []byte{table, 0, 0, byte(id >> 8), byte(id), 0xc1, 0, 0}
	sec = append(sec, body...)
	binary.BigEndian.PutUint16(sec[1:], 0xb000|uint16(len(sec)-3+4))
	sec = binary.BigEndian.AppendUint32(sec, crc32MPEG(sec))

	p := m.pkt[:]
	m.header(p, pid, true, false)
	p[headerSize] = 0 // pointer_field
	n := copy(p[headerSize+1:], sec)
	for i := headerSize + 1 + n; i < packetSize; i++ {
		p[i] = 0xff
	}
	return m.emit(p)
}

// header writes a packet header with the next continuity counter for the PID.
func (m *muxer) header(p []byte, pid uint16, start, adaptation bool) {
	cc := m.cc[pid]
	m.cc[pid] = (cc + 1) & 0x0f
	p[0] = syncByte
	p[1] = byte(pid >> 8 & 0x1f)
	if start {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	p[3] = 0x10 | cc
	if adaptation {
		p[3] |= 0x20
	}
}

func (m *muxer) emit(p []byte) error {
	if _, err := m.w.Write(p); err != nil {
		return fmt.Errorf("writing the output: %w", err)
	}
	return nil
}

// writeSample writes one sample as one PES packet.
func (m *muxer) writeSample(s *pending, carriesPCR bool) error {
	t := s.track
	dts := outStart + durationToTicks(s.decode)
	pts := outStart + durationToTicks(s.show)
	video := t.src.Handler == "vide"

	fields := timestampField(pts, 0x02)
	if pts != dts {
		fields = append(timestampField(pts, 0x03), timestampField(dts, 0x01)...)
	}
	head := []byte{0, 0, 1, t.codec.streamID, 0, 0, 0x80, 0x80, byte(len(fields))}
	if pts != dts {
		head[7] = 0xc0
	}
	head = append(head, fields...)

	var body int64
	if !video {
		body = s.sample.Size
		if t.src.Entry.Codec == "aac" {
			body += 7
		}
		length := int64(len(head)) - 6 + body
		if length > 0xffff {
			return fmt.Errorf("%w: an audio sample of %d bytes, over what a PES can hold", isobmff.ErrUnsupportedSource, body)
		}
		binary.BigEndian.PutUint16(head[4:], uint16(length))
	}

	m.pes.begin(t.pid, s.sample.Sync && video, carriesPCR, dts-pcrLead)
	if _, err := m.pes.Write(head); err != nil {
		return err
	}
	var err error
	switch {
	case video:
		err = m.writeVideo(t, s.sample)
	case t.src.Entry.Codec == "aac":
		err = m.writeADTS(t, s.sample)
	default:
		err = m.copySample(s.sample.Offset, s.sample.Size)
	}
	if err != nil {
		return err
	}
	return m.pes.finish()
}

// timestampField encodes a PTS or DTS: a four bit prefix and the 33 bits in
// runs of three, fifteen, and fifteen, each followed by a marker bit.
// ISO/IEC 13818-1 2.4.3.7.
func timestampField(ts int64, prefix byte) []byte {
	ts &= timestampWrap - 1
	return []byte{
		prefix<<4 | byte(ts>>29)&0x0e | 1,
		byte(ts >> 22),
		byte(ts>>14)&0xfe | 1,
		byte(ts >> 7),
		byte(ts<<1) | 1,
	}
}

// audDelimiter is the access unit delimiter put in front of each access unit:
// the NAL header and a payload saying any picture type may follow.
var audDelimiter = map[bool][]byte{
	false: {0, 0, 0, 1, h264AUD, 0xf0},
	true:  {0, 0, 0, 1, hevcAUD << 1, 0x01, 0x50},
}

var startCode = []byte{0, 0, 0, 1}

// writeVideo writes one length-prefixed sample as Annex B: a delimiter, the
// parameter sets in front of a keyframe, and each NAL unit behind a start code.
// A delimiter already in the sample is dropped, since one went out in front.
func (m *muxer) writeVideo(t *outTrack, s isobmff.Sample) error {
	if _, err := m.pes.Write(audDelimiter[t.hevc]); err != nil {
		return err
	}
	if s.Sync {
		for _, p := range t.params {
			if _, err := m.pes.Write(startCode); err != nil {
				return err
			}
			if _, err := m.pes.Write(p); err != nil {
				return err
			}
		}
	}

	width := t.src.Entry.NALLengthSize
	m.read.Reset(io.NewSectionReader(m.src.ReaderAt(), s.Offset, s.Size))
	for left := s.Size; left > 0; {
		if left < int64(width) {
			return fmt.Errorf("%w: a sample ends inside a NAL length", isobmff.ErrMalformed)
		}
		prefix := m.buf[:width]
		if _, err := io.ReadFull(m.read, prefix); err != nil {
			return fmt.Errorf("%w: reading a sample at %d: %w", isobmff.ErrTruncated, s.Offset, err)
		}
		var n int64
		for _, b := range prefix {
			n = n<<8 | int64(b)
		}
		left -= int64(width)
		if n > left {
			return fmt.Errorf("%w: a %d byte NAL unit with %d bytes of sample left", isobmff.ErrMalformed, n, left)
		}
		left -= n
		if n == 0 {
			continue
		}
		head, err := m.read.Peek(1)
		if err != nil {
			return fmt.Errorf("%w: reading a sample at %d: %w", isobmff.ErrTruncated, s.Offset, err)
		}
		if typ := nalType(t.hevc, head[0]); typ == h264AUD && !t.hevc || typ == hevcAUD && t.hevc {
			if _, err := m.read.Discard(int(n)); err != nil {
				return fmt.Errorf("%w: reading a sample at %d: %w", isobmff.ErrTruncated, s.Offset, err)
			}
			continue
		}
		if _, err := m.pes.Write(startCode); err != nil {
			return err
		}
		if err := m.pass(n); err != nil {
			return err
		}
	}
	return nil
}

// writeADTS writes one AAC sample behind the ADTS header its size and the
// track's configuration make.
func (m *muxer) writeADTS(t *outTrack, s isobmff.Sample) error {
	size := s.Size + 7
	if size > 0x1fff {
		return fmt.Errorf("%w: an AAC frame of %d bytes, over what an ADTS header can state", isobmff.ErrUnsupportedSource, size)
	}
	h := t.adts
	header := []byte{
		0xff, 0xf1, // sync word, MPEG-4, no CRC
		byte(h.profile<<6 | h.rate<<2 | h.channels>>2),
		byte(h.channels&3<<6) | byte(size>>11),
		byte(size >> 3),
		byte(size&7<<5) | 0x1f,
		0xfc, // buffer fullness unstated, one raw data block
	}
	if _, err := m.pes.Write(header); err != nil {
		return err
	}
	return m.copySample(s.Offset, s.Size)
}

// copySample copies a sample's bytes as they stand.
func (m *muxer) copySample(offset, size int64) error {
	m.read.Reset(io.NewSectionReader(m.src.ReaderAt(), offset, size))
	return m.pass(size)
}

// pass moves n bytes from the sample reader into the PES through the one
// buffer.
func (m *muxer) pass(n int64) error {
	for n > 0 {
		chunk := m.buf[:min(n, int64(len(m.buf)))]
		got, err := io.ReadFull(m.read, chunk)
		if err != nil {
			return fmt.Errorf("%w: reading a sample: %w", isobmff.ErrTruncated, err)
		}
		if _, err := m.pes.Write(chunk[:got]); err != nil {
			return err
		}
		n -= int64(got)
	}
	return nil
}

// pesWriter cuts one PES packet into transport packets as its bytes arrive. It
// holds at most one packet's payload, so a PES of any size streams through.
// The first packet carries the adaptation field for the clock reference and the
// random access flag where they apply, and the last is padded out with
// adaptation field stuffing, since a PES may not share its packets.
type pesWriter struct {
	m        *muxer
	pid      uint16
	first    bool
	random   bool
	pcr      bool
	pcrValue int64
	payload  [payloadSize]byte
	n        int
}

func (p *pesWriter) begin(pid uint16, random, pcr bool, pcrValue int64) {
	p.pid, p.first, p.random, p.pcr, p.pcrValue, p.n = pid, true, random, pcr, max(pcrValue, 0), 0
}

// fields is the adaptation field the current packet needs before stuffing,
// counting its length byte.
func (p *pesWriter) fields() int {
	switch {
	case !p.first || !p.random && !p.pcr:
		return 0
	case p.pcr:
		return 2 + 6
	default:
		return 2
	}
}

func (p *pesWriter) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		room := payloadSize - p.fields() - p.n
		k := copy(p.payload[p.n:p.n+room], b)
		p.n += k
		b = b[k:]
		if p.n == payloadSize-p.fields() {
			if err := p.flush(); err != nil {
				return total - len(b), err
			}
		}
	}
	return total, nil
}

// finish writes the last packet, stuffed out to its full size.
func (p *pesWriter) finish() error {
	if p.n == 0 {
		return nil
	}
	return p.flush()
}

// flush writes the held payload as one packet, with whatever adaptation field
// it needs and stuffing for whatever room is left. ISO/IEC 13818-1 2.4.3.4.
func (p *pesWriter) flush() error {
	var (
		pkt      = p.m.pkt[:]
		base     = p.fields()
		stuffing = payloadSize - base - p.n
	)
	p.m.header(pkt, p.pid, p.first, base+stuffing > 0)
	at := headerSize
	if base+stuffing > 0 {
		length := base + stuffing - 1
		pkt[at] = byte(length)
		at++
		if length > 0 {
			var flags byte
			if base > 0 && p.random {
				flags |= 0x40
			}
			if base > 0 && p.pcr {
				flags |= 0x10
			}
			pkt[at] = flags
			at++
			if base > 0 && p.pcr {
				// 33 bits of base, six reserved bits, and a zero extension.
				v := p.pcrValue & (timestampWrap - 1)
				pkt[at] = byte(v >> 25)
				pkt[at+1] = byte(v >> 17)
				pkt[at+2] = byte(v >> 9)
				pkt[at+3] = byte(v >> 1)
				pkt[at+4] = byte(v<<7) | 0x7e
				pkt[at+5] = 0
				at += 6
			}
			for ; at < packetSize-p.n; at++ {
				pkt[at] = 0xff
			}
		}
	}
	copy(pkt[at:], p.payload[:p.n])
	p.first, p.n = false, 0
	return p.m.emit(pkt)
}
