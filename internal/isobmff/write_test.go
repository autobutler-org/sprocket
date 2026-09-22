package isobmff

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
)

// writeCorpus parses a corpus file, writes it out again, and parses the result.
// Both files are returned, because what this package's tests check is that the
// second says what the first said.
func writeCorpus(t *testing.T, name string, target Target) (src, out *File) {
	t.Helper()

	src, _, _ = openCorpus(t, name)
	var buf bytes.Buffer
	if err := Write(&buf, src, target, nil); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	out, err := Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("parse the output of %s: %v", name, err)
	}
	return src, out
}

// trackPayload reads a track's samples end to end, in order. A test may hold a
// corpus file whole; nothing in the library reads a track this way. The chunk
// layout is deliberately flattened away: the source's is not the output's, and
// what has to match is the bytes and their order.
func trackPayload(t *testing.T, f *File, track *Track) []byte {
	t.Helper()

	var out []byte
	err := track.tables.eachRange(0, track.tables.count, func(r byteRange) error {
		buf := make([]byte, r.size)
		if _, err := f.r.ReadAt(buf, r.offset); err != nil {
			return err
		}
		out = append(out, buf...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk samples: %v", err)
	}
	return out
}

func TestWriteCarriesEveryTrackAcross(t *testing.T) {
	for _, name := range []string{"h264-aac.mp4", "h264-aac-faststart.mp4", "hevc-aac-8bit.mov", "no-audio.mp4", "rotate-90.mp4"} {
		t.Run(name, func(t *testing.T) {
			src, out := writeCorpus(t, name, TargetMP4)

			if out.Timescale != src.Timescale || out.MovieDuration != src.MovieDuration {
				t.Errorf("movie header = %d ticks at %d, want %d at %d",
					out.MovieDuration, out.Timescale, src.MovieDuration, src.Timescale)
			}
			if len(out.Tracks) != len(src.Tracks) {
				t.Fatalf("wrote %d tracks, want %d", len(out.Tracks), len(src.Tracks))
			}

			for i, want := range src.Tracks {
				got := out.Tracks[i]
				if got.ID != want.ID || got.Handler != want.Handler {
					t.Errorf("track %d = %d/%q, want %d/%q", i, got.ID, got.Handler, want.ID, want.Handler)
				}
				if got.Timescale != want.Timescale || got.MediaDuration != want.MediaDuration {
					t.Errorf("track %d media = %d ticks at %d, want %d at %d",
						i, got.MediaDuration, got.Timescale, want.MediaDuration, want.Timescale)
				}
				if got.Language != want.Language {
					t.Errorf("track %d language = %q, want %q", i, got.Language, want.Language)
				}
				if got.Width != want.Width || got.Height != want.Height {
					t.Errorf("track %d = %dx%d, want %dx%d", i, got.Width, got.Height, want.Width, want.Height)
				}
				if got.Matrix != want.Matrix || got.Rotation != want.Rotation {
					t.Errorf("track %d matrix = %v (%d degrees), want %v (%d)",
						i, got.Matrix, got.Rotation, want.Matrix, want.Rotation)
				}
				if !slices.Equal(got.Edits, want.Edits) {
					t.Errorf("track %d edits = %+v, want %+v", i, got.Edits, want.Edits)
				}
				// The sample description is what a decoder is configured from,
				// so it has to come across byte for byte whatever it carried.
				if got.Entry.Format != want.Entry.Format || got.Entry.Codec != want.Entry.Codec {
					t.Errorf("track %d entry = %q/%q, want %q/%q",
						i, got.Entry.Format, got.Entry.Codec, want.Entry.Format, want.Entry.Codec)
				}
				if !bytes.Equal(got.Entry.Config, want.Entry.Config) {
					t.Errorf("track %d codec configuration changed", i)
				}
				if got.SampleCount() != want.SampleCount() {
					t.Errorf("track %d holds %d samples, want %d", i, got.SampleCount(), want.SampleCount())
				}

				// The payload is the point: every sample, in order, unchanged.
				if !bytes.Equal(trackPayload(t, out, got), trackPayload(t, src, want)) {
					t.Errorf("track %d samples differ from the source's", i)
				}
			}
		})
	}
}

func TestWriteKeepsTheSyncSampleAnswers(t *testing.T) {
	// Probe and Thumbnail both ask the tables where a keyframe is, so the
	// answer has to survive the trip whatever the source's chunk layout was.
	src, out := writeCorpus(t, "hevc-aac-8bit.mov", TargetMP4)

	want, err := src.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read the source keyframe: %v", err)
	}
	got, err := out.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read the written keyframe: %v", err)
	}

	if got.Index != want.Index || got.Time != want.Time || got.MovieTime != want.MovieTime {
		t.Errorf("keyframe %d at %v (%v on the movie timeline), want %d at %v (%v)",
			got.Index, got.Time, got.MovieTime, want.Index, want.Time, want.MovieTime)
	}
	if !bytes.Equal(got.Data, want.Data) {
		t.Error("the keyframe's bytes changed")
	}
}

func TestWriteRefusesWhatItCannotCarry(t *testing.T) {
	// A track with a handler that is neither video nor audio is dropped, so a
	// file made only of those has nothing left to write.
	timecodeOnly := concat(
		box("ftyp", []byte("isom\x00\x00\x00\x00isom")),
		box("moov", concat(
			box("mvhd", mvhd(1000, 1000)),
			box("trak", concat(
				box("tkhd", tkhd(1, matrixIdentity, 128, 72)),
				box("mdia", concat(box("mdhd", mdhd(1000, 1000)), box("hdlr", hdlr("tmcd")))),
			)),
		)),
	)

	for _, tc := range []struct {
		name   string
		source []byte
		file   string
		target Target
		want   error
	}{
		{name: "fragmented", file: "fragmented.mp4", target: TargetMP4, want: ErrUnsupportedSource},
		{name: "no media track", source: timecodeOnly, target: TargetMP4, want: ErrUnsupportedSource},
		{name: "prores and pcm", file: "prores-pcm.mov", target: TargetMP4, want: ErrIncompatibleCodec},
		{name: "unknown target", file: "hevc-aac-8bit.mov", target: Target("mkv"), want: ErrUnsupportedTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var file *File
			if tc.file != "" {
				file, _, _ = openCorpus(t, tc.file)
			} else {
				parsed, err := Parse(readerAt(tc.source), int64(len(tc.source)))
				if err != nil {
					t.Fatalf("parse the handcrafted source: %v", err)
				}
				file = parsed
			}

			if err := Write(io.Discard, file, tc.target, nil); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestWriteKeepsACompactSampleSizeTableCompact(t *testing.T) {
	// A source that used stz2 gets an stz2 back, rather than being expanded to
	// four bytes a sample. This is the one table whose two forms are not
	// interchangeable in size, and no corpus file uses the compact one.
	const fieldSize = 4
	stz2 := box("stz2", concat([]byte{0, 0, 0, 0}, []byte{0, 0, 0, fieldSize}, put32(3), []byte{0x12, 0x30}))

	var source Track
	if err := source.parseStbl(stz2, 0); err != nil {
		t.Fatalf("parse the handcrafted stz2: %v", err)
	}

	var b boxWriter
	b.sampleSizes(&source.tables)

	var out Track
	if err := out.parseStbl(b.buf, 0); err != nil {
		t.Fatalf("parse what was written: %v", err)
	}
	if out.tables.sizeBits != fieldSize {
		t.Errorf("field size = %d bits, want %d", out.tables.sizeBits, fieldSize)
	}
	if out.tables.count != source.tables.count {
		t.Errorf("count = %d, want %d", out.tables.count, source.tables.count)
	}
	for i := range source.tables.count {
		want, _ := source.tables.sampleSize(i)
		got, err := out.tables.sampleSize(i)
		if err != nil || got != want {
			t.Errorf("sample %d = %d (%v), want %d", i, got, err, want)
		}
	}
}

// runTables builds an stbl whose every table is cut mid-entry by a slice of
// samples 2 through 6: three stts runs, two ctts runs, sync samples on either
// side of the span, nine sizes, and three chunks of three.
func runTables(t *testing.T) *sampleTables {
	t.Helper()

	stbl := concat(
		box("stts", concat([]byte{0, 0, 0, 0}, put32(3),
			put32(3), put32(10), put32(2), put32(20), put32(4), put32(30))),
		box("ctts", concat([]byte{0, 0, 0, 0}, put32(2), put32(4), put32(5), put32(5), put32(7))),
		box("stss", concat([]byte{0, 0, 0, 0}, put32(3), put32(1), put32(4), put32(8))),
		box("stsz", concat([]byte{0, 0, 0, 0}, put32(0), put32(9),
			put32(10), put32(20), put32(30), put32(40), put32(50), put32(60), put32(70), put32(80), put32(90))),
		box("stsc", concat([]byte{0, 0, 0, 0}, put32(1), put32(1), put32(3), put32(1))),
		box("stco", concat([]byte{0, 0, 0, 0}, put32(3), put32(100), put32(1000), put32(2000))),
	)

	var track Track
	if err := track.parseStbl(stbl, 0); err != nil {
		t.Fatalf("parse the handcrafted stbl: %v", err)
	}
	return &track.tables
}

func TestSliceCutsTheRunTablesAtBothEnds(t *testing.T) {
	const first, n = 2, 5

	cut, err := runTables(t).slice(first, n)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}

	if cut.count != n {
		t.Errorf("count = %d, want %d", cut.count, n)
	}
	// The point of slicing rather than expanding: the runs stay runs. Three
	// stts entries in, three out, with the first and last trimmed.
	if got := len(cut.stts) / 8; got != 3 {
		t.Errorf("stts holds %d runs, want 3", got)
	}
	if got := len(cut.ctts) / 8; got != 2 {
		t.Errorf("ctts holds %d runs, want 2", got)
	}

	// The first kept sample decodes at zero, and the rest keep their spacing.
	for i, want := range []uint64{0, 10, 30, 50, 80} {
		if got := cut.sampleTime(uint32(i)); got != want {
			t.Errorf("sample %d decodes at %d, want %d", i, got, want)
		}
	}
	if got := cut.duration(); got != 110 {
		t.Errorf("duration = %d ticks, want 110", got)
	}
	// The composition offsets came across less the span's earliest composition
	// time, which was sample 2's five ticks, so the cut is displayed from zero.
	for i, want := range []int64{0, 0, 2, 2, 2} {
		if got := cut.compositionOffset(uint32(i)); got != want {
			t.Errorf("sample %d composition offset = %d, want %d", i, got, want)
		}
	}
	if got := cut.compositionTicks(0); got != 0 {
		t.Errorf("the first sample is displayed at %d, want 0", got)
	}
	for i, want := range []uint32{30, 40, 50, 60, 70} {
		got, err := cut.sampleSize(uint32(i))
		if err != nil || got != want {
			t.Errorf("sample %d is %d bytes (%v), want %d", i, got, err, want)
		}
	}

	// Sample 3 of the source was a sync sample and is sample 1 here; the ones
	// at 0 and 7 fall outside the span and are gone with it.
	if got := len(cut.stss) / 4; got != 1 {
		t.Fatalf("stss holds %d entries, want 1", got)
	}
	if sync, ok := cut.syncAtOrBefore(4); !ok || sync != 1 {
		t.Errorf("sync at or before sample 4 = %d (%v), want 1", sync, ok)
	}
	if _, ok := cut.syncAtOrBefore(0); ok {
		t.Error("sample 0 reports a sync sample at or before it, and the span starts mid-GOP")
	}
}

func TestSliceNormalizesToASignedCompositionTable(t *testing.T) {
	// Three samples ten ticks apart, the first displayed after the second: the
	// reordering B frames give a cut. Subtracting the earliest composition time
	// puts the second sample at zero and drives the rest below it, which is
	// what the signed form of the table is for.
	stbl := concat(
		box("stts", concat([]byte{0, 0, 0, 0}, put32(1), put32(3), put32(10))),
		box("ctts", concat([]byte{0, 0, 0, 0}, put32(2), put32(1), put32(20), put32(2), put32(0))),
		box("stsz", concat([]byte{0, 0, 0, 0}, put32(1), put32(3))),
	)

	var track Track
	if err := track.parseStbl(stbl, 0); err != nil {
		t.Fatalf("parse the handcrafted stbl: %v", err)
	}
	cut, err := track.tables.slice(0, 3)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}

	if !cut.cttsSigned {
		t.Error("the cut has negative composition offsets and did not ask for the signed table")
	}
	for i, want := range []int64{10, -10, -10} {
		if got := cut.compositionOffset(uint32(i)); got != want {
			t.Errorf("sample %d composition offset = %d, want %d", i, got, want)
		}
	}
	// Relative timing is untouched: every sample moved by the same ten ticks.
	for i, want := range []uint64{10, 0, 10} {
		if got := cut.compositionTicks(uint32(i)); got != want {
			t.Errorf("sample %d is displayed at %d, want %d", i, got, want)
		}
	}

	// And the signed table survives being written and read back.
	var b boxWriter
	b.entryTable("ctts", 1, 8, cut.ctts)
	var out Track
	if err := out.parseStbl(concat(b.buf, box("stsz", concat([]byte{0, 0, 0, 0}, put32(1), put32(3)))), 0); err != nil {
		t.Fatalf("parse what was written: %v", err)
	}
	if got := out.tables.compositionOffset(1); got != -10 {
		t.Errorf("sample 1 read back at %d, want -10", got)
	}
}

func TestEachRangeWalksASpan(t *testing.T) {
	// Samples 2 through 6 of three chunks of three: the tail of the first
	// chunk, all of the second, and the head of the third.
	want := []byteRange{
		{offset: 130, size: 30},
		{offset: 1000, size: 150},
		{offset: 2000, size: 70},
	}

	var got []byteRange
	if err := runTables(t).eachRange(2, 5, func(r byteRange) error {
		got = append(got, r)
		return nil
	}); err != nil {
		t.Fatalf("walk the span: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ranges = %v, want %v", got, want)
	}
}

func TestSliceRepacksACompactSampleSizeTable(t *testing.T) {
	// Four-bit entries pack two to a byte, so a span starting on an odd sample
	// starts halfway through one and has to be repacked rather than resliced.
	const fieldSize = 4
	stz2 := box("stz2", concat([]byte{0, 0, 0, 0}, []byte{0, 0, 0, fieldSize}, put32(5),
		[]byte{0x12, 0x34, 0x50}))

	var track Track
	if err := track.parseStbl(stz2, 0); err != nil {
		t.Fatalf("parse the handcrafted stz2: %v", err)
	}

	cut, err := track.tables.slice(1, 3)
	if err != nil {
		t.Fatalf("slice: %v", err)
	}
	for i, want := range []uint32{2, 3, 4} {
		got, err := cut.sampleSize(uint32(i))
		if err != nil || got != want {
			t.Errorf("sample %d is %d bytes (%v), want %d", i, got, err, want)
		}
	}
}

// rangePayload reads n of a track's samples from first, end to end.
func rangePayload(t *testing.T, f *File, track *Track, first, n uint32) []byte {
	t.Helper()

	var out []byte
	err := track.tables.eachRange(first, n, func(r byteRange) error {
		buf := make([]byte, r.size)
		if _, err := f.r.ReadAt(buf, r.offset); err != nil {
			return err
		}
		out = append(out, buf...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk samples: %v", err)
	}
	return out
}

func TestWriteCutsOneTrackAndCarriesTheOther(t *testing.T) {
	// The keyframe file's second GOP, with the audio track left whole: one
	// output, one cut track and one untouched one.
	const first, n = 24, 29

	src, _, _ := openCorpus(t, "h264-gop12.mp4")
	video := src.VideoTrack()

	var buf bytes.Buffer
	ranges := map[uint32]Range{video.ID: {First: first, Last: first + n - 1}}
	if err := Write(&buf, src, TargetMP4, ranges); err != nil {
		t.Fatalf("write the cut: %v", err)
	}
	out, err := Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("parse the cut: %v", err)
	}

	cutVideo, keptAudio := out.VideoTrack(), out.AudioTrack()
	if got := cutVideo.SampleCount(); got != n {
		t.Errorf("the cut video holds %d samples, want %d", got, n)
	}
	if got, want := keptAudio.SampleCount(), src.AudioTrack().SampleCount(); got != want {
		t.Errorf("the audio track holds %d samples, want the source's %d", got, want)
	}
	// The payload is the point: the samples that were asked for, in order.
	if !bytes.Equal(trackPayload(t, out, cutVideo), rangePayload(t, src, video, first, n)) {
		t.Error("the cut video's samples are not the source's")
	}
	// The cut invalidated the video's edit list, and left the audio's alone.
	if len(cutVideo.Edits) != 0 {
		t.Errorf("the cut video carries an edit list: %+v", cutVideo.Edits)
	}
	if !slices.Equal(keptAudio.Edits, src.AudioTrack().Edits) {
		t.Errorf("the audio edit list = %+v, want the source's %+v", keptAudio.Edits, src.AudioTrack().Edits)
	}
	if got, want := cutVideo.MediaDuration, uint64(n*512); got != want {
		t.Errorf("the cut video declares %d ticks, want %d", got, want)
	}
}

func TestWriteRefusesARangeTheTrackDoesNotHold(t *testing.T) {
	src, _, _ := openCorpus(t, "h264-gop12.mp4")
	id := src.VideoTrack().ID
	count := src.VideoTrack().SampleCount()

	for name, span := range map[string]Range{
		"past the end": {First: 0, Last: count},
		"backwards":    {First: 4, Last: 2},
	} {
		t.Run(name, func(t *testing.T) {
			err := Write(io.Discard, src, TargetMP4, map[uint32]Range{id: span})
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("error = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestPackLanguageRoundTrips(t *testing.T) {
	for _, code := range []string{"und", "eng", "deu", "zxx", "aaa"} {
		if got := decodeLanguage(packLanguage(code)); got != code {
			t.Errorf("packLanguage(%q) decoded back to %q", code, got)
		}
	}
	// Anything that is not three lowercase letters is written as undetermined.
	for _, code := range []string{"", "en", "ENG", "e1g", "engl"} {
		if got := decodeLanguage(packLanguage(code)); got != "und" {
			t.Errorf("packLanguage(%q) decoded back to %q, want %q", code, got, "und")
		}
	}
}
