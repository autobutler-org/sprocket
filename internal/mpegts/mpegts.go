// Package mpegts reads and writes MPEG transport streams: .ts files, and the
// M2TS variant Blu-ray uses, whose packets carry a four byte timestamp in front.
//
// A transport stream has no header and no index. It is a run of 188 byte
// packets, each tagged with a PID; the program association table on PID 0
// names the program map table's PID, the program map table names the
// elementary streams and their types, and each stream's bytes arrive as PES
// packets scattered across the packets of its PID, with a 90 kHz presentation
// and decode timestamp at the front of each one. Everything this package
// reports is learned by reading packets, so every read is bounded by a cap it
// names.
//
// # How Parse is bounded
//
// Parse reads at most probeScanBytes from the front of the file and at most
// tailScanBytes from the end, 4 MiB each, and nothing in between, however long
// the file is.
//
// The front scan finds the program association table, then the program map
// table, then the first PES of each stream it reads, which is enough to name
// the codecs, read the video's sequence parameter set for its dimensions, and
// read the first ADTS header for an AAC stream's configuration. It keeps going
// until the video has run for two seconds, which is what the frame rate is
// measured over, or until the cap. The tail scan reads the PES timestamps of
// the last 4 MiB, which is where the duration's end comes from.
//
// When the front cap is hit first: a file with no program association table in
// it returns ErrNotTS, because a run of sync bytes with no tables is not a
// stream this package can name anything in; one with the table but no program
// map table, or with a video stream that shows no PES, or no parameter sets for
// an H.264 or HEVC one, returns ErrMalformed. An audio stream that shows no PES
// in the front scan is dropped rather than failing the file: a stream that
// starts late is not a broken one.
//
// # What the numbers mean
//
// Times are measured from the earliest presentation timestamp any stream shows
// in the front scan, which is the start time ffprobe reports. Duration runs from
// there to the end of the last video frame the tail scan shows: its
// presentation timestamp plus one frame. A file with no video uses its audio
// the same way. Timestamps are unwrapped across the 33 bit rollover as the
// value nearest that first one, which holds for any file shorter than 13 hours.
// A discontinuity between the front and the tail, where a splice or an encoder
// restart reset the clock, makes the estimate wrong, and nothing detects it:
// the answer is the naive difference, clamped at zero.
//
// FrameRate counts the video PES packets of the front scan over the span of
// their decode timestamps, which is exact for constant frame rate video,
// assumes one picture per PES as every muxer writes, and averages a variable
// rate over the first two seconds.
//
// A continuity counter that skips is noted in File.Gaps and otherwise ignored:
// a lost packet damages a frame, not the file.
package mpegts

import (
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	// ErrNotTS means the file is not a transport stream this package reads.
	ErrNotTS = errors.New("mpegts: not an MPEG transport stream")
	// ErrTruncated means the file ended before something it declared.
	ErrTruncated = errors.New("mpegts: file is truncated")
	// ErrMalformed means the stream is recognizably a transport stream and does
	// not hold together.
	ErrMalformed = errors.New("mpegts: malformed stream")
	// ErrFrameTooLarge means a frame runs past maxFrameBytes.
	ErrFrameTooLarge = errors.New("mpegts: frame is over the size cap")
	// ErrNoVideoTrack means the program declares no video stream this package
	// reads.
	ErrNoVideoTrack = errors.New("mpegts: no video stream")
	// ErrNoSyncSample means no keyframe was found where one was looked for.
	ErrNoSyncSample = errors.New("mpegts: no keyframe")
)

// Read limits.
const (
	// probeScanBytes caps the front scan, which finds the tables, names the
	// codecs, and measures the frame rate.
	probeScanBytes = 4 << 20
	// tailScanBytes caps the scan backward from the end for the last
	// timestamps.
	tailScanBytes = 4 << 20
	// configSearchBytes is how much of a PES the front scan looks at for a
	// sequence parameter set or an ADTS header. Parameter sets come before the
	// first slice of an access unit, so they are in its first few hundred
	// bytes whatever the picture size.
	configSearchBytes = 64 << 10
	// rateSpan is how long a stretch of video the front scan measures the frame
	// rate over before it stops early.
	rateSpan = 2 * clockRate
	// maxFrameBytes caps the one frame a keyframe read holds.
	maxFrameBytes = 32 << 20
)

// Stream is one elementary stream: what it carries and what the front scan
// learned about it.
type Stream struct {
	// PID is the packet identifier its packets carry.
	PID uint16
	// Codec is the stable short name, as the sprocket documentation lists them:
	// h264, hevc, aac, mp3, ac-3, ec-3, and the ffmpeg names mpeg1video,
	// mpeg2video, mpeg4, mp2, and aac_latm for the streams this package can
	// name and cannot carry anywhere.
	Codec string
	// Config is the codec configuration as the MP4 family stores it: an avcC or
	// hvcC record built from the parameter sets in the stream, or an AAC
	// AudioSpecificConfig built from the first ADTS header. It is nil for a
	// codec that needs none.
	Config []byte
	// Width and Height are the displayed size, from the sequence parameter set.
	Width, Height int
	// SampleRate and Channels describe an AAC stream.
	SampleRate, Channels int

	hevc bool
	// params are the parameter sets Config holds, each as a NAL unit. An
	// in-band copy of one of them is dropped from a sample on the way into the
	// MP4 family.
	params [][]byte

	// The front scan's working state and findings. Timestamps are unwrapped.
	configured bool
	inspecting bool
	head       []byte
	havePTS    bool
	minPTS     int64
	maxPTS     int64
	firstDTS   int64
	lastDTS    int64
	frames     int
	step       int64
	ccSeen     bool
	cc         uint8
}

// File is a parsed transport stream. It keeps the reader it was parsed from,
// so it stays valid only as long as that reader does.
type File struct {
	// Video and Audio are the first video and the first audio stream of the
	// first program. Either may be nil.
	Video, Audio *Stream
	// Gaps counts the continuity counter jumps the front scan saw.
	Gaps int

	r      io.ReaderAt
	size   int64
	stride int64
	pmtPID uint16
	// ref is the first timestamp seen, which every other is unwrapped against.
	ref     int64
	haveRef bool
	// origin is the earliest presentation timestamp, which times are measured
	// from, and end is where the duration stops.
	origin, end int64
}

// Parse reads the structure of a transport stream within the bounds the
// package documentation describes.
func Parse(r io.ReaderAt, size int64) (*File, error) {
	stride, err := detectStride(r, size)
	if err != nil {
		return nil, err
	}
	f := &File{r: r, size: size, stride: stride}
	frontEnd, err := f.scanFront()
	if err != nil {
		return nil, err
	}
	if err := f.scanTail(frontEnd); err != nil {
		return nil, err
	}
	f.measure()
	return f, nil
}

// streamOf returns the stream a PID carries, or nil for one this package is
// not reading.
func (f *File) streamOf(pid uint16) *Stream {
	switch {
	case f.Video != nil && f.Video.PID == pid:
		return f.Video
	case f.Audio != nil && f.Audio.PID == pid:
		return f.Audio
	}
	return nil
}

// unwrap places a raw timestamp on the file's unwrapped timeline.
func (f *File) unwrap(raw int64) int64 {
	if !f.haveRef {
		f.ref, f.haveRef = raw, true
	}
	return unwrapNear(raw, f.ref)
}

// scanFront reads the front of the file for the tables, the codecs, and the
// frame rate, and returns where it stopped.
func (f *File) scanFront() (int64, error) {
	rd := newReader(f.r, f.stride)
	rd.seek(0, min(f.size, probeScanBytes))

	var (
		pat, pmt       section
		havePAT, ready bool
	)
	for !ready || !f.satisfied() {
		p, ok, err := rd.next()
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		if !p.hasPayload {
			continue
		}
		switch {
		case p.pid == patPID && !havePAT:
			// A table that fails its CRC is skipped rather than fatal: the
			// next copy, a tenth of a second on, is as good.
			if sec, ok := pat.add(p); ok {
				if pid, err := parsePAT(sec); err == nil {
					f.pmtPID, havePAT = pid, true
				}
			}
		case havePAT && !ready && p.pid == f.pmtPID:
			if sec, ok := pmt.add(p); ok {
				if streams, err := parsePMT(sec); err == nil {
					f.choose(streams)
					ready = true
				}
			}
		default:
			if s := f.streamOf(p.pid); s != nil {
				f.observe(s, p)
			}
		}
	}
	for _, s := range []*Stream{f.Video, f.Audio} {
		if s != nil && s.inspecting {
			s.inspect()
		}
	}

	switch {
	case !havePAT:
		return 0, fmt.Errorf("%w: no program association table in the first %d bytes", ErrNotTS, int64(probeScanBytes))
	case !ready:
		return 0, fmt.Errorf("%w: a program association table and no program map table in the first %d bytes",
			ErrMalformed, int64(probeScanBytes))
	case f.Video == nil && f.Audio == nil:
		return 0, fmt.Errorf("%w: the program declares no video or audio stream this package reads", ErrMalformed)
	}
	if v := f.Video; v != nil {
		if !v.havePTS {
			return 0, fmt.Errorf("%w: video PID %#x shows no timestamped PES in the first %d bytes",
				ErrMalformed, v.PID, int64(probeScanBytes))
		}
		if !v.configured {
			return 0, fmt.Errorf("%w: video PID %#x shows no parameter sets in the first %d bytes",
				ErrMalformed, v.PID, int64(probeScanBytes))
		}
	}
	if a := f.Audio; a != nil && (!a.havePTS || !a.configured) {
		f.Audio = nil
	}
	return rd.pos, nil
}

// choose picks the first video and the first audio stream the program map
// table declares.
//
// ponytail: one program, and one stream of each kind. A multi-program
// broadcast capture, or a second audio language, is read as its first program's
// first streams; carrying the rest is the upgrade.
func (f *File) choose(streams []esInfo) {
	for _, es := range streams {
		s := &Stream{PID: es.pid, Codec: es.codec, hevc: es.codec == "hevc"}
		switch {
		case es.kind == kindVideo && f.Video == nil:
			f.Video = s
		case es.kind == kindAudio && f.Audio == nil:
			f.Audio = s
		}
	}
}

// satisfied reports whether the front scan has what it came for: every stream
// named and configured, and enough video to measure a frame rate over.
func (f *File) satisfied() bool {
	for _, s := range []*Stream{f.Video, f.Audio} {
		if s != nil && (!s.configured || s.frames < 2) {
			return false
		}
	}
	return f.Video == nil || f.Video.lastDTS-f.Video.firstDTS >= rateSpan
}

// observe takes one packet of a stream during the front scan.
func (f *File) observe(s *Stream, p packet) {
	if s.ccSeen && p.cc != (s.cc+1)&0x0f && p.cc != s.cc && !p.discontinuity {
		f.Gaps++
	}
	s.ccSeen, s.cc = true, p.cc

	es, h, err := esPayload(p)
	if err != nil {
		// A PES whose header does not parse is skipped, and so is its payload.
		s.inspecting = false
		return
	}
	if p.start {
		if s.inspecting {
			s.inspect()
		}
		if h.hasPTS {
			f.timestamp(s, h)
		}
		if !s.configured {
			s.inspecting, s.head = true, s.head[:0]
		}
	}
	if s.inspecting {
		s.head = append(s.head, es[:min(len(es), configSearchBytes-len(s.head))]...)
		if len(s.head) >= configSearchBytes {
			s.inspect()
		}
	}
}

// timestamp records one PES header's timestamps against its stream.
func (f *File) timestamp(s *Stream, h pesHeader) {
	pts := f.unwrap(h.pts)
	dts := pts
	if h.hasDTS {
		dts = f.unwrap(h.dts)
	}
	if !s.havePTS {
		s.havePTS, s.minPTS, s.maxPTS, s.firstDTS, s.lastDTS = true, pts, pts, dts, dts
	}
	s.frames++
	if delta := dts - s.lastDTS; delta > 0 && (s.step == 0 || delta < s.step) {
		s.step = delta
	}
	s.lastDTS = dts
	s.minPTS, s.maxPTS = min(s.minPTS, pts), max(s.maxPTS, pts)
}

// inspect looks at the front of the PES gathered in head for what the stream's
// codec needs: parameter sets for a video stream, a frame header for audio.
// A PES that does not have it leaves the stream unconfigured, and the next one
// is looked at.
func (s *Stream) inspect() {
	s.inspecting = false
	head := s.head
	switch s.Codec {
	case "h264", "hevc":
		s.configured = s.configureVideo(head) == nil
	case "mpeg1video", "mpeg2video":
		// A sequence header is the start code 0xB3 and then twelve bits each
		// of width and height. ISO/IEC 13818-2 6.2.2.1.
		if at := indexStartCode(head, 0xb3); at >= 0 && at+7 <= len(head) {
			b := head[at+4:]
			s.Width, s.Height = int(b[0])<<4|int(b[1]>>4), int(b[1]&0x0f)<<8|int(b[2])
			s.configured = true
		}
	case "aac":
		if h, err := parseADTS(head); err == nil {
			s.SampleRate, s.Channels, s.Config = adtsSampleRates[h.rate], h.channels, h.audioSpecificConfig()
			if h.channels == 7 {
				s.Channels = 8
			}
			s.configured = true
		}
	case "private":
		// A private stream with nothing declared about it is named by its
		// first frame, and stays unconfigured, which drops it, when the frame
		// is not one this package knows.
		if _, err := parseADTS(head); err == nil {
			s.Codec = "aac"
			s.inspect()
		} else if len(head) >= 6 && head[0] == 0x0b && head[1] == 0x77 {
			// An AC-3 sync frame; a bitstream ID above 10 is E-AC-3.
			// ATSC A/52 5.3 and Annex E.
			s.Codec, s.configured = "ac-3", true
			if head[5]>>3 > 10 {
				s.Codec = "ec-3"
			}
		}
	case "mp3":
		switch mpegAudioLayer(head) {
		case 1:
			s.Codec, s.configured = "mp1", true
		case 2:
			s.Codec, s.configured = "mp2", true
		case 3:
			s.configured = true
		}
	default:
		s.configured = true
	}
}

// configureVideo builds the configuration record out of the parameter sets at
// the front of an access unit and reads the picture size out of its sequence
// parameter set.
func (s *Stream) configureVideo(head []byte) error {
	m := measure{hevc: s.hevc, collect: true}
	if err := m.feed(head); err != nil {
		return err
	}
	m.end()
	sets := paramSets(m.found, s.hevc)

	var (
		pic picture
		err error
	)
	if s.hevc {
		if len(sets[hevcVPS]) == 0 || len(sets[hevcSPS]) == 0 || len(sets[hevcPPS]) == 0 {
			return fmt.Errorf("%w: no VPS, SPS, and PPS", ErrMalformed)
		}
		if pic, err = parseHEVCSPS(sets[hevcSPS][0]); err != nil {
			return err
		}
		if s.Config, err = hvcC(sets, pic); err != nil {
			return err
		}
	} else {
		if len(sets[h264SPS]) == 0 || len(sets[h264PPS]) == 0 {
			return fmt.Errorf("%w: no SPS and PPS", ErrMalformed)
		}
		if pic, err = parseH264SPS(sets[h264SPS][0]); err != nil {
			return err
		}
		s.Config = avcC(sets[h264SPS], sets[h264PPS], pic)
	}
	s.Width, s.Height = pic.width, pic.height
	for _, list := range sets {
		s.params = append(s.params, list...)
	}
	return nil
}

// indexStartCode finds a start code followed by a given byte.
func indexStartCode(b []byte, code byte) int {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 && b[i+3] == code {
			return i
		}
	}
	return -1
}

// scanTail reads the timestamps of the last tailScanBytes of the file. It
// starts no earlier than where the front scan stopped, so a small file is read
// once.
func (f *File) scanTail(frontEnd int64) error {
	from := f.size - tailScanBytes
	if rem := from % f.stride; rem != 0 {
		from += f.stride - rem
	}
	from = max(from, frontEnd)

	rd := newReader(f.r, f.stride)
	rd.seek(from, f.size)
	for {
		p, ok, err := rd.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		s := f.streamOf(p.pid)
		if s == nil || !p.start || !p.hasPayload {
			continue
		}
		if h, err := parsePESHeader(p.payload); err == nil && h.hasPTS {
			s.maxPTS = max(s.maxPTS, f.unwrap(h.pts))
		}
	}
}

// measure works out the origin and the end of the file's timeline, as the
// package documentation describes.
func (f *File) measure() {
	primary := f.Video
	if primary == nil {
		primary = f.Audio
	}
	f.origin = primary.minPTS
	if f.Audio != nil {
		f.origin = min(f.origin, f.Audio.minPTS)
	}
	f.end = max(primary.maxPTS+primary.step, f.origin)
}

// Duration is how long the file runs, as the package documentation describes.
func (f *File) Duration() time.Duration { return ticksToDuration(f.end - f.origin) }

// FrameRate is the video's frame rate as the front scan measured it, or 0 with
// no video or too little of it to measure.
func (f *File) FrameRate() float64 {
	v := f.Video
	if v == nil || v.frames < 2 || v.lastDTS <= v.firstDTS {
		return 0
	}
	return float64(v.frames-1) * clockRate / float64(v.lastDTS-v.firstDTS)
}

// ticksToDuration converts 90 kHz ticks to wall time. A tick is 100000/9
// nanoseconds, and multiplying first keeps the conversion exact to the
// nanosecond without overflowing for thirty years of ticks.
func ticksToDuration(ticks int64) time.Duration { return time.Duration(ticks * 100000 / 9) }

// durationToTicks converts wall time to 90 kHz ticks, rounding to nearest, so
// that a time which came from ticks, and lost a fraction of a nanosecond on
// the way, goes back to the tick it came from.
func durationToTicks(d time.Duration) int64 {
	n := int64(d)
	whole, rest := n/100000*9, n%100000*9
	if rest >= 0 {
		return whole + (rest+50000)/100000
	}
	return whole + (rest-50000)/100000
}

// Source returns the reader the file was parsed from and its size, for a
// caller that copies the stream whole.
func (f *File) Source() (io.ReaderAt, int64) { return f.r, f.size }
