package decode_test

import (
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// readEveryPixel checks a decoded picture the way a caller would use it.
// Reading every pixel catches a plane shorter than the bounds claim, which
// would otherwise only show up in whatever drew the image.
func readEveryPixel(t *testing.T, img image.Image) {
	t.Helper()

	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		t.Errorf("decoded an image of %v", bounds)
		return
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_ = img.At(x, y)
		}
	}
}

// FuzzKeyframeVP8 and FuzzKeyframeAV1 drive the two codecs whose frames carry
// no configuration record, so the input the fuzzer varies is the whole of what
// the decoder is given.
func FuzzKeyframeVP8(f *testing.F) {
	f.Add(matroskaSample(f, "vp8-vorbis.webm").Data)
	f.Add([]byte(nil))
	f.Add([]byte{0x9d, 0x01, 0x2a})

	f.Fuzz(func(t *testing.T, sample []byte) { fuzzDecode(t, "vp8", sample) })
}

func FuzzKeyframeAV1(f *testing.F) {
	f.Add(matroskaSample(f, "av1-opus.webm").Data)
	f.Add([]byte(nil))
	f.Add([]byte{0x12, 0x00})

	f.Fuzz(func(t *testing.T, sample []byte) { fuzzDecode(t, "av1", sample) })
}

// fuzzDecode decodes one sample and reads every pixel of whatever comes back,
// which catches a plane shorter than the bounds claim.
func fuzzDecode(t *testing.T, codec string, sample []byte) {
	t.Helper()

	picture, err := decode.Keyframe(codec, nil, 0, sample)
	if err != nil {
		return
	}
	readEveryPixel(t, picture.Image)
	_ = decode.RGBA(picture)
}
