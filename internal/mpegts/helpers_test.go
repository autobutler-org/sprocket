package mpegts

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/autobutler-org/sprocket/internal/corpus"
)

// This file holds the test harness's own helpers. Anything here that parses a
// stream does it the plain way on purpose, so it cannot share a mistake with
// the package.

func bytesReader(b []byte) io.ReaderAt { return bytes.NewReader(b) }

// readCorpus loads a whole corpus file, which is small enough to hold.
func readCorpus(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return raw
}

// parseCorpus parses a corpus file held in memory.
func parseCorpus(t testing.TB, name string) (*File, []byte) {
	t.Helper()
	raw := readCorpus(t, name)
	f, err := Parse(bytesReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return f, raw
}

// packetPID reads the PID of a 188 byte packet.
func packetPID(pkt []byte) uint16 { return uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2]) }

// eachPacket calls fn with every 188 byte packet of a plain transport stream.
func eachPacket(ts []byte, fn func(pkt []byte)) {
	for off := 0; off+packetSize <= len(ts); off += packetSize {
		fn(ts[off : off+packetSize])
	}
}

// payloadOf returns the payload of a packet, after any adaptation field.
func payloadOf(pkt []byte) []byte {
	at := 4
	if pkt[3]&0x20 != 0 {
		at += 1 + int(pkt[4])
	}
	if pkt[3]&0x10 == 0 || at >= len(pkt) {
		return nil
	}
	return pkt[at:]
}

// shiftTimestamps adds delta to every PTS and DTS in a plain transport stream,
// modulo the 33 bit wrap, rewriting the PES headers in place.
func shiftTimestamps(ts []byte, delta int64) {
	eachPacket(ts, func(pkt []byte) {
		if pkt[1]&0x40 == 0 {
			return
		}
		pes := payloadOf(pkt)
		if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || pes[3] < 0xbd {
			return
		}
		flags := pes[7] >> 6
		fields := pes[9:]
		rewrite := func(at int, prefix byte) {
			v := readTimestamp(fields[at:])
			copy(fields[at:], timestampField(v+delta, prefix))
		}
		switch flags {
		case 2:
			rewrite(0, 0x02)
		case 3:
			rewrite(0, 0x03)
			rewrite(5, 0x01)
		}
	})
}

// firstPES gathers the first PES of a PID, header included, out of a plain
// transport stream, for the fuzz seeds.
func firstPES(ts []byte, pid uint16) []byte {
	var out []byte
	started := false
	eachPacket(ts, func(pkt []byte) {
		if packetPID(pkt) != pid {
			return
		}
		if pkt[1]&0x40 != 0 {
			if started {
				started = false
				return
			}
			if out == nil {
				started = true
			}
		}
		if started {
			out = append(out, payloadOf(pkt)...)
		}
	})
	return out
}
