package mpegts

import (
	"encoding/binary"
	"fmt"
)

// Table IDs. ISO/IEC 13818-1 table 2-31.
const (
	tablePAT = 0x00
	tablePMT = 0x02
)

// maxSectionBytes is the most a PAT or PMT section can take up: a 12 bit
// section_length capped at 1021 by the standard, plus the three bytes in front
// of it. ISO/IEC 13818-1 2.4.4.10.
const maxSectionBytes = 1024

// Stream types this package names. ISO/IEC 13818-1 table 2-34, and ATSC A/52
// for the two Dolby types.
const (
	streamMPEG1Video = 0x01
	streamMPEG2Video = 0x02
	streamMPEG1Audio = 0x03
	streamMPEG2Audio = 0x04
	streamPrivate    = 0x06
	streamADTS       = 0x0f
	streamMPEG4Video = 0x10
	streamLATM       = 0x11
	streamH264       = 0x1b
	streamHEVC       = 0x24
	streamAC3        = 0x81
	streamEAC3       = 0x87
)

// Descriptor tags that name a codec inside a private stream. ETSI EN 300 468
// for the two DVB Dolby descriptors, ISO/IEC 13818-1 2.6.8 for registration.
const (
	descRegistration = 0x05
	descAC3          = 0x6a
	descEAC3         = 0x7a
)

// Stream kinds, which is all Probe needs to know about a stream to decide
// whether it is the video or the audio one.
const (
	kindOther = iota
	kindVideo
	kindAudio
)

// codecOf maps a PMT entry onto this library's codec short name and the kind of
// stream it is. MPEG audio is named mp3 for now; the first frame header decides
// between mp3 and mp2 once a PES of it has been read. A stream type this
// package does not name reports kindOther and is not read.
func codecOf(streamType byte, descriptors []byte) (string, int) {
	switch streamType {
	case streamH264:
		return "h264", kindVideo
	case streamHEVC:
		return "hevc", kindVideo
	case streamMPEG1Video:
		return "mpeg1video", kindVideo
	case streamMPEG2Video:
		return "mpeg2video", kindVideo
	case streamMPEG4Video:
		return "mpeg4", kindVideo
	case streamADTS:
		return "aac", kindAudio
	case streamLATM:
		// LATM wraps AAC in a framing the MP4 family has no use for and this
		// package does not take apart, so it is named apart from aac, which
		// keeps the remux compatibility table from promising it.
		return "aac_latm", kindAudio
	case streamMPEG1Audio, streamMPEG2Audio:
		return "mp3", kindAudio
	case streamAC3:
		return "ac-3", kindAudio
	case streamEAC3:
		return "ec-3", kindAudio
	case streamPrivate:
		return privateCodec(descriptors)
	}
	return "", kindOther
}

// privateCodec names a private data stream from its descriptors, which is how
// DVB carries Dolby audio: a stream type of 0x06 and a descriptor that says
// what is in it. One with no descriptors at all is what an M2TS muxer writes
// for AAC, and is taken as audio for the first PES to name: see inspect.
func privateCodec(descriptors []byte) (string, int) {
	if len(descriptors) == 0 {
		return "private", kindAudio
	}
	for len(descriptors) >= 2 {
		tag, length := descriptors[0], int(descriptors[1])
		if length > len(descriptors)-2 {
			break
		}
		body := descriptors[2 : 2+length]
		switch {
		case tag == descAC3:
			return "ac-3", kindAudio
		case tag == descEAC3:
			return "ec-3", kindAudio
		case tag == descRegistration && len(body) >= 4 && string(body[:4]) == "AC-3":
			return "ac-3", kindAudio
		case tag == descRegistration && len(body) >= 4 && string(body[:4]) == "EAC3":
			return "ec-3", kindAudio
		}
		descriptors = descriptors[2+length:]
	}
	return "", kindOther
}

// section gathers one PSI section out of the packets of its PID. A section may
// run across several packets, and a packet that starts one says where in its
// payload it begins.
type section struct {
	buf  []byte
	busy bool
}

// add feeds a packet's payload in and returns a whole section once one is
// complete. The section it returns aliases the gatherer and is valid until the
// next call.
func (s *section) add(p packet) ([]byte, bool) {
	payload := p.payload
	if p.start {
		if len(payload) < 1 || int(payload[0]) >= len(payload) {
			s.busy = false
			return nil, false
		}
		payload = payload[1+int(payload[0]):]
		s.buf, s.busy = s.buf[:0], true
	}
	if !s.busy {
		return nil, false
	}
	s.buf = append(s.buf, payload[:min(len(payload), maxSectionBytes-len(s.buf))]...)
	if len(s.buf) < 3 {
		return nil, false
	}
	total := 3 + int(binary.BigEndian.Uint16(s.buf[1:3])&0x0fff)
	if len(s.buf) < total {
		if len(s.buf) >= maxSectionBytes {
			s.busy = false
		}
		return nil, false
	}
	s.busy = false
	return s.buf[:total], true
}

// checkSection verifies a long-form section's header and CRC and returns the
// body between the fixed header and the CRC. ISO/IEC 13818-1 2.4.4.
func checkSection(sec []byte, table byte) ([]byte, error) {
	const (
		fixed = 8 // table_id through last_section_number
		crc   = 4
	)
	if len(sec) < fixed+crc || sec[0] != table {
		return nil, fmt.Errorf("%w: a %d byte section where table %#x should be", ErrMalformed, len(sec), table)
	}
	if crc32MPEG(sec) != 0 {
		return nil, fmt.Errorf("%w: table %#x fails its CRC", ErrMalformed, table)
	}
	return sec[fixed : len(sec)-crc], nil
}

// parsePAT returns the PMT PID of the first program the table lists. Program
// number zero names the network information table rather than a program and is
// skipped. ISO/IEC 13818-1 2.4.4.3.
func parsePAT(sec []byte) (uint16, error) {
	body, err := checkSection(sec, tablePAT)
	if err != nil {
		return 0, err
	}
	for ; len(body) >= 4; body = body[4:] {
		if binary.BigEndian.Uint16(body) != 0 {
			return binary.BigEndian.Uint16(body[2:]) & 0x1fff, nil
		}
	}
	return 0, fmt.Errorf("%w: the program association table lists no program", ErrMalformed)
}

// esInfo is one elementary stream a PMT declares.
type esInfo struct {
	pid        uint16
	streamType byte
	codec      string
	kind       int
}

// parsePMT returns the elementary streams a program map table declares, in
// the order it declares them. ISO/IEC 13818-1 2.4.4.8.
func parsePMT(sec []byte) ([]esInfo, error) {
	body, err := checkSection(sec, tablePMT)
	if err != nil {
		return nil, err
	}
	if len(body) < 4 {
		return nil, fmt.Errorf("%w: a %d byte program map table", ErrMalformed, len(body))
	}
	infoLength := int(binary.BigEndian.Uint16(body[2:]) & 0x0fff)
	if infoLength > len(body)-4 {
		return nil, fmt.Errorf("%w: program info of %d bytes in a %d byte table", ErrMalformed, infoLength, len(body))
	}
	body = body[4+infoLength:]

	var streams []esInfo
	for len(body) >= 5 {
		length := int(binary.BigEndian.Uint16(body[3:]) & 0x0fff)
		if length > len(body)-5 {
			return nil, fmt.Errorf("%w: stream info of %d bytes with %d left", ErrMalformed, length, len(body)-5)
		}
		es := esInfo{streamType: body[0], pid: binary.BigEndian.Uint16(body[1:]) & 0x1fff}
		es.codec, es.kind = codecOf(es.streamType, body[5:5+length])
		streams = append(streams, es)
		body = body[5+length:]
	}
	return streams, nil
}

// crc32MPEG is the CRC every PSI section ends with: polynomial 0x04C11DB7,
// initial value all ones, no reflection and no final inversion. Run over a
// whole section including its CRC it comes out zero. ISO/IEC 13818-1 Annex A.
func crc32MPEG(b []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, v := range b {
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
