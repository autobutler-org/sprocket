package matroska

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// copyCut cuts a Matroska file into a Matroska one and returns the time the cut
// is shown from with the bytes written.
func copyCut(t *testing.T, src *File, start, end time.Duration) (time.Duration, []byte) {
	t.Helper()

	cut, actual, err := src.TrimSpan(start, end)
	if err != nil {
		t.Fatalf("trim span: %v", err)
	}
	var out bytes.Buffer
	if err := Copy(&out, src, isobmff.TargetMKV, &cut); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return actual, out.Bytes()
}

// TestACutIsReadOnItsShownTimeline reads back the sparse cut, whose lead-in is
// hidden behind a CodecDelay, and checks that every reader of it works on the
// timeline a player shows rather than the one the blocks are stored on.
func TestACutIsReadOnItsShownTimeline(t *testing.T) {
	src, _ := openMatroska(t, "h264-gop72.mkv")
	_, raw := copyCut(t, src, sparseStart, sparseEnd)
	clip, err := Parse(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse the cut: %v", err)
	}

	t.Run("duration", func(t *testing.T) {
		if got, want := clip.Duration(), sparseEnd-sparseStart; got < want-50*time.Millisecond || got > want+50*time.Millisecond {
			t.Errorf("duration = %v, want about %v", got, want)
		}
	})

	t.Run("keyframe", func(t *testing.T) {
		for _, at := range []time.Duration{0, 500 * time.Millisecond, 1100 * time.Millisecond} {
			got, err := clip.ReadNearestSyncSample(at)
			if err != nil {
				t.Fatalf("read the cut's keyframe at %v: %v", at, err)
			}
			want, err := src.ReadNearestSyncSample(sparseStart + at)
			if err != nil {
				t.Fatalf("read the source's keyframe at %v: %v", sparseStart+at, err)
			}
			if !bytes.Equal(got.Data, want.Data) {
				t.Errorf("at %v the cut's nearest keyframe is not the source's at %v", at, sparseStart+at)
			}
			if want.Time >= sparseStart && got.Time != want.Time-sparseStart {
				t.Errorf("at %v the keyframe is shown at %v, want %v", at, got.Time, want.Time-sparseStart)
			}
		}
	})

	t.Run("trim again", func(t *testing.T) {
		const start, end = 500 * time.Millisecond, 1200 * time.Millisecond
		actual, again := copyCut(t, clip, start, end)
		if actual != start {
			t.Errorf("the second cut is shown from %v, want %v", actual, start)
		}
		_, direct := copyCut(t, src, sparseStart+start, sparseStart+end)

		gotFile, got := blockTimes(t, again)
		wantFile, want := blockTimes(t, direct)
		for track, times := range want {
			if !slices.Equal(got[track], times) {
				t.Errorf("track %d is shown at %v, want %v, as a cut of the source shows it",
					track, got[track][:min(len(got[track]), 4)], times[:min(len(times), 4)])
			}
		}
		if gotFile.Duration() != wantFile.Duration() {
			t.Errorf("duration = %v, want %v", gotFile.Duration(), wantFile.Duration())
		}
	})

	t.Run("remux to mkv", func(t *testing.T) {
		var out bytes.Buffer
		if err := Copy(&out, clip, isobmff.TargetMKV, nil); err != nil {
			t.Fatalf("copy: %v", err)
		}
		assertStartsAtTheRequestedFrame(t, out.Bytes())
	})

	t.Run("remux to mp4", func(t *testing.T) {
		out, raw := writeFragmented(t, clip, nil)
		assertFragmentedStartsAtTheRequestedFrame(t, out, raw)
	})
}

// TestAudioCodecDelayIsApplied reads a file whose audio alone declares a
// CodecDelay, the AAC priming ffmpeg writes, and checks the audio is read where
// ffmpeg shows it: one priming frame before the video, at -21 milliseconds.
func TestAudioCodecDelayIsApplied(t *testing.T) {
	src, raw := openMatroska(t, "h264-aac.mkv")
	_, times := blockTimes(t, raw)
	video, audio := src.VideoTrack().Number, src.AudioTrack().Number
	if got := times[video][0]; got != 0 {
		t.Errorf("the video begins at %d, want 0", got)
	}
	if got := times[audio][0]; got != -21 {
		t.Errorf("the audio begins at %d, want -21, where the priming CodecDelay puts it", got)
	}

	var out bytes.Buffer
	if err := Copy(&out, src, isobmff.TargetMKV, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if _, copied := blockTimes(t, out.Bytes()); !slices.Equal(copied[audio], times[audio]) {
		t.Errorf("a whole-file copy shows the audio at %v, want %v", copied[audio][:4], times[audio][:4])
	}
}
