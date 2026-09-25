package sprocket_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/isobmff"
	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

// tsTwins pairs each MPEG-TS corpus file with the ISOBMFF file it was copied
// out of, which carries the same bitstreams.
var tsTwins = map[string]string{
	"h264-aac.ts":   "h264-aac.mp4",
	"h264-aac.m2ts": "h264-aac.mp4",
	"h264-gop12.ts": "h264-gop12.mp4",
	"hevc-aac.ts":   "hevc-aac-8bit.mov",
}

// oneFrame is a frame of the 24 fps corpus.
const oneFrame = time.Second / 24

// TestProbeReadsTheTSCorpus is the family-specific half of the golden
// comparison TestProbeMatchesCorpusGoldens already makes over the whole corpus,
// and asserts the files are there.
func TestProbeReadsTheTSCorpus(t *testing.T) {
	names, err := corpus.TSFiles()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	for name := range tsTwins {
		if !slices.Contains(names, name) {
			t.Errorf("the corpus is missing %s", name)
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			info := probeBytes(t, readCorpus(t, name))
			twin := probeBytes(t, readCorpus(t, tsTwins[name]))
			if info.Rotation != 0 {
				t.Errorf("rotation = %d, want 0: a transport stream has no display matrix", info.Rotation)
			}
			// The twin's duration is its movie's; the stream's runs from the
			// audio's first frame, which the twin's edit list trims, to the
			// end of the video, so it is longer by the AAC priming delay,
			// which is under a frame.
			if diff := info.Duration - twin.Duration; diff < 0 || diff > oneFrame {
				t.Errorf("duration = %v, want within a frame over the twin's %v", info.Duration, twin.Duration)
			}
		})
	}
}

// tsImage serves a transport stream of size bytes whose first and last bytes
// are a real corpus file and whose middle is null packets, and counts what is
// read out of it. It stands in for a multi-gigabyte recording without writing
// one.
type tsImage struct {
	real []byte
	size int64
	read atomic.Int64
	// headless leaves the real file out of the front, so the front holds no
	// table at all.
	headless bool
}

func (s *tsImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), s.size-off)
	real := int64(len(s.real))
	for i := range p[:n] {
		at := off + int64(i)
		switch {
		case at < real && !s.headless:
			p[i] = s.real[at]
		case at >= s.size-real:
			p[i] = s.real[at-(s.size-real)]
		default:
			// A null packet: sync byte, PID 0x1fff, payload only, zeros.
			switch at % 188 {
			case 0:
				p[i] = 0x47
			case 1:
				p[i] = 0x1f
			case 2:
				p[i] = 0xff
			case 3:
				p[i] = 0x10
			default:
				p[i] = 0
			}
		}
	}
	s.read.Add(n)
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

// probeScanBytes is the mpegts package's front and tail cap, restated here
// because the test is the thing that holds the package to it.
const probeScanBytes = int64(4) << 20

// scanSlack is what the other two parsers and the packet size detection read
// before the transport stream parser starts: a box header, an element header,
// and the first three packets.
const scanSlack = int64(4) << 10

func TestProbeTSReadsOnlyItsScanBounds(t *testing.T) {
	const size = int64(188) << 23 // about 1.5 GiB
	real := readCorpus(t, "h264-aac.ts")
	want := probeBytes(t, real)

	image := &tsImage{real: real, size: size}
	info, err := sprocket.Probe(image, size)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if info.VideoCodec != "h264" || info.AudioCodec != "aac" || info.Width != 128 || info.Height != 72 {
		t.Errorf("info = %+v, want the corpus file's streams", info)
	}
	// The tail repeats the front's timestamps, so the timeline is the one the
	// real file has, however far apart the two copies are.
	if info.Duration != want.Duration {
		t.Errorf("duration = %v, want %v", info.Duration, want.Duration)
	}
	if got, budget := image.read.Load(), 2*probeScanBytes+scanSlack; got > budget {
		t.Errorf("read %d bytes of a %d byte file, want at most %d", got, size, budget)
	} else {
		t.Logf("read %d bytes of a %d byte file", got, size)
	}
}

func TestProbeTSWithNoTablesInTheScanIsUnsupported(t *testing.T) {
	const size = int64(188) << 20
	image := &tsImage{real: readCorpus(t, "h264-aac.ts"), size: size, headless: true}

	_, err := sprocket.Probe(image, size)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
	if got, budget := image.read.Load(), probeScanBytes+scanSlack; got > budget {
		t.Errorf("read %d bytes before giving up, want at most %d", got, budget)
	}
}

func TestProbeTSRejectsTablesWithNoMedia(t *testing.T) {
	// The first three packets are the service description and the two tables.
	// Cut there, the file declares a video stream and holds none of it.
	real := readCorpus(t, "h264-aac.ts")
	cut := real[:188*3]
	_, err := sprocket.Probe(bytes.NewReader(cut), int64(len(cut)))
	if !errors.Is(err, sprocket.ErrCorrupt) {
		t.Errorf("error = %v, want ErrCorrupt", err)
	}
}

// TestThumbnailTS holds a transport stream's thumbnails to its twin's: the
// same bitstream, so the same picture, at a time that differs by at most the
// AAC priming delay the stream's timeline starts with.
func TestThumbnailTS(t *testing.T) {
	cases := []struct {
		name string
		at   []time.Duration
	}{
		{"hevc-aac.ts", []time.Duration{0}},
		{"h264-aac.ts", []time.Duration{0, 1300 * time.Millisecond}},
		{"h264-aac.m2ts", []time.Duration{0}},
		{"h264-gop12.ts", []time.Duration{0, 700 * time.Millisecond, 1300 * time.Millisecond, 10 * time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, at := range tc.at {
				got := mustThumbnail(t, tc.name, at, sprocket.ThumbnailOptions{})
				want := mustThumbnail(t, tsTwins[tc.name], at, sprocket.ThumbnailOptions{})
				if diff := got.Time - want.Time; diff < 0 || diff > oneFrame {
					t.Errorf("at %v the keyframe is at %v and the twin's at %v", at, got.Time, want.Time)
				}
				if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
					t.Errorf("at %v the stream decoded a different picture from its twin", at)
				}
			}
		})
	}
}

// assertSameKeyframe holds a keyframe that went through a transport stream to
// the one it started as. The access unit delimiter and the in-band parameter
// sets a stream adds are what a remux into the MP4 family takes back out, so
// an H.264 keyframe comes back byte for byte. An HEVC one comes back with the
// SEI the encoder put in its hvcC record in front, because the stream carries
// it in-band and a remux keeps every SEI, so it ends with the original instead.
func assertSameKeyframe(t *testing.T, got, want []byte, codec string) {
	t.Helper()
	switch {
	case codec == "h264" && !bytes.Equal(got, want):
		t.Errorf("the keyframe is %d bytes and differs from the original's %d", len(got), len(want))
	case !bytes.HasSuffix(got, want):
		t.Errorf("the keyframe, %d bytes, does not end with the original's %d", len(got), len(want))
	}
}

// syncData reads the first keyframe of an ISOBMFF file held in memory.
func syncData(t *testing.T, content []byte) []byte {
	t.Helper()
	sample, err := demux(t, content).ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read the keyframe: %v", err)
	}
	return sample.Data
}

func TestRemuxTSIntoMP4(t *testing.T) {
	for _, name := range []string{"h264-aac.ts", "hevc-aac.ts", "h264-gop12.ts", "h264-aac.m2ts"} {
		for _, target := range fragmentedTargets {
			t.Run(name+"/"+string(target), func(t *testing.T) {
				source := readCorpus(t, name)
				out := remuxed(t, name, target)
				if types := topLevelTypes(t, out); !slices.Contains(types, "moof") {
					t.Fatalf("box order = %v, want movie fragments", types)
				}
				info := probeBytes(t, source)
				assertProbesAgree(t, probeBytes(t, out), info)
				assertSameKeyframe(t, syncData(t, out), syncData(t, readCorpus(t, tsTwins[name])), info.VideoCodec)
			})
		}
	}
}

func TestRemuxTSIntoMP4KeepsTheThumbnail(t *testing.T) {
	for _, name := range []string{"hevc-aac.ts", "h264-gop12.ts"} {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			out := remuxed(t, name, sprocket.MP4)
			for _, at := range []time.Duration{0, 1300 * time.Millisecond} {
				want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), at, sprocket.ThumbnailOptions{})
				skipIfCompiledOut(t, name, err)
				if err != nil {
					t.Fatalf("thumbnail the source at %v: %v", at, err)
				}
				got, err := sprocket.Thumbnail(bytes.NewReader(out), int64(len(out)), at, sprocket.ThumbnailOptions{})
				if err != nil {
					t.Fatalf("thumbnail the remux at %v: %v", at, err)
				}
				if got.Time != want.Time {
					t.Errorf("at %v the remux's keyframe is at %v and the source's at %v", at, got.Time, want.Time)
				}
				if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
					t.Errorf("at %v the remux decoded a different picture", at)
				}
			}
		})
	}
}

func TestRemuxMP4IntoTS(t *testing.T) {
	for _, name := range []string{"h264-aac.mp4", "hevc-aac-8bit.mov", "h264-gop12.mp4", "no-audio.mp4"} {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			out := remuxed(t, name, sprocket.TS)
			if len(out)%188 != 0 || out[0] != 0x47 {
				t.Fatalf("the output is %d bytes starting %#x, want whole 188 byte packets", len(out), out[0])
			}
			info := probeBytes(t, source)
			assertProbesAgree(t, probeBytes(t, out), info)

			// And back: the round trip keeps the probe and the keyframe.
			var back bytes.Buffer
			if err := sprocket.Remux(bytes.NewReader(out), int64(len(out)), &back, sprocket.MP4); err != nil {
				t.Fatalf("remux the stream back into an mp4: %v", err)
			}
			assertProbesAgree(t, probeBytes(t, back.Bytes()), info)
			assertSameKeyframe(t, syncData(t, back.Bytes()), syncData(t, source), info.VideoCodec)
		})
	}
}

func TestRemuxMP4IntoTSKeepsTheThumbnail(t *testing.T) {
	source := readCorpus(t, gop12)
	out := remuxed(t, gop12, sprocket.TS)
	for _, at := range []time.Duration{0, 1300 * time.Millisecond} {
		want, err := sprocket.Thumbnail(bytes.NewReader(source), int64(len(source)), at, sprocket.ThumbnailOptions{})
		skipIfCompiledOut(t, gop12, err)
		if err != nil {
			t.Fatalf("thumbnail the source at %v: %v", at, err)
		}
		got, err := sprocket.Thumbnail(bytes.NewReader(out), int64(len(out)), at, sprocket.ThumbnailOptions{})
		if err != nil {
			t.Fatalf("thumbnail the stream at %v: %v", at, err)
		}
		if diff := got.Time - want.Time; diff < 0 || diff > oneFrame {
			t.Errorf("at %v the stream's keyframe is at %v and the source's at %v", at, got.Time, want.Time)
		}
		if !bytes.Equal(got.Image.(*image.RGBA).Pix, want.Image.(*image.RGBA).Pix) {
			t.Errorf("at %v the stream decoded a different picture", at)
		}
	}
}

func TestRemuxIntoTSRefusesWhatTSCannotCarry(t *testing.T) {
	for _, name := range []string{"vp9-opus.webm", "prores-pcm.mov"} {
		t.Run(name, func(t *testing.T) {
			source := readCorpus(t, name)
			if sprocket.CanRemux(probeBytes(t, source), sprocket.TS) {
				t.Error("CanRemux said yes")
			}
		})
	}

	// An ISOBMFF source refuses by codec, which CanRemux predicts.
	source := readCorpus(t, "prores-pcm.mov")
	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.TS)
	if !errors.Is(err, sprocket.ErrIncompatible) {
		t.Errorf("error = %v, want ErrIncompatible", err)
	}
}

func TestRemuxMatroskaIntoTSIsUnsupported(t *testing.T) {
	source := readCorpus(t, "h264-aac.mkv")
	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.TS)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

func TestRemuxTSIntoTSCopiesTheFile(t *testing.T) {
	source := readCorpus(t, "h264-aac.ts")
	if out := remuxed(t, "h264-aac.ts", sprocket.TS); !bytes.Equal(out, source) {
		t.Error("the copy differs from the source")
	}
}

func TestRemuxTSIntoMatroskaIsUnsupported(t *testing.T) {
	source := readCorpus(t, "h264-aac.ts")
	err := sprocket.Remux(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.MKV)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

func TestTrimTSSourceIsUnsupported(t *testing.T) {
	source := readCorpus(t, "h264-gop12.ts")
	_, err := sprocket.Trim(bytes.NewReader(source), int64(len(source)), io.Discard, sprocket.MP4, time.Second, 2*time.Second)
	if !errors.Is(err, sprocket.ErrUnsupportedContainer) {
		t.Errorf("error = %v, want ErrUnsupportedContainer", err)
	}
}

func TestTrimIntoTS(t *testing.T) {
	out, actual := trimmedInto(t, gop12, sprocket.TS, 1200*time.Millisecond, 2400*time.Millisecond)
	if actual != time.Second {
		t.Errorf("the trim starts at %v, want the keyframe at 1s", actual)
	}
	// The stream's duration runs to the end of the frame shown last, and the
	// cut keeps the frames decoded by the end, which in a reordered stream
	// include a couple shown after it; an mp4 counts decode durations
	// instead. The two agree to those frames.
	mp4, _ := trimmedInto(t, gop12, sprocket.MP4, 1200*time.Millisecond, 2400*time.Millisecond)
	got, want := probeBytes(t, out), probeBytes(t, mp4)
	if diff := got.Duration - want.Duration; diff < -oneFrame || diff > 3*oneFrame {
		t.Errorf("duration = %v, want about the mp4 cut's %v", got.Duration, want.Duration)
	}
	if got.VideoCodec != want.VideoCodec || got.AudioCodec != want.AudioCodec || got.Width != want.Width {
		t.Errorf("probe = %+v, want the mp4 cut's %+v", got, want)
	}
}

// trimmedInto trims a corpus file into memory.
func trimmedInto(t *testing.T, name string, target sprocket.Container, start, end time.Duration) ([]byte, time.Duration) {
	t.Helper()
	source := readCorpus(t, name)
	var out bytes.Buffer
	actual, err := sprocket.Trim(bytes.NewReader(source), int64(len(source)), &out, target, start, end)
	if err != nil {
		t.Fatalf("trim %s: %v", name, err)
	}
	return out.Bytes(), actual
}

// hugeAccessUnit serves a transport stream of one program with one video PES
// whose access unit is size bytes: the tables and the corpus file's first
// access unit's parameter sets and slice header are real, and the rest of the
// slice is generated packet by packet, so the file never exists anywhere.
type hugeAccessUnit struct {
	head  []byte // real packets up to and including the PES start
	pid   uint16
	size  int64
	total int64
}

func (h *hugeAccessUnit) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= h.total {
		return 0, io.EOF
	}
	n := min(int64(len(p)), h.total-off)
	for i := range p[:n] {
		at := off + int64(i)
		if at < int64(len(h.head)) {
			p[i] = h.head[at]
			continue
		}
		k := at - int64(len(h.head))
		switch k % 188 {
		case 0:
			p[i] = 0x47
		case 1:
			p[i] = byte(h.pid >> 8)
		case 2:
			p[i] = byte(h.pid)
		case 3:
			p[i] = 0x10 | byte((k/188+1)&0x0f)
		default:
			p[i] = 0xaa
		}
	}
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

// newHugeAccessUnit takes the tables and the first video PES packet of a
// corpus file and follows them with enough generated video packets to make an
// access unit of about size bytes.
func newHugeAccessUnit(t *testing.T, size int64) *hugeAccessUnit {
	t.Helper()
	real := readCorpus(t, "h264-aac.ts")
	// Everything up to the second video PES, PID 0x100: the tables, and the
	// first access unit with its parameter sets and the start of its slice,
	// which the generated packets then carry on.
	var (
		head   []byte
		starts int
	)
	for off := 0; off+188 <= len(real); off += 188 {
		pkt := real[off : off+188]
		pid := uint16(pkt[1]&0x1f)<<8 | uint16(pkt[2])
		if pid == 0x100 && pkt[1]&0x40 != 0 {
			if starts++; starts == 2 {
				break
			}
		}
		head = append(head, pkt...)
	}
	packets := size / 184
	return &hugeAccessUnit{head: head, pid: 0x100, size: size, total: int64(len(head)) + packets*188}
}

func TestRemuxTSHoldsBoundedMemory(t *testing.T) {
	const (
		accessUnit  = int64(64) << 20
		allocBudget = uint64(4) << 20
	)
	source := newHugeAccessUnit(t, accessUnit)
	out := &countingWriter{}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := sprocket.Remux(source, source.total, out, sprocket.MP4); err != nil {
		t.Fatalf("remux: %v", err)
	}
	runtime.ReadMemStats(&after)

	if out.n < accessUnit {
		t.Fatalf("wrote %d bytes, want at least the %d byte access unit", out.n, accessUnit)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > allocBudget {
		t.Errorf("allocated %d bytes to move %d, want at most %d", grew, out.n, allocBudget)
	} else {
		t.Logf("allocated %d bytes to move %d, with heap at %d", grew, out.n, after.HeapAlloc)
	}
}

func TestRemuxIntoTSHoldsBoundedMemory(t *testing.T) {
	const (
		sampleSize  = int64(64) << 20
		allocBudget = uint64(4) << 20
	)
	source, size := inflatedSource(t, sampleSize)
	// The inflated sample runs over whatever follows it and then into zeros,
	// which is no NAL structure at all. Its first length is made to cover all
	// of it, so it is one NAL unit of the whole sample.
	reader := source.(*countingReaderAt)
	file, err := isobmff.Parse(reader, size)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	video := file.VideoTrack()
	last, err := video.Sample(video.SampleCount() - 1)
	if err != nil {
		t.Fatalf("the inflated sample: %v", err)
	}
	binary.BigEndian.PutUint32(reader.prefix[last.Offset:], uint32(sampleSize-4))
	out := &countingWriter{}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := sprocket.Remux(source, size, out, sprocket.TS); err != nil {
		t.Fatalf("remux: %v", err)
	}
	runtime.ReadMemStats(&after)

	if out.n < sampleSize {
		t.Fatalf("wrote %d bytes, want at least the %d byte sample", out.n, sampleSize)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > allocBudget {
		t.Errorf("allocated %d bytes to move %d, want at most %d", grew, out.n, allocBudget)
	} else {
		t.Logf("allocated %d bytes to move %d, with heap at %d", grew, out.n, after.HeapAlloc)
	}
}

// TestRemuxTSKeepsAKeyframeFromTheFile checks the remux path of a TS source
// reads the keyframe the way the demuxer does, through the isobmff reader of
// the output, which is the one piece TestRemuxTSIntoMP4 does not name.
func TestRemuxTSKeepsAKeyframeFromTheFile(t *testing.T) {
	out := remuxed(t, "h264-gop12.ts", sprocket.MP4)
	file, err := isobmff.Parse(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sample, err := file.ReadSyncSample(1300 * time.Millisecond)
	if err != nil {
		t.Fatalf("read the keyframe at 1.3s: %v", err)
	}
	// The keyframes are half a second apart from the video's first frame,
	// which the stream's timeline starts a priming delay after zero.
	if diff := sample.MovieTime - time.Second; diff < 0 || diff > oneFrame {
		t.Errorf("the keyframe at or before 1.3s is at %v, want about 1s", sample.MovieTime)
	}
}
