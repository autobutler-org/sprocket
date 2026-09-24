package decode_test

import (
	"bytes"
	"errors"
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/decode"
	"github.com/autobutler-org/sprocket/internal/isobmff"
	"github.com/gen2brain/h265/hevc"
)

// syncSample reads the first sync sample of a corpus file, which is what a
// thumbnail at t=0 decodes.
func syncSample(t testing.TB, name string) isobmff.SyncSample {
	t.Helper()

	file, _, err := corpus.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { file.Close() })

	stat, err := file.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	parsed, err := isobmff.Parse(file, stat.Size())
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	sample, err := parsed.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read sync sample of %s: %v", name, err)
	}
	return sample
}

// keyframe decodes a corpus file's first sync sample.
func keyframe(t testing.TB, name string) image.Image {
	t.Helper()

	sample := syncSample(t, name)
	picture, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return picture.Image
}

// luma reports the distinct luma values in an image and the mean luma of its
// top-left and bottom-right quadrants. A decode that returns a blank or a
// constant frame fails on the first; one that returns the same block of pixels
// everywhere fails on the second.
func luma(t testing.TB, img image.Image) (distinct int, topLeft, bottomRight float64) {
	t.Helper()

	ycc, ok := img.(*image.YCbCr)
	if !ok {
		t.Fatalf("image is %T, want *image.YCbCr", img)
	}
	var (
		seen             [256]bool
		sumTL, sumBR     float64
		countTL, countBR int
	)
	bounds := ycc.Bounds()
	midX, midY := bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/2
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			value := ycc.Y[ycc.YOffset(x, y)]
			seen[value] = true
			switch {
			case x < midX && y < midY:
				sumTL += float64(value)
				countTL++
			case x >= midX && y >= midY:
				sumBR += float64(value)
				countBR++
			}
		}
	}
	for _, ok := range seen {
		if ok {
			distinct++
		}
	}
	return distinct, sumTL / float64(countTL), sumBR / float64(countBR)
}

func TestKeyframeEightBit(t *testing.T) {
	img := keyframe(t, "hevc-aac-8bit.mov")

	if _, ok := img.(*image.YCbCr); !ok {
		t.Fatalf("image is %T, want *image.YCbCr", img)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 128, 72); got != want {
		t.Errorf("bounds are %v, want %v", got, want)
	}

	distinct, topLeft, bottomRight := luma(t, img)
	if distinct < 16 {
		t.Errorf("frame has %d distinct luma values, want at least 16", distinct)
	}
	if diff := topLeft - bottomRight; diff < 4 && diff > -4 {
		t.Errorf("top-left mean luma %.1f and bottom-right %.1f are too close to be a test pattern",
			topLeft, bottomRight)
	}
}

func TestKeyframeTenBit(t *testing.T) {
	img := keyframe(t, "hevc-aac-10bit.mov")

	ycc, ok := img.(*image.YCbCr)
	if !ok {
		t.Fatalf("image is %T, want *image.YCbCr", img)
	}
	if got, want := img.Bounds(), image.Rect(0, 0, 128, 72); got != want {
		t.Errorf("bounds are %v, want %v", got, want)
	}
	if ycc.SubsampleRatio != image.YCbCrSubsampleRatio420 {
		t.Errorf("subsample ratio is %v, want 4:2:0", ycc.SubsampleRatio)
	}

	distinct, topLeft, bottomRight := luma(t, img)
	if distinct < 16 {
		t.Errorf("frame has %d distinct luma values, want at least 16", distinct)
	}
	if diff := topLeft - bottomRight; diff < 4 && diff > -4 {
		t.Errorf("top-left mean luma %.1f and bottom-right %.1f are too close to be a test pattern",
			topLeft, bottomRight)
	}
}

// TestKeyframeDeterministic is the stand-in for a committed reference frame:
// the same input decodes to the same bytes, so a change in the decoder or in
// the plane handling shows up as a test failure rather than as a subtly
// different thumbnail.
func TestKeyframeDeterministic(t *testing.T) {
	for _, name := range []string{"hevc-aac-8bit.mov", "hevc-aac-10bit.mov"} {
		t.Run(name, func(t *testing.T) {
			first, second := keyframe(t, name).(*image.YCbCr), keyframe(t, name).(*image.YCbCr)

			if !bytes.Equal(first.Y, second.Y) {
				t.Error("two decodes of the same sample differ in luma")
			}
			if !bytes.Equal(first.Cb, second.Cb) || !bytes.Equal(first.Cr, second.Cr) {
				t.Error("two decodes of the same sample differ in chroma")
			}
		})
	}
}

func TestKeyframeUnsupportedCodec(t *testing.T) {
	sample := syncSample(t, "hevc-aac-8bit.mov")

	for _, codec := range []string{"vp9", "mp3", "", "HEVC"} {
		t.Run(codec, func(t *testing.T) {
			_, err := decode.Keyframe(codec, sample.Config, sample.NALLengthSize, sample.Data)
			if !errors.Is(err, decode.ErrUnsupportedCodec) {
				t.Errorf("error is %v, want ErrUnsupportedCodec", err)
			}
		})
	}
}

// TestKeyframeRejects feeds damaged configuration records and samples through
// the decoder. Every one must come back as an error rather than as a panic or
// as a plausible-looking picture.
func TestKeyframeRejects(t *testing.T) {
	sample := syncSample(t, "hevc-aac-8bit.mov")
	avcC := syncSample(t, "h264-aac.mp4").Config

	garbage := make([]byte, len(sample.Data))
	for i := range garbage {
		garbage[i] = byte(i*7 + 13)
	}
	noArrays := bytes.Clone(sample.Config)[:23]
	noArrays[22] = 0

	tests := []struct {
		name          string
		config        []byte
		nalLengthSize int
		data          []byte
	}{
		{"truncated sample", sample.Config, sample.NALLengthSize, sample.Data[:len(sample.Data)/2]},
		{"four byte sample", sample.Config, sample.NALLengthSize, sample.Data[:4]},
		{"garbage sample", sample.Config, sample.NALLengthSize, garbage},
		{"empty sample", sample.Config, sample.NALLengthSize, nil},
		{"avcC as hvcC", avcC, sample.NALLengthSize, sample.Data},
		{"no parameter set arrays", noArrays, sample.NALLengthSize, sample.Data},
		{"empty config", nil, sample.NALLengthSize, sample.Data},
		{"truncated config", sample.Config[:len(sample.Config)/2], sample.NALLengthSize, sample.Data},
		{"zero length prefix", sample.Config, 0, sample.Data},
		{"five byte length prefix", sample.Config, 5, sample.Data},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			picture, err := decode.Keyframe("hevc", test.config, test.nalLengthSize, test.data)
			if err == nil {
				t.Fatalf("decoded %v with no error", picture.Image.Bounds())
			}
			if !errors.Is(err, decode.ErrCorruptSample) && !errors.Is(err, decode.ErrUnsupportedCodec) {
				t.Errorf("error is %v, want ErrCorruptSample or ErrUnsupportedCodec", err)
			}
		})
	}
}

// TestKeyframeChroma checks the conformance window and every chroma sampling
// the decoder can hand back. The corpus is all 4:2:0 at a multiple of the
// coding tree size, so the frames are made here: an encoder pads anything that
// does not fill the grid and crops it back with a conformance window.
func TestKeyframeChroma(t *testing.T) {
	const width, height = 66, 34

	tests := []struct {
		name   string
		chroma hevc.ChromaFormat
		ratio  image.YCbCrSubsampleRatio
		gray   bool
	}{
		{"4:2:0", hevc.Chroma420, image.YCbCrSubsampleRatio420, false},
		{"4:2:2", hevc.Chroma422, image.YCbCrSubsampleRatio422, false},
		{"4:4:4", hevc.Chroma444, image.YCbCrSubsampleRatio444, false},
		{"monochrome", hevc.ChromaMono, 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, sample := encodeKeyframe(t, width, height, test.chroma)
			picture, err := decode.Keyframe("hevc", config, 4, sample)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			img := picture.Image
			if got, want := img.Bounds(), image.Rect(0, 0, width, height); got != want {
				t.Errorf("bounds are %v, want %v", got, want)
			}

			if test.gray {
				gray, ok := img.(*image.Gray)
				if !ok {
					t.Fatalf("image is %T, want *image.Gray", img)
				}
				if gray.Stride <= width {
					t.Errorf("stride is %d, want more than the %d pixel width so the crop is real",
						gray.Stride, width)
				}
				return
			}
			ycc, ok := img.(*image.YCbCr)
			if !ok {
				t.Fatalf("image is %T, want *image.YCbCr", img)
			}
			if ycc.SubsampleRatio != test.ratio {
				t.Errorf("subsample ratio is %v, want %v", ycc.SubsampleRatio, test.ratio)
			}
			if ycc.YStride <= width {
				t.Errorf("luma stride is %d, want more than the %d pixel width so the crop is real",
					ycc.YStride, width)
			}
		})
	}
}

// TestKeyframeFrameTooLarge rewrites the sequence parameter set's coded
// dimensions to something no level allows, which the package refuses before the
// decoder allocates a plane for it.
func TestKeyframeFrameTooLarge(t *testing.T) {
	sample := syncSample(t, "hevc-aac-8bit.mov")

	config := hvcCRecord(t, spsNAL(65535, 65535))
	_, err := decode.Keyframe("hevc", config, sample.NALLengthSize, sample.Data)
	if !errors.Is(err, decode.ErrFrameTooLarge) {
		t.Errorf("error is %v, want ErrFrameTooLarge", err)
	}
}

// TestKeyframeReportsTheHEVCColorDescription checks that the matrix and range
// the sequence declares come back with the picture. The corpus is encoded with
// no color description, which ffprobe reports as an unknown color space, so
// what comes back is H.273's unspecified.
func TestKeyframeReportsTheHEVCColorDescription(t *testing.T) {
	for _, name := range []string{"hevc-aac-8bit.mov", "hevc-aac-10bit.mov"} {
		t.Run(name, func(t *testing.T) {
			sample := syncSample(t, name)
			picture, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if picture.Matrix != unspecified || picture.FullRange {
				t.Errorf("matrix %d, full range %v; want %d, false", picture.Matrix, picture.FullRange, unspecified)
			}
		})
	}
}
