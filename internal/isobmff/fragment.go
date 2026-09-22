package isobmff

import "fmt"

// tfhd flags. ISO/IEC 14496-12 8.8.7.1.
const (
	tfhdBaseDataOffset    = 0x000001
	tfhdSampleDescIndex   = 0x000002
	tfhdDefaultDuration   = 0x000008
	tfhdDefaultSize       = 0x000010
	tfhdDefaultFlags      = 0x000020
	tfhdDefaultBaseIsMoof = 0x020000
)

// trun flags. ISO/IEC 14496-12 8.8.8.1.
const (
	trunDataOffset       = 0x000001
	trunFirstSampleFlags = 0x000004
	trunSampleDuration   = 0x000100
	trunSampleSize       = 0x000200
	trunSampleFlags      = 0x000400
	trunSampleCompOffset = 0x000800
)

// sampleNonSync is the sample_is_non_sync_sample bit of a sample flags word.
// ISO/IEC 14496-12 8.8.3.1.
const sampleNonSync = 0x00010000

// maxDefaultedSamples bounds a trun that declares a sample count with no
// per-sample bytes behind it, where the table size cannot bound the count.
const maxDefaultedSamples = 1 << 20

// moofRange locates one movie fragment header in the file.
type moofRange struct {
	// start is the moof box itself, which fragment data offsets are relative to.
	start   int64
	payload int64
	size    int64
}

// trackDefaults are the trex per-track sample defaults a fragment may omit.
// ISO/IEC 14496-12 8.8.3.
type trackDefaults struct {
	duration uint32
	size     uint32
	flags    uint32
}

// trackFragment summarizes one traf. The samples stay in the file: only enough
// to find the fragment covering a time is kept, and the moof is read again when
// a sample from it is actually wanted.
type trackFragment struct {
	moof       int // index into File.moofs
	firstIndex uint32
	baseTime   uint64 // decode time of the first sample, in media ticks
	duration   uint64
	samples    uint32
	bytes      int64
	sync       bool // the fragment holds at least one sync sample
}

// fragSample is one sample described by a trun.
type fragSample struct {
	decode   uint64 // decode time on the media timeline
	duration uint64
	comp     int64 // composition offset from the decode time
	offset   int64
	size     int64
	sync     bool
}

// tfhd is a track fragment header as stored, before the trex defaults are
// folded in. ISO/IEC 14496-12 8.8.7.
type tfhd struct {
	trackID  uint32
	flags    uint32
	base     int64
	duration uint32
	size     uint32
	sample   uint32
}

// parseMvex reads the per-track sample defaults. ISO/IEC 14496-12 8.8.1.
func parseMvex(body []byte, depth int, out map[uint32]trackDefaults) error {
	return walk(body, depth, func(typ string, child []byte) error {
		if typ != "trex" {
			return nil
		}
		_, _, rest, ok := fullBoxVersion(child)
		if !ok || len(rest) < 20 {
			return fmt.Errorf("%w: trex is %d bytes", ErrMalformed, len(child))
		}
		out[be32(rest)] = trackDefaults{
			duration: be32(rest[8:]),
			size:     be32(rest[12:]),
			flags:    be32(rest[16:]),
		}
		return nil
	})
}

// parseFragments reads every moof in turn and records a summary per traf. Each
// moof is read whole under maxMoofBytes and discarded, so a file with thousands
// of fragments costs one fragment at a time plus the summaries.
//
// ponytail: sidx is skipped. It would only be an index onto the same moofs the
// top-level scan already finds by header, so it buys nothing until a file turns
// up where walking the top level is itself too slow.
func (f *File) parseFragments() error {
	// Decode time carries across fragments for a track whose traf omits tfdt.
	next := map[uint32]uint64{}
	for i, m := range f.moofs {
		body, err := readWhole(f.r, m.payload, m.size, maxMoofBytes, "moof")
		if err != nil {
			return err
		}
		if err := f.parseMoof(body, i, m.start, next); err != nil {
			return err
		}
	}
	return nil
}

func (f *File) parseMoof(body []byte, index int, moofStart int64, next map[uint32]uint64) error {
	return walk(body, 1, func(typ string, child []byte) error {
		if typ != "traf" {
			return nil
		}
		return f.parseTraf(child, index, moofStart, next, 2)
	})
}

func (f *File) parseTraf(body []byte, index int, moofStart int64, next map[uint32]uint64, depth int) error {
	head, base, haveBase, truns, err := readTraf(body, depth)
	if err != nil {
		return err
	}
	track := f.trackByID(head.trackID)
	if track == nil {
		return nil
	}
	if !haveBase {
		base = next[head.trackID]
	}

	fragment := trackFragment{
		moof:       index,
		firstIndex: track.fragCount,
		baseTime:   base,
	}
	end := base
	if err := eachTrafSample(head, base, truns, moofStart, track.defaults, func(s fragSample) {
		fragment.samples++
		fragment.bytes += s.size
		fragment.sync = fragment.sync || s.sync
		end = s.decode + s.duration
	}); err != nil {
		return err
	}
	fragment.duration = end - base

	track.fragments = append(track.fragments, fragment)
	track.fragCount += fragment.samples
	track.fragBytes += fragment.bytes
	track.fragDur += fragment.duration
	next[head.trackID] = base + fragment.duration
	return nil
}

// readTraf collects the one tfhd, the optional tfdt, and the truns of a traf.
func readTraf(body []byte, depth int) (head tfhd, base uint64, haveBase bool, truns [][]byte, err error) {
	err = walk(body, depth, func(typ string, child []byte) error {
		switch typ {
		case "tfhd":
			return head.parse(child)
		case "tfdt":
			version, _, rest, ok := fullBoxVersion(child)
			if !ok {
				return fmt.Errorf("%w: tfdt is %d bytes", ErrMalformed, len(child))
			}
			switch {
			case version == 1 && len(rest) >= 8:
				base, haveBase = be64(rest), true
			case version != 1 && len(rest) >= 4:
				base, haveBase = uint64(be32(rest)), true
			default:
				return fmt.Errorf("%w: tfdt v%d body is %d bytes", ErrMalformed, version, len(rest))
			}
		case "trun":
			truns = append(truns, child)
		}
		return nil
	})
	return head, base, haveBase, truns, err
}

func (h *tfhd) parse(body []byte) error {
	_, flags, rest, ok := fullBoxVersion(body)
	if !ok || len(rest) < 4 {
		return fmt.Errorf("%w: tfhd is %d bytes", ErrMalformed, len(body))
	}
	h.flags = flags
	h.trackID = be32(rest)
	rest = rest[4:]

	read := func(width int, dst func([]byte)) error {
		if len(rest) < width {
			return fmt.Errorf("%w: tfhd flags %06x run past its %d byte body", ErrMalformed, flags, len(body))
		}
		dst(rest)
		rest = rest[width:]
		return nil
	}
	if flags&tfhdBaseDataOffset != 0 {
		if err := read(8, func(b []byte) { h.base = int64(be64(b)) }); err != nil {
			return err
		}
	}
	if flags&tfhdSampleDescIndex != 0 {
		if err := read(4, func([]byte) {}); err != nil {
			return err
		}
	}
	if flags&tfhdDefaultDuration != 0 {
		if err := read(4, func(b []byte) { h.duration = be32(b) }); err != nil {
			return err
		}
	}
	if flags&tfhdDefaultSize != 0 {
		if err := read(4, func(b []byte) { h.size = be32(b) }); err != nil {
			return err
		}
	}
	if flags&tfhdDefaultFlags != 0 {
		if err := read(4, func(b []byte) { h.sample = be32(b) }); err != nil {
			return err
		}
	}
	return nil
}

// resolve folds the trex defaults into the fragment header. A traf that declares
// neither a base data offset nor default-base-is-moof still measures from the
// moof, which is what every writer in practice means.
func (h tfhd) resolve(d trackDefaults, moofStart int64) (base int64, duration, size, flags uint32) {
	base = moofStart
	if h.flags&tfhdBaseDataOffset != 0 {
		base = h.base
	} else if h.flags&tfhdDefaultBaseIsMoof != 0 {
		base = moofStart
	}
	duration, size, flags = d.duration, d.size, d.flags
	if h.flags&tfhdDefaultDuration != 0 {
		duration = h.duration
	}
	if h.flags&tfhdDefaultSize != 0 {
		size = h.size
	}
	if h.flags&tfhdDefaultFlags != 0 {
		flags = h.sample
	}
	return base, duration, size, flags
}

// trunSampleWidth is the bytes each trun sample entry occupies, given which
// optional fields the box declares.
func trunSampleWidth(flags uint32) int {
	width := 0
	for _, present := range [...]uint32{trunSampleDuration, trunSampleSize, trunSampleFlags, trunSampleCompOffset} {
		if flags&present != 0 {
			width += 4
		}
	}
	return width
}

// eachTrafSample walks the samples of one traf in order, calling fn for each.
// Nothing is stored: the caller either summarizes or picks one out.
func eachTrafSample(head tfhd, base uint64, truns [][]byte, moofStart int64, d trackDefaults, fn func(fragSample)) error {
	baseOffset, defDuration, defSize, defFlags := head.resolve(d, moofStart)
	pos := baseOffset
	decode := base

	for _, trun := range truns {
		_, flags, rest, ok := fullBoxVersion(trun)
		if !ok || len(rest) < 4 {
			return fmt.Errorf("%w: trun is %d bytes", ErrMalformed, len(trun))
		}
		count := be32(rest)
		rest = rest[4:]

		if flags&trunDataOffset != 0 {
			if len(rest) < 4 {
				return fmt.Errorf("%w: trun declares a data offset it does not hold", ErrMalformed)
			}
			pos = baseOffset + int64(int32(be32(rest)))
			rest = rest[4:]
		}
		firstFlags := defFlags
		if flags&trunFirstSampleFlags != 0 {
			if len(rest) < 4 {
				return fmt.Errorf("%w: trun declares first sample flags it does not hold", ErrMalformed)
			}
			firstFlags = be32(rest)
			rest = rest[4:]
		}

		width := trunSampleWidth(flags)
		if width == 0 && count > maxDefaultedSamples {
			return fmt.Errorf("%w: trun declares %d samples with no per-sample bytes", ErrMalformed, count)
		}
		table, err := entries(rest, count, width)
		if err != nil {
			return err
		}

		for i := range count {
			at := int(i) * width
			sample := fragSample{decode: decode, offset: pos, size: int64(defSize)}
			sampleFlags := defFlags
			if i == 0 && flags&trunFirstSampleFlags != 0 {
				sampleFlags = firstFlags
			}
			duration := defDuration
			if flags&trunSampleDuration != 0 {
				duration = be32(table[at:])
				at += 4
			}
			if flags&trunSampleSize != 0 {
				sample.size = int64(be32(table[at:]))
				at += 4
			}
			if flags&trunSampleFlags != 0 {
				sampleFlags = be32(table[at:])
				at += 4
			}
			if flags&trunSampleCompOffset != 0 {
				sample.comp = int64(int32(be32(table[at:])))
			}
			sample.duration = uint64(duration)
			sample.sync = sampleFlags&sampleNonSync == 0
			fn(sample)

			decode += uint64(duration)
			pos += sample.size
		}
	}
	return nil
}

// fragmentSamples walks one fragment's samples for one track. The moof is read
// again here rather than kept: a long file has thousands of them and only one is
// ever wanted at a time.
func (f *File) fragmentSamples(t *Track, fr trackFragment, fn func(fragSample)) error {
	if fr.moof < 0 || fr.moof >= len(f.moofs) {
		return fmt.Errorf("%w: fragment names moof %d of %d", ErrMalformed, fr.moof, len(f.moofs))
	}
	m := f.moofs[fr.moof]
	body, err := readWhole(f.r, m.payload, m.size, maxMoofBytes, "moof")
	if err != nil {
		return err
	}
	return walk(body, 1, func(typ string, child []byte) error {
		if typ != "traf" {
			return nil
		}
		head, base, haveBase, truns, err := readTraf(child, 2)
		if err != nil || head.trackID != t.ID {
			return err
		}
		if !haveBase {
			base = fr.baseTime
		}
		return eachTrafSample(head, base, truns, m.start, t.defaults, fn)
	})
}
