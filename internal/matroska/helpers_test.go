package matroska

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"sync/atomic"
)

// This file builds handcrafted EBML documents for the malformed-input and fuzz
// seed tests. Everything here is deliberately literal: a helper that shared
// code with the parser could hide the same mistake twice.

func readerAt(data []byte) io.ReaderAt { return bytes.NewReader(data) }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// elemID encodes an element ID, which is stored with its length marker already
// in the value, so the bytes are simply the ID's own significant bytes.
func elemID(id uint32) []byte {
	switch {
	case id <= 0xff:
		return []byte{byte(id)}
	case id <= 0xffff:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xffffff:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// elemSize encodes a size as an eight-byte vint, which is always legal and
// keeps the encoding out of the way of what a test is actually checking.
func elemSize(size uint64) []byte {
	out := binary.BigEndian.AppendUint64(nil, size)
	out[0] |= 0x01
	return out
}

// elem builds one element from an ID and a payload.
func elem(id uint32, payload []byte) []byte {
	return concat(elemID(id), elemSize(uint64(len(payload))), payload)
}

// unknownElem builds an element that declares it does not know its own size.
func unknownElem(id uint32, payload []byte) []byte {
	return concat(elemID(id), []byte{0xff}, payload)
}

// nestElem wraps payload in depth copies of the same element.
func nestElem(id uint32, depth int, payload []byte) []byte {
	out := payload
	for range depth {
		out = elem(id, out)
	}
	return out
}

// uintElem builds an element holding an EBML unsigned integer.
func uintElem(id uint32, value uint64) []byte {
	return elem(id, binary.BigEndian.AppendUint64(nil, value))
}

// floatElem builds an element holding an eight-byte EBML float.
func floatElem(id uint32, value float64) []byte {
	return elem(id, binary.BigEndian.AppendUint64(nil, math.Float64bits(value)))
}

// ebmlHead builds the EBML header of a document of the given type.
func ebmlHead(docType string) []byte {
	return elem(idEBML, elem(idDocType, []byte(docType)))
}

// simpleBlock builds a SimpleBlock with no lacing, for a single-byte track
// number.
func simpleBlock(track byte, timestamp int16, keyframe bool, frame []byte) []byte {
	var flags byte
	if keyframe {
		flags = blockKeyframeFlag
	}
	head := []byte{track | 0x80, byte(uint16(timestamp) >> 8), byte(timestamp), flags}
	return elem(idSimpleBlock, concat(head, frame))
}

// videoTrack builds a TrackEntry for a video track.
func videoTrack(number byte, codecID string, width, height uint64, defaultDuration uint64) []byte {
	entry := concat(
		uintElem(idTrackNumber, uint64(number)),
		uintElem(idTrackType, trackVideo),
		elem(idCodecID, []byte(codecID)),
		elem(idVideo, concat(uintElem(idPixelWidth, width), uintElem(idPixelHeight, height))),
	)
	if defaultDuration > 0 {
		entry = append(entry, uintElem(idDefaultDur, defaultDuration)...)
	}
	return elem(idTrackEntry, entry)
}

// segment builds a whole document: an EBML header and a segment holding the
// given children.
func segment(docType string, children ...[]byte) []byte {
	return concat(ebmlHead(docType), elem(idSegment, concat(children...)))
}

// info builds a segment information element.
func info(scale uint64, duration float64) []byte {
	return elem(idInfo, concat(uintElem(idTimestampScal, scale), floatElem(idDuration, duration)))
}

// cluster builds a cluster at a timestamp holding the given blocks.
func cluster(timestamp uint64, blocks ...[]byte) []byte {
	return elem(idCluster, concat(uintElem(idTimestamp, timestamp), concat(blocks...)))
}

// zeroBacked serves a prefix of real bytes and zeros past it, reporting a size
// far larger than the bytes it holds, and counts what was read. It stands in
// for a huge file without writing one.
type zeroBacked struct {
	prefix []byte
	size   int64
	read   atomic.Int64
}

func (z *zeroBacked) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= z.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), z.size-off)
	for i := range p[:n] {
		if at := off + int64(i); at < int64(len(z.prefix)) {
			p[i] = z.prefix[at]
		} else {
			p[i] = 0
		}
	}
	z.read.Add(n)
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}
