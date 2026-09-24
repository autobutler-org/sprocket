//go:build hevc

package decode

import (
	"errors"
	"fmt"
	"image"
	"runtime"

	"github.com/gen2brain/h265/hevc"
)

// hevcKeyframe decodes an HEVC keyframe. The parameter sets come from the hvcC
// record and the slices from the sample, which is what a container splits
// between the two.
func hevcKeyframe(config []byte, nalLengthSize int, sample []byte) (Picture, error) {
	if nalLengthSize < 1 || nalLengthSize > 4 {
		return Picture{}, fmt.Errorf("%w: the NAL length prefix is %d bytes, want 1 through 4",
			ErrCorruptSample, nalLengthSize)
	}
	nals, err := hvccParameterSets(config)
	if err != nil {
		return Picture{}, err
	}
	nals = append(nals, hevc.SplitHVCC(sample, nalLengthSize)...)
	if err := checkSequenceSize(nals); err != nil {
		return Picture{}, err
	}

	var decoder hevc.Decoder
	decoder.Threads(min(runtime.GOMAXPROCS(0), maxThreads))
	decoder.FrameSizeLimit(maxLumaSamples)

	var picture *hevc.Picture
	for _, nal := range nals {
		pictures, err := decoder.DecodeNAL(nal)
		if err != nil {
			return Picture{}, decodeError(err)
		}
		if len(pictures) > 0 {
			picture = pictures[0]
		}
	}
	if picture == nil {
		// A single access unit is normally released by the next one, so a
		// keyframe on its own usually surfaces here rather than above.
		if pictures := decoder.Flush(); len(pictures) > 0 {
			picture = pictures[0]
		}
	}
	if picture == nil {
		return Picture{}, fmt.Errorf("%w: the decoder read %d NAL units and produced no picture",
			ErrCorruptSample, len(nals))
	}
	img, err := pictureImage(picture)
	if err != nil {
		return Picture{}, err
	}
	return Picture{Image: img, Matrix: int(picture.ColorMatrix), FullRange: picture.FullRange}, nil
}

// decodeError maps what the decoder reports onto this package's sentinels.
func decodeError(err error) error {
	switch {
	case errors.Is(err, hevc.ErrUnsupported):
		// The decoder returns this both for a coding tool it does not implement
		// and for a sequence over FrameSizeLimit. checkSequenceSize has already
		// refused the second, so what is left is the first.
		return fmt.Errorf("%w: hevc: %w", ErrUnsupportedCodec, err)
	default:
		return fmt.Errorf("%w: %w", ErrCorruptSample, err)
	}
}

// checkSequenceSize refuses a sequence whose pictures are over the cap. The
// decoder enforces the same bound, but it reports a picture over the cap and a
// coding tool it does not implement as the same error, and the two mean
// different things to a caller.
func checkSequenceSize(nals []hevc.NALUnit) error {
	for _, nal := range nals {
		if nal.Type != hevc.NALSPS {
			continue
		}
		width, height, ok := spsPictureSize(nal.RBSP)
		if !ok {
			return fmt.Errorf("%w: unreadable sequence parameter set", ErrCorruptSample)
		}
		if int64(width)*int64(height) > maxLumaSamples {
			return fmt.Errorf("%w: %dx%d is %d luma samples, over the %d sample cap",
				ErrFrameTooLarge, width, height, int64(width)*int64(height), int64(maxLumaSamples))
		}
	}
	return nil
}

// pictureImage wraps a decoded picture as an image. An 8-bit picture aliases the
// decoder's planes, which is safe because the decoder is dropped with this
// call's frame and the picture is never released back to it.
func pictureImage(picture *hevc.Picture) (image.Image, error) {
	width, height := picture.CropW, picture.CropH
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w: the conformance window is %dx%d", ErrCorruptSample, width, height)
	}
	rect := image.Rect(0, 0, width, height)

	// picture.ChromaFormat is chroma_format_idc of 7.4.3.2, not the encoder's
	// ChromaFormat constants.
	var ratio image.YCbCrSubsampleRatio
	subX, subY := 1, 1
	switch picture.ChromaFormat {
	case 0: // Monochrome, which has no chroma planes at all.
	case 1:
		ratio, subX, subY = image.YCbCrSubsampleRatio420, 2, 2
	case 2:
		ratio, subX = image.YCbCrSubsampleRatio422, 2
	case 3:
		ratio = image.YCbCrSubsampleRatio444
	default:
		return nil, fmt.Errorf("%w: chroma_format_idc %d", ErrCorruptSample, picture.ChromaFormat)
	}

	luma, err := eightBitPlane(picture.Y, picture.Y16, picture.BitDepth,
		picture.StrideY, picture.CropX, picture.CropY, width, height)
	if err != nil {
		return nil, err
	}
	if picture.ChromaFormat == 0 {
		return &image.Gray{Pix: luma.pix, Stride: luma.stride, Rect: rect}, nil
	}

	chromaW, chromaH := (width+subX-1)/subX, (height+subY-1)/subY
	blue, err := eightBitPlane(picture.Cb, picture.Cb16, picture.BitDepthC,
		picture.StrideC, picture.CropX/subX, picture.CropY/subY, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	red, err := eightBitPlane(picture.Cr, picture.Cr16, picture.BitDepthC,
		picture.StrideC, picture.CropX/subX, picture.CropY/subY, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	return &image.YCbCr{
		Y: luma.pix, Cb: blue.pix, Cr: red.pix,
		YStride: luma.stride, CStride: blue.stride,
		SubsampleRatio: ratio, Rect: rect,
	}, nil
}
