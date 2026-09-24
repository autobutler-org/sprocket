//go:build h264

package decode_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// h264SPS builds a Baseline profile sequence parameter set NAL unit declaring a
// picture of the given size in macroblocks, coded as frames or as fields.
// Baseline is the profile with no chroma format or bit depth fields, so the
// record is short, and every field after those two is the smallest value that
// parses. Nothing decodes one of these: both of the things they state are
// refused before any slice data is read.
func h264SPS(widthMbs, heightMbs uint32, frameMbsOnly bool) []byte {
	mbsOnly := uint32(0)
	if frameMbsOnly {
		mbsOnly = 1
	}

	var body bitWriter
	body.u(8, 66) // profile_idc, Baseline.
	body.u(8, 0)  // The constraint flags and the reserved bits under them.
	body.u(8, 40) // level_idc.
	body.ue(0)    // seq_parameter_set_id.
	body.ue(0)    // log2_max_frame_num_minus4.
	body.ue(0)    // pic_order_cnt_type.
	body.ue(0)    // log2_max_pic_order_cnt_lsb_minus4.
	body.ue(1)    // max_num_ref_frames.
	body.u(1, 0)  // gaps_in_frame_num_value_allowed_flag.
	body.ue(widthMbs - 1)
	body.ue(heightMbs - 1)
	body.u(1, mbsOnly) // frame_mbs_only_flag.
	if mbsOnly == 0 {
		body.u(1, 0) // mb_adaptive_frame_field_flag.
	}
	body.u(1, 1) // direct_8x8_inference_flag.
	body.u(1, 0) // frame_cropping_flag.
	body.u(1, 0) // vui_parameters_present_flag.
	body.u(1, 1) // rbsp_stop_one_bit, then bytes() pads the alignment zeros.

	const spsNALHeader = 0x67 // nal_ref_idc 3, nal_unit_type 7.
	return append([]byte{spsNALHeader}, body.bytes()...)
}

// avcCWithSPS rebuilds an avcC record around one sequence parameter set. The
// record's own picture parameter sets are kept, so the sample's slice still
// finds the picture parameter set it names and through it the new sequence.
func avcCWithSPS(t testing.TB, config, sps []byte) []byte {
	t.Helper()

	offset := 6
	for range int(config[5] & 0x1f) {
		offset += 2 + int(binary.BigEndian.Uint16(config[offset:]))
	}
	const oneSequenceParameterSet = 0xe0 | 1 // Three reserved bits, then five of count.
	record := append(bytes.Clone(config[:5]), oneSequenceParameterSet)
	record = binary.BigEndian.AppendUint16(record, uint16(len(sps)))
	record = append(record, sps...)
	return append(record, config[offset:]...)
}

// TestKeyframeH264 decodes the corpus keyframe out of two files, so the adapter
// is shown reading a record and a sample rather than one file's bytes.
func TestKeyframeH264(t *testing.T) {
	for _, name := range []string{"h264-aac.mp4", "no-audio.mp4"} {
		t.Run(name, func(t *testing.T) {
			img := keyframe(t, name)

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

func TestKeyframeH264Deterministic(t *testing.T) {
	first, second := keyframe(t, "h264-aac.mp4").(*image.YCbCr), keyframe(t, "h264-aac.mp4").(*image.YCbCr)

	if !bytes.Equal(first.Y, second.Y) {
		t.Error("two decodes of the same sample differ in luma")
	}
	if !bytes.Equal(first.Cb, second.Cb) || !bytes.Equal(first.Cr, second.Cr) {
		t.Error("two decodes of the same sample differ in chroma")
	}
}

// TestKeyframeH264Rejects feeds damaged configuration records and samples
// through the decoder. Every one must come back as an error rather than as a
// panic or as a plausible-looking picture.
func TestKeyframeH264Rejects(t *testing.T) {
	sample := syncSample(t, "h264-aac.mp4")
	hvcC := syncSample(t, "hevc-aac-8bit.mov").Config

	garbage := make([]byte, len(sample.Data))
	for i := range garbage {
		garbage[i] = byte(i*7 + 13)
	}
	noSPS := bytes.Clone(sample.Config)
	noSPS[5] &^= 0x1f

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
		{"hvcC as avcC", hvcC, sample.NALLengthSize, sample.Data},
		{"no sequence parameter sets", noSPS, sample.NALLengthSize, sample.Data},
		{"empty config", nil, sample.NALLengthSize, sample.Data},
		{"truncated config", sample.Config[:len(sample.Config)/2], sample.NALLengthSize, sample.Data},
		{"zero length prefix", sample.Config, 0, sample.Data},
		{"five byte length prefix", sample.Config, 5, sample.Data},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			picture, err := decode.Keyframe("h264", test.config, test.nalLengthSize, test.data)
			if err == nil {
				t.Fatalf("decoded %v with no error", picture.Image.Bounds())
			}
			if !errors.Is(err, decode.ErrCorruptSample) && !errors.Is(err, decode.ErrUnsupportedCodec) {
				t.Errorf("error is %v, want ErrCorruptSample or ErrUnsupportedCodec", err)
			}
		})
	}
}

// TestKeyframeH264FrameTooLarge swaps in a sequence parameter set declaring a
// picture no level allows, which is refused before a plane is allocated for it.
// 1024 by 1024 macroblocks is 16384x16384 luma samples, over the cap and inside
// the decoder's own syntax bound, so the size is what is refused rather than
// the parse.
func TestKeyframeH264FrameTooLarge(t *testing.T) {
	sample := syncSample(t, "h264-aac.mp4")

	config := avcCWithSPS(t, sample.Config, h264SPS(1024, 1024, true))
	_, err := decode.Keyframe("h264", config, sample.NALLengthSize, sample.Data)
	if !errors.Is(err, decode.ErrFrameTooLarge) {
		t.Errorf("error is %v, want ErrFrameTooLarge", err)
	}
}

// TestKeyframeH264UnsupportedTool swaps in a sequence parameter set that codes
// fields rather than frames, which is one of the coding tools the decoder does
// not implement. A file this library will never decode has to say so, since
// that is the error a caller falls back on.
func TestKeyframeH264UnsupportedTool(t *testing.T) {
	sample := syncSample(t, "h264-aac.mp4")

	config := avcCWithSPS(t, sample.Config, h264SPS(8, 4, false))
	_, err := decode.Keyframe("h264", config, sample.NALLengthSize, sample.Data)
	if !errors.Is(err, decode.ErrUnsupportedCodec) {
		t.Errorf("error is %v, want ErrUnsupportedCodec", err)
	}
}

// FuzzAVCC drives the configuration record parser and the Annex B builder over
// arbitrary bytes. The sample is empty on purpose: the parameter sets are read
// but no slice is ever decoded, so every execution is cheap and the input the
// fuzzer explores is the record itself.
func FuzzAVCC(f *testing.F) {
	config := syncSample(f, "h264-aac.mp4").Config
	f.Add(config)
	f.Add(avcCWithSPS(f, config, h264SPS(1024, 1024, true)))
	f.Add(syncSample(f, "hevc-aac-8bit.mov").Config)
	f.Add(make([]byte, 6))
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, config []byte) {
		if _, err := decode.Keyframe("h264", config, fuzzNALLengthSize, nil); err == nil {
			t.Error("a sample with no slices decoded to a picture")
		}
	})
}

// FuzzH264Keyframe drives a whole decode, configuration record and sample
// together.
func FuzzH264Keyframe(f *testing.F) {
	for _, name := range []string{"h264-aac.mp4", "no-audio.mp4"} {
		sample := syncSample(f, name)
		f.Add(sample.Config, sample.Data)
	}
	f.Add([]byte(nil), []byte(nil))

	f.Fuzz(func(t *testing.T, config, sample []byte) {
		picture, err := decode.Keyframe("h264", config, fuzzNALLengthSize, sample)
		if err != nil {
			return
		}
		img := picture.Image
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

// TestKeyframeH264IsUnsignaled holds what the H.264 path says about color: the
// decoder does not expose the sequence's color description, so the picture
// comes back unspecified whatever the stream declares.
func TestKeyframeH264IsUnsignaled(t *testing.T) {
	sample := syncSample(t, "h264-aac.mp4")
	picture, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if picture.Matrix != unspecified || picture.FullRange {
		t.Errorf("matrix %d, full range %v; want %d, false", picture.Matrix, picture.FullRange, unspecified)
	}
}
