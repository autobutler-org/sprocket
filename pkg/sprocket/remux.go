package sprocket

import (
	"errors"
	"fmt"
	"io"

	"github.com/autobutler-org/sprocket/internal/isobmff"
	"github.com/autobutler-org/sprocket/internal/matroska"
	"github.com/autobutler-org/sprocket/internal/mpegts"
)

// ErrIncompatible means the file's codecs have no valid representation in the
// container that was asked for, so there is no remux to attempt: a MOV
// carrying ProRes or PCM into an MP4 is the case that comes up. The error
// names the codec and the container that would not take it. It is the answer
// CanRemux gives ahead of time, so a caller can offer only the targets that
// will work rather than finding out here. See the package documentation under
// "Remux" for the table.
var ErrIncompatible = errors.New("sprocket: codec does not fit the container")

// Container names an output container for Remux and Trim, which write the same
// set of them.
type Container string

// The containers Remux and Trim write.
const (
	MP4     Container = "mp4"
	M4V     Container = "m4v"
	ThreeGP Container = "3gp"
	MOV     Container = "mov"
	ThreeG2 Container = "3g2"
	MKV     Container = "mkv"
	WebM    Container = "webm"
	TS      Container = "ts"
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
// An MP4-family output written from a Matroska, WebM, or MPEG-TS source is
// fragmented, which is the only way to keep the header in front of a source
// that has no index. See the package documentation under "Fragmented output".
// A TS output has no header at all; see the package documentation under
// "MPEG-TS" for what goes into one.
//
// Video and audio tracks are carried over, with their sample descriptions,
// edit lists, and display matrices copied verbatim, so the output probes and
// thumbnails to what the input did. A timecode or subtitle track is dropped.
//
// A codec the target cannot hold returns ErrIncompatible, which CanRemux
// reports ahead of time from the same table. Input that is not a container
// this library reads returns ErrUnsupportedContainer, and so does a source
// this library cannot write from: a fragmented file, whose samples are
// described by its fragments rather than by the movie header, a Matroska file
// with laced blocks written into the MP4 family, where a sample is one frame,
// and a file with neither a video nor an audio track. So does a pairing no
// writer covers: a Matroska source into TS, or a TS source into Matroska. A
// container whose headers are malformed or cut short returns ErrCorrupt. An
// error from w is returned as it came, so errors.Is finds the caller's own.
func Remux(r io.ReaderAt, size int64, w io.Writer, target Container) error {
	file, err := open(r, size)
	if err != nil {
		return err
	}
	return writeInto(w, file, target, cut{})
}

// writeInto writes a parsed source into the target container, cut down where
// the caller passed a cut. It is the one place that pairs a source family with
// a writer, so Remux and Trim cannot disagree about which pairs exist.
//
// A source with an index, the MP4 family, is copied into a header-first output
// of any family. A source without one, Matroska or MPEG-TS, goes into the MP4
// family as a fragmented file: nothing before the media says how many frames
// there are, so there is no index to put in front of them. See the package
// documentation under "Fragmented output". A TS source into a TS output is the
// file itself, and the two cross-family pairings with no index on either side
// are not written.
func writeInto(w io.Writer, file container, target Container, c cut) error {
	out := isobmff.Target(target)
	matroskaTarget := out == isobmff.TargetMKV || out == isobmff.TargetWebM
	tsTarget := out == isobmff.TargetTS

	switch {
	case file.ts != nil && tsTarget:
		return copyWhole(w, file.ts)
	case file.ts != nil && matroskaTarget, file.mkv != nil && tsTarget:
		return fmt.Errorf("%w: no writer takes %s into %s", ErrUnsupportedContainer, sourceFamily(file), target)
	case file.ts != nil:
		return writeError(mpegts.WriteFragmented(w, file.ts, out))
	case tsTarget:
		return writeError(mpegts.Write(w, file.mp4, c.ranges))
	case file.mkv != nil && matroskaTarget:
		return writeError(matroska.Copy(w, file.mkv, out, c.span))
	case file.mkv != nil:
		return writeError(matroska.WriteFragmented(w, file.mkv, out, c.span))
	case matroskaTarget:
		return writeError(matroska.Write(w, file.mp4, out, c.ranges))
	default:
		return writeError(isobmff.Write(w, file.mp4, out, c.ranges))
	}
}

// copyWhole writes a transport stream out as it stands, which is the whole of a
// remux from TS into TS.
func copyWhole(w io.Writer, file *mpegts.File) error {
	r, size := file.Source()
	if _, err := io.Copy(w, io.NewSectionReader(r, 0, size)); err != nil {
		return fmt.Errorf("copying the stream: %w", err)
	}
	return nil
}

// sourceFamily names the family a parsed source belongs to, for an error.
func sourceFamily(file container) string {
	switch {
	case file.mkv != nil:
		return "Matroska"
	case file.ts != nil:
		return "MPEG-TS"
	}
	return "ISOBMFF"
}

// writeError maps a muxer error onto this package's sentinels, for Remux and
// for Trim, which go through the same writer. Anything the muxer did not
// classify came from the writer, and is passed through so a caller can
// recognize its own failure.
func writeError(err error) error {
	switch {
	case errors.Is(err, isobmff.ErrIncompatibleCodec):
		return fmt.Errorf("%w: %w", ErrIncompatible, err)
	case errors.Is(err, isobmff.ErrUnsupportedSource), errors.Is(err, isobmff.ErrUnsupportedTarget):
		return fmt.Errorf("%w: %w", ErrUnsupportedContainer, err)
	case errors.Is(err, isobmff.ErrTruncated), errors.Is(err, isobmff.ErrMalformed),
		errors.Is(err, isobmff.ErrNoSyncSample), errors.Is(err, matroska.ErrTruncated),
		errors.Is(err, matroska.ErrMalformed), errors.Is(err, matroska.ErrNoSyncSample),
		errors.Is(err, matroska.ErrElementTooLarge), errors.Is(err, isobmff.ErrBoxTooLarge),
		errors.Is(err, mpegts.ErrTruncated), errors.Is(err, mpegts.ErrMalformed):
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	default:
		return err
	}
}
