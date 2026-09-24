package sprocket

import (
	"errors"
	"fmt"
	"image"
	"io"
	"time"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// Frame is one decoded picture and when it is shown.
type Frame struct {
	// Image is the decoded picture, rotated the right way up and resized if
	// ThumbnailOptions asked for it. It is an *image.RGBA, opaque everywhere.
	Image image.Image
	// Time is the presentation time of the keyframe that was decoded. Thumbnail
	// snaps to a keyframe, so this routinely differs from the time that was
	// asked for, and a caller labelling a thumbnail should use this rather than
	// its own request.
	Time time.Duration
}

// ThumbnailOptions is what a caller can ask Thumbnail to do to the frame after
// it is decoded. The zero value decodes at the frame's own size.
type ThumbnailOptions struct {
	// MaxDimension caps the longer side of the returned image in pixels,
	// preserving the aspect ratio. Zero leaves the frame at its own size, and a
	// frame already inside the cap is left alone: this never upscales.
	MaxDimension int
}

// Thumbnail decodes the keyframe nearest a time and returns it as an image.
//
// The result snaps to a keyframe, so Frame.Time is what came back and may
// differ from at by seconds. Decoding forward from a keyframe to an arbitrary
// frame is a non-goal of this library. Nearest is measured on presentation
// times and a tie goes to the earlier keyframe; a time past the end of the file
// answers with the last keyframe, and a negative time is read as zero.
//
// size is the length of the file r reads from, as with Probe. The track's
// rotation is applied, so the image comes back the way up a player would show
// it, and the image is converted from the stream's own color space to RGB. One
// keyframe sample and one decoded frame are held at a time and nothing is kept
// across calls.
//
// A file whose video codec has no decoder returns ErrUnsupportedCodec, which is
// the signal to fall back to a generic icon rather than to fail. A file with no
// video track at all returns ErrNoVideo. Input that is not a container this
// library reads returns ErrUnsupportedContainer, and a container whose headers
// or keyframe are malformed or cut short returns ErrCorrupt.
func Thumbnail(r io.ReaderAt, size int64, at time.Duration, opts ThumbnailOptions) (Frame, error) {
	file, err := open(r, size)
	if err != nil {
		return Frame{}, err
	}
	frame, rotation, err := nearestKeyframe(file, max(at, 0))
	if err != nil {
		return Frame{}, containerError(err)
	}
	picture, err := decode.Keyframe(frame.codec, frame.config, frame.nalLengthSize, frame.data)
	if err != nil {
		return Frame{}, thumbnailError(err)
	}

	img := rotate(decode.RGBA(picture), rotation)
	return Frame{Image: resize(img, opts.MaxDimension), Time: frame.at}, nil
}

// keyframe is one keyframe read out of either container family, with the
// fields a decoder needs and when the frame is shown.
type keyframe struct {
	codec         string
	config        []byte
	nalLengthSize int
	data          []byte
	at            time.Duration
}

// nearestKeyframe reads the keyframe nearest a time out of whichever family
// the file belongs to, and reports the rotation to apply to it. Neither
// Matroska nor MPEG-TS carries a display matrix, so neither is ever rotated.
func nearestKeyframe(file container, at time.Duration) (keyframe, int, error) {
	if file.ts != nil {
		sample, err := file.ts.ReadNearestSyncSample(at)
		return keyframe{
			codec: sample.Codec, config: sample.Config,
			nalLengthSize: sample.NALLengthSize, data: sample.Data, at: sample.Time,
		}, 0, err
	}
	if file.mkv != nil {
		sample, err := file.mkv.ReadNearestSyncSample(at)
		return keyframe{
			codec: sample.Codec, config: sample.Config,
			nalLengthSize: sample.NALLengthSize, data: sample.Data, at: sample.Time,
		}, 0, err
	}

	sample, err := file.mp4.ReadNearestSyncSample(at)
	if err != nil {
		return keyframe{}, 0, err
	}
	var rotation int
	if video := file.mp4.VideoTrack(); video != nil {
		rotation = video.Rotation
	}
	return keyframe{
		codec: sample.Codec, config: sample.Config,
		nalLengthSize: sample.NALLengthSize, data: sample.Data, at: sample.MovieTime,
	}, rotation, nil
}

// thumbnailError maps a decoder error onto this package's sentinels. A picture
// over the decoder's size cap reports ErrUnsupportedCodec too: the file is not
// corrupt, but the caller's answer to it is the same fallback.
func thumbnailError(err error) error {
	if errors.Is(err, decode.ErrUnsupportedCodec) || errors.Is(err, decode.ErrFrameTooLarge) {
		return fmt.Errorf("%w: %w", ErrUnsupportedCodec, err)
	}
	return fmt.Errorf("%w: %w", ErrCorrupt, err)
}

// rotate turns an image clockwise by 90, 180, or 270 degrees. Any other angle,
// including zero, returns the image untouched.
func rotate(src *image.RGBA, degrees int) *image.RGBA {
	width, height := src.Rect.Dx(), src.Rect.Dy()

	var dst *image.RGBA
	switch degrees {
	case 90, 270:
		dst = image.NewRGBA(image.Rect(0, 0, height, width))
	case 180:
		dst = image.NewRGBA(image.Rect(0, 0, width, height))
	default:
		return src
	}

	for y := range height {
		row := src.Pix[y*src.Stride:]
		for x := range width {
			var dx, dy int
			switch degrees {
			case 90:
				dx, dy = height-1-y, x
			case 180:
				dx, dy = width-1-x, height-1-y
			default:
				dx, dy = y, width-1-x
			}
			copy(dst.Pix[dy*dst.Stride+dx*4:][:4], row[x*4:][:4])
		}
	}
	return dst
}

// resize scales an image down so that its longer side is at most maxDimension,
// preserving the aspect ratio. A maxDimension of zero, or an image already
// inside it, comes back untouched: this never upscales.
//
// Each output pixel is the average of the input pixels it covers, which is what
// suits a large reduction. ponytail: the average is taken on the stored values
// rather than on linearized ones, so a hard edge between saturated colors comes
// out a shade dark. Linearize through a lookup table if that ever shows.
func resize(src *image.RGBA, maxDimension int) *image.RGBA {
	var (
		width  = src.Rect.Dx()
		height = src.Rect.Dy()
		longer = max(width, height)
	)
	if maxDimension <= 0 || longer <= maxDimension {
		return src
	}
	var (
		outWidth  = max(width*maxDimension/longer, 1)
		outHeight = max(height*maxDimension/longer, 1)
		dst       = image.NewRGBA(image.Rect(0, 0, outWidth, outHeight))
	)

	for dy := range outHeight {
		top, bottom := dy*height/outHeight, max((dy+1)*height/outHeight, dy*height/outHeight+1)
		out := dst.Pix[dy*dst.Stride:]
		for dx := range outWidth {
			left, right := dx*width/outWidth, max((dx+1)*width/outWidth, dx*width/outWidth+1)
			var red, green, blue, alpha, count uint32
			for y := top; y < bottom; y++ {
				row := src.Pix[y*src.Stride+left*4:]
				for i := 0; i < (right-left)*4; i += 4 {
					red += uint32(row[i])
					green += uint32(row[i+1])
					blue += uint32(row[i+2])
					alpha += uint32(row[i+3])
					count++
				}
			}
			pixel := out[dx*4:]
			pixel[0] = uint8(red / count)
			pixel[1] = uint8(green / count)
			pixel[2] = uint8(blue / count)
			pixel[3] = uint8(alpha / count)
		}
	}
	return dst
}
