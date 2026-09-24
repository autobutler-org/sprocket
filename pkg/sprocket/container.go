package sprocket

import (
	"errors"
	"fmt"
	"io"

	"github.com/autobutler-org/sprocket/internal/isobmff"
	"github.com/autobutler-org/sprocket/internal/matroska"
	"github.com/autobutler-org/sprocket/internal/mpegts"
)

// container is an input file parsed as whichever family it belongs to. Exactly
// one field is set. Three container families do not justify an interface: the
// four operations switch on this, and each family's answers differ enough that
// an interface would be three implementations of one method each.
type container struct {
	mp4 *isobmff.File
	mkv *matroska.File
	ts  *mpegts.File
}

// cut is a trim expressed the way each source family needs it. The ISOBMFF
// side names sample indexes, which it has an index to name; the Matroska side
// names a window of timestamps, which is all a file with no sample table can
// be cut against. Neither is set for a whole-file remux.
type cut struct {
	ranges map[uint32]isobmff.Range
	span   *matroska.TrimSpan
}

// open parses r as one of the container families this library reads. The
// parsers are asked in turn, ISOBMFF, then Matroska, then MPEG-TS, and each
// one that says the file is not one of its own passes it to the next. Input
// none recognizes reports ErrUnsupportedContainer, and a file a parser did
// recognize but could not read reports what went wrong with it rather than
// being handed to the next one.
//
// MPEG-TS goes last because it is the only one with no header to check: it is
// recognized by three sync bytes at a packet's spacing, which the other two
// would never mistake for their own.
func open(r io.ReaderAt, size int64) (container, error) {
	file, err := isobmff.Parse(r, size)
	switch {
	case err == nil:
		return container{mp4: file}, nil
	case !errors.Is(err, isobmff.ErrNotISOBMFF):
		return container{}, containerError(err)
	}

	matroskaFile, err := matroska.Parse(r, size)
	switch {
	case err == nil:
		return container{mkv: matroskaFile}, nil
	case !errors.Is(err, matroska.ErrNotMatroska):
		return container{}, containerError(err)
	}

	tsFile, err := mpegts.Parse(r, size)
	if err != nil {
		// No parser claimed it, so the last refusal is as much of an answer
		// as the other two and only one of them can be reported.
		return container{}, containerError(err)
	}
	return container{ts: tsFile}, nil
}

// containerError maps a demuxer error onto this package's sentinels. The
// demuxer's own sentinel stays in the chain, so errors.Is finds either one.
func containerError(err error) error {
	switch {
	case errors.Is(err, isobmff.ErrNotISOBMFF), errors.Is(err, matroska.ErrNotMatroska),
		errors.Is(err, mpegts.ErrNotTS):
		return fmt.Errorf("%w: %w", ErrUnsupportedContainer, err)
	case errors.Is(err, isobmff.ErrNoVideoTrack), errors.Is(err, matroska.ErrNoVideoTrack),
		errors.Is(err, mpegts.ErrNoVideoTrack):
		return fmt.Errorf("%w: %w", ErrNoVideo, err)
	default:
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
}
