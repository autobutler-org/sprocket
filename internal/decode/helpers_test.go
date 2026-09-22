package decode_test

import (
	"encoding/binary"
	"math/bits"
	"testing"

	"github.com/gen2brain/h265/hevc"
)

// bitWriter builds an RBSP out of the fields the tests need to state exactly. It
// keeps one byte per bit and packs at the end, which is slow and obvious.
type bitWriter struct {
	bits []byte
}

// u appends an n bit unsigned integer, most significant bit first.
func (w *bitWriter) u(n int, value uint32) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, byte(value>>uint(i)&1))
	}
}

// ue appends an unsigned Exp-Golomb coded field.
func (w *bitWriter) ue(value uint32) {
	size := bits.Len32(value + 1)
	w.u(2*size-1, value+1)
}

// bytes packs the written bits, padding the last byte with zeros.
func (w *bitWriter) bytes() []byte {
	out := make([]byte, (len(w.bits)+7)/8)
	for i, bit := range w.bits {
		out[i/8] |= bit << (7 - i%8)
	}
	return out
}

// spsNAL builds a sequence parameter set NAL unit declaring a 4:2:0 picture of
// the given coded size. Only the prefix the size check reads is real; the rest
// of the sequence parameter set is absent, which is enough because nothing gets
// as far as decoding one.
func spsNAL(width, height uint32) []byte {
	var body bitWriter
	body.u(4, 0) // sps_video_parameter_set_id.
	body.u(3, 0) // sps_max_sub_layers_minus1.
	body.u(1, 1) // sps_temporal_id_nesting_flag.
	for range 12 {
		body.u(8, 0) // profile_tier_level.
	}
	body.ue(0) // sps_seq_parameter_set_id.
	body.ue(1) // chroma_format_idc, 4:2:0.
	body.ue(width)
	body.ue(height)

	const (
		spsNALType = 33
		temporalID = 1
	)
	return append([]byte{spsNALType << 1, temporalID}, body.bytes()...)
}

// hvcCRecord wraps NAL units in an HEVCDecoderConfigurationRecord holding one
// array, with a four byte NAL length prefix.
func hvcCRecord(t testing.TB, nals ...[]byte) []byte {
	t.Helper()

	const (
		headerBytes       = 23
		lengthSizeMinus1  = 3
		arrayNALUnitType  = 33
		reservedThenSize  = 0xfc
		numOfArraysOffset = 22
	)
	record := make([]byte, headerBytes)
	record[0] = 1 // configurationVersion.
	record[headerBytes-2] = reservedThenSize | lengthSizeMinus1
	if len(nals) == 0 {
		return record
	}

	record[numOfArraysOffset] = 1
	record = append(record, arrayNALUnitType, 0, 0)
	binary.BigEndian.PutUint16(record[len(record)-2:], uint16(len(nals)))
	for _, nal := range nals {
		record = binary.BigEndian.AppendUint16(record, uint16(len(nal)))
		record = append(record, nal...)
	}
	return record
}

// encodeKeyframe codes one intra frame at a size that does not fill the coding
// tree grid, and returns it split the way a container splits it: the parameter
// sets in an hvcC record, the slices in a length-prefixed sample.
func encodeKeyframe(t testing.TB, width, height int, chroma hevc.ChromaFormat) (config, sample []byte) {
	t.Helper()

	encoder, err := hevc.NewEncoder(hevc.EncoderOptions{Width: width, Height: height, Chroma: chroma})
	if err != nil {
		t.Fatalf("new encoder: %v", err)
	}
	frame := hevc.Frame{Y: make([]uint8, width*height), StrideY: width}
	for i := range frame.Y {
		frame.Y[i] = byte(i)
	}
	if chroma != hevc.ChromaMono {
		chromaW, chromaH := width, height
		if chroma != hevc.Chroma444 {
			chromaW = (width + 1) / 2
		}
		if chroma == hevc.Chroma420 {
			chromaH = (height + 1) / 2
		}
		frame.Cb = make([]uint8, chromaW*chromaH)
		frame.Cr = make([]uint8, chromaW*chromaH)
		frame.StrideC = chromaW
	}

	nals, err := encoder.Encode(frame)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var params [][]byte
	for _, nal := range nals {
		if nal.Type.IsVCL() {
			unit := hevc.MarshalNAL(nal)
			sample = binary.BigEndian.AppendUint32(sample, uint32(len(unit)))
			sample = append(sample, unit...)
			continue
		}
		params = append(params, hevc.MarshalNAL(nal))
	}
	return hvcCRecord(t, params...), sample
}
