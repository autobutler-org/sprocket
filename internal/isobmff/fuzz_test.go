package isobmff

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
)

// seedCorpus adds every corpus media file to a fuzz target. The files are read
// whole here on purpose: this is the test harness, not the parser.
func seedCorpus(f *testing.F) {
	f.Helper()

	names, err := corpus.Files()
	if err != nil {
		f.Fatalf("list corpus: %v", err)
	}
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
		if err != nil {
			f.Fatalf("read %s: %v", name, err)
		}
		f.Add(raw)
	}
}

// FuzzBoxWalker drives both walkers over arbitrary bytes: the flat top-level
// scan over an io.ReaderAt, and the recursive in-memory walk, which is pushed
// past its depth limit by a callback that descends into every box.
func FuzzBoxWalker(f *testing.F) {
	seedCorpus(f)
	f.Add([]byte("\x00\x00\x00\x08free"))
	f.Add([]byte("\x00\x00\x00\x01uuid\x00\x00\x00\x00\x00\x00\x00\x18"))
	f.Add([]byte("\x00\x00\x00\x00mdat"))
	f.Add(nest("moov", 40, nil))

	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = scanTop(bytes.NewReader(data), int64(len(data)), func(header, int64) error { return nil })

		var descend func(payload []byte, depth int)
		descend = func(payload []byte, depth int) {
			_ = walk(payload, depth, func(_ string, body []byte) error {
				descend(body, depth+1)
				return nil
			})
		}
		descend(data, 0)
	})
}

// FuzzSampleTables drives the sample table parser and every lookup it answers,
// treating the input as an stbl payload.
func FuzzSampleTables(f *testing.F) {
	seedCorpus(f)
	f.Add(concat(
		box("stts", concat([]byte{0, 0, 0, 0}, put32(1), put32(3), put32(512))),
		box("stsc", concat([]byte{0, 0, 0, 0}, put32(1), put32(1), put32(1), put32(1))),
		box("stsz", concat([]byte{0, 0, 0, 0}, put32(0), put32(3), put32(10), put32(20), put32(30))),
		box("stco", concat([]byte{0, 0, 0, 0}, put32(3), put32(100), put32(200), put32(300))),
		box("stss", concat([]byte{0, 0, 0, 0}, put32(1), put32(1))),
	))
	f.Add(box("stz2", concat([]byte{0, 0, 0, 0}, []byte{0, 0, 0, 4}, put32(4), []byte{0x12, 0x34})))
	f.Add(box("stsz", concat([]byte{0, 0, 0, 0}, put32(0), put32(1<<30))))

	f.Fuzz(func(_ *testing.T, data []byte) {
		track := &Track{Handler: "vide"}
		if err := track.parseStbl(data, 0); err != nil {
			return
		}
		s := &track.tables
		for _, at := range []uint64{0, 1, 1 << 20, 1 << 62} {
			index := s.sampleAtTime(at)
			_ = s.sampleTime(index)
			_ = s.compositionOffset(index)
			if sync, ok := s.syncAtOrBefore(index); ok {
				_, _ = s.sampleRange(sync)
			}
		}
		_ = s.totalBytes()
		_ = s.duration()
	})
}

// FuzzParse drives the whole entry point, which is what a caller hands
// untrusted bytes to.
func FuzzParse(f *testing.F) {
	seedCorpus(f)
	f.Add(concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), box("moov", nil)))

	f.Fuzz(func(_ *testing.T, data []byte) {
		file, err := Parse(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		_ = file.Duration()
		for _, track := range file.Tracks {
			_ = track.Duration()
			_ = track.FrameRate()
			_ = track.SampleBytes()
			_ = track.EmptyEditDuration()
		}
		_, _ = file.ReadSyncSample(0)
		_, _ = file.ReadSyncSample(time.Hour)
	})
}
