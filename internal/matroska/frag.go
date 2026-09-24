package matroska

import (
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// WriteFragmented writes a Matroska source into w as a fragmented file of an
// ISOBMFF target: mp4, m4v, or 3gp. Nothing is re-encoded and no frame is
// looked at.
//
// The output is fragmented rather than indexed because a Matroska file states
// its frames cluster by cluster. Nothing before the media says how many frames
// a track holds or how large each one is, so a plain MP4 written in one pass
// would mean buffering every sample's size and offset, unbounded in the length
// of the input. One moof and one mdat per source cluster costs a cluster's
// block headers, which is the same bound the Matroska writer already works to.
//
// cut is the window to keep, which TrimSpan picks; a nil cut writes the file
// whole.
//
// Only video and audio tracks are carried over. A laced block, which packs
// several frames into one, returns ErrUnsupportedSource: an ISOBMFF sample is
// one frame, so carrying it across would mean taking the block apart. A source
// with no video or audio track returns ErrUnsupportedSource too, a codec the
// target cannot hold returns ErrIncompatibleCodec, and a codec with no ISOBMFF
// sample entry here returns ErrUnsupportedSource.
func WriteFragmented(w io.Writer, src *File, target isobmff.Target, cut *TrimSpan) error {
	span := whole(src.Tracks)
	if cut != nil {
		span = *cut
	}
	source, err := newFragSource(src, span)
	if err != nil {
		return err
	}
	return isobmff.WriteFragmented(w, src.r, target, source.duration, source.tracks, source.next)
}

// fragTrack is the decode timeline this package derives for one track, which is
// the whole of what Matroska does not state.
//
// # Decode times from presentation times
//
// Matroska stores blocks in decode order and stamps each one with the time it
// is shown. An ISOBMFF trun wants the other pair: a decode time, a duration on
// the decode timeline, and an offset to the presentation. Where a codec
// reorders frames the two differ, and the difference has to be derived.
//
// Within one fragment the block timestamps are sorted. Decode order is the
// order the blocks are stored in, and the frames of a group are shown in some
// permutation of the same set of times, so the i-th block in storage order is
// decoded at the i-th smallest timestamp. Its duration is the step to the next
// one, and the last block of a fragment steps to the first timestamp the next
// fragment holds for the same track, which is one cluster of lookahead.
//
// That leaves the lead: a reordered stream has to be decoded ahead of being
// shown, so the first blocks would want a decode time before zero, which
// ISOBMFF cannot express. The presentation is pushed forward by that lead
// instead, and an edit list takes it back off, which is what every muxer does
// and what keeps the times this library reports out of the output equal to the
// ones it reports out of the source. Signed offsets with no edit list would
// state the same presentation, but ffmpeg reads a negative offset by shifting
// the whole track later by the deepest one, so the output would play late by
// that much there. The lead is the largest gap between a block's sorted
// position and its own timestamp, measured over the first fragment the track
// appears in and held for the rest of the file: a later fragment that reorders
// more deeply drives a composition offset negative, and the signed form of the
// trun carries that rather than the whole track's timeline moving underneath
// the edit list.
type fragTrack struct {
	number uint64
	// delay is the lead, in output ticks, that the edit list takes back off.
	delay int64
	// cursor is the decode time the next fragment continues from.
	cursor int64
	// fallback is the nominal frame duration in output ticks, from
	// DefaultDuration, used for the last block of the file.
	fallback int64
	// last is the previous duration, which stands in for the final block's when
	// the track declares no nominal one.
	last int64

	// pos, pts, and sorted are the current fragment's working space, reused
	// across fragments so that a long file costs one fragment's worth.
	pos    []int
	pts    []int64
	sorted []int64
}

// rawBlock is one source block as the fragment builder holds it: no payload,
// and its timestamp already rebased onto the output's timeline.
type rawBlock struct {
	track  uint64
	ticks  int64
	sync   bool
	offset int64
	size   int64
}

// fragSource turns a Matroska file into the sample stream
// isobmff.WriteFragmented is driven by. It holds two clusters of block
// descriptors: the one being handed out, and the one after it, whose first
// timestamp per track gives the fragment's last block its duration.
type fragSource struct {
	src      *File
	span     TrimSpan
	tracks   []isobmff.FragTrack
	state    map[uint64]*fragTrack
	duration time.Duration

	// nanosPerTick is one output tick in nanoseconds, which every source
	// timestamp is converted through.
	nanosPerTick int64

	cursor         clusterCursor
	current, ahead []rawBlock
	pending        []isobmff.FragSample
	at             int
	drained        bool
}

// fragTimescale is the media timescale a fragmented output falls back to when
// the source's own does not divide a second. A source measured in
// milliseconds, which every real file is, keeps its own ticks instead and
// nothing is rescaled at all.
const fragTimescale = 1000

func newFragSource(src *File, span TrimSpan) (*fragSource, error) {
	timescale := uint64(fragTimescale)
	if scale := src.TimestampScale; scale > 0 && scale <= uint64(time.Second) && uint64(time.Second)%scale == 0 {
		timescale = uint64(time.Second) / scale
	}
	s := &fragSource{
		src: src, span: span, state: map[uint64]*fragTrack{},
		nanosPerTick: int64(uint64(time.Second) / timescale),
		cursor:       src.newClusterCursor(),
	}

	for _, t := range src.Tracks {
		if _, kept := span.First[t.Number]; !kept {
			continue
		}
		if t.Number > math.MaxUint32 {
			return nil, fmt.Errorf("%w: track number %d does not fit an ISOBMFF track ID",
				isobmff.ErrUnsupportedSource, t.Number)
		}
		handler := "vide"
		if t.Type == trackAudio {
			handler = "soun"
		}
		s.tracks = append(s.tracks, isobmff.FragTrack{
			ID: uint32(t.Number), Handler: handler, Timescale: uint32(timescale),
			Codec: t.Codec, Config: t.CodecPrivate,
			Width: uint32(max(t.Width, 0)), Height: uint32(max(t.Height, 0)),
			Channels: uint16(min(t.Channels, math.MaxUint16)), SampleRate: uint32(min(max(t.SampleRate, 0), math.MaxUint32)),
		})
		// Rounded to nearest rather than truncated: a nominal 41.67 millisecond
		// frame truncates to 41, which leaves the track a tick short of where
		// its last sample ends and the measured frame rate a hair high.
		s.state[t.Number] = &fragTrack{
			number:   t.Number,
			fallback: (int64(t.DefaultDuration) + s.nanosPerTick/2) / s.nanosPerTick,
		}
	}
	if len(s.tracks) == 0 {
		return nil, fmt.Errorf("%w: it carries no video or audio track", isobmff.ErrUnsupportedSource)
	}

	if err := s.prime(); err != nil {
		return nil, err
	}
	s.duration = s.outputDuration()
	return s, nil
}

// tick converts a source timestamp of a track, rebased as TrimSpan.rebase
// describes, onto the output's media timeline. A source whose scale divides a
// second, which every real file has, comes through unchanged.
func (s *fragSource) tick(track uint64, ticks int64) int64 {
	return s.span.rebase(track, ticks) * int64(s.src.TimestampScale) / s.nanosPerTick
}

// outputDuration is how long the output runs. A whole-file write keeps the
// duration the source declares, so the output probes to what the input probed
// to; a cut states the window it kept.
func (s *fragSource) outputDuration() time.Duration {
	declared := s.src.Duration() - s.src.ticks(float64(s.span.Origin))
	if s.span.Last == math.MaxInt64 {
		return max(declared, 0)
	}
	cut := s.src.ticks(float64(s.span.Last - s.span.Origin))
	if declared > 0 {
		cut = min(cut, declared)
	}
	return max(cut, 0)
}

// prime reads the first fragment and measures each track's decode lead from it,
// which the edit lists in the movie header need before any sample goes out.
func (s *fragSource) prime() error {
	blocks, ok, err := s.readCluster(s.current)
	if err != nil {
		return err
	}
	s.current, s.drained = blocks, !ok
	if !ok {
		return nil
	}

	s.group(s.current)
	for i, t := range s.tracks {
		state := s.state[uint64(t.ID)]
		var lead int64
		for at := range state.pos {
			lead = max(lead, state.sorted[at]-state.pts[at])
		}
		// The lead is a whole number of frames, and a source that stamps its
		// blocks in milliseconds rounds each one on its own, so the measured
		// gap lands a tick either side of the frame it really is. Rounding up
		// to the nominal frame closes that, and costs at most one frame of
		// lead, which the edit list takes back off anyway.
		if state.fallback > 0 {
			lead = (lead + state.fallback - 1) / state.fallback * state.fallback
		}
		s.tracks[i].EditDelay, state.delay = uint64(lead), lead
	}
	return nil
}

// readCluster fills dst with the blocks of the next cluster that holds any,
// reporting false once the segment is spent. Clusters the cut leaves empty are
// stepped over here rather than becoming empty fragments.
func (s *fragSource) readCluster(dst []rawBlock) ([]rawBlock, bool, error) {
	for {
		payload, ok, err := s.cursor.next()
		if err != nil {
			return dst[:0], false, err
		}
		if !ok {
			return dst[:0], false, nil
		}

		dst = dst[:0]
		if err := s.src.eachBlock(payload, func(b block) error {
			if _, kept := s.state[b.track]; !kept || !s.span.keeps(b.track, b.ticks) {
				return nil
			}
			if b.frames != 1 {
				return fmt.Errorf("%w: a block of %d laced frames, which an ISOBMFF sample cannot hold",
					isobmff.ErrUnsupportedSource, b.frames)
			}
			if len(dst) >= maxClusterBlocks {
				return fmt.Errorf("%w: a cluster of over %d blocks", ErrMalformed, maxClusterBlocks)
			}
			dst = append(dst, rawBlock{
				track: b.track, ticks: s.tick(b.track, b.ticks), sync: b.keyframe,
				offset: b.first.start, size: b.first.size,
			})
			return nil
		}); err != nil {
			return dst[:0], false, err
		}
		if len(dst) > 0 {
			return dst, true, nil
		}
	}
}

// next hands out the samples of the current fragment and reads the next one
// when it runs out. It is the isobmff.NextFragSample the writer pulls on.
func (s *fragSource) next() (isobmff.FragSample, bool, error) {
	for s.at >= len(s.pending) {
		if len(s.current) == 0 {
			return isobmff.FragSample{}, false, nil
		}
		if err := s.fill(); err != nil {
			return isobmff.FragSample{}, false, err
		}
	}
	out := s.pending[s.at]
	s.at++
	return out, true, nil
}

// fill turns the cluster in hand into samples, reading the one after it first
// so that its last block of each track has a duration to step to.
func (s *fragSource) fill() error {
	if !s.drained {
		blocks, ok, err := s.readCluster(s.ahead)
		if err != nil {
			return err
		}
		s.ahead, s.drained = blocks, !ok
	} else {
		s.ahead = s.ahead[:0]
	}

	s.emit(s.current, s.ahead)
	s.current, s.ahead = s.ahead, s.current[:0]
	return nil
}

// group sorts one fragment's blocks by track, which is what the decode
// timeline is derived from.
func (s *fragSource) group(blocks []rawBlock) {
	for _, t := range s.state {
		t.pos, t.pts = t.pos[:0], t.pts[:0]
	}
	for i, b := range blocks {
		t := s.state[b.track]
		t.pos, t.pts = append(t.pos, i), append(t.pts, b.ticks)
	}
	for _, t := range s.state {
		t.sorted = append(t.sorted[:0], t.pts...)
		slices.Sort(t.sorted)
	}
}

// emit turns one fragment's blocks into samples, in the order the source stores
// them, deriving each track's decode timeline as fragTrack describes.
func (s *fragSource) emit(blocks, ahead []rawBlock) {
	s.group(blocks)
	for len(s.pending) < len(blocks) {
		s.pending = append(s.pending, isobmff.FragSample{})
	}
	s.pending, s.at = s.pending[:len(blocks)], 0
	if len(blocks) == 0 {
		return
	}

	for _, t := range s.state {
		n := len(t.pos)
		if n == 0 {
			continue
		}
		// The next fragment's earliest timestamp for this track is where the
		// last block of this one stops decoding.
		next := int64(-1)
		for _, b := range ahead {
			if b.track == t.number && (next < 0 || b.ticks < next) {
				next = b.ticks
			}
		}

		for i := range n {
			decode := max(t.sorted[i], t.cursor)
			var end int64
			switch {
			case i+1 < n:
				end = max(t.sorted[i+1], decode)
			case next >= 0:
				end = max(next, decode)
			case t.fallback > 0:
				end = decode + t.fallback
			default:
				end = decode + t.last
			}
			t.last, t.cursor = end-decode, end

			block := blocks[t.pos[i]]
			s.pending[t.pos[i]] = isobmff.FragSample{
				Track:       uint32(t.number),
				Decode:      uint64(max(decode, 0)),
				Duration:    uint32(min(max(end-decode, 0), math.MaxUint32)),
				Composition: int32(min(max(block.ticks+t.delay-decode, math.MinInt32), math.MaxInt32)),
				Sync:        block.sync,
				Offset:      block.offset,
				Size:        block.size,
			}
		}
	}
	s.pending[0].Fragment = true
}
