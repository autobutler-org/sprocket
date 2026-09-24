package matroska

import (
	"fmt"
	"time"
)

// SyncSample is one keyframe read out of the file, with everything a decoder
// needs to make sense of it on its own.
type SyncSample struct {
	// Data is the frame exactly as stored. For h264 and hevc that is a series
	// of NAL units, each prefixed with a NALLengthSize byte big-endian length;
	// for vp8, vp9, and av1 it is the codec's own bitstream with no framing
	// added.
	Data []byte
	// Time is when the frame is shown, from the block's timestamp. Matroska has
	// no edit list and no composition offsets, so this is the only timeline
	// there is.
	Time time.Duration
	// Codec is the track's codec short name, as Track.Codec documents.
	Codec string
	// Config is the track's CodecPrivate, aliasing the parsed Tracks element. A
	// decoder takes it verbatim. Treat it as read-only.
	Config []byte
	// NALLengthSize is the width in bytes of the length prefix on each NAL unit
	// in Data, 1 through 4, and 0 for a codec that carries none.
	NALLengthSize int
}

// keyframeAt locates one keyframe: when it is shown, and where to start looking
// for its block.
type keyframeAt struct {
	// ticks is the keyframe's time on the file's timestamp scale.
	ticks int64
	// cluster is the offset of the cluster's element header.
	cluster int64
	// within is the offset of the block's element header from the start of the
	// cluster's payload, or 0 to search the cluster from its first child.
	within int64
}

// ReadSyncSample reads the video track's keyframe at or before a presentation
// time and returns it with the track's codec configuration.
//
// The read is bounded: a frame declaring more than 32 MiB returns
// ErrFrameTooLarge rather than allocating what the file asks for.
func (f *File) ReadSyncSample(at time.Duration) (SyncSample, error) {
	video, before, _, err := f.locateKeyframes(at)
	if err != nil {
		return SyncSample{}, err
	}
	if before == nil {
		return SyncSample{}, fmt.Errorf("%w: none at or before %v", ErrNoSyncSample, at)
	}
	return f.readKeyframe(video, *before)
}

// ReadNearestSyncSample reads the video track's keyframe nearest a presentation
// time, which is the frame a thumbnail wants.
//
// The keyframe after the requested time wins when it is the closer of the two.
// A tie goes to the earlier one, a time past the end of the file answers with
// the last keyframe, and a time before the first answers with that first one.
//
// One frame is read, not two: the candidates are located in the seek index, or
// by walking block headers where there is none, and only the winner's bytes are
// pulled off disk.
func (f *File) ReadNearestSyncSample(at time.Duration) (SyncSample, error) {
	video, before, after, err := f.locateKeyframes(at)
	if err != nil {
		return SyncSample{}, err
	}
	want := f.durationTicks(at)
	switch {
	case before == nil && after == nil:
		return SyncSample{}, fmt.Errorf("%w: the video track declares none", ErrNoSyncSample)
	case before == nil:
		return f.readKeyframe(video, *after)
	case after != nil && after.ticks-want < want-before.ticks:
		return f.readKeyframe(video, *after)
	default:
		return f.readKeyframe(video, *before)
	}
}

// locateKeyframes finds the video track's keyframes on either side of a time.
// Either may be missing: a time before the first keyframe has nothing before
// it, and a time at or after the last has nothing after it.
func (f *File) locateKeyframes(at time.Duration) (*Track, *keyframeAt, *keyframeAt, error) {
	video := f.VideoTrack()
	if video == nil {
		return nil, nil, nil, ErrNoVideoTrack
	}
	want := f.durationTicks(max(at, 0))
	if f.cues.size > 0 {
		before, after, err := f.cueKeyframes(video, want)
		if err != nil {
			return nil, nil, nil, err
		}
		// A Cues element that indexes some other track, or none at all, is no
		// use here and the cluster walk is the fallback it leaves.
		if before != nil || after != nil {
			return video, before, after, nil
		}
	}
	before, after, err := f.scanKeyframes(video, want)
	return video, before, after, err
}

// durationTicks converts wall time to a count on the file's timestamp scale.
func (f *File) durationTicks(at time.Duration) int64 {
	if at <= 0 || f.TimestampScale == 0 {
		return 0
	}
	return int64(at) / int64(f.TimestampScale)
}

// cueKeyframes reads the seek index and returns the cue points on either side
// of a time. The index is read whole under maxCuesBytes and walked once; only
// two entries are kept, so a file with a hundred thousand cue points costs the
// index and nothing per entry.
func (f *File) cueKeyframes(video *Track, want int64) (*keyframeAt, *keyframeAt, error) {
	body, err := f.readCues()
	if err != nil {
		return nil, nil, err
	}

	var before, after *keyframeAt
	err = walk(body, 1, func(id uint32, point []byte) error {
		if id != idCuePoint {
			return nil
		}
		found, ok, err := f.parseCuePoint(video, point)
		if err != nil || !ok {
			return err
		}
		switch {
		case found.ticks <= want && (before == nil || found.ticks > before.ticks):
			before = &found
		case found.ticks > want && (after == nil || found.ticks < after.ticks):
			after = &found
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// readCues reads the seek index. The Cues element is located either by the
// segment walk, which knows its size, or by the SeekHead, which gives only its
// position, so its header is decoded here rather than assumed.
func (f *File) readCues() ([]byte, error) {
	var buf [maxElementHeader]byte
	read := min(f.cues.size, maxElementHeader)
	if _, err := f.r.ReadAt(buf[:read], f.cues.start); err != nil {
		return nil, fmt.Errorf("%w: reading the seek index header at %d: %w", ErrTruncated, f.cues.start, err)
	}
	e, err := parseElement(buf[:read], f.cues.size)
	if err != nil {
		return nil, fmt.Errorf("the seek index at %d: %w", f.cues.start, err)
	}
	if e.id != idCues || e.size == unknownSize {
		return nil, fmt.Errorf("%w: element %#x where the seek index should be", ErrMalformed, e.id)
	}
	return readWhole(f.r, f.cues.start+e.hdrSize, e.size, maxCuesBytes, "the seek index")
}

// parseCuePoint reads one cue point, reporting whether it indexes the video
// track. A cue position outside the segment is refused rather than followed.
func (f *File) parseCuePoint(video *Track, point []byte) (keyframeAt, bool, error) {
	var (
		found     keyframeAt
		ticks     uint64
		forTrack  bool
		haveTrack bool
	)
	err := walk(point, 2, func(id uint32, child []byte) error {
		var err error
		switch id {
		case idCueTime:
			ticks, err = readUint(child)
		case idCueTrackPos:
			if haveTrack {
				// One cue point may index several tracks. The first entry for
				// the video track wins and the rest are not read.
				return nil
			}
			found, forTrack, err = f.parseCueTrackPositions(video, child)
			haveTrack = forTrack
		}
		return err
	})
	if err != nil || !forTrack {
		return keyframeAt{}, false, err
	}
	found.ticks = int64(ticks)
	return found, true, nil
}

func (f *File) parseCueTrackPositions(video *Track, body []byte) (keyframeAt, bool, error) {
	var track, cluster, within uint64
	if err := walk(body, 3, func(id uint32, child []byte) error {
		var err error
		switch id {
		case idCueTrack:
			track, err = readUint(child)
		case idCueClusterPos:
			cluster, err = readUint(child)
		case idCueRelPos:
			within, err = readUint(child)
		}
		return err
	}); err != nil {
		return keyframeAt{}, false, err
	}
	if track != video.Number || cluster > uint64(f.segment.size) {
		return keyframeAt{}, false, nil
	}
	return keyframeAt{cluster: f.segment.start + int64(cluster), within: int64(within)}, true, nil
}

// scanKeyframes walks the clusters to find the video keyframes on either side
// of a time. It is what a file with no usable seek index leaves, and it reads
// block headers rather than block payloads. The walk stops at the first
// keyframe past the time, so a thumbnail near the front of a long file costs
// the clusters up to it and no more; it is bounded by maxScanClusters either
// way.
func (f *File) scanKeyframes(video *Track, want int64) (*keyframeAt, *keyframeAt, error) {
	var before, after *keyframeAt
	err := f.eachCluster(func(payload span, at int64) error {
		err := f.eachBlock(payload, func(b block) error {
			if b.track != video.Number || !b.keyframe {
				return nil
			}
			found := keyframeAt{ticks: b.ticks, cluster: at, within: b.at - payload.start}
			if b.ticks <= want {
				before = &found
				return nil
			}
			after = &found
			return errStopScan
		})
		if err != nil {
			return err
		}
		if after != nil {
			return errStopScan
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// readKeyframe pulls a located keyframe's bytes off disk and pairs them with
// the track's codec configuration.
//
// The search starts where the caller pointed and runs forward through the
// cluster: a cue's relative position lands on the block itself, and a cue that
// carries none lands on the cluster's first child, so one loop covers both. A
// cue that points at something else is not trusted, only followed.
func (f *File) readKeyframe(video *Track, found keyframeAt) (SyncSample, error) {
	payload, err := f.clusterPayload(found.cluster)
	if err != nil {
		return SyncSample{}, err
	}
	from := payload
	if found.within > 0 && found.within < payload.size {
		from = span{start: payload.start + found.within, size: payload.size - found.within}
	}

	var frame block
	var got bool
	err = f.eachBlock(from, func(b block) error {
		if b.track != video.Number || !b.keyframe {
			return nil
		}
		frame, got = b, true
		return errStopScan
	})
	if err != nil {
		return SyncSample{}, err
	}
	if !got {
		return SyncSample{}, fmt.Errorf("%w: the cluster at %d holds none for track %d",
			ErrNoSyncSample, found.cluster, video.Number)
	}

	data, err := f.readFrame(frame.first)
	if err != nil {
		return SyncSample{}, err
	}
	// A block found partway through a cluster was reached without its Timestamp
	// element, so the cue's own time is the better of the two.
	ticks := frame.ticks
	if from.start != payload.start {
		ticks = found.ticks
	}
	return SyncSample{
		Data:          data,
		Time:          f.ticks(float64(ticks)),
		Codec:         video.Codec,
		Config:        video.CodecPrivate,
		NALLengthSize: video.NALLengthSize,
	}, nil
}

// clusterPayload decodes the header of the cluster at an offset and returns the
// span of its children.
func (f *File) clusterPayload(at int64) (span, error) {
	if at < f.segment.start || at >= f.segment.end() {
		return span{}, fmt.Errorf("%w: a cluster at %d outside the segment", ErrMalformed, at)
	}
	var buf [maxElementHeader]byte
	avail := f.segment.end() - at
	read := min(avail, maxElementHeader)
	if _, err := f.r.ReadAt(buf[:read], at); err != nil {
		return span{}, fmt.Errorf("%w: reading the cluster header at %d: %w", ErrTruncated, at, err)
	}
	e, err := parseElement(buf[:read], avail)
	if err != nil {
		return span{}, fmt.Errorf("the cluster at %d: %w", at, err)
	}
	if e.id != idCluster {
		return span{}, fmt.Errorf("%w: element %#x where a cluster should be", ErrMalformed, e.id)
	}
	start := at + e.hdrSize
	return span{start: start, size: f.clusterEnd(e, at) - start}, nil
}

// readFrame reads one frame's bytes, refusing anything over the cap.
func (f *File) readFrame(at span) ([]byte, error) {
	if at.size > maxFrameBytes {
		return nil, fmt.Errorf("%w: a frame of %d bytes, over the %d byte cap",
			ErrFrameTooLarge, at.size, int64(maxFrameBytes))
	}
	if at.size <= 0 || at.start < 0 || at.start > f.size || at.size > f.size-at.start {
		return nil, fmt.Errorf("%w: a frame of %d bytes at %d of a %d byte file",
			ErrTruncated, at.size, at.start, f.size)
	}
	data := make([]byte, at.size)
	if _, err := f.r.ReadAt(data, at.start); err != nil {
		return nil, fmt.Errorf("%w: reading the frame at %d: %w", ErrTruncated, at.start, err)
	}
	return data, nil
}
