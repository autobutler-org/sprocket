package isobmff

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Sentinel errors. Every error this package returns wraps one of these, so a
// caller can tell an unsupported file from a corrupt one with errors.Is.
var (
	// ErrNotISOBMFF means the file does not start like a member of the family.
	ErrNotISOBMFF = errors.New("isobmff: not an ISO base media file")
	// ErrTruncated means a box or a sample runs past the end of the file.
	ErrTruncated = errors.New("isobmff: file is truncated")
	// ErrMalformed means the box tree or a table inside it is self-contradictory.
	ErrMalformed = errors.New("isobmff: malformed structure")
	// ErrBoxTooLarge means a box this package has to read whole is over its cap.
	ErrBoxTooLarge = errors.New("isobmff: box is over the size cap")
	// ErrTooDeep means the box tree nests deeper than this package will follow.
	ErrTooDeep = errors.New("isobmff: box nesting is too deep")
	// ErrSampleTooLarge means a sample declares more bytes than the read cap.
	ErrSampleTooLarge = errors.New("isobmff: sample is over the size cap")
	// ErrNoVideoTrack means the file carries no track with a vide handler.
	ErrNoVideoTrack = errors.New("isobmff: no video track")
	// ErrNoSyncSample means the video track declares no sync sample at or
	// before the requested time.
	ErrNoSyncSample = errors.New("isobmff: no sync sample")
	// ErrIncompatibleCodec means a codec has no valid representation in the
	// container it was asked to be written into. CodecTargets is the table.
	ErrIncompatibleCodec = errors.New("isobmff: codec does not fit the target container")
	// ErrUnsupportedSource means a parsed file is not one this package can
	// write from: it is fragmented, or it carries no video or audio track.
	ErrUnsupportedSource = errors.New("isobmff: cannot write from this source")
	// ErrUnsupportedTarget means the requested Target is not one this package
	// writes.
	ErrUnsupportedTarget = errors.New("isobmff: unsupported target container")
)

const (
	// boxHeaderSize is the compact header: a 32-bit size and a
	// four-character type. ISO/IEC 14496-12 4.2.
	boxHeaderSize = 8
	// largeBoxHeaderSize is the header of a box that declares size 1 and
	// carries a 64-bit size after the type.
	largeBoxHeaderSize = 16
	// uuidTypeSize is the extended type that follows a uuid box header.
	uuidTypeSize = 16

	// maxBoxDepth bounds recursion into nested boxes. A real file nests about
	// nine deep (moov > trak > mdia > minf > stbl > stsd > mp4a > wave > esds),
	// so this leaves room without letting a hostile file drive the stack.
	maxBoxDepth = 16
)

// header is a decoded box header.
type header struct {
	// typ is the four-character box type, unpadded, as it appears in the file.
	typ string
	// size is the whole box, header included.
	size int64
	// hdrSize is the bytes before the payload: 8, 16 for a 64-bit size, plus 16
	// more for the extended type of a uuid box.
	hdrSize int64
}

// parseHeader decodes a box header from the start of buf and checks it against
// avail, the bytes left in the enclosing box or file. A declared size of 0 means
// the box runs to the end of its parent, which is legal only for the last one.
func parseHeader(buf []byte, avail int64) (header, error) {
	if avail < boxHeaderSize || len(buf) < boxHeaderSize {
		return header{}, fmt.Errorf("%w: %d bytes left, need a %d byte box header", ErrTruncated, avail, boxHeaderSize)
	}

	h := header{typ: string(buf[4:8]), hdrSize: boxHeaderSize}
	size := uint64(binary.BigEndian.Uint32(buf))
	switch size {
	case 0:
		size = uint64(avail)
	case 1:
		if avail < largeBoxHeaderSize || len(buf) < largeBoxHeaderSize {
			return header{}, fmt.Errorf("%w: %d bytes left, need a %d byte box header", ErrTruncated, avail, largeBoxHeaderSize)
		}
		size = binary.BigEndian.Uint64(buf[8:16])
		h.hdrSize = largeBoxHeaderSize
	}
	if h.typ == "uuid" {
		h.hdrSize += uuidTypeSize
	}

	if size > uint64(avail) {
		return header{}, fmt.Errorf("%w: box %q declares %d bytes with %d left", ErrMalformed, h.typ, size, avail)
	}
	h.size = int64(size)
	if h.size < h.hdrSize {
		return header{}, fmt.Errorf("%w: box %q declares %d bytes, less than its %d byte header", ErrMalformed, h.typ, h.size, h.hdrSize)
	}
	return h, nil
}

// errStopScan ends a scanTop walk without reporting an error.
var errStopScan = errors.New("isobmff: stop")

// scanTop calls fn for every top-level box, reading nothing but headers. The
// walk is flat on purpose: only moov and moof are ever descended into, and both
// are read whole first, so nothing here has to follow a hostile file into a
// nested tree.
func scanTop(r io.ReaderAt, size int64, fn func(h header, off int64) error) error {
	var buf [largeBoxHeaderSize]byte
	for off := int64(0); off < size; {
		avail := min(size-off, largeBoxHeaderSize)
		if _, err := r.ReadAt(buf[:avail], off); err != nil {
			return fmt.Errorf("%w: reading the box header at %d: %w", ErrTruncated, off, err)
		}
		h, err := parseHeader(buf[:avail], size-off)
		if err != nil {
			return fmt.Errorf("top-level box at %d: %w", off, err)
		}
		if err := fn(h, off); err != nil {
			if errors.Is(err, errStopScan) {
				return nil
			}
			return err
		}
		off += h.size
	}
	return nil
}

// walk calls fn for every child box of an in-memory payload. depth is the depth
// of payload's own parent; a parser that descends passes depth+1, and the walk
// refuses to go past maxBoxDepth.
func walk(payload []byte, depth int, fn func(typ string, body []byte) error) error {
	if depth > maxBoxDepth {
		return fmt.Errorf("%w: past %d levels", ErrTooDeep, maxBoxDepth)
	}
	for off := 0; off < len(payload); {
		h, err := parseHeader(payload[off:], int64(len(payload)-off))
		if err != nil {
			return err
		}
		if err := fn(h.typ, payload[off+int(h.hdrSize):off+int(h.size)]); err != nil {
			return err
		}
		off += int(h.size)
	}
	return nil
}

// readWhole reads one box payload into memory, refusing anything over limit.
// This is the only place the package allocates from a size the file declares,
// and the cap is checked before the allocation.
func readWhole(r io.ReaderAt, off, size, limit int64, what string) ([]byte, error) {
	if size > limit {
		return nil, fmt.Errorf("%w: %s is %d bytes, over the %d byte cap", ErrBoxTooLarge, what, size, limit)
	}
	if size <= 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("%w: reading %s at %d: %w", ErrTruncated, what, off, err)
	}
	return buf, nil
}

// fullBoxVersion splits the version and flags off the front of a FullBox and
// returns the rest of its payload. ISO/IEC 14496-12 4.2.
func fullBoxVersion(body []byte) (version uint8, flags uint32, rest []byte, ok bool) {
	if len(body) < 4 {
		return 0, 0, nil, false
	}
	return body[0], binary.BigEndian.Uint32(body) & 0x00ffffff, body[4:], true
}

// entries checks that body holds count entries of width bytes each and returns
// exactly those bytes. Nothing is allocated from count: the table is a view into
// the buffer the box was read into, so a declared count of a billion against a
// twenty kilobyte moov fails here instead of at a make.
func entries(body []byte, count uint32, width int) ([]byte, error) {
	need := uint64(count) * uint64(width)
	if need > uint64(len(body)) {
		return nil, fmt.Errorf("%w: table declares %d entries of %d bytes with %d available", ErrMalformed, count, width, len(body))
	}
	return body[:need], nil
}
