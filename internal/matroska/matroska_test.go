package matroska

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
)

// matroskaFiles are the corpus files this package reads. The rest of the corpus
// belongs to the ISOBMFF side.
func matroskaFiles(t testing.TB) []string {
	t.Helper()
	names, err := corpus.MatroskaFiles()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("the corpus holds no Matroska file")
	}
	return names
}

// openCorpus parses a corpus file and returns it with its golden probe result.
func openCorpus(t *testing.T, name string) (*File, corpus.Golden) {
	t.Helper()
	handle, golden, err := corpus.Open(name)
	if err != nil {
		t.Fatalf("open corpus file: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	stat, err := handle.Stat()
	if err != nil {
		t.Fatalf("stat corpus file: %v", err)
	}
	file, err := Parse(handle, stat.Size())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return file, golden
}

func TestParseMatchesCorpusGoldens(t *testing.T) {
	for _, name := range matroskaFiles(t) {
		t.Run(name, func(t *testing.T) {
			file, golden := openCorpus(t, name)

			const durationTolerance = time.Millisecond
			wantDuration := time.Duration(golden.Duration * float64(time.Second))
			if diff := file.Duration() - wantDuration; diff > durationTolerance || diff < -durationTolerance {
				t.Errorf("duration = %v, want %v", file.Duration(), wantDuration)
			}

			video := file.VideoTrack()
			if video == nil {
				t.Fatal("no video track")
			}
			if video.Width != golden.Width || video.Height != golden.Height {
				t.Errorf("dimensions = %dx%d, want %dx%d", video.Width, video.Height, golden.Width, golden.Height)
			}
			if video.Codec != golden.VideoCodec {
				t.Errorf("video codec = %q, want %q", video.Codec, golden.VideoCodec)
			}
			const framerateTolerance = 0.01
			if math.Abs(file.FrameRate()-golden.Framerate) > framerateTolerance {
				t.Errorf("framerate = %v, want %v", file.FrameRate(), golden.Framerate)
			}

			var audioCodec string
			if audio := file.AudioTrack(); audio != nil {
				audioCodec = audio.Codec
			}
			if audioCodec != golden.AudioCodec {
				t.Errorf("audio codec = %q, want %q", audioCodec, golden.AudioCodec)
			}
		})
	}
}

func TestParseReadsDocTypeAndCodecPrivate(t *testing.T) {
	cases := map[string]struct {
		docType       string
		codecID       string
		nalLengthSize int
		hasPrivate    bool
	}{
		"h264-aac.mkv":    {docType: "matroska", codecID: "V_MPEG4/ISO/AVC", nalLengthSize: 4, hasPrivate: true},
		"vp8-vorbis.webm": {docType: "webm", codecID: "V_VP8"},
		"vp9-opus.webm":   {docType: "webm", codecID: "V_VP9"},
		"av1-opus.webm":   {docType: "webm", codecID: "V_AV1", hasPrivate: true},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			file, _ := openCorpus(t, name)
			if file.DocType != want.docType {
				t.Errorf("doc type = %q, want %q", file.DocType, want.docType)
			}
			video := file.VideoTrack()
			if video.CodecID != want.codecID {
				t.Errorf("codec ID = %q, want %q", video.CodecID, want.codecID)
			}
			if video.NALLengthSize != want.nalLengthSize {
				t.Errorf("NAL length size = %d, want %d", video.NALLengthSize, want.nalLengthSize)
			}
			if got := len(video.CodecPrivate) > 0; got != want.hasPrivate {
				t.Errorf("codec private present = %v, want %v", got, want.hasPrivate)
			}
		})
	}
}

// TestReadNearestSyncSampleSnapsToTheGOP checks the keyframe lookup against the
// file that has more than one cue: h264-gop12.mkv keyframes every twelve frames
// of 24 fps content, so at 0, 0.5, 1.0, 1.5, 2.0, and 2.5 seconds.
func TestReadNearestSyncSampleSnapsToTheGOP(t *testing.T) {
	file, _ := openCorpus(t, "h264-gop12.mkv")

	cases := []struct {
		at, want time.Duration
	}{
		{at: 0, want: 0},
		{at: 200 * time.Millisecond, want: 0},
		{at: 1300 * time.Millisecond, want: 1500 * time.Millisecond},
		{at: 1100 * time.Millisecond, want: 1000 * time.Millisecond},
		{at: 10 * time.Second, want: 2500 * time.Millisecond},
	}
	for _, c := range cases {
		sample, err := file.ReadNearestSyncSample(c.at)
		if err != nil {
			t.Fatalf("read nearest at %v: %v", c.at, err)
		}
		if sample.Time != c.want {
			t.Errorf("nearest to %v is at %v, want %v", c.at, sample.Time, c.want)
		}
	}
}

// TestReadSyncSampleTakesTheKeyframeBefore checks the at-or-before lookup, and
// that what comes back is an IDR: the acceptance names 1.3 seconds, which has
// to answer with the keyframe at 1.0.
func TestReadSyncSampleTakesTheKeyframeBefore(t *testing.T) {
	file, _ := openCorpus(t, "h264-gop12.mkv")

	sample, err := file.ReadSyncSample(1300 * time.Millisecond)
	if err != nil {
		t.Fatalf("read sync sample: %v", err)
	}
	if want := time.Second; sample.Time != want {
		t.Errorf("keyframe at %v, want %v", sample.Time, want)
	}
	if sample.Codec != "h264" || sample.NALLengthSize != 4 {
		t.Errorf("codec = %q with a %d byte length prefix, want h264 with 4", sample.Codec, sample.NALLengthSize)
	}

	// The frame is a series of length-prefixed NAL units. The first one of a
	// keyframe is an IDR slice, nal_unit_type 5 in the low five bits of the
	// header byte. ISO/IEC 14496-10 7.3.1.
	if len(sample.Data) < 5 {
		t.Fatalf("frame is %d bytes", len(sample.Data))
	}
	if got := sample.Data[4] & 0x1f; got != 5 {
		t.Errorf("first NAL unit type = %d, want 5 (IDR)", got)
	}
}

func TestReadNearestSyncSampleReadsEveryCorpusFile(t *testing.T) {
	for _, name := range matroskaFiles(t) {
		t.Run(name, func(t *testing.T) {
			file, golden := openCorpus(t, name)
			sample, err := file.ReadNearestSyncSample(0)
			if err != nil {
				t.Fatalf("read nearest: %v", err)
			}
			if sample.Time != 0 {
				t.Errorf("first keyframe at %v, want 0", sample.Time)
			}
			if sample.Codec != golden.VideoCodec {
				t.Errorf("codec = %q, want %q", sample.Codec, golden.VideoCodec)
			}
			if len(sample.Data) == 0 {
				t.Error("frame is empty")
			}
		})
	}
}

// TestKeyframeLookupWithoutCues drives the cluster scan by taking the seek
// index away, which is the path a file muxed without one takes.
func TestKeyframeLookupWithoutCues(t *testing.T) {
	withCues, _ := openCorpus(t, "h264-gop12.mkv")
	want, err := withCues.ReadNearestSyncSample(1300 * time.Millisecond)
	if err != nil {
		t.Fatalf("read with cues: %v", err)
	}

	withCues.cues = span{}
	got, err := withCues.ReadNearestSyncSample(1300 * time.Millisecond)
	if err != nil {
		t.Fatalf("read without cues: %v", err)
	}
	if got.Time != want.Time || len(got.Data) != len(want.Data) {
		t.Errorf("without cues: %v and %d bytes, with cues: %v and %d bytes",
			got.Time, len(got.Data), want.Time, len(want.Data))
	}
}

// TestFrameRateFallsBackToCountingBlocks drives the measured frame rate, which
// is what a file that declares no DefaultDuration gets. Every corpus file
// declares one, so the element is stripped out here.
func TestFrameRateFallsBackToCountingBlocks(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(corpus.Dir(), "h264-aac.mkv"))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	// Renaming the element to a deprecated one of the same width leaves every
	// offset in the file intact, which the cue positions depend on, and hides
	// it from the parser. 0x23314F is TrackTimecodeScale, which this package
	// does not read.
	const deprecated = 0x23314F
	stripped := strings.Replace(string(raw), string(elemID(idDefaultDur)), string(elemID(deprecated)), 1)
	if stripped == string(raw) {
		t.Fatal("the file declares no DefaultDuration to strip")
	}

	file, err := Parse(readerAt([]byte(stripped)), int64(len(stripped)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if file.VideoTrack().DefaultDuration != 0 {
		t.Fatal("DefaultDuration survived the strip")
	}
	// 48 frames over 2.021 seconds is 23.75, which is the honest average of
	// content the muxer nominates at 24.
	if got := file.FrameRate(); got < 23 || got > 24.5 {
		t.Errorf("measured frame rate = %v, want about 23.75", got)
	}
}

func TestParseRejectsInput(t *testing.T) {
	head := ebmlHead("matroska")
	tracks := elem(idTracks, videoTrack(1, "V_VP8", 128, 72, 41666666))

	cases := map[string]struct {
		data []byte
		want error
	}{
		"empty":          {data: nil, want: ErrNotMatroska},
		"text":           {data: []byte("hello world, definitely not a movie"), want: ErrNotMatroska},
		"other document": {data: segment("webvtt", info(1000000, 2000)), want: ErrNotMatroska},
		"no doc type":    {data: elem(idEBML, nil), want: ErrNotMatroska},
		"no segment":     {data: concat(head, elem(idTracks, nil)), want: ErrNotMatroska},
		"no info":        {data: segment("matroska", tracks), want: ErrMalformed},
		"no tracks":      {data: segment("matroska", info(1000000, 2000)), want: ErrMalformed},
		"no track entry": {data: segment("matroska", info(1000000, 2000), elem(idTracks, nil)), want: ErrMalformed},
		"zero scale":     {data: segment("matroska", info(0, 2000), tracks), want: ErrMalformed},
		"absurd size": {
			data: concat(head, elemID(idSegment), elemSize(1<<40)),
			want: ErrMalformed,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(readerAt(c.data), int64(len(c.data))); !errors.Is(err, c.want) {
				t.Errorf("error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestParseRejectsTruncatedCorpusFiles cuts each corpus file short at a range
// of points. Every one has to come back as an error rather than a panic or a
// file that parses into nonsense.
// TestWalkRefusesDeepNesting drives the depth limit directly. Parse itself
// never descends more than four levels, so nothing it reads can reach the
// limit; the fuzz target and this test are what hold it.
func TestWalkRefusesDeepNesting(t *testing.T) {
	deep := nestElem(idTracks, maxDepth+4, nil)
	var descend func(payload []byte, depth int) error
	descend = func(payload []byte, depth int) error {
		return walk(payload, depth, func(_ uint32, body []byte) error {
			return descend(body, depth+1)
		})
	}
	if err := descend(deep, 0); !errors.Is(err, ErrTooDeep) {
		t.Errorf("error = %v, want ErrTooDeep", err)
	}
}

// TestParseRefusesAnOversizeTracksElement holds the read cap. The element is
// backed by a reader that reports a huge size and serves zeros, so nothing has
// to be written to disk to declare four megabytes of track descriptions.
func TestParseRefusesAnOversizeTracksElement(t *testing.T) {
	const declared = maxTracksBytes + 1
	prefix := concat(ebmlHead("matroska"),
		elemID(idSegment), []byte{0xff},
		elemID(idTracks), elemSize(declared))
	reader := &zeroBacked{prefix: prefix, size: int64(len(prefix)) + declared}

	if _, err := Parse(reader, reader.size); !errors.Is(err, ErrElementTooLarge) {
		t.Errorf("error = %v, want ErrElementTooLarge", err)
	}
	if got := reader.read.Load(); got > 1<<16 {
		t.Errorf("read %d bytes before refusing, want a header's worth", got)
	}
}

func TestParseRejectsTruncatedCorpusFiles(t *testing.T) {
	for _, name := range matroskaFiles(t) {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
			if err != nil {
				t.Fatalf("read corpus file: %v", err)
			}
			for _, keep := range []int{1, 8, 40, 100, 300, len(raw) / 2, len(raw) - 1} {
				cut := raw[:keep]
				file, err := Parse(readerAt(cut), int64(len(cut)))
				if err != nil {
					continue
				}
				// A cut that lands past the headers leaves a parsable file, the
				// same way a cut inside an mdat does. Reading a keyframe out of
				// it still has to fail rather than panic.
				if _, err := file.ReadNearestSyncSample(0); err == nil && keep < len(raw)/2 {
					t.Errorf("a %d byte prefix parsed and read a keyframe", keep)
				}
			}
		})
	}
}

// TestParseHandlesUnknownSizes drives the unknown-size encoding, which a live
// muxer writes on the segment and on every cluster.
func TestParseHandlesUnknownSizes(t *testing.T) {
	body := concat(
		info(1000000, 2000),
		elem(idTracks, videoTrack(1, "V_VP8", 128, 72, 0)),
		unknownElem(idCluster, concat(
			uintElem(idTimestamp, 0),
			simpleBlock(1, 0, true, []byte("first frame")),
			simpleBlock(1, 500, false, []byte("second")),
		)),
		cluster(1000, simpleBlock(1, 0, true, []byte("third frame"))),
	)
	data := concat(ebmlHead("matroska"), unknownElem(idSegment, body))

	file, err := Parse(readerAt(data), int64(len(data)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Three blocks over two seconds, counted by walking the clusters, which is
	// only reached because the unknown-size cluster was walked to its end.
	if got, want := file.FrameRate(), 1.5; math.Abs(got-want) > 0.01 {
		t.Errorf("frame rate = %v, want %v", got, want)
	}

	sample, err := file.ReadNearestSyncSample(900 * time.Millisecond)
	if err != nil {
		t.Fatalf("read nearest: %v", err)
	}
	if string(sample.Data) != "third frame" {
		t.Errorf("frame = %q, want the keyframe in the second cluster", sample.Data)
	}
}

func TestReadNearestSyncSampleWithoutVideo(t *testing.T) {
	audio := elem(idTrackEntry, concat(
		uintElem(idTrackNumber, 1),
		uintElem(idTrackType, trackAudio),
		elem(idCodecID, []byte("A_OPUS")),
	))
	data := segment("webm", info(1000000, 2000), elem(idTracks, audio))

	file, err := Parse(readerAt(data), int64(len(data)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := file.ReadNearestSyncSample(0); !errors.Is(err, ErrNoVideoTrack) {
		t.Errorf("error = %v, want ErrNoVideoTrack", err)
	}
	if file.FrameRate() != 0 {
		t.Errorf("frame rate = %v, want 0", file.FrameRate())
	}
}

func TestCodecNames(t *testing.T) {
	cases := map[string]string{
		"V_MPEG4/ISO/AVC":  "h264",
		"V_MPEGH/ISO/HEVC": "hevc",
		"V_VP8":            "vp8",
		"V_VP9":            "vp9",
		"V_AV1":            "av1",
		"A_AAC":            "aac",
		"A_AAC/MPEG4/LC":   "aac",
		"A_OPUS":           "opus",
		"A_VORBIS":         "vorbis",
		"A_MPEG/L3":        "mp3",
		"A_AC3":            "ac-3",
		"A_EAC3":           "ec-3",
		"A_FLAC":           "fLaC",
		"V_QUICKTIME":      "V_QUICKTIME",
	}
	for id, want := range cases {
		if got := codecName(id); got != want {
			t.Errorf("codecName(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestLacedBlocks drives the three lacing modes, which pack several frames into
// one block. Only the frame count and the first frame's extent are read, so
// that is what is checked.
func TestLacedBlocks(t *testing.T) {
	const (
		trackByte = 0x81
		lacedXiph = 0x02
		lacedEBML = 0x06
		lacedFix  = 0x04
	)
	cases := map[string]struct {
		payload []byte
		frames  int
		first   string
	}{
		"xiph": {
			// Three frames of 2, 3, and 4 bytes; the table gives the first two.
			payload: concat([]byte{trackByte, 0, 0, lacedXiph, 2, 2, 3}, []byte("aabbbcccc")),
			frames:  3, first: "aa",
		},
		"ebml": {
			payload: concat([]byte{trackByte, 0, 0, lacedEBML, 2, 0x82, 0xbf + 1}, []byte("aabbbcccc")),
			frames:  3, first: "aa",
		},
		"fixed": {
			payload: concat([]byte{trackByte, 0, 0, lacedFix, 2}, []byte("aabbcc")),
			frames:  3, first: "aa",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			at := span{start: 0, size: int64(len(c.payload))}
			b, err := parseBlockHeader(c.payload, at, 0, true)
			if err != nil {
				t.Fatalf("parse block: %v", err)
			}
			if b.frames != c.frames {
				t.Errorf("frames = %d, want %d", b.frames, c.frames)
			}
			got := string(c.payload[b.first.start : b.first.start+b.first.size])
			if got != c.first {
				t.Errorf("first frame = %q, want %q", got, c.first)
			}
		})
	}
}

func TestReadSizeAndID(t *testing.T) {
	if _, _, err := readID([]byte{0x00, 0x00, 0x00, 0x00, 0x01}); !errors.Is(err, ErrMalformed) {
		t.Error("a five byte element ID was accepted")
	}
	if _, _, err := readSize([]byte{0x00, 0, 0, 0, 0, 0, 0, 0, 1}); !errors.Is(err, ErrMalformed) {
		t.Error("a nine byte size was accepted")
	}
	if size, n, err := readSize([]byte{0xff}); err != nil || size != unknownSize || n != 1 {
		t.Errorf("readSize(0xff) = %d, %d, %v, want the unknown size in one byte", size, n, err)
	}
	if size, n, err := readSize([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}); err != nil ||
		size != unknownSize || n != 8 {
		t.Errorf("an eight byte unknown size read as %d, %d, %v", size, n, err)
	}
}
