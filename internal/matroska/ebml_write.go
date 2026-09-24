package matroska

import (
	"encoding/binary"
	"math"
)

// EBML encoding limits, which the header declares and the writer stays inside.
const (
	maxElementIDBytes   = 4
	maxElementSizeBytes = 8
)

// unknownSizeByte is the one-byte encoding of an unknown size, which is what
// the segment declares: a marker bit and seven value bits, all set.
const unknownSizeByte = 0xff

// elementID encodes an element ID, which is stored with its length marker
// already part of the value, so the encoding is the ID's own bytes.
func elementID(id uint32) []byte {
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

// sizeVint encodes a size in as few bytes as it fits in. The marker bit sits at
// the top of the first byte and is not part of the value, and a value whose
// bits are all set would mean "unknown", so each width holds one less than its
// bits allow.
func sizeVint(size uint64) []byte {
	for n := 1; n <= maxElementSizeBytes; n++ {
		if size >= 1<<(7*n)-1 {
			continue
		}
		out := make([]byte, n)
		for i := n - 1; i >= 0; i-- {
			out[i] = byte(size)
			size >>= 8
		}
		out[0] |= 0x80 >> (n - 1)
		return out
	}
	// Unreachable for any size a file can hold: eight bytes cover 2^56 - 2.
	return []byte{unknownSizeByte}
}

// elementHeader builds an element's ID and size, which is everything before its
// payload.
func elementHeader(id uint32, size int64) []byte {
	return append(elementID(id), sizeVint(uint64(size))...)
}

// integerElement builds an element holding an EBML unsigned integer, which is
// stored big-endian in as few bytes as it needs. Zero is stored as no bytes at
// all, which is the encoding the format defines for it.
func integerElement(id uint32, value uint64) []byte {
	var raw [8]byte
	binary.BigEndian.PutUint64(raw[:], value)
	body := raw[:]
	for len(body) > 0 && body[0] == 0 {
		body = body[1:]
	}
	return append(elementHeader(id, int64(len(body))), body...)
}

// docWriter builds an element tree in memory. An element is opened with a
// fixed-width placeholder size, its children are appended, and closing it
// patches the size in, so a tree is written in one pass with no counting ahead.
// The placeholder is always eight bytes, which the format allows and which is
// what makes the patch a write in place.
type docWriter struct{ buf []byte }

// open starts an element and returns the position of its size field, which is
// what close patches.
func (d *docWriter) open(id uint32) int {
	d.buf = append(d.buf, elementID(id)...)
	at := len(d.buf)
	d.buf = append(d.buf, make([]byte, maxElementSizeBytes)...)
	return at
}

// close patches an element's size now that its payload is written.
func (d *docWriter) close(at int) {
	binary.BigEndian.PutUint64(d.buf[at:], uint64(len(d.buf)-at-maxElementSizeBytes))
	// The marker bit of an eight byte size is the low bit of its first byte.
	d.buf[at] |= 0x01
}

func (d *docWriter) integer(id uint32, value uint64) {
	d.buf = append(d.buf, integerElement(id, value)...)
}

func (d *docWriter) float(id uint32, value float64) {
	d.buf = append(d.buf, elementHeader(id, 8)...)
	d.buf = binary.BigEndian.AppendUint64(d.buf, math.Float64bits(value))
}

func (d *docWriter) text(id uint32, value string) {
	d.buf = append(d.buf, elementHeader(id, int64(len(value)))...)
	d.buf = append(d.buf, value...)
}

func (d *docWriter) binary(id uint32, value []byte) {
	d.buf = append(d.buf, elementHeader(id, int64(len(value)))...)
	d.buf = append(d.buf, value...)
}
