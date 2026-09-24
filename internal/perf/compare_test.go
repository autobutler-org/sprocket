//go:build perf

// Package perf times sprocket against ffmpeg doing the same work on the same
// file. It is not part of the normal test run: it needs ffmpeg and ffprobe on
// PATH, takes minutes, and reports rather than asserts. make test/perf runs it.
//
// Both sides run as fresh processes, sprocket as a child of this test binary,
// so the wall times and peak memory are measured the same way and both
// include process startup.
package perf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/pkg/sprocket"
)

// childEnv carries a job to the child process that runs it.
const childEnv = "SPROCKET_PERF_JOB"

const thumbnailCap = 320

// The operations a job runs.
const (
	opProbe     = "probe"
	opThumbnail = "thumbnail"
	opRemux     = "remux"
	opTrim      = "trim"
)

// job is one sprocket operation, run in a child process.
type job struct {
	Op     string             `json:"op"`
	In     string             `json:"in"`
	Out    string             `json:"out,omitempty"`
	Target sprocket.Container `json:"target,omitempty"`
	// At is the thumbnail time, or the start of a trim.
	At     time.Duration `json:"at,omitempty"`
	End    time.Duration `json:"end,omitempty"`
	MaxDim int           `json:"maxDim,omitempty"`
	// Width and Height are the thumbnail's expected size.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// row is one line of the comparison: a sprocket job and the ffmpeg command that
// does the same thing.
type row struct {
	operation string
	input     string
	// source is the generated file both sides read.
	source string
	job    job
	ffmpeg []string
	// ffOut is the file ffmpeg writes. Empty means it writes to stdout.
	ffOut string
	// ffBytes is the exact stdout size a raw frame has. Zero asks only for
	// some output.
	ffBytes int
	// unsupported says why sprocket does not do this, when it does not.
	unsupported string
}

// result is one row's measurements, as written to results.json.
type result struct {
	Operation   string `json:"operation"`
	Input       string `json:"input"`
	SprocketNS  int64  `json:"sprocketMedianNs,omitempty"`
	FFmpegNS    int64  `json:"ffmpegMedianNs,omitempty"`
	SprocketRSS int64  `json:"sprocketPeakRssBytes,omitempty"`
	FFmpegRSS   int64  `json:"ffmpegPeakRssBytes,omitempty"`
	Note        string `json:"note,omitempty"`
}

func TestCompare(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; install ffmpeg to run the comparison", tool)
		}
	}
	iterations := 5
	if v := os.Getenv("PERF_ITERATIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			t.Fatalf("PERF_ITERATIONS=%q: want a positive whole number", v)
		}
		iterations = n
	}

	start := time.Now()
	media := t.TempDir()
	skipped := generate(t, media)
	t.Logf("generated inputs in %s", time.Since(start).Round(time.Second))

	out := t.TempDir()
	results := make([]result, 0, 32)
	for _, r := range rows(media, out) {
		res := result{Operation: r.operation, Input: r.input}
		if reason, ok := skipped[r.source]; ok {
			res.Note = reason
			results = append(results, res)
			continue
		}
		var err error
		res.FFmpegNS, res.FFmpegRSS, err = measure(iterations, func() *exec.Cmd { return exec.Command(r.ffmpeg[0], r.ffmpeg[1:]...) }, r.checkFFmpeg)
		if err != nil {
			t.Errorf("%s %s: ffmpeg: %v", r.operation, r.input, err)
			res.Note = "invalid ffmpeg output"
		}
		switch {
		case r.unsupported != "":
			res.Note = r.unsupported
		case res.Note == "":
			j := r.job
			if j.Out != "" {
				j.Out = filepath.Join(out, "sprocket-"+j.Out)
			}
			res.SprocketNS, res.SprocketRSS, err = measure(iterations, func() *exec.Cmd { return child(j) }, j.check)
			if err != nil {
				t.Errorf("%s %s: sprocket: %v", r.operation, r.input, err)
				res.Note = "invalid sprocket output"
			}
		}
		t.Logf("%s %s: sprocket %s, ffmpeg %s %s", res.Operation, res.Input,
			time.Duration(res.SprocketNS), time.Duration(res.FFmpegNS), res.Note)
		results = append(results, res)
	}

	summary := report(ffmpegVersion(t), iterations, results)
	fmt.Print(summary)
	if err := publish(summary, results); err != nil {
		t.Error(err)
	}
	t.Logf("comparison took %s", time.Since(start).Round(time.Second))
}

// TestSprocketChild runs the job in childEnv. TestCompare starts it in a child
// process so sprocket's peak memory is measured the way ffmpeg's is.
func TestSprocketChild(t *testing.T) {
	spec := os.Getenv(childEnv)
	if spec == "" {
		t.Skip("run by TestCompare in a child process")
	}
	var j job
	if err := json.Unmarshal([]byte(spec), &j); err != nil {
		t.Fatalf("decode job: %v", err)
	}
	if err := j.run(); err != nil {
		t.Fatal(err)
	}
}

// rows lists every case. Paths to inputs are under media, and ffmpeg's output
// files are under out.
func rows(media, out string) []row {
	in := func(name string) string { return filepath.Join(media, name) }
	ff := func(args ...string) []string {
		return append([]string{"ffmpeg", "-y", "-hide_banner", "-loglevel", "error"}, args...)
	}
	secs := func(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }

	var all []row
	for _, c := range []struct{ label, name string }{
		{"mp4", "h264-1080p.mp4"},
		{"mov", "h264-1080p.mov"},
		{"mkv", "h264-1080p.mkv"},
		{"webm", "av1-1080p.webm"},
		{"ts", "h264-1080p.ts"},
	} {
		all = append(all, row{
			operation: "Probe", input: c.label, source: c.name,
			job:    job{Op: opProbe, In: in(c.name)},
			ffmpeg: []string{"ffprobe", "-v", "error", "-show_format", "-show_streams", in(c.name)},
		})
	}

	codecs := []struct{ label, prefix, ext string }{
		{"H.264", "h264", "mp4"},
		{"HEVC 8-bit", "hevc8", "mp4"},
		{"HEVC 10-bit", "hevc10", "mp4"},
		{"AV1", "av1", "webm"},
		{"VP8", "vp8", "webm"},
	}
	thumbnail := func(operation, label, name string, width, height, maxDim int, filter ...string) row {
		args := append([]string{"-ss", secs(midpoint), "-i", in(name), "-frames:v", "1"}, filter...)
		return row{
			operation: operation, input: label, source: name,
			job:     job{Op: opThumbnail, In: in(name), At: midpoint, MaxDim: maxDim, Width: width, Height: height},
			ffmpeg:  ff(append(args, "-f", "rawvideo", "-pix_fmt", "rgb24", "-")...),
			ffBytes: width * height * 3,
		}
	}
	for _, res := range []struct {
		label         string
		width, height int
	}{{"1080p", 1920, 1080}, {"2160p", 3840, 2160}} {
		for _, c := range codecs {
			name := c.prefix + "-" + res.label + "." + c.ext
			all = append(all, thumbnail("Thumbnail", c.label+" "+res.label, name, res.width, res.height, 0))
		}
	}
	// The scaled thumbnails are at 4K, where scaling down costs the most.
	for _, c := range codecs {
		name := c.prefix + "-2160p." + c.ext
		all = append(all, thumbnail("Thumbnail, max "+strconv.Itoa(thumbnailCap)+" px", c.label+" 2160p", name,
			thumbnailCap, thumbnailCap*9/16, thumbnailCap, "-vf", "scale="+strconv.Itoa(thumbnailCap)+":-2"))
	}

	for i, c := range []struct {
		label, name string
		target      sprocket.Container
	}{
		{"mov → mp4", "h264-1080p.mov", sprocket.MP4},
		{"mp4 → mkv", "h264-1080p.mp4", sprocket.MKV},
		{"mkv → mp4", "h264-1080p.mkv", sprocket.MP4},
		{"webm → mp4", "av1-1080p.webm", sprocket.MP4},
		{"mp4 → ts", "h264-1080p.mp4", sprocket.TS},
		{"ts → mp4", "h264-1080p.ts", sprocket.MP4},
		{"mp4 → mkv, 2160p", "h264-2160p.mp4", sprocket.MKV},
	} {
		outName := "remux-" + strconv.Itoa(i) + "." + string(c.target)
		all = append(all, row{
			operation: "Remux", input: c.label, source: c.name,
			job:    job{Op: opRemux, In: in(c.name), Out: outName, Target: c.target},
			ffmpeg: ff("-i", in(c.name), "-c", "copy", "-map", "0", filepath.Join(out, "ffmpeg-"+outName)),
			ffOut:  filepath.Join(out, "ffmpeg-"+outName),
		})
	}

	for _, c := range []struct {
		label, name string
		target      sprocket.Container
		unsupported string
	}{
		{"mp4", "h264-1080p.mp4", sprocket.MP4, ""},
		{"mkv", "h264-1080p.mkv", sprocket.MKV, ""},
		{"ts", "h264-1080p.ts", sprocket.TS, "unsupported"},
	} {
		outName := "trim." + string(c.target)
		all = append(all, row{
			operation: "Trim", input: c.label, source: c.name,
			job: job{Op: opTrim, In: in(c.name), Out: outName, Target: c.target, At: trimStart, End: trimEnd},
			ffmpeg: ff("-ss", secs(trimStart), "-to", secs(trimEnd), "-i", in(c.name),
				"-c", "copy", "-map", "0", filepath.Join(out, "ffmpeg-"+outName)),
			ffOut:       filepath.Join(out, "ffmpeg-"+outName),
			unsupported: c.unsupported,
		})
	}
	return all
}

// measure runs a command once to warm up and check its output, then iterations
// more times, and returns the median wall time in nanoseconds and the highest
// peak RSS in bytes.
func measure(iterations int, command func() *exec.Cmd, check func(stdout int) error) (int64, int64, error) {
	stdout, _, _, err := run(command())
	if err != nil {
		return 0, 0, err
	}
	if err := check(stdout); err != nil {
		return 0, 0, err
	}

	times := make([]int64, 0, iterations)
	var peak int64
	for range iterations {
		_, elapsed, rss, err := run(command())
		if err != nil {
			return 0, 0, err
		}
		times = append(times, elapsed.Nanoseconds())
		peak = max(peak, rss)
	}
	slices.Sort(times)
	return times[len(times)/2], peak, nil
}

// counter is a writer that keeps only the number of bytes written to it.
type counter int

func (c *counter) Write(p []byte) (int, error) {
	*c += counter(len(p))
	return len(p), nil
}

// run runs cmd and returns how many bytes it wrote to stdout, its wall time,
// and its peak RSS in bytes.
func run(cmd *exec.Cmd) (int, time.Duration, int64, error) {
	var stdout counter
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s: %w\n%s", strings.Join(cmd.Args, " "), err, stderr.String())
	}
	return int(stdout), elapsed, peakRSS(cmd.ProcessState), nil
}

// peakRSS is a finished process's peak resident set size in bytes. getrusage
// reports ru_maxrss in bytes on macOS and in kibibytes on Linux.
func peakRSS(state *os.ProcessState) int64 {
	usage, ok := state.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return usage.Maxrss
	}
	return usage.Maxrss * 1024
}

// child is a command that runs j in a fresh copy of this test binary. Its
// output goes to stdout, which run discards, and a failure shows on stderr.
func child(j job) *exec.Cmd {
	spec, _ := json.Marshal(j) // A struct of strings and numbers always encodes.
	cmd := exec.Command(os.Args[0], "-test.run=^TestSprocketChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"="+string(spec))
	return cmd
}

// checkFFmpeg checks what ffmpeg or ffprobe produced: a raw frame of the exact
// size, an output file with something in it, or some text on stdout.
func (r row) checkFFmpeg(stdout int) error {
	switch {
	case r.ffBytes != 0 && stdout != r.ffBytes:
		return fmt.Errorf("wrote %d bytes of raw frame, want %d", stdout, r.ffBytes)
	case r.ffOut != "":
		info, err := os.Stat(r.ffOut)
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return errors.New(r.ffOut + " is empty")
		}
	case stdout == 0:
		return errors.New("wrote nothing")
	}
	return nil
}

// check checks what a sprocket job produced. The child checks probes and
// thumbnails itself and fails if they are wrong; a remux or trim output has to
// probe back.
func (j job) check(int) error {
	if j.Out == "" {
		return nil
	}
	info, err := probeFile(j.Out)
	if err != nil {
		return fmt.Errorf("probe output: %w", err)
	}
	if info.Width == 0 || info.Duration == 0 {
		return fmt.Errorf("output probed as %+v", info)
	}
	return nil
}

func probeFile(path string) (sprocket.Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return sprocket.Info{}, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return sprocket.Info{}, err
	}
	return sprocket.Probe(f, stat.Size())
}

// run does the job's operation and checks what it can without a second file.
func (j job) run() error {
	f, err := os.Open(j.In)
	if err != nil {
		return err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()

	switch j.Op {
	case opProbe:
		info, err := sprocket.Probe(f, size)
		if err != nil {
			return err
		}
		if info.Width == 0 || info.Duration == 0 {
			return fmt.Errorf("probed as %+v", info)
		}
		return nil
	case opThumbnail:
		frame, err := sprocket.Thumbnail(f, size, j.At, sprocket.ThumbnailOptions{MaxDimension: j.MaxDim})
		if err != nil {
			return err
		}
		if b := frame.Image.Bounds(); b.Dx() != j.Width || b.Dy() != j.Height {
			return fmt.Errorf("thumbnail is %dx%d, want %dx%d", b.Dx(), b.Dy(), j.Width, j.Height)
		}
		return nil
	case opRemux, opTrim:
		out, err := os.Create(j.Out)
		if err != nil {
			return err
		}
		if j.Op == opRemux {
			err = sprocket.Remux(f, size, out, j.Target)
		} else {
			_, err = sprocket.Trim(f, size, out, j.Target, j.At, j.End)
		}
		return errors.Join(err, out.Close())
	}
	return fmt.Errorf("unknown operation %q", j.Op)
}

// report renders the results as a markdown table under a line saying what they
// were measured with.
func report(ffmpeg string, iterations int, results []result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## sprocket vs ffmpeg\n\n")
	fmt.Fprintf(&b, "ffmpeg %s, %s, %s/%s. Median of %d runs after one warmup.\n\n",
		ffmpeg, runtime.Version(), runtime.GOOS, runtime.GOARCH, iterations)
	b.WriteString("Both sides are timed as fresh processes, so every time includes process startup: " +
		"for ffmpeg that is what a caller pays to shell out, for sprocket it is a Go test binary starting. " +
		"Peak RSS is the whole process's.\n\n")
	b.WriteString("| Operation | Input | sprocket | ffmpeg | ffmpeg / sprocket | sprocket peak RSS | ffmpeg peak RSS |\n")
	b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, r := range results {
		sprocketTime, ratio := millis(r.SprocketNS), "–"
		switch {
		case r.SprocketNS == 0 && r.Note != "":
			sprocketTime = r.Note
		case r.FFmpegNS > 0:
			ratio = speedup(float64(r.FFmpegNS) / float64(r.SprocketNS))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Operation, r.Input,
			sprocketTime, millis(r.FFmpegNS), ratio, mebibytes(r.SprocketRSS), mebibytes(r.FFmpegRSS))
	}
	return b.String()
}

// speedup words a ratio of ffmpeg's time to sprocket's.
func speedup(ratio float64) string {
	if ratio >= 1 {
		return fmt.Sprintf("%.1fx faster", ratio)
	}
	return fmt.Sprintf("%.1fx slower", 1/ratio)
}

func millis(ns int64) string {
	if ns == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f ms", float64(ns)/float64(time.Millisecond))
}

func mebibytes(n int64) string {
	if n == 0 {
		return "–"
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// publish writes the summary and the raw results where CI picks them up: the
// workflow's step summary when GITHUB_STEP_SUMMARY is set, and summary.md and
// results.json in PERF_OUT when that is set.
func publish(summary string, results []result) error {
	if path := os.Getenv("GITHUB_STEP_SUMMARY"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, err = f.WriteString(summary)
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}

	dir := os.Getenv("PERF_OUT")
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return err
	}
	return errors.Join(
		os.WriteFile(filepath.Join(dir, "summary.md"), []byte(summary), 0o644),
		os.WriteFile(filepath.Join(dir, "results.json"), append(raw, '\n'), 0o644),
	)
}
