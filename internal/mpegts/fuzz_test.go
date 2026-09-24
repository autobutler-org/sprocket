package mpegts

import (
	"io"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// tsCorpus are the corpus files the fuzz targets are seeded from.
var tsCorpus = []string{"h264-aac.ts", "hevc-aac.ts", "h264-gop12.ts", "h264-aac.m2ts"}

// FuzzPackets drives the whole demuxer over arbitrary bytes: the packet size
// detection, the packet and adaptation field parsing, the table gathering and
// CRC, the PMT's stream loop, the front and tail scans, the keyframe search,
// and the remux walk with its Payload functions, which walk the packets a
// second and third time.
func FuzzPackets(f *testing.F) {
	for _, name := range tsCorpus {
		raw := readCorpus(f, name)
		f.Add(raw)
		// The first dozen packets alone: the tables and a truncated access
		// unit, which is where most of the parsing happens.
		f.Add(raw[:min(len(raw), 12*packetSize)])
	}
	f.Add([]byte{syncByte})

	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(bytesReader(data), int64(len(data)))
		if err != nil {
			return
		}
		if file.Duration() < 0 {
			t.Fatalf("a negative duration %v", file.Duration())
		}
		_ = file.FrameRate()
		for _, at := range []time.Duration{0, time.Second, 1 << 62} {
			_, _ = file.ReadSyncSample(at)
			_, _ = file.ReadNearestSyncSample(at)
		}
		_ = WriteFragmented(io.Discard, file, isobmff.TargetMP4)
	})
}

// FuzzPES drives the parsers that read inside a packet's payload: the PES
// header and its timestamps, the Annex B splitter and both passes of the
// conversion to length-prefixed NAL units, the sequence parameter set readers,
// the configuration record readers, and the ADTS header.
func FuzzPES(f *testing.F) {
	for _, name := range tsCorpus[:3] {
		raw := readCorpus(f, name)
		f.Add(firstPES(raw, 0x100))
		f.Add(firstPES(raw, 0x101))
	}
	f.Add(append([]byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10}, make([]byte, 10)...))
	f.Add([]byte{0, 0, 1, 0x67, 0x64, 0, 0x0a, 0xac, 0, 0, 3, 0, 0, 1, 0x68})

	f.Fuzz(func(t *testing.T, data []byte) {
		if h, err := parsePESHeader(data); err == nil {
			if h.size > len(data) || (h.hasPTS && (h.pts < 0 || h.pts >= timestampWrap)) {
				t.Fatalf("header %+v out of its %d bytes", h, len(data))
			}
			data = data[h.size:]
		}
		for _, hevc := range []bool{false, true} {
			out, err := toLengthPrefixed(data, hevc, [][]byte{data[:min(len(data), 8)]})
			if err == nil && len(out) > 2*len(data)+8 {
				t.Fatalf("%d bytes of Annex B came out as %d", len(data), len(out))
			}
			_, _ = recordParamSets(data, hevc)
		}
		_, _ = parseH264SPS(data)
		_, _ = parseHEVCSPS(data)
		_, _ = parseADTS(data)
		_, _ = adtsFromConfig(data)
		_ = mpegAudioLayer(data)

		s := &Stream{Codec: "hevc", hevc: true}
		_ = s.configureVideo(data)
		s = &Stream{Codec: "h264"}
		_ = s.configureVideo(data)
	})
}
