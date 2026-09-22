package decode_test

import (
	"image"
	"image/color"
	"math"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// reference converts one studio-range YCbCr triple to RGB in floating point.
// It is written from the coefficients rather than shared with the package, so
// it fails if the fixed-point version drifts.
func reference(y, cb, cr float64, bt709 bool) (r, g, b float64) {
	const (
		lumaScale  = 255.0 / 219
		lumaFloor  = 16.0
		chromaMid  = 128.0
		bt601Red   = 0.299
		bt601Blue  = 0.114
		bt709Red   = 0.2126
		bt709Blue  = 0.0722
		chromaSpan = 255.0 / 224
	)
	kr, kb := bt601Red, bt601Blue
	if bt709 {
		kr, kb = bt709Red, bt709Blue
	}
	kg := 1 - kr - kb

	luma := (y - lumaFloor) * lumaScale
	blue := (cb - chromaMid) * chromaSpan
	red := (cr - chromaMid) * chromaSpan
	return luma + 2*(1-kr)*red,
		luma - 2*kb*(1-kb)/kg*blue - 2*kr*(1-kr)/kg*red,
		luma + 2*(1-kb)*blue
}

func clampFloat(v float64) int {
	return int(math.Round(math.Min(math.Max(v, 0), 255)))
}

// ycbcr444 builds a 4:4:4 image of the given size from one sample per pixel.
func ycbcr444(width, height int, samples [][3]byte) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, width, height), image.YCbCrSubsampleRatio444)
	for i, s := range samples {
		img.Y[i], img.Cb[i], img.Cr[i] = s[0], s[1], s[2]
	}
	return img
}

func TestRGBAAppliesStudioRangeAndTheMatrix(t *testing.T) {
	samples := [][3]byte{
		{16, 128, 128},  // studio black
		{235, 128, 128}, // studio white
		{126, 128, 128}, // mid gray
		{126, 128, 240}, // saturated red
	}
	for _, tc := range []struct {
		name          string
		width, height int
		bt709         bool
	}{
		{name: "bt601 under 720 lines", width: 4, height: 1},
		{name: "bt709 at 720 lines", width: 4, height: 720, bt709: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([][3]byte, 0, tc.width*tc.height)
			for range tc.height {
				rows = append(rows, samples...)
			}
			got := decode.RGBA(ycbcr444(tc.width, tc.height, rows))

			if bounds := got.Bounds(); bounds != image.Rect(0, 0, tc.width, tc.height) {
				t.Fatalf("bounds = %v, want %v", bounds, image.Rect(0, 0, tc.width, tc.height))
			}
			for x, s := range samples {
				wantR, wantG, wantB := reference(float64(s[0]), float64(s[1]), float64(s[2]), tc.bt709)
				c := got.RGBAAt(x, 0)
				const tolerance = 1
				if abs(int(c.R)-clampFloat(wantR)) > tolerance ||
					abs(int(c.G)-clampFloat(wantG)) > tolerance ||
					abs(int(c.B)-clampFloat(wantB)) > tolerance {
					t.Errorf("pixel %d of %v = (%d,%d,%d), want (%d,%d,%d)", x, s,
						c.R, c.G, c.B, clampFloat(wantR), clampFloat(wantG), clampFloat(wantB))
				}
				if c.A != 0xff {
					t.Errorf("pixel %d alpha = %d, want 255", x, c.A)
				}
			}
		})
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

	got := decode.RGBA(img)
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

	got := decode.RGBA(img)
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

	if got := decode.RGBA(img).RGBAAt(0, 0); got != (color.RGBA{R: 1, G: 2, B: 3, A: 0xff}) {
		t.Errorf("pixel = %v, want the source color", got)
	}
}

func TestRGBAConvertsADecodedKeyframe(t *testing.T) {
	// The studio-range read is what the package documentation says a caller has
	// to do, so the darkest pixel of a real frame has to reach further down than
	// image.YCbCr's own full-range conversion puts it.
	img := keyframe(t, "hevc-aac-8bit.mov")
	got := decode.RGBA(img)

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
