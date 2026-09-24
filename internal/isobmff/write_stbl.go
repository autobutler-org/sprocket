package isobmff

import "fmt"

// stbl writes a track's sample tables and returns the position of its chunk
// offset field, which the caller fills in once the layout is known.
//
// Most of the tables are the source's own, copied entry for entry: stts, ctts,
// and stss describe the samples themselves and say nothing about where they
// sit, so a remux has nothing to recompute in them, and copying keeps them in
// the compact run form the source chose rather than expanding them per sample.
// Only stsc and stco describe the layout, and the output's layout is one chunk
// per track, so each of those is a single entry.
//
// stsd is copied whole, including any sample entry past the first. The demuxer
// reads only the first one, and the stsc written here points every sample at
// it, so a track with more than one description would come out mislabelled.
// ponytail: nothing in this library produces or reads such a track; carry the
// source's own description indices across if one ever turns up.
func (b *boxWriter) stbl(t *Track) int {
	s := &t.tables
	start := b.open("stbl")

	stsd := b.open("stsd")
	if len(t.stsd) > 0 {
		b.raw(t.stsd)
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

// payloadBytes is the total size of a track's samples, refusing a table that
// cannot account for every one of them. It is what the output's chunk offsets
// are laid out from, so a partial answer here would put every later track's
// samples at the wrong place.
func (s *sampleTables) payloadBytes() (int64, error) { return s.spanBytes(0, s.count) }

// spanBytes is the total stored size of n samples starting at index. A
// constant sample size is multiplied rather than summed, for the reason
// sampleRange gives: an stsz that declares one size and a billion samples has
// no table behind it to bound the loop.
func (s *sampleTables) spanBytes(index, n uint32) (int64, error) {
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

// eachRange hands fn the byte ranges a track's samples occupy, in the order the
// source stores them, one range per chunk.
//
// Chunk by chunk rather than sample by sample: samples sit back to back inside
// a chunk, so their ranges join into one, and a track whose samples are all the
// same size then costs a multiplication per chunk rather than a step per
// sample. That is what keeps a file declaring a constant size and a billion
// samples cheap, since there is no size table there to bound the count. It
// fails rather than copying a prefix when the chunk tables do not reach as far
// as the sample count claims.
func (s *sampleTables) eachRange(fn func(byteRange) error) error {
	if s.count == 0 {
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
			size, err := s.spanBytes(index, held)
			if err != nil {
				return err
			}
			if err := fn(byteRange{offset: offset, size: size}); err != nil {
				return err
			}
			index += held
		}
	}

	if index != s.count {
		return fmt.Errorf("%w: the chunk tables place %d of %d samples", ErrMalformed, index, s.count)
	}
	return nil
}
