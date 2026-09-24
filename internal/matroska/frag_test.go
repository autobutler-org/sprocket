package matroska

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// openMatroska parses a corpus file of this package's own family.
func openMatroska(t *testing.T, name string) (*File, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	file, err := Parse(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file, raw
}

// writeFragmented writes a Matroska source into a fragmented mp4 and parses the
// result back.
func writeFragmented(t *testing.T, src *File, cut *TrimSpan) (*isobmff.File, []byte) {
	t.Helper()

	var out bytes.Buffer
	if err := WriteFragmented(&out, src, isobmff.TargetMP4, cut); err != nil {
		t.Fatalf("write a fragmented mp4: %v", err)
	}
	file, err := isobmff.Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("reparse the output: %v", err)
	}
	return file, out.Bytes()
}

func TestWriteFragmentedFromMatroska(t *testing.T) {
	for _, name := range []string{"h264-aac.mkv", "hevc-aac.mkv", "av1-opus.webm", "vp9-opus.webm"} {
		t.Run(name, func(t *testing.T) {
			src, _ := openMatroska(t, name)
			out, _ := writeFragmented(t, src, nil)

			if !out.Fragmented {
				t.Error("the output is not fragmented")
			}
			if got, want := out.Duration(), src.Duration(); got != want {
				t.Errorf("duration = %v, want %v", got, want)
			}
			video, audio := out.VideoTrack(), out.AudioTrack()
			if video == nil || audio == nil {
				t.Fatalf("the output has video %v and audio %v", video, audio)
			}
			if got, want := video.Entry.Codec, src.VideoTrack().Codec; got != want {
				t.Errorf("video codec = %q, want %q", got, want)
			}
			if got, want := audio.Entry.Codec, src.AudioTrack().Codec; got != want {
				t.Errorf("audio codec = %q, want %q", got, want)
			}
			if video.Width != 128 || video.Height != 72 {
				t.Errorf("dimensions = %dx%d, want 128x72", video.Width, video.Height)
			}
			// The measured rate has to land on the nominal one the source
			// declares, which is what says the derived durations add up.
			const tolerance = 0.01
			if diff := video.FrameRate() - src.FrameRate(); diff > tolerance || diff < -tolerance {
				t.Errorf("frame rate = %v, want %v", video.FrameRate(), src.FrameRate())
			}
		})
	}
}

// TestWriteFragmentedDerivesDecodeTimes holds the derivation for a source with
// B-frames: the decode times rise, every sample is shown at or after it is
// decoded, and the edit list takes the lead back off so the presentation times
// are the ones the source stated.
func TestWriteFragmentedDerivesDecodeTimes(t *testing.T) {
	src, _ := openMatroska(t, "h264-gop12.mkv")
	out, raw := writeFragmented(t, src, nil)

	video := out.VideoTrack()
	if len(video.Edits) != 1 || video.Edits[0].MediaTime <= 0 {
		t.Fatalf("edit list = %v, want one entry with a positive media time", video.Edits)
	}
	delay := uint64(video.Edits[0].MediaTime)

	// Every block's timestamp in the source, in the order the file stores them.
	var shown []int64
	if err := src.eachCluster(func(payload span, _ int64) error {
		return src.eachBlock(payload, func(b block) error {
			if b.track == src.VideoTrack().Number {
				shown = append(shown, b.ticks)
			}
			return nil
		})
	}); err != nil {
		t.Fatalf("walk the source: %v", err)
	}

	var (
		previous uint64
		reorders bool
	)
	samples := trunSamples(t, raw, uint32(video.ID))
	for nth, s := range samples {
		if nth > 0 && s.decode < previous {
			t.Errorf("sample %d decodes at %d, before sample %d at %d", nth, s.decode, nth-1, previous)
		}
		previous = s.decode
		if s.comp < 0 {
			t.Errorf("sample %d is shown %d ticks before it is decoded", nth, -s.comp)
		}
		if s.comp > 0 {
			reorders = true
		}
		// The edit list is what puts the presentation back where the source had
		// it, so the sample has to come out at the source's own time.
		if nth >= len(shown) {
			break
		}
		if got, want := int64(s.decode)+s.comp-int64(delay), shown[nth]; got != want {
			t.Errorf("sample %d is shown at %d, want the source's %d", nth, got, want)
		}
	}
	if len(samples) != len(shown) {
		t.Errorf("the output holds %d video samples, want the source's %d", len(samples), len(shown))
	}
	if !reorders {
		t.Error("no sample carries a composition offset, so this file has no B-frames to test with")
	}

	// A lookup at 1.3 seconds lands where it does on the source: the keyframe
	// at or before is the 1.0 second one, and the nearest is the 1.5 second
	// one. Both are shown at the source's own time once the edit list takes
	// the lead back off, and sit that lead later on the media timeline.
	for _, tc := range []struct {
		name string
		read func(time.Duration) (isobmff.SyncSample, error)
		want time.Duration
	}{
		{name: "at or before", read: out.ReadSyncSample, want: time.Second},
		{name: "nearest", read: out.ReadNearestSyncSample, want: 1500 * time.Millisecond},
	} {
		sample, err := tc.read(1300 * time.Millisecond)
		if err != nil {
			t.Fatalf("read the keyframe %s 1.3s: %v", tc.name, err)
		}
		if sample.MovieTime != tc.want {
			t.Errorf("the keyframe %s 1.3s is shown at %v, want %v", tc.name, sample.MovieTime, tc.want)
		}
		if got, want := sample.Time-sample.MovieTime, time.Duration(delay)*time.Second/time.Duration(video.Timescale); got != want {
			t.Errorf("the keyframe %s 1.3s sits %v ahead on the media timeline, want the %v lead", tc.name, got, want)
		}
	}
}

// TestWriteFragmentedRoundTripsTheStream is the three-way comparison: an mp4
// goes into a Matroska file and back out again, and the keyframe's bytes have
// to be the same all three times. Nothing is re-encoded, so anything else means
// a writer moved the wrong bytes.
func TestWriteFragmentedRoundTripsTheStream(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(corpus.Dir(), "h264-aac.mp4"))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	source, err := isobmff.Parse(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse the source: %v", err)
	}

	var intoMatroska bytes.Buffer
	if err := Write(&intoMatroska, source, isobmff.TargetMKV, nil); err != nil {
		t.Fatalf("write the mkv: %v", err)
	}
	middle, err := Parse(bytes.NewReader(intoMatroska.Bytes()), int64(intoMatroska.Len()))
	if err != nil {
		t.Fatalf("parse the mkv: %v", err)
	}
	back, _ := writeFragmented(t, middle, nil)

	// Every keyframe lookup a thumbnail could make, including one past the
	// end, has to find the same bytes in all three files.
	for _, at := range []time.Duration{0, 700 * time.Millisecond, 1300 * time.Millisecond, time.Hour} {
		first, err := source.ReadSyncSample(at)
		if err != nil {
			t.Fatalf("read the source keyframe at %v: %v", at, err)
		}
		second, err := middle.ReadSyncSample(at)
		if err != nil {
			t.Fatalf("read the mkv keyframe at %v: %v", at, err)
		}
		third, err := back.ReadSyncSample(at)
		if err != nil {
			t.Fatalf("read the fragmented keyframe at %v: %v", at, err)
		}

		if !bytes.Equal(first.Data, second.Data) {
			t.Errorf("at %v the mkv's keyframe is %d bytes, the source's %d", at, len(second.Data), len(first.Data))
		}
		if !bytes.Equal(first.Data, third.Data) {
			t.Errorf("at %v the round trip's keyframe is %d bytes, the source's %d", at, len(third.Data), len(first.Data))
		}
		if !bytes.Equal(first.Config, third.Config) {
			t.Errorf("at %v the round trip lost the codec configuration record", at)
		}
	}
	if got, want := back.Duration(), middle.Duration(); got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
}

func TestWriteFragmentedRefusesLacedBlocks(t *testing.T) {
	// A fixed-laced block of two frames, which an ISOBMFF sample cannot hold.
	const lacedFixed = 0x04
	laced := elem(idSimpleBlock, concat([]byte{0x81, 0, 0, blockKeyframeFlag | lacedFixed, 1}, make([]byte, 4)))
	data := segment("matroska",
		info(defaultTimestampScale, 1000),
		elem(idTracks, videoTrack(1, "V_VP9", 128, 72, 0)),
		cluster(0, laced),
	)
	file, err := Parse(readerAt(data), int64(len(data)))
	if err != nil {
		t.Fatalf("parse the handcrafted file: %v", err)
	}

	err = WriteFragmented(&bytes.Buffer{}, file, isobmff.TargetMP4, nil)
	if !errors.Is(err, isobmff.ErrUnsupportedSource) {
		t.Errorf("error = %v, want ErrUnsupportedSource", err)
	}
}

func TestTrimSpanSnapsToAKeyframe(t *testing.T) {
	src, _ := openMatroska(t, "h264-gop12.mkv")

	for _, tc := range []struct {
		start, end time.Duration
		want       time.Duration
	}{
		{start: 0, end: time.Second, want: 0},
		{start: -time.Second, end: time.Second, want: 0},
		{start: 1300 * time.Millisecond, end: 2200 * time.Millisecond, want: time.Second},
		{start: 1500 * time.Millisecond, end: 2200 * time.Millisecond, want: 1500 * time.Millisecond},
		{start: time.Hour, end: 2 * time.Hour, want: 2500 * time.Millisecond},
	} {
		t.Run(tc.start.String(), func(t *testing.T) {
			cut, actual, err := src.TrimSpan(tc.start, tc.end)
			if err != nil {
				t.Fatalf("trim span: %v", err)
			}
			if actual != tc.want {
				t.Errorf("the cut begins at %v, want %v", actual, tc.want)
			}
			// The audio track begins on its last block at or before the video's
			// first, the frame that straddles it, never after: a cut that
			// started late would be silent where the picture is not.
			video, audio := src.VideoTrack(), src.AudioTrack()
			anchor := cut.First[video.Number]
			want := int64(math.MinInt64)
			if err := src.eachCluster(func(payload span, _ int64) error {
				return src.eachBlock(payload, func(b block) error {
					if b.track == audio.Number && b.ticks <= anchor {
						want = max(want, b.ticks)
					}
					return nil
				})
			}); err != nil {
				t.Fatalf("walk the source: %v", err)
			}
			if got := cut.First[audio.Number]; got != want {
				t.Errorf("audio begins at %d, want the block at %d that straddles the video's %d", got, want, anchor)
			}
			if cut.Origin != anchor {
				t.Errorf("origin = %d, want the video keyframe at %d", cut.Origin, anchor)
			}
		})
	}
}

// TestTrimSpanEdges covers the two anchors a keyframe grid does not give: a
// file whose first keyframe comes after the requested start snaps forward to
// it, and a file with no video anchors on the requested time itself.
func TestTrimSpanEdges(t *testing.T) {
	t.Run("snaps forward", func(t *testing.T) {
		data := segment("webm",
			info(defaultTimestampScale, 1000),
			elem(idTracks, videoTrack(1, "V_VP9", 128, 72, 0)),
			cluster(0,
				simpleBlock(1, 0, false, []byte("delta")),
				simpleBlock(1, 500, true, []byte("key")),
				simpleBlock(1, 900, false, []byte("delta"))),
		)
		file, err := Parse(readerAt(data), int64(len(data)))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cut, actual, err := file.TrimSpan(100*time.Millisecond, time.Second)
		if err != nil {
			t.Fatalf("trim span: %v", err)
		}
		if actual != 500*time.Millisecond || cut.First[1] != 500 {
			t.Errorf("the cut begins at %v on block %d, want the keyframe at 500ms", actual, cut.First[1])
		}
	})

	t.Run("no video", func(t *testing.T) {
		audio := elem(idTrackEntry, concat(
			uintElem(idTrackNumber, 1),
			uintElem(idTrackType, trackAudio),
			elem(idCodecID, []byte("A_OPUS")),
		))
		data := segment("webm",
			info(defaultTimestampScale, 1000),
			elem(idTracks, audio),
			cluster(0,
				simpleBlock(1, 0, true, []byte("a")),
				simpleBlock(1, 400, true, []byte("b")),
				simpleBlock(1, 800, true, []byte("c"))),
		)
		file, err := Parse(readerAt(data), int64(len(data)))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		cut, actual, err := file.TrimSpan(500*time.Millisecond, time.Second)
		if err != nil {
			t.Fatalf("trim span: %v", err)
		}
		// The anchor is the requested time, and the track begins on the block
		// that straddles it.
		if actual != 500*time.Millisecond || cut.First[1] != 400 {
			t.Errorf("the cut begins at %v on block %d, want 500ms on the block at 400", actual, cut.First[1])
		}
	})
}

// TestTrimMatroskaIntoBothFamilies cuts the same window into each container and
// checks that both begin on the keyframe and hold the window that was asked
// for.
func TestTrimMatroskaIntoBothFamilies(t *testing.T) {
	src, _ := openMatroska(t, "h264-gop12.mkv")
	cut, actual, err := src.TrimSpan(1300*time.Millisecond, 2200*time.Millisecond)
	if err != nil {
		t.Fatalf("trim span: %v", err)
	}
	if actual != time.Second {
		t.Fatalf("the cut begins at %v, want 1s", actual)
	}

	t.Run("mkv", func(t *testing.T) {
		var out bytes.Buffer
		if err := Copy(&out, src, isobmff.TargetMKV, &cut); err != nil {
			t.Fatalf("copy: %v", err)
		}
		file, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		sample, err := file.ReadSyncSample(0)
		if err != nil {
			t.Fatalf("read the first keyframe: %v", err)
		}
		if sample.Time != 0 {
			t.Errorf("the first keyframe is at %v, want 0", sample.Time)
		}
		assertCutDuration(t, file.Duration())

		// Each track is rebased onto its own first block, as the ISOBMFF side
		// rebases each onto its own first sample, so both begin at zero.
		starts := map[uint64]int64{}
		if err := file.eachCluster(func(payload span, _ int64) error {
			return file.eachBlock(payload, func(b block) error {
				if at, seen := starts[b.track]; !seen || b.ticks < at {
					starts[b.track] = b.ticks
				}
				return nil
			})
		}); err != nil {
			t.Fatalf("walk the cut: %v", err)
		}
		for _, track := range file.Tracks {
			if starts[track.Number] != 0 {
				t.Errorf("track %d begins at %d, want 0", track.Number, starts[track.Number])
			}
		}
	})

	t.Run("mp4", func(t *testing.T) {
		out, raw := writeFragmented(t, src, &cut)
		sample, err := out.ReadSyncSample(0)
		if err != nil {
			t.Fatalf("read the first keyframe: %v", err)
		}
		if sample.MovieTime != 0 {
			t.Errorf("the first keyframe is shown at %v, want 0", sample.MovieTime)
		}
		assertCutDuration(t, out.Duration())

		for _, track := range out.Tracks {
			samples := trunSamples(t, raw, uint32(track.ID))
			if len(samples) == 0 || samples[0].decode != 0 {
				t.Errorf("track %d does not begin decoding at 0: %v", track.ID, samples[:min(len(samples), 1)])
			}
		}
	})
}

// assertCutDuration holds the length the 1.0 to 2.2 second window comes to.
func assertCutDuration(t *testing.T, got time.Duration) {
	t.Helper()
	const (
		want      = 1200 * time.Millisecond
		tolerance = 50 * time.Millisecond
	)
	if diff := got - want; diff > tolerance || diff < -tolerance {
		t.Errorf("duration = %v, want about %v", got, want)
	}
}

// trunSample is one sample of a fragmented file as the truns describe it. The
// walk below decodes them out of the bytes rather than asking the demuxer, so
// that a mistake shared between the writer and the parser cannot hide here.
type trunSample struct {
	decode uint64
	comp   int64
}

// trunSamples reads every sample of one track out of an mp4's movie fragments.
// ISO/IEC 14496-12 8.8.7 and 8.8.8.
func trunSamples(t *testing.T, file []byte, track uint32) []trunSample {
	t.Helper()

	var (
		out     []trunSample
		current uint32
		decode  uint64
	)
	var walkBoxes func(body []byte)
	walkBoxes = func(body []byte) {
		for off := 0; off+8 <= len(body); {
			size, header := int(binary.BigEndian.Uint32(body[off:])), 8
			typ := string(body[off+4 : off+8])
			// A declared size of 1 means the real one is the 64-bit largesize
			// after the type, which is how the payload box is written.
			if size == 1 {
				size, header = int(binary.BigEndian.Uint64(body[off+8:])), 16
			}
			if size < header || off+size > len(body) {
				t.Fatalf("box %q at %d declares %d bytes", typ, off, size)
			}
			payload := body[off+header : off+size]
			switch typ {
			case "moof", "traf":
				walkBoxes(payload)
			case "tfhd":
				current = binary.BigEndian.Uint32(payload[4:])
			case "tfdt":
				decode = binary.BigEndian.Uint64(payload[4:])
			case "trun":
				if current == track {
					out = append(out, readTrun(t, payload, &decode)...)
				}
			}
			off += size
		}
	}
	walkBoxes(file)
	return out
}

// readTrun decodes one sample run, which this writer always states in full: a
// data offset, then a duration, a size, and a flags word per sample, and a
// composition offset too on a track that reorders.
func readTrun(t *testing.T, body []byte, decode *uint64) []trunSample {
	t.Helper()

	const (
		wanted      = 0x000701
		compOffsets = 0x000800
	)
	version, flags := body[0], binary.BigEndian.Uint32(body)&0x00ffffff
	if flags&^compOffsets != wanted {
		t.Fatalf("trun flags = %06x, want %06x with or without %06x", flags, wanted, compOffsets)
	}
	stride := 12
	if flags&compOffsets != 0 {
		stride = 16
	}
	count := binary.BigEndian.Uint32(body[4:])
	at := 12 // past the count and the data offset

	out := make([]trunSample, 0, count)
	for range count {
		duration := binary.BigEndian.Uint32(body[at:])
		var comp int64
		if stride == 16 {
			raw := binary.BigEndian.Uint32(body[at+12:])
			comp = int64(raw)
			if version == 1 {
				comp = int64(int32(raw))
			}
		}
		out = append(out, trunSample{decode: *decode, comp: comp})
		*decode += uint64(duration)
		at += stride
	}
	return out
}
