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
	if err := Write(&buf, src, target); err != nil {
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
	err := track.tables.eachRange(func(r byteRange) error {
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

			if err := Write(io.Discard, file, tc.target); !errors.Is(err, tc.want) {
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
