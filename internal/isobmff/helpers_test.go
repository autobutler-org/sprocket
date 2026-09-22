package isobmff

import (
	"bytes"
	"encoding/binary"
	"io"
)

// This file builds handcrafted boxes for the malformed-input and fuzz-seed
// tests. Everything here is deliberately literal: a helper that shared code
// with the parser could hide the same mistake twice.

func readerAt(data []byte) io.ReaderAt { return bytes.NewReader(data) }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// box wraps a payload in a compact box header.
func box(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

// nest wraps payload in depth copies of the same box type.
func nest(typ string, depth int, payload []byte) []byte {
	out := payload
	for range depth {
		out = box(typ, out)
	}
	return out
}

func put32(v uint32) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return out[:]
}

// mvhd builds a version 0 movie header.
func mvhd(timescale, duration uint32) []byte {
	out := concat(
		[]byte{0, 0, 0, 0}, // version and flags
		put32(0), put32(0), // creation and modification time
		put32(timescale), put32(duration),
		put32(0x00010000), // rate
		[]byte{1, 0},      // volume
		make([]byte, 10),  // reserved
	)
	for _, v := range matrixIdentity {
		out = append(out, put32(uint32(v))...)
	}
	return append(out, make([]byte, 24+4)...) // pre_defined and next_track_ID
}

// tkhd builds a version 0 track header.
func tkhd(id uint32, matrix [9]int32, width, height uint32) []byte {
	out := concat(
		[]byte{0, 0, 0, 3}, // version, and the enabled and in-movie flags
		put32(0), put32(0), // creation and modification time
		put32(id), put32(0), put32(0), // track ID, reserved, duration
		make([]byte, 8), // reserved
		[]byte{0, 0},    // layer
		[]byte{0, 0},    // alternate group
		[]byte{0, 0},    // volume
		[]byte{0, 0},    // reserved
	)
	for _, v := range matrix {
		out = append(out, put32(uint32(v))...)
	}
	return concat(out, put32(width<<16), put32(height<<16))
}

// mdhd builds a version 0 media header with an undetermined language.
func mdhd(timescale, duration uint32) []byte {
	return concat(
		[]byte{0, 0, 0, 0},
		put32(0), put32(0),
		put32(timescale), put32(duration),
		[]byte{0x55, 0xc4}, // "und"
		[]byte{0, 0},       // pre_defined
	)
}

// hdlr builds a handler reference of the given type.
func hdlr(handler string) []byte {
	return concat([]byte{0, 0, 0, 0}, put32(0), []byte(handler), make([]byte, 12), []byte{0})
}
