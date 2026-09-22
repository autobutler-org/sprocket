package matroska

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// openISOBMFF parses a corpus file of the other family, which is what this
// package writes from.
func openISOBMFF(t *testing.T, name string) *isobmff.File {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpus.Dir(), name))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	file, err := isobmff.Parse(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return file
}

// writeMatroska writes a source out and parses the result back.
func writeMatroska(t *testing.T, name string, target isobmff.Target) (*File, []byte) {
	t.Helper()
	src := openISOBMFF(t, name)

	var out bytes.Buffer
	if err := Write(&out, src, target, nil); err != nil {
		t.Fatalf("write %s as %s: %v", name, target, err)
	}
	file, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("reparse the output: %v", err)
	}
	return file, out.Bytes()
}

func TestWriteRoundTrips(t *testing.T) {
	cases := map[string]struct {
		codecID      string
		audioCodecID string
	}{
		"h264-aac.mp4":      {codecID: "V_MPEG4/ISO/AVC", audioCodecID: "A_AAC"},
		"hevc-aac-8bit.mov": {codecID: "V_MPEGH/ISO/HEVC", audioCodecID: "A_AAC"},
		"no-audio.mp4":      {codecID: "V_MPEG4/ISO/AVC"},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			file, _ := writeMatroska(t, name, isobmff.TargetMKV)

			if file.DocType != "matroska" {
				t.Errorf("doc type = %q, want matroska", file.DocType)
			}
			video := file.VideoTrack()
			if video.CodecID != want.codecID {
				t.Errorf("codec ID = %q, want %q", video.CodecID, want.codecID)
			}
			if video.Width != 128 || video.Height != 72 {
				t.Errorf("dimensions = %dx%d, want 128x72", video.Width, video.Height)
			}
			audio := file.AudioTrack()
			switch {
			case want.audioCodecID == "" && audio != nil:
				t.Errorf("the output has an audio track and the source has none")
			case want.audioCodecID != "" && audio == nil:
				t.Error("the output has no audio track")
			case audio != nil && audio.CodecID != want.audioCodecID:
				t.Errorf("audio codec ID = %q, want %q", audio.CodecID, want.audioCodecID)
			}

			sample, err := file.ReadNearestSyncSample(0)
			if err != nil {
				t.Fatalf("read the first keyframe back: %v", err)
			}
			if sample.Time != 0 {
				t.Errorf("the first keyframe is at %v, want 0", sample.Time)
			}
		})
	}
}

// TestWriteCopiesTheCodecConfiguration holds the two records that are not a
// straight copy of what the source stored: an AAC track's CodecPrivate is the
// AudioSpecificConfig out of the esds, and an H.264 track's is the avcC.
func TestWriteCopiesTheCodecConfiguration(t *testing.T) {
	src := openISOBMFF(t, "h264-aac.mp4")
	file, _ := writeMatroska(t, "h264-aac.mp4", isobmff.TargetMKV)

	if got, want := file.VideoTrack().CodecPrivate, src.VideoTrack().Entry.Config; !bytes.Equal(got, want) {
		t.Errorf("video CodecPrivate = %x, want the source avcC %x", got, want)
	}
	if got, want := file.AudioTrack().CodecPrivate, src.AudioTrack().Entry.DecoderConfig; !bytes.Equal(got, want) {
		t.Errorf("audio CodecPrivate = %x, want the esds AudioSpecificConfig %x", got, want)
	}
	if file.VideoTrack().NALLengthSize != src.VideoTrack().Entry.NALLengthSize {
		t.Errorf("NAL length size = %d, want %d",
			file.VideoTrack().NALLengthSize, src.VideoTrack().Entry.NALLengthSize)
	}
}

// TestWriteKeepsEveryKeyframe walks the output's seek index against the
// source's sync samples: a seek that lands on the wrong block is worse than one
// that fails.
func TestWriteKeepsEveryKeyframe(t *testing.T) {
	src := openISOBMFF(t, "h264-gop12.mp4")
	file, _ := writeMatroska(t, "h264-gop12.mp4", isobmff.TargetMKV)

	// Six keyframes, every half second of three seconds of 24 fps content.
	for i := range 6 {
		at := time.Duration(i) * 500 * time.Millisecond
		sample, err := file.ReadSyncSample(at)
		if err != nil {
			t.Fatalf("read the keyframe at %v: %v", at, err)
		}
		if sample.Time != at {
			t.Errorf("the keyframe at or before %v is at %v", at, sample.Time)
		}
		source, err := src.ReadSyncSample(at)
		if err != nil {
			t.Fatalf("read the source keyframe at %v: %v", at, err)
		}
		if !bytes.Equal(sample.Data, source.Data) {
			t.Errorf("the keyframe at %v differs from the source's %d bytes", at, len(source.Data))
		}
	}
}

func TestWriteRefuses(t *testing.T) {
	cases := map[string]struct {
		file   string
		target isobmff.Target
		want   error
	}{
		"unknown target":  {file: "h264-aac.mp4", target: isobmff.TargetMP4, want: isobmff.ErrUnsupportedTarget},
		"fragmented":      {file: "fragmented.mp4", target: isobmff.TargetMKV, want: isobmff.ErrUnsupportedSource},
		"prores into mkv": {file: "prores-pcm.mov", target: isobmff.TargetMKV, want: isobmff.ErrIncompatibleCodec},
		"h264 into webm":  {file: "h264-aac.mp4", target: isobmff.TargetWebM, want: isobmff.ErrIncompatibleCodec},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := openISOBMFF(t, c.file)
			if err := Write(&bytes.Buffer{}, src, c.target, nil); !errors.Is(err, c.want) {
				t.Errorf("error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestOpusHead checks the one configuration record this writer converts rather
// than copies: a dOps box holds the Opus identification header's body with the
// magic dropped and the multi-byte fields the other way round.
func TestOpusHead(t *testing.T) {
	dOps := []byte{
		0,          // version
		2,          // output channel count
		0x01, 0x38, // pre-skip, 312
		0x00, 0x00, 0xbb, 0x80, // input sample rate, 48000
		0x00, 0x00, // output gain
		0, // channel mapping family
	}
	track := &isobmff.Track{Entry: isobmff.SampleEntry{Codec: "opus", Config: dOps}}

	head, err := opusHead(track)
	if err != nil {
		t.Fatalf("opusHead: %v", err)
	}
	if string(head[:8]) != "OpusHead" {
		t.Errorf("header begins %q, want OpusHead", head[:8])
	}
	if head[8] != 1 || head[9] != 2 {
		t.Errorf("version %d and %d channels, want 1 and 2", head[8], head[9])
	}
	if got := binary.LittleEndian.Uint16(head[10:]); got != 312 {
		t.Errorf("pre-skip = %d, want 312", got)
	}
	if got := binary.LittleEndian.Uint32(head[12:]); got != 48000 {
		t.Errorf("sample rate = %d, want 48000", got)
	}
	if len(head) != 19 {
		t.Errorf("header is %d bytes, want 19", len(head))
	}
}

func TestSizeVintRoundTrips(t *testing.T) {
	for _, size := range []uint64{0, 1, 126, 127, 128, 16382, 16383, 16384, 1 << 20, 1 << 40} {
		encoded := sizeVint(size)
		got, n, err := readSize(encoded)
		if err != nil {
			t.Fatalf("readSize(%x) for %d: %v", encoded, size, err)
		}
		if n != len(encoded) || uint64(got) != size {
			t.Errorf("%d encoded to %x and read back as %d in %d bytes", size, encoded, got, n)
		}
	}
}
