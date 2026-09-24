package matroska

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Lacing modes, from bits 1 and 2 of a block's flags. RFC 9559 section 12.
const (
	lacingNone  = 0
	lacingXiph  = 1
	lacingFixed = 2
	lacingEBML  = 3
)

// blockKeyframeFlag is bit 7 of a SimpleBlock's flags. A Block inside a
// BlockGroup does not carry it; there, a keyframe is a block with no
// ReferenceBlock beside it.
const blockKeyframeFlag = 0x80

// block is one decoded block header. The frame payload stays on disk.
type block struct {
	// track is the TrackNumber the block belongs to.
	track uint64
	// ticks is when the block is shown, absolute, on the file's timestamp
	// scale: the cluster's timestamp plus the block's own signed offset.
	ticks int64
	// keyframe reports whether the block decodes on its own.
	keyframe bool
	// frames is how many frames the block carries, which is more than one only
	// when it is laced.
	frames int
	// at is the offset of the block's own element header, or of the BlockGroup
	// around it. It is where a seek index points, and where a walk of the
	// cluster has to restart to find this block again.
	at int64
	// first is the first frame's bytes in the file. A block with no lacing
	// carries exactly one frame, and this is it.
	first span
	// flags is the block's own flags byte, keyframe bit and lacing mode
	// included. A writer copying the block across keeps it as it stands.
	flags byte
	// body is everything after the flags byte: the lacing table, where there is
	// one, and the frames. It is what a Matroska-to-Matroska copy moves
	// verbatim, which is how a laced block survives the trip without being
	// taken apart.
	body span
}

// eachBlock calls fn for every block in one cluster, reading block headers and
// nothing else. The walk stops when fn returns errStopScan.
//
// The cluster's own Timestamp is its first child in every file that follows the
// spec, so a block is given an absolute time as it is walked. A cluster that
// declares its timestamp late, or not at all, has its blocks reported against
// zero, which is what the format leaves as the default.
func (f *File) eachBlock(cluster span, fn func(b block) error) error {
	var clusterTicks int64
	return scan(f.r, cluster.start, cluster.size, maxClusterChildren, func(e element, off int64) error {
		payload := span{start: off + e.hdrSize, size: e.size}
		switch e.id {
		case idTimestamp:
			raw, err := readWhole(f.r, payload.start, payload.size, 8, "a cluster timestamp")
			if err != nil {
				return err
			}
			ticks, err := readUint(raw)
			if err != nil {
				return err
			}
			clusterTicks = int64(ticks)
		case idSimpleBlock:
			b, err := f.readBlockHeader(payload, clusterTicks, true)
			if err != nil {
				return err
			}
			b.at = off
			return fn(b)
		case idBlockGroup:
			b, ok, err := f.readBlockGroup(payload, clusterTicks)
			if err != nil || !ok {
				return err
			}
			b.at = off
			return fn(b)
		}
		return nil
	})
}

// readBlockGroup finds the Block inside a BlockGroup and works out whether it
// is a keyframe, which here is a block with no ReferenceBlock beside it. A
// group with no Block at all is skipped rather than refused: it is a valid
// group carrying nothing this package reads.
func (f *File) readBlockGroup(group span, clusterTicks int64) (block, bool, error) {
	var (
		at         span
		found      bool
		referenced bool
	)
	if err := scan(f.r, group.start, group.size, maxClusterChildren, func(e element, off int64) error {
		switch e.id {
		case idBlock:
			at, found = span{start: off + e.hdrSize, size: e.size}, true
		case idReferenceBloc:
			referenced = true
		}
		return nil
	}); err != nil {
		return block{}, false, err
	}
	if !found {
		return block{}, false, nil
	}
	b, err := f.readBlockHeader(at, clusterTicks, false)
	if err != nil {
		return block{}, false, err
	}
	// A Block inside a group carries no keyframe bit; the absence of a
	// ReferenceBlock is what says so. Folding the answer into the flags lets a
	// writer emit the block as a SimpleBlock without working it out again.
	b.keyframe = !referenced
	if b.keyframe {
		b.flags |= blockKeyframeFlag
	}
	return b, true, nil
}

// readBlockHeader decodes one block's header off disk. simple says whether the
// block is a SimpleBlock, whose flags carry the keyframe bit.
func (f *File) readBlockHeader(at span, clusterTicks int64, simple bool) (block, error) {
	if at.size <= 0 {
		return block{}, fmt.Errorf("%w: an empty block at %d", ErrMalformed, at.start)
	}
	var buf [maxBlockHeaderBytes]byte
	head := buf[:min(at.size, maxBlockHeaderBytes)]
	if _, err := f.r.ReadAt(head, at.start); err != nil && err != io.EOF {
		return block{}, fmt.Errorf("%w: reading the block header at %d: %w", ErrTruncated, at.start, err)
	}
	return parseBlockHeader(head, at, clusterTicks, simple)
}

// parseBlockHeader decodes a block header out of the bytes at the front of the
// block: a track number, a signed 16-bit timestamp relative to the cluster, a
// flags byte, and, when the block is laced, the table of frame sizes.
// RFC 9559 section 12.
func parseBlockHeader(head []byte, at span, clusterTicks int64, simple bool) (block, error) {
	track, n, err := readSize(head)
	if err != nil {
		return block{}, fmt.Errorf("block at %d: %w", at.start, err)
	}
	if track == unknownSize || track == 0 {
		return block{}, fmt.Errorf("%w: a block at %d names no track", ErrMalformed, at.start)
	}
	if len(head) < n+3 {
		return block{}, fmt.Errorf("%w: a block header at %d is %d bytes", ErrTruncated, at.start, len(head))
	}
	var (
		offset = int64(int16(binary.BigEndian.Uint16(head[n:])))
		flags  = head[n+2]
		body   = span{start: at.start + int64(n) + 3, size: at.size - int64(n) - 3}
	)
	if body.size < 0 {
		return block{}, fmt.Errorf("%w: a block at %d has no payload", ErrMalformed, at.start)
	}

	b := block{
		track:    uint64(track),
		ticks:    clusterTicks + offset,
		keyframe: simple && flags&blockKeyframeFlag != 0,
		frames:   1,
		first:    body,
		flags:    flags,
		body:     body,
	}
	if lacing := (flags >> 1) & 0x03; lacing != lacingNone {
		if err := b.applyLacing(head[n+3:], body, lacing); err != nil {
			return block{}, fmt.Errorf("block at %d: %w", at.start, err)
		}
	}
	return b, nil
}

// applyLacing reads the frame table of a laced block, which packs several
// frames into one block to save a header each. Only the count and the first
// frame's extent are wanted here: a thumbnail reads the first frame of a block
// and a frame count needs the rest counted, not located.
func (b *block) applyLacing(table []byte, body span, lacing byte) error {
	if len(table) == 0 {
		return fmt.Errorf("%w: a laced block with no frame count", ErrTruncated)
	}
	count := int(table[0]) + 1
	table, rest := table[1:], body.size-1
	if rest < 0 {
		return fmt.Errorf("%w: a laced block with no frames", ErrMalformed)
	}
	b.frames = count

	var (
		first int64
		read  int
		err   error
	)
	switch lacing {
	case lacingFixed:
		// Every frame is the same size, so there is no table at all.
		if rest%int64(count) != 0 {
			return fmt.Errorf("%w: %d bytes of fixed lacing does not divide into %d frames", ErrMalformed, rest, count)
		}
		first = rest / int64(count)
	case lacingXiph:
		first, read, err = xiphSize(table)
	case lacingEBML:
		first, read, err = ebmlLaceSize(table)
	}
	if err != nil {
		return err
	}
	if first < 0 || first > rest-int64(read) {
		return fmt.Errorf("%w: a first laced frame of %d bytes in %d", ErrMalformed, first, rest)
	}
	// The rest of the size table sits between the count and the frames. Only
	// the first frame's start matters, and the sizes of the frames after it are
	// never asked for, so the remaining entries are skipped rather than read.
	skipped, err := laceTableBytes(table, lacing, count, read)
	if err != nil {
		return err
	}
	b.first = span{start: body.start + 1 + int64(skipped), size: first}
	if b.first.size > body.size-1-int64(skipped) {
		return fmt.Errorf("%w: a first laced frame of %d bytes past the block", ErrMalformed, b.first.size)
	}
	return nil
}

// laceTableBytes is the length of a laced block's whole size table, given that
// the first entry took read bytes. Fixed lacing has no table, and both of the
// other two store a size for all but the last frame.
func laceTableBytes(table []byte, lacing byte, count, read int) (int, error) {
	if lacing == lacingFixed {
		return 0, nil
	}
	total := read
	for range count - 2 {
		var (
			n   int
			err error
		)
		if lacing == lacingXiph {
			_, n, err = xiphSize(table[total:])
		} else {
			_, n, err = ebmlLaceDelta(table[total:])
		}
		if err != nil {
			return 0, err
		}
		total += n
	}
	if total > len(table) {
		return 0, fmt.Errorf("%w: a lacing table of %d bytes in %d", ErrMalformed, total, len(table))
	}
	return total, nil
}

// xiphSize reads one Xiph-laced size: bytes of 255 summed until one that is
// less, which is included in the sum and ends the entry.
func xiphSize(table []byte) (int64, int, error) {
	var size int64
	for n, b := range table {
		size += int64(b)
		if b < 255 {
			return size, n + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: a Xiph lacing size that never ends", ErrTruncated)
}

// ebmlLaceSize reads the first EBML-laced size, which is an unsigned vint.
func ebmlLaceSize(table []byte) (int64, int, error) {
	size, n, err := readSize(table)
	if err != nil {
		return 0, 0, err
	}
	if size == unknownSize {
		return 0, 0, fmt.Errorf("%w: an EBML lacing size of unknown length", ErrMalformed)
	}
	return size, n, nil
}

// ebmlLaceDelta reads a later EBML-laced size, which is a signed vint holding
// the difference from the previous one. The sign comes from subtracting the
// midpoint of the encoding's range, so only its width is wanted here.
func ebmlLaceDelta(table []byte) (int64, int, error) {
	size, n, err := readSize(table)
	if err != nil {
		return 0, 0, err
	}
	if size == unknownSize {
		return 0, 0, fmt.Errorf("%w: an EBML lacing delta of unknown length", ErrMalformed)
	}
	return size - (1<<(7*n-1) - 1), n, nil
}
