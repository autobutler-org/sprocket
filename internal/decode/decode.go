// Package decode turns one keyframe sample into an image.
//
// Keyframe is the only entry point. It takes what a container already knows
// about a sample, dispatches on the codec short name, and returns the decoded
// picture. HEVC and H.264 are implemented. A codec with no decoder returns
// ErrUnsupportedCodec, which is the signal for a caller to fall back rather
// than to fail.
//
// Nothing is kept between calls. Each call builds a decoder, uses it once, and
// drops it, so the only memory a caller holds afterwards is the image it asked
// for.
//
// # The h264 build tag
//
// H.264 is compiled in only under the h264 build tag, and without it an H.264
// sample returns ErrUnsupportedCodec naming the tag. H.264 is patent
// encumbered and the maintainer's decision is that a downstream opts into it
// deliberately rather than acquiring it through go get. The build tag is the
// whole of the difference: the decoder is a Go dependency like any other, and
// the behavior under the tag is the same shape as the HEVC path.
//
// The H.264 decoder reads 8 bit 4:2:0 progressive sequences. A 10 bit, 4:2:2,
// 4:4:4, monochrome, or interlaced sequence returns ErrUnsupportedCodec. One
// known gap returns ErrCorruptSample instead: High profile with CAVLC entropy
// coding and the 8x8 transform, which the decoder reports as invalid syntax
// rather than as a tool it does not implement. Encoders pair High profile with
// CABAC in practice, so this is rare footage.
//
// # Output
//
// The image is an *image.YCbCr for a sequence with chroma, or an *image.Gray
// for a monochrome one, cropped to the conformance window so that it has the
// dimensions the stream means rather than the ones it codes. An 8-bit sequence
// hands its planes over as they were decoded, with no conversion at all.
//
// A sequence deeper than 8 bits is shifted down to 8. That is a loss of
// precision and nothing else: the code values are not tone mapped, so a PQ or
// HLG frame comes back with its own code values in an 8-bit container and will
// look flat and desaturated next to a tone-mapped render. The decoder reports
// the CICP primaries, transfer, matrix, and range of the sequence, and this
// package does not pass them on, because there is nowhere yet for a caller to
// put them. Anything that wants to correct the color needs them returned.
//
// # Color range
//
// The planes are what the bitstream carried and nothing has been done to them,
// which means image.YCbCr's own conversion to RGB is wrong for most video.
// image.YCbCr reads its planes as full-range BT.601 whatever the sequence
// signals, and video is almost always studio range, so drawing the image
// straight lifts the blacks and flattens the contrast. Measured against
// ffmpeg's render of the same frame that is a mean absolute error of 11 to 12
// out of 255; telling ffmpeg to read the same planes as full range brings it
// down to 0.4, which is the whole of the difference. Whatever converts these
// frames for display has to apply the range and the matrix itself.
//
// # Memory
//
// One 3840x2160 Main10 keyframe on darwin/arm64, decoded in a fresh process
// from a 164 KB sample: 55 ms and 69 MiB of peak resident memory, of which
// 12 MiB is the image handed back. Six sequential decodes in one process run
// 45 to 50 ms each and peak at 162 MiB, since the runtime keeps what it has
// grown rather than returning it. There is no per-process warmup cost.
//
// The same size in H.264 High profile, from a 167 KB sample and with the h264
// build tag: 95 to 101 ms and 55 MiB of peak resident memory, of which 12 MiB
// is the image handed back. Six sequential decodes in one process stay at 93
// to 101 ms each and peak at 80 MiB.
//
// A picture is refused before a plane is allocated for it if the sequence
// declares more luma samples than maxLumaSamples, which is the largest picture
// any HEVC level allows. Both codecs take that bound, the H.264 one counted in
// macroblocks.
package decode

import (
	"errors"
	"fmt"
	"image"
	"runtime"

	"github.com/gen2brain/h265/hevc"
)

// Sentinel errors. Every error this package returns wraps one of these, so a
// caller can tell a file it will never decode from one that is broken.
var (
	// ErrUnsupportedCodec means there is no decoder for the codec, or the
	// bitstream is valid but uses a coding tool the decoder does not implement.
	// Either way the file will not decode, and a caller with a fallback should
	// take it.
	ErrUnsupportedCodec = errors.New("decode: unsupported codec")
	// ErrCorruptSample means the configuration record or the sample is
	// malformed, or the decoder read the whole sample without producing a
	// picture.
	ErrCorruptSample = errors.New("decode: corrupt sample")
	// ErrFrameTooLarge means the sequence declares a picture with more luma
	// samples than this package will decode.
	ErrFrameTooLarge = errors.New("decode: frame is over the size cap")
)

const (
	// maxLumaSamples bounds one decoded picture. It is MaxLumaPs of Table A.8,
	// which every HEVC level from 6.0 up shares, so a picture over it has no
	// level to have been coded at. It is a little over 8192x4320.
	//
	// ponytail: an 8K ceiling, not a thumbnail-sized one, which costs about
	// 100 MB of planes at 10 bits. Lower it if decoding an 8K frame to make a
	// 200 pixel thumbnail is not worth that.
	maxLumaSamples = hevc.MaxLumaSamples
	// maxThreads bounds the goroutines one picture is spread over.
	//
	// ponytail: a thumbnail is a background chore, so it does not get the whole
	// machine. Raise it if decode latency ever matters more than that does.
	maxThreads = 4
)

// Keyframe decodes one keyframe sample to an image.
//
// codec is the container's short codec name, config the raw decoder
// configuration record the track carries, nalLengthSize the width in bytes of
// the length prefix on each NAL unit in sample, and sample the keyframe exactly
// as the container stores it. The arguments are the fields of an
// isobmff.SyncSample, spelled out rather than imported, so that this package
// does not depend on any one container.
//
// See the package documentation for what the returned image holds, what happens
// to a sequence deeper than 8 bits, and what is refused.
func Keyframe(codec string, config []byte, nalLengthSize int, sample []byte) (image.Image, error) {
	switch codec {
	case "hevc":
		return hevcKeyframe(config, nalLengthSize, sample)
	case "h264":
		return h264Keyframe(config, nalLengthSize, sample)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedCodec, codec)
	}
}

// hevcKeyframe decodes an HEVC keyframe. The parameter sets come from the hvcC
// record and the slices from the sample, which is what a container splits
// between the two.
func hevcKeyframe(config []byte, nalLengthSize int, sample []byte) (image.Image, error) {
	if nalLengthSize < 1 || nalLengthSize > 4 {
		return nil, fmt.Errorf("%w: the NAL length prefix is %d bytes, want 1 through 4",
			ErrCorruptSample, nalLengthSize)
	}
	nals, err := hvccParameterSets(config)
	if err != nil {
		return nil, err
	}
	nals = append(nals, hevc.SplitHVCC(sample, nalLengthSize)...)
	if err := checkSequenceSize(nals); err != nil {
		return nil, err
	}

	var decoder hevc.Decoder
	decoder.Threads(min(runtime.GOMAXPROCS(0), maxThreads))
	decoder.FrameSizeLimit(maxLumaSamples)

	var picture *hevc.Picture
	for _, nal := range nals {
		pictures, err := decoder.DecodeNAL(nal)
		if err != nil {
			return nil, decodeError(err)
		}
		if len(pictures) > 0 {
			picture = pictures[0]
		}
	}
	if picture == nil {
		// A single access unit is normally released by the next one, so a
		// keyframe on its own usually surfaces here rather than above.
		if pictures := decoder.Flush(); len(pictures) > 0 {
			picture = pictures[0]
		}
	}
	if picture == nil {
		return nil, fmt.Errorf("%w: the decoder read %d NAL units and produced no picture",
			ErrCorruptSample, len(nals))
	}
	return pictureImage(picture)
}

// decodeError maps what the decoder reports onto this package's sentinels.
func decodeError(err error) error {
	switch {
	case errors.Is(err, hevc.ErrUnsupported):
		// The decoder returns this both for a coding tool it does not implement
		// and for a sequence over FrameSizeLimit. checkSequenceSize has already
		// refused the second, so what is left is the first.
		return fmt.Errorf("%w: hevc: %w", ErrUnsupportedCodec, err)
	default:
		return fmt.Errorf("%w: %w", ErrCorruptSample, err)
	}
}

// checkSequenceSize refuses a sequence whose pictures are over the cap. The
// decoder enforces the same bound, but it reports a picture over the cap and a
// coding tool it does not implement as the same error, and the two mean
// different things to a caller.
func checkSequenceSize(nals []hevc.NALUnit) error {
	for _, nal := range nals {
		if nal.Type != hevc.NALSPS {
			continue
		}
		width, height, ok := spsPictureSize(nal.RBSP)
		if !ok {
			return fmt.Errorf("%w: unreadable sequence parameter set", ErrCorruptSample)
		}
		if int64(width)*int64(height) > maxLumaSamples {
			return fmt.Errorf("%w: %dx%d is %d luma samples, over the %d sample cap",
				ErrFrameTooLarge, width, height, int64(width)*int64(height), int64(maxLumaSamples))
		}
	}
	return nil
}

// pictureImage wraps a decoded picture as an image. An 8-bit picture aliases the
// decoder's planes, which is safe because the decoder is dropped with this
// call's frame and the picture is never released back to it.
func pictureImage(picture *hevc.Picture) (image.Image, error) {
	width, height := picture.CropW, picture.CropH
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w: the conformance window is %dx%d", ErrCorruptSample, width, height)
	}
	rect := image.Rect(0, 0, width, height)

	// picture.ChromaFormat is chroma_format_idc of 7.4.3.2, not the encoder's
	// ChromaFormat constants.
	var ratio image.YCbCrSubsampleRatio
	subX, subY := 1, 1
	switch picture.ChromaFormat {
	case 0: // Monochrome, which has no chroma planes at all.
	case 1:
		ratio, subX, subY = image.YCbCrSubsampleRatio420, 2, 2
	case 2:
		ratio, subX = image.YCbCrSubsampleRatio422, 2
	case 3:
		ratio = image.YCbCrSubsampleRatio444
	default:
		return nil, fmt.Errorf("%w: chroma_format_idc %d", ErrCorruptSample, picture.ChromaFormat)
	}

	luma, err := eightBitPlane(picture.Y, picture.Y16, picture.BitDepth,
		picture.StrideY, picture.CropX, picture.CropY, width, height)
	if err != nil {
		return nil, err
	}
	if picture.ChromaFormat == 0 {
		return &image.Gray{Pix: luma.pix, Stride: luma.stride, Rect: rect}, nil
	}

	chromaW, chromaH := (width+subX-1)/subX, (height+subY-1)/subY
	blue, err := eightBitPlane(picture.Cb, picture.Cb16, picture.BitDepthC,
		picture.StrideC, picture.CropX/subX, picture.CropY/subY, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	red, err := eightBitPlane(picture.Cr, picture.Cr16, picture.BitDepthC,
		picture.StrideC, picture.CropX/subX, picture.CropY/subY, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	return &image.YCbCr{
		Y: luma.pix, Cb: blue.pix, Cr: red.pix,
		YStride: luma.stride, CStride: blue.stride,
		SubsampleRatio: ratio, Rect: rect,
	}, nil
}

// plane is one 8-bit image plane with its conformance crop already applied.
type plane struct {
	pix    []byte
	stride int
}

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
