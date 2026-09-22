package sprocket

import (
	"errors"
	"fmt"
	"io"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// ErrIncompatible means the file's codecs have no valid representation in the
// container that was asked for, so there is no remux to attempt: a MOV
// carrying ProRes or PCM into an MP4 is the case that comes up. The error
// names the codec and the container that would not take it. It is the answer
// CanRemux gives ahead of time, so a caller can offer only the targets that
// will work rather than finding out here. See the package documentation under
// "Remux" for the table.
var ErrIncompatible = errors.New("sprocket: codec does not fit the container")

// Container names an output container for Remux.
type Container string

// The containers Remux writes.
const (
	MP4     Container = "mp4"
	M4V     Container = "m4v"
	ThreeGP Container = "3gp"
)

// CanRemux reports whether a probed file can be moved into target without
// re-encoding. It reads the same compatibility table the remux itself reads,
// so an answer of true is not a guess about the codecs, and a Container this
// library does not write reports false.
//
// It answers from an Info alone, so a caller can probe once and offer the
// targets that will work. It speaks only for the codecs: a source this library
// cannot write from, a fragmented file, still fails at Remux. Those are the
// cases Remux documents, and they are not about the codec pair.
func CanRemux(info Info, target Container) bool {
	container := isobmff.Target(target)
	return isobmff.CodecFits(info.VideoCodec, container) &&
		isobmff.CodecFits(info.AudioCodec, container)
}

// Remux writes the file r holds into w as a file of the target container,
// moving the same streams across without re-encoding anything.
//
// size is the length of the file r reads from, as with Probe. The output is
// header-first: the movie header goes in front of the payload, so the result
// is progressively playable and probing it costs a read of the head. Payload
// moves as byte ranges through one buffer, so a multi-gigabyte source costs
// the same heap as a small one.
//
// Video and audio tracks are carried over, with their sample descriptions,
// edit lists, and display matrices copied verbatim, so the output probes and
// thumbnails to what the input did. A timecode or subtitle track is dropped.
//
// A codec the target cannot hold returns ErrIncompatible, which CanRemux
// reports ahead of time from the same table. Input that is not a container
// this library reads returns ErrUnsupportedContainer, and so does a source
// this library cannot write from: a fragmented file, whose samples are
// described by its fragments rather than by the movie header, and a file with
// neither a video nor an audio track. A container whose headers are malformed
// or cut short returns ErrCorrupt. An error from w is returned as it came, so
// errors.Is finds the caller's own.
func Remux(r io.ReaderAt, size int64, w io.Writer, target Container) error {
	file, err := isobmff.Parse(r, size)
	if err != nil {
		return containerError(err)
	}
	if err := isobmff.Write(w, file, isobmff.Target(target)); err != nil {
		return remuxError(err)
	}
	return nil
}

// remuxError maps a muxer error onto this package's sentinels. Anything the
// muxer did not classify came from the writer, and is passed through so a
// caller can recognize its own failure.
func remuxError(err error) error {
	switch {
	case errors.Is(err, isobmff.ErrIncompatibleCodec):
		return fmt.Errorf("%w: %w", ErrIncompatible, err)
	case errors.Is(err, isobmff.ErrUnsupportedSource), errors.Is(err, isobmff.ErrUnsupportedTarget):
		return fmt.Errorf("%w: %w", ErrUnsupportedContainer, err)
	case errors.Is(err, isobmff.ErrTruncated), errors.Is(err, isobmff.ErrMalformed):
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	default:
		return err
	}
}
