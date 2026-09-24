package matroska

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Sentinel errors. Every error this package returns wraps one of these, so a
// caller can tell an unsupported file from a corrupt one with errors.Is.
var (
	// ErrNotMatroska means the file does not start with an EBML header naming a
	// document type this package reads.
	ErrNotMatroska = errors.New("matroska: not a Matroska file")
	// ErrTruncated means an element or a frame runs past the end of the file.
	ErrTruncated = errors.New("matroska: file is truncated")
	// ErrMalformed means the element tree, or a structure inside it, is
	// self-contradictory.
	ErrMalformed = errors.New("matroska: malformed structure")
	// ErrElementTooLarge means an element this package has to read whole is
	// over its cap.
	ErrElementTooLarge = errors.New("matroska: element is over the size cap")
	// ErrTooDeep means the element tree nests deeper than this package will
	// follow.
	ErrTooDeep = errors.New("matroska: element nesting is too deep")
	// ErrFrameTooLarge means a frame declares more bytes than the read cap.
	ErrFrameTooLarge = errors.New("matroska: frame is over the size cap")
	// ErrNoVideoTrack means the file carries no video track.
	ErrNoVideoTrack = errors.New("matroska: no video track")
	// ErrNoSyncSample means the video track has no keyframe at or before the
	// requested time, or none this package could reach.
	ErrNoSyncSample = errors.New("matroska: no keyframe")
)

// Element IDs, stored the way the file stores them: with the length marker bit
// still in the value, so an ID compares as the bytes on disk. Matroska
// specification RFC 9559 section 5 and the element tables of section 27.
const (
	idEBML          = 0x1A45DFA3
	idDocType       = 0x4282
	idVoid          = 0xEC
	idCRC32         = 0xBF
	idSegment       = 0x18538067
	idSeekHead      = 0x114D9B74
	idSeek          = 0x4DBB
	idSeekID        = 0x53AB
	idSeekPosition  = 0x53AC
	idInfo          = 0x1549A966
	idTimestampScal = 0x2AD7B1
	idDuration      = 0x4489
	idTracks        = 0x1654AE6B
	idTrackEntry    = 0xAE
	idTrackNumber   = 0xD7
	idTrackType     = 0x83
	idCodecID       = 0x86
	idCodecPrivate  = 0x63A2
	idFlagLacing    = 0x9C
	idDefaultDur    = 0x23E383
	idVideo         = 0xE0
	idPixelWidth    = 0xB0
	idPixelHeight   = 0xBA
	idAudio         = 0xE1
	idSamplingFreq  = 0xB5
	idChannels      = 0x9F
	idCues          = 0x1C53BB6B
	idCuePoint      = 0xBB
	idCueTime       = 0xB3
	idCueTrackPos   = 0xB7
	idCueTrack      = 0xF7
	idCueClusterPos = 0xF1
	idCueRelPos     = 0xF0
	idCluster       = 0x1F43B675
	idTimestamp     = 0xE7
	idPosition      = 0xA7
	idPrevSize      = 0xAB
	idSilentTracks  = 0x5854
	idSimpleBlock   = 0xA3
	idBlockGroup    = 0xA0
	idBlock         = 0xA1
	idReferenceBloc = 0xFB
	idBlockDuration = 0x9B
)

// Size caps. Everything this package reads whole is bounded by one of these
// rather than by a size the file declares.
const (
	// maxHeaderBytes bounds the EBML header, which holds the document type and
	// a handful of integers.
	maxHeaderBytes = 4 << 10
	// maxSeekHeadBytes bounds the seek index of the segment's own children.
	maxSeekHeadBytes = 64 << 10
	// maxInfoBytes bounds the segment information, which holds the timestamp
	// scale, the duration, and a few strings.
	maxInfoBytes = 1 << 20
	// maxTracksBytes bounds the track descriptions. Their weight is the codec
	// private data, which is a few hundred bytes per track.
	maxTracksBytes = 4 << 20
	// maxCuesBytes bounds the seek index. A cue point is about twenty bytes and
	// a long file with a keyframe every two seconds has a few thousand, so this
	// is generous by several orders of magnitude and still a bound.
	maxCuesBytes = 16 << 20
	// maxFrameBytes bounds one frame read, matching the sample cap of the
	// ISOBMFF side. A 4K intra frame runs to a few hundred kilobytes.
	maxFrameBytes = 32 << 20
	// maxBlockHeaderBytes bounds the read that decodes one block header: the
	// track number, the timestamp, the flags, and any lacing table. A lacing
	// table longer than this belongs to a block with hundreds of frames in it,
	// which no muxer writes.
	maxBlockHeaderBytes = 256

	// maxDepth bounds recursion into nested elements. A real file nests five
	// deep, Segment > Tracks > TrackEntry > Video > Colour > MasteringMetadata,
	// so this leaves room without letting a hostile file drive the stack.
	maxDepth = 12
	// maxSegmentChildren bounds the walk of the segment's own children. It is
	// hit only by a file with more clusters than any muxer writes: a four hour
	// recording with a cluster a second has fourteen thousand.
	//
	// ponytail: a flat ceiling rather than a byte budget. Raise it if a real
	// file ever writes a cluster every few kilobytes.
	maxSegmentChildren = 1 << 17
	// maxClusterChildren bounds the walk of one cluster's blocks.
	maxClusterChildren = 1 << 20
	// maxScanClusters bounds the forward scan that stands in for a seek index,
	// used by a file with no Cues and by the frame rate average. It is about
	// four hours of one-second clusters.
	maxScanClusters = 1 << 14
)

// unknownSize is the size an element declares when it does not know its own
// length yet, all value bits set. It is legal on a Segment and on a Cluster,
// which a live muxer writes before it knows where they end.
const unknownSize = int64(-1)

// element is a decoded element header.
type element struct {
	// id is the element ID as the file stores it, marker bit included.
	id uint32
	// size is the payload size in bytes, or unknownSize.
	size int64
	// hdrSize is the bytes of ID and size that come before the payload.
	hdrSize int64
}

// end reports where the element's payload ends, given where its header started
// and how many bytes are left in the parent. An unknown size runs to the end of
// the parent, which is what a caller that cannot resolve it better has to
// assume.
func (e element) end(off, avail int64) int64 {
	if e.size == unknownSize {
		return off + avail
	}
	return off + e.hdrSize + e.size
}

// parseElement decodes an element header from the start of buf and checks it
// against avail, the bytes left in the enclosing element or file.
func parseElement(buf []byte, avail int64) (element, error) {
	id, idLen, err := readID(buf)
	if err != nil {
		return element{}, err
	}
	size, sizeLen, err := readSize(buf[idLen:])
	if err != nil {
		return element{}, err
	}

	e := element{id: id, size: size, hdrSize: int64(idLen + sizeLen)}
	if e.hdrSize > avail {
		return element{}, fmt.Errorf("%w: a %d byte element header with %d bytes left", ErrTruncated, e.hdrSize, avail)
	}
	if e.size != unknownSize && e.size > avail-e.hdrSize {
		return element{}, fmt.Errorf("%w: element %#x declares %d bytes with %d left",
			ErrMalformed, e.id, e.size, avail-e.hdrSize)
	}
	return e, nil
}

// readID decodes an element ID: one to four bytes, where the position of the
// first set bit gives the length and the whole encoding is the value.
func readID(buf []byte) (uint32, int, error) {
	if len(buf) == 0 {
		return 0, 0, fmt.Errorf("%w: no bytes left for an element ID", ErrTruncated)
	}
	n := 1
	for ; n <= 4; n++ {
		if buf[0]&(0x80>>(n-1)) != 0 {
			break
		}
	}
	if n > 4 {
		return 0, 0, fmt.Errorf("%w: an element ID of over four bytes", ErrMalformed)
	}
	if len(buf) < n {
		return 0, 0, fmt.Errorf("%w: a %d byte element ID with %d bytes left", ErrTruncated, n, len(buf))
	}
	var id uint32
	for _, b := range buf[:n] {
		id = id<<8 | uint32(b)
	}
	return id, n, nil
}

// readSize decodes a size: one to eight bytes, where the position of the first
// set bit gives the length and that bit is not part of the value. An encoding
// whose value bits are all set means the size is not yet known.
func readSize(buf []byte) (int64, int, error) {
	if len(buf) == 0 {
		return 0, 0, fmt.Errorf("%w: no bytes left for an element size", ErrTruncated)
	}
	n := 1
	for ; n <= 8; n++ {
		if buf[0]&(0x80>>(n-1)) != 0 {
			break
		}
	}
	if n > 8 {
		return 0, 0, fmt.Errorf("%w: an element size of over eight bytes", ErrMalformed)
	}
	if len(buf) < n {
		return 0, 0, fmt.Errorf("%w: a %d byte element size with %d bytes left", ErrTruncated, n, len(buf))
	}
	value := uint64(buf[0] &^ (0x80 >> (n - 1)))
	for _, b := range buf[1:n] {
		value = value<<8 | uint64(b)
	}
	// All value bits set is the unknown-size encoding: seven bits per byte
	// after the marker.
	if value == 1<<(7*n)-1 {
		return unknownSize, n, nil
	}
	if value > math.MaxInt64 {
		return 0, 0, fmt.Errorf("%w: an element size of %d bytes", ErrMalformed, value)
	}
	return int64(value), n, nil
}

// maxElementHeader is the longest header parseElement can need: a four byte ID
// and an eight byte size.
const maxElementHeader = 12

// errStopScan ends a scan without reporting an error.
var errStopScan = errors.New("matroska: stop")

// scan calls fn for every element in a span of the file, reading nothing but
// headers. It is the walk used where the payload stays on disk: the segment's
// own children and one cluster's blocks. fn is given the element and the offset
// of its header; the walk stops when fn returns errStopScan, when limit
// elements have been seen, or when the span runs out. An element that declares
// an unknown size ends the walk, since nothing here can say where it stops.
func scan(r io.ReaderAt, off, avail int64, limit int, fn func(e element, at int64) error) error {
	return scanResolving(r, off, avail, limit, nil, fn)
}

// scanResolving is scan with an answer for the unknown-size case: resolve is
// given such an element and returns where its payload ends, so that the walk
// can carry on past it. Only a caller that knows what the element is can say,
// which is why this is not scan's own business.
func scanResolving(r io.ReaderAt, off, avail int64, limit int,
	resolve func(e element, at int64) int64, fn func(e element, at int64) error,
) error {
	var buf [maxElementHeader]byte
	for seen := 0; avail > 0; seen++ {
		if seen >= limit {
			return fmt.Errorf("%w: past %d elements at %d", ErrMalformed, limit, off)
		}
		read := min(avail, maxElementHeader)
		if _, err := r.ReadAt(buf[:read], off); err != nil {
			return fmt.Errorf("%w: reading the element header at %d: %w", ErrTruncated, off, err)
		}
		e, err := parseElement(buf[:read], avail)
		if err != nil {
			return fmt.Errorf("element at %d: %w", off, err)
		}
		if err := fn(e, off); err != nil {
			if errors.Is(err, errStopScan) {
				return nil
			}
			return err
		}
		next := off + e.hdrSize + e.size
		if e.size == unknownSize {
			if resolve == nil {
				return nil
			}
			next = resolve(e, off)
		}
		if next <= off {
			return fmt.Errorf("%w: element %#x at %d does not advance", ErrMalformed, e.id, off)
		}
		avail -= next - off
		off = next
	}
	return nil
}

// walk calls fn for every child of an in-memory payload. depth is the depth of
// payload's own parent; a parser that descends passes depth+1, and the walk
// refuses to go past maxDepth. An unknown size inside a buffer runs to the end
// of it, which is the only reading available once the bytes are in hand.
func walk(payload []byte, depth int, fn func(id uint32, body []byte) error) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: past %d levels", ErrTooDeep, maxDepth)
	}
	for off := 0; off < len(payload); {
		avail := int64(len(payload) - off)
		e, err := parseElement(payload[off:], avail)
		if err != nil {
			return err
		}
		size := e.size
		if size == unknownSize {
			size = avail - e.hdrSize
		}
		body := payload[off+int(e.hdrSize) : off+int(e.hdrSize+size)]
		if err := fn(e.id, body); err != nil {
			return err
		}
		off += int(e.hdrSize + size)
	}
	return nil
}

// readWhole reads one element payload into memory, refusing anything over
// limit. This is the only place the package allocates from a size the file
// declares, and the cap is checked before the allocation.
func readWhole(r io.ReaderAt, off, size, limit int64, what string) ([]byte, error) {
	if size > limit {
		return nil, fmt.Errorf("%w: %s is %d bytes, over the %d byte cap", ErrElementTooLarge, what, size, limit)
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

// readUint reads an EBML unsigned integer, which is stored big-endian in as few
// bytes as it needs. An empty body is zero, which is what the format means by
// omitting the bytes.
func readUint(body []byte) (uint64, error) {
	if len(body) > 8 {
		return 0, fmt.Errorf("%w: an %d byte integer", ErrMalformed, len(body))
	}
	var value uint64
	for _, b := range body {
		value = value<<8 | uint64(b)
	}
	return value, nil
}

// readFloat reads an EBML float, which is four or eight bytes, or none at all for
// zero.
func readFloat(body []byte) (float64, error) {
	switch len(body) {
	case 0:
		return 0, nil
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(body))), nil
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(body)), nil
	default:
		return 0, fmt.Errorf("%w: a %d byte float", ErrMalformed, len(body))
	}
}
