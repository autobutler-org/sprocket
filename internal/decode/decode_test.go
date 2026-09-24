package decode_test

import (
	"errors"
	"image"
	"testing"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/decode"
	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// syncSample reads the first sync sample of a corpus file, which is what a
// thumbnail at t=0 decodes.
func syncSample(t testing.TB, name string) isobmff.SyncSample {
	t.Helper()

	file, _, err := corpus.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { file.Close() })

	stat, err := file.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	parsed, err := isobmff.Parse(file, stat.Size())
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	sample, err := parsed.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read sync sample of %s: %v", name, err)
	}
	return sample
}

// luma reports the distinct luma values in an image and the mean luma of its
// top-left and bottom-right quadrants. A decode that returns a blank or a
// constant frame fails on the first; one that returns the same block of pixels
// everywhere fails on the second.
func luma(t testing.TB, img image.Image) (distinct int, topLeft, bottomRight float64) {
	t.Helper()

	ycc, ok := img.(*image.YCbCr)
	if !ok {
		t.Fatalf("image is %T, want *image.YCbCr", img)
	}
	var (
		seen             [256]bool
		sumTL, sumBR     float64
		countTL, countBR int
	)
	bounds := ycc.Bounds()
	midX, midY := bounds.Min.X+bounds.Dx()/2, bounds.Min.Y+bounds.Dy()/2
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			value := ycc.Y[ycc.YOffset(x, y)]
			seen[value] = true
			switch {
			case x < midX && y < midY:
				sumTL += float64(value)
				countTL++
			case x >= midX && y >= midY:
				sumBR += float64(value)
				countBR++
			}
		}
	}
	for _, ok := range seen {
		if ok {
			distinct++
		}
	}
	return distinct, sumTL / float64(countTL), sumBR / float64(countBR)
}

func TestKeyframeUnsupportedCodec(t *testing.T) {
	sample := syncSample(t, "hevc-aac-8bit.mov")

	for _, codec := range []string{"vp9", "mp3", "", "HEVC"} {
		t.Run(codec, func(t *testing.T) {
			_, err := decode.Keyframe(codec, sample.Config, sample.NALLengthSize, sample.Data)
			if !errors.Is(err, decode.ErrUnsupportedCodec) {
				t.Errorf("error is %v, want ErrUnsupportedCodec", err)
			}
		})
	}
}
