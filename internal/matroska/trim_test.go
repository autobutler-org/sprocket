package matroska

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// The cut issue 42 reported, against the sparse-keyframe corpus files:
// keyframes at 0 and 3 seconds only, and a start of 1.969 seconds, which has
// nothing but the keyframe at zero to copy from. At 24 fps the frame on screen
// at 1.969 is the 48th, which a Matroska file stamps at 1958 milliseconds.
const (
	sparseStart = 1969 * time.Millisecond
	sparseEnd   = 3500 * time.Millisecond
	sparseFrame = 47
	// sparseBefore is how long before the start that frame began, in
	// milliseconds, the scale every file here is stamped on.
	sparseBefore = 1969 - 1958
	// audioFrameTicks is one AAC frame at 48 kHz in milliseconds, rounded up.
	audioFrameTicks = 22
)

// shownAt reports, for a list of the times a track's frames are shown at, the
// position in display order of the frame on screen at zero and how long before
// zero it began.
func shownAt(t *testing.T, what string, shown []int64) (position int, before int64) {
	t.Helper()

	slices.Sort(shown)
	position = -1
	for i, at := range shown {
		if at <= 0 {
			position = i
		}
	}
	if position < 0 {
		t.Fatalf("the %s track shows nothing at or before zero: %v", what, shown[:min(len(shown), 4)])
	}
	return position, -shown[position]
}

// blockTimes walks a written Matroska file and returns the time each block of
// every track is shown at, on the file's own scale: its timestamp less the
// track's CodecDelay, which is what a player subtracts.
func blockTimes(t *testing.T, out []byte) (*File, map[uint64][]int64) {
	t.Helper()

	file, err := Parse(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("reparse the cut: %v", err)
	}
	delay := map[uint64]int64{}
	for _, track := range file.Tracks {
		delay[track.Number] = int64((track.CodecDelay + file.TimestampScale/2) / file.TimestampScale)
	}
	times := map[uint64][]int64{}
	if err := file.eachCluster(func(payload span, _ int64) error {
		return file.eachBlock(payload, func(b block) error {
			times[b.track] = append(times[b.track], b.ticks-delay[b.track])
			return nil
		})
	}); err != nil {
		t.Fatalf("walk the cut: %v", err)
	}
	return file, times
}

// assertStartsAtTheRequestedFrame checks a Matroska cut of the sparse window:
// the keyframe is shown ahead of zero, the frame on screen at zero is the one
// the source showed at the requested start, and the audio begins with it.
func assertStartsAtTheRequestedFrame(t *testing.T, out []byte) {
	t.Helper()

	file, times := blockTimes(t, out)
	video, audio := file.VideoTrack(), file.AudioTrack()

	if first := times[video.Number][0]; first != -1969 {
		t.Errorf("the keyframe is shown at %d, want -1969: the lead-in before the start", first)
	}
	// The stored timestamps stay at or after zero, where every reader can
	// take them, and CodecDelay alone moves the lead-in before the start.
	if delay := time.Duration(video.CodecDelay); delay < sparseStart-time.Millisecond || delay > sparseStart+time.Millisecond {
		t.Errorf("the video's CodecDelay is %v, want the %v lead-in", delay, sparseStart)
	}
	position, before := shownAt(t, "video", times[video.Number])
	if position != sparseFrame || before != sparseBefore {
		t.Errorf("frame %d, begun %dms before, is on screen at zero, want frame %d begun %dms before",
			position, before, sparseFrame, sparseBefore)
	}
	if _, before := shownAt(t, "audio", times[audio.Number]); before >= audioFrameTicks {
		t.Errorf("the audio on at zero began %dms before it, want under one %dms frame", before, audioFrameTicks)
	}
	if got, want := file.Duration(), sparseEnd-sparseStart; got < want-50*time.Millisecond || got > want+50*time.Millisecond {
		t.Errorf("duration = %v, want about %v", got, want)
	}
}

func TestTrimSpanHidesTheLeadIn(t *testing.T) {
	src, _ := openMatroska(t, "h264-gop72.mkv")
	cut, actual, err := src.TrimSpan(sparseStart, sparseEnd)
	if err != nil {
		t.Fatalf("trim span: %v", err)
	}
	if actual != sparseStart {
		t.Errorf("the cut is shown from %v, want the %v asked for", actual, sparseStart)
	}
	if first := cut.First[src.VideoTrack().Number]; first != 0 {
		t.Errorf("the video begins on the block at %d, want the keyframe at 0", first)
	}

	t.Run("mkv", func(t *testing.T) {
		var out bytes.Buffer
		if err := Copy(&out, src, isobmff.TargetMKV, &cut); err != nil {
			t.Fatalf("copy: %v", err)
		}
		assertStartsAtTheRequestedFrame(t, out.Bytes())
	})

	t.Run("mp4", func(t *testing.T) {
		out, raw := writeFragmented(t, src, &cut)
		for _, track := range out.Tracks {
			if len(track.Edits) != 1 {
				t.Fatalf("the %s track's edit list is %+v, want one entry", track.Handler, track.Edits)
			}
			// The fragments are on the source's millisecond scale, so the
			// presentation the edit list leaves is in milliseconds too.
			skip := track.Edits[0].MediaTime
			var shown []int64
			for _, s := range trunSamples(t, raw, track.ID) {
				shown = append(shown, int64(s.decode)+s.comp-skip)
			}
			position, before := shownAt(t, track.Handler, shown)
			switch track.Handler {
			case "vide":
				if position != sparseFrame || before != sparseBefore {
					t.Errorf("frame %d, begun %dms before, is on screen at zero, want frame %d begun %dms before",
						position, before, sparseFrame, sparseBefore)
				}
			case "soun":
				if before >= audioFrameTicks {
					t.Errorf("the audio on at zero began %dms before it, want under one %dms frame", before, audioFrameTicks)
				}
			}
		}
	})
}

func TestTrimSpanSnapsWhereTheLeadInCannotBeHidden(t *testing.T) {
	src, _ := openMatroska(t, "h264-gop72.mkv")

	for _, tc := range []struct {
		name       string
		start, end time.Duration
		want       time.Duration
	}{
		{name: "empty window", start: sparseStart, end: sparseStart, want: 0},
		{name: "past the end", start: time.Hour, end: 2 * time.Hour, want: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cut, actual, err := src.TrimSpan(tc.start, tc.end)
			if err != nil {
				t.Fatalf("trim span: %v", err)
			}
			if actual != tc.want {
				t.Errorf("actual start = %v, want the keyframe at %v", actual, tc.want)
			}
			for track := range cut.First {
				if lead := cut.lead(track); lead != 0 {
					t.Errorf("track %d hides %d ticks, want none", track, lead)
				}
			}
		})
	}
}

func TestWriteHidesTheLeadIn(t *testing.T) {
	src := openISOBMFF(t, "h264-gop72.mp4")
	ranges, actual, err := src.TrimRanges(sparseStart, sparseEnd, true)
	if err != nil {
		t.Fatalf("pick the ranges: %v", err)
	}
	if actual != sparseStart {
		t.Errorf("the cut is shown from %v, want the %v asked for", actual, sparseStart)
	}

	var out bytes.Buffer
	if err := Write(&out, src, isobmff.TargetMKV, ranges); err != nil {
		t.Fatalf("write: %v", err)
	}
	assertStartsAtTheRequestedFrame(t, out.Bytes())
}
