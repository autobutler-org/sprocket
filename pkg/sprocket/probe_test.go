package sprocket_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

func TestProbeMatchesCorpusGoldens(t *testing.T) {
	names, err := corpus.Files()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("corpus is empty")
	}

	// The acceptance list names three cases by hand, so assert the corpus still
	// holds them rather than trusting that the table covered them.
	for _, want := range []string{"no-audio.mp4", "h264-aac-faststart.mp4", "fragmented.mp4"} {
		if !slices.Contains(names, want) {
			t.Errorf("corpus is missing %s, which this test has to exercise", want)
		}
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			file, golden, err := corpus.Open(name)
			if err != nil {
				t.Fatalf("open corpus file: %v", err)
			}
			defer file.Close()
			stat, err := file.Stat()
			if err != nil {
				t.Fatalf("stat corpus file: %v", err)
			}

			info, err := sprocket.Probe(file, stat.Size())
			if err != nil {
				t.Fatalf("probe: %v", err)
			}

			const durationTolerance = time.Millisecond
			wantDuration := time.Duration(golden.Duration * float64(time.Second))
			if diff := info.Duration - wantDuration; diff > durationTolerance || diff < -durationTolerance {
				t.Errorf("duration = %v, want %v", info.Duration, wantDuration)
			}
			if info.Width != golden.Width || info.Height != golden.Height {
				t.Errorf("dimensions = %dx%d, want %dx%d", info.Width, info.Height, golden.Width, golden.Height)
			}
			if info.VideoCodec != golden.VideoCodec {
				t.Errorf("video codec = %q, want %q", info.VideoCodec, golden.VideoCodec)
			}
			if info.AudioCodec != golden.AudioCodec {
				t.Errorf("audio codec = %q, want %q", info.AudioCodec, golden.AudioCodec)
			}
			if info.Bitrate != golden.Bitrate {
				t.Errorf("bitrate = %d, want %d", info.Bitrate, golden.Bitrate)
			}
			const framerateTolerance = 0.01
			if math.Abs(info.FrameRate-golden.Framerate) > framerateTolerance {
				t.Errorf("framerate = %v, want %v", info.FrameRate, golden.Framerate)
			}
			if info.Rotation != golden.Rotation {
				t.Errorf("rotation = %d, want %d", info.Rotation, golden.Rotation)
			}
		})
	}
}

func TestProbeRejectsNonMedia(t *testing.T) {
	cases := map[string][]byte{
		"text":  []byte("hello world, definitely not a movie"),
		"empty": {},
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := sprocket.Probe(bytes.NewReader(content), int64(len(content)))
			if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
				t.Errorf("error = %v, want ErrUnsupportedContainer", err)
			}
		})
	}
}

func TestProbeRejectsTruncatedFile(t *testing.T) {
	// h264-aac.mp4 carries its moov at the end, so cutting the file removes the
	// headers Probe needs and leaves an mdat that runs past the end.
	const keep = 0.6
	whole := readCorpus(t, "h264-aac.mp4")
	cut := whole[:int(float64(len(whole))*keep)]

	_, err := sprocket.Probe(bytes.NewReader(cut), int64(len(cut)))
	if !errors.Is(err, sprocket.ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
}

func TestProbeRejectsPayloadTruncatedMidBox(t *testing.T) {
	// The faststart file carries its moov before the mdat, so a cut inside the
	// payload leaves everything Probe reads intact. The cut still shows up in the
	// box walk, because the mdat header declares more bytes than are left.
	whole := readCorpus(t, "h264-aac-faststart.mp4")
	cut := whole[:len(whole)-1024]

	_, err := sprocket.Probe(bytes.NewReader(cut), int64(len(cut)))
	if !errors.Is(err, sprocket.ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
}

func TestProbeAcceptsPayloadCutOnABoxBoundary(t *testing.T) {
	// Cutting the faststart file at the start of its mdat drops the payload
	// without contradicting any header. Probe reads no samples, so it cannot
	// tell, and reports the headers it was given. That is the limitation the
	// Probe doc comment names.
	whole := readCorpus(t, "h264-aac-faststart.mp4")
	cut := whole[:topLevelOffset(t, whole, "mdat")]

	info, err := sprocket.Probe(bytes.NewReader(cut), int64(len(cut)))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.VideoCodec != "h264" || info.Width != 128 {
		t.Errorf("info = %+v, want the intact header values", info)
	}
}

// countingReaderAt serves a prefix of real bytes and zeros past it, reporting a
// size far larger than the bytes it holds, and counts what was read.
type countingReaderAt struct {
	prefix []byte
	size   int64
	read   atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= c.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), c.size-off)
	for i := range p[:n] {
		if at := off + int64(i); at < int64(len(c.prefix)) {
			p[i] = c.prefix[at]
		} else {
			p[i] = 0
		}
	}
	c.read.Add(n)
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func TestProbeReadsOnlyHeadersOfAHugeFile(t *testing.T) {
	const (
		size       = int64(4) << 30
		readBudget = int64(64) << 10
	)

	whole := readCorpus(t, "h264-aac-faststart.mp4")
	mdat := topLevelOffset(t, whole, "mdat")

	// Replace the mdat's compact header with a 64-bit largesize one covering the
	// rest of a 4 GiB file. The walk skips the box by arithmetic, so the zeros
	// behind it are never touched.
	prefix := make([]byte, 0, mdat+16)
	prefix = append(prefix, whole[:mdat]...)
	prefix = binary.BigEndian.AppendUint32(prefix, 1)
	prefix = append(prefix, "mdat"...)
	prefix = binary.BigEndian.AppendUint64(prefix, uint64(size-mdat))

	reader := &countingReaderAt{prefix: prefix, size: size}
	info, err := sprocket.Probe(reader, size)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.VideoCodec != "h264" {
		t.Errorf("video codec = %q, want %q", info.VideoCodec, "h264")
	}
	if got := reader.read.Load(); got > readBudget {
		t.Errorf("read %d bytes of a %d byte file, want at most %d", got, size, readBudget)
	} else {
		t.Logf("read %d bytes of a %d byte file", got, size)
	}
}

// readCorpus loads a whole corpus file, which is small enough for a test to hold.
func readCorpus(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	return content
}

// topLevelOffset returns the offset of the first top-level box of the given type.
func topLevelOffset(t *testing.T, content []byte, typ string) int64 {
	t.Helper()
	for off := int64(0); off+8 <= int64(len(content)); {
		size := int64(binary.BigEndian.Uint32(content[off:]))
		if string(content[off+4:off+8]) == typ {
			return off
		}
		if size < 8 {
			break
		}
		off += size
	}
	t.Fatalf("no top-level %q box", typ)
	return 0
}
