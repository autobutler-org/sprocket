package mpegts

import (
	"fmt"
	"time"
)

// Keyframe search limits. See ReadSyncSample for the strategy they bound.
const (
	// seekScanBytes caps one forward scan for keyframes.
	seekScanBytes = 64 << 20
	// seekStepBytes is the first step back when a scan starts past the
	// keyframe it wants. Each further step doubles.
	seekStepBytes = 1 << 20
	// maxSeekAttempts bounds the steps back, which with the doubling reaches
	// 255 MiB behind the first estimate.
	maxSeekAttempts = 8
)

// SyncSample is one keyframe read out of the stream, in the form the MP4
// family and the decoders take.
type SyncSample struct {
	// Data is the access unit as a series of NAL units, each behind a four
	// byte big-endian length, with the access unit delimiter and any in-band
	// copy of a parameter set in Config dropped.
	Data []byte
	// Time is when the frame is shown, measured from the file's origin as the
	// package documentation describes.
	Time time.Duration
	// Codec is the video stream's codec short name.
	Codec string
	// Config is the avcC or hvcC record built from the stream's parameter
	// sets. Treat it as read-only.
	Config []byte
	// NALLengthSize is 4, the length prefix Data is written with.
	NALLengthSize int
}

// keyframeAt locates one keyframe: the packet slot its PES starts in, and when
// it is shown.
type keyframeAt struct {
	slot int64
	pts  int64
}

// ReadSyncSample reads the video's keyframe at or before a time.
//
// There is no index, so the search is an estimate refined by reading. The time
// is turned into a byte offset in proportion to the file's duration, rounded to
// a packet, and the file is scanned forward from there for the start of each
// video PES, until the first keyframe after the time, the end of the file, or
// seekScanBytes. A keyframe is a PES whose first packet sets the random access
// indicator, or whose first slice is an IDR picture in H.264 or an IRAP picture
// in HEVC; only the first configSearchBytes of each PES are looked at to decide.
// When the scan found no keyframe at or before the time, it steps back by
// seekStepBytes and scans again, doubling the step each time, up to
// maxSeekAttempts times. A constant bitrate file lands within a GOP of the
// estimate and costs one scan of a GOP or so; the worst case, a file whose
// bitrate swings wildly, costs every attempt, which is bounded by the caps
// above. Only the winning keyframe's payload is gathered, under maxFrameBytes.
//
// A video codec this package cannot find pictures in, such as MPEG-2, comes
// back with no data and its codec named, for the decoder to refuse by name.
func (f *File) ReadSyncSample(at time.Duration) (SyncSample, error) {
	v := f.Video
	if v == nil {
		return SyncSample{}, ErrNoVideoTrack
	}
	if v.Codec != "h264" && v.Codec != "hevc" {
		return SyncSample{Codec: v.Codec}, nil
	}
	before, _, err := f.locate(at)
	if err != nil {
		return SyncSample{}, err
	}
	if before == nil {
		return SyncSample{}, fmt.Errorf("%w: none at or before %v", ErrNoSyncSample, at)
	}
	return f.readKeyframe(*before)
}

// ReadNearestSyncSample reads the video's keyframe nearest a time, measured on
// presentation times, the way ReadSyncSample searches. A tie goes to the
// earlier keyframe, a time past the end answers with the last one, and a time
// before the first answers with the first.
func (f *File) ReadNearestSyncSample(at time.Duration) (SyncSample, error) {
	v := f.Video
	if v == nil {
		return SyncSample{}, ErrNoVideoTrack
	}
	if v.Codec != "h264" && v.Codec != "hevc" {
		return SyncSample{Codec: v.Codec}, nil
	}
	before, after, err := f.locate(at)
	if err != nil {
		return SyncSample{}, err
	}
	want := f.want(at)
	switch {
	case before == nil && after == nil:
		return SyncSample{}, fmt.Errorf("%w: the video shows none", ErrNoSyncSample)
	case before == nil:
		return f.readKeyframe(*after)
	case after != nil && after.pts-want < want-before.pts:
		return f.readKeyframe(*after)
	default:
		return f.readKeyframe(*before)
	}
}

// want is a time as an unwrapped timestamp.
func (f *File) want(at time.Duration) int64 { return f.origin + durationToTicks(max(at, 0)) }

// locate finds the keyframes on either side of a time, as ReadSyncSample
// describes.
func (f *File) locate(at time.Duration) (before, after *keyframeAt, err error) {
	want := f.want(at)
	var est int64
	if span := f.end - f.origin; span > 0 {
		est = int64(float64(f.size) * float64(want-f.origin) / float64(span))
	}
	est = min(max(est, 0), f.size)
	from, step := est-est%f.stride, int64(seekStepBytes)
	for range maxSeekAttempts {
		b, a, err := f.scanKeyframes(from, want)
		if err != nil {
			return nil, nil, err
		}
		if a != nil && (after == nil || a.pts < after.pts) {
			after = a
		}
		if b != nil || from == 0 {
			return b, after, nil
		}
		from = max(from-step, 0)
		from -= from % f.stride
		step *= 2
	}
	return nil, after, nil
}

// scanKeyframes scans forward from a packet slot for video keyframes, keeping
// the last one at or before want and stopping at the first one after it.
func (f *File) scanKeyframes(from, want int64) (before, after *keyframeAt, err error) {
	v := f.Video
	rd := newReader(f.r, f.stride)
	rd.seek(from, min(f.size, from+seekScanBytes))

	var (
		cur                keyframeAt
		open, decided, key bool
		wantHeader         bool
		inspected          int
		split              annexB
	)
	data := func(b []byte) error {
		if wantHeader && len(b) > 0 {
			wantHeader = false
			if typ := nalType(v.hevc, b[0]); isVCL(v.hevc, typ) {
				decided, key = true, isRandomAccess(v.hevc, typ)
			}
		}
		return nil
	}
	start := func() error {
		wantHeader = true
		return nil
	}

	for {
		p, ok, err := rd.next()
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return before, after, nil
		}
		if p.pid != v.PID || !p.hasPayload {
			continue
		}
		es, h, err := esPayload(p)
		if err != nil {
			open = false
			continue
		}
		if p.start {
			open, decided, key, wantHeader, inspected, split = h.hasPTS, false, false, false, 0, annexB{}
			if open {
				cur = keyframeAt{slot: rd.slotOf(p), pts: f.unwrap(h.pts)}
				decided, key = p.randomAccess, p.randomAccess
			}
		}
		if !open {
			continue
		}
		if !decided {
			if err := split.feed(es, data, start); err != nil {
				return nil, nil, err
			}
			inspected += len(es)
			decided = decided || inspected >= configSearchBytes
		}
		if !decided {
			continue
		}
		open = false
		if !key {
			continue
		}
		found := cur
		if found.pts > want {
			return before, &found, nil
		}
		before = &found
	}
}

// readKeyframe gathers one keyframe's access unit, from the packet its PES
// starts in to the next PES of the stream, and converts it.
func (f *File) readKeyframe(k keyframeAt) (SyncSample, error) {
	v := f.Video
	rd := newReader(f.r, f.stride)
	rd.seek(k.slot, f.size)

	var (
		au      []byte
		started bool
	)
	for {
		p, ok, err := rd.next()
		if err != nil {
			return SyncSample{}, err
		}
		if !ok {
			break
		}
		if p.pid != v.PID || !p.hasPayload {
			continue
		}
		if p.start && started {
			break
		}
		if !p.start && !started {
			return SyncSample{}, fmt.Errorf("%w: no PES starts at %d", ErrMalformed, k.slot)
		}
		started = true
		es, _, err := esPayload(p)
		if err != nil {
			return SyncSample{}, err
		}
		if len(au)+len(es) > maxFrameBytes {
			return SyncSample{}, fmt.Errorf("%w: the keyframe at %d runs past %d bytes",
				ErrFrameTooLarge, k.slot, int64(maxFrameBytes))
		}
		au = append(au, es...)
	}
	data, err := toLengthPrefixed(au, v.hevc, v.params)
	if err != nil {
		return SyncSample{}, err
	}
	return SyncSample{
		Data:          data,
		Time:          ticksToDuration(max(k.pts-f.origin, 0)),
		Codec:         v.Codec,
		Config:        v.Config,
		NALLengthSize: 4,
	}, nil
}
