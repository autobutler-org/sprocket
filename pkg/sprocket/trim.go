package sprocket

import (
	"fmt"
	"io"
	"time"
)

// Trim writes the part of the file r holds between start and end into w as a
// file of the target container, copying the streams across without re-encoding
// anything. It returns the time the output actually begins at, on the source's
// timeline.
//
// The start snaps back to the keyframe at or before it. A cut cannot begin
// mid-GOP without re-encoding, and re-encoding is a non-goal, so the output
// starts earlier than was asked for and the returned time says by how much: a
// caller labelling the result, or seeking inside it, uses that rather than its
// own request. A negative start is read as zero, a start past the end of the
// file takes the last keyframe, and a file whose first keyframe comes after the
// start begins at that first keyframe instead. The end takes the last sample at
// or before it, with no snapping constraint, and an end at or before the start
// keeps the single sample the start landed on.
//
// size is the length of the file r reads from, as with Probe. Every track is
// cut against the same window on its own sample boundaries, so an audio track
// may start up to one audio frame early and end up to one late, around 21
// milliseconds for AAC at 48 kHz. Payload moves as byte ranges through one
// buffer, so trimming a minute out of a multi-gigabyte file costs the same heap
// as trimming a small one, and the output is header-first the way Remux's is.
//
// Timestamps are rewritten rather than expressed as an edit list: the output
// decodes and is displayed from zero, and the source's edit list is dropped.
// See the package documentation under "Trim timestamps" for what that trades
// away. The compatibility rules are Remux's, so a codec the target
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
		ranges, at, err := file.mp4.TrimRanges(start, end)
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
