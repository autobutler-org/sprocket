package decode

import (
	"image"
	"image/draw"
)

// Fixed-point conversion coefficients, 16.16, folding the studio-range scale
// into the matrix. Luma 16 becomes 0 and luma 235 becomes 255; chroma is
// centered on 128 and spans 16 to 240.
const (
	fixedOne  = 1 << 16
	fixedHalf = fixedOne / 2
	lumaFloor = 16
	chromaMid = 128
	lumaCoeff = 76309 // 255/219

	bt601RedCr   = 104597
	bt601GreenCb = -25675
	bt601GreenCr = -53279
	bt601BlueCb  = 132201

	bt709RedCr   = 117489
	bt709GreenCb = -13975
	bt709GreenCr = -34926
	bt709BlueCb  = 138438

	// bt709Lines is the picture height at which the matrix heuristic switches.
	// It is where the standards themselves divide: BT.601 defined standard
	// definition and BT.709 defined high definition at 720 lines.
	bt709Lines = 720
)

// RGBA converts a decoded picture to RGB, applying the range and the matrix
// that the "Color range" section of this package's documentation says a caller
// has to apply itself. The result is a fresh *image.RGBA with its origin at
// (0, 0), opaque everywhere, sharing nothing with the source.
//
// The planes are read as studio range, because the decoder does not pass the
// sequence's full-range flag on and video that carries one is rare. The matrix
// is chosen by picture height, BT.601 below 720 lines and BT.709 at or above,
// which is what players do for video that signals nothing.
//
// An *image.YCbCr is converted at any subsampling and an *image.Gray has its
// luma stretched to the full range. Anything else is copied through
// image/draw, which reads it by its own At method.
func RGBA(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	switch src := src.(type) {
	case *image.YCbCr:
		ycbcrToRGBA(src, dst)
	case *image.Gray:
		grayToRGBA(src, dst)
	default:
		draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	}
	return dst
}

// ycbcrToRGBA converts every pixel of a YCbCr picture. YOffset and COffset
// carry the subsampling, so one loop covers 4:2:0, 4:2:2, and 4:4:4.
func ycbcrToRGBA(src *image.YCbCr, dst *image.RGBA) {
	redCr, greenCb, greenCr, blueCb := bt601RedCr, bt601GreenCb, bt601GreenCr, bt601BlueCb
	if src.Bounds().Dy() >= bt709Lines {
		redCr, greenCb, greenCr, blueCb = bt709RedCr, bt709GreenCb, bt709GreenCr, bt709BlueCb
	}

	bounds := src.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		out := dst.Pix[(y-bounds.Min.Y)*dst.Stride:]
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			var (
				luma   = (int(src.Y[src.YOffset(x, y)]) - lumaFloor) * lumaCoeff
				chroma = src.COffset(x, y)
				blue   = int(src.Cb[chroma]) - chromaMid
				red    = int(src.Cr[chroma]) - chromaMid
				pixel  = out[(x-bounds.Min.X)*4:]
			)
			pixel[0] = clamp8(luma + red*redCr)
			pixel[1] = clamp8(luma + blue*greenCb + red*greenCr)
			pixel[2] = clamp8(luma + blue*blueCb)
			pixel[3] = 0xff
		}
	}
}

// grayToRGBA stretches a monochrome plane from studio range to full range.
func grayToRGBA(src *image.Gray, dst *image.RGBA) {
	bounds := src.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		out := dst.Pix[(y-bounds.Min.Y)*dst.Stride:]
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			value := clamp8((int(src.Pix[src.PixOffset(x, y)]) - lumaFloor) * lumaCoeff)
			pixel := out[(x-bounds.Min.X)*4:]
			pixel[0], pixel[1], pixel[2], pixel[3] = value, value, value, 0xff
		}
	}
}

// clamp8 rounds a 16.16 fixed-point value to a byte, clamping out of range
// values rather than wrapping them.
func clamp8(fixed int) uint8 {
	value := (fixed + fixedHalf) >> 16
	if value < 0 {
		return 0
	}
	if value > 0xff {
		return 0xff
	}
	return uint8(value)
}
