package sprocket_test

import (
	"bytes"
	"errors"
	"image"
	"io"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

// The keyframe corpus file: three seconds of the same 128x72 content at 24 fps
// with a keyframe every twelve frames, so a trim has more than one to land on.
const (
	gop12 = "h264-gop12.mp4"
	// audioFrame is one AAC frame at 48 kHz, the granularity an audio track can
	// be cut on.
	audioFrame = time.Second * 1024 / 48000
	tolerance  = time.Millisecond
)

// trimmed trims a corpus file into memory. The corpus is small enough to hold
// twice over; the bounded-memory case is its own test.
func trimmed(t *testing.T, name string, start, end time.Duration) (out []byte, actual time.Duration) {
	t.Helper()

	source := readCorpus(t, name)
	var buf bytes.Buffer
	actual, err := sprocket.Trim(bytes.NewReader(source), int64(len(source)), &buf, sprocket.MP4, start, end)
	if err != nil {
		t.Fatalf("trim %s: %v", name, err)
	}
	return buf.Bytes(), actual
}

// demux parses an in-memory file with the demuxer, which is where the sample
// tables a trim rewrites can be looked at directly.
func demux(t *testing.T, content []byte) *isobmff.File {
	t.Helper()

	file, err := isobmff.Parse(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("parse the trim: %v", err)
	}
	return file
}

// trackOf returns the output's first track with the given handler.
func trackOf(t *testing.T, file *isobmff.File, handler string) *isobmff.Track {
	t.Helper()

	for _, track := range file.Tracks {
		if track.Handler == handler {
			return track
		}
	}
	t.Fatalf("no %q track in the output", handler)
	return nil
}

// closeTo fails unless two durations are within a millisecond of each other.
func closeTo(t *testing.T, what string, got, want time.Duration) {
	t.Helper()

	if diff := got - want; diff > tolerance || diff < -tolerance {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestTrimSnapsBackToTheKeyframe(t *testing.T) {
	// The keyframes sit at 0, 0.5, 1.0, 1.5, 2.0, and 2.5 seconds, so a cut
	// asked for at 1.3 has to come back at 1.0 rather than mid-GOP.
	out, actual := trimmed(t, gop12, 1300*time.Millisecond, 2200*time.Millisecond)

	closeTo(t, "actual start", actual, time.Second)

	// 29 video samples: the keyframe at 1.0s and the 1.2s asked for after it,
	// which lands inside the sample starting at 2.166s.
	file := demux(t, out)
	if got := trackOf(t, file, "vide").SampleCount(); got != 29 {
		t.Errorf("video holds %d samples, want 29", got)
	}
	if got := trackOf(t, file, "soun").SampleCount(); got != 57 {
		t.Errorf("audio holds %d samples, want 57", got)
	}
	closeTo(t, "duration", probeBytes(t, out).Duration, 1216*time.Millisecond)

	// Everything that is not the cut has to survive it.
	source, cut := probeBytes(t, readCorpus(t, gop12)), probeBytes(t, out)
	if cut.Width != source.Width || cut.Height != source.Height {
		t.Errorf("dimensions = %dx%d, want %dx%d", cut.Width, cut.Height, source.Width, source.Height)
	}
	if cut.VideoCodec != source.VideoCodec || cut.AudioCodec != source.AudioCodec {
		t.Errorf("codecs = %q/%q, want %q/%q", cut.VideoCodec, cut.AudioCodec, source.VideoCodec, source.AudioCodec)
	}
	if cut.Rotation != source.Rotation {
		t.Errorf("rotation = %d, want %d", cut.Rotation, source.Rotation)
	}
}

func TestTrimOutputStartsAtItsFirstFrame(t *testing.T) {
	out, _ := trimmed(t, gop12, 1300*time.Millisecond, 2200*time.Millisecond)
	file := demux(t, out)

	sample, err := file.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read the first keyframe: %v", err)
	}
	if sample.Index != 0 {
		t.Errorf("the first keyframe is sample %d, want 0", sample.Index)
	}
	if sample.DecodeTime != 0 {
		t.Errorf("the first sample decodes at %v, want 0", sample.DecodeTime)
	}
	// Displayed at zero, not one composition offset after it: the cut carries
	// no edit list to hide the codec's delay, so the delay comes out of the
	// composition offsets instead.
	if sample.Time != 0 || sample.MovieTime != 0 {
		t.Errorf("the first sample is shown at %v (%v on the movie timeline), want 0", sample.Time, sample.MovieTime)
	}

	// The source's edit list described a timeline the cut no longer has, so it
	// is dropped rather than carried across.
	for _, track := range file.Tracks {
		if len(track.Edits) != 0 {
			t.Errorf("%s track carries an edit list: %+v", track.Handler, track.Edits)
		}
	}
}

func TestTrimKeepsTheKeyframeItLandedOn(t *testing.T) {
	// The strongest check there is that the cut landed on the right keyframe and
	// copied the right bytes: the output's first frame decodes to the same
	// picture as the source's keyframe at the actual start time.
	source := readCorpus(t, gop12)
	want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), time.Second, sprocket.ThumbnailOptions{})
	if err != nil && !h264Compiled && errors.Is(err, sprocket.ErrUnsupportedCodec) {
		t.Skipf("%s needs the h264 build tag", gop12)
	}
	if err != nil {
		t.Fatalf("thumbnail the source at 1s: %v", err)
	}

	out, actual := trimmed(t, gop12, 1300*time.Millisecond, 2200*time.Millisecond)
	got, err := sprocket.Thumbnail(bytes.NewReader(out), int64(len(out)), 0, sprocket.ThumbnailOptions{})
	if err != nil {
		t.Fatalf("thumbnail the trim: %v", err)
	}
	if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
		t.Errorf("the trim starting at %v decoded a different picture from the source's keyframe there", actual)
	}
}

func TestTrimOnAFileWithOneKeyframe(t *testing.T) {
	// The rest of the corpus holds a single keyframe at the start, so every cut
	// snaps back to zero however late it was asked for.
	for _, name := range []string{"hevc-aac-8bit.mov", "h264-aac.mp4"} {
		t.Run(name, func(t *testing.T) {
			out, actual := trimmed(t, name, 500*time.Millisecond, 1500*time.Millisecond)

			if actual != 0 {
				t.Errorf("actual start = %v, want 0", actual)
			}
			file := demux(t, out)
			// 37 of the 48 samples: 1.5 seconds of 24 fps video, and the sample
			// that starts exactly at 1.5 seconds is at or before it.
			if got := trackOf(t, file, "vide").SampleCount(); got != 37 {
				t.Errorf("video holds %d samples, want 37", got)
			}
			closeTo(t, "duration", probeBytes(t, out).Duration, 1541667*time.Microsecond)

			frame, err := sprocket.Thumbnail(bytes.NewReader(out), int64(len(out)), 0, sprocket.ThumbnailOptions{})
			if err != nil && !h264Compiled && errors.Is(err, sprocket.ErrUnsupportedCodec) {
				t.Skipf("%s needs the h264 build tag", name)
			}
			if err != nil {
				t.Fatalf("thumbnail the trim: %v", err)
			}
			source := readCorpus(t, name)
			want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), 0, sprocket.ThumbnailOptions{})
			if err != nil {
				t.Fatalf("thumbnail the source: %v", err)
			}
			if !bytes.Equal(frame.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
				t.Error("the trim decoded a different picture from the source")
			}
		})
	}
}

func TestTrimClampsTheEndToTheFile(t *testing.T) {
	// Twelve frames: the keyframe at 2.5 seconds and everything after it, which
	// is all the file has left however much was asked for.
	out, actual := trimmed(t, gop12, 2900*time.Millisecond, 10*time.Second)

	closeTo(t, "actual start", actual, 2500*time.Millisecond)
	if got := trackOf(t, demux(t, out), "vide").SampleCount(); got != 12 {
		t.Errorf("video holds %d samples, want 12", got)
	}
}

func TestTrimPastTheEndTakesTheLastKeyframe(t *testing.T) {
	out, actual := trimmed(t, gop12, 10*time.Second, 11*time.Second)

	closeTo(t, "actual start", actual, 2500*time.Millisecond)
	if got := trackOf(t, demux(t, out), "vide").SampleCount(); got != 12 {
		t.Errorf("video holds %d samples, want 12", got)
	}
}

func TestTrimBeforeZeroStartsAtZero(t *testing.T) {
	_, actual := trimmed(t, gop12, -time.Second, time.Second)

	if actual != 0 {
		t.Errorf("actual start = %v, want 0", actual)
	}
}

func TestTrimOfAnEmptyRangeKeepsOneSample(t *testing.T) {
	// An end at or before the start keeps the sample the start snapped to,
	// rather than writing a file with no samples in it.
	out, actual := trimmed(t, gop12, time.Second, time.Second)

	closeTo(t, "actual start", actual, time.Second)
	file := demux(t, out)
	if got := trackOf(t, file, "vide").SampleCount(); got != 1 {
		t.Errorf("video holds %d samples, want 1", got)
	}
	if got := trackOf(t, file, "soun").SampleCount(); got != 1 {
		t.Errorf("audio holds %d samples, want 1", got)
	}
}

func TestTrimAVideoOnlyFile(t *testing.T) {
	out, actual := trimmed(t, "no-audio.mp4", 500*time.Millisecond, 1500*time.Millisecond)

	if actual != 0 {
		t.Errorf("actual start = %v, want 0", actual)
	}
	file := demux(t, out)
	if len(file.Tracks) != 1 {
		t.Fatalf("the output holds %d tracks, want 1", len(file.Tracks))
	}
	if got := trackOf(t, file, "vide").SampleCount(); got != 37 {
		t.Errorf("video holds %d samples, want 37", got)
	}
}

func TestTrimAlignsAudioWithTheVideo(t *testing.T) {
	// Audio is cut on its own sample boundaries, so the two tracks cannot end
	// at the same instant. One AAC frame is the granularity available.
	out, _ := trimmed(t, gop12, 1300*time.Millisecond, 2200*time.Millisecond)
	file := demux(t, out)

	video := trackOf(t, file, "vide").Duration()
	audio := trackOf(t, file, "soun").Duration()
	if diff := video - audio; diff > audioFrame || diff < -audioFrame {
		t.Errorf("audio runs %v and video %v, %v apart, want at most one %v frame", audio, video, diff, audioFrame)
	}
}

func TestTrimRefusesWhatItCannotWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		content []byte
		target  sprocket.Container
		want    error
	}{
		{name: "prores and pcm", file: "prores-pcm.mov", target: sprocket.MP4, want: sprocket.ErrIncompatible},
		{name: "fragmented", file: "fragmented.mp4", target: sprocket.MP4, want: sprocket.ErrUnsupportedContainer},
		{name: "unknown target", file: gop12, target: sprocket.Container("mkv"), want: sprocket.ErrUnsupportedContainer},
		{name: "not media", content: []byte("hello world, definitely not a movie"), target: sprocket.MP4, want: sprocket.ErrUnsupportedContainer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.content
			if tc.file != "" {
				content = readCorpus(t, tc.file)
			}

			_, err := sprocket.Trim(bytes.NewReader(content), int64(len(content)), io.Discard, tc.target, 0, time.Second)
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTrimReportsAWriteFailure(t *testing.T) {
	source := readCorpus(t, gop12)
	cut, _ := trimmed(t, gop12, 500*time.Millisecond, 2*time.Second)

	// Failing inside the ftyp, inside the moov, and inside the payload copy are
	// three different places in the writer.
	for _, limit := range []int{0, 4, len(cut) / 2, len(cut) - 1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			_, err := sprocket.Trim(bytes.NewReader(source), int64(len(source)), &shortWriter{left: limit},
				sprocket.MP4, 500*time.Millisecond, 2*time.Second)
			if !errors.Is(err, errWriteFailed) {
				t.Errorf("error = %v, want the writer's own error", err)
			}
		})
	}
}

func TestTrimHoldsBoundedMemory(t *testing.T) {
	const (
		sampleSize  = int64(64) << 20
		allocBudget = uint64(4) << 20
	)

	// The inflated sample is the video track's last one, so a trim that runs to
	// the end of the file is a trim that has to move it.
	source, size := inflatedSource(t, sampleSize)
	out := &countingWriter{}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := sprocket.Trim(source, size, out, sprocket.MP4, 500*time.Millisecond, time.Hour); err != nil {
		t.Fatalf("trim: %v", err)
	}
	runtime.ReadMemStats(&after)

	if out.n < sampleSize {
		t.Fatalf("wrote %d bytes, want at least the %d byte sample", out.n, sampleSize)
	}
	grew := after.TotalAlloc - before.TotalAlloc
	if grew > allocBudget {
		t.Errorf("allocated %d bytes to move %d, want at most %d", grew, out.n, allocBudget)
	} else {
		t.Logf("allocated %d bytes to move %d, with heap at %d", grew, out.n, after.HeapAlloc)
	}
}
