package decode_test

import (
	"image"
	"image/color"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// ycbcr444 builds a 4:4:4 image of the given size from one sample per pixel.
func ycbcr444(width, height int, samples [][3]byte) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio444)
	for i, s := range samples {
		img.Y[i], img.Cb[i], img.Cr[i] = s[0], s[1], s[2]
	}
	return img
}

// The H.273 matrix_coefficients code points the tests name.
const (
	bt709       = 1
	unspecified = 2
	bt601       = 6
	bt2020      = 9
)

func TestRGBAAppliesTheMatrixAndTheRange(t *testing.T) {
	// The expected values are worked by hand from the Kr and Kb of each
	// standard and the range equations of H.273, rounded to the nearest byte.
	// They are literals rather than a formula so that they fail if the
	// package's fixed-point arithmetic drifts from the spec.
	samples := [][3]byte{
		{100, 110, 170},
		{150, 100, 110},
		{16, 128, 128},  // studio black
		{235, 128, 128}, // studio white
	}
	for _, tc := range []struct {
		name      string
		matrix    int
		fullRange bool
		want      [][3]int
	}{
		{"bt601 studio", bt601, false, [][3]int{{165, 71, 62}, {127, 182, 100}, {0, 0, 0}, {255, 255, 255}}},
		{"bt601 full", bt601, true, [][3]int{{159, 76, 68}, {125, 172, 100}, {16, 16, 16}, {235, 235, 235}}},
		{"bt709 studio", bt709, false, [][3]int{{173, 79, 60}, {124, 172, 97}, {0, 0, 0}, {255, 255, 255}}},
		{"bt709 full", bt709, true, [][3]int{{166, 84, 67}, {122, 164, 98}, {16, 16, 16}, {235, 235, 235}}},
		{"bt2020 studio", bt2020, false, [][3]int{{168, 74, 59}, {126, 173, 96}, {0, 0, 0}, {255, 255, 255}}},
		{"bt2020 full", bt2020, true, [][3]int{{162, 79, 66}, {123, 165, 97}, {16, 16, 16}, {235, 235, 235}}},
		// Unsignaled video is read as BT.601, as ffmpeg reads it, and so is a
		// matrix this package has no coefficients for.
		{"unspecified", unspecified, false, [][3]int{{165, 71, 62}, {127, 182, 100}, {0, 0, 0}, {255, 255, 255}}},
		{"unknown code point", 4, false, [][3]int{{165, 71, 62}, {127, 182, 100}, {0, 0, 0}, {255, 255, 255}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decode.RGBA(decode.Picture{
				Image:  ycbcr444(len(samples), 1, samples),
				Matrix: tc.matrix, FullRange: tc.fullRange,
			})
			if bounds := got.Bounds(); bounds != image.Rect(0, 0, len(samples), 1) {
				t.Fatalf("bounds = %v, want %v", bounds, image.Rect(0, 0, len(samples), 1))
			}
			for x, want := range tc.want {
				c := got.RGBAAt(x, 0)
				const tolerance = 1
				if abs(int(c.R)-want[0]) > tolerance || abs(int(c.G)-want[1]) > tolerance ||
					abs(int(c.B)-want[2]) > tolerance {
					t.Errorf("pixel %d of %v = (%d,%d,%d), want %v", x, samples[x], c.R, c.G, c.B, want)
				}
				if c.A != 0xff {
					t.Errorf("pixel %d alpha = %d, want 255", x, c.A)
				}
			}
		})
	}
}

func TestRGBAReadsUnsignaledHDAsBT601(t *testing.T) {
	// Picture height used to pick BT.709 at 720 lines and up. It no longer
	// does: an unsignaled picture of any size gets the BT.601 matrix.
	const width, height = 1, 720
	samples := make([][3]byte, height)
	for i := range samples {
		samples[i] = [3]byte{100, 110, 170}
	}
	got := decode.RGBA(decode.Picture{Image: ycbcr444(width, height, samples), Matrix: unspecified})
	if c := got.RGBAAt(0, height-1); abs(int(c.R)-165) > 1 || abs(int(c.G)-71) > 1 || abs(int(c.B)-62) > 1 {
		t.Errorf("pixel = (%d,%d,%d), want the BT.601 (165,71,62)", c.R, c.G, c.B)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func TestRGBAReadsSubsampledChroma(t *testing.T) {
	// One 2x2 luma block over a single 4:2:0 chroma sample: every pixel takes
	// the same chroma, so all four come out the same color.
	img := image.NewYCbCr(image.Rect(0, 0, 2, 2), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = 126
	}
	img.Cb[0], img.Cr[0] = 128, 240

	got := decode.RGBA(decode.Picture{Image: img})
	want := got.RGBAAt(0, 0)
	if want.R <= want.B {
		t.Errorf("pixel = %v, want a red one", want)
	}
	for _, p := range []image.Point{{X: 1}, {Y: 1}, {X: 1, Y: 1}} {
		if c := got.RGBAAt(p.X, p.Y); c != want {
			t.Errorf("pixel %v = %v, want %v", p, c, want)
		}
	}
}

func TestRGBAStretchesAMonochromePlane(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 3, 1))
	img.Pix[0], img.Pix[1], img.Pix[2] = 16, 126, 235

	got := decode.RGBA(decode.Picture{Image: img})
	for x, want := range []uint8{0, 128, 255} {
		c := got.RGBAAt(x, 0)
		if c.R != c.G || c.G != c.B {
			t.Errorf("pixel %d = %v, want a neutral one", x, c)
		}
		if int(c.R) < int(want)-1 || int(c.R) > int(want)+1 {
			t.Errorf("pixel %d = %d, want %d", x, c.R, want)
		}
	}
}

func TestRGBACopiesAnythingElse(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 1, G: 2, B: 3, A: 0xff})

	if got := decode.RGBA(decode.Picture{Image: img}).RGBAAt(0, 0); got != (color.RGBA{R: 1, G: 2, B: 3, A: 0xff}) {
		t.Errorf("pixel = %v, want the source color", got)
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

func TestRGBALeavesAFullRangeMonochromePlane(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 3, 1))
	img.Pix[0], img.Pix[1], img.Pix[2] = 0, 126, 255

	got := decode.RGBA(decode.Picture{Image: img, FullRange: true})
	for x, want := range []uint8{0, 126, 255} {
		if c := got.RGBAAt(x, 0); c != (color.RGBA{R: want, G: want, B: want, A: 0xff}) {
			t.Errorf("pixel %d = %v, want %d", x, c, want)
		}
	}
}
