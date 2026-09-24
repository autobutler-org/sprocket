//go:build perf

package perf

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Every input is the same synthetic content: a test pattern at 24 fps and a
// 440 Hz tone, twelve seconds long, with a keyframe every two seconds and none
// anywhere else.
const (
	clipSeconds = 12
	fps         = 24
	gop         = 2 * fps
)

// The times the cases ask for. midpoint is a keyframe, so ffmpeg, which decodes
// forward to the exact time, and sprocket, which snaps to the nearest keyframe,
// return the same frame.
const (
	midpoint  = clipSeconds / 2 * time.Second
	trimStart = 2 * time.Second
	trimEnd   = 8 * time.Second
)

// input is one file generated before the cases run.
type input struct {
	name string
	// encoders are the ffmpeg encoders the file needs. A build without one of
	// them skips the file and every case that reads it.
	encoders []string
	// size is the frame size of a file encoded from the synthetic source.
	size string
	// codec is the encoding arguments for a file encoded from the source.
	codec []string
	// from names the input this one is copied from, changing only the
	// container. It is set instead of size and codec.
	from string
}

var (
	h264Codec = []string{"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "128k"}
	// open-gop=0 makes every keyframe an IDR, as the other encoders' are.
	hevcCodec   = []string{"-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1", "-x265-params", "log-level=error:scenecut=0:open-gop=0", "-c:a", "aac", "-b:a", "128k"}
	hevc8Codec  = append([]string{"-profile:v", "main", "-pix_fmt", "yuv420p"}, hevcCodec...)
	hevc10Codec = append([]string{"-profile:v", "main10", "-pix_fmt", "yuv420p10le"}, hevcCodec...)
	av1Codec    = []string{"-c:v", "libsvtav1", "-preset", "12", "-pix_fmt", "yuv420p", "-c:a", "libopus", "-b:a", "96k"}
	// ffmpeg's own Vorbis encoder is in every build, where libvorbis is not.
	// It is experimental and takes only stereo.
	vp8Codec = []string{"-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "8M", "-c:a", "vorbis", "-ac", "2", "-strict", "experimental"}
)

// inputs lists the files the cases read, in the order they are generated: a
// file copied from another comes after it. 4K is generated only for the
// thumbnail cases and one remux, since it is the slow part of generation.
func inputs() []input {
	const (
		hd    = "1920x1080"
		uhd   = "3840x2160"
		x264  = "libx264"
		x265  = "libx265"
		svt   = "libsvtav1"
		libvp = "libvpx"
	)
	h264 := []string{x264}
	hevc := []string{x265}
	av1 := []string{svt, "libopus"}
	vp8 := []string{libvp}
	return []input{
		{name: "h264-1080p.mp4", encoders: h264, size: hd, codec: h264Codec},
		{name: "h264-1080p.mov", encoders: h264, from: "h264-1080p.mp4"},
		{name: "h264-1080p.mkv", encoders: h264, from: "h264-1080p.mp4"},
		{name: "h264-1080p.ts", encoders: h264, from: "h264-1080p.mp4"},
		{name: "hevc8-1080p.mp4", encoders: hevc, size: hd, codec: hevc8Codec},
		{name: "hevc10-1080p.mp4", encoders: hevc, size: hd, codec: hevc10Codec},
		{name: "av1-1080p.webm", encoders: av1, size: hd, codec: av1Codec},
		{name: "vp8-1080p.webm", encoders: vp8, size: hd, codec: vp8Codec},
		{name: "h264-2160p.mp4", encoders: h264, size: uhd, codec: h264Codec},
		{name: "hevc8-2160p.mp4", encoders: hevc, size: uhd, codec: hevc8Codec},
		{name: "hevc10-2160p.mp4", encoders: hevc, size: uhd, codec: hevc10Codec},
		{name: "av1-2160p.webm", encoders: av1, size: uhd, codec: av1Codec},
		{name: "vp8-2160p.webm", encoders: vp8, size: uhd, codec: vp8Codec},
	}
}

// generate writes every input into dir and returns why each one it could not
// write was skipped, keyed by file name. A failed encode with every encoder
// present is a broken case, not a missing feature, and fails the test.
func generate(t *testing.T, dir string) map[string]string {
	t.Helper()

	listing, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		t.Fatalf("list ffmpeg encoders: %v", err)
	}
	available := map[string]bool{}
	for line := range strings.Lines(string(listing)) {
		if fields := strings.Fields(line); len(fields) > 1 {
			available[fields[1]] = true
		}
	}

	skipped := map[string]string{}
	for _, in := range inputs() {
		if reason, ok := skipped[in.from]; ok {
			skipped[in.name] = reason
			continue
		}
		missing := ""
		for _, encoder := range in.encoders {
			if !available[encoder] {
				missing = encoder
				break
			}
		}
		if missing != "" {
			skipped[in.name] = "skipped: this ffmpeg has no " + missing
			continue
		}

		args := []string{"-y", "-hide_banner", "-loglevel", "error"}
		if in.from != "" {
			args = append(args, "-i", filepath.Join(dir, in.from), "-c", "copy", "-map", "0")
		} else {
			args = append(args,
				"-f", "lavfi", "-i", "testsrc2=size="+in.size+":rate="+strconv.Itoa(fps),
				"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
				"-t", strconv.Itoa(clipSeconds),
				"-g", strconv.Itoa(gop), "-keyint_min", strconv.Itoa(gop), "-sc_threshold", "0")
			args = append(args, in.codec...)
		}
		args = append(args, filepath.Join(dir, in.name))

		start := time.Now()
		var stderr bytes.Buffer
		cmd := exec.Command("ffmpeg", args...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("generate %s: %v\n%s", in.name, err, stderr.String())
		}
		t.Logf("generated %s in %s", in.name, time.Since(start).Round(time.Millisecond))
	}
	return skipped
}

// ffmpegVersion is the version ffmpeg reports, such as 6.1.1-3ubuntu5.
func ffmpegVersion(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("ffmpeg", "-version").Output()
	if err != nil {
		t.Fatalf("ffmpeg -version: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return fmt.Sprintf("unknown (%q)", out)
	}
	return fields[2]
}
