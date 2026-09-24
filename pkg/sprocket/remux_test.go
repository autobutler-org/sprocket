package sprocket_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

// remuxed remuxes a corpus file into memory. The corpus is small enough to hold
// twice over; the bounded-memory case is its own test.
func remuxed(t *testing.T, name string, target sprocket.Container) []byte {
	t.Helper()

	source := readCorpus(t, name)
	var out bytes.Buffer
	if err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), &out, target); err != nil {
		t.Fatalf("remux %s: %v", name, err)
	}
	return out.Bytes()
}

// probeBytes probes an in-memory file.
func probeBytes(t *testing.T, content []byte) sprocket.Info {
	t.Helper()

	info, err := sprocket.Probe(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return info
}

func TestRemuxPreservesWhatProbeReports(t *testing.T) {
	for _, name := range []string{
		"hevc-aac-8bit.mov", "hevc-aac-10bit.mov", "h264-aac.mp4",
		"h264-aac-faststart.mp4", "rotate-90.mp4", "rotate-180.mp4", "no-audio.mp4",
	} {
		t.Run(name, func(t *testing.T) {
			want := probeBytes(t, readCorpus(t, name))
			got := probeBytes(t, remuxed(t, name, sprocket.MP4))

			// Bitrate is the only field that may move: it is derived from the
			// file size, and the output drops whatever padding the source
			// carried. Everything else has to survive the trip.
			want.Bitrate, got.Bitrate = 0, 0
			if got != want {
				t.Errorf("probe of the remux = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRemuxPreservesTheThumbnail(t *testing.T) {
	for _, name := range []string{"hevc-aac-8bit.mov", "h264-aac.mp4", "rotate-90.mp4"} {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), 0, sprocket.ThumbnailOptions{})
			if err != nil && !h264Compiled && errors.Is(err, sprocket.ErrUnsupportedCodec) {
				t.Skipf("%s needs the h264 build tag", name)
			}
			if err != nil {
				t.Fatalf("thumbnail the source: %v", err)
			}

			out := remuxed(t, name, sprocket.MP4)
			got, err := sprocket.Thumbnail(bytes.NewReader(out), int64(len(out)), 0, sprocket.ThumbnailOptions{})
			if err != nil {
				t.Fatalf("thumbnail the remux: %v", err)
			}

			if got.Time != want.Time {
				t.Errorf("time = %v, want %v", got.Time, want.Time)
			}
			if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
				t.Error("the remuxed file decoded a different picture")
			}
		})
	}
}

func TestRemuxWritesTheHeaderBeforeThePayload(t *testing.T) {
	out := remuxed(t, "hevc-aac-8bit.mov", sprocket.MP4)

	want := []string{"ftyp", "moov", "mdat"}
	if got := topLevelTypes(t, out); !slices.Equal(got, want) {
		t.Fatalf("box order = %v, want %v", got, want)
	}

	// moov in front is only worth anything if a reader can stop there, so
	// probing the output must not touch the payload.
	mdat := topLevelOffset(t, out, "mdat")
	reader := &countingReaderAt{prefix: out, size: int64(len(out))}
	if _, err := sprocket.Probe(reader, int64(len(out))); err != nil {
		t.Fatalf("probe the remux: %v", err)
	}
	budget := mdat + 1024
	if got := reader.read.Load(); got > budget {
		t.Errorf("probing the remux read %d bytes, want at most %d, the header plus slack", got, budget)
	} else {
		t.Logf("probing the remux read %d bytes of %d, with mdat at %d", got, len(out), mdat)
	}
}

func TestRemuxWritesEveryTargetBrand(t *testing.T) {
	for _, tc := range []struct {
		target sprocket.Container
		brand  string
	}{
		{target: sprocket.MP4, brand: "isom"},
		{target: sprocket.M4V, brand: "M4V "},
		{target: sprocket.ThreeGP, brand: "3gp4"},
	} {
		t.Run(string(tc.target), func(t *testing.T) {
			out := remuxed(t, "hevc-aac-8bit.mov", tc.target)
			if got := string(out[8:12]); got != tc.brand {
				t.Errorf("major brand = %q, want %q", got, tc.brand)
			}
			if _, err := sprocket.Probe(bytes.NewReader(out), int64(len(out))); err != nil {
				t.Errorf("probe the remux: %v", err)
			}
		})
	}
}

func TestRemuxRefusesProResAndPCM(t *testing.T) {
	const name = "prores-pcm.mov"

	source := readCorpus(t, name)
	info := probeBytes(t, source)
	if sprocket.CanRemux(info, sprocket.MP4) {
		t.Errorf("CanRemux(%+v, mp4) = true, want false", info)
	}

	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.MP4)
	if !errors.Is(err, sprocket.ErrIncompatible) {
		t.Fatalf("error = %v, want ErrIncompatible", err)
	}
	// The error has to name what blocked the remux, or a caller cannot tell the
	// user which stream is the problem.
	if !strings.Contains(err.Error(), info.VideoCodec) {
		t.Errorf("error %q does not name the codec %q that blocked it", err, info.VideoCodec)
	}
}

func TestCanRemux(t *testing.T) {
	for _, tc := range []struct {
		video, audio string
		target       sprocket.Container
		want         bool
	}{
		{video: "h264", audio: "aac", target: sprocket.MP4, want: true},
		{video: "hevc", audio: "aac", target: sprocket.MP4, want: true},
		{video: "h264", audio: "aac", target: sprocket.ThreeGP, want: true},
		{video: "h264", audio: "", target: sprocket.MP4, want: true},
		{video: "", audio: "aac", target: sprocket.MP4, want: true},
		{video: "av1", audio: "opus", target: sprocket.MP4, want: true},
		// ProRes and PCM are the MOV-only cases: the video is fine on its own,
		// the audio is fine on its own, and neither fits an MP4.
		{video: "apcn", audio: "aac", target: sprocket.MP4, want: false},
		{video: "h264", audio: "sowt", target: sprocket.MP4, want: false},
		{video: "h264", audio: "lpcm", target: sprocket.MP4, want: false},
		// A container narrower than mp4 refuses what mp4 takes.
		{video: "av1", audio: "aac", target: sprocket.ThreeGP, want: false},
		{video: "h264", audio: "opus", target: sprocket.ThreeGP, want: false},
		{video: "h264", audio: "aac", target: sprocket.Container("avi"), want: false},
		// The Matroska family takes what the MP4 family takes and more; WebM is
		// the strict one, and refuses everything but its own three codecs.
		{video: "h264", audio: "aac", target: sprocket.MKV, want: true},
		{video: "vp8", audio: "vorbis", target: sprocket.MKV, want: true},
		{video: "vp9", audio: "opus", target: sprocket.WebM, want: true},
		{video: "av1", audio: "opus", target: sprocket.WebM, want: true},
		{video: "h264", audio: "opus", target: sprocket.WebM, want: false},
		{video: "vp9", audio: "aac", target: sprocket.WebM, want: false},
		{video: "vp8", audio: "vorbis", target: sprocket.MP4, want: false},
		// A transport stream carries the codecs it has a stream type for.
		{video: "h264", audio: "aac", target: sprocket.TS, want: true},
		{video: "hevc", audio: "ac-3", target: sprocket.TS, want: true},
		{video: "h264", audio: "mp3", target: sprocket.TS, want: true},
		{video: "av1", audio: "aac", target: sprocket.TS, want: false},
		{video: "h264", audio: "opus", target: sprocket.TS, want: false},
		{video: "mpeg2video", audio: "mp2", target: sprocket.MP4, want: false},
	} {
		t.Run(tc.video+"+"+tc.audio+"/"+string(tc.target), func(t *testing.T) {
			info := sprocket.Info{VideoCodec: tc.video, AudioCodec: tc.audio}
			if got := sprocket.CanRemux(info, tc.target); got != tc.want {
				t.Errorf("CanRemux = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanRemuxAgreesWithRemux(t *testing.T) {
	names, err := corpus.Files()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}

	for _, name := range names {
		for _, target := range []sprocket.Container{sprocket.MP4, sprocket.M4V, sprocket.ThreeGP, sprocket.TS} {
			t.Run(name+"/"+string(target), func(t *testing.T) {
				source := readCorpus(t, name)
				allowed := sprocket.CanRemux(probeBytes(t, source), target)

				err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, target)
				switch {
				case allowed && errors.Is(err, sprocket.ErrIncompatible):
					t.Errorf("CanRemux said yes and Remux returned %v", err)
				case !allowed && err == nil:
					t.Error("CanRemux said no and Remux succeeded")
				}
			})
		}
	}
}

func TestRemuxRefusesAFragmentedSource(t *testing.T) {
	// The samples of a fragmented file live in moof boxes rather than in the
	// moov's tables, and this writer builds its output from the tables.
	source := readCorpus(t, "fragmented.mp4")

	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.MP4)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

func TestRemuxRefusesAnUnknownTarget(t *testing.T) {
	source := readCorpus(t, "h264-aac.mp4")

	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.Container("avi"))
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

func TestRemuxRejectsNonMedia(t *testing.T) {
	content := []byte("hello world, definitely not a movie")

	err := sprocket.Remux(bytes.NewReader(content), int64(len(content)), io.Discard, sprocket.MP4)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

// errWriteFailed is what the failing writer below reports, so the test can
// check the caller's error survives the trip out of Remux.
var errWriteFailed = errors.New("the disk went away")

// shortWriter accepts limit bytes and fails on everything after them.
type shortWriter struct{ left int }

func (s *shortWriter) Write(p []byte) (int, error) {
	if len(p) > s.left {
		taken := s.left
		s.left = 0
		return taken, errWriteFailed
	}
	s.left -= len(p)
	return len(p), nil
}

func TestRemuxReportsAWriteFailure(t *testing.T) {
	source := readCorpus(t, "hevc-aac-8bit.mov")
	// Failing inside the ftyp, inside the moov, and inside the payload copy
	// are three different places in the writer.
	whole := len(remuxed(t, "hevc-aac-8bit.mov", sprocket.MP4))

	for _, limit := range []int{0, 4, whole / 2, whole - 1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), &shortWriter{left: limit}, sprocket.MP4)
			if !errors.Is(err, errWriteFailed) {
				t.Errorf("error = %v, want the writer's own error", err)
			}
		})
	}
}

func TestRemuxReportsATruncatedSource(t *testing.T) {
	// The faststart file keeps its moov in front, so the tables parse and the
	// payload copy is where the missing bytes show up. That is the one failure
	// the writer can only find once it is already moving samples.
	whole := readCorpus(t, "h264-aac-faststart.mp4")
	cut := whole[:len(whole)-1024]
	// Shrink the mdat to match the cut, so the box walk is happy and only the
	// sample ranges run past the end of the file.
	mdat := topLevelOffset(t, cut, "mdat")
	binary.BigEndian.PutUint32(cut[mdat:], uint32(int64(len(cut))-mdat))

	err := sprocket.Remux(bytes.NewReader(cut), int64(len(cut)), io.Discard, sprocket.MP4)
	if !errors.Is(err, sprocket.ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
}

// countingWriter counts what it is handed and keeps none of it. It deliberately
// implements nothing but io.Writer: an io.ReaderFrom would let io.CopyBuffer
// bypass the writer's own buffer and hide what this measures.
type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func TestRemuxHoldsBoundedMemory(t *testing.T) {
	const (
		sampleSize   = int64(64) << 20
		allocBudget  = uint64(4) << 20
		realBytesCap = int64(64) << 10
	)

	source, size := inflatedSource(t, sampleSize)
	out := &countingWriter{}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := sprocket.Remux(source, size, out, sprocket.MP4); err != nil {
		t.Fatalf("remux: %v", err)
	}
	runtime.ReadMemStats(&after)

	if out.n < sampleSize {
		t.Fatalf("wrote %d bytes, want at least the %d byte sample", out.n, sampleSize)
	}
	grew := after.TotalAlloc - before.TotalAlloc
	if grew > allocBudget {
		t.Errorf("allocated %d bytes to move %d, want at most %d", grew, out.n, allocBudget)
	} else {
		t.Logf("allocated %d bytes to move %d, with heap at %d", grew, out.n, after.HeapAlloc)
	}
}

// inflatedSource returns h264-aac-faststart.mp4 with its video track's last
// sample declared sampleSize bytes and its mdat grown to hold it, served by a
// reader that hands back zeros past the real bytes. Nothing moves: the stsz
// entry and the mdat size are patched in place, so every chunk offset still
// points where it did.
func inflatedSource(t *testing.T, sampleSize int64) (io.ReaderAt, int64) {
	t.Helper()

	whole := readCorpus(t, "h264-aac-faststart.mp4")

	// The video trak comes first, so the first stsz is its own. The body is a
	// version and flags word, a constant sample size, a count, and the table.
	stsz := bytes.Index(whole, []byte("stsz"))
	if stsz < 0 {
		t.Fatal("no stsz box in the corpus file")
	}
	body := whole[stsz+4:]
	if binary.BigEndian.Uint32(body[4:]) != 0 {
		t.Fatal("the video track declares a constant sample size, so there is no entry to inflate")
	}
	count := binary.BigEndian.Uint32(body[8:])
	if count == 0 {
		t.Fatal("the video track declares no samples")
	}
	binary.BigEndian.PutUint32(body[12+4*(count-1):], uint32(sampleSize))

	mdat := topLevelOffset(t, whole, "mdat")
	size := int64(len(whole)) + sampleSize
	binary.BigEndian.PutUint32(whole[mdat:], uint32(size-mdat))

	return &countingReaderAt{prefix: whole, size: size}, size
}

// topLevelTypes lists the file's top-level box types in order.
func topLevelTypes(t *testing.T, content []byte) []string {
	t.Helper()

	var types []string
	for off := int64(0); off+8 <= int64(len(content)); {
		size := int64(binary.BigEndian.Uint32(content[off:]))
		types = append(types, string(content[off+4:off+8]))
		// A declared size of 1 means the real one is the 64-bit largesize that
		// follows the type, which is how the payload box is written.
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(content[off+8:]))
		}
		if size < 8 {
			t.Fatalf("box at %d declares %d bytes", off, size)
		}
		off += size
	}
	return types
}
