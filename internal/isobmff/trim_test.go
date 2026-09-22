package isobmff

import (
	"bytes"
	"errors"
	"io"
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

	ranges, actual, err := file.TrimRanges(0, 4*time.Second)
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

	if _, _, err := file.TrimRanges(0, time.Second); err == nil {
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
	// together, within the one audio frame the cut can land on.
	const audioFrameTicks = 1024

	src, _, _ := openCorpus(t, "h264-gop12.mp4")
	ranges, _, err := src.TrimRanges(1300*time.Millisecond, 2200*time.Millisecond)
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

	ranges, actual, err := src.TrimRanges(time.Second, 0)
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
