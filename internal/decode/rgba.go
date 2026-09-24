package decode

import (
	"image"
	"image/draw"
	"math"
)

// Fixed-point arithmetic is 16.16. Studio range puts luma on 16 to 235 and
// chroma on 16 to 240 centered on 128; full range puts both on 0 to 255, with
// chroma still centered on 128.
const (
	fixedOne  = 1 << 16
	fixedHalf = fixedOne / 2
	lumaFloor = 16
	chromaMid = 128
)

// conversion is one matrix and range folded into fixed-point coefficients.
type conversion struct {
	floor, luma, redCr, greenCb, greenCr, blueCb int
}

// newConversion works out the coefficients for a matrix and a range from the
// luma weights Kr and Kb of each standard. A matrix this package has no
// weights for, unspecified among them, gets BT.601's, which is what ffmpeg
// does.
func newConversion(matrix int, fullRange bool) conversion {
	kr, kb := 0.299, 0.114
	switch matrix {
	case matrixBT709:
		kr, kb = 0.2126, 0.0722
	case matrixBT2020:
		kr, kb = 0.2627, 0.0593
	}
	floor, lumaScale, chromaScale := lumaFloor, 255.0/219, 255.0/224
	if fullRange {
		floor, lumaScale, chromaScale = 0, 1, 1
	}
	kg := 1 - kr - kb
	fixed := func(v float64) int { return int(math.Round(v * fixedOne)) }
	return conversion{
		floor:   floor,
		luma:    fixed(lumaScale),
		redCr:   fixed(2 * (1 - kr) * chromaScale),
		greenCb: fixed(-2 * kb * (1 - kb) / kg * chromaScale),
		greenCr: fixed(-2 * kr * (1 - kr) / kg * chromaScale),
		blueCb:  fixed(2 * (1 - kb) * chromaScale),
	}
}

// RGBA converts a decoded picture to RGB, applying the range and the matrix
// that the "Color range" section of this package's documentation says a caller
// has to apply itself. The result is a fresh *image.RGBA with its origin at
// (0, 0), opaque everywhere, sharing nothing with the source.
//
// The matrix and the range are the ones the picture reports. BT.709, BT.601,
// and BT.2020 are converted as themselves; an unspecified matrix, or any other,
// is read as BT.601 whatever the picture's size, which is what ffmpeg does.
//
// An *image.YCbCr is converted at any subsampling, and an *image.Gray has a
// studio-range luma stretched to the full range. Anything else is copied
// through image/draw, which reads it by its own At method.
func RGBA(picture Picture) *image.RGBA {
	src := picture.Image
	bounds := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	switch src := src.(type) {
	case *image.YCbCr:
		ycbcrToRGBA(src, dst, newConversion(picture.Matrix, picture.FullRange))
	case *image.Gray:
		grayToRGBA(src, dst, newConversion(picture.Matrix, picture.FullRange))
	default:
		draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	}
	return dst
}

// ycbcrToRGBA converts every pixel of a YCbCr picture. YOffset and COffset
// carry the subsampling, so one loop covers 4:2:0, 4:2:2, and 4:4:4.
func ycbcrToRGBA(src *image.YCbCr, dst *image.RGBA, c conversion) {
	bounds := src.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		out := dst.Pix[(y-bounds.Min.Y)*dst.Stride:]
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			var (
				luma   = (int(src.Y[src.YOffset(x, y)]) - c.floor) * c.luma
				chroma = src.COffset(x, y)
				blue   = int(src.Cb[chroma]) - chromaMid
				red    = int(src.Cr[chroma]) - chromaMid
				pixel  = out[(x-bounds.Min.X)*4:]
			)
			pixel[0] = clamp8(luma + red*c.redCr)
			pixel[1] = clamp8(luma + blue*c.greenCb + red*c.greenCr)
			pixel[2] = clamp8(luma + blue*c.blueCb)
			pixel[3] = 0xff
		}
	}
}

// grayToRGBA converts a monochrome plane, which only has a range to apply.
func grayToRGBA(src *image.Gray, dst *image.RGBA, c conversion) {
	bounds := src.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		out := dst.Pix[(y-bounds.Min.Y)*dst.Stride:]
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			value := clamp8((int(src.Pix[src.PixOffset(x, y)]) - c.floor) * c.luma)
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
