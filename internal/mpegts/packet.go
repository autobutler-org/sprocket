package mpegts

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Packet layout. ISO/IEC 13818-1 2.4.3.
const (
	packetSize = 188
	syncByte   = 0x47
	// m2tsPrefix is the four byte arrival timestamp a Blu-ray M2TS file puts in
	// front of every packet, which makes its stride 192 bytes.
	m2tsPrefix = 4
	// headerSize is the fixed packet header: the sync byte, the flags and PID,
	// and the scrambling, adaptation, and continuity bits.
	headerSize = 4
	// payloadSize is what a packet with no adaptation field carries.
	payloadSize = packetSize - headerSize
	// patPID is where the program association table lives.
	patPID = 0
)

// chunkPackets is how many packets one read pulls in. It is a few tens of
// kilobytes whatever the stride, which keeps a sequential walk to a handful of
// reads per megabyte without holding anything worth measuring.
const chunkPackets = 128

// maxResyncBytes bounds how far the reader looks for the next sync byte after
// losing it. A file that has lost sync for longer than this is not one this
// package reads.
const maxResyncBytes = 64 << 10

// packet is one transport packet with its header decoded. payload aliases the
// reader's buffer and is valid until the next call to next.
type packet struct {
	// offset is where the 188 byte packet begins, after any M2TS prefix.
	offset int64
	pid    uint16
	// start is payload_unit_start_indicator: a PES packet or a section begins
	// in this packet.
	start bool
	cc    uint8
	// randomAccess is the adaptation field's random_access_indicator, which a
	// muxer sets on a packet that begins a keyframe.
	randomAccess bool
	// discontinuity is the adaptation field's discontinuity_indicator: the
	// continuity counter, and possibly the clock, restart here on purpose.
	discontinuity bool
	hasPayload    bool
	payload       []byte
}

// parsePacket decodes one 188 byte packet. It checks the structure a packet
// has to have and nothing about what it carries.
func parsePacket(b []byte, offset int64) (packet, error) {
	if len(b) < packetSize || b[0] != syncByte {
		return packet{}, fmt.Errorf("%w: no sync byte at %d", ErrMalformed, offset)
	}
	p := packet{
		offset: offset,
		pid:    binary.BigEndian.Uint16(b[1:3]) & 0x1fff,
		start:  b[1]&0x40 != 0,
		cc:     b[3] & 0x0f,
	}
	control := b[3] >> 4 & 0x03
	rest := b[headerSize:packetSize]
	if control&0x02 != 0 {
		length := int(rest[0])
		if length > len(rest)-1 {
			return packet{}, fmt.Errorf("%w: the packet at %d declares a %d byte adaptation field",
				ErrMalformed, offset, length)
		}
		if length > 0 {
			flags := rest[1]
			p.discontinuity = flags&0x80 != 0
			p.randomAccess = flags&0x40 != 0
		}
		rest = rest[1+length:]
	}
	if control&0x01 != 0 {
		p.hasPayload, p.payload = true, rest
	}
	return p, nil
}

// reader walks the packets of a byte range in order, a chunk at a time. It
// never reads outside the range it was given, which is what keeps the scans
// that use it inside their caps.
type reader struct {
	r      io.ReaderAt
	stride int64 // 188, or 192 for M2TS
	prefix int64 // 0, or 4 for M2TS

	pos, end int64 // the next packet slot and the end of the range
	buf      []byte
	bufAt    int64 // the file offset buf[0] holds
	bufLen   int
	read     int64 // bytes read so far, for the scans that are capped
}

func newReader(r io.ReaderAt, stride int64) *reader {
	return &reader{r: r, stride: stride, prefix: stride - packetSize, buf: make([]byte, chunkPackets*stride)}
}

// seek points the reader at a range, whose first packet slot is at from. A
// caller aiming at an estimate rounds it to a slot first; one that is not on a
// slot costs a resync.
func (r *reader) seek(from, end int64) {
	r.pos, r.end, r.bufLen = max(from, 0), end, 0
}

// slotOf is where the packet that p came from begins in the file, which is
// where seek has to be pointed to read it again.
func (r *reader) slotOf(p packet) int64 { return p.offset - r.prefix }

// next returns the next packet in the range and false once the range is spent.
// A slot with no sync byte in it is stepped over by searching forward for the
// next one, up to maxResyncBytes; a file that has lost sync for longer than
// that is refused.
func (r *reader) next() (packet, bool, error) {
	searched := int64(0)
	for {
		if r.pos+r.stride > r.end {
			return packet{}, false, nil
		}
		b, err := r.slot(r.pos)
		if err != nil {
			return packet{}, false, err
		}
		if b[0] == syncByte {
			p, err := parsePacket(b, r.pos+r.prefix)
			r.pos += r.stride
			return p, err == nil, err
		}
		if searched >= maxResyncBytes {
			return packet{}, false, fmt.Errorf("%w: lost sync at %d and found none in %d bytes",
				ErrMalformed, r.pos, int64(maxResyncBytes))
		}
		r.pos++
		searched++
	}
}

// slot returns the stride bytes at off, which lie inside the range, and the
// packet in them starts prefix bytes in.
func (r *reader) slot(off int64) ([]byte, error) {
	if off < r.bufAt || off+r.stride > r.bufAt+int64(r.bufLen) {
		n := min(int64(len(r.buf)), r.end-off)
		got, err := r.r.ReadAt(r.buf[:n], off)
		r.read += int64(got)
		if int64(got) < n {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("%w: reading packets at %d: %w", ErrTruncated, off, err)
		}
		r.bufAt, r.bufLen = off, int(n)
	}
	at := off - r.bufAt
	return r.buf[at+r.prefix : at+r.stride], nil
}

// detectStride works out the packet size from the first bytes of the file:
// 188 for plain transport stream and 192 for M2TS, whose four byte prefix puts
// the sync byte at 4. Three sync bytes in a row at the same spacing is what
// counts, which an unrelated file matches by accident one time in sixteen
// million. 204 byte packets, which carry Reed-Solomon parity, are recognized
// and refused.
func detectStride(r io.ReaderAt, size int64) (int64, error) {
	const (
		rsStride = 204
		probe    = 3*rsStride + 1
	)
	var head [probe]byte
	n := min(size, probe)
	if got, err := r.ReadAt(head[:n], 0); int64(got) < n {
		return 0, fmt.Errorf("%w: reading the first packets: %w", ErrNotTS, err)
	}
	synced := func(first, stride int) bool {
		for i := range 3 {
			at := first + i*stride
			if at >= int(n) || head[at] != syncByte {
				return false
			}
		}
		return true
	}
	switch {
	case synced(0, packetSize):
		return packetSize, nil
	case synced(m2tsPrefix, packetSize+m2tsPrefix):
		return packetSize + m2tsPrefix, nil
	case synced(0, rsStride):
		return 0, fmt.Errorf("%w: 204 byte packets, which carry Reed-Solomon parity, are not read here", ErrNotTS)
	}
	return 0, fmt.Errorf("%w: no run of three sync bytes at a 188 or 192 byte spacing", ErrNotTS)
}
