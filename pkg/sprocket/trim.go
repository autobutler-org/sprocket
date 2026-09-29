package sprocket

import (
	"fmt"
	"io"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// Trim writes the part of the file r holds between start and end into w as a
// file of the target container, copying the streams across without re-encoding
// anything. It returns the time the output is shown from, on the source's
// timeline.
//
// A cut cannot begin decoding mid-GOP without re-encoding, and re-encoding is a
// non-goal, so the video is copied from the keyframe at or before start. The
// frames between that keyframe and start are the lead-in: they are kept, since
// the frames after them are coded against them, and hidden, so the first frame
// shown is the one the source shows at start. Every target but TS hides it, the
// MP4 family with an edit list and Matroska and WebM with a CodecDelay, and
// start is what is returned. A TS output has no way to hide frames and shows
// the lead-in, so it returns the keyframe's time. Player support for the two
// mechanisms differs; the package documentation has it under "Trim".
//
// Where there is no lead-in to hide the returned time is the keyframe's too: a
// start on a keyframe, past the end of the file, which takes the last keyframe,
// or before a file's first keyframe, which begins there instead. A negative
// start is read as zero. The end takes the last sample at or before it, with no
// snapping constraint, and an end at or before the start keeps the single
// sample the keyframe landed on and returns its time. A caller labelling the
// result, or seeking inside it, uses the returned time rather than its own
// request.
//
// size is the length of the file r reads from, as with Probe. Every track is
// cut against the same window on its own sample boundaries, and an audio
// track's frame straddling the start is kept with its part before the start
// hidden alongside the video's lead-in; where nothing is hidden, the audio may
// start up to one frame early, around 21 milliseconds for AAC at 48 kHz, and
// it may end up to one late either way. Payload moves as byte ranges through
// one buffer, so trimming a minute out of a multi-gigabyte file costs the same
// heap as trimming a small one, and the output is header-first the way Remux's
// is.
//
// Timestamps are rewritten: the output decodes from zero, and the source's
// edit list is dropped. See the package documentation under "Trim timestamps"
// for what that trades away. The compatibility rules are Remux's, so a codec the target
// cannot hold returns ErrIncompatible and CanRemux reports it ahead of time.
// Input that is not a container this library reads returns
// ErrUnsupportedContainer, and so does a fragmented source. A Matroska source
// is cut against a window of timestamps rather than a range of sample numbers,
// which the package documentation describes under "Trim"; writing one into the
// MP4 family produces a fragmented file, as a remux does. A container whose
// headers are malformed or cut short returns ErrCorrupt, and so does a file
// whose video track declares no keyframe at all. An MPEG-TS source returns
// ErrUnsupportedContainer: see the package documentation under "MPEG-TS". An
// MP4-family source cut into a TS output is fine. An error from w is returned as
// it came, so errors.Is finds the caller's own.
func Trim(r io.ReaderAt, size int64, w io.Writer, target Container, start, end time.Duration) (time.Duration, error) {
	file, err := open(r, size)
	if err != nil {
		return 0, err
	}

	var (
		c      cut
		actual time.Duration
	)
	switch {
	case file.ts != nil:
		return 0, fmt.Errorf("%w: an MPEG-TS source is not trimmed", ErrUnsupportedContainer)
	case file.mkv != nil:
		span, at, err := file.mkv.TrimSpan(start, end)
		if err != nil {
			return 0, writeError(err)
		}
		c.span, actual = &span, at
	default:
		// A transport stream has nothing like an edit list to hide frames
		// behind, so it is the one target that shows its lead-in.
		ranges, at, err := file.mp4.TrimRanges(start, end, isobmff.Target(target) != isobmff.TargetTS)
		if err != nil {
			return 0, writeError(err)
		}
		c.ranges, actual = ranges, at
	}
	if err := writeInto(w, file, target, c); err != nil {
		return 0, err
	}
	return actual, nil
}
