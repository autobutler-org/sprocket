package isobmff

import (
	"fmt"
	"strings"
)

// Fixed offsets inside a sample entry. ISO/IEC 14496-12 12.1.3 and 12.2.3.
const (
	// visualEntryHead is the bytes of a VisualSampleEntry before its child
	// boxes: the base entry, the pre-defined fields, the dimensions, the
	// resolutions, the 32-byte compressor name, and the depth.
	visualEntryHead = 78
	// audioEntryHead is the same for an AudioSampleEntry, through the sample
	// rate. A version 1 or 2 entry carries extra fields after it.
	audioEntryHead = 28
	// audioEntryV1Extra and audioEntryV2Extra are those extra fields.
	audioEntryV1Extra = 16
	audioEntryV2Extra = 36
)

// codecNames maps a sample entry format to this package's stable short name.
var codecNames = map[string]string{
	"avc1": "h264", "avc2": "h264", "avc3": "h264", "avc4": "h264",
	"hvc1": "hevc", "hev1": "hevc",
	"av01": "av1",
	"vp08": "vp8", "vp09": "vp9",
	"Opus": "opus",
}

// byteRange locates a sample in the file.
type byteRange struct{ offset, size int64 }

// sampleTables is a set of views into the buffer the moov was read into.
// Nothing is copied and nothing is expanded per sample: a table declaring more
// entries than the box holds fails when it is sliced, before any allocation.
type sampleTables struct {
	stts []byte // (sample_count, sample_delta) pairs
	ctts []byte // (sample_count, sample_offset) pairs
	stsc []byte // (first_chunk, samples_per_chunk, sample_description_index)
	stss []byte // 1-based sync sample numbers, increasing
	// chunks is stco or co64, chunkWidth bytes per entry.
	chunks     []byte
	chunkWidth int
	// constSize is the stsz sample_size when every sample is the same size.
	constSize uint32
	// sizes is the per-sample size table when constSize is zero, sizeBits wide
	// per entry: 32 from stsz, or 4, 8, or 16 from stz2.
	sizes    []byte
	sizeBits uint8
	// count is the number of samples the tables describe.
	count uint32
	// cttsSigned is set for a version 1 ctts, whose offsets may be negative.
	cttsSigned bool
}

func (t *Track) parseStbl(body []byte, depth int) error {
	s := &t.tables
	return walk(body, depth, func(typ string, child []byte) error {
		switch typ {
		case "stsd":
			entry, err := parseStsd(child, t.Handler, depth+1)
			if err != nil {
				return err
			}
			t.Entry = entry
		case "stts":
			return s.parseRuns(child, &s.stts)
		case "ctts":
			version, _, _, ok := fullBoxVersion(child)
			if !ok {
				return fmt.Errorf("%w: ctts is %d bytes", ErrMalformed, len(child))
			}
			s.cttsSigned = version == 1
			return s.parseRuns(child, &s.ctts)
		case "stsc":
			return s.parseTable(child, 12, &s.stsc)
		case "stss":
			return s.parseTable(child, 4, &s.stss)
		case "stco":
			s.chunkWidth = 4
			return s.parseTable(child, 4, &s.chunks)
		case "co64":
			s.chunkWidth = 8
			return s.parseTable(child, 8, &s.chunks)
		case "stsz":
			return s.parseStsz(child)
		case "stz2":
			return s.parseStz2(child)
		}
		return nil
	})
}

// parseTable stores the entry array of a FullBox that starts with a count.
func (s *sampleTables) parseTable(body []byte, width int, dst *[]byte) error {
	_, _, rest, ok := fullBoxVersion(body)
	if !ok || len(rest) < 4 {
		return fmt.Errorf("%w: table box is %d bytes", ErrMalformed, len(body))
	}
	table, err := entries(rest[4:], be32(rest), width)
	if err != nil {
		return err
	}
	*dst = table
	return nil
}

// parseRuns stores an eight-byte-per-entry run table (stts or ctts).
func (s *sampleTables) parseRuns(body []byte, dst *[]byte) error {
	return s.parseTable(body, 8, dst)
}

// parseStsz reads the sample size box. ISO/IEC 14496-12 8.7.3.2.
func (s *sampleTables) parseStsz(body []byte) error {
	_, _, rest, ok := fullBoxVersion(body)
	if !ok || len(rest) < 8 {
		return fmt.Errorf("%w: stsz is %d bytes", ErrMalformed, len(body))
	}
	s.constSize, s.count = be32(rest), be32(rest[4:])
	if s.constSize != 0 {
		return nil
	}
	table, err := entries(rest[8:], s.count, 4)
	if err != nil {
		return err
	}
	s.sizes, s.sizeBits = table, 32
	return nil
}

// parseStz2 reads the compact sample size box. ISO/IEC 14496-12 8.7.3.3.
func (s *sampleTables) parseStz2(body []byte) error {
	_, _, rest, ok := fullBoxVersion(body)
	if !ok || len(rest) < 8 {
		return fmt.Errorf("%w: stz2 is %d bytes", ErrMalformed, len(body))
	}
	s.sizeBits, s.count = rest[3], be32(rest[4:])
	var need uint64
	switch s.sizeBits {
	case 4:
		need = (uint64(s.count) + 1) / 2
	case 8, 16:
		need = uint64(s.count) * uint64(s.sizeBits) / 8
	default:
		return fmt.Errorf("%w: stz2 field size %d", ErrMalformed, s.sizeBits)
	}
	if need > uint64(len(rest)-8) {
		return fmt.Errorf("%w: stz2 declares %d entries of %d bits with %d bytes available",
			ErrMalformed, s.count, s.sizeBits, len(rest)-8)
	}
	s.sizes = rest[8 : 8+need]
	return nil
}

// duration sums the stts run durations, in media ticks.
func (s *sampleTables) duration() uint64 {
	var total uint64
	for off := 0; off+8 <= len(s.stts); off += 8 {
		total += uint64(be32(s.stts[off:])) * uint64(be32(s.stts[off+4:]))
	}
	return total
}

// sampleAtTime returns the index of the sample covering a media time, clamped to
// the last sample.
//
// ponytail: linear over stts runs, of which a real file has one per distinct
// frame duration. Swap in a binary search over a cumulative table if a file with
// millions of runs ever turns up.
func (s *sampleTables) sampleAtTime(at uint64) uint32 {
	var index uint32
	var t uint64
	for off := 0; off+8 <= len(s.stts); off += 8 {
		count, delta := be32(s.stts[off:]), uint64(be32(s.stts[off+4:]))
		run := uint64(count) * delta
		if delta > 0 && at < t+run {
			return index + uint32((at-t)/delta)
		}
		t += run
		index += count
	}
	if s.count == 0 {
		return 0
	}
	return s.count - 1
}

// sampleTime returns a sample's decode time on the media timeline.
func (s *sampleTables) sampleTime(index uint32) uint64 {
	var seen uint32
	var t uint64
	for off := 0; off+8 <= len(s.stts); off += 8 {
		count, delta := be32(s.stts[off:]), uint64(be32(s.stts[off+4:]))
		if index < seen+count {
			return t + uint64(index-seen)*delta
		}
		t += uint64(count) * delta
		seen += count
	}
	return t
}

// compositionOffset returns a sample's ctts offset, which shifts its
// presentation time relative to its decode time. ISO/IEC 14496-12 8.6.1.3.
func (s *sampleTables) compositionOffset(index uint32) int64 {
	var seen uint32
	for off := 0; off+8 <= len(s.ctts); off += 8 {
		count, raw := be32(s.ctts[off:]), be32(s.ctts[off+4:])
		if index < seen+count {
			if s.cttsSigned {
				return int64(int32(raw))
			}
			return int64(raw)
		}
		seen += count
	}
	return 0
}

// syncAtOrBefore returns the index of the last sync sample at or before index.
// A track with no stss has no random access points declared, which by
// ISO/IEC 14496-12 8.6.2.1 means every sample is one.
func (s *sampleTables) syncAtOrBefore(index uint32) (uint32, bool) {
	if s.count == 0 {
		return 0, false
	}
	if len(s.stss) == 0 {
		return index, true
	}
	var best uint32
	found := false
	for off := 0; off+4 <= len(s.stss); off += 4 {
		n := be32(s.stss[off:])
		if n == 0 || n-1 > index {
			break // stss is in increasing order
		}
		best, found = n-1, true
	}
	return best, found
}

// sampleSize returns the stored size of one sample.
func (s *sampleTables) sampleSize(index uint32) (uint32, error) {
	if index >= s.count {
		return 0, fmt.Errorf("%w: sample %d of %d", ErrMalformed, index, s.count)
	}
	if s.constSize != 0 {
		return s.constSize, nil
	}
	i := int(index)
	switch s.sizeBits {
	case 32:
		return be32(s.sizes[i*4:]), nil
	case 16:
		return uint32(be16(s.sizes[i*2:])), nil
	case 8:
		return uint32(s.sizes[i]), nil
	case 4:
		if i%2 == 0 {
			return uint32(s.sizes[i/2] >> 4), nil
		}
		return uint32(s.sizes[i/2] & 0x0f), nil
	}
	return 0, fmt.Errorf("%w: no sample size table", ErrMalformed)
}

// totalBytes sums every sample size, which is what a per-track bitrate needs.
func (s *sampleTables) totalBytes() int64 {
	if s.constSize != 0 {
		return int64(s.count) * int64(s.constSize)
	}
	var total int64
	for i := range s.count {
		size, err := s.sampleSize(i)
		if err != nil {
			break
		}
		total += int64(size)
	}
	return total
}

// chunkOffset returns the file offset of a zero-based chunk.
func (s *sampleTables) chunkOffset(chunk uint32) (int64, error) {
	if s.chunkWidth == 0 {
		return 0, fmt.Errorf("%w: no chunk offset table", ErrMalformed)
	}
	off := int(chunk) * s.chunkWidth
	if off < 0 || off+s.chunkWidth > len(s.chunks) {
		return 0, fmt.Errorf("%w: chunk %d of %d", ErrMalformed, chunk, len(s.chunks)/s.chunkWidth)
	}
	if s.chunkWidth == 8 {
		return int64(be64(s.chunks[off:])), nil
	}
	return int64(be32(s.chunks[off:])), nil
}

// chunkOf locates the chunk holding a sample and the index of that chunk's first
// sample. ISO/IEC 14496-12 8.7.4.
//
// ponytail: linear over stsc groups rather than over chunks, so a file with one
// group per chunk is O(chunks). Real files have a handful of groups.
func (s *sampleTables) chunkOf(index uint32) (chunk, first uint32, err error) {
	if s.chunkWidth == 0 {
		return 0, 0, fmt.Errorf("%w: no chunk offset table", ErrMalformed)
	}
	chunkCount := uint32(len(s.chunks) / s.chunkWidth)
	var sample uint32
	for off := 0; off+12 <= len(s.stsc); off += 12 {
		firstChunk, perChunk := be32(s.stsc[off:]), be32(s.stsc[off+4:])
		if firstChunk == 0 || perChunk == 0 {
			return 0, 0, fmt.Errorf("%w: stsc entry %d has chunk %d and %d samples", ErrMalformed, off/12, firstChunk, perChunk)
		}
		lastChunk := chunkCount
		if next := off + 12; next+12 <= len(s.stsc) {
			if nextFirst := be32(s.stsc[next:]); nextFirst > 0 && nextFirst-1 < lastChunk {
				lastChunk = nextFirst - 1
			}
		}
		if lastChunk < firstChunk {
			continue
		}
		// span cannot overflow the uint32 addition below: if it were that
		// large, index would have fallen inside it and we would have returned.
		span := uint64(lastChunk-firstChunk+1) * uint64(perChunk)
		if uint64(index) < uint64(sample)+span {
			nth := (index - sample) / perChunk
			return firstChunk - 1 + nth, sample + nth*perChunk, nil
		}
		sample += uint32(span)
	}
	return 0, 0, fmt.Errorf("%w: sample %d belongs to no chunk", ErrMalformed, index)
}

// sampleRange returns where a sample lives in the file.
func (s *sampleTables) sampleRange(index uint32) (byteRange, error) {
	size, err := s.sampleSize(index)
	if err != nil {
		return byteRange{}, err
	}
	chunk, first, err := s.chunkOf(index)
	if err != nil {
		return byteRange{}, err
	}
	offset, err := s.chunkOffset(chunk)
	if err != nil {
		return byteRange{}, err
	}
	// Samples sit back to back inside a chunk, so the ones ahead of this sample
	// have to be added up. A constant sample size is multiplied rather than
	// summed: an stsz that declares one size and a billion samples has no table
	// to bound the loop.
	if s.constSize != 0 {
		offset += int64(index-first) * int64(s.constSize)
		return byteRange{offset: offset, size: int64(size)}, nil
	}
	// ponytail: O(samples per chunk), which a size table bounds to the bytes
	// the moov actually holds. A cumulative table would make it O(1) if a file
	// with enormous chunks ever shows up.
	for i := first; i < index; i++ {
		ahead, err := s.sampleSize(i)
		if err != nil {
			return byteRange{}, err
		}
		offset += int64(ahead)
	}
	return byteRange{offset: offset, size: int64(size)}, nil
}

// parseStsd reads the sample description box and returns its first entry. A
// track with more than one description is vanishingly rare and nothing
// downstream can act on the others. ISO/IEC 14496-12 8.5.2.
func parseStsd(body []byte, handler string, depth int) (SampleEntry, error) {
	_, _, rest, ok := fullBoxVersion(body)
	if !ok || len(rest) < 4 {
		return SampleEntry{}, fmt.Errorf("%w: stsd is %d bytes", ErrMalformed, len(body))
	}
	if be32(rest) == 0 {
		return SampleEntry{}, nil
	}
	list := rest[4:]
	h, err := parseHeader(list, int64(len(list)))
	if err != nil {
		return SampleEntry{}, err
	}
	return parseSampleEntry(h.typ, list[h.hdrSize:h.size], handler, depth+1)
}

func parseSampleEntry(format string, body []byte, handler string, depth int) (SampleEntry, error) {
	e := SampleEntry{Format: strings.TrimRight(format, " \x00")}

	var children []byte
	switch handler {
	case "vide":
		if len(body) < visualEntryHead {
			return e, fmt.Errorf("%w: %s visual entry is %d bytes", ErrMalformed, e.Format, len(body))
		}
		e.Width, e.Height = be16(body[24:]), be16(body[26:])
		children = body[visualEntryHead:]
	case "soun":
		if len(body) < audioEntryHead {
			return e, fmt.Errorf("%w: %s audio entry is %d bytes", ErrMalformed, e.Format, len(body))
		}
		e.ChannelCount, e.SampleRate = be16(body[16:]), be32(body[24:])>>16
		head := audioEntryHead
		switch be16(body[8:]) { // the QuickTime sound description version
		case 1:
			head += audioEntryV1Extra
		case 2:
			head += audioEntryV2Extra
		}
		if len(body) < head {
			return e, fmt.Errorf("%w: %s audio entry is %d bytes, need %d", ErrMalformed, e.Format, len(body), head)
		}
		children = body[head:]
	}

	if err := e.parseEntryChildren(children, depth); err != nil {
		return e, err
	}
	e.Codec = codecName(e.Format, e.ObjectType)
	return e, nil
}

// parseEntryChildren picks the codec configuration out of a sample entry.
func (e *SampleEntry) parseEntryChildren(children []byte, depth int) error {
	return walk(children, depth, func(typ string, child []byte) error {
		switch typ {
		case "avcC":
			e.Config = child
			// avcC byte 4 carries lengthSizeMinusOne in its low two bits.
			// ISO/IEC 14496-15 5.3.3.1.
			if len(child) >= 5 {
				e.NALLengthSize = int(child[4]&0x03) + 1
			}
		case "hvcC":
			e.Config = child
			// hvcC byte 21 carries lengthSizeMinusOne in its low two bits.
			// ISO/IEC 14496-15 8.3.3.1.
			if len(child) >= 23 {
				e.NALLengthSize = int(child[21]&0x03) + 1
			}
		case "av1C", "vpcC", "dOps":
			e.Config = child
		case "esds":
			e.ObjectType = esdsObjectType(child)
		case "wave":
			// QuickTime buries the esds of an mp4a entry in a wave box.
			return e.parseEntryChildren(child, depth+1)
		}
		return nil
	})
}

// codecName maps a sample entry format, and for mp4a its esds object type, to
// this package's stable short name. An unrecognized format reports itself, so a
// caller can log something useful rather than an empty string.
func codecName(format string, objectType byte) string {
	if format == "mp4a" {
		switch objectType {
		case 0x69, 0x6b: // MPEG-1 and MPEG-2 Part 3 audio
			return "mp3"
		case 0x40, 0x66, 0x67, 0x68: // MPEG-4 audio, and MPEG-2 Main, LC, SSR
			return "aac"
		case 0: // no esds to say otherwise, and mp4a in the wild is AAC
			return "aac"
		default:
			return "mp4a"
		}
	}
	if name, ok := codecNames[format]; ok {
		return name
	}
	return format
}

// Descriptor tags from the MPEG-4 object descriptor framework.
// ISO/IEC 14496-1 7.2.2.
const (
	tagESDescr        = 0x03
	tagDecoderConfig  = 0x04
	descriptorLenMax  = 4 // a length is at most four base-128 bytes
	esdsStreamDepend  = 0x80
	esdsURLFlag       = 0x40
	esdsOCRStreamFlag = 0x20
)

// esdsObjectType pulls the objectTypeIndication out of an ES descriptor, which
// is what distinguishes AAC from MP3 inside an mp4a entry.
func esdsObjectType(body []byte) byte {
	_, _, rest, ok := fullBoxVersion(body)
	if !ok {
		return 0
	}
	tag, es, _, ok := readDescriptor(rest)
	if !ok || tag != tagESDescr || len(es) < 3 {
		return 0
	}
	off := 3
	flags := es[2]
	if flags&esdsStreamDepend != 0 {
		off += 2
	}
	if flags&esdsURLFlag != 0 {
		if off >= len(es) {
			return 0
		}
		off += 1 + int(es[off])
	}
	if flags&esdsOCRStreamFlag != 0 {
		off += 2
	}
	if off >= len(es) {
		return 0
	}
	tag, config, _, ok := readDescriptor(es[off:])
	if !ok || tag != tagDecoderConfig || len(config) == 0 {
		return 0
	}
	return config[0]
}

// readDescriptor splits one tag-length-value descriptor off the front of b. The
// length is base-128, seven bits per byte, high bit set to continue.
func readDescriptor(b []byte) (tag byte, body, rest []byte, ok bool) {
	if len(b) < 2 {
		return 0, nil, nil, false
	}
	tag = b[0]
	size := 0
	off := 1
	for range descriptorLenMax {
		if off >= len(b) {
			return 0, nil, nil, false
		}
		c := b[off]
		off++
		size = size<<7 | int(c&0x7f)
		if c&0x80 == 0 {
			break
		}
	}
	if off+size > len(b) {
		return 0, nil, nil, false
	}
	return tag, b[off : off+size], b[off+size:], true
}
