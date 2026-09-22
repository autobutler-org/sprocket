package isobmff

import (
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
)

// openCorpus parses one corpus file and returns it alongside its golden.
func openCorpus(t *testing.T, name string) (*File, corpus.Golden, int64) {
	t.Helper()

	media, golden, err := corpus.Open(name)
	if err != nil {
		t.Fatalf("open corpus %s: %v", name, err)
	}
	t.Cleanup(func() { media.Close() })

	info, err := media.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}

	file, err := Parse(media, info.Size())
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file, golden, info.Size()
}

func TestParseMatchesGoldens(t *testing.T) {
	names, err := corpus.Files()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("corpus is empty")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			file, golden, size := openCorpus(t, name)

			const durationTolerance = time.Millisecond
			want := time.Duration(golden.Duration * float64(time.Second))
			if got := file.Duration(); absDuration(got-want) > durationTolerance {
				t.Errorf("duration = %v, want %v", got, want)
			}

			video := file.VideoTrack()
			if video == nil {
				t.Fatal("no video track")
			}
			if int(video.Width) != golden.Width || int(video.Height) != golden.Height {
				t.Errorf("dimensions = %dx%d, want %dx%d", video.Width, video.Height, golden.Width, golden.Height)
			}
			if video.Entry.Codec != golden.VideoCodec {
				t.Errorf("video codec = %q, want %q", video.Entry.Codec, golden.VideoCodec)
			}
			if video.Rotation != golden.Rotation {
				t.Errorf("rotation = %d, want %d", video.Rotation, golden.Rotation)
			}
			if !video.RotationKnown {
				t.Errorf("rotation not recognized from matrix %v", video.Matrix)
			}

			const framerateTolerance = 0.01
			if got := video.FrameRate(); math.Abs(got-golden.Framerate) > framerateTolerance {
				t.Errorf("framerate = %v, want %v", got, golden.Framerate)
			}

			audio := file.AudioTrack()
			switch {
			case golden.AudioCodec == "" && audio != nil:
				t.Errorf("audio track %q present, want none", audio.Entry.Codec)
			case golden.AudioCodec != "" && audio == nil:
				t.Errorf("no audio track, want %q", golden.AudioCodec)
			case audio != nil && audio.Entry.Codec != golden.AudioCodec:
				t.Errorf("audio codec = %q, want %q", audio.Entry.Codec, golden.AudioCodec)
			}

			// Bitrate is #5's rule, but it is derived from what this package
			// exposes: the file size over the movie duration.
			const bitrateTolerance = 1
			bitrate := int(float64(size*8) / file.Duration().Seconds())
			if diff := bitrate - golden.Bitrate; diff > bitrateTolerance || diff < -bitrateTolerance {
				t.Errorf("bitrate = %d, want %d", bitrate, golden.Bitrate)
			}

			if video.SampleBytes() <= 0 {
				t.Error("video sample bytes = 0, want the summed sample sizes")
			}
		})
	}
}

func TestParseReadsBrandsAndTimescales(t *testing.T) {
	file, _, _ := openCorpus(t, "h264-aac.mp4")

	if file.MajorBrand != "isom" {
		t.Errorf("major brand = %q, want %q", file.MajorBrand, "isom")
	}
	if file.Timescale != 48000 {
		t.Errorf("movie timescale = %d, want 48000", file.Timescale)
	}
	if file.Fragmented {
		t.Error("file reported as fragmented")
	}

	video := file.VideoTrack()
	if video.Timescale != 12288 {
		t.Errorf("media timescale = %d, want 12288", video.Timescale)
	}
	if video.ID != 1 {
		t.Errorf("track id = %d, want 1", video.ID)
	}
	if video.Language != "und" {
		t.Errorf("language = %q, want %q", video.Language, "und")
	}
	if len(video.Edits) != 1 || video.Edits[0].MediaTime != 1024 {
		t.Errorf("edits = %+v, want one edit with media time 1024", video.Edits)
	}
	if video.SampleCount() != 48 {
		t.Errorf("sample count = %d, want 48", video.SampleCount())
	}
}

func TestParseFragmented(t *testing.T) {
	file, _, _ := openCorpus(t, "fragmented.mp4")

	if !file.Fragmented {
		t.Error("file not reported as fragmented")
	}
	if file.MovieDuration != 0 {
		t.Errorf("mvhd duration = %d, want 0 for an empty moov", file.MovieDuration)
	}

	video := file.VideoTrack()
	if video.SampleCount() != 48 {
		t.Errorf("video sample count = %d, want 48", video.SampleCount())
	}
	if got := video.Duration(); absDuration(got-2*time.Second) > time.Millisecond {
		t.Errorf("video duration = %v, want 2s", got)
	}

	audio := file.AudioTrack()
	if audio.SampleCount() != 95 {
		t.Errorf("audio sample count = %d, want 95", audio.SampleCount())
	}
}

func TestSampleEntryCarriesCodecConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		format  string
		codec   string
		lenSize int
	}{
		{"h264-aac.mp4", "avc1", "h264", 4},
		{"hevc-aac-8bit.mov", "hvc1", "hevc", 4},
		{"hevc-aac-10bit.mov", "hvc1", "hevc", 4},
		{"fragmented.mp4", "avc1", "h264", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, _, _ := openCorpus(t, tc.name)
			entry := file.VideoTrack().Entry
			if entry.Format != tc.format {
				t.Errorf("format = %q, want %q", entry.Format, tc.format)
			}
			if entry.Codec != tc.codec {
				t.Errorf("codec = %q, want %q", entry.Codec, tc.codec)
			}
			if entry.NALLengthSize != tc.lenSize {
				t.Errorf("NAL length size = %d, want %d", entry.NALLengthSize, tc.lenSize)
			}
			if len(entry.Config) == 0 || entry.Config[0] != 1 {
				t.Errorf("config = %x, want a record starting with version 1", entry.Config)
			}
		})
	}
}

func TestReadSyncSampleReturnsAKeyframe(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration
	}{
		{"h264-aac.mp4", 0},
		{"h264-aac.mp4", time.Second},
		{"h264-aac-faststart.mp4", 0},
		{"hevc-aac-8bit.mov", 500 * time.Millisecond},
		{"hevc-aac-10bit.mov", 0},
		{"no-audio.mp4", time.Second},
		{"fragmented.mp4", 0},
		{"fragmented.mp4", time.Second},
	} {
		t.Run(tc.name+"@"+tc.at.String(), func(t *testing.T) {
			file, _, _ := openCorpus(t, tc.name)

			sample, err := file.ReadSyncSample(tc.at)
			if err != nil {
				t.Fatalf("read sync sample: %v", err)
			}
			if len(sample.Data) == 0 {
				t.Fatal("sample is empty")
			}
			if sample.NALLengthSize != 4 || len(sample.Config) == 0 {
				t.Fatalf("config = %x, NAL length size = %d", sample.Config, sample.NALLengthSize)
			}
			types := nalTypes(t, sample)
			// An encoder puts an SEI ahead of the slice, so the keyframe NAL is
			// not necessarily the first one in the sample.
			var want []byte
			switch sample.Codec {
			case "h264":
				want = []byte{5} // IDR slice
			case "hevc":
				want = []byte{19, 20, 21} // IDR_W_RADL, IDR_N_LP, CRA_NUT
			default:
				t.Fatalf("unexpected codec %q", sample.Codec)
			}
			if !slices.ContainsFunc(types, func(n byte) bool { return slices.Contains(want, n) }) {
				t.Errorf("NAL types %v hold none of the keyframe types %v", types, want)
			}
		})
	}
}

// nalTypes splits a sample into its length-prefixed NAL units and returns each
// unit's type, decoded per the sample's codec.
func nalTypes(t *testing.T, sample SyncSample) []byte {
	t.Helper()

	var types []byte
	for off := 0; off+sample.NALLengthSize < len(sample.Data); {
		length := 0
		for _, b := range sample.Data[off : off+sample.NALLengthSize] {
			length = length<<8 | int(b)
		}
		off += sample.NALLengthSize
		if length <= 0 || off+length > len(sample.Data) {
			t.Fatalf("NAL at %d declares %d bytes of a %d byte sample", off, length, len(sample.Data))
		}
		if sample.Codec == "hevc" {
			types = append(types, (sample.Data[off]>>1)&0x3f)
		} else {
			types = append(types, sample.Data[off]&0x1f)
		}
		off += length
	}
	if len(types) == 0 {
		t.Fatalf("sample of %d bytes holds no NAL", len(sample.Data))
	}
	return types
}

func TestReadSyncSampleRejectsAnOversizedSample(t *testing.T) {
	file, _, _ := openCorpus(t, "h264-aac.mp4")
	video := file.VideoTrack()
	video.tables.constSize = maxSampleBytes + 1

	if _, err := file.ReadSyncSample(0); err == nil {
		t.Fatal("want an error for a sample over the cap")
	}
}

// countingReaderAt records how many bytes a parse actually pulls from the file.
type countingReaderAt struct {
	r    io.ReaderAt
	read atomic.Int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read.Add(int64(n))
	return n, err
}

func TestParseReadsFarLessThanTheFile(t *testing.T) {
	media, _, err := corpus.Open("h264-aac.mp4")
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer media.Close()

	info, err := media.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	counter := &countingReaderAt{r: media}
	if _, err := Parse(counter, info.Size()); err != nil {
		t.Fatalf("parse: %v", err)
	}

	read := counter.read.Load()
	t.Logf("read %d of %d bytes (%.1f%%)", read, info.Size(), 100*float64(read)/float64(info.Size()))
	if read >= info.Size()/2 {
		t.Errorf("read %d of %d bytes, want well under half: mdat must not be read", read, info.Size())
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	const timescale = 1000

	for _, tc := range []struct {
		name string
		data []byte
		is   error
	}{
		{"empty", nil, ErrNotISOBMFF},
		{"short header", []byte{0, 0, 0}, ErrNotISOBMFF},
		{"not isobmff", []byte("#!/bin/sh\necho hello, world\n"), ErrNotISOBMFF},
		{
			"box larger than the file",
			concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), []byte{0x7f, 0xff, 0xff, 0xff, 'm', 'o', 'o', 'v'}),
			ErrMalformed,
		},
		{
			"64-bit box larger than the file",
			concat(
				box("ftyp", []byte("isom\x00\x00\x00\x00")),
				[]byte{0, 0, 0, 1, 'm', 'o', 'o', 'v', 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
			),
			ErrMalformed,
		},
		{
			"header runs past the file",
			concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), []byte{0, 0, 0, 0x20, 'm'}),
			ErrTruncated,
		},
		{"no moov", box("ftyp", []byte("isom\x00\x00\x00\x00")), ErrNotISOBMFF},
		{"empty moov", concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), box("moov", nil)), ErrMalformed},
		{
			"moov with no track",
			concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), box("moov", box("mvhd", mvhd(timescale, timescale)))),
			ErrMalformed,
		},
		{
			"child box overruns its parent",
			concat(
				box("ftyp", []byte("isom\x00\x00\x00\x00")),
				box("moov", concat(box("mvhd", mvhd(timescale, timescale)), []byte{0, 0, 0x10, 0, 't', 'r', 'a', 'k'})),
			),
			ErrMalformed,
		},
		{
			"stsz declares more samples than the moov has bytes",
			concat(
				box("ftyp", []byte("isom\x00\x00\x00\x00")),
				box("moov", concat(
					box("mvhd", mvhd(timescale, timescale)),
					box("trak", concat(
						box("tkhd", tkhd(1, matrixIdentity, 640, 480)),
						box("mdia", concat(
							box("mdhd", mdhd(timescale, timescale)),
							box("hdlr", hdlr("vide")),
							box("minf", box("stbl", box("stsz", concat(
								[]byte{0, 0, 0, 0},             // version and flags
								[]byte{0, 0, 0, 0},             // a variable sample size
								[]byte{0x3b, 0x9a, 0xca, 0x00}, // 1e9 entries
							)))),
						)),
					)),
				)),
			),
			ErrMalformed,
		},
		{
			"nested to death",
			concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), box("moov", nest("trak", 64, nil))),
			ErrMalformed,
		},
		{
			"truncated moov",
			concat(box("ftyp", []byte("isom\x00\x00\x00\x00")), []byte{0, 0, 0x04, 0, 'm', 'o', 'o', 'v'}),
			ErrMalformed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(readerAt(tc.data), int64(len(tc.data)))
			if err == nil {
				t.Fatal("want an error")
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("error %v, want one wrapping %v", err, tc.is)
			}
		})
	}
}

func TestParseRejectsATruncatedCorpusFile(t *testing.T) {
	names, err := corpus.Files()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
			if err != nil {
				t.Fatalf("read corpus file: %v", err)
			}
			// Half a file always cuts either the moov or the box that leads to it.
			half := raw[:len(raw)/2]
			if _, err := Parse(readerAt(half), int64(len(half))); err == nil {
				t.Fatal("want an error for a half file")
			}
		})
	}
}

func TestRotationFromMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		matrix [9]int32
		want   int
		known  bool
	}{
		{"identity", matrixIdentity, 0, true},
		{"90", [9]int32{0, one16, 0, -one16, 0, 0, 0, 0, one30}, 90, true},
		{"180", [9]int32{-one16, 0, 0, 0, -one16, 0, 0, 0, one30}, 180, true},
		{"270", [9]int32{0, -one16, 0, one16, 0, 0, 0, 0, one30}, 270, true},
		{"translated 90", [9]int32{0, one16, 0, -one16, 0, 0, 72 * one16, 0, one30}, 90, true},
		{"flipped", [9]int32{-one16, 0, 0, 0, one16, 0, 0, 0, one30}, 0, false},
		{"sheared", [9]int32{one16 / 2, one16 / 3, 0, 0, one16, 0, 0, 0, one30}, 0, false},
		{"zero", [9]int32{}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := rotationFromMatrix(tc.matrix)
			if got != tc.want || known != tc.known {
				t.Errorf("rotationFromMatrix = (%d, %t), want (%d, %t)", got, known, tc.want, tc.known)
			}
		})
	}
}

func TestSampleLookupsWalkTheCompactTables(t *testing.T) {
	file, _, _ := openCorpus(t, "h264-aac.mp4")
	video := file.VideoTrack()

	// stts is a single run of 48 samples at 512 ticks each, timescale 12288.
	for _, tc := range []struct {
		mediaTime uint64
		want      uint32
	}{{0, 0}, {511, 0}, {512, 1}, {1023, 1}, {12287, 23}, {1 << 40, 47}} {
		if got := video.tables.sampleAtTime(tc.mediaTime); got != tc.want {
			t.Errorf("sampleAtTime(%d) = %d, want %d", tc.mediaTime, got, tc.want)
		}
	}

	// stss holds sample 1 alone, so every sample resolves back to index 0.
	for _, index := range []uint32{0, 1, 24, 47} {
		got, ok := video.tables.syncAtOrBefore(index)
		if !ok || got != 0 {
			t.Errorf("syncAtOrBefore(%d) = (%d, %t), want (0, true)", index, got, ok)
		}
	}

	// stsc is (chunk 1: 2 samples), (chunk 2 onward: 1 sample), so sample 1 sits
	// in the first chunk right behind sample 0.
	first, err := video.tables.sampleRange(0)
	if err != nil {
		t.Fatalf("sampleRange(0): %v", err)
	}
	second, err := video.tables.sampleRange(1)
	if err != nil {
		t.Fatalf("sampleRange(1): %v", err)
	}
	if second.offset != first.offset+first.size {
		t.Errorf("sample 1 at %d, want %d", second.offset, first.offset+first.size)
	}
	if _, err := video.tables.sampleRange(video.tables.count); err == nil {
		t.Error("want an error past the last sample")
	}
}

func TestEmptyEditIsReported(t *testing.T) {
	edits := []Edit{{Duration: 500, MediaTime: -1}, {Duration: 1000, MediaTime: 0}}
	track := &Track{Edits: edits}
	if got := track.EmptyEditDuration(); got != 500 {
		t.Errorf("EmptyEditDuration = %d, want 500", got)
	}
	if got := (&Track{}).EmptyEditDuration(); got != 0 {
		t.Errorf("EmptyEditDuration with no edits = %d, want 0", got)
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func TestCodecNames(t *testing.T) {
	for format, want := range map[string]string{
		"avc1": "h264",
		"avc3": "h264",
		"hvc1": "hevc",
		"hev1": "hevc",
		"av01": "av1",
		"vp09": "vp9",
		"Opus": "opus",
		"mp4a": "aac",
		"dumb": "dumb",
	} {
		if got := codecName(format, 0); got != want {
			t.Errorf("codecName(%q) = %q, want %q", format, got, want)
		}
	}
	if got := codecName("mp4a", 0x6b); got != "mp3" {
		t.Errorf("codecName(mp4a, 0x6b) = %q, want %q", got, "mp3")
	}
	if got := codecName("mp4a", 0x40); got != "aac" {
		t.Errorf("codecName(mp4a, 0x40) = %q, want %q", got, "aac")
	}
}

func TestCapsAndGuardsReportTheirOwnErrors(t *testing.T) {
	if _, err := readWhole(readerAt(make([]byte, 64)), 0, 64, 32, "moov"); !errors.Is(err, ErrBoxTooLarge) {
		t.Errorf("readWhole over the cap = %v, want %v", err, ErrBoxTooLarge)
	}
	deep := func(string, []byte) error { return nil }
	if err := walk(box("moov", nil), maxBoxDepth+1, deep); !errors.Is(err, ErrTooDeep) {
		t.Errorf("walk past the depth limit = %v, want %v", err, ErrTooDeep)
	}
	audioOnly := &File{Tracks: []*Track{{Handler: "soun"}}}
	if _, err := audioOnly.ReadSyncSample(0); !errors.Is(err, ErrNoVideoTrack) {
		t.Errorf("ReadSyncSample with no video = %v, want %v", err, ErrNoVideoTrack)
	}
	noSamples := &File{Tracks: []*Track{{Handler: "vide", Timescale: 1000}}}
	if _, err := noSamples.ReadSyncSample(0); !errors.Is(err, ErrNoSyncSample) {
		t.Errorf("ReadSyncSample with no samples = %v, want %v", err, ErrNoSyncSample)
	}
}
