//go:build h264 || hevc

package decode

import "fmt"

// eightBitPlane returns a cropped 8-bit view of one decoded plane. An 8-bit
// plane is aliased in place; a deeper one is shifted down into a packed copy,
// which loses the low bits and nothing else.
func eightBitPlane(p8 []uint8, p16 []uint16, depth, stride, x, y, width, height int) (plane, error) {
	if stride < width || x < 0 || y < 0 {
		return plane{}, fmt.Errorf("%w: a %dx%d plane at (%d,%d) has stride %d",
			ErrCorruptSample, width, height, x, y, stride)
	}
	offset := y*stride + x
	need := (height-1)*stride + width

	if p16 != nil {
		if len(p16)-offset < need {
			return plane{}, fmt.Errorf("%w: a %dx%d plane needs %d samples and has %d",
				ErrCorruptSample, width, height, need, len(p16)-offset)
		}
		shift := min(max(depth-8, 0), 8)
		src := p16[offset:]
		pix := make([]byte, width*height)
		for row := range height {
			in, out := src[row*stride:], pix[row*width:]
			for i := range width {
				out[i] = byte(in[i] >> shift)
			}
		}
		return plane{pix: pix, stride: width}, nil
	}
	if len(p8)-offset < need {
		return plane{}, fmt.Errorf("%w: a %dx%d plane needs %d samples and has %d",
			ErrCorruptSample, width, height, need, len(p8)-offset)
	}
	return plane{pix: p8[offset:], stride: stride}, nil
}
