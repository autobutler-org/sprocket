package sprocket

import (
	"errors"
	"fmt"
	"io"

	"github.com/autobutler-org/sprocket/internal/isobmff"
	"github.com/autobutler-org/sprocket/internal/matroska"
)

// container is an input file parsed as whichever family it belongs to. Exactly
// one field is set. Two container families do not justify an interface: the
// four operations switch on this and the compiler checks they covered both.
type container struct {
	mp4 *isobmff.File
	mkv *matroska.File
}

// open parses r as one of the container families this library reads. The
// ISOBMFF parser is asked first and, when it says the file is not one of its
// own, the Matroska parser is asked. Input neither recognizes reports
// ErrUnsupportedContainer, and a file a parser did recognize but could not read
// reports what went wrong with it rather than being handed to the other one.
func open(r io.ReaderAt, size int64) (container, error) {
	file, err := isobmff.Parse(r, size)
	switch {
	case err == nil:
		return container{mp4: file}, nil
	case !errors.Is(err, isobmff.ErrNotISOBMFF):
		return container{}, containerError(err)
	}

	matroskaFile, matroskaErr := matroska.Parse(r, size)
	if matroskaErr != nil {
		// Neither parser claimed it, so the ISOBMFF refusal is as much of an
		// answer as the Matroska one and only one of them can be reported.
		return container{}, containerError(matroskaErr)
	}
	return container{mkv: matroskaFile}, nil
}

// containerError maps a demuxer error onto this package's sentinels. The
// demuxer's own sentinel stays in the chain, so errors.Is finds either one.
func containerError(err error) error {
	switch {
	case errors.Is(err, isobmff.ErrNotISOBMFF), errors.Is(err, matroska.ErrNotMatroska):
		return fmt.Errorf("%w: %w", ErrUnsupportedContainer, err)
	case errors.Is(err, isobmff.ErrNoVideoTrack), errors.Is(err, matroska.ErrNoVideoTrack):
		return fmt.Errorf("%w: %w", ErrNoVideo, err)
	default:
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
}
