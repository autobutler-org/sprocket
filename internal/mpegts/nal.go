package mpegts

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// NAL unit types this package acts on. ITU-T H.264 table 7-1 and ITU-T H.265
// table 7-1.
const (
	h264IDR = 5
	h264SPS = 7
	h264PPS = 8
	h264AUD = 9

	hevcFirstIRAP = 16 // BLA_W_LP
	hevcLastIRAP  = 21 // CRA_NUT
	hevcVPS       = 32
	hevcSPS       = 33
	hevcPPS       = 34
	hevcAUD       = 35
	hevcFirstNVCL = 32
)

// Access unit limits.
const (
	// maxParamBytes is the longest parameter set this package holds on to. A
	// real one is tens of bytes; one with a large VUI and HRD runs to a few
	// hundred.
	maxParamBytes = 4 << 10
	// maxAUUnits bounds the NAL units one access unit may hold. A picture
	// coded as one slice per macroblock row of an 8K frame has a few hundred.
	maxAUUnits = 1 << 12
)

// nalType reads the unit type out of a NAL unit's first header byte.
func nalType(hevc bool, header byte) byte {
	if hevc {
		return header >> 1 & 0x3f
	}
	return header & 0x1f
}

// isParamSet reports whether a unit type is a parameter set, which the MP4
// family keeps in the configuration record rather than in the samples.
func isParamSet(hevc bool, typ byte) bool {
	if hevc {
		return typ == hevcVPS || typ == hevcSPS || typ == hevcPPS
	}
	return typ == h264SPS || typ == h264PPS
}

// isRandomAccess reports whether a unit type is a slice of a picture that
// decodes on its own: an IDR picture in H.264, any IRAP picture in HEVC.
func isRandomAccess(hevc bool, typ byte) bool {
	if hevc {
		return typ >= hevcFirstIRAP && typ <= hevcLastIRAP
	}
	return typ == h264IDR
}

// isVCL reports whether a unit type carries picture data, which is where the
// search for what kind of picture an access unit holds can stop.
func isVCL(hevc bool, typ byte) bool {
	if hevc {
		return typ < hevcFirstNVCL
	}
	return typ >= 1 && typ <= h264IDR
}

// annexB splits an Annex B byte stream into NAL units as its bytes arrive, in
// pieces of any size. It hands each unit's bytes to data as they go past and
// calls open at each start code, so a unit that spans a hundred transport
// packets costs nothing to follow.
//
// A zero byte is held until the next byte says what it was: part of a start
// code, the zero_byte in front of a four byte one, trailing_zero_8bits after a
// unit, or data. Emulation prevention means a unit never holds two zeros
// followed by anything below 4, so two zeros and a one is always a start code,
// and zeros held when a start code or the end arrives are dropped. That is
// what every Annex B to length-prefixed converter does. ITU-T H.264 Annex B.
type annexB struct {
	zeros int
	in    bool
}

// zeroRun is a supply of zero bytes for a unit whose held zeros turn out to be
// data.
var zeroRun [64]byte

func (a *annexB) feed(p []byte, data func([]byte) error, open func() error) error {
	for len(p) > 0 {
		if p[0] == 0 {
			a.zeros++
			p = p[1:]
			continue
		}
		if p[0] == 1 && a.zeros >= 2 {
			a.zeros, a.in = 0, true
			p = p[1:]
			if err := open(); err != nil {
				return err
			}
			continue
		}
		n := bytes.IndexByte(p, 0)
		if n < 0 {
			n = len(p)
		}
		if a.in {
			for a.zeros > 0 {
				k := min(a.zeros, len(zeroRun))
				if err := data(zeroRun[:k]); err != nil {
					return err
				}
				a.zeros -= k
			}
			if err := data(p[:n]); err != nil {
				return err
			}
		}
		a.zeros = 0
		p = p[n:]
	}
	return nil
}

// nalUnit is one NAL unit of an access unit as the measuring pass found it.
type nalUnit struct {
	size int
	keep bool
}

// measure is the first pass over an access unit: it finds each NAL unit, sizes
// it, and decides whether the MP4 family keeps it. An access unit delimiter is
// dropped, since the MP4 family marks sample boundaries itself, and so is a
// parameter set the configuration record already holds byte for byte; one that
// differs is kept in the sample, where a decoder finds it when it needs it.
type measure struct {
	hevc   bool
	params [][]byte // the parameter sets the configuration record holds
	record bool     // keep the units, which the writing pass needs
	// collect keeps every parameter set seen, which is how a configuration
	// record is built out of the stream.
	collect bool

	split  annexB
	cur    nalUnit
	head   []byte
	in     bool
	units  []nalUnit
	count  int
	size   int64 // the length-prefixed size of what is kept
	random bool  // a random access slice was seen
	vcl    bool  // a slice was seen, so the picture type is known
	found  [][]byte
}

func (m *measure) reset() {
	m.split, m.cur, m.head, m.in = annexB{}, nalUnit{}, m.head[:0], false
	m.units, m.count, m.size, m.random, m.vcl = m.units[:0], 0, 0, false, false
	m.found = m.found[:0]
}

func (m *measure) feed(p []byte) error { return m.split.feed(p, m.data, m.open) }

func (m *measure) data(p []byte) error {
	m.cur.size += len(p)
	if room := maxParamBytes + 1 - len(m.head); room > 0 {
		m.head = append(m.head, p[:min(room, len(p))]...)
	}
	return nil
}

func (m *measure) open() error {
	m.close()
	if m.count >= maxAUUnits {
		return fmt.Errorf("%w: an access unit of over %d NAL units", ErrMalformed, maxAUUnits)
	}
	m.cur, m.head, m.in = nalUnit{}, m.head[:0], true
	return nil
}

// end closes the last unit once the access unit's bytes are spent.
func (m *measure) end() { m.close() }

func (m *measure) close() {
	if !m.in {
		return
	}
	m.in = false
	m.count++
	if m.cur.size == 0 {
		// Two start codes in a row. There is nothing to carry.
		if m.record {
			m.units = append(m.units, m.cur)
		}
		return
	}
	typ := nalType(m.hevc, m.head[0])
	m.cur.keep = true
	switch {
	case typ == h264AUD && !m.hevc, typ == hevcAUD && m.hevc:
		m.cur.keep = false
	case isParamSet(m.hevc, typ):
		if m.collect && m.cur.size <= maxParamBytes {
			m.found = append(m.found, bytes.Clone(m.head))
		}
		for _, p := range m.params {
			if m.cur.size == len(p) && bytes.Equal(m.head, p) {
				m.cur.keep = false
				break
			}
		}
	case isVCL(m.hevc, typ):
		m.vcl = true
		m.random = m.random || isRandomAccess(m.hevc, typ)
	}
	if m.cur.keep {
		m.size += 4 + int64(m.cur.size)
	}
	if m.record {
		m.units = append(m.units, m.cur)
	}
}

// lengthPrefixer is the second pass over an access unit: the same split, with
// each unit the first pass kept written behind a four byte length.
type lengthPrefixer struct {
	units []nalUnit
	w     func([]byte) error
	split annexB
	at    int
	lead  [4]byte
}

func (l *lengthPrefixer) feed(p []byte) error { return l.split.feed(p, l.data, l.open) }

func (l *lengthPrefixer) open() error {
	l.at++
	if l.at > len(l.units) {
		return fmt.Errorf("%w: the access unit changed between two reads of it", ErrMalformed)
	}
	if u := l.units[l.at-1]; u.keep {
		binary.BigEndian.PutUint32(l.lead[:], uint32(u.size))
		return l.w(l.lead[:])
	}
	return nil
}

func (l *lengthPrefixer) data(p []byte) error {
	if l.at == 0 || !l.units[l.at-1].keep {
		return nil
	}
	return l.w(p)
}

// toLengthPrefixed converts an Annex B access unit held in memory into the
// length-prefixed form the MP4 family and the decoders take, with the same
// units dropped that a remux drops.
func toLengthPrefixed(au []byte, hevc bool, params [][]byte) ([]byte, error) {
	m := measure{hevc: hevc, params: params, record: true}
	if err := m.feed(au); err != nil {
		return nil, err
	}
	m.end()
	out := make([]byte, 0, m.size)
	l := lengthPrefixer{units: m.units, w: func(p []byte) error {
		out = append(out, p...)
		return nil
	}}
	if err := l.feed(au); err != nil {
		return nil, err
	}
	return out, nil
}

// rbsp strips the emulation prevention bytes out of a NAL unit: every 0x03
// that follows two zeros was put there so the payload cannot contain a start
// code, and a parser of the payload has to take it back out.
func rbsp(nal []byte) []byte {
	out := make([]byte, 0, len(nal))
	zeros := 0
	for _, b := range nal {
		if zeros >= 2 && b == 3 {
			zeros = 0
			continue
		}
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, b)
	}
	return out
}

// bitReader reads an RBSP a field at a time. A read past the end sets err and
// yields zero from then on, so a caller checks once at the end.
type bitReader struct {
	src []byte
	pos int
	err bool
}

func (r *bitReader) u(n int) uint32 {
	var v uint32
	for range n {
		if r.pos >= len(r.src)*8 {
			r.err = true
			return 0
		}
		v = v<<1 | uint32(r.src[r.pos>>3]>>(7-r.pos&7)&1)
		r.pos++
	}
	return v
}

func (r *bitReader) skip(n int) {
	if n > len(r.src)*8-r.pos {
		r.err, r.pos = true, len(r.src)*8
		return
	}
	r.pos += n
}

// ue reads an unsigned Exp-Golomb field. A prefix of 32 zeros codes nothing a
// real stream holds, so it fails rather than reading on.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.u(1) == 0 {
		if r.err || zeros >= 31 {
			r.err = true
			return 0
		}
		zeros++
	}
	return uint32(1)<<zeros - 1 + r.u(zeros)
}

func (r *bitReader) se() int32 {
	v := r.ue()
	if v&1 == 1 {
		return int32(v/2 + 1)
	}
	return -int32(v / 2)
}

// picture is what a sequence parameter set says about the frames that follow
// it: the displayed size, after the cropping window, and what the MP4 family's
// configuration records repeat.
type picture struct {
	width, height          int
	chroma                 uint32
	lumaDepth, chromaDepth uint32 // minus 8, as coded
	subLayers              uint32 // HEVC only, minus 1, as coded
}

// cropped applies a conformance or cropping window, measured in chroma units,
// to a coded size. ITU-T H.264 7.4.2.1.1 and ITU-T H.265 7.4.3.2.1.
func cropped(coded, units, lo, hi uint32) (int, bool) {
	cut := uint64(units) * (uint64(lo) + uint64(hi))
	if uint64(coded) <= cut {
		return 0, false
	}
	return int(uint64(coded) - cut), true
}

// chromaUnits is the width and height of one chroma sample in luma samples,
// which is what a cropping window is measured in.
func chromaUnits(chroma uint32, separate bool) (x, y uint32) {
	if separate {
		return 1, 1
	}
	switch chroma {
	case 1:
		return 2, 2
	case 2:
		return 2, 1
	}
	return 1, 1
}

// h264HighProfiles are the profiles whose sequence parameter set carries the
// chroma format, bit depths, and scaling matrices. ITU-T H.264 7.3.2.1.1.
var h264HighProfiles = map[uint32]bool{
	100: true, 110: true, 122: true, 244: true, 44: true, 83: true,
	86: true, 118: true, 128: true, 138: true, 139: true, 134: true, 135: true,
}

// parseH264SPS reads an H.264 sequence parameter set, NAL header included, up
// to its cropping window. ITU-T H.264 7.3.2.1.1.
func parseH264SPS(nal []byte) (picture, error) {
	if len(nal) < 4 {
		return picture{}, fmt.Errorf("%w: a %d byte sequence parameter set", ErrMalformed, len(nal))
	}
	r := &bitReader{src: rbsp(nal[1:])}
	profile := r.u(8)
	r.skip(16) // constraint flags and level_idc
	r.ue()     // seq_parameter_set_id
	p := picture{chroma: 1}
	var separate bool
	if h264HighProfiles[profile] {
		p.chroma = r.ue()
		if p.chroma == 3 {
			separate = r.u(1) == 1
		}
		p.lumaDepth, p.chromaDepth = r.ue(), r.ue()
		r.skip(1) // qpprime_y_zero_transform_bypass_flag
		if r.u(1) == 1 {
			lists := 8
			if p.chroma == 3 {
				lists = 12
			}
			for i := range lists {
				if r.u(1) == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.skip(1) // delta_pic_order_always_zero_flag
		r.se()    // offset_for_non_ref_pic
		r.se()    // offset_for_top_to_bottom_field
		cycle := r.ue()
		if cycle > 255 {
			return picture{}, fmt.Errorf("%w: a picture order cycle of %d frames", ErrMalformed, cycle)
		}
		for range cycle {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.skip(1) // gaps_in_frame_num_value_allowed_flag
	widthMbs, heightUnits := r.ue()+1, r.ue()+1
	frameMbsOnly := r.u(1)
	if frameMbsOnly == 0 {
		r.skip(1) // mb_adaptive_frame_field_flag
	}
	r.skip(1) // direct_8x8_inference_flag
	var left, right, top, bottom uint32
	if r.u(1) == 1 {
		left, right, top, bottom = r.ue(), r.ue(), r.ue(), r.ue()
	}
	if r.err || widthMbs > 1<<12 || heightUnits > 1<<12 {
		return picture{}, fmt.Errorf("%w: a sequence parameter set that does not parse", ErrMalformed)
	}

	unitX, unitY := chromaUnits(p.chroma, separate)
	if p.chroma == 0 {
		unitX, unitY = 1, 1
	}
	unitY *= 2 - frameMbsOnly
	var okW, okH bool
	p.width, okW = cropped(widthMbs*16, unitX, left, right)
	p.height, okH = cropped(heightUnits*16*(2-frameMbsOnly), unitY, top, bottom)
	if !okW || !okH {
		return picture{}, fmt.Errorf("%w: a cropping window larger than the picture", ErrMalformed)
	}
	return p, nil
}

// skipScalingList steps over one scaling list. ITU-T H.264 7.3.2.1.1.1.
func skipScalingList(r *bitReader, size int) {
	last, next := int32(8), int32(8)
	for range size {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
		if r.err {
			return
		}
	}
}

// parseHEVCSPS reads an HEVC sequence parameter set, NAL header included, up
// to its bit depths. ITU-T H.265 7.3.2.2.1.
func parseHEVCSPS(nal []byte) (picture, error) {
	const (
		generalPTLBits  = 88 // profile_tier_level's general part, less level_idc
		subLayerPTLBits = 88
		maxSubLayers    = 7
	)
	if len(nal) < 3 {
		return picture{}, fmt.Errorf("%w: a %d byte sequence parameter set", ErrMalformed, len(nal))
	}
	r := &bitReader{src: rbsp(nal[2:])}
	r.skip(4) // sps_video_parameter_set_id
	var p picture
	p.subLayers = r.u(3)
	r.skip(1) // sps_temporal_id_nesting_flag
	if p.subLayers >= maxSubLayers {
		return picture{}, fmt.Errorf("%w: %d sub-layers", ErrMalformed, p.subLayers+1)
	}
	r.skip(generalPTLBits + 8)
	var profilePresent, levelPresent [maxSubLayers]bool
	for i := range p.subLayers {
		profilePresent[i], levelPresent[i] = r.u(1) == 1, r.u(1) == 1
	}
	if p.subLayers > 0 {
		r.skip(2 * (8 - int(p.subLayers)))
	}
	for i := range p.subLayers {
		if profilePresent[i] {
			r.skip(subLayerPTLBits)
		}
		if levelPresent[i] {
			r.skip(8)
		}
	}
	r.ue() // sps_seq_parameter_set_id
	p.chroma = r.ue()
	var separate bool
	if p.chroma == 3 {
		separate = r.u(1) == 1
	}
	width, height := r.ue(), r.ue()
	var left, right, top, bottom uint32
	if r.u(1) == 1 {
		left, right, top, bottom = r.ue(), r.ue(), r.ue(), r.ue()
	}
	p.lumaDepth, p.chromaDepth = r.ue(), r.ue()
	if r.err || p.chroma > 3 || width == 0 || height == 0 || width > 1<<16 || height > 1<<16 {
		return picture{}, fmt.Errorf("%w: a sequence parameter set that does not parse", ErrMalformed)
	}

	unitX, unitY := chromaUnits(p.chroma, separate)
	var okW, okH bool
	p.width, okW = cropped(width, unitX, left, right)
	p.height, okH = cropped(height, unitY, top, bottom)
	if !okW || !okH {
		return picture{}, fmt.Errorf("%w: a conformance window larger than the picture", ErrMalformed)
	}
	return p, nil
}

// paramSets sorts the parameter sets found in the stream by type, first seen
// first, keeping one copy of each distinct set.
func paramSets(found [][]byte, hevc bool) map[byte][][]byte {
	out := map[byte][][]byte{}
	for _, nal := range found {
		if len(nal) < 2 {
			continue
		}
		typ := nalType(hevc, nal[0])
		dup := false
		for _, have := range out[typ] {
			dup = dup || bytes.Equal(have, nal)
		}
		if !dup {
			out[typ] = append(out[typ], nal)
		}
	}
	return out
}

// avcC builds an AVCDecoderConfigurationRecord out of the parameter sets found
// in the stream, with a four byte NAL length. ISO/IEC 14496-15 5.3.3.1.
func avcC(sps, pps [][]byte, p picture) []byte {
	first := sps[0]
	out := []byte{1, first[1], first[2], first[3], 0xfc | 3, 0xe0 | byte(len(sps))}
	for _, s := range sps {
		out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
		out = append(out, s...)
	}
	out = append(out, byte(len(pps)))
	for _, s := range pps {
		out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
		out = append(out, s...)
	}
	if h264HighProfiles[uint32(first[1])] {
		out = append(out, 0xfc|byte(p.chroma&3), 0xf8|byte(p.lumaDepth&7), 0xf8|byte(p.chromaDepth&7), 0)
	}
	return out
}

// hvcC builds an HEVCDecoderConfigurationRecord out of the parameter sets found
// in the stream, with a four byte NAL length. The general profile, tier, and
// level are the twelve bytes that follow the sequence parameter set's first
// byte. ISO/IEC 14496-15 8.3.3.1.
func hvcC(sets map[byte][][]byte, p picture) ([]byte, error) {
	sps := rbsp(sets[hevcSPS][0][2:])
	if len(sps) < 13 {
		return nil, fmt.Errorf("%w: a sequence parameter set too short for its profile", ErrMalformed)
	}
	out := []byte{1}
	out = append(out, sps[1:13]...)
	out = append(out,
		0xf0, 0x00, // min_spatial_segmentation_idc, unknown
		0xfc,                  // parallelismType, unknown
		0xfc|byte(p.chroma&3), // chroma_format_idc
		0xf8|byte(p.lumaDepth&7),
		0xf8|byte(p.chromaDepth&7),
		0, 0, // avgFrameRate, unstated
		byte((p.subLayers+1)&7)<<3|(sps[0]&1)<<2|3, // layers, nesting, four byte lengths
	)
	var arrays byte
	for _, typ := range [...]byte{hevcVPS, hevcSPS, hevcPPS} {
		if len(sets[typ]) > 0 {
			arrays++
		}
	}
	out = append(out, arrays)
	for _, typ := range [...]byte{hevcVPS, hevcSPS, hevcPPS} {
		if len(sets[typ]) == 0 {
			continue
		}
		out = append(out, 0x80|typ)
		out = binary.BigEndian.AppendUint16(out, uint16(len(sets[typ])))
		for _, s := range sets[typ] {
			out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
			out = append(out, s...)
		}
	}
	return out, nil
}
