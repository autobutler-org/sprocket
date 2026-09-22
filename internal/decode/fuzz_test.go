package decode_test

import (
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// fuzzNALLengthSize is the prefix width every corpus file uses, and the one the
// fuzz targets hold fixed so that the input they vary is the bitstream.
const fuzzNALLengthSize = 4

// FuzzHVCC drives the configuration record parser over arbitrary bytes. The
// sample is empty on purpose: the parameter sets are read and the sequence size
// is checked, but no slice is ever decoded, so every execution is cheap and the
// input the fuzzer explores is the record itself.
func FuzzHVCC(f *testing.F) {
	f.Add(syncSample(f, "hevc-aac-8bit.mov").Config)
	f.Add(syncSample(f, "hevc-aac-10bit.mov").Config)
	f.Add(syncSample(f, "h264-aac.mp4").Config)
	f.Add(hvcCRecord(f, spsNAL(65535, 65535)))
	f.Add(make([]byte, 23))
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, config []byte) {
		if _, err := decode.Keyframe("hevc", config, fuzzNALLengthSize, nil); err == nil {
			t.Error("a sample with no slices decoded to a picture")
		}
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
		if bounds := img.Bounds(); bounds.Dx() <= 0 || bounds.Dy() <= 0 {
			t.Errorf("decoded an image of %v", bounds)
		}
		// Reading every pixel catches a plane shorter than the bounds claim,
		// which would otherwise only show up in a caller.
		for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
			for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
				_ = img.At(x, y)
			}
		}
	})
}
