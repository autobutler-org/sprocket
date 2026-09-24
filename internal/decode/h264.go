//go:build h264

package decode

import (
	"encoding/binary"
	"fmt"
	"image"
	"strings"

	h264 "github.com/rcarmo/go-264/decode"
	"github.com/rcarmo/go-264/frame"
)

const (
	// avccHeaderBytes is the fixed part of an AVCDecoderConfigurationRecord, up
	// to and including numOfSequenceParameterSets. ISO/IEC 14496-15 5.3.3.1.2.
	avccHeaderBytes = 6
	// annexBStartCode begins every NAL unit in the buffer the decoder reads.
	annexBStartCode = "\x00\x00\x00\x01"
	// maxFrameMacroblocks bounds one decoded picture. A macroblock is 16x16
	// luma samples, so this is the HEVC path's ceiling counted the way the
	// H.264 decoder counts, and the two codecs refuse the same picture.
	//
	// ponytail: the same 8K ceiling maxLumaSamples explains, shared so there is
	// one bound to change rather than two to keep in step.
	maxFrameMacroblocks = maxLumaSamples / (16 * 16)
)

// h264Keyframe decodes an H.264 keyframe. The parameter sets come from the avcC
// record and the slices from the sample, which is what a container splits
// between the two.
func h264Keyframe(config []byte, nalLengthSize int, sample []byte) (Picture, error) {
	if nalLengthSize < 1 || nalLengthSize > 4 {
		return Picture{}, fmt.Errorf("%w: the NAL length prefix is %d bytes, want 1 through 4",
			ErrCorruptSample, nalLengthSize)
	}
	stream, err := avccAnnexB(config, nalLengthSize, sample)
	if err != nil {
		return Picture{}, err
	}

	decoder := h264.NewDecoder()
	decoder.MaxFrames = 1
	decoder.MaxFrameMacroblocks = maxFrameMacroblocks

	pictures, err := decoder.Decode(stream)
	if err != nil {
		return Picture{}, h264Error(err)
	}
	if len(pictures) == 0 {
		// A buffer the decoder reads to the end without assembling a picture,
		// an empty one among them, comes back as no pictures and no error.
		return Picture{}, fmt.Errorf("%w: the decoder read %d bytes and produced no picture",
			ErrCorruptSample, len(stream))
	}
	img, err := frameImage(pictures[0])
	if err != nil {
		return Picture{}, err
	}
	// The decoder does not expose the sequence's VUI, so the color description
	// is unknown here whatever the stream declares.
	return Picture{Image: img, Matrix: matrixUnspecified}, nil
}

// avccAnnexB turns an avcC record and a length-prefixed sample into the single
// Annex B buffer the decoder reads: a start code before each parameter set from
// the record and before each NAL unit carved out of the sample. Emulation
// prevention bytes are already in the payloads, so nothing is escaped here.
//
// Every declared length is checked against what is left, so a record or a
// sample that lies about its own contents is refused rather than read past.
func avccAnnexB(config []byte, nalLengthSize int, sample []byte) ([]byte, error) {
	if len(config) < avccHeaderBytes {
		return nil, fmt.Errorf("%w: the avcC record is %d bytes, too short to hold one",
			ErrCorruptSample, len(config))
	}
	if config[0] != 1 {
		return nil, fmt.Errorf("%w: the avcC record is version %d, want 1",
			ErrCorruptSample, config[0])
	}

	// numOfSequenceParameterSets is five bits, with three reserved above it.
	stream, rest, err := appendParameterSets(nil, config[avccHeaderBytes:], int(config[5]&0x1f))
	if err != nil {
		return nil, err
	}
	if len(rest) < 1 {
		return nil, fmt.Errorf("%w: the avcC record ends before numOfPictureParameterSets",
			ErrCorruptSample)
	}
	stream, _, err = appendParameterSets(stream, rest[1:], int(rest[0]))
	if err != nil {
		return nil, err
	}
	// What follows for profile 100, 110, 122, and 144 is the chroma format, the
	// bit depths, and the sequence parameter set extensions. The record may end
	// here instead, which is legal and common, and the first three are in the
	// sequence parameter set anyway, so none of it is read.

	for len(sample) > 0 {
		if len(sample) < nalLengthSize {
			return nil, fmt.Errorf("%w: the sample ends inside a %d byte NAL length prefix",
				ErrCorruptSample, nalLengthSize)
		}
		size := 0
		for _, b := range sample[:nalLengthSize] {
			size = size<<8 | int(b)
		}
		sample = sample[nalLengthSize:]
		if size == 0 || size > len(sample) {
			return nil, fmt.Errorf("%w: the sample declares a %d byte NAL unit with %d bytes left",
				ErrCorruptSample, size, len(sample))
		}
		stream = append(append(stream, annexBStartCode...), sample[:size]...)
		sample = sample[size:]
	}
	return stream, nil
}

// appendParameterSets appends count parameter sets, each a 16 bit length and
// that many bytes, to an Annex B buffer, and returns what is left of the record.
// The lengths inside an avcC record are 16 bits whatever the sample's NAL length
// prefix is.
func appendParameterSets(stream, record []byte, count int) (out, rest []byte, err error) {
	for set := range count {
		if len(record) < 2 {
			return nil, nil, fmt.Errorf("%w: the avcC declares %d parameter sets and runs out at %d",
				ErrCorruptSample, count, set)
		}
		size := int(binary.BigEndian.Uint16(record[:2]))
		record = record[2:]
		if size > len(record) {
			return nil, nil, fmt.Errorf("%w: the avcC declares a %d byte parameter set with %d bytes left",
				ErrCorruptSample, size, len(record))
		}
		stream = append(append(stream, annexBStartCode...), record[:size]...)
		record = record[size:]
	}
	return stream, record, nil
}

// h264Error maps what the decoder reports onto this package's sentinels. The
// decoder builds both of the errors that are not a corrupt sample with
// fmt.Errorf and exports no sentinel for either, so the match is on the message:
// every "unsupported" one names a coding tool the decoder does not implement,
// and the budget one is a picture over the macroblock cap.
func h264Error(err error) error {
	switch message := err.Error(); {
	case strings.Contains(message, "unsupported "):
		return fmt.Errorf("%w: h264: %w", ErrUnsupportedCodec, err)
	case strings.Contains(message, "exceeds allocation budget"):
		return fmt.Errorf("%w: %w", ErrFrameTooLarge, err)
	default:
		return fmt.Errorf("%w: %w", ErrCorruptSample, err)
	}
}

// frameImage wraps a decoded picture as an image. The decoder hands back a view
// of the coded picture whose planes already start at the top left of the
// conformance window and whose Width and Height are the cropped size, so the
// image aliases the planes as they are. That is safe because the decoder is
// dropped with this call's frame.
func frameImage(picture *frame.Frame) (image.Image, error) {
	if !picture.IsIDR {
		// Only sync samples reach this package, so a picture that is not an IDR
		// came out of a sample the container was wrong about.
		return nil, fmt.Errorf("%w: the decoded picture is not an IDR frame", ErrCorruptSample)
	}
	width, height := picture.Width, picture.Height
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("%w: the decoded picture is %dx%d", ErrCorruptSample, width, height)
	}

	// The decoder decodes 8 bit 4:2:0 and refuses everything else, so there is
	// one plane layout here rather than the HEVC path's four.
	luma, err := eightBitPlane(picture.Y, nil, 8, picture.StrideY, 0, 0, width, height)
	if err != nil {
		return nil, err
	}
	chromaW, chromaH := (width+1)/2, (height+1)/2
	blue, err := eightBitPlane(picture.U, nil, 8, picture.StrideC, 0, 0, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	red, err := eightBitPlane(picture.V, nil, 8, picture.StrideC, 0, 0, chromaW, chromaH)
	if err != nil {
		return nil, err
	}
	return &image.YCbCr{
		Y: luma.pix, Cb: blue.pix, Cr: red.pix,
		YStride: luma.stride, CStride: blue.stride,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, width, height),
	}, nil
}
