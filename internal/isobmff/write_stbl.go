package isobmff

import (
	"encoding/binary"
	"fmt"
	"math"
)

// stbl writes a track's sample tables and returns the position of its chunk
// offset field, which the caller fills in once the layout is known.
//
// Most of the tables are the source's own, copied entry for entry: stts, ctts,
// and stss describe the samples themselves and say nothing about where they
// sit, so a remux has nothing to recompute in them, and copying keeps them in
// the compact run form the source chose rather than expanding them per sample.
// A cut track brings slices of those same tables, which are still tables of the
// same shape. Only stsc and stco describe the layout, and the output's layout
// is one chunk per track, so each of those is a single entry.
//
// stsd is copied whole, including any sample entry past the first. The demuxer
// reads only the first one, and the stsc written here points every sample at
// it, so a track with more than one description would come out mislabelled.
// ponytail: nothing in this library produces or reads such a track; carry the
// source's own description indices across if one ever turns up.
func (b *boxWriter) stbl(c cutTrack) int {
	s := c.tables
	start := b.open("stbl")

	stsd := b.open("stsd")
	if len(c.src.stsd) > 0 {
		b.raw(c.src.stsd)
	} else {
		b.u32(0) // version and flags
		b.u32(0) // entry count
	}
	b.close(stsd)

	b.entryTable("stts", 0, 8, s.stts)
	if len(s.ctts) > 0 {
		var version uint8
		if s.cttsSigned {
			version = 1
		}
		b.entryTable("ctts", version, 8, s.ctts)
	}
	if len(s.stss) > 0 {
		b.entryTable("stss", 0, 4, s.stss)
	}
	b.sampleSizes(s)

	// One chunk holding every sample of the track, described from the first
	// chunk onwards by the first sample description.
	stsc := b.full("stsc", 0, 0)
	if s.count == 0 {
		b.u32(0)
	} else {
		b.u32(1)
		b.u32(1)
		b.u32(s.count)
		b.u32(1)
	}
	b.close(stsc)

	// co64 rather than stco whatever the file's size. Eight bytes a track buys
	// one code path instead of a size-dependent choice between two, and every
	// reader of this family has handled co64 since the 2003 base spec.
	at := -1
	co64 := b.full("co64", 0, 0)
	if s.count == 0 {
		b.u32(0)
	} else {
		b.u32(1)
		at = len(b.buf)
		b.u64(0)
	}
	b.close(co64)

	b.close(start)
	return at
}

// entryTable writes a FullBox whose body is an entry count and the entries
// themselves, copying the source's entries as they stand.
func (b *boxWriter) entryTable(typ string, version uint8, width int, entries []byte) {
	start := b.full(typ, version, 0)
	b.u32(uint32(len(entries) / width))
	b.raw(entries)
	b.close(start)
}

// sampleSizes writes the sample size table in whichever of the two forms the
// source used, so a compact stz2 stays compact rather than being expanded into
// four bytes a sample. ISO/IEC 14496-12 8.7.3.
func (b *boxWriter) sampleSizes(s *sampleTables) {
	switch s.sizeBits {
	case 4, 8, 16:
		start := b.full("stz2", 0, 0)
		b.zeros(3) // reserved
		b.raw([]byte{s.sizeBits})
		b.u32(s.count)
		b.raw(s.sizes)
		b.close(start)
	default:
		start := b.full("stsz", 0, 0)
		b.u32(s.constSize)
		b.u32(s.count)
		if s.constSize == 0 {
			b.raw(s.sizes)
		}
		b.close(start)
	}
}

// spanBytes is the total stored size of n samples starting at index. It is what
// the output's chunk offsets are laid out from, so it refuses a table that
// cannot account for every sample in the span rather than answering short and
// putting every later track's samples in the wrong place.
//
// A constant sample size is multiplied rather than summed, for the reason
// sampleRange gives: an stsz that declares one size and a billion samples has
// no table behind it to bound the loop.
func (s *sampleTables) spanBytes(index, n uint32) (int64, error) {
	if n == 0 {
		return 0, nil
	}
	if s.constSize != 0 {
		if index >= s.count || s.count-index < n {
			return 0, fmt.Errorf("%w: %d samples from %d of %d", ErrMalformed, n, index, s.count)
		}
		return int64(n) * int64(s.constSize), nil
	}
	var total int64
	for i := index; i < index+n; i++ {
		size, err := s.sampleSize(i)
		if err != nil {
			return 0, err
		}
		total += int64(size)
	}
	return total, nil
}

// eachRange hands fn the byte ranges n samples from first occupy, in the order
// the source stores them, at most one range per chunk. The whole track is
// first 0 and n the sample count.
//
// Chunk by chunk rather than sample by sample: samples sit back to back inside
// a chunk, so their ranges join into one, and a track whose samples are all the
// same size then costs a multiplication per chunk rather than a step per
// sample. That is what keeps a file declaring a constant size and a billion
// samples cheap, since there is no size table there to bound the count. A span
// that starts or ends inside a chunk takes the part of it that is in the span.
// It fails rather than copying a prefix when the chunk tables do not reach as
// far as the sample count claims.
func (s *sampleTables) eachRange(first, n uint32, fn func(byteRange) error) error {
	if s.count == 0 || n == 0 {
		return nil
	}
	if s.chunkWidth == 0 {
		return fmt.Errorf("%w: no chunk offset table", ErrMalformed)
	}

	chunkCount := uint32(len(s.chunks) / s.chunkWidth)
	var index uint32
	for off := 0; off+12 <= len(s.stsc) && index < s.count; off += 12 {
		firstChunk, perChunk := be32(s.stsc[off:]), be32(s.stsc[off+4:])
		if firstChunk == 0 || perChunk == 0 {
			return fmt.Errorf("%w: stsc entry %d has chunk %d and %d samples", ErrMalformed, off/12, firstChunk, perChunk)
		}
		last := chunkCount
		if next := off + 12; next+12 <= len(s.stsc) {
			if nextFirst := be32(s.stsc[next:]); nextFirst > 0 && nextFirst-1 < last {
				last = nextFirst - 1
			}
		}

		for chunk := firstChunk; chunk <= last && index < s.count; chunk++ {
			offset, err := s.chunkOffset(chunk - 1)
			if err != nil {
				return err
			}
			held := min(perChunk, s.count-index)
			if lo, hi := max(index, first), min(index+held, first+n); lo < hi {
				ahead, err := s.spanBytes(index, lo-index)
				if err != nil {
					return err
				}
				size, err := s.spanBytes(lo, hi-lo)
				if err != nil {
					return err
				}
				if err := fn(byteRange{offset: offset + ahead, size: size}); err != nil {
					return err
				}
			}
			index += held
		}
	}

	if index != s.count {
		return fmt.Errorf("%w: the chunk tables place %d of %d samples", ErrMalformed, index, s.count)
	}
	return nil
}

// slice returns the tables describing n samples from first, the run tables cut
// at both ends. A slice of a run table is still a run table: the runs covering
// the span come across with the counts of the first and last trimmed, so
// nothing is expanded per sample and the cost is the runs the source already
// had. The first sample of the slice decodes at zero, which is what makes the
// output play from its own start with no edit list and no arithmetic.
//
// Only the tables that describe the samples themselves are carried. The
// output's layout is one chunk a track, so the source's stsc and chunk offsets
// have no bearing on it, and the payload copy reads the byte ranges from the
// source's own tables rather than from these.
//
// The composition offsets are normalized: the span's earliest composition time
// is subtracted from every ctts entry, so the cut is displayed from zero as
// well as decoded from zero. Without that the first frame would be shown one
// codec delay after the file began, since the edit list that used to hide the
// delay is gone with the cut, and a track with no ctts at all, which is every
// audio track, would start ahead of it. Relative timing is untouched: every
// sample moves by the same amount.
func (s *sampleTables) slice(first, n uint32) (*sampleTables, error) {
	if uint64(first)+uint64(n) > uint64(s.count) {
		return nil, fmt.Errorf("%w: %d samples from %d of %d", ErrMalformed, n, first, s.count)
	}
	sizes, err := s.sliceSizes(first, n)
	if err != nil {
		return nil, err
	}
	out := &sampleTables{
		stts:       sliceRuns(s.stts, first, n),
		ctts:       sliceRuns(s.ctts, first, n),
		stss:       sliceSync(s.stss, first, n),
		constSize:  s.constSize,
		sizes:      sizes,
		sizeBits:   s.sizeBits,
		count:      n,
		cttsSigned: s.cttsSigned,
	}
	if shift := out.earliestComposition(); shift > 0 {
		out.cttsSigned = out.shiftComposition(shift) || out.cttsSigned
	}
	return out, nil
}

// earliestComposition is the earliest composition time the tables describe,
// measured from their first sample's decode time. It is zero for a track with
// no ctts, whose samples are displayed when they are decoded.
//
// Composition time only rises inside a ctts run, since the offset is fixed
// there and the decode time is not, so the minimum can only sit at the first
// sample of a run, or at the first sample past the end of the table where one
// stops short of the track. Both tables are walked once, together, and the
// decode cursor only moves forwards.
func (s *sampleTables) earliestComposition() int64 {
	if len(s.ctts) == 0 || s.count == 0 {
		return 0
	}
	var (
		// The stts run at sttsAt covers samples from seen, starting at base.
		sttsAt int
		seen   uint32
		base   uint64
	)
	decodeAt := func(index uint32) uint64 {
		for sttsAt+8 <= len(s.stts) {
			run, delta := be32(s.stts[sttsAt:]), uint64(be32(s.stts[sttsAt+4:]))
			if index < seen+run {
				return base + uint64(index-seen)*delta
			}
			base += uint64(run) * delta
			seen += run
			sttsAt += 8
		}
		return base
	}

	var (
		earliest = int64(math.MaxInt64)
		index    uint32
	)
	for off := 0; off+8 <= len(s.ctts) && index < s.count; off += 8 {
		earliest = min(earliest, int64(decodeAt(index))+cttsValue(s.ctts[off+4:], s.cttsSigned))
		index += be32(s.ctts[off:])
	}
	// Samples the table stops short of carry no offset at all, so they are
	// displayed when they are decoded.
	if index < s.count {
		earliest = min(earliest, int64(decodeAt(index)))
	}
	if earliest == math.MaxInt64 {
		return 0
	}
	return earliest
}

// shiftComposition subtracts shift from every composition offset in place,
// reporting whether any of them came out negative, which is what the signed
// form of the table exists for. The buffer is one sliceRuns built, never the
// parsed moov, so it is this package's to rewrite.
func (s *sampleTables) shiftComposition(shift int64) (signed bool) {
	for off := 0; off+8 <= len(s.ctts); off += 8 {
		shifted := cttsValue(s.ctts[off+4:], s.cttsSigned) - shift
		if shifted < 0 {
			signed = true
		}
		binary.BigEndian.PutUint32(s.ctts[off+4:], uint32(int32(shifted)))
	}
	return signed
}

// cttsValue reads one composition offset, which is signed in a version 1 table
// and unsigned in a version 0 one.
func cttsValue(entry []byte, signed bool) int64 {
	if signed {
		return int64(int32(be32(entry)))
	}
	return int64(be32(entry))
}

// sliceRuns cuts an eight-byte-per-entry run table, stts or ctts, down to n
// samples from first. Every run overlapping the span is emitted with the count
// of the overlap and the value it already carried: a duration or a composition
// offset means the same thing wherever the samples sit.
func sliceRuns(table []byte, first, n uint32) []byte {
	if len(table) == 0 || n == 0 {
		return nil
	}
	// Counts are summed as 64-bit: a malformed table may declare far more
	// samples than the track holds, and the walk has to stay in range anyway.
	var (
		out   = make([]byte, 0, len(table))
		seen  uint64
		lo    = uint64(first)
		hi    = uint64(first) + uint64(n)
		width = 8
	)
	for off := 0; off+width <= len(table) && seen < hi; off += width {
		count := uint64(be32(table[off:]))
		if from, to := max(seen, lo), min(seen+count, hi); from < to {
			out = binary.BigEndian.AppendUint32(out, uint32(to-from))
			out = append(out, table[off+4:off+8]...)
		}
		seen += count
	}
	return out
}

// sliceSync cuts the sync sample table down to n samples from first,
// renumbering what is left. stss holds one-based sample numbers, so an entry
// stays an entry and only its number moves.
func sliceSync(table []byte, first, n uint32) []byte {
	if len(table) == 0 || n == 0 {
		return nil
	}
	out := make([]byte, 0, len(table))
	for off := 0; off+4 <= len(table); off += 4 {
		number := be32(table[off:])
		if number == 0 || number-1 < first || uint64(number-1) >= uint64(first)+uint64(n) {
			continue
		}
		out = binary.BigEndian.AppendUint32(out, number-first)
	}
	return out
}

// sliceSizes cuts the sample size table down to n samples from first. A
// constant size stays constant and needs no table, and the byte-aligned widths
// are resliced rather than copied.
func (s *sampleTables) sliceSizes(first, n uint32) ([]byte, error) {
	if s.constSize != 0 || n == 0 {
		return nil, nil
	}
	switch s.sizeBits {
	case 32, 16, 8:
		width := uint64(s.sizeBits) / 8
		lo, hi := uint64(first)*width, (uint64(first)+uint64(n))*width
		if hi > uint64(len(s.sizes)) {
			return nil, fmt.Errorf("%w: %d samples from %d of a %d byte size table",
				ErrMalformed, n, first, len(s.sizes))
		}
		return s.sizes[lo:hi], nil
	case 4:
		// Four-bit entries are repacked rather than resliced, because a span
		// starting on an odd sample starts halfway through a byte.
		out := make([]byte, (int(n)+1)/2)
		for i := range n {
			size, err := s.sampleSize(first + i)
			if err != nil {
				return nil, err
			}
			if i%2 == 0 {
				out[i/2] = byte(size) << 4
			} else {
				out[i/2] |= byte(size)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: no sample size table for %d samples", ErrMalformed, s.count)
}
