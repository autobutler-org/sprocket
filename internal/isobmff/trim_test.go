package isobmff

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
	"time"
)

// syncLate builds a file whose video track's first sync sample is its third.
// No corpus file is like that, and it is the case the forward snap exists for:
// a cut at the start of the file has no keyframe to snap back to. The four
// samples are one byte each, which also puts a constant sample size through the
// cut, another thing the corpus does not hold.
func syncLate(t *testing.T) *File {
	t.Helper()

	const (
		samples   = 4
		timescale = 10
		perSample = 10
	)
	stbl := concat(
		box("stsd", concat([]byte{0, 0, 0, 0}, put32(0))),
		box("stts", concat([]byte{0, 0, 0, 0}, put32(1), put32(samples), put32(perSample))),
		box("stss", concat([]byte{0, 0, 0, 0}, put32(1), put32(3))),
		box("stsc", concat([]byte{0, 0, 0, 0}, put32(1), put32(1), put32(samples), put32(1))),
		box("stsz", concat([]byte{0, 0, 0, 0}, put32(1), put32(samples))),
		box("stco", concat([]byte{0, 0, 0, 0}, put32(1), put32(0))),
	)
	content := concat(
		box("ftyp", []byte("isom\x00\x00\x00\x00isom")),
		box("moov", concat(
			box("mvhd", mvhd(100, samples*perSample*100/timescale)),
			box("trak", concat(
				box("tkhd", tkhd(1, matrixIdentity, 128, 72)),
				box("mdia", concat(
					box("mdhd", mdhd(timescale, samples*perSample)),
					box("hdlr", hdlr("vide")),
					box("minf", box("stbl", stbl)),
				)),
			)),
		)),
	)

	file, err := Parse(readerAt(content), int64(len(content)))
	if err != nil {
		t.Fatalf("parse the handcrafted file: %v", err)
	}
	return file
}

func TestTrimRangesSnapsForwardWhenNoKeyframePrecedes(t *testing.T) {
	file := syncLate(t)

	ranges, actual, err := file.TrimRanges(0, 4*time.Second, true)
	if err != nil {
		t.Fatalf("pick the ranges: %v", err)
	}

	// Nothing to snap back to at zero, so the cut starts at the first keyframe
	// there is, which is the third sample, two seconds in.
	if actual != 2*time.Second {
		t.Errorf("actual start = %v, want 2s", actual)
	}
	if got, want := ranges[1], (Range{First: 2, Last: 3}); got != want {
		t.Errorf("range = %+v, want %+v", got, want)
	}

	// And the writer takes that range over a constant-size sample table.
	if err := Write(io.Discard, file, TargetMP4, ranges); err != nil {
		t.Errorf("write the cut: %v", err)
	}
}

func TestTrimRangesRefusesAFragmentedSource(t *testing.T) {
	file, _, _ := openCorpus(t, "fragmented.mp4")

	if _, _, err := file.TrimRanges(0, time.Second, true); err == nil {
		t.Error("a fragmented source produced ranges, and its samples are not in the moov")
	}
}

func TestTrimRangesReportsATrackWithNoKeyframe(t *testing.T) {
	// An stss whose entry is sample zero names a sample that does not exist,
	// since the table counts from one, so the track declares keyframes and
	// points at none of them. There is nowhere for a cut to start.
	var track Track
	if err := track.parseStbl(concat(
		box("stts", concat([]byte{0, 0, 0, 0}, put32(1), put32(2), put32(10))),
		box("stsz", concat([]byte{0, 0, 0, 0}, put32(1), put32(2))),
		box("stss", concat([]byte{0, 0, 0, 0}, put32(1), put32(0))),
	), 0); err != nil {
		t.Fatalf("parse the handcrafted stbl: %v", err)
	}
	track.Timescale = 10

	if _, err := track.trimStart(0, 1000); !errors.Is(err, ErrNoSyncSample) {
		t.Errorf("error = %v, want ErrNoSyncSample", err)
	}
}

func TestTrimStartsEveryTrackAtTheSameInstant(t *testing.T) {
	// The cut drops the edit lists that hid each codec's delay, so the tracks
	// would drift apart by the difference between those delays unless the
	// composition offsets are normalized. Video and audio have to start
	// together, within the one audio frame the cut can land on. The ranges are
	// picked without hiding a lead-in, which is the cut a transport stream
	// gets, so no edit list stands in for the normalization.
	const audioFrameTicks = 1024

	src, _, _ := openCorpus(t, "h264-gop12.mp4")
	ranges, _, err := src.TrimRanges(1300*time.Millisecond, 2200*time.Millisecond, false)
	if err != nil {
		t.Fatalf("pick the ranges: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, src, TargetMP4, ranges); err != nil {
		t.Fatalf("write the cut: %v", err)
	}
	out, err := Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("parse the cut: %v", err)
	}

	video, audio := out.VideoTrack(), out.AudioTrack()
	if got := video.tables.compositionTicks(0); got != 0 {
		t.Errorf("the first video frame is displayed at %d ticks, want 0", got)
	}
	if got := audio.tables.compositionTicks(0); got != 0 {
		t.Errorf("the first audio frame is displayed at %d ticks, want 0", got)
	}
	apart := ticksToDuration(video.tables.compositionTicks(0), video.Timescale) -
		ticksToDuration(audio.tables.compositionTicks(0), audio.Timescale)
	if window := ticksToDuration(audioFrameTicks, audio.Timescale); apart > window || apart < -window {
		t.Errorf("the tracks start %v apart, want at most one %v audio frame", apart, window)
	}
}

func TestTrimRangesHoldsOneSampleForAnEmptyWindow(t *testing.T) {
	src, _, _ := openCorpus(t, "h264-gop12.mp4")

	ranges, actual, err := src.TrimRanges(time.Second, 0, true)
	if err != nil {
		t.Fatalf("pick the ranges: %v", err)
	}
	if actual != time.Second {
		t.Errorf("actual start = %v, want 1s", actual)
	}
	for id, span := range ranges {
		if span.First != span.Last {
			t.Errorf("track %d keeps %+v, want the one sample it snapped to", id, span)
		}
	}

	var buf bytes.Buffer
	if err := Write(&buf, src, TargetMP4, ranges); err != nil {
		t.Fatalf("write the cut: %v", err)
	}
	out, err := Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("parse the cut: %v", err)
	}
	for _, track := range out.Tracks {
		if got := track.SampleCount(); got != 1 {
			t.Errorf("the %s track holds %d samples, want 1", track.Handler, got)
		}
	}
}

// The sparse-keyframe corpus file and the cut issue 42 reported: keyframes at 0
// and 3 seconds only, and a start of 1.969 seconds, which has nothing but the
// keyframe at zero to copy from. At 24 fps the frame on screen at 1.969 is the
// 48th, shown from 1.958.
const (
	gop72          = "h264-gop72.mp4"
	sparseStart    = 1969 * time.Millisecond
	sparseEnd      = 3500 * time.Millisecond
	sparseFrame    = 47
	videoFrame     = time.Second / 24
	audioFrameTime = time.Second * 1024 / 48000
)

// shownAtZero reports, for a track of a written file, the position in display
// order of the sample on screen when the movie starts, and how long before
// that start the sample began. The edit list's media time is the instant the
// movie starts from, and the sample showing then is the last one displayed at
// or before it.
func shownAtZero(t *testing.T, track *Track) (position int, before time.Duration) {
	t.Helper()

	var skip uint64
	if len(track.Edits) > 0 {
		if len(track.Edits) != 1 || track.Edits[0].MediaTime < 0 {
			t.Fatalf("the %s track's edit list is %+v, want one entry that skips a lead-in", track.Handler, track.Edits)
		}
		skip = uint64(track.Edits[0].MediaTime)
	}
	shown := make([]uint64, 0, track.SampleCount())
	for i := range track.SampleCount() {
		sample, err := track.Sample(i)
		if err != nil {
			t.Fatalf("read sample %d: %v", i, err)
		}
		shown = append(shown, sample.Composition)
	}
	slices.Sort(shown)
	position = -1
	for i, at := range shown {
		if at <= skip {
			position = i
		}
	}
	if position < 0 {
		t.Fatalf("the %s track shows nothing at or before %d ticks", track.Handler, skip)
	}
	return position, ticksToDuration(skip-shown[position], track.Timescale)
}

func TestTrimHidesTheLeadInBehindAnEditList(t *testing.T) {
	src, _, _ := openCorpus(t, gop72)
	ranges, actual, err := src.TrimRanges(sparseStart, sparseEnd, true)
	if err != nil {
		t.Fatalf("pick the ranges: %v", err)
	}
	if actual != sparseStart {
		t.Errorf("the cut is shown from %v, want the %v asked for", actual, sparseStart)
	}

	for _, target := range []Target{TargetMP4, TargetMOV} {
		t.Run(string(target), func(t *testing.T) {
			var buf bytes.Buffer
			if err := Write(&buf, src, target, ranges); err != nil {
				t.Fatalf("write the cut: %v", err)
			}
			out, err := Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
			if err != nil {
				t.Fatalf("parse the cut: %v", err)
			}

			// The video still starts on the keyframe, since nothing else
			// decodes, but the frame on screen when the movie starts is the
			// one the source showed at the requested start.
			video, audio := out.VideoTrack(), out.AudioTrack()
			if sample, err := video.Sample(0); err != nil || !sample.Sync {
				t.Errorf("the first video sample is %+v (%v), want the keyframe", sample, err)
			}
			position, before := shownAtZero(t, video)
			if position != sparseFrame {
				t.Errorf("frame %d is on screen at the start, want frame %d", position, sparseFrame)
			}
			if want := sparseStart - sparseFrame*videoFrame; before < want-time.Millisecond || before > want+time.Millisecond {
				t.Errorf("the first frame began %v before the start, want %v", before, want)
			}

			// The audio starts on the frame playing at the requested start,
			// with the part of it before the start cut away too, so the two
			// tracks begin at the same instant rather than a frame apart.
			if _, before := shownAtZero(t, audio); before < 0 || before >= audioFrameTime {
				t.Errorf("the first audio frame began %v before the start, want under one %v frame", before, audioFrameTime)
			}

			// What plays is the window that was asked for.
			if got, want := out.Duration(), sparseEnd-sparseStart; got < want-videoFrame || got > want+videoFrame {
				t.Errorf("duration = %v, want about %v", got, want)
			}
		})
	}
}

func TestTrimRangesSnapsWhereTheLeadInCannotBeHidden(t *testing.T) {
	src, _, _ := openCorpus(t, gop72)

	for _, tc := range []struct {
		name       string
		start, end time.Duration
		hide       bool
	}{
		// A target that cannot hide frames, which is a transport stream.
		{name: "not hidden", start: sparseStart, end: sparseEnd},
		// A window that ends before it starts keeps the keyframe's one sample.
		{name: "empty window", start: sparseStart, end: sparseStart, hide: true},
		// A start past the end of the file has nothing to show from.
		{name: "past the end", start: time.Hour, end: 2 * time.Hour, hide: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ranges, actual, err := src.TrimRanges(tc.start, tc.end, tc.hide)
			if err != nil {
				t.Fatalf("pick the ranges: %v", err)
			}
			keyframe := time.Duration(0)
			if tc.start > sparseEnd {
				keyframe = 3 * time.Second
			}
			if actual != keyframe {
				t.Errorf("actual start = %v, want the keyframe at %v", actual, keyframe)
			}
			for id, span := range ranges {
				if span.Lead != 0 {
					t.Errorf("track %d hides %d ticks, want none", id, span.Lead)
				}
			}
		})
	}
}
