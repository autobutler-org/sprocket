package decode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"runtime"

	"github.com/gen2brain/gav1d/av1"
)

// av1Keyframe decodes an AV1 keyframe. A block of an AV1 track is one temporal
// unit, and a keyframe's temporal unit carries its own sequence header, so the
// container's configuration record is not needed and is not read: feeding the
// av1C record's configOBUs ahead of the frame produces the same picture.
func av1Keyframe(sample []byte) (image.Image, error) {
	decoder := av1.Decoder{
		Threads: min(runtime.GOMAXPROCS(0), maxThreads),
		// The area bound is this package's rather than the decoder's default,
		// so that both codecs refuse the same picture.
		FrameSizeLimit: maxLumaSamples,
	}
	pictures, err := decoder.DecodeOBUs(sample)
	if err != nil {
		if errors.Is(err, av1.ErrFrameTooLarge) {
			return nil, fmt.Errorf("%w: av1: %w", ErrFrameTooLarge, err)
		}
		return nil, fmt.Errorf("%w: av1: %w", ErrCorruptSample, err)
	}
	if len(pictures) == 0 {
		return nil, fmt.Errorf("%w: the decoder read the temporal unit and produced no picture", ErrCorruptSample)
	}
	// The picture is not released back to the decoder: the image below aliases
	// its planes, and the decoder is dropped with this call's frame anyway.
	return av1PictureImage(pictures[0])
}

// av1PictureImage wraps a decoded picture as an image, shifting a sequence
// deeper than 8 bits down the way the HEVC path does.
func av1PictureImage(p *av1.Picture) (image.Image, error) {
	if p.Width <= 0 || p.Height <= 0 {
		return nil, fmt.Errorf("%w: the picture is %dx%d", ErrCorruptSample, p.Width, p.Height)
	}
	rect := image.Rect(0, 0, p.Width, p.Height)

	var ratio image.YCbCrSubsampleRatio
	subX, subY := 1, 1
	switch p.Layout {
	case av1.LayoutI400: // Monochrome, which has no chroma planes at all.
	case av1.LayoutI420:
		ratio, subX, subY = image.YCbCrSubsampleRatio420, 2, 2
	case av1.LayoutI422:
		ratio, subX = image.YCbCrSubsampleRatio422, 2
	case av1.LayoutI444:
		ratio = image.YCbCrSubsampleRatio444
	default:
		return nil, fmt.Errorf("%w: pixel layout %d", ErrCorruptSample, p.Layout)
	}

	luma, err := av1Plane(p, 0, p.Width, p.Height)
	if err != nil {
		return nil, err
	}
	if p.Layout == av1.LayoutI400 {
		return &image.Gray{Pix: luma.pix, Stride: luma.stride, Rect: rect}, nil
	}

	chromaW, chromaH := (p.Width+subX-1)/subX, (p.Height+subY-1)/subY
	blue, err := av1Plane(p, 1, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	red, err := av1Plane(p, 2, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	return &image.YCbCr{
		Y: luma.pix, Cb: blue.pix, Cr: red.pix,
		YStride: luma.stride, CStride: blue.stride,
		SubsampleRatio: ratio, Rect: rect,
	}, nil
}

// av1Plane returns an 8-bit view of one decoded plane. An 8-bit plane is
// aliased in place; a deeper one is shifted down into a packed copy, which
// loses the low bits and nothing else. The decoder's planes are bytes whatever
// the depth, with two native-endian bytes per sample above 8 bits.
func av1Plane(p *av1.Picture, index, width, height int) (plane, error) {
	src, stride := p.Data[index], p.Stride[index]
	sampleBytes := 1
	if p.BitDepth > 8 {
		sampleBytes = 2
	}
	if stride < width*sampleBytes || (height-1)*stride+width*sampleBytes > len(src) {
		return plane{}, fmt.Errorf("%w: a %dx%d plane of %d bytes has stride %d",
			ErrCorruptSample, width, height, len(src), stride)
	}
	if sampleBytes == 1 {
		return plane{pix: src, stride: stride}, nil
	}

	shift := min(p.BitDepth-8, 8)
	pix := make([]byte, width*height)
	for y := range height {
		row, out := src[y*stride:], pix[y*width:]
		for x := range width {
			out[x] = byte(binary.NativeEndian.Uint16(row[x*2:]) >> shift)
		}
	}
	return plane{pix: pix, stride: width}, nil
}
