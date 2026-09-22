package sprocket_test

import (
	"bytes"
	"errors"
	"image"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

// TestProbeReadsTheMatroskaCorpus is the family-specific half of the golden
// comparison TestProbeMatchesCorpusGoldens already makes over the whole corpus.
// It asserts the files are there, so that a corpus that lost them fails here
// rather than passing quietly.
func TestProbeReadsTheMatroskaCorpus(t *testing.T) {
	names, err := corpus.MatroskaFiles()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	if len(names) < 5 {
		t.Fatalf("the corpus holds %d Matroska files, want the mkv and webm set", len(names))
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			info, err := sprocket.Probe(bytes.NewReader(source), int64(len(source)))
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			// Matroska carries no display matrix, so this is the whole of what
			// the format can say about orientation.
			if info.Rotation != 0 {
				t.Errorf("rotation = %d, want 0: Matroska has no display matrix", info.Rotation)
			}
		})
	}
}

func TestThumbnailWebM(t *testing.T) {
	source := readCorpus(t, "vp8-vorbis.webm")

	frame, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), 0, sprocket.ThumbnailOptions{})
	if err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	if got, want := frame.Image.Bounds(), image.Rect(0, 0, 128, 72); got != want {
		t.Errorf("bounds are %v, want %v", got, want)
	}
	if _, ok := frame.Image.(*image.RGBA); !ok {
		t.Errorf("image is %T, want *image.RGBA", frame.Image)
	}
	if frame.Time != 0 {
		t.Errorf("frame time = %v, want 0", frame.Time)
	}
	if uniform(frame.Image) {
		t.Error("every pixel is the same color, so nothing was decoded")
	}
}

// TestThumbnailVP9IsUnsupported holds the acceptance case: the file probes
// correctly and the thumbnail path reports the typed error, which is the signal
// to fall back to an icon rather than to treat the file as broken.
func TestThumbnailVP9IsUnsupported(t *testing.T) {
	source := readCorpus(t, "vp9-opus.webm")

	info, err := sprocket.Probe(bytes.NewReader(source), int64(len(source)))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.VideoCodec != "vp9" || info.Width != 128 {
		t.Errorf("info = %+v, want a 128 pixel wide vp9 file", info)
	}

	_, err = sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), 0, sprocket.ThumbnailOptions{})
	if !errors.Is(err, sprocket.ErrUnsupportedCodec) {
		t.Errorf("error = %v, want ErrUnsupportedCodec", err)
	}
}

func TestThumbnailAV1(t *testing.T) {
	source := readCorpus(t, "av1-opus.webm")

	frame, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), 0, sprocket.ThumbnailOptions{})
	if err != nil {
		t.Fatalf("thumbnail: %v", err)
	}
	if got, want := frame.Image.Bounds(), image.Rect(0, 0, 128, 72); got != want {
		t.Errorf("bounds are %v, want %v", got, want)
	}
	if uniform(frame.Image) {
		t.Error("every pixel is the same color, so nothing was decoded")
	}
}

// TestRemuxIntoMatroska is the headline case of the other direction: an mp4
// goes into an mkv and the output probes to what the input probed to.
func TestRemuxIntoMatroska(t *testing.T) {
	for _, name := range []string{"h264-aac.mp4", "hevc-aac-8bit.mov", "no-audio.mp4"} {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			want := probeBytes(t, source)

			var out bytes.Buffer
			if err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), &out, sprocket.MKV); err != nil {
				t.Fatalf("remux: %v", err)
			}
			got := probeBytes(t, out.Bytes())

			// One frame of slack on the duration: Matroska states the duration
			// the muxer measured rather than the movie duration an mvhd
			// declares, and the two agree only to within the last frame.
			const tolerance = 50 * time.Millisecond
			if diff := got.Duration - want.Duration; diff > tolerance || diff < -tolerance {
				t.Errorf("duration = %v, want about %v", got.Duration, want.Duration)
			}
			if got.Width != want.Width || got.Height != want.Height {
				t.Errorf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, want.Width, want.Height)
			}
			if got.VideoCodec != want.VideoCodec || got.AudioCodec != want.AudioCodec {
				t.Errorf("codecs = %s/%s, want %s/%s",
					got.VideoCodec, got.AudioCodec, want.VideoCodec, want.AudioCodec)
			}
			const frameRateTolerance = 0.01
			if diff := got.FrameRate - want.FrameRate; diff > frameRateTolerance || diff < -frameRateTolerance {
				t.Errorf("frame rate = %v, want %v", got.FrameRate, want.FrameRate)
			}
		})
	}
}

// TestRemuxIntoMatroskaKeepsKeyframes checks that the output is not merely
// probeable: its seek index has to land a thumbnail on the same keyframe the
// source does.
func TestRemuxIntoMatroskaKeepsKeyframes(t *testing.T) {
	if !h264Compiled {
		t.Skip("this build has no H.264 decoder, so there is no frame to compare")
	}
	source := readCorpus(t, gop12)

	var out bytes.Buffer
	if err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), &out, sprocket.MKV); err != nil {
		t.Fatalf("remux: %v", err)
	}

	for _, at := range []time.Duration{0, 700 * time.Millisecond, 1300 * time.Millisecond, 10 * time.Second} {
		want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), at, sprocket.ThumbnailOptions{})
		if err != nil {
			t.Fatalf("thumbnail of the source at %v: %v", at, err)
		}
		got, err := sprocket.Thumbnail(bytes.NewReader(out.Bytes()), int64(out.Len()), at, sprocket.ThumbnailOptions{})
		if err != nil {
			t.Fatalf("thumbnail of the output at %v: %v", at, err)
		}
		if got.Time != want.Time {
			t.Errorf("at %v the output's keyframe is at %v and the source's at %v", at, got.Time, want.Time)
		}
		if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
			t.Errorf("at %v the output decoded to a different picture", at)
		}
	}
}

// TestRemuxIntoWebMRefusesTheWrongCodecs holds the one container in the set
// with a codec list of its own.
func TestRemuxIntoWebMRefusesTheWrongCodecs(t *testing.T) {
	source := readCorpus(t, "h264-aac.mp4")

	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.WebM)
	if !errors.Is(err, sprocket.ErrIncompatible) {
		t.Fatalf("error = %v, want ErrIncompatible", err)
	}
	if !strings.Contains(err.Error(), "h264") {
		t.Errorf("error %q does not name the codec that blocked it", err)
	}
}

// TestRemuxRefusesAMatroskaSource holds what is not built yet, so that the gap
// is a typed error a caller can branch on rather than a broken output.
func TestRemuxRefusesAMatroskaSource(t *testing.T) {
	source := readCorpus(t, "h264-aac.mkv")

	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.MP4)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

// TestTrimIntoMatroska cuts an mp4 and writes the result as an mkv, which is
// the two features crossed.
func TestTrimIntoMatroska(t *testing.T) {
	source := readCorpus(t, gop12)

	var out bytes.Buffer
	actual, err := sprocket.Trim(bytes.NewReader(source), int64(len(source)), &out,
		sprocket.MKV, 1300*time.Millisecond, 2300*time.Millisecond)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if want := time.Second; actual != want {
		t.Errorf("the cut begins at %v, want %v", actual, want)
	}

	// The cut runs from the keyframe it snapped back to, at 1.0 seconds, to the
	// end that was asked for, at 2.3, so the output is the 1.3 seconds between
	// them rather than the second that was asked for.
	info := probeBytes(t, out.Bytes())
	const (
		want      = 1300 * time.Millisecond
		tolerance = 100 * time.Millisecond
	)
	if diff := info.Duration - want; diff > tolerance || diff < -tolerance {
		t.Errorf("duration = %v, want about %v", info.Duration, want)
	}
	if info.VideoCodec != "h264" || info.AudioCodec != "aac" {
		t.Errorf("codecs = %s/%s, want h264/aac", info.VideoCodec, info.AudioCodec)
	}
	if !h264Compiled {
		return
	}
	// The output has to start on the keyframe the cut snapped to.
	frame, err := sprocket.Thumbnail(bytes.NewReader(out.Bytes()), int64(out.Len()), 0, sprocket.ThumbnailOptions{})
	if err != nil {
		t.Fatalf("thumbnail of the trimmed output: %v", err)
	}
	if frame.Time != 0 {
		t.Errorf("the trimmed output's first keyframe is at %v, want 0", frame.Time)
	}
}

// TestRemuxIntoMatroskaHoldsBoundedMemory is the same assertion the ISOBMFF
// path makes, against the same inflated source: a sample that declares 64 MiB
// costs the writer its headers and one copy buffer, not the sample.
func TestRemuxIntoMatroskaHoldsBoundedMemory(t *testing.T) {
	const (
		sampleSize  = int64(64) << 20
		allocBudget = uint64(4) << 20
	)

	source, size := inflatedSource(t, sampleSize)
	out := &countingWriter{}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := sprocket.Remux(source, size, out, sprocket.MKV); err != nil {
		t.Fatalf("remux: %v", err)
	}
	runtime.ReadMemStats(&after)

	if out.n < sampleSize {
		t.Fatalf("wrote %d bytes, want at least the %d byte sample", out.n, sampleSize)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > allocBudget {
		t.Errorf("allocated %d bytes to move %d, want at most %d", grew, out.n, allocBudget)
	} else {
		t.Logf("allocated %d bytes to move %d, with heap at %d", grew, out.n, after.HeapAlloc)
	}
}

// uniform reports whether every pixel of an image is the same color, which is
// what a decode that produced nothing looks like.
func uniform(img image.Image) bool {
	bounds := img.Bounds()
	first := img.At(bounds.Min.X, bounds.Min.Y)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if img.At(x, y) != first {
				return false
			}
		}
	}
	return true
}
