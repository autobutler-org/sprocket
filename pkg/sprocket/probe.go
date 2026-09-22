package sprocket

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// Sentinel errors. Every error this package returns wraps one of these, so a
// caller can tell a file this library does not read from one it cannot.
var (
	// ErrUnsupportedContainer means the input is not a container this library
	// reads. A file that is not media at all reports this.
	ErrUnsupportedContainer = errors.New("sprocket: unsupported container")
	// ErrCorrupt means the container was recognized but is malformed or
	// truncated where the headers live.
	ErrCorrupt = errors.New("sprocket: corrupt file")
)

// Info is what Probe reports about a file. Every zero value is meaningful.
type Info struct {
	// Duration is the movie duration, which accounts for an edit list where the
	// container carries one.
	Duration time.Duration
	// Width and Height are the stored dimensions in pixels, before Rotation is
	// applied. They are 0 when the file has no video track.
	Width, Height int
	// VideoCodec and AudioCodec are the stable short names the package
	// documentation lists under "Codec names". VideoCodec is empty when the file
	// has no video track and AudioCodec is empty when it has no audio track;
	// neither is an error.
	VideoCodec, AudioCodec string
	// Bitrate is the overall rate in bits per second, derived as the package
	// documentation describes under "Bitrate". It is 0 when the duration is 0,
	// which is the only case where it cannot be derived.
	Bitrate int
	// FrameRate is the average frames per second of the video track, derived as
	// the package documentation describes under "Frame rate". It is 0 when the
	// file has no video track, no video samples, or no duration.
	FrameRate float64
	// Rotation is the clockwise rotation to apply on playback, in degrees: 0,
	// 90, 180, or 270. A matrix that is not one of those four, because it also
	// flips or shears, reports 0.
	Rotation int
}

// Probe reports what a file holds: duration, dimensions, codecs, bitrate, frame
// rate, and rotation. It reads box headers and the movie header only, never the
// media payload, so probing a multi-gigabyte file costs a few reads.
//
// size is the length of the file r reads from; Probe trusts it and never reads
// past it. A file with no audio track, or with no video track, is not an error:
// the corresponding fields stay at their zero values. Input that is not a
// container this library reads returns ErrUnsupportedContainer with no partial
// result, and a recognized container whose headers are malformed or cut short
// returns ErrCorrupt.
//
// Truncation is caught when it contradicts a box header, which is the usual
// case: a cut inside the media payload leaves the enclosing box declaring more
// bytes than the file holds. A cut that lands exactly on a box boundary does
// not, so a file whose payload was dropped whole probes successfully. Detecting
// that would mean reading the payload, which is the cost Probe exists to avoid.
func Probe(r io.ReaderAt, size int64) (Info, error) {
	file, err := isobmff.Parse(r, size)
	if err != nil {
		return Info{}, probeError(err)
	}

	info := Info{Duration: file.Duration()}
	if video := file.VideoTrack(); video != nil {
		info.Width = int(video.Width)
		info.Height = int(video.Height)
		info.VideoCodec = video.Entry.Codec
		info.FrameRate = video.FrameRate()
		info.Rotation = video.Rotation
	}
	if audio := file.AudioTrack(); audio != nil {
		info.AudioCodec = audio.Entry.Codec
	}
	if seconds := info.Duration.Seconds(); seconds > 0 {
		info.Bitrate = int(float64(size) * 8 / seconds)
	}
	return info, nil
}

// probeError maps a demuxer error onto this package's sentinels. The demuxer's
// own sentinel stays in the chain, so errors.Is finds either one.
func probeError(err error) error {
	if errors.Is(err, isobmff.ErrNotISOBMFF) {
		return fmt.Errorf("%w: %w", ErrUnsupportedContainer, err)
	}
	return fmt.Errorf("%w: %w", ErrCorrupt, err)
}
