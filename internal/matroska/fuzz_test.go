package matroska

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// seedCorpus adds every Matroska corpus file to a fuzz target. The files are
// read whole here on purpose: this is the test harness, not the parser.
func seedCorpus(f *testing.F) {
	f.Helper()
	for _, name := range matroskaFiles(f) {
		raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
		if err != nil {
			f.Fatalf("read %s: %v", name, err)
		}
		f.Add(raw)
	}
}

// FuzzEBML drives both walkers over arbitrary bytes: the flat scan over an
// io.ReaderAt, which is what reads the segment's children and one cluster's
// blocks, and the recursive in-memory walk, which is pushed past its depth
// limit by a callback that descends into every element.
func FuzzEBML(f *testing.F) {
	seedCorpus(f)
	f.Add(ebmlHead("matroska"))
	f.Add(segment("webm", info(1000000, 2000), elem(idTracks, videoTrack(1, "V_VP8", 128, 72, 0))))
	f.Add(unknownElem(idSegment, unknownElem(idCluster, simpleBlock(1, 0, true, []byte("frame")))))
	f.Add(nestElem(idTracks, maxDepth+8, nil))
	f.Add([]byte{0xff})
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})

	// The element limit is the harness's own rather than the parser's: walking
	// a megabyte of two-byte elements one at a time is slow enough to starve
	// the fuzzer of executions, and it exercises nothing the first few thousand
	// do not.
	const limit = 1 << 12

	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = scan(readerAt(data), 0, int64(len(data)), limit, func(element, int64) error { return nil })
		_ = scanResolving(readerAt(data), 0, int64(len(data)), limit,
			func(_ element, at int64) int64 { return at + 1 },
			func(element, int64) error { return nil })

		var descend func(payload []byte, depth int)
		descend = func(payload []byte, depth int) {
			_ = walk(payload, depth, func(_ uint32, body []byte) error {
				descend(body, depth+1)
				return nil
			})
		}
		descend(data, 0)
	})
}

// FuzzParse drives the whole demuxer: the document header, the segment walk,
// the track descriptions, the frame rate measurement, and both keyframe
// lookups, which between them read the seek index, the cluster walk, and the
// block headers.
func FuzzParse(f *testing.F) {
	seedCorpus(f)
	f.Add(segment("matroska",
		info(1000000, 2000),
		elem(idTracks, videoTrack(1, "V_MPEG4/ISO/AVC", 128, 72, 0)),
		cluster(0, simpleBlock(1, 0, true, []byte("frame"))),
		elem(idCues, elem(idCuePoint, concat(
			uintElem(idCueTime, 0),
			elem(idCueTrackPos, concat(uintElem(idCueTrack, 1), uintElem(idCueClusterPos, 0))),
		))),
	))
	f.Add(segment("webm", info(0, -1), elem(idTracks, videoTrack(1, "V_AV1", 1<<40, 1<<40, 0))))

	f.Fuzz(func(t *testing.T, data []byte) {
		file, err := Parse(readerAt(data), int64(len(data)))
		if err != nil {
			return
		}
		if file.TimestampScale == 0 {
			t.Fatal("a parsed file has a zero timestamp scale")
		}
		_, _ = file.Duration(), file.FrameRate()
		for _, at := range []time.Duration{0, time.Second, 1 << 62} {
			_, _ = file.ReadSyncSample(at)
			_, _ = file.ReadNearestSyncSample(at)
		}
		// Both writers read the same untrusted clusters the lookups do, and they
		// walk every block rather than one, so they go through the same door.
		// The fragmented one has a second set of arithmetic to get wrong: the
		// decode timeline it derives from the block timestamps.
		_ = Copy(io.Discard, file, isobmff.TargetMKV, nil)
		_ = WriteFragmented(io.Discard, file, isobmff.TargetMP4, nil)

		cut, _, err := file.TrimSpan(time.Second, 2*time.Second)
		if err != nil {
			return
		}
		_ = Copy(io.Discard, file, isobmff.TargetMKV, &cut)
		_ = WriteFragmented(io.Discard, file, isobmff.TargetMP4, &cut)
	})
}
