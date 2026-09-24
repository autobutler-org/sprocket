//go:build hevc

package decode_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
	"github.com/gen2brain/h265/hevc"
)

// spsNAL builds a sequence parameter set NAL unit declaring a 4:2:0 picture of
// the given coded size. Only the prefix the size check reads is real; the rest
// of the sequence parameter set is absent, which is enough because nothing gets
// as far as decoding one.
func spsNAL(width, height uint32) []byte {
	var body bitWriter
	body.u(4, 0) // sps_video_parameter_set_id.
	body.u(3, 0) // sps_max_sub_layers_minus1.
	body.u(1, 1) // sps_temporal_id_nesting_flag.
	for range 12 {
		body.u(8, 0) // profile_tier_level.
	}
	body.ue(0) // sps_seq_parameter_set_id.
	body.ue(1) // chroma_format_idc, 4:2:0.
	body.ue(width)
	body.ue(height)

	const (
		spsNALType = 33
		temporalID = 1
	)
	return append([]byte{spsNALType << 1, temporalID}, body.bytes()...)
}

// hvcCRecord wraps NAL units in an HEVCDecoderConfigurationRecord holding one
// array, with a four byte NAL length prefix.
func hvcCRecord(t testing.TB, nals ...[]byte) []byte {
	t.Helper()

	const (
		headerBytes       = 23
		lengthSizeMinus1  = 3
		arrayNALUnitType  = 33
		reservedThenSize  = 0xfc
		numOfArraysOffset = 22
	)
	record := make([]byte, headerBytes)
	record[0] = 1 // configurationVersion.
	record[headerBytes-2] = reservedThenSize | lengthSizeMinus1
	if len(nals) == 0 {
		return record
	}

	record[numOfArraysOffset] = 1
	record = append(record, arrayNALUnitType, 0, 0)
	binary.BigEndian.PutUint16(record[len(record)-2:], uint16(len(nals)))
	for _, nal := range nals {
		record = binary.BigEndian.AppendUint16(record, uint16(len(nal)))
		record = append(record, nal...)
	}
	return record
}

// encodeKeyframe codes one intra frame at a size that does not fill the coding
// tree grid, and returns it split the way a container splits it: the parameter
// sets in an hvcC record, the slices in a length-prefixed sample.
func encodeKeyframe(t testing.TB, width, height int, chroma hevc.ChromaFormat) (config, sample []byte) {
	t.Helper()

	encoder, err := hevc.NewEncoder(hevc.EncoderOptions{Width: width, Height: height, Chroma: chroma})
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	frame := hevc.Frame{Y: make([]uint8, width*height), StrideY: width}
	for i := range frame.Y {
		frame.Y[i] = byte(i)
	}
	if chroma != hevc.ChromaMono {
		chromaW, chromaH := width, height
		if chroma != hevc.Chroma444 {
			chromaW = (width + 1) / 2
		}
		if chroma == hevc.Chroma420 {
			chromaH = (height + 1) / 2
		}
		frame.Cb = make([]uint8, chromaW*chromaH)
		frame.Cr = make([]uint8, chromaW*chromaH)
		frame.StrideC = chromaW
	}

	nals, err := encoder.Encode(frame)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var params [][]byte
	for _, nal := range nals {
		if nal.Type.IsVCL() {
			unit := hevc.MarshalNAL(nal)
			sample = binary.BigEndian.AppendUint32(sample, uint32(len(unit)))
			sample = append(sample, unit...)
			continue
		}
		params = append(params, hevc.MarshalNAL(nal))
	}
	return hvcCRecord(t, params...), sample
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

func TestRGBAConvertsADecodedKeyframe(t *testing.T) {
	// The studio-range read is what the package documentation says a caller has
	// to do, so the darkest pixel of a real frame has to reach further down than
	// image.YCbCr's own full-range conversion puts it.
	img := keyframe(t, "hevc-aac-8bit.mov")
	got := decode.RGBA(decode.Picture{Image: img})

	var darkest, naive int
	darkest, naive = 255, 255
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			c := got.RGBAAt(x-bounds.Min.X, y-bounds.Min.Y)
			darkest = min(darkest, int(max(max(c.R, c.G), c.B)))
			r, g, b, _ := img.At(x, y).RGBA()
			naive = min(naive, int(max(max(r, g), b)>>8))
		}
	}
	if darkest >= naive {
		t.Errorf("darkest pixel is %d with the studio range applied and %d without, want it lower", darkest, naive)
	}
}

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
		picture, err := decode.Keyframe("hevc", config, fuzzNALLengthSize, nil)
		if err != nil {
			return
		}
		readEveryPixel(t, picture.Image)
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
		picture, err := decode.Keyframe("hevc", config, fuzzNALLengthSize, sample)
		if err != nil {
			return
		}
		readEveryPixel(t, picture.Image)
	})
}
