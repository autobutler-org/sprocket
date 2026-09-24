package mpegts

import (
	"fmt"
	"io"
	"math"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// WriteFragmented writes a transport stream into w as a fragmented file of an
// ISOBMFF target: mp4, m4v, or 3gp. Nothing is re-encoded.
//
// It is fragmented for the reason a Matroska source is: nothing up front says
// how many frames the stream holds or how large each one is, so a header-first
// plain file would mean buffering every sample's size or reading the file
// twice. A fragment starts at each video keyframe, and is capped by the
// writer's sample limit, so building one costs a GOP's sample descriptors.
//
// A sample is not a byte range here. A video access unit is scattered across
// packets interleaved with every other stream and is Annex B, where the MP4
// family wants length-prefixed NAL units, so each sample carries a Payload that
// walks its packets again and rebuilds it on the way out, with the access unit
// delimiter and any in-band copy of a parameter set in the configuration
// record dropped. Its size has to be in the moof before its bytes, so the walk
// that finds the samples measures each access unit as it goes by, and the
// Payload measures it again, which is bounded by one access unit's NAL units,
// before writing it: an access unit is read three times in all. An AAC stream's
// ADTS headers are stripped the same way, frame by frame, and the
// AudioSpecificConfig comes from the first of them.
//
// Timestamps are unwrapped across the 33 bit rollover against the previous
// one, so a file of any length comes through. Video decode times are the DTS,
// and presentation offsets the PTS less the DTS; an edit list shifts the track
// so that its frames come out at the times Probe and Thumbnail report for the
// source. Audio decode times start at the first frame's PTS and advance by the
// frames' own lengths, which is exact for a stream with no gaps; a
// discontinuity in the source is carried as if it were not there.
//
// Only the first video and the first audio stream are carried over. A codec
// the target cannot hold returns isobmff.ErrIncompatibleCodec, and one with no
// sample entry in the writer returns isobmff.ErrUnsupportedSource, before any
// of the stream is read.
func WriteFragmented(w io.Writer, f *File, target isobmff.Target) error {
	s := newFragSource(f)
	return isobmff.WriteFragmented(w, f.r, target, f.Duration(), s.tracks, s.next)
}

// fragSource walks the file once, packet by packet, and turns it into the
// sample stream isobmff.WriteFragmented pulls on.
type fragSource struct {
	f      *File
	tracks []isobmff.FragTrack
	rd     *reader
	clock  int64 // the last timestamp unwrapped, which the next is unwrapped against
	video  *fragVideo
	audio  *fragAudio
	queue  []isobmff.FragSample
	at     int
	done   bool

	// The Payload functions share one reader and one measuring pass. The
	// writer calls them one at a time, after the walk has moved on, so nothing
	// they use is the walk's own.
	again     *reader
	remeasure measure
}

// fragVideo is the video stream's state in the walk.
type fragVideo struct {
	s        *Stream
	id       uint32
	base     int64 // the unwrapped timestamp decode time zero stands for
	open     bool
	slot     int64
	pts, dts int64
	random   bool
	bytes    int64
	m        measure
	held     isobmff.FragSample
	haveHeld bool
}

// fragAudio is the AAC stream's state in the walk: an ADTS parser that runs
// across packet and PES boundaries, since a frame may straddle either.
type fragAudio struct {
	s       *Stream
	id      uint32
	synced  bool // a PES start has been seen
	hdr     [9]byte
	hlen    int
	need    int
	frame   adtsHeader
	left    int // raw bytes of the current frame still to pass
	slot    int64
	skip    int
	pending bool // the PES just started carries a PTS for its first frame
	pesPTS  int64
	pts     int64 // the PTS of the frame being read, when hasPTS
	hasPTS  bool
	cursor  int64 // the next frame's decode time, in samples
	started bool
}

func newFragSource(f *File) *fragSource {
	s := &fragSource{
		f: f, rd: newReader(f.r, f.stride), again: newReader(f.r, f.stride),
		clock: f.ref, queue: make([]isobmff.FragSample, 0, 16),
	}
	s.rd.seek(0, f.size)
	if v := f.Video; v != nil {
		base := min(v.firstDTS, f.origin)
		s.video = &fragVideo{s: v, id: uint32(len(s.tracks) + 1), base: base, m: measure{hevc: v.hevc, params: v.params}}
		s.tracks = append(s.tracks, isobmff.FragTrack{
			ID: s.video.id, Handler: "vide", Timescale: clockRate, Codec: v.Codec, Config: v.Config,
			Width: uint32(v.Width), Height: uint32(v.Height), EditDelay: uint64(f.origin - base),
		})
		s.remeasure = measure{hevc: v.hevc, params: v.params, record: true}
	}
	if a := f.Audio; a != nil {
		s.audio = &fragAudio{s: a, id: uint32(len(s.tracks) + 1)}
		rate := a.SampleRate
		if rate <= 0 {
			rate = clockRate
		}
		s.tracks = append(s.tracks, isobmff.FragTrack{
			ID: s.audio.id, Handler: "soun", Timescale: uint32(rate), Codec: a.Codec, Config: a.Config,
			Channels: uint16(a.Channels), SampleRate: uint32(rate),
		})
	}
	return s
}

// unwrap places a raw timestamp on the walk's timeline, nearest the last one.
func (s *fragSource) unwrap(raw int64) int64 {
	s.clock = unwrapNear(raw, s.clock)
	return s.clock
}

// next hands out the samples the walk has finished, walking on when it runs
// out. It is the isobmff.NextFragSample the writer pulls on.
func (s *fragSource) next() (isobmff.FragSample, bool, error) {
	for s.at >= len(s.queue) {
		if s.done {
			return isobmff.FragSample{}, false, nil
		}
		s.queue, s.at = s.queue[:0], 0
		if err := s.step(); err != nil {
			return isobmff.FragSample{}, false, err
		}
	}
	out := s.queue[s.at]
	s.at++
	return out, true, nil
}

// step takes one packet, or finishes the streams at the end of the file.
func (s *fragSource) step() error {
	p, ok, err := s.rd.next()
	if err != nil {
		return err
	}
	if !ok {
		s.done = true
		if s.video != nil {
			s.finishVideo()
			if v := s.video; v.haveHeld {
				s.queue = append(s.queue, v.held)
			}
		}
		return nil
	}
	if !p.hasPayload {
		return nil
	}
	switch {
	case s.video != nil && p.pid == s.video.s.PID:
		return s.takeVideo(p)
	case s.audio != nil && p.pid == s.audio.s.PID && s.audio.s.Codec == "aac":
		return s.takeAudio(p)
	}
	return nil
}

func (s *fragSource) takeVideo(p packet) error {
	v := s.video
	es, h, err := esPayload(p)
	if err != nil {
		return err
	}
	if p.start {
		s.finishVideo()
		v.open, v.slot, v.bytes, v.random = true, s.rd.slotOf(p), 0, p.randomAccess
		v.m.reset()
		if h.hasPTS {
			v.pts = s.unwrap(h.pts)
			v.dts = v.pts
			if h.hasDTS {
				v.dts = s.unwrap(h.dts)
			}
		} else {
			// A PES with no timestamp is one frame on from the last.
			v.dts += v.s.step
			v.pts = v.dts
		}
	}
	if !v.open {
		return nil
	}
	v.bytes += int64(len(es))
	return v.m.feed(es)
}

// finishVideo turns the PES the walk has been measuring into a sample. The
// sample before it is released at the same time, now that its duration, the
// step to this one's decode time, is known.
func (s *fragSource) finishVideo() {
	v := s.video
	if !v.open {
		return
	}
	v.open = false
	v.m.end()
	sync := v.random || v.m.random
	sample := isobmff.FragSample{
		Track:       v.id,
		Decode:      uint64(max(v.dts-v.base, 0)),
		Composition: int32(min(max(v.pts-v.dts, math.MinInt32), math.MaxInt32)),
		Sync:        sync,
		Fragment:    sync,
		Size:        v.m.size,
		Payload:     s.videoPayload(v.slot, v.bytes, v.m.size),
		// One frame, until the next sample says otherwise; the last sample
		// of the stream keeps the one before it.
		Duration: uint32(min(max(v.s.step, 0), math.MaxUint32)),
	}
	if v.haveHeld {
		v.held.Duration = uint32(min(max(int64(sample.Decode)-int64(v.held.Decode), 0), math.MaxUint32))
		s.queue = append(s.queue, v.held)
		sample.Duration = v.held.Duration
	}
	v.held, v.haveHeld = sample, true
}

func (s *fragSource) takeAudio(p packet) error {
	a := s.audio
	es, h, err := esPayload(p)
	if err != nil {
		return err
	}
	if p.start {
		a.synced = true
		if h.hasPTS {
			a.pending, a.pesPTS = true, s.unwrap(h.pts)
		}
	}
	if !a.synced {
		return nil
	}
	slot := s.rd.slotOf(p)
	for i := 0; i < len(es); {
		if a.left > 0 {
			take := min(a.left, len(es)-i)
			i += take
			a.left -= take
			if a.left == 0 {
				s.finishAudio()
			}
			continue
		}
		if a.hlen == 0 {
			// The PTS in a PES header belongs to the first frame that starts
			// in it. ISO/IEC 13818-1 2.4.3.7.
			a.hasPTS, a.pts, a.pending = a.pending, a.pesPTS, false
		}
		a.hdr[a.hlen] = es[i]
		a.hlen++
		i++
		if a.hlen == 7 {
			frame, err := parseADTS(a.hdr[:7])
			if err != nil {
				// Not a header here: slide along by a byte and look again.
				copy(a.hdr[:], a.hdr[1:7])
				a.hlen = 6
				continue
			}
			a.frame, a.need = frame, frame.size
		}
		if a.hlen >= 7 && a.hlen == a.need {
			a.left, a.slot, a.skip, a.hlen = a.frame.frame-a.frame.size, slot, i, 0
		}
	}
	return nil
}

// finishAudio turns the ADTS frame just passed into a sample.
func (s *fragSource) finishAudio() {
	a := s.audio
	// ponytail: decode times advance by frame lengths from the first PTS and
	// never look at a later one, so a gap or a clock reset in the audio is
	// carried as if it were not there. Resyncing the cursor to a PTS that
	// disagrees with it by more than a frame is the upgrade.
	if !a.started {
		if !a.hasPTS {
			return
		}
		a.cursor = max(a.pts-s.f.origin, 0) * int64(a.s.SampleRate) / clockRate
		a.started = true
	}
	duration := int64(a.frame.blocks * aacFrameSamples)
	size := int64(a.frame.frame - a.frame.size)
	s.queue = append(s.queue, isobmff.FragSample{
		Track:    a.id,
		Decode:   uint64(a.cursor),
		Duration: uint32(duration),
		Sync:     true,
		Size:     size,
		Payload:  s.audioPayload(a.slot, a.skip, size),
	})
	a.cursor += duration
}

// audioPayload writes one ADTS frame's raw data block, which starts skip bytes
// into the stream bytes of the packet at slot.
func (s *fragSource) audioPayload(slot int64, skip int, size int64) func(io.Writer) error {
	pid := s.audio.s.PID
	return func(w io.Writer) error {
		return s.walk(pid, slot, skip, size, func(b []byte) error {
			_, err := w.Write(b)
			return err
		})
	}
}

// videoPayload writes one access unit, bytes of stream data from the PES that
// starts at slot, as length-prefixed NAL units. It measures the unit again to
// learn each NAL unit's length before the unit's bytes go past, and refuses an
// access unit that measures differently from the first time.
//
// ponytail: a PES is taken to hold one access unit, which is what every muxer
// writes for video, and each one is read three times. Keeping each unit's NAL
// lengths from the walk would save one read at the cost of holding a
// fragment's worth of them.
func (s *fragSource) videoPayload(slot, bytes, size int64) func(io.Writer) error {
	pid := s.video.s.PID
	return func(w io.Writer) error {
		m := &s.remeasure
		m.reset()
		if err := s.walk(pid, slot, 0, bytes, m.feed); err != nil {
			return err
		}
		m.end()
		if m.size != size {
			return fmt.Errorf("%w: the access unit at %d measured %d bytes and then %d", ErrMalformed, slot, size, m.size)
		}
		l := lengthPrefixer{units: m.units, w: func(b []byte) error {
			_, err := w.Write(b)
			return err
		}}
		return s.walk(pid, slot, 0, bytes, l.feed)
	}
}

// walk hands fn n bytes of one stream's data, from skip bytes into the packet
// at slot, passing over every other stream's packets and every PES header on
// the way.
func (s *fragSource) walk(pid uint16, slot int64, skip int, n int64, fn func([]byte) error) error {
	rd := s.again
	rd.seek(slot, s.f.size)
	for first := true; n > 0; {
		p, ok, err := rd.next()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: the stream ends %d bytes short of a sample", ErrTruncated, n)
		}
		if p.pid != pid || !p.hasPayload {
			continue
		}
		es, _, err := esPayload(p)
		if err != nil {
			return err
		}
		if first {
			es, first = es[min(skip, len(es)):], false
		}
		take := min(int64(len(es)), n)
		if err := fn(es[:take]); err != nil {
			return err
		}
		n -= take
	}
	return nil
}
