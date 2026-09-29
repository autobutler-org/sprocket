package isobmff

import (
	"fmt"
	"time"
)

// TrimRanges picks the samples each track keeps for a cut of the movie timeline
// between start and end, and reports the presentation time the cut is shown
// from. The result is what Write takes as its ranges.
//
// The video track's samples start at a sync sample, because a cut cannot begin
// mid-GOP without re-encoding: its sync sample at or before the requested time
// wins, a start past the end of the file takes the last one, and a track whose
// first sync sample comes later than the request snaps forward to it, which is
// the nearest range that decodes. A negative start is read as zero.
//
// With hide set, the frames between that sync sample and the requested start
// are the cut's lead-in: they are kept, since the frames after them are coded
// against them, and each Range's Lead says how much of the track to hide, which
// a writer turns into an edit list or a Matroska CodecDelay. The reported
// time is then the requested start. Without hide, which is what a target that
// cannot hide frames asks for, and wherever there is nothing to hide, the
// reported time is the sync sample's presentation time on the movie timeline,
// the time Thumbnail would name for the same keyframe. There is nothing to hide
// for a start on a sync sample, a start past the end of the file, or an end at
// or before the start.
//
// Every other track is cut against the same window: it starts at the sample
// covering the reported time, snapped back to its own sync sample where it
// declares any, and holds what the caller asked for measured from that time.
// Each track therefore lands on its own sample boundaries, an audio track
// within one frame of the video, and a hidden cut hides that frame's part
// before the start too. An end at or before the start keeps the single sample
// the start snapped to, and an end past the file keeps the rest of it.
//
// A fragmented source returns ErrUnsupportedSource, as it does from Write, and
// a video track that declares no sync sample at all returns ErrNoSyncSample.
func (f *File) TrimRanges(start, end time.Duration, hide bool) (map[uint32]Range, time.Duration, error) {
	if f.Fragmented {
		return nil, 0, fmt.Errorf("%w: the source is fragmented, and its samples are described by moof boxes rather than by the moov's tables", ErrUnsupportedSource)
	}

	var (
		video  = f.VideoTrack()
		anchor uint32
		actual = max(start, 0)
		hiding bool
	)
	if video != nil && video.tables.count > 0 {
		sync, err := video.trimStart(actual, f.Timescale)
		if err != nil {
			return nil, 0, err
		}
		anchor = sync
		keyframe := video.MovieTime(video.tables.compositionTicks(sync), f.Timescale)
		hiding = hide && actual > keyframe && actual < end && actual < f.Duration()
		if !hiding {
			actual = keyframe
		}
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
		// The lead is measured from when the first kept sample is shown to
		// where the start falls on the track's own media timeline, which is
		// the one the edit list a writer makes of it is measured on.
		var lead uint64
		if at, from := t.mediaTicks(actual, f.Timescale), t.tables.compositionTicks(first); hiding && at > from {
			lead = at - from
		}
		// The end is measured from the first kept sample's own decode time,
		// past any lead, so every track holds the span that was asked for
		// rather than the span plus whatever its codec's delay happens to be.
		last := t.tables.sampleAtTime(t.tables.sampleTime(first) + lead + durationToTicks(span, t.Timescale))
		ranges[t.ID] = Range{First: first, Last: max(min(last, t.tables.count-1), first), Lead: lead}
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
