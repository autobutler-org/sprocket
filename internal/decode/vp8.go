package decode

import (
	"bytes"
	"fmt"

	"golang.org/x/image/vp8"
)

// vp8Keyframe decodes a VP8 keyframe. A VP8 frame in a container is the raw
// bitstream with nothing wrapped round it, so there is no configuration record
// to read and no framing to strip: the whole block payload goes straight in.
//
// VP8 has one color space: RFC 6386 section 9.2 defines color_space 0 as
// BT.601 and leaves 1 reserved, and says nothing of range, which libvpx takes
// as studio. So every picture reports BT.601 studio range, and the bit is not
// read.
//
// The decoder reads the frame header first, which is where the picture size
// comes from, so the size cap is applied before a plane is allocated for it.
func vp8Keyframe(sample []byte) (Picture, error) {
	decoder := vp8.NewDecoder()
	decoder.Init(bytes.NewReader(sample), len(sample))

	header, err := decoder.DecodeFrameHeader()
	if err != nil {
		return Picture{}, fmt.Errorf("%w: vp8: %w", ErrCorruptSample, err)
	}
	if !header.KeyFrame {
		return Picture{}, fmt.Errorf("%w: the frame is not a keyframe, so it has no picture of its own", ErrCorruptSample)
	}
	if header.Width <= 0 || header.Height <= 0 {
		return Picture{}, fmt.Errorf("%w: the frame header declares a %dx%d picture",
			ErrCorruptSample, header.Width, header.Height)
	}
	if area := int64(header.Width) * int64(header.Height); area > maxLumaSamples {
		return Picture{}, fmt.Errorf("%w: %dx%d is %d luma samples, over the %d sample cap",
			ErrFrameTooLarge, header.Width, header.Height, area, int64(maxLumaSamples))
	}

	// VP8 codes 8-bit 4:2:0 and nothing else, so the picture comes back as an
	// *image.YCbCr with no depth or layout to branch on.
	picture, err := decoder.DecodeFrame()
	if err != nil {
		return Picture{}, fmt.Errorf("%w: vp8: %w", ErrCorruptSample, err)
	}
	if picture == nil {
		return Picture{}, fmt.Errorf("%w: the decoder read the frame and produced no picture", ErrCorruptSample)
	}
	return Picture{Image: picture, Matrix: matrixBT601}, nil
}
