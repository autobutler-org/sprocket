package decode_test

import (
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// fuzzNALLengthSize is the prefix width every corpus file uses, and the one the
// fuzz targets hold fixed so that the input they vary is the bitstream.
const fuzzNALLengthSize = 4

// FuzzHVCC drives the configuration record parser over arbitrary bytes. The
// sample is empty on purpose, so that the input the fuzzer explores is the
// record itself.
//
// A picture is a legal outcome and not a failure. An hvcC holds arrays of NAL
// units and nothing restricts them to parameter sets, so a record carrying a
// slice decodes to a picture with no sample at all; the corpus holds an input
// the fuzzer found that does exactly that. What is checked is what a caller
// would go on to do with whatever comes back.
func FuzzHVCC(f *testing.F) {
	f.Add(syncSample(f, "hevc-aac-8bit.mov").Config)
	f.Add(syncSample(f, "hevc-aac-10bit.mov").Config)
	f.Add(syncSample(f, "h264-aac.mp4").Config)
	f.Add(hvcCRecord(f, spsNAL(65535, 65535)))
	f.Add(make([]byte, 23))
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, config []byte) {
		img, err := decode.Keyframe("hevc", config, fuzzNALLengthSize, nil)
		if err != nil {
			return
		}
		readEveryPixel(t, img)
	})
}

// FuzzKeyframe drives a whole decode, configuration record and sample together.
func FuzzKeyframe(f *testing.F) {
	for _, name := range []string{"hevc-aac-8bit.mov", "hevc-aac-10bit.mov"} {
		sample := syncSample(f, name)
		f.Add(sample.Config, sample.Data)
	}
	f.Add([]byte(nil), []byte(nil))

	f.Fuzz(func(t *testing.T, config, sample []byte) {
		img, err := decode.Keyframe("hevc", config, fuzzNALLengthSize, sample)
		if err != nil {
			return
		}
		readEveryPixel(t, img)
	})
}

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

	img, err := decode.Keyframe(codec, nil, 0, sample)
	if err != nil {
		return
	}
	readEveryPixel(t, img)
	_ = decode.RGBA(img)
}
