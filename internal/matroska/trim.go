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
	// Origin is where the cut begins: the video keyframe it snapped to, or the
	// requested start in a file with no video. The output's duration is
	// measured from it.
	Origin int64
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
// presentation time the cut actually begins at.
//
// start snaps back to the video track's keyframe at or before it, for the
// reason the ISOBMFF side snaps: a cut cannot begin mid-GOP without
// re-encoding. A start past the end of the file takes the last keyframe, a file
// whose first keyframe comes later snaps forward to it, and a negative start
// reads as zero. A file with no video track anchors on the requested time
// itself, since there is no keyframe grid to land on.
//
// Every other track begins at its own last block at or before that anchor, so
// an audio track carries up to one frame of sound from before the video's first
// frame rather than starting late. Finding those costs one walk of the cluster
// headers, which is what a file with no sample table leaves.
//
// The end takes the last block at or before it, with no snapping constraint,
// and an end at or before the start keeps the single block each track began on.
func (f *File) TrimSpan(start, end time.Duration) (TrimSpan, time.Duration, error) {
	anchor, err := f.trimAnchor(max(start, 0))
	if err != nil {
		return TrimSpan{}, 0, err
	}

	cut := TrimSpan{First: map[uint64]int64{}, Last: max(f.durationTicks(end), anchor), Origin: anchor}
	for _, t := range f.Tracks {
		if t.Type == trackVideo || t.Type == trackAudio {
			cut.First[t.Number] = math.MinInt64
		}
	}
	if err := f.eachCluster(func(payload span, _ int64) error {
		return f.eachBlock(payload, func(b block) error {
			if at, ok := cut.First[b.track]; ok && b.ticks <= anchor && b.ticks > at {
				cut.First[b.track] = b.ticks
			}
			return nil
		})
	}); err != nil {
		return TrimSpan{}, 0, err
	}

	// A track with no block at or before the anchor begins at its first block
	// after it, which is what the anchor itself selects.
	for track, at := range cut.First {
		if at == math.MinInt64 {
			cut.First[track] = anchor
		}
	}
	return cut, f.ticks(float64(anchor)), nil
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
