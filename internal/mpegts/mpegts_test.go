package mpegts

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

func TestParseReadsTheCorpus(t *testing.T) {
	for _, tc := range []struct {
		name, video string
		stride      int64
	}{
		{"h264-aac.ts", "h264", 188},
		{"h264-gop12.ts", "h264", 188},
		{"hevc-aac.ts", "hevc", 188},
		{"h264-aac.m2ts", "h264", 192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := parseCorpus(t, tc.name)
			if f.stride != tc.stride {
				t.Errorf("stride = %d, want %d", f.stride, tc.stride)
			}
			if f.Video == nil || f.Video.Codec != tc.video || f.Video.Width != 128 || f.Video.Height != 72 {
				t.Fatalf("video = %+v, want a 128x72 %s stream", f.Video, tc.video)
			}
			if f.Audio == nil || f.Audio.Codec != "aac" || f.Audio.SampleRate != 48000 || f.Audio.Channels != 1 {
				t.Fatalf("audio = %+v, want mono AAC at 48 kHz", f.Audio)
			}
			if !bytes.Equal(f.Audio.Config, []byte{0x11, 0x88}) {
				t.Errorf("AudioSpecificConfig = %x, want 1188", f.Audio.Config)
			}
			if f.Gaps != 0 {
				t.Errorf("gaps = %d, want 0", f.Gaps)
			}
			if got := f.FrameRate(); got != 24 {
				t.Errorf("frame rate = %v, want 24", got)
			}
		})
	}
}

// TestParseBuildsTheTwinsConfiguration holds the avcC built from the stream's
// parameter sets to the one the mp4 it was copied from stores: the same
// parameter sets in the same record, extension fields included.
func TestParseBuildsTheTwinsConfiguration(t *testing.T) {
	f, _ := parseCorpus(t, "h264-aac.ts")
	raw := readCorpus(t, "h264-aac.mp4")
	mp4, err := isobmff.Parse(bytesReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse the twin: %v", err)
	}
	if want := mp4.VideoTrack().Entry.Config; !bytes.Equal(f.Video.Config, want) {
		t.Errorf("avcC = %x\nwant   %x", f.Video.Config, want)
	}
}

func TestDetectStride(t *testing.T) {
	packets := func(stride, prefix int) []byte {
		out := make([]byte, 4*stride)
		for i := range 4 {
			out[i*stride+prefix] = syncByte
		}
		return out
	}
	for _, tc := range []struct {
		name string
		data []byte
		want int64
	}{
		{"188", packets(188, 0), 188},
		{"192", packets(192, 4), 192},
		{"204", packets(204, 0), 0},
		{"two packets", packets(188, 0)[:376], 0},
		{"text", []byte("definitely not a transport stream, not even close to one"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := detectStride(bytesReader(tc.data), int64(len(tc.data)))
			if tc.want == 0 {
				if !errors.Is(err, ErrNotTS) {
					t.Errorf("error = %v, want ErrNotTS", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("stride = %d, %v, want %d", got, err, tc.want)
			}
		})
	}
}

func TestUnwrapNear(t *testing.T) {
	const hour = 3600 * clockRate
	for _, tc := range []struct{ raw, ref, want int64 }{
		{100, 50, 100},
		{50, 100, 50},
		// Just past the wrap, a small raw value is a large unwrapped one.
		{10, timestampWrap - 10, timestampWrap + 10},
		// And a large raw value just before a small reference is behind it.
		{timestampWrap - 10, 10, -10},
		{hour, timestampWrap - hour, timestampWrap + hour},
	} {
		if got := unwrapNear(tc.raw, tc.ref); got != tc.want {
			t.Errorf("unwrapNear(%d, %d) = %d, want %d", tc.raw, tc.ref, got, tc.want)
		}
	}
}

func TestPESHeaderTimestamps(t *testing.T) {
	for _, pts := range []int64{0, 1, clockRate, timestampWrap - 1} {
		dts := (pts + timestampWrap - 3750) % timestampWrap
		header := append([]byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0xc0, 10}, timestampField(pts, 3)...)
		header = append(header, timestampField(dts, 1)...)
		h, err := parsePESHeader(header)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !h.hasPTS || !h.hasDTS || h.pts != pts || h.dts != dts || h.size != len(header) {
			t.Errorf("header = %+v, want PTS %d and DTS %d in %d bytes", h, pts, dts, len(header))
		}
	}

	for name, bad := range map[string][]byte{
		"no start code":      {0, 0, 2, 0xe0, 0, 0, 0x80, 0x80, 5, 0, 0, 0, 0, 0},
		"header runs over":   {0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 200, 0, 0},
		"PTS runs over":      {0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 2, 0x21, 0},
		"no optional fields": {0, 0, 1, 0xe0, 0, 0, 0x40},
	} {
		if _, err := parsePESHeader(bad); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: error = %v, want ErrMalformed", name, err)
		}
	}
}

// TestParseAcrossTheTimestampWrap moves a corpus file's timestamps so the 33
// bit clock rolls over half a second in, which a reader that did not unwrap
// would read as a file 26 hours long or of negative length.
func TestParseAcrossTheTimestampWrap(t *testing.T) {
	want, raw := parseCorpus(t, "h264-aac.ts")
	shifted := bytes.Clone(raw)
	shiftTimestamps(shifted, timestampWrap-want.origin-clockRate/2)

	f, err := Parse(bytesReader(shifted), int64(len(shifted)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Duration() != want.Duration() || f.FrameRate() != want.FrameRate() {
		t.Errorf("duration %v at %v fps, want %v at %v", f.Duration(), f.FrameRate(), want.Duration(), want.FrameRate())
	}
	sample, err := f.ReadNearestSyncSample(0)
	if err != nil {
		t.Fatalf("read the keyframe: %v", err)
	}
	if sample.Time > 50*time.Millisecond {
		t.Errorf("the first keyframe is at %v, want within the priming delay of zero", sample.Time)
	}

	var out bytes.Buffer
	if err := WriteFragmented(&out, f, isobmff.TargetMP4); err != nil {
		t.Fatalf("remux: %v", err)
	}
	mp4, err := isobmff.Parse(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatalf("parse the remux: %v", err)
	}
	if got := mp4.VideoTrack().SampleCount(); got != 48 {
		t.Errorf("the remux holds %d video samples, want 48", got)
	}
	if got := mp4.VideoTrack().FrameRate(); got < 23.99 || got > 24.01 {
		t.Errorf("the remux runs at %v fps, want 24", got)
	}
}

func TestParseCountsContinuityGaps(t *testing.T) {
	raw := readCorpus(t, "h264-aac.ts")
	// Drop the fortieth video packet, which is well past the parameter sets
	// and is not the first of a PES.
	var out []byte
	seen := 0
	eachPacket(raw, func(pkt []byte) {
		if packetPID(pkt) == 0x100 {
			if seen++; seen == 40 {
				return
			}
		}
		out = append(out, pkt...)
	})
	f, err := Parse(bytesReader(out), int64(len(out)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Gaps != 1 {
		t.Errorf("gaps = %d, want 1", f.Gaps)
	}
}

func TestAnnexBSplitsAcrossAnyBoundary(t *testing.T) {
	// A four byte start code, a unit with a zero in it and trailing zeros, a
	// three byte start code, and a unit that ends the stream.
	stream := []byte{0, 0, 0, 1, 0x67, 1, 0, 2, 0, 0, 0, 0, 1, 0x68, 3, 0, 0}
	want := [][]byte{{0x67, 1, 0, 2}, {0x68, 3}}

	for cut := range len(stream) + 1 {
		var (
			units [][]byte
			a     annexB
		)
		data := func(b []byte) error {
			units[len(units)-1] = append(units[len(units)-1], b...)
			return nil
		}
		open := func() error {
			units = append(units, nil)
			return nil
		}
		for _, part := range [][]byte{stream[:cut], stream[cut:]} {
			if err := a.feed(part, data, open); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.EqualFunc(units, want, bytes.Equal) {
			t.Errorf("cut at %d: units = %x, want %x", cut, units, want)
		}
	}
}

func TestToLengthPrefixedDropsWhatTheRecordHolds(t *testing.T) {
	sps := []byte{0x67, 0x64, 0, 0x0a}
	pps := []byte{0x68, 0xeb}
	other := []byte{0x68, 0xee}
	slice := []byte{0x65, 0x88, 0x84}
	var au []byte
	for _, nal := range [][]byte{{h264AUD, 0xf0}, sps, pps, other, slice} {
		au = append(append(au, 0, 0, 0, 1), nal...)
	}

	got, err := toLengthPrefixed(au, false, [][]byte{sps, pps})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0, 0, 0, 2}, other...)
	want = append(append(want, 0, 0, 0, 3), slice...)
	if !bytes.Equal(got, want) {
		t.Errorf("length-prefixed = %x, want %x", got, want)
	}
}

func TestToLengthPrefixedCapsTheUnitCount(t *testing.T) {
	au := bytes.Repeat([]byte{0, 0, 1, 0x41}, maxAUUnits+1)
	if _, err := toLengthPrefixed(au, false, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("error = %v, want ErrMalformed", err)
	}
}

func TestADTSRoundTrip(t *testing.T) {
	h, err := adtsFromConfig([]byte{0x11, 0x88})
	if err != nil {
		t.Fatal(err)
	}
	if h.profile != 1 || adtsSampleRates[h.rate] != 48000 || h.channels != 1 {
		t.Fatalf("header = %+v, want AAC LC at 48 kHz, mono", h)
	}
	if got := h.audioSpecificConfig(); !bytes.Equal(got, []byte{0x11, 0x88}) {
		t.Errorf("AudioSpecificConfig = %x, want 1188", got)
	}
	for name, asc := range map[string][]byte{
		"HE-AAC":        {0x2b, 0x88},
		"explicit rate": {0x17, 0x80},
		"too short":     {0x11},
	} {
		if _, err := adtsFromConfig(asc); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: error = %v, want ErrMalformed", name, err)
		}
	}
}

// TestReadSyncSampleStepsBack puts the keyframes all at the front of a file
// padded out with null packets, so the proportional estimate for a late time
// lands in padding and the search has to step back to find them.
func TestReadSyncSampleStepsBack(t *testing.T) {
	raw := readCorpus(t, "h264-gop12.ts")
	null := make([]byte, packetSize)
	null[0], null[1], null[2], null[3] = syncByte, 0x1f, 0xff, 0x10
	padded := append(bytes.Clone(raw), bytes.Repeat(null, (3<<20)/packetSize)...)
	f, err := Parse(bytesReader(padded), int64(len(padded)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for _, tc := range []struct {
		at, before, nearest time.Duration
	}{
		{1300 * time.Millisecond, time.Second, 1500 * time.Millisecond},
		{2200 * time.Millisecond, 2 * time.Second, 2 * time.Second},
		{time.Hour, 2500 * time.Millisecond, 2500 * time.Millisecond},
	} {
		// The stream's timeline starts at the audio, a priming delay before
		// the video's first frame.
		const priming = 21333 * time.Microsecond
		sample, err := f.ReadSyncSample(tc.at)
		if err != nil {
			t.Fatalf("at or before %v: %v", tc.at, err)
		}
		if diff := sample.Time - tc.before - priming; diff < -time.Millisecond || diff > time.Millisecond {
			t.Errorf("at or before %v: keyframe at %v, want %v", tc.at, sample.Time, tc.before+priming)
		}
		nearest, err := f.ReadNearestSyncSample(tc.at)
		if err != nil {
			t.Fatalf("nearest %v: %v", tc.at, err)
		}
		if diff := nearest.Time - tc.nearest - priming; diff < -time.Millisecond || diff > time.Millisecond {
			t.Errorf("nearest %v: keyframe at %v, want %v", tc.at, nearest.Time, tc.nearest+priming)
		}
	}
}

func TestReadSyncSampleOfAnUnreadableCodec(t *testing.T) {
	f, _ := parseCorpus(t, "h264-aac.ts")
	f.Video.Codec = "mpeg2video"
	sample, err := f.ReadNearestSyncSample(0)
	if err != nil || sample.Codec != "mpeg2video" || sample.Data != nil {
		t.Errorf("sample = %+v, %v, want the codec named and no data", sample, err)
	}
}

// TestWriteProducesAWellFormedStream reads the muxer's output back packet by
// packet: whole packets, continuity counters that never skip, tables whose
// CRC holds, and a PES header in front of every sample.
func TestWriteProducesAWellFormedStream(t *testing.T) {
	raw := readCorpus(t, "h264-gop12.mp4")
	src, err := isobmff.Parse(bytesReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out bytes.Buffer
	if err := Write(&out, src, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	ts := out.Bytes()
	if len(ts)%packetSize != 0 {
		t.Fatalf("the output is %d bytes, not whole packets", len(ts))
	}

	cc := map[uint16]byte{}
	starts := map[uint16]int{}
	eachPacket(ts, func(pkt []byte) {
		pid := packetPID(pkt)
		if pkt[0] != syncByte {
			t.Fatalf("a packet with no sync byte")
		}
		if last, ok := cc[pid]; ok && pkt[3]&0x0f != (last+1)&0x0f {
			t.Errorf("PID %#x skips from counter %d to %d", pid, last, pkt[3]&0x0f)
		}
		cc[pid] = pkt[3] & 0x0f
		if pkt[1]&0x40 == 0 {
			return
		}
		starts[pid]++
		switch pid {
		case patPID, outPMTPID:
			payload := payloadOf(pkt)
			sec := payload[1+int(payload[0]):]
			length := 3 + int(sec[1]&0x0f)<<8 + int(sec[2])
			if crc32MPEG(sec[:length]) != 0 {
				t.Errorf("the table on PID %#x fails its CRC", pid)
			}
		default:
			if _, err := parsePESHeader(payloadOf(pkt)); err != nil {
				t.Errorf("PID %#x: %v", pid, err)
			}
		}
	})
	video, audio := src.VideoTrack().SampleCount(), src.AudioTrack().SampleCount()
	if starts[outFirstPID] != int(video) || starts[outFirstPID+1] != int(audio) {
		t.Errorf("PES starts = %v, want %d video and %d audio", starts, video, audio)
	}
	// One copy of the tables up front and one before each keyframe half a
	// second on: at 0, 0.5, 1.0, 1.5, 2.0, and 2.5 seconds.
	if starts[patPID] != 6 || starts[outPMTPID] != 6 {
		t.Errorf("the tables went out %d and %d times, want 6", starts[patPID], starts[outPMTPID])
	}
}

func TestWriteReportsAWriteFailure(t *testing.T) {
	raw := readCorpus(t, "h264-aac.mp4")
	src, err := isobmff.Parse(bytesReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	failure := errors.New("disk full")
	if err := Write(failingWriter{failure}, src, nil); !errors.Is(err, failure) {
		t.Errorf("error = %v, want the writer's own", err)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestWriteFragmentedReportsATruncatedSource(t *testing.T) {
	f, raw := parseCorpus(t, "h264-aac.ts")
	// Parse again over a reader that ends early, as a file cut after it was
	// probed would.
	cut := raw[:len(raw)/2]
	f.r, f.size = bytesReader(cut), int64(len(raw))
	err := WriteFragmented(io.Discard, f, isobmff.TargetMP4)
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("error = %v, want ErrTruncated", err)
	}
}

// TestInspectNamesWhatItCannotCarry covers the streams no corpus file holds:
// the ones this package names from their first bytes and does not remux.
func TestInspectNamesWhatItCannotCarry(t *testing.T) {
	for _, tc := range []struct {
		codec, want   string
		head          []byte
		width, height int
	}{
		{"mpeg2video", "mpeg2video", []byte{0, 0, 1, 0xb3, 0x2d, 0x02, 0x40, 0x33}, 720, 576},
		{"mp3", "mp3", []byte{0xff, 0xfb, 0x90, 0x64}, 0, 0},
		{"mp3", "mp2", []byte{0xff, 0xfd, 0x90, 0x64}, 0, 0},
		{"mp3", "mp1", []byte{0xff, 0xff, 0x90, 0x64}, 0, 0},
		{"private", "aac", []byte{0xff, 0xf1, 0x4c, 0x40, 0x10, 0x1f, 0xfc}, 0, 0},
		{"private", "ac-3", []byte{0x0b, 0x77, 0, 0, 0, 0x40}, 0, 0},
		{"private", "ec-3", []byte{0x0b, 0x77, 0, 0, 0, 0x80}, 0, 0},
		{"ac-3", "ac-3", nil, 0, 0},
	} {
		s := &Stream{Codec: tc.codec, head: tc.head}
		s.inspect()
		if !s.configured || s.Codec != tc.want || s.Width != tc.width || s.Height != tc.height {
			t.Errorf("%s %x: stream = %s %dx%d configured %v, want %s %dx%d",
				tc.codec, tc.head, s.Codec, s.Width, s.Height, s.configured, tc.want, tc.width, tc.height)
		}
	}

	// A private stream whose first frame is nothing this package knows stays
	// unconfigured, which is what drops it.
	s := &Stream{Codec: "private", head: []byte("subtitles")}
	if s.inspect(); s.configured {
		t.Errorf("an unknown private stream was configured as %s", s.Codec)
	}
}

func TestCodecOfReadsDescriptors(t *testing.T) {
	for _, tc := range []struct {
		streamType  byte
		descriptors []byte
		codec       string
		kind        int
	}{
		{streamH264, nil, "h264", kindVideo},
		{streamLATM, nil, "aac_latm", kindAudio},
		{streamPrivate, []byte{descAC3, 1, 0}, "ac-3", kindAudio},
		{streamPrivate, []byte{descEAC3, 0}, "ec-3", kindAudio},
		{streamPrivate, []byte{descRegistration, 4, 'E', 'A', 'C', '3'}, "ec-3", kindAudio},
		{streamPrivate, []byte{0x56, 5, 'e', 'n', 'g', 0, 0}, "", kindOther}, // teletext
		{streamPrivate, []byte{descAC3, 9}, "", kindOther},                   // runs over
		{0xea, nil, "", kindOther},                                           // VC-1
	} {
		codec, kind := codecOf(tc.streamType, tc.descriptors)
		if codec != tc.codec || kind != tc.kind {
			t.Errorf("stream type %#x %x = %q %d, want %q %d", tc.streamType, tc.descriptors, codec, kind, tc.codec, tc.kind)
		}
	}
}
