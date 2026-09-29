package matroska

import (
	"fmt"
	"math"
	"time"
)

// TrimSpan is the window a cut keeps, on the file's own timestamp scale. It is
// what both writers take in place of the sample ranges the ISOBMFF side uses,
// because a Matroska file has no sample index to name a range of.
type TrimSpan struct {
	// First is the timestamp of the first block each track keeps, keyed by
	// TrackNumber. A track with no entry keeps nothing.
	First map[uint64]int64
	// Last is the timestamp of the last block any track keeps. A block at
	// exactly First is kept whatever this says, so a cut always holds a frame.
	Last int64
	// Origin is where the cut is shown from: the requested start where Lead is
	// set, and otherwise the video keyframe it snapped to, or the requested
	// start in a file with no video. The output's duration is measured from it.
	Origin int64
	// Lead is how far before Origin the video's keyframe sits: the lead-in the
	// cut keeps so the frames after it decode, and hides. Zero hides nothing.
	Lead int64
}

// rebase moves a kept block's timestamp onto the output's timeline. Each track
// is rebased onto its own first block, so every track of a cut begins at zero,
// which is what the ISOBMFF side does by cutting each sample table at its first
// kept sample. An audio track that began on the frame straddling the video's
// first therefore sits up to one frame later than it did in the source, the
// same residual error the package documentation describes for the MP4 family.
// A track with no block at or before the anchor is rebased onto the anchor, so
// it begins as late after zero as it began after the video in the source. A
// whole-file span rebases nothing.
func (s TrimSpan) rebase(track uint64, ticks int64) int64 {
	if first := s.First[track]; first != math.MinInt64 {
		return ticks - first
	}
	return ticks
}

// keeps reports whether a block of a track belongs in the cut.
func (s TrimSpan) keeps(track uint64, ticks int64) bool {
	first, ok := s.First[track]
	if !ok {
		return false
	}
	return ticks == first || (ticks > first && ticks <= s.Last)
}

// lead is how far a track's first kept block sits before Origin, in the
// source's ticks, which is what a writer hides: zero unless the cut has a Lead.
func (s TrimSpan) lead(track uint64) int64 {
	if first := s.First[track]; s.Lead > 0 && first != math.MinInt64 {
		return max(s.Origin-first, 0)
	}
	return 0
}

// whole is the span that keeps every block of every track, which is what a
// remux asks for.
func whole(tracks []*Track) TrimSpan {
	cut := TrimSpan{First: make(map[uint64]int64, len(tracks)), Last: math.MaxInt64}
	for _, t := range tracks {
		if t.Type == trackVideo || t.Type == trackAudio {
			cut.First[t.Number] = math.MinInt64
		}
	}
	return cut
}

// TrimSpan picks the window a cut between start and end keeps, and reports the
// presentation time the cut is shown from.
//
// The video begins on its keyframe at or before start, for the reason the
// ISOBMFF side does: a cut cannot begin mid-GOP without re-encoding. A start
// past the end of the file takes the last keyframe, a file whose first keyframe
// comes later snaps forward to it, and a negative start reads as zero. A file
// with no video track anchors on the requested time itself, since there is no
// keyframe grid to land on.
//
// The blocks between that keyframe and start are the cut's lead-in, which the
// span's Lead measures: Origin is start, the writers hide the lead-in behind a
// CodecDelay or an edit list, and the reported time is start. Where there is
// nothing to hide, a start on a keyframe, past the last video block, or at or
// after end, Origin is the keyframe and so is the reported time.
//
// Every other track begins at its own last block at or before Origin, so an
// audio track carries up to one frame of sound from before it rather than
// starting late. Finding those costs one walk of the cluster headers, which is
// what a file with no sample table leaves.
//
// The end takes the last block at or before it, with no snapping constraint,
// and an end at or before the start keeps the single block each track began on.
func (f *File) TrimSpan(start, end time.Duration) (TrimSpan, time.Duration, error) {
	start = max(start, 0)
	anchor, err := f.trimAnchor(start)
	if err != nil {
		return TrimSpan{}, 0, err
	}

	var (
		video = f.VideoTrack()
		want  = f.durationTicks(start)
		// Each track's last block at or before the keyframe and at or before
		// the start, since which one it begins on depends on whether the start
		// turns out to be inside the file, and the walk is what says so.
		atAnchor  = map[uint64]int64{}
		atStart   = map[uint64]int64{}
		lastVideo = int64(math.MinInt64)
	)
	for _, t := range f.Tracks {
		if t.Type == trackVideo || t.Type == trackAudio {
			atAnchor[t.Number], atStart[t.Number] = math.MinInt64, math.MinInt64
		}
	}
	if err := f.eachCluster(func(payload span, _ int64) error {
		return f.eachBlock(payload, func(b block) error {
			if at, ok := atAnchor[b.track]; ok && b.ticks <= anchor && b.ticks > at {
				atAnchor[b.track] = b.ticks
			}
			if at, ok := atStart[b.track]; ok && b.ticks <= want && b.ticks > at {
				atStart[b.track] = b.ticks
			}
			if video != nil && b.track == video.Number {
				lastVideo = max(lastVideo, b.ticks)
			}
			return nil
		})
	}); err != nil {
		return TrimSpan{}, 0, err
	}

	cut := TrimSpan{First: atAnchor, Last: max(f.durationTicks(end), anchor), Origin: anchor}
	if video != nil && want > anchor && start < end && want <= lastVideo {
		cut.First, cut.Origin, cut.Lead = atStart, want, want-anchor
		cut.First[video.Number] = anchor
	}

	// A track with no block at or before the origin begins at its first block
	// after it, which is what the origin itself selects.
	for track, at := range cut.First {
		if at == math.MinInt64 {
			cut.First[track] = cut.Origin
		}
	}
	return cut, f.ticks(float64(cut.Origin)), nil
}

// trimAnchor is the timestamp a cut begins at: the video keyframe at or before
// the requested time, or the first one after it where none precedes.
func (f *File) trimAnchor(at time.Duration) (int64, error) {
	if f.VideoTrack() == nil {
		return f.durationTicks(at), nil
	}
	_, before, after, err := f.locateKeyframes(at)
	if err != nil {
		return 0, err
	}
	switch {
	case before != nil:
		return before.ticks, nil
	case after != nil:
		return after.ticks, nil
	default:
		return 0, fmt.Errorf("%w: the video track declares none", ErrNoSyncSample)
	}
}

// clusterCursor walks the segment's clusters one at a time, which is what a
// writer driven by a pull iterator needs and what eachCluster, which pushes,
// cannot give it.
type clusterCursor struct {
	f  *File
	at int64
}

// newClusterCursor starts a walk at the file's first cluster.
func (f *File) newClusterCursor() clusterCursor {
	at := f.firstCluster
	if at < 0 {
		at = f.segment.end()
	}
	return clusterCursor{f: f, at: at}
}

// next returns the payload of the next cluster, reporting false once the
// segment is spent. Anything that is not a cluster is stepped over.
func (c *clusterCursor) next() (span, bool, error) {
	end := c.f.segment.end()
	if c.at >= end {
		return span{}, false, nil
	}

	var (
		payload span
		found   bool
	)
	err := scanResolving(c.f.r, c.at, end-c.at, maxScanClusters, c.f.clusterEnd,
		func(e element, off int64) error {
			if e.id != idCluster {
				return nil
			}
			start := off + e.hdrSize
			stop := c.f.clusterEnd(e, off)
			payload, found, c.at = span{start: start, size: stop - start}, true, stop
			return errStopScan
		})
	if err != nil {
		return span{}, false, err
	}
	if !found {
		c.at = end
	}
	return payload, found, nil
}
