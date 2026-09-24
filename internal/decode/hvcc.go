//go:build hevc

package decode

import (
	"encoding/binary"
	"fmt"

	"github.com/gen2brain/h265/hevc"
)

// hvccHeaderBytes is the fixed part of an HEVCDecoderConfigurationRecord, up to
// and including numOfArrays. ISO/IEC 14496-15 8.3.3.1.
const hvccHeaderBytes = 23

// hvccParameterSets pulls the parameter set NAL units out of an hvcC record.
// Every declared length is checked against what is left of the record, so a
// record that lies about its own contents is refused rather than read past.
//
// A record with no arrays is not an error. A hev1 track may carry its parameter
// sets in the samples instead, and the decoder is the one that decides whether
// what it was given adds up to a sequence.
func hvccParameterSets(config []byte) ([]hevc.NALUnit, error) {
	if len(config) < hvccHeaderBytes {
		return nil, fmt.Errorf("%w: the hvcC record is %d bytes, too short to hold one",
			ErrCorruptSample, len(config))
	}
	arrays := int(config[hvccHeaderBytes-1])
	rest := config[hvccHeaderBytes:]

	nals := make([]hevc.NALUnit, 0, arrays)
	for array := range arrays {
		// One byte of array_completeness, a reserved bit, and NAL_unit_type,
		// which ParseNAL reads again out of the unit itself.
		if len(rest) < 3 {
			return nil, fmt.Errorf("%w: the hvcC declares %d arrays and runs out at %d",
				ErrCorruptSample, arrays, array)
		}
		count := int(binary.BigEndian.Uint16(rest[1:3]))
		rest = rest[3:]

		for nal := range count {
			if len(rest) < 2 {
				return nil, fmt.Errorf("%w: hvcC array %d declares %d units and runs out at %d",
					ErrCorruptSample, array, count, nal)
			}
			size := int(binary.BigEndian.Uint16(rest[:2]))
			rest = rest[2:]
			if size > len(rest) {
				return nil, fmt.Errorf("%w: the hvcC declares a %d byte NAL unit with %d bytes left",
					ErrCorruptSample, size, len(rest))
			}
			parsed, ok := hevc.ParseNAL(rest[:size])
			if !ok {
				return nil, fmt.Errorf("%w: hvcC array %d unit %d is not a NAL unit",
					ErrCorruptSample, array, nal)
			}
			nals = append(nals, parsed)
			rest = rest[size:]
		}
	}
	return nals, nil
}

// spsPictureSize reads the coded picture dimensions out of a sequence parameter
// set RBSP. ISO/IEC 23008-2 7.3.2.2. Only the prefix up to the dimensions is
// read; the decoder parses the rest.
func spsPictureSize(rbsp []byte) (width, height int, ok bool) {
	reader := &bitReader{src: rbsp}
	reader.skip(4) // sps_video_parameter_set_id.
	layers := int(reader.u(3))
	reader.skip(1) // sps_temporal_id_nesting_flag.
	skipProfileTierLevel(reader, layers)
	reader.ue()           // sps_seq_parameter_set_id.
	if reader.ue() == 3 { // chroma_format_idc.
		reader.skip(1) // separate_colour_plane_flag.
	}
	codedWidth, codedHeight := reader.ue(), reader.ue()
	if reader.err || codedWidth == 0 || codedHeight == 0 {
		return 0, 0, false
	}
	return int(codedWidth), int(codedHeight), true
}

// skipProfileTierLevel advances past profile_tier_level with a profile present,
// which is 96 fixed bits and then a block per sub-layer. ISO/IEC 23008-2 7.3.3.
func skipProfileTierLevel(reader *bitReader, layers int) {
	const (
		generalBits  = 96
		subLayerBits = 88
		maxSubLayers = 7
	)
	if layers > maxSubLayers {
		reader.err = true
		return
	}
	reader.skip(generalBits)

	var profile, level [maxSubLayers]bool
	for i := range layers {
		profile[i] = reader.u(1) == 1
		level[i] = reader.u(1) == 1
	}
	if layers > 0 {
		reader.skip(2 * (8 - layers)) // reserved_zero_2bits.
	}
	for i := range layers {
		if profile[i] {
			reader.skip(subLayerBits)
		}
		if level[i] {
			reader.skip(8) // sub_layer_level_idc.
		}
	}
}

// bitReader reads an RBSP a field at a time. A read past the end sets err and
// yields zero from then on, so a caller checks once at the end rather than after
// every field.
type bitReader struct {
	src []byte
	pos int // In bits.
	err bool
}

// u reads an n bit unsigned integer, most significant bit first.
func (r *bitReader) u(n int) uint32 {
	var value uint32
	for range n {
		if r.pos >= len(r.src)*8 {
			r.err = true
			return 0
		}
		value = value<<1 | uint32(r.src[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return value
}

// skip advances n bits without reading them.
func (r *bitReader) skip(n int) {
	if n > len(r.src)*8-r.pos {
		r.err = true
		r.pos = len(r.src) * 8
		return
	}
	r.pos += n
}

// ue reads an unsigned Exp-Golomb coded field. A prefix of 32 zeros or more
// codes nothing this decoder will ever see, so it fails rather than reading on.
func (r *bitReader) ue() uint32 {
	const maxPrefix = 31

	zeros := 0
	for zeros <= maxPrefix && r.u(1) == 0 {
		if r.err {
			return 0
		}
		zeros++
	}
	if r.err || zeros > maxPrefix {
		r.err = true
		return 0
	}
	return uint32(1<<zeros-1) + r.u(zeros)
}
