package isobmff

import (
	"fmt"
	"time"
)

// TrimRanges picks the samples each track keeps for a cut of the movie timeline
// between start and end, and reports the presentation time the cut actually
// begins at. The result is what Write takes as its ranges.
//
// start snaps back to a sync sample, because a cut cannot begin mid-GOP without
// re-encoding: the video track's sync sample at or before the requested time
// wins, a start past the end of the file takes the last one, and a track whose
// first sync sample comes later than the request snaps forward to it, which is
// the nearest range that decodes. A negative start is read as zero. The
// reported time is that sample's presentation time on the movie timeline, so it
// is the time Thumbnail would name for the same keyframe.
//
// Every other track is cut against the same window: it starts at the sample
// covering the reported time, snapped back to its own sync sample where it
// declares any, and holds what the caller asked for measured from that sample's
// own decode time. Each track therefore lands on its own sample boundaries, an
// audio track within one frame of the video, rather than on the video's. An end
// at or before the start keeps the single sample the start snapped to, and an
// end past the file keeps the rest of it.
//
// A fragmented source returns ErrUnsupportedSource, as it does from Write, and
// a video track that declares no sync sample at all returns ErrNoSyncSample.
func (f *File) TrimRanges(start, end time.Duration) (map[uint32]Range, time.Duration, error) {
	if f.Fragmented {
		return nil, 0, fmt.Errorf("%w: the source is fragmented, and its samples are described by moof boxes rather than by the moov's tables", ErrUnsupportedSource)
	}

	var (
		video  = f.VideoTrack()
		anchor uint32
		actual = max(start, 0)
	)
	if video != nil && video.tables.count > 0 {
		sync, err := video.trimStart(actual, f.Timescale)
		if err != nil {
			return nil, 0, err
		}
		anchor = sync
		actual = video.movieTime(video.tables.compositionTicks(sync), f.Timescale)
	}

	span := max(end-actual, 0)
	ranges := make(map[uint32]Range, len(f.Tracks))
	for _, t := range f.Tracks {
		if t.tables.count == 0 || (t.Handler != "vide" && t.Handler != "soun") {
			continue
		}
		// The video track keeps the sample the whole cut was anchored on rather
		// than looking it up again: mapping the time back through the edit list
		// and the composition offsets could land a frame or two later.
		first := anchor
		if t != video {
			found, err := t.trimStart(actual, f.Timescale)
			if err != nil {
				return nil, 0, err
			}
			first = found
		}
		// The end is measured from the first kept sample's own decode time, so
		// every track holds the span that was asked for rather than the span
		// plus whatever its codec's delay happens to be.
		last := t.tables.sampleAtTime(t.tables.sampleTime(first) + durationToTicks(span, t.Timescale))
		ranges[t.ID] = Range{First: first, Last: max(min(last, t.tables.count-1), first)}
	}
	return ranges, actual, nil
}

// trimStart is the sample a track begins at for a cut at a movie time: the
// sample covering that time, snapped back to the track's own sync sample. A
// track whose first sync sample comes later snaps forward to it instead, since
// a range that cannot be decoded is worse than one that starts late.
func (t *Track) trimStart(at time.Duration, movieTimescale uint32) (uint32, error) {
	index := min(t.tables.sampleAtTime(t.mediaTicks(at, movieTimescale)), t.tables.count-1)
	if sync, ok := t.tables.syncAtOrBefore(index); ok {
		return sync, nil
	}
	if sync, ok := t.tables.syncAfter(index); ok {
		return sync, nil
	}
	return 0, fmt.Errorf("%w: track %d declares none at or after sample %d", ErrNoSyncSample, t.ID, index)
}
