package isobmff

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

// fragStream turns a slice of samples into the pull iterator WriteFragmented
// takes, which is all a test source has to be.
func fragStream(samples []FragSample) NextFragSample {
	at := 0
	return func() (FragSample, bool, error) {
		if at >= len(samples) {
			return FragSample{}, false, nil
		}
		s := samples[at]
		at++
		return s, true, nil
	}
}

// twoTrackStream is a handcrafted source: a video track of six frames in two
// fragments of three, reordered so that the second and third frames of each
// group are shown out of decode order, and an audio track of four frames
// interleaved with them. The payload is the byte at each sample's own offset,
// so a test can check that the data offsets point at the right bytes.
func twoTrackStream() (payload []byte, tracks []FragTrack, samples []FragSample) {
	const (
		video = 1
		audio = 2
	)
	tracks = []FragTrack{
		{
			ID: video, Handler: "vide", Timescale: 1000, Codec: "h264",
			Config: []byte{1, 0x42, 0, 0x1e, 0xff, 0xe1, 0, 0, 1, 0, 0}, Width: 128, Height: 72,
		},
		{
			ID: audio, Handler: "soun", Timescale: 1000, Codec: "aac",
			Config: []byte{0x12, 0x10}, Channels: 2, SampleRate: 48000,
		},
	}

	// Sample n's payload is n+1 copies of the byte n+1, laid out one after
	// another, so both the offset and the size of every sample are distinct.
	var at int64
	add := func(s FragSample, size int64) {
		s.Offset, s.Size = at, size
		at += size
		samples = append(samples, s)
	}

	add(FragSample{Track: video, Decode: 0, Duration: 40, Sync: true, Fragment: true}, 1)
	add(FragSample{Track: audio, Decode: 0, Duration: 20, Sync: true}, 2)
	add(FragSample{Track: video, Decode: 40, Duration: 40, Composition: 40}, 3)
	add(FragSample{Track: audio, Decode: 20, Duration: 20, Sync: true}, 4)
	add(FragSample{Track: video, Decode: 80, Duration: 40, Composition: -40}, 5)
	add(FragSample{Track: video, Decode: 120, Duration: 40, Sync: true, Fragment: true}, 6)
	add(FragSample{Track: audio, Decode: 40, Duration: 20, Sync: true}, 7)
	add(FragSample{Track: video, Decode: 160, Duration: 40, Composition: 40}, 8)
	add(FragSample{Track: audio, Decode: 60, Duration: 20, Sync: true}, 9)
	add(FragSample{Track: video, Decode: 200, Duration: 40, Composition: -40}, 10)

	payload = make([]byte, at)
	for _, s := range samples {
		for i := range s.Size {
			payload[s.Offset+i] = byte(s.Size)
		}
	}
	return payload, tracks, samples
}

// writeTwoTrackFragments writes the handcrafted stream and parses the result.
func writeTwoTrackFragments(t *testing.T) ([]byte, *File) {
	t.Helper()

	payload, tracks, samples := twoTrackStream()
	var out bytes.Buffer
	if err := WriteFragmented(&out, bytes.NewReader(payload), TargetMP4,
		240*time.Millisecond, tracks, fragStream(samples)); err != nil {
		t.Fatalf("write: %v", err)
	}

	file, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("parse the output: %v", err)
	}
	return out.Bytes(), file
}

func TestWriteFragmentedParsesBack(t *testing.T) {
	_, file := writeTwoTrackFragments(t)

	if !file.Fragmented {
		t.Error("the output does not report itself as fragmented")
	}
	if got, want := len(file.moofs), 2; got != want {
		t.Errorf("the output holds %d fragments, want %d", got, want)
	}
	if got, want := file.Duration(), 240*time.Millisecond; got != want {
		t.Errorf("duration = %v, want %v", got, want)
	}
	if got, want := len(file.Tracks), 2; got != want {
		t.Fatalf("the output holds %d tracks, want %d", got, want)
	}

	video, audio := file.VideoTrack(), file.AudioTrack()
	if video == nil || audio == nil {
		t.Fatalf("the output has video %v and audio %v", video, audio)
	}
	if got, want := video.Entry.Codec, "h264"; got != want {
		t.Errorf("video codec = %q, want %q", got, want)
	}
	if got, want := video.Entry.NALLengthSize, 4; got != want {
		t.Errorf("NAL length size = %d, want %d", got, want)
	}
	if got, want := audio.Entry.Codec, "aac"; got != want {
		t.Errorf("audio codec = %q, want %q", got, want)
	}
	if got, want := string(audio.Entry.DecoderConfig), "\x12\x10"; got != want {
		t.Errorf("AudioSpecificConfig = %q, want %q", got, want)
	}
	if got, want := video.SampleCount(), uint32(6); got != want {
		t.Errorf("video holds %d samples, want %d", got, want)
	}
	if got, want := audio.SampleCount(), uint32(4); got != want {
		t.Errorf("audio holds %d samples, want %d", got, want)
	}
	// The media header of a fragmented track declares nothing, so the duration
	// has to come from adding the fragments up.
	if got, want := video.Duration(), 240*time.Millisecond; got != want {
		t.Errorf("video duration = %v, want %v", got, want)
	}
}

// TestWriteFragmentedTfdtProgresses holds what makes the second fragment land
// where the first one stopped.
func TestWriteFragmentedTfdtProgresses(t *testing.T) {
	_, file := writeTwoTrackFragments(t)

	for _, tc := range []struct {
		track *Track
		name  string
		bases []uint64
	}{
		{track: file.VideoTrack(), name: "video", bases: []uint64{0, 120}},
		{track: file.AudioTrack(), name: "audio", bases: []uint64{0, 40}},
	} {
		got := make([]uint64, 0, len(tc.track.fragments))
		for _, f := range tc.track.fragments {
			got = append(got, f.baseTime)
		}
		if len(got) != len(tc.bases) {
			t.Errorf("%s holds %d fragments, want %d", tc.name, len(got), len(tc.bases))
			continue
		}
		for i, want := range tc.bases {
			if got[i] != want {
				t.Errorf("%s fragment %d begins at %d, want %d", tc.name, i, got[i], want)
			}
		}
	}
}

// TestWriteFragmentedDataOffsetsPointAtThePayload is the one assertion that
// catches a moof whose size was measured before it was finished: every sample
// has to read back as the bytes the source put at its own offset.
func TestWriteFragmentedDataOffsetsPointAtThePayload(t *testing.T) {
	out, file := writeTwoTrackFragments(t)
	_, _, samples := twoTrackStream()

	for _, track := range file.Tracks {
		var nth int
		for _, fragment := range track.fragments {
			err := file.fragmentSamples(track, fragment, func(s fragSample) {
				want := wantedSample(samples, track.ID, nth)
				nth++
				if s.size != want.Size {
					t.Errorf("track %d sample %d is %d bytes, want %d", track.ID, nth-1, s.size, want.Size)
					return
				}
				got := out[s.offset : s.offset+s.size]
				if !bytes.Equal(got, bytes.Repeat([]byte{byte(want.Size)}, int(want.Size))) {
					t.Errorf("track %d sample %d reads back as %v", track.ID, nth-1, got)
				}
			})
			if err != nil {
				t.Fatalf("walk track %d: %v", track.ID, err)
			}
		}
	}
}

// TestWriteFragmentedSampleFlagsAndTimes reads every sample back through the
// demuxer and holds its sync flag, decode time, and duration to what the
// stream said, which is what the trun's per-sample fields and the tfdt carry
// between them.
func TestWriteFragmentedSampleFlagsAndTimes(t *testing.T) {
	_, file := writeTwoTrackFragments(t)
	_, _, samples := twoTrackStream()

	for _, track := range file.Tracks {
		var nth int
		for _, fragment := range track.fragments {
			if err := file.fragmentSamples(track, fragment, func(s fragSample) {
				want := wantedSample(samples, track.ID, nth)
				if s.sync != want.Sync {
					t.Errorf("track %d sample %d sync = %v, want %v", track.ID, nth, s.sync, want.Sync)
				}
				if s.decode != want.Decode || s.duration != uint64(want.Duration) {
					t.Errorf("track %d sample %d decodes at %d for %d, want %d for %d",
						track.ID, nth, s.decode, s.duration, want.Decode, want.Duration)
				}
				nth++
			}); err != nil {
				t.Fatalf("walk track %d: %v", track.ID, err)
			}
		}
	}
}

// wantedSample is the nth sample of a track in the handcrafted stream.
func wantedSample(samples []FragSample, track uint32, nth int) FragSample {
	for _, s := range samples {
		if s.Track != track {
			continue
		}
		if nth == 0 {
			return s
		}
		nth--
	}
	return FragSample{}
}

// TestWriteFragmentedSignedCompositionOffsets holds the case the signed form of
// the box exists for: a track whose frames are shown out of decode order far
// enough that one of them is shown before it is decoded.
func TestWriteFragmentedSignedCompositionOffsets(t *testing.T) {
	_, file := writeTwoTrackFragments(t)

	video := file.VideoTrack()
	want := []int64{0, 40, -40, 0, 40, -40}
	var got []int64
	for _, fragment := range video.fragments {
		if err := file.fragmentSamples(video, fragment, func(s fragSample) {
			got = append(got, s.comp)
		}); err != nil {
			t.Fatalf("walk the video track: %v", err)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d composition offsets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sample %d has composition offset %d, want %d", i, got[i], want[i])
		}
	}
}

// TestWriteFragmentedReadsSyncSamples is the lookup a thumbnail makes, over the
// fragments rather than a sample table.
func TestWriteFragmentedReadsSyncSamples(t *testing.T) {
	_, file := writeTwoTrackFragments(t)

	for _, tc := range []struct {
		at    time.Duration
		index uint32
	}{
		{at: 0, index: 0},
		{at: 100 * time.Millisecond, index: 0},
		{at: 130 * time.Millisecond, index: 3},
		{at: time.Second, index: 3},
	} {
		sample, err := file.ReadSyncSample(tc.at)
		if err != nil {
			t.Fatalf("read the keyframe at %v: %v", tc.at, err)
		}
		if sample.Index != tc.index {
			t.Errorf("the keyframe at or before %v is sample %d, want %d", tc.at, sample.Index, tc.index)
		}
		if len(sample.Data) == 0 {
			t.Errorf("the keyframe at %v came back empty", tc.at)
		}
	}
}

func TestWriteFragmentedBrands(t *testing.T) {
	out, _ := writeTwoTrackFragments(t)

	if got, want := string(out[8:12]), "isom"; got != want {
		t.Errorf("major brand = %q, want %q", got, want)
	}
	if !bytes.Contains(out[:36], []byte(fragmentBrand)) {
		t.Errorf("the ftyp does not carry the %q brand", fragmentBrand)
	}
}

func TestWriteFragmentedRefusesWhatItCannotWrite(t *testing.T) {
	_, tracks, samples := twoTrackStream()

	for _, tc := range []struct {
		name   string
		target Target
		tracks []FragTrack
		want   error
	}{
		{name: "unknown target", target: Target("avi"), tracks: tracks, want: ErrUnsupportedTarget},
		{name: "no tracks", target: TargetMP4, want: ErrUnsupportedSource},
		{
			name: "codec the target refuses", target: Target3GP,
			tracks: []FragTrack{{ID: 1, Handler: "soun", Timescale: 48000, Codec: "opus"}},
			want:   ErrIncompatibleCodec,
		},
		{
			name: "codec with no sample entry", target: TargetMP4,
			tracks: []FragTrack{{ID: 1, Handler: "soun", Timescale: 1000, Codec: "fLaC"}},
			want:   ErrUnsupportedSource,
		},
		{
			name: "video with no configuration record", target: TargetMP4,
			tracks: []FragTrack{{ID: 1, Handler: "vide", Timescale: 1000, Codec: "h264"}},
			want:   ErrUnsupportedSource,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := WriteFragmented(bytes.NewBuffer(nil), bytes.NewReader(nil), tc.target,
				time.Second, tc.tracks, fragStream(samples))
			if err == nil {
				t.Fatalf("write succeeded, want %v", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestWriteFragmentedRefusesAnUnreachableDataOffset holds the one ceiling the
// trun imposes: its data offset is signed 32-bit, so a track whose run would
// begin more than 2 GiB past the moof cannot be described. Nothing is read, so
// the payload does not have to exist.
func TestWriteFragmentedRefusesAnUnreachableDataOffset(t *testing.T) {
	_, tracks, _ := twoTrackStream()
	samples := []FragSample{
		{Track: 1, Duration: 40, Sync: true, Fragment: true, Size: 3 << 30},
		{Track: 2, Duration: 20, Sync: true, Size: 1},
	}
	err := WriteFragmented(io.Discard, bytes.NewReader(nil), TargetMP4, time.Second, tracks, fragStream(samples))
	if !errors.Is(err, ErrUnsupportedSource) {
		t.Errorf("error = %v, want ErrUnsupportedSource", err)
	}
}

// TestWriteFragmentedOpusConfig holds the one audio record that is rebuilt
// rather than copied: Matroska keeps the identification header and this family
// keeps its body in a dOps, so a round trip through both has to come back with
// the same fields.
func TestWriteFragmentedOpusConfig(t *testing.T) {
	// "OpusHead", version 1, two channels, 312 samples of pre-skip, 48 kHz in,
	// no gain, mapping family 0.
	head := concat([]byte("OpusHead"), []byte{1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0})

	var out bytes.Buffer
	err := WriteFragmented(&out, bytes.NewReader(nil), TargetMP4, time.Second,
		[]FragTrack{{
			ID: 1, Handler: "soun", Timescale: 48000, Codec: "opus",
			Config: head, Channels: 2, SampleRate: 48000,
		}},
		fragStream(nil))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	file, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("parse the output: %v", err)
	}
	config := file.Tracks[0].Entry.Config
	want := []byte{0, 2, 0x01, 0x38, 0, 0, 0xbb, 0x80, 0, 0, 0}
	if !bytes.Equal(config, want) {
		t.Errorf("dOps = %v, want %v", config, want)
	}
	if got, want := file.Tracks[0].Entry.Format, "Opus"; got != want {
		t.Errorf("sample entry = %q, want %q", got, want)
	}
}

// TestWriteFragmentedSampleEntries walks every codec the writer can describe,
// since each one puts a different configuration box inside its entry and only
// two of them are exercised by the handcrafted stream above.
func TestWriteFragmentedSampleEntries(t *testing.T) {
	for _, tc := range []struct {
		codec, format string
		handler       string
		config        []byte
	}{
		{codec: "h264", format: "avc1", handler: "vide", config: []byte{1, 0x42, 0, 0x1e, 0xff}},
		{codec: "hevc", format: "hvc1", handler: "vide", config: make([]byte, 23)},
		{codec: "av1", format: "av01", handler: "vide", config: []byte{0x81, 0, 0, 0}},
		{codec: "vp9", format: "vp09", handler: "vide"},
		{codec: "aac", format: "mp4a", handler: "soun", config: []byte{0x12, 0x10}},
	} {
		t.Run(tc.codec, func(t *testing.T) {
			// A nonzero edit delay so the edit list goes out too: it is written
			// per track, and only a reordered stream asks for one.
			track := FragTrack{
				ID: 1, Handler: tc.handler, Timescale: 1000, Codec: tc.codec, Config: tc.config,
				Width: 128, Height: 72, Channels: 2, SampleRate: 48000, EditDelay: 80,
			}
			var out bytes.Buffer
			err := WriteFragmented(&out, bytes.NewReader(nil), TargetMP4, time.Second,
				[]FragTrack{track}, fragStream(nil))
			if err != nil {
				t.Fatalf("write: %v", err)
			}

			file, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
			if err != nil {
				t.Fatalf("parse the output: %v", err)
			}
			entry := file.Tracks[0].Entry
			if entry.Format != tc.format {
				t.Errorf("sample entry = %q, want %q", entry.Format, tc.format)
			}
			if entry.Codec != tc.codec {
				t.Errorf("codec = %q, want %q", entry.Codec, tc.codec)
			}
			// vp9 is the one codec whose record this writer invents, because
			// Matroska carries none to copy.
			// A vpcC is a version and flags word, six bytes of profile, level,
			// depth, and colour, and a two byte initialization data length.
			if tc.codec == "vp9" && len(entry.Config) != 12 {
				t.Errorf("vpcC is %d bytes, want the 12 byte minimum record", len(entry.Config))
			}
			edits := file.Tracks[0].Edits
			if len(edits) != 1 || edits[0].MediaTime != 80 || edits[0].Rate != 1 {
				t.Errorf("edit list = %+v, want one entry at media time 80 and rate 1", edits)
			}
			if got, want := file.Tracks[0].MovieTime(120, file.Timescale), 40*time.Millisecond; got != want {
				t.Errorf("a media time of 120 shows at %v, want %v", got, want)
			}
		})
	}
}

// TestWriteFragmentedTakesAPayloadWriter holds the one change an MPEG-TS source
// needed: a sample whose bytes are written by a function rather than copied
// out of a range. The reader holds nothing at all, so every byte of the output
// payload came through the functions.
func TestWriteFragmentedTakesAPayloadWriter(t *testing.T) {
	_, tracks, _ := twoTrackStream()
	bodies := [][]byte{[]byte("keyframe"), []byte("aac!"), []byte("delta")}
	samples := []FragSample{
		{Track: 1, Duration: 40, Sync: true, Fragment: true},
		{Track: 2, Duration: 20, Sync: true},
		{Track: 1, Decode: 40, Duration: 40},
	}
	for i := range samples {
		body := bodies[i]
		samples[i].Size = int64(len(body))
		samples[i].Offset = 1 << 40 // never read
		samples[i].Payload = func(w io.Writer) error {
			_, err := w.Write(body)
			return err
		}
	}

	var out bytes.Buffer
	if err := WriteFragmented(&out, bytes.NewReader(nil), TargetMP4, 80*time.Millisecond, tracks, fragStream(samples)); err != nil {
		t.Fatalf("write: %v", err)
	}
	file, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("parse the output: %v", err)
	}
	sample, err := file.ReadSyncSample(0)
	if err != nil {
		t.Fatalf("read the keyframe: %v", err)
	}
	if !bytes.Equal(sample.Data, bodies[0]) {
		t.Errorf("keyframe = %q, want %q", sample.Data, bodies[0])
	}
	// Video goes before audio within a fragment, so the audio frame follows
	// both video samples in the mdat.
	if !bytes.HasSuffix(out.Bytes(), []byte("keyframedeltaaac!")) {
		t.Errorf("the payload does not end with the three bodies in track order")
	}
}

func TestWriteFragmentedRefusesAPayloadOfTheWrongSize(t *testing.T) {
	_, tracks, _ := twoTrackStream()
	samples := []FragSample{{
		Track: 1, Duration: 40, Sync: true, Size: 4,
		Payload: func(w io.Writer) error {
			_, err := w.Write([]byte("too long"))
			return err
		},
	}}
	err := WriteFragmented(io.Discard, bytes.NewReader(nil), TargetMP4, time.Second, tracks[:1], fragStream(samples))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("error = %v, want ErrMalformed", err)
	}
}
