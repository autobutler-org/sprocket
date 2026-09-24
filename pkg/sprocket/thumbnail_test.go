package sprocket_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

// The corpus is two seconds of 128x72 content at 24 fps, with a single keyframe
// at the start of every file.
const (
	corpusWidth  = 128
	corpusHeight = 72
)

// thumbnail takes a thumbnail of a corpus file.
func thumbnail(t *testing.T, name string, at time.Duration, opts sprocket.ThumbnailOptions) (sprocket.Frame, error) {
	t.Helper()

	file, _, err := corpus.Open(name)
	if err != nil {
		t.Fatalf("open corpus %s: %v", name, err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	return sprocket.Thumbnail(file, stat.Size(), at, opts)
}

// skipIfCompiledOut skips a test whose thumbnail came back as the fallback for
// a decoder this build leaves out. An H.264 file without the h264 tag, or an
// HEVC one without the hevc tag, is the documented ErrUnsupportedCodec rather
// than a failure, and there is no frame to assert anything about.
func skipIfCompiledOut(t *testing.T, name string, err error) {
	t.Helper()

	if !errors.Is(err, sprocket.ErrUnsupportedCodec) {
		return
	}
	for _, tag := range []struct {
		name     string
		compiled bool
	}{{"h264", h264Compiled}, {"hevc", hevcCompiled}} {
		if !tag.compiled && strings.Contains(err.Error(), "-tags "+tag.name) {
			t.Skipf("%s needs the %s build tag", name, tag.name)
		}
	}
}

// mustThumbnail takes a thumbnail and fails if it could not be decoded. A file
// whose decoder is compiled out skips instead.
func mustThumbnail(t *testing.T, name string, at time.Duration, opts sprocket.ThumbnailOptions) sprocket.Frame {
	t.Helper()

	frame, err := thumbnail(t, name, at, opts)
	skipIfCompiledOut(t, name, err)
	if err != nil {
		t.Fatalf("thumbnail %s: %v", name, err)
	}
	return frame
}

// distinct counts the colors an image holds. A decode that hands back a blank
// frame, or a resize that copies one pixel everywhere, comes back with one.
func distinct(img image.Image) int {
	seen := make(map[color.Color]struct{})
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			seen[img.At(x, y)] = struct{}{}
		}
	}
	return len(seen)
}

func TestThumbnailDecodesACorpusKeyframe(t *testing.T) {
	for _, name := range []string{"hevc-aac-8bit.mov", "hevc-aac-10bit.mov", "h264-aac.mp4", "no-audio.mp4"} {
		for _, at := range []time.Duration{0, time.Second} {
			t.Run(name+"@"+at.String(), func(t *testing.T) {
				frame := mustThumbnail(t, name, at, sprocket.ThumbnailOptions{})

				if _, ok := frame.Image.(*image.RGBA); !ok {
					t.Errorf("image is %T, want *image.RGBA", frame.Image)
				}
				want := image.Rect(0, 0, corpusWidth, corpusHeight)
				if bounds := frame.Image.Bounds(); bounds != want {
					t.Errorf("bounds = %v, want %v", bounds, want)
				}
				if got := distinct(frame.Image); got < 2 {
					t.Errorf("image holds %d colors, want a real picture", got)
				}
				// Every corpus file has one keyframe, at the start, so a request
				// a second in comes back with the frame at zero. That is the
				// documented case of the returned time differing from the
				// requested one.
				if frame.Time != 0 {
					t.Errorf("time = %v, want 0, the only keyframe in the file", frame.Time)
				}
			})
		}
	}
}

func TestThumbnailReadsANegativeTimeAsZero(t *testing.T) {
	want := mustThumbnail(t, "hevc-aac-8bit.mov", 0, sprocket.ThumbnailOptions{})
	got := mustThumbnail(t, "hevc-aac-8bit.mov", -time.Hour, sprocket.ThumbnailOptions{})

	if got.Time != want.Time {
		t.Errorf("time = %v, want %v", got.Time, want.Time)
	}
	if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
		t.Error("a negative time decoded a different frame from zero")
	}
}

func TestThumbnailAppliesTheTrackRotation(t *testing.T) {
	// The rotated files are the baseline remuxed with a tkhd matrix, so their
	// pixels are the baseline's and only the corner they land in changes.
	upright := mustThumbnail(t, "h264-aac.mp4", 0, sprocket.ThumbnailOptions{})
	topLeft := upright.Image.At(0, 0)
	if topLeft == upright.Image.At(corpusWidth-1, 0) || topLeft == upright.Image.At(0, corpusHeight-1) {
		t.Fatalf("the top-left pixel is %v, which the other corners share: nothing to tell rotations apart by", topLeft)
	}

	for _, tc := range []struct {
		name          string
		width, height int
		// corner is where the upright frame's top-left pixel has to end up.
		corner image.Point
	}{
		{name: "rotate-90.mp4", width: corpusHeight, height: corpusWidth, corner: image.Pt(corpusHeight-1, 0)},
		{name: "rotate-180.mp4", width: corpusWidth, height: corpusHeight, corner: image.Pt(corpusWidth-1, corpusHeight-1)},
		{name: "rotate-270.mp4", width: corpusHeight, height: corpusWidth, corner: image.Pt(0, corpusWidth-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := mustThumbnail(t, tc.name, 0, sprocket.ThumbnailOptions{})

			want := image.Rect(0, 0, tc.width, tc.height)
			if bounds := frame.Image.Bounds(); bounds != want {
				t.Fatalf("bounds = %v, want %v", bounds, want)
			}
			if got := frame.Image.At(tc.corner.X, tc.corner.Y); got != topLeft {
				t.Errorf("the pixel at %v is %v, want the upright frame's top-left pixel %v",
					tc.corner, got, topLeft)
			}
		})
	}
}

func TestThumbnailResizesToTheMaxDimension(t *testing.T) {
	for _, tc := range []struct {
		name          string
		maxDimension  int
		width, height int
	}{
		{name: "hevc-aac-8bit.mov", maxDimension: 32, width: 32, height: 18},
		{name: "hevc-aac-8bit.mov", maxDimension: 1, width: 1, height: 1},
		{name: "hevc-aac-8bit.mov", maxDimension: 1000, width: corpusWidth, height: corpusHeight},
		{name: "hevc-aac-8bit.mov", maxDimension: 0, width: corpusWidth, height: corpusHeight},
		// A rotated frame is capped after the rotation, on the side that is
		// longer once it is the right way up.
		{name: "rotate-90.mp4", maxDimension: 32, width: 18, height: 32},
	} {
		t.Run(tc.name+"/"+strconv.Itoa(tc.maxDimension), func(t *testing.T) {
			frame := mustThumbnail(t, tc.name, 0, sprocket.ThumbnailOptions{MaxDimension: tc.maxDimension})

			want := image.Rect(0, 0, tc.width, tc.height)
			if bounds := frame.Image.Bounds(); bounds != want {
				t.Errorf("bounds = %v, want %v", bounds, want)
			}
			if tc.maxDimension > 1 && distinct(frame.Image) < 2 {
				t.Error("the resized image holds one color, want a real picture")
			}
		})
	}
}

func TestThumbnailReportsAnUnsupportedCodec(t *testing.T) {
	// Without its build tag an H.264 or HEVC file is exactly the fallback case
	// the error documents: a readable container this build cannot picture. The
	// error names the tag that puts the decoder in.
	for _, tc := range []struct {
		name, tag string
		compiled  bool
	}{
		{name: "h264-aac.mp4", tag: "h264", compiled: h264Compiled},
		{name: "hevc-aac-8bit.mov", tag: "hevc", compiled: hevcCompiled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.compiled {
				t.Skipf("this build has the %s decoder", tc.tag)
			}
			_, err := thumbnail(t, tc.name, 0, sprocket.ThumbnailOptions{})
			if !errors.Is(err, sprocket.ErrUnsupportedCodec) {
				t.Fatalf("error = %v, want ErrUnsupportedCodec", err)
			}
			if !strings.Contains(err.Error(), "-tags "+tc.tag) {
				t.Errorf("error = %q, want it to name the %s build tag", err, tc.tag)
			}
		})
	}
}

func TestThumbnailRejectsAnUndecodableKeyframe(t *testing.T) {
	// Wiping the payload leaves every header intact, so the demuxer finds the
	// keyframe and hands the decoder a sample with nothing in it. That is the
	// one path where the failure comes from the decoder rather than the
	// container, and it is a corrupt file either way.
	if !hevcCompiled {
		t.Skip("this build has no HEVC decoder to hand the wiped sample to")
	}
	wiped := readCorpus(t, "hevc-aac-8bit.mov")
	mdat := topLevelOffset(t, wiped, "mdat")
	size := int64(binary.BigEndian.Uint32(wiped[mdat:]))
	clear(wiped[mdat+8 : mdat+size])

	_, err := sprocket.Thumbnail(bytes.NewReader(wiped), int64(len(wiped)), 0, sprocket.ThumbnailOptions{})
	if !errors.Is(err, sprocket.ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
}

func TestThumbnailReportsAFileWithNoVideo(t *testing.T) {
	// The corpus has no audio-only file, so one is made by retyping the video
	// track's handler. A hdlr box carries its type eight bytes in, after the
	// version, the flags, and a pre-defined word, and matching that much of it
	// avoids the "videolan.org" the encoder wrote into the payload. The
	// substitution is the same length as what it replaces, so every offset in
	// the file still points where it did.
	const hdlrPrefix = "hdlr\x00\x00\x00\x00\x00\x00\x00\x00"
	audioOnly := bytes.Replace(readCorpus(t, "h264-aac.mp4"),
		[]byte(hdlrPrefix+"vide"), []byte(hdlrPrefix+"none"), 1)

	_, err := sprocket.Thumbnail(bytes.NewReader(audioOnly), int64(len(audioOnly)), 0, sprocket.ThumbnailOptions{})
	if !errors.Is(err, sprocket.ErrNoVideo) {
		t.Errorf("error = %v, want ErrNoVideo", err)
	}
}

func TestThumbnailRejectsNonMedia(t *testing.T) {
	content := []byte("hello world, definitely not a movie")
	_, err := sprocket.Thumbnail(bytes.NewReader(content), int64(len(content)), 0, sprocket.ThumbnailOptions{})
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

func TestThumbnailRejectsATruncatedFile(t *testing.T) {
	// h264-aac.mp4 carries its moov at the end, so cutting the file takes the
	// headers with it and the failure lands before any decoding.
	const keep = 0.6
	whole := readCorpus(t, "h264-aac.mp4")
	cut := whole[:int(float64(len(whole))*keep)]

	_, err := sprocket.Thumbnail(bytes.NewReader(cut), int64(len(cut)), 0, sprocket.ThumbnailOptions{})
	if !errors.Is(err, sprocket.ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
}
