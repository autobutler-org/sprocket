package decode_test

import (
	"bytes"
	"errors"
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/decode"
	"github.com/autobutler-org/sprocket/internal/matroska"
	"github.com/gen2brain/gav1d/av1"
)

// matroskaSample reads the first keyframe of a Matroska corpus file, which is
// what a thumbnail at t=0 decodes. VP8 and AV1 only reach this package through
// that container, so their tests start there rather than at an mp4.
func matroskaSample(t testing.TB, name string) matroska.SyncSample {
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
	parsed, err := matroska.Parse(file, stat.Size())
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	sample, err := parsed.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read keyframe of %s: %v", name, err)
	}
	return sample
}

// matroskaKeyframe decodes a Matroska corpus file's first keyframe.
func matroskaKeyframe(t testing.TB, name string) image.Image {
	t.Helper()

	sample := matroskaSample(t, name)
	picture, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return picture.Image
}

func TestKeyframeVP8AndAV1(t *testing.T) {
	for _, name := range []string{"vp8-vorbis.webm", "av1-opus.webm"} {
		t.Run(name, func(t *testing.T) {
			img := matroskaKeyframe(t, name)

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
		})
	}
}

// TestKeyframeVP8AndAV1Deterministic is the stand-in for a committed reference
// frame, as it is on the HEVC side: the same input decodes to the same bytes.
func TestKeyframeVP8AndAV1Deterministic(t *testing.T) {
	for _, name := range []string{"vp8-vorbis.webm", "av1-opus.webm"} {
		t.Run(name, func(t *testing.T) {
			first := matroskaKeyframe(t, name).(*image.YCbCr)
			second := matroskaKeyframe(t, name).(*image.YCbCr)

			if !bytes.Equal(first.Y, second.Y) {
				t.Error("two decodes of the same sample differ in luma")
			}
			if !bytes.Equal(first.Cb, second.Cb) || !bytes.Equal(first.Cr, second.Cr) {
				t.Error("two decodes of the same sample differ in chroma")
			}
		})
	}
}

// TestKeyframeVP9IsUnsupported holds the one codec in the WebM set that has no
// decoder. The file reads fine; there is just nothing to decode it with.
func TestKeyframeVP9IsUnsupported(t *testing.T) {
	sample := matroskaSample(t, "vp9-opus.webm")
	if sample.Codec != "vp9" || len(sample.Data) == 0 {
		t.Fatalf("read a %d byte %q keyframe, want a vp9 one", len(sample.Data), sample.Codec)
	}

	_, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
	if !errors.Is(err, decode.ErrUnsupportedCodec) {
		t.Errorf("error is %v, want ErrUnsupportedCodec", err)
	}
}

// TestKeyframeVP8AndAV1Reject feeds damaged samples to both decoders. Every one
// has to come back as an error rather than as a panic or as a picture.
func TestKeyframeVP8AndAV1Reject(t *testing.T) {
	cases := map[string]struct {
		codec string
		file  string
	}{
		"vp8": {codec: "vp8", file: "vp8-vorbis.webm"},
		"av1": {codec: "av1", file: "av1-opus.webm"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			whole := matroskaSample(t, c.file).Data
			garbage := make([]byte, len(whole))
			for i := range garbage {
				garbage[i] = byte(i*7 + 13)
			}

			for what, data := range map[string][]byte{
				"empty":     nil,
				"truncated": whole[:len(whole)/2],
				"four byte": whole[:4],
				"garbage":   garbage,
			} {
				t.Run(what, func(t *testing.T) {
					if _, err := decode.Keyframe(c.codec, nil, 0, data); err == nil {
						t.Error("a damaged sample decoded to a picture")
					}
				})
			}
		})
	}
}

// TestKeyframeReportsTheVP8AndAV1ColorDescription checks what each decoder
// says about color. The AV1 corpus file declares nothing, so its matrix is
// unspecified. VP8 has one color space, BT.601, so that is what it reports.
func TestKeyframeReportsTheVP8AndAV1ColorDescription(t *testing.T) {
	for name, want := range map[string]int{"av1-opus.webm": unspecified, "vp8-vorbis.webm": bt601} {
		t.Run(name, func(t *testing.T) {
			sample := matroskaSample(t, name)
			picture, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if picture.Matrix != want || picture.FullRange {
				t.Errorf("matrix %d, full range %v; want %d, false", picture.Matrix, picture.FullRange, want)
			}
		})
	}
}

// TestKeyframeReportsAnAV1FullRangeFlag encodes a frame that sets color_range,
// which the corpus has no example of, and checks the flag comes through.
func TestKeyframeReportsAnAV1FullRangeFlag(t *testing.T) {
	sample := av1.Encode(av1.EncodeConfig{Width: 16, Height: 16, BitDepth: 8, QIndex: 100, FullRange: true})
	if sample == nil {
		t.Fatal("the encoder refused the configuration")
	}
	picture, err := decode.Keyframe("av1", nil, 0, sample)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !picture.FullRange {
		t.Error("full range is false, want true")
	}
}
