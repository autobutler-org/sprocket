package sprocket_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"math"
	"runtime"
	"slices"
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
			assertProbesAgree(t, probeBytes(t, out.Bytes()), want)
		})
	}
}

// assertProbesAgree holds two probes of the same streams in different
// containers to each other. The duration gets one frame of slack: Matroska
// states the duration the muxer measured rather than the movie duration an
// mvhd declares, and the two agree only to within the last frame.
func assertProbesAgree(t *testing.T, got, want sprocket.Info) {
	t.Helper()

	const (
		tolerance          = 50 * time.Millisecond
		frameRateTolerance = 0.01
	)
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
	if diff := got.FrameRate - want.FrameRate; diff > frameRateTolerance || diff < -frameRateTolerance {
		t.Errorf("frame rate = %v, want %v", got.FrameRate, want.FrameRate)
	}
}

// TestRemuxRoundTripsThroughMatroska takes an mp4 into an mkv and back into an
// mp4, which crosses both writers, and holds all three probes to one another.
// The keyframe bytes are compared in the matroska package, where the samples
// can be read directly.
func TestRemuxRoundTripsThroughMatroska(t *testing.T) {
	const name = "h264-aac.mp4"
	source := readCorpus(t, name)
	middle := remuxed(t, name, sprocket.MKV)

	var back bytes.Buffer
	if err := sprocket.Remux(bytes.NewReader(middle), int64(len(middle)), &back, sprocket.MP4); err != nil {
		t.Fatalf("remux the mkv back into an mp4: %v", err)
	}

	want := probeBytes(t, source)
	assertProbesAgree(t, probeBytes(t, middle), want)
	assertProbesAgree(t, probeBytes(t, back.Bytes()), want)
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

// matroskaSources are the corpus files whose codecs all fit an mp4, which is
// what the Matroska-to-MP4 remux can be asked for.
var matroskaSources = []string{"h264-aac.mkv", "hevc-aac.mkv", "h264-gop12.mkv", "vp9-opus.webm", "av1-opus.webm"}

// TestRemuxMatroskaIntoMP4 is the other direction of TestRemuxIntoMatroska: a
// Matroska source into the MP4 family, which comes out fragmented because the
// source states its frames cluster by cluster and has no index to put in front
// of them.
func TestRemuxMatroskaIntoMP4(t *testing.T) {
	for _, name := range matroskaSources {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			want := probeBytes(t, source)

			out := remuxed(t, name, sprocket.MP4)
			if types := topLevelTypes(t, out); !slices.Contains(types, "moof") {
				t.Fatalf("box order = %v, want movie fragments in it", types)
			}
			got := probeBytes(t, out)

			// The duration is the source's own, carried into the movie header,
			// so it comes back exactly rather than within a frame.
			if got.Duration != want.Duration {
				t.Errorf("duration = %v, want %v", got.Duration, want.Duration)
			}
			if got.Width != want.Width || got.Height != want.Height {
				t.Errorf("dimensions = %dx%d, want %dx%d", got.Width, got.Height, want.Width, want.Height)
			}
			if got.VideoCodec != want.VideoCodec || got.AudioCodec != want.AudioCodec {
				t.Errorf("codecs = %s/%s, want %s/%s",
					got.VideoCodec, got.AudioCodec, want.VideoCodec, want.AudioCodec)
			}
			// Matroska states a nominal frame rate and the output states a
			// measured one, so they agree to the rounding of one frame time.
			const frameRateTolerance = 0.01
			if diff := got.FrameRate - want.FrameRate; diff > frameRateTolerance || diff < -frameRateTolerance {
				t.Errorf("frame rate = %v, want %v", got.FrameRate, want.FrameRate)
			}
			if got.Rotation != 0 {
				t.Errorf("rotation = %d, want 0: Matroska has no display matrix to carry across", got.Rotation)
			}
		})
	}
}

// TestRemuxMatroskaIntoMP4KeepsTheThumbnail is the assertion the fragmented
// writer's whole decode timeline exists for: the frames come out at the times
// they went in, so a thumbnail of the output is the same picture at the same
// time as one of the source.
func TestRemuxMatroskaIntoMP4KeepsTheThumbnail(t *testing.T) {
	for _, name := range []string{"hevc-aac.mkv", "h264-aac.mkv", "h264-gop12.mkv", "av1-opus.webm"} {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			out := remuxed(t, name, sprocket.MP4)

			for _, at := range []time.Duration{0, 1300 * time.Millisecond} {
				want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), at, sprocket.ThumbnailOptions{})
				if err != nil && !h264Compiled && errors.Is(err, sprocket.ErrUnsupportedCodec) {
					t.Skipf("%s needs the h264 build tag", name)
				}
				if err != nil {
					t.Fatalf("thumbnail the source at %v: %v", at, err)
				}
				got, err := sprocket.Thumbnail(bytes.NewReader(out), int64(len(out)), at, sprocket.ThumbnailOptions{})
				if err != nil {
					t.Fatalf("thumbnail the remux at %v: %v", at, err)
				}
				if got.Time != want.Time {
					t.Errorf("at %v the remux's keyframe is at %v and the source's at %v", at, got.Time, want.Time)
				}
				if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
					t.Errorf("at %v the remux decoded a different picture", at)
				}
			}
		})
	}
}

// TestRemuxMatroskaIntoMP4RefusesWhatMP4CannotHold holds the compatibility
// table from the Matroska side: WebM may carry Vorbis and an mp4 may not.
func TestRemuxMatroskaIntoMP4RefusesWhatMP4CannotHold(t *testing.T) {
	source := readCorpus(t, "vp8-vorbis.webm")

	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.MP4)
	if !errors.Is(err, sprocket.ErrIncompatible) {
		t.Fatalf("error = %v, want ErrIncompatible", err)
	}
	if !strings.Contains(err.Error(), "vp8") {
		t.Errorf("error %q does not name the codec that blocked it", err)
	}
}

// TestRemuxMatroskaIntoMatroska is the fourth pairing: a Matroska source into a
// Matroska output, which is a filtered copy of the source's own clusters.
func TestRemuxMatroskaIntoMatroska(t *testing.T) {
	for _, name := range []string{"h264-aac.mkv", "vp8-vorbis.webm", "av1-opus.webm"} {
		t.Run(name, func(t *testing.T) {
			want := probeBytes(t, readCorpus(t, name))
			got := probeBytes(t, remuxed(t, name, sprocket.MKV))

			// Bitrate is the only field that may move: it is derived from the
			// file size, and the output drops whatever padding the source had.
			want.Bitrate, got.Bitrate = 0, 0
			if got != want {
				t.Errorf("probe of the copy = %+v, want %+v", got, want)
			}
		})
	}
}

// TestRemuxMatroskaHoldsBoundedMemory is the assertion the other two remux
// paths make, against a WebM whose one block declares 64 MiB: the writer costs
// its headers and one copy buffer, not the block.
func TestRemuxMatroskaHoldsBoundedMemory(t *testing.T) {
	const (
		frameSize   = int64(64) << 20
		allocBudget = uint64(4) << 20
	)

	for _, target := range []sprocket.Container{sprocket.MP4, sprocket.WebM} {
		t.Run(string(target), func(t *testing.T) {
			source, size := inflatedMatroskaSource(frameSize)
			out := &countingWriter{}

			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			if err := sprocket.Remux(source, size, out, target); err != nil {
				t.Fatalf("remux: %v", err)
			}
			runtime.ReadMemStats(&after)

			if out.n < frameSize {
				t.Fatalf("wrote %d bytes, want at least the %d byte frame", out.n, frameSize)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > allocBudget {
				t.Errorf("allocated %d bytes to move %d, want at most %d", grew, out.n, allocBudget)
			} else {
				t.Logf("allocated %d bytes to move %d, with heap at %d", grew, out.n, after.HeapAlloc)
			}
		})
	}
}

// TestTrimMatroskaSource cuts a Matroska file into each container the codecs
// allow. The start snaps back to the keyframe at 1.0 seconds, so the output
// holds the 1.2 seconds from there to the requested end.
func TestTrimMatroskaSource(t *testing.T) {
	for _, target := range []sprocket.Container{sprocket.MKV, sprocket.MP4} {
		t.Run(string(target), func(t *testing.T) {
			source := readCorpus(t, "h264-gop12.mkv")

			var out bytes.Buffer
			actual, err := sprocket.Trim(bytes.NewReader(source), int64(len(source)), &out,
				target, 1300*time.Millisecond, 2200*time.Millisecond)
			if err != nil {
				t.Fatalf("trim: %v", err)
			}
			if want := time.Second; actual != want {
				t.Errorf("the cut begins at %v, want %v", actual, want)
			}

			info := probeBytes(t, out.Bytes())
			const (
				want      = 1200 * time.Millisecond
				tolerance = 50 * time.Millisecond
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
			// The output has to begin on the keyframe the cut snapped to, and
			// it has to be the same picture the source shows there.
			want1s, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), actual, sprocket.ThumbnailOptions{})
			if err != nil {
				t.Fatalf("thumbnail the source at %v: %v", actual, err)
			}
			got, err := sprocket.Thumbnail(bytes.NewReader(out.Bytes()), int64(out.Len()), 0, sprocket.ThumbnailOptions{})
			if err != nil {
				t.Fatalf("thumbnail the cut: %v", err)
			}
			if got.Time != 0 {
				t.Errorf("the cut's first keyframe is at %v, want 0", got.Time)
			}
			if !bytes.Equal(got.Image.(*image.RGBA).Pix, want1s.Image.(*image.RGBA).Pix) {
				t.Error("the cut begins on a different picture from the source's keyframe at 1 second")
			}
		})
	}
}

// inflatedMatroskaSource is a one-block WebM whose frame declares size bytes,
// served by a reader that hands back zeros past the real bytes. Nothing is
// written to disk: it stands in for a huge file, the way the ISOBMFF side's
// inflated source does.
func inflatedMatroskaSource(size int64) (io.ReaderAt, int64) {
	// An EBML header naming webm, then a segment holding the timestamp scale
	// and duration, one VP8 track, and a cluster with one keyframe block.
	elem := func(id []byte, payload []byte) []byte {
		out := append([]byte{}, id...)
		out = binary.BigEndian.AppendUint64(out, uint64(len(payload)))
		out[len(id)] |= 0x01
		return append(out, payload...)
	}
	u := func(id []byte, v uint64) []byte {
		return elem(id, binary.BigEndian.AppendUint64(nil, v))
	}
	f := func(id []byte, v float64) []byte {
		return elem(id, binary.BigEndian.AppendUint64(nil, math.Float64bits(v)))
	}
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

	var (
		idEBML      = []byte{0x1A, 0x45, 0xDF, 0xA3}
		idDocType   = []byte{0x42, 0x82}
		idSegment   = []byte{0x18, 0x53, 0x80, 0x67}
		idInfo      = []byte{0x15, 0x49, 0xA9, 0x66}
		idScale     = []byte{0x2A, 0xD7, 0xB1}
		idDuration  = []byte{0x44, 0x89}
		idTracks    = []byte{0x16, 0x54, 0xAE, 0x6B}
		idEntry     = []byte{0xAE}
		idNumber    = []byte{0xD7}
		idType      = []byte{0x83}
		idCodecID   = []byte{0x86}
		idVideo     = []byte{0xE0}
		idWidth     = []byte{0xB0}
		idHeight    = []byte{0xBA}
		idCluster   = []byte{0x1F, 0x43, 0xB6, 0x75}
		idTimestamp = []byte{0xE7}
		idBlock     = []byte{0xA3}
	)

	// The block is a track number, a relative timestamp, keyframe flags, and
	// then the frame, whose bytes the reader invents.
	head := join(idBlock, binary.BigEndian.AppendUint64(nil, uint64(4+size)), []byte{0x81, 0, 0, 0x80})
	head[len(idBlock)] |= 0x01
	prefix := join(
		elem(idEBML, elem(idDocType, []byte("webm"))),
		idSegment, []byte{0xFF},
		elem(idInfo, join(u(idScale, 1_000_000), f(idDuration, 1000))),
		elem(idTracks, elem(idEntry, join(
			u(idNumber, 1), u(idType, 1), elem(idCodecID, []byte("V_VP9")),
			elem(idVideo, join(u(idWidth, 128), u(idHeight, 72))),
		))),
		idCluster, []byte{0xFF},
		u(idTimestamp, 0),
		head,
	)
	return &countingReaderAt{prefix: prefix, size: int64(len(prefix)) + size}, int64(len(prefix)) + size
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
