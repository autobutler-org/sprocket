// Package decode turns one keyframe sample into an image.
//
// Keyframe is the only entry point. It takes what a container already knows
// about a sample, dispatches on the codec short name, and returns the decoded
// picture. VP8 and AV1 are implemented, and H.264 and HEVC each behind a build
// tag. A codec with no decoder returns ErrUnsupportedCodec, which is the signal for a
// caller to fall back rather than to fail. VP9 is the one codec the containers
// this library reads can carry that has no decoder here: nothing pure Go
// decodes it today.
//
// # What each decoder is given
//
// HEVC and H.264 split their parameter sets from their slices, so both the
// configuration record and the sample are needed, along with the width of the
// length prefix the container put on each NAL unit. VP8 and AV1 need neither:
// a VP8 frame is the raw bitstream and an AV1 block is a whole temporal unit
// that carries its own sequence header, so the sample goes in on its own. The
// av1C record's configOBUs are not read, and prepending them changes nothing.
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
// # The hevc build tag
//
// HEVC is compiled in only under the hevc build tag, and without it an HEVC
// sample returns ErrUnsupportedCodec naming the tag. HEVC is licensed through
// several patent pools, and a default build decodes only royalty-free codecs,
// AV1 and VP8. The two tags are independent, and a build that wants every
// decoder passes -tags h264,hevc.
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
// hands its planes over as they were decoded, with no conversion at all. VP8
// codes 8-bit 4:2:0 and nothing else, so its picture needs no branching at all.
//
// A sequence deeper than 8 bits is shifted down to 8. That is a loss of
// precision and nothing else: the code values are not tone mapped, so a PQ or
// HLG frame comes back with its own code values in an 8-bit container and will
// look flat and desaturated next to a tone-mapped render. The primaries and
// the transfer are not passed on, since nothing here tone maps.
//
// Alongside the image, Picture carries the matrix and the range the sequence
// declares, as H.273 code points. HEVC and AV1 report what their sequence
// headers say, which is unspecified, 2, for a stream that says nothing. VP8
// has one color space, so it always reports BT.601 studio range. The H.264
// decoder does not expose the sequence's VUI, so an H.264 picture reports
// unspecified whatever the stream declares.
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
// frames for display has to apply the range and the matrix itself, which is
// what RGBA is for.
//
// RGBA uses the matrix and range the picture reports. A matrix the stream does
// not declare is read as BT.601 studio range at every size, which is what
// ffmpeg does. Players often pick BT.709 for unsignaled HD instead, and this
// package used to, by picture height; on an unsignaled 3840x2160 frame that
// rule was a mean absolute error of 25 on green against ffmpeg's render, and
// reading it as BT.601 brings that to 0.2.
//
// # Memory
//
// One 3840x2160 Main10 keyframe on darwin/arm64, decoded in a fresh process
// from a 164 KB sample with the hevc build tag: 55 ms and 69 MiB of peak resident memory, of which
// 12 MiB is the image handed back. Six sequential decodes in one process run
// 45 to 50 ms each and peak at 162 MiB, since the runtime keeps what it has
// grown rather than returning it. There is no per-process warmup cost.
//
// The same size in H.264 High profile, from a 167 KB sample and with the h264
// build tag: 95 to 101 ms and 55 MiB of peak resident memory, of which 12 MiB
// is the image handed back. Six sequential decodes in one process stay at 93
// to 101 ms each and peak at 80 MiB.
//
// One 3840x2160 8-bit AV1 keyframe from a 159 KB sample, measured the same way:
// 36 to 38 ms and 32 to 33 MiB of peak resident memory.
//
// A picture is refused before a plane is allocated for it if the sequence
// declares more luma samples than maxLumaSamples, which is the largest picture
// any HEVC level allows. Every codec takes that bound: the H.264 one counted in
// macroblocks, the AV1 one through the decoder's own frame size limit, and the
// VP8 one from the frame header, which is read before the frame is.
package decode

import (
	"errors"
	"fmt"
	"image"
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
	maxLumaSamples = 35651584
	// maxThreads bounds the goroutines one picture is spread over.
	//
	// ponytail: a thumbnail is a background chore, so it does not get the whole
	// machine. Raise it if decode latency ever matters more than that does.
	maxThreads = 4
)

// Picture is one decoded keyframe and the color description its sequence
// declares, which is what RGBA needs to convert it.
type Picture struct {
	// Image is the decoded picture. See the package documentation for what it
	// holds.
	Image image.Image
	// Matrix is the sequence's matrix_coefficients, as the code points of
	// ITU-T H.273: 1 is BT.709, 5 and 6 are BT.601, 9 is BT.2020, and 2 is
	// unspecified, which is what a sequence that declares nothing reports.
	Matrix int
	// FullRange is the sequence's full-range flag. False, studio range, is
	// also what a sequence that declares nothing reports.
	FullRange bool
}

// The H.273 matrix_coefficients code points this package names.
const (
	matrixBT709       = 1
	matrixUnspecified = 2
	matrixBT601       = 6
	matrixBT2020      = 9
)

// Keyframe decodes one keyframe sample to a picture.
//
// codec is the container's short codec name, config the raw decoder
// configuration record the track carries, nalLengthSize the width in bytes of
// the length prefix on each NAL unit in sample, and sample the keyframe exactly
// as the container stores it. The arguments are the fields of an
// isobmff.SyncSample, spelled out rather than imported, so that this package
// does not depend on any one container.
//
// See the package documentation for what the returned image holds, what happens
// to a sequence deeper than 8 bits, what each codec reports about color, and
// what is refused.
func Keyframe(codec string, config []byte, nalLengthSize int, sample []byte) (Picture, error) {
	switch codec {
	case "hevc":
		return hevcKeyframe(config, nalLengthSize, sample)
	case "h264":
		return h264Keyframe(config, nalLengthSize, sample)
	case "vp8":
		return vp8Keyframe(sample)
	case "av1":
		return av1Keyframe(sample)
	default:
		return Picture{}, fmt.Errorf("%w: %q", ErrUnsupportedCodec, codec)
	}
}

// plane is one 8-bit image plane with its conformance crop already applied.
type plane struct {
	pix    []byte
	stride int
}
