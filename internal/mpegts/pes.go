package mpegts

import (
	"encoding/binary"
	"fmt"
)

// Timestamps are 33 bit counts of a 90 kHz clock. ISO/IEC 13818-1 2.4.3.7.
const (
	clockRate     = 90000
	timestampWrap = int64(1) << 33
)

// pesHeader is what the front of a PES packet says. ISO/IEC 13818-1 2.4.3.6.
type pesHeader struct {
	pts, dts       int64
	hasPTS, hasDTS bool
	streamID       byte
	size           int // the header's own bytes, which the payload follows
}

// Stream IDs whose PES packets carry no optional header, and so no timestamps.
// ISO/IEC 13818-1 table 2-22.
var bareStreamIDs = map[byte]bool{
	0xbc: true, // program_stream_map
	0xbe: true, // padding_stream
	0xbf: true, // private_stream_2
	0xf0: true, // ECM
	0xf1: true, // EMM
	0xf2: true, // DSMCC
	0xf8: true, // H.222.1 type E
	0xff: true, // program_stream_directory
}

// parsePESHeader decodes the header at the front of a PES packet. The header
// has to lie inside b, which is the payload of the packet that starts the PES:
// the standard allows it to spill into the next packet, and no muxer does.
//
// ponytail: a header that spills is refused as malformed. Gathering it across
// packets is the upgrade if a muxer that does it ever turns up.
func parsePESHeader(b []byte) (pesHeader, error) {
	const bare = 6 // start code prefix, stream_id, PES_packet_length
	if len(b) < bare || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return pesHeader{}, fmt.Errorf("%w: a PES packet with no start code", ErrMalformed)
	}
	h := pesHeader{streamID: b[3], size: bare}
	if bareStreamIDs[h.streamID] {
		return h, nil
	}

	const fixed = 9 // the bare header, two flag bytes, PES_header_data_length
	if len(b) < fixed || b[6]&0xc0 != 0x80 {
		return pesHeader{}, fmt.Errorf("%w: a PES header with no optional fields", ErrMalformed)
	}
	h.size = fixed + int(b[8])
	if h.size > len(b) {
		return pesHeader{}, fmt.Errorf("%w: a %d byte PES header in a %d byte payload", ErrMalformed, h.size, len(b))
	}
	fields := b[fixed:h.size]
	switch b[7] >> 6 {
	case 0x02:
		if len(fields) < 5 {
			return pesHeader{}, fmt.Errorf("%w: a PES header too short for its PTS", ErrMalformed)
		}
		h.pts, h.hasPTS = readTimestamp(fields), true
	case 0x03:
		if len(fields) < 10 {
			return pesHeader{}, fmt.Errorf("%w: a PES header too short for its PTS and DTS", ErrMalformed)
		}
		h.pts, h.hasPTS = readTimestamp(fields), true
		h.dts, h.hasDTS = readTimestamp(fields[5:]), true
	}
	return h, nil
}

// readTimestamp decodes a five byte PTS or DTS field: three, fifteen, and
// fifteen bits of the 33 bit value, each followed by a marker bit. The marker
// bits are not checked; a muxer that gets them wrong is still telling the
// time.
func readTimestamp(b []byte) int64 {
	return int64(b[0]>>1&0x07)<<30 |
		int64(binary.BigEndian.Uint16(b[1:])>>1)<<15 |
		int64(binary.BigEndian.Uint16(b[3:])>>1)
}

// unwrapNear places a raw 33 bit timestamp on the unwrapped timeline, as the
// value nearest ref. The clock wraps every 26.5 hours, so this is right for any
// timestamp within 13 hours of the reference either way, which covers any seek
// in a file shorter than that.
//
// ponytail: Probe and the keyframe search unwrap against the first timestamp,
// so a file over 13 hours reads its tail and its late keyframes wrong. The
// remux walk unwraps against the previous timestamp and has no such limit;
// counting rollovers across the file is the upgrade for the other two.
func unwrapNear(raw, ref int64) int64 {
	d := (raw - ref) % timestampWrap
	switch {
	case d >= timestampWrap/2:
		d -= timestampWrap
	case d < -timestampWrap/2:
		d += timestampWrap
	}
	return ref + d
}

// esPayload is the elementary stream bytes a packet carries: the whole payload,
// or for the packet that starts a PES, what follows the PES header. Every walk
// over a stream's bytes goes through here, which is what keeps the two passes
// of a remux agreeing on where each byte is.
func esPayload(p packet) ([]byte, pesHeader, error) {
	if !p.start {
		return p.payload, pesHeader{}, nil
	}
	h, err := parsePESHeader(p.payload)
	if err != nil {
		return nil, pesHeader{}, fmt.Errorf("the packet at %d: %w", p.offset, err)
	}
	return p.payload[h.size:], h, nil
}
