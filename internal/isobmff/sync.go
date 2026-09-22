package isobmff

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// SyncSample is one sync sample read out of the file, with everything a decoder
// needs to make sense of it on its own.
type SyncSample struct {
	// Data is the sample exactly as stored. For h264 and hevc that is a series
	// of NAL units, each prefixed with a NALLengthSize byte big-endian length.
	Data []byte
	// Index is the sample's position in the track, counting from zero and
	// running across fragments.
	Index uint32
	// DecodeTime is the sample's decode time on the track's media timeline.
	DecodeTime time.Duration
	// Time is the sample's composition time, which is DecodeTime plus the ctts
	// or trun composition offset. It is when the sample is displayed, on the
	// track's own media timeline.
	Time time.Duration
	// MovieTime is when the sample is displayed on the movie timeline, which is
	// the timeline ReadSyncSample's argument is measured on. It is Time mapped
	// back through the track's edit list, so a file with no edit list reports
	// the same value twice.
	MovieTime time.Duration
	// Codec is the track's codec short name, as SampleEntry.Codec documents.
	Codec string
	// Config is the raw avcC or hvcC record, aliasing the parsed moov. A
	// decoder takes it verbatim. Treat it as read-only.
	Config []byte
	// NALLengthSize is the width in bytes of the length prefix on each NAL unit
	// in Data, 1 through 4.
	NALLengthSize int
}

// ReadSyncSample reads the video track's sync sample at or before a
// presentation time and returns it with the track's codec configuration. at is
// measured on the movie timeline and mapped through the track's edit list.
//
// The read is bounded: a sample declaring more than 32 MiB returns
// ErrSampleTooLarge rather than allocating what the file asks for.
func (f *File) ReadSyncSample(at time.Duration) (SyncSample, error) {
	track := f.VideoTrack()
	if track == nil {
		return SyncSample{}, ErrNoVideoTrack
	}
	found, index, err := f.syncAtOrBefore(track, track.mediaTicks(at, f.Timescale))
	if err != nil {
		return SyncSample{}, err
	}
	return f.readSyncSample(track, found, index)
}

// ReadNearestSyncSample reads the video track's sync sample nearest a
// presentation time, which is the keyframe a thumbnail wants. at is measured on
// the movie timeline and mapped through the track's edit list.
//
// The sync sample after the requested time wins when it is the closer of the
// two, measured on composition times, which is when each one is displayed. A
// tie goes to the earlier keyframe. A time past the end of the track answers
// with the last keyframe, and a time before the first keyframe answers with
// that first one.
//
// One sample is read, not two: the candidates are located in the sample tables
// and only the winner's bytes are pulled off disk, under the same cap
// ReadSyncSample documents.
func (f *File) ReadNearestSyncSample(at time.Duration) (SyncSample, error) {
	track := f.VideoTrack()
	if track == nil {
		return SyncSample{}, ErrNoVideoTrack
	}
	media := track.mediaTicks(at, f.Timescale)

	found, index, beforeErr := f.syncAtOrBefore(track, media)
	next, nextIndex, haveNext, err := f.syncAfter(track, index)
	if err != nil {
		return SyncSample{}, err
	}
	switch {
	case beforeErr != nil:
		// Nothing at or before the time. A track whose first keyframe is not
		// its first sample still has a nearest one; anything else is the error.
		if !haveNext || !errors.Is(beforeErr, ErrNoSyncSample) {
			return SyncSample{}, beforeErr
		}
		found, index = next, nextIndex
	case haveNext && ticksApart(compositionTime(next), media) < ticksApart(compositionTime(found), media):
		found, index = next, nextIndex
	}
	return f.readSyncSample(track, found, index)
}

// ticksApart is the distance between two media times, either way round.
func ticksApart(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

// syncAtOrBefore locates the sync sample at or before a media time, from the
// track's own tables or across its fragments.
func (f *File) syncAtOrBefore(t *Track, media uint64) (fragSample, uint32, error) {
	if len(t.fragments) > 0 {
		return f.fragmentSync(t, media)
	}
	return t.tableSync(media)
}

// syncAfter locates the first sync sample after index, reporting whether the
// track has one at all. A track with no later keyframe is not an error.
func (f *File) syncAfter(t *Track, index uint32) (fragSample, uint32, bool, error) {
	if len(t.fragments) > 0 {
		return f.fragmentSyncAfter(t, index)
	}
	sync, ok := t.tables.syncAfter(index)
	if !ok {
		return fragSample{}, 0, false, nil
	}
	found, err := t.tableSample(sync)
	return found, sync, err == nil, err
}

// readSyncSample pulls a located sample's bytes off disk and pairs them with
// the track's codec configuration.
func (f *File) readSyncSample(track *Track, found fragSample, index uint32) (SyncSample, error) {
	if found.size > maxSampleBytes {
		return SyncSample{}, fmt.Errorf("%w: sample %d is %d bytes, over the %d byte cap",
			ErrSampleTooLarge, index, found.size, int64(maxSampleBytes))
	}
	if found.size <= 0 || found.offset < 0 || found.offset > f.size || found.size > f.size-found.offset {
		return SyncSample{}, fmt.Errorf("%w: sample %d spans %d bytes at %d of a %d byte file",
			ErrTruncated, index, found.size, found.offset, f.size)
	}
	data := make([]byte, found.size)
	if _, err := f.r.ReadAt(data, found.offset); err != nil {
		return SyncSample{}, fmt.Errorf("%w: reading sample %d: %w", ErrTruncated, index, err)
	}

	return SyncSample{
		Data:          data,
		Index:         index,
		DecodeTime:    ticksToDuration(found.decode, track.Timescale),
		Time:          ticksToDuration(compositionTime(found), track.Timescale),
		MovieTime:     track.movieTime(compositionTime(found), f.Timescale),
		Codec:         track.Entry.Codec,
		Config:        track.Entry.Config,
		NALLengthSize: track.Entry.NALLengthSize,
	}, nil
}

// compositionTime shifts a decode time by its composition offset, which may be
// negative, without underflowing.
func compositionTime(s fragSample) uint64 {
	t := int64(s.decode) + s.comp
	if t < 0 {
		return 0
	}
	return uint64(t)
}

// tableSync finds the sync sample at or before a media time in a track's own
// sample tables.
func (t *Track) tableSync(media uint64) (fragSample, uint32, error) {
	index := t.tables.sampleAtTime(media)
	sync, ok := t.tables.syncAtOrBefore(index)
	if !ok {
		return fragSample{}, 0, fmt.Errorf("%w: at or before sample %d", ErrNoSyncSample, index)
	}
	found, err := t.tableSample(sync)
	return found, sync, err
}

// tableSample describes one sample of a track's own sample tables: when it is
// shown and where it lives in the file.
func (t *Track) tableSample(index uint32) (fragSample, error) {
	where, err := t.tables.sampleRange(index)
	if err != nil {
		return fragSample{}, err
	}
	return fragSample{
		decode: t.tables.sampleTime(index),
		comp:   t.tables.compositionOffset(index),
		offset: where.offset,
		size:   where.size,
		sync:   true,
	}, nil
}

// fragmentSync finds the sync sample at or before a media time across a
// fragmented track. It locates the fragment covering the time from the
// summaries, then reads that one moof to walk its samples.
func (f *File) fragmentSync(t *Track, media uint64) (fragSample, uint32, error) {
	i := len(t.fragments) - 1
	for i > 0 && t.fragments[i].baseTime > media {
		i--
	}
	// The first fragment searched is bounded by the requested time. If it holds
	// no sync sample that early, earlier fragments are searched whole.
	limit := media
	for ; i >= 0; i-- {
		fragment := t.fragments[i]
		if !fragment.sync {
			continue
		}
		var (
			best  fragSample
			index uint32
			found bool
			nth   uint32
		)
		if err := f.fragmentSamples(t, fragment, func(s fragSample) {
			if s.sync && s.decode <= limit {
				best, index, found = s, fragment.firstIndex+nth, true
			}
			nth++
		}); err != nil {
			return fragSample{}, 0, err
		}
		if found {
			return best, index, nil
		}
		limit = math.MaxUint64
	}
	return fragSample{}, 0, fmt.Errorf("%w: at or before %d media ticks", ErrNoSyncSample, media)
}

// fragmentSyncAfter finds the first sync sample after index across a fragmented
// track. Fragments are in index order, so the search starts at the first one
// holding a sample past index and stops at the first keyframe it finds.
func (f *File) fragmentSyncAfter(t *Track, index uint32) (fragSample, uint32, bool, error) {
	for _, fragment := range t.fragments {
		if !fragment.sync || fragment.firstIndex+fragment.samples <= index+1 {
			continue
		}
		var (
			best  fragSample
			at    uint32
			found bool
			nth   uint32
		)
		if err := f.fragmentSamples(t, fragment, func(s fragSample) {
			if !found && s.sync && fragment.firstIndex+nth > index {
				best, at, found = s, fragment.firstIndex+nth, true
			}
			nth++
		}); err != nil {
			return fragSample{}, 0, false, err
		}
		if found {
			return best, at, true, nil
		}
	}
	return fragSample{}, 0, false, nil
}
