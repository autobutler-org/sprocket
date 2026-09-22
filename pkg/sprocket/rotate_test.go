package sprocket

import (
	"image"
	"image/color"
	"strconv"
	"testing"
)

// The rotated corpus files are all H.264, so a build without the h264 tag never
// reaches rotate through Thumbnail. This is the index math on its own, spelled
// out on a 3x2 image small enough to write the answers by hand.
func TestRotate(t *testing.T) {
	// 3 wide by 2 tall, each pixel numbered by its position.
	//
	//	1 2 3
	//	4 5 6
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := range 6 {
		src.SetRGBA(i%3, i/3, color.RGBA{R: uint8(i + 1), A: 0xff})
	}

	for _, tc := range []struct {
		degrees       int
		width, height int
		want          []uint8
	}{
		{degrees: 0, width: 3, height: 2, want: []uint8{1, 2, 3, 4, 5, 6}},
		{degrees: 45, width: 3, height: 2, want: []uint8{1, 2, 3, 4, 5, 6}},
		{degrees: 90, width: 2, height: 3, want: []uint8{4, 1, 5, 2, 6, 3}},
		{degrees: 180, width: 3, height: 2, want: []uint8{6, 5, 4, 3, 2, 1}},
		{degrees: 270, width: 2, height: 3, want: []uint8{3, 6, 2, 5, 1, 4}},
	} {
		t.Run(strconv.Itoa(tc.degrees), func(t *testing.T) {
			got := rotate(src, tc.degrees)

			want := image.Rect(0, 0, tc.width, tc.height)
			if bounds := got.Bounds(); bounds != want {
				t.Fatalf("bounds = %v, want %v", bounds, want)
			}
			for i, value := range tc.want {
				x, y := i%tc.width, i/tc.width
				if red := got.RGBAAt(x, y).R; red != value {
					t.Errorf("pixel (%d,%d) = %d, want %d", x, y, red, value)
				}
			}
		})
	}
}
