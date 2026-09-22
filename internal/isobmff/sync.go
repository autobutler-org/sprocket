package isobmff

import (
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
	// or trun composition offset. It is when the sample is displayed.
	Time time.Duration
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
	media := track.mediaTicks(at, f.Timescale)

	var (
		found fragSample
		index uint32
		err   error
	)
	if len(track.fragments) > 0 {
		found, index, err = f.fragmentSync(track, media)
	} else {
		found, index, err = track.tableSync(media)
	}
	if err != nil {
		return SyncSample{}, err
	}

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
	where, err := t.tables.sampleRange(sync)
	if err != nil {
		return fragSample{}, 0, err
	}
	return fragSample{
		decode: t.tables.sampleTime(sync),
		comp:   t.tables.compositionOffset(sync),
		offset: where.offset,
		size:   where.size,
		sync:   true,
	}, sync, nil
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
