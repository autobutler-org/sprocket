// Package matroska parses the Matroska family: mkv and webm. One parser covers
// both, which differ by the DocType in their EBML header and by what codecs
// they are allowed to carry, not by structure.
//
// Parse takes an io.ReaderAt and a size and never reads the file whole. It
// reads the EBML header, walks the segment's children by header, and reads the
// Info and Tracks elements into memory under caps this package chooses. The
// seek index is read on demand, by the keyframe lookup that needs it, so
// probing costs a handful of reads whatever the length of the file. Clusters
// are never read whole: their blocks are walked by header and only the one
// frame a caller asked for is pulled off disk.
//
// Times come in two shapes and the distinction matters. Matroska measures
// everything in ticks of Info's TimestampScale, which is nanoseconds per tick
// and is a millisecond in practice. Fields named in ticks are on that scale;
// methods that return a time.Duration have already done the conversion.
//
// # What Matroska does not carry
//
// There is no display matrix, so Rotation is not a question this container can
// answer and this package does not pretend otherwise. A Projection element
// exists and its ProjectionPoseRoll can express a rotation, but it is part of
// the spherical video signaling rather than the orientation flag a phone
// writes, and nothing outside 360 degree video sets it, so it is not read.
//
// There is no sample table either. A block carries its own timestamp and a
// keyframe flag, the Cues element indexes the keyframes, and nothing states up
// front how many frames a track holds or how long each one runs. Frame rate is
// derived as FrameRate documents.
package matroska

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Track types, from the TrackType element. RFC 9559 section 5.1.4.1.5.
const (
	trackVideo = 1
	trackAudio = 2
)

// defaultTimestampScale is the TimestampScale a file that declares none is read
// at: one millisecond per tick. RFC 9559 section 5.1.2.7.
const defaultTimestampScale = 1_000_000

// codecNames maps a Matroska CodecID onto this package's stable short name,
// the same scheme the ISOBMFF side reports. An ID that is not on the list
// reports itself, so a caller can log something useful.
var codecNames = map[string]string{
	"V_MPEG4/ISO/AVC":  "h264",
	"V_MPEGH/ISO/HEVC": "hevc",
	"V_VP8":            "vp8",
	"V_VP9":            "vp9",
	"V_AV1":            "av1",
	"A_OPUS":           "opus",
	"A_VORBIS":         "vorbis",
	"A_MPEG/L3":        "mp3",
	"A_AC3":            "ac-3",
	"A_EAC3":           "ec-3",
	// fLaC is the spelling the ISOBMFF side reports, from the sample entry's
	// own four-character code, and the two have to agree for the remux
	// compatibility table to be one table.
	"A_FLAC": "fLaC",
}

// File is a parsed Matroska file. It keeps the reader it was parsed from, so it
// stays valid only as long as that reader does.
type File struct {
	// DocType is the EBML header's document type: "matroska" or "webm".
	DocType string
	// TimestampScale is nanoseconds per tick, from Info. Every timestamp in the
	// file is on this scale.
	TimestampScale uint64
	// SegmentDuration is Info's duration in TimestampScale ticks. It is a float
	// because the element is one; it is 0 when the file declares none.
	SegmentDuration float64
	// Tracks are the TrackEntry elements in file order.
	Tracks []*Track

	r    io.ReaderAt
	size int64
	// segment spans the segment's payload: where its children start and where
	// they end. Cue positions are measured from start.
	segment span
	// cues spans the Cues element's payload, read on demand. size is 0 when the
	// file has none.
	cues span
	// firstCluster is where the first cluster's header begins, or -1.
	firstCluster int64
	// scannedRate is the frame rate measured by counting blocks, set only when
	// the video track declares no DefaultDuration. See FrameRate.
	scannedRate float64
}

// span is a half-open byte range in the file.
type span struct{ start, size int64 }

// end is one past the last byte of the span.
func (s span) end() int64 { return s.start + s.size }

// Track is one TrackEntry: what it carries and how to decode it.
type Track struct {
	// Number is the TrackNumber a block names to say it belongs here.
	Number uint64
	// Type is the TrackType: trackVideo, trackAudio, or something this package
	// does not look at.
	Type uint64
	// CodecID is the Matroska codec identifier as stored, such as
	// "V_MPEG4/ISO/AVC".
	CodecID string
	// Codec is the stable short name: h264, hevc, av1, vp8, vp9, aac, opus,
	// vorbis, mp3. An unrecognized CodecID reports itself.
	Codec string
	// CodecPrivate is the codec configuration record as stored, aliasing the
	// parsed Tracks element: avcC for h264, hvcC for hevc, av1C for av1, the
	// raw AudioSpecificConfig for aac. Treat it as read-only; it is nil when
	// the track carries none.
	CodecPrivate []byte
	// Width and Height are PixelWidth and PixelHeight, set for a video track
	// only. Matroska stores no rotation, so these are what a player shows.
	Width, Height int
	// SampleRate and Channels are set for an audio track only.
	SampleRate float64
	Channels   uint64
	// DefaultDuration is the nominal nanoseconds per frame, or 0 when the track
	// declares none.
	DefaultDuration uint64
	// NALLengthSize is the width in bytes of the length prefix on each NAL unit
	// in a frame, 1 through 4, for h264 and hevc. Zero for everything else,
	// where a frame is the codec's own bitstream with no framing added.
	NALLengthSize int
}

// Parse reads the structure of a Matroska file. It reads the EBML header, the
// segment's element headers, and the Info and Tracks elements, and nothing
// else; frame payload is left where it is.
func Parse(r io.ReaderAt, size int64) (*File, error) {
	head, err := readEBMLHeader(r, size)
	if err != nil {
		return nil, err
	}
	f := &File{
		DocType:        head.docType,
		TimestampScale: defaultTimestampScale,
		r:              r,
		size:           size,
		firstCluster:   -1,
	}
	if err := f.findSegment(head.end); err != nil {
		return nil, err
	}

	info, tracks, err := f.locate()
	if err != nil {
		return nil, err
	}
	if err := f.parseInfo(info); err != nil {
		return nil, err
	}
	if err := f.parseTracks(tracks); err != nil {
		return nil, err
	}
	if len(f.Tracks) == 0 {
		return nil, fmt.Errorf("%w: the segment declares no track", ErrMalformed)
	}
	return f, f.measureFrameRate()
}

// ebmlHeader is what the file's first element says about itself.
type ebmlHeader struct {
	docType string
	// end is the offset just past the header, where the segment starts.
	end int64
}

// docTypes are the document types this package reads. Anything else is another
// EBML document, such as a WebVTT sidecar, and is not this package's business.
var docTypes = map[string]bool{"matroska": true, "webm": true}

// readEBMLHeader reads the file's first element, which decides whether this is
// worth walking at all.
func readEBMLHeader(r io.ReaderAt, size int64) (ebmlHeader, error) {
	if size < 2 {
		return ebmlHeader{}, fmt.Errorf("%w: %d bytes is too short to hold an element", ErrNotMatroska, size)
	}
	var buf [maxElementHeader]byte
	read := min(size, maxElementHeader)
	if _, err := r.ReadAt(buf[:read], 0); err != nil {
		return ebmlHeader{}, fmt.Errorf("%w: reading the first element header: %w", ErrNotMatroska, err)
	}
	e, err := parseElement(buf[:read], size)
	if err != nil {
		return ebmlHeader{}, fmt.Errorf("%w: %w", ErrNotMatroska, err)
	}
	if e.id != idEBML || e.size == unknownSize {
		return ebmlHeader{}, fmt.Errorf("%w: it begins with element %#x", ErrNotMatroska, e.id)
	}

	body, err := readWhole(r, e.hdrSize, e.size, maxHeaderBytes, "the EBML header")
	if err != nil {
		return ebmlHeader{}, err
	}
	head := ebmlHeader{end: e.hdrSize + e.size}
	if err := walk(body, 1, func(id uint32, child []byte) error {
		if id == idDocType {
			head.docType = strings.TrimRight(string(child), "\x00")
		}
		return nil
	}); err != nil {
		return ebmlHeader{}, fmt.Errorf("%w: %w", ErrNotMatroska, err)
	}
	if !docTypes[head.docType] {
		return ebmlHeader{}, fmt.Errorf("%w: its document type is %q", ErrNotMatroska, head.docType)
	}
	return head, nil
}

// findSegment locates the segment, which is the only element after the header
// this package cares about. A Void or a CRC-32 may sit in front of it.
func (f *File) findSegment(at int64) error {
	return scan(f.r, at, f.size-at, maxDepth, func(e element, off int64) error {
		switch e.id {
		case idVoid, idCRC32:
			return nil
		case idSegment:
			f.segment = span{start: off + e.hdrSize, size: e.end(off, f.size-off) - off - e.hdrSize}
			return errStopScan
		default:
			return fmt.Errorf("%w: element %#x where the segment should be", ErrNotMatroska, e.id)
		}
	})
}

// locate walks the segment's children by header and returns the Info and Tracks
// payloads, reading each whole under its own cap. It also records where the
// Cues element and the first cluster are, without reading either.
//
// The walk stops at the first cluster once everything has been located, which
// is what keeps probing a long file cheap: a SeekHead at the front names where
// the Cues are, so the clusters in between never have to be stepped over.
// Without one the walk runs to the end of the segment, which costs a read per
// cluster header and nothing else.
func (f *File) locate() (info, tracks []byte, err error) {
	var seen struct{ info, tracks, cues, seekHead bool }
	err = scan(f.r, f.segment.start, f.segment.size, maxSegmentChildren, func(e element, off int64) error {
		body := span{start: off + e.hdrSize, size: e.end(off, f.segment.end()-off) - off - e.hdrSize}
		switch e.id {
		case idSeekHead:
			seen.seekHead = true
			return f.readSeekHead(body, &seen.cues)
		case idInfo:
			info, err = readWhole(f.r, body.start, body.size, maxInfoBytes, "the segment information")
			seen.info = true
			return err
		case idTracks:
			tracks, err = readWhole(f.r, body.start, body.size, maxTracksBytes, "the track descriptions")
			seen.tracks = true
			return err
		case idCues:
			// The whole element, header included: the seek index is read by
			// decoding its header, because the SeekHead route to it gives a
			// position and no size.
			f.cues, seen.cues = span{start: off, size: e.end(off, f.segment.end()-off) - off}, true
		case idCluster:
			if f.firstCluster < 0 {
				f.firstCluster = off
			}
			// An unknown-size cluster is the last thing this walk can locate:
			// resolving where it ends means walking its blocks, and nothing
			// after it is reachable by arithmetic.
			if e.size == unknownSize || (seen.info && seen.tracks && (seen.cues || seen.seekHead)) {
				return errStopScan
			}
		}
		return nil
	})
	switch {
	case err != nil:
		return nil, nil, err
	case !seen.info:
		return nil, nil, fmt.Errorf("%w: the segment carries no Info element", ErrMalformed)
	case !seen.tracks:
		return nil, nil, fmt.Errorf("%w: the segment carries no Tracks element", ErrMalformed)
	}
	return info, tracks, nil
}

// readSeekHead reads the index of the segment's own children and records where
// the Cues are, so a file that keeps them at the end does not have to be walked
// to find them. A seek entry that points outside the segment is ignored rather
// than trusted.
func (f *File) readSeekHead(at span, foundCues *bool) error {
	body, err := readWhole(f.r, at.start, at.size, maxSeekHeadBytes, "the seek index")
	if err != nil {
		return err
	}
	return walk(body, 1, func(id uint32, seek []byte) error {
		if id != idSeek {
			return nil
		}
		var target, position uint64
		if err := walk(seek, 2, func(id uint32, child []byte) error {
			var err error
			switch id {
			case idSeekID:
				target, err = readUint(child)
			case idSeekPosition:
				position, err = readUint(child)
			}
			return err
		}); err != nil {
			return err
		}
		if target != idCues || position > uint64(f.segment.size) {
			return nil
		}
		// The position is measured from the start of the segment's payload, and
		// the element it names carries its own size, so only its header offset
		// is known here.
		start := f.segment.start + int64(position)
		f.cues = span{start: start, size: f.segment.end() - start}
		*foundCues = true
		return nil
	})
}

// parseInfo reads the segment information: the timestamp scale every other
// number in the file is on, and the duration.
func (f *File) parseInfo(body []byte) error {
	return walk(body, 1, func(id uint32, child []byte) error {
		switch id {
		case idTimestampScal:
			scale, err := readUint(child)
			if err != nil {
				return err
			}
			if scale == 0 {
				return fmt.Errorf("%w: a timestamp scale of zero", ErrMalformed)
			}
			f.TimestampScale = scale
		case idDuration:
			duration, err := readFloat(child)
			if err != nil {
				return err
			}
			if duration < 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
				return fmt.Errorf("%w: a duration of %v ticks", ErrMalformed, duration)
			}
			f.SegmentDuration = duration
		}
		return nil
	})
}

// parseTracks reads every TrackEntry.
func (f *File) parseTracks(body []byte) error {
	return walk(body, 1, func(id uint32, entry []byte) error {
		if id != idTrackEntry {
			return nil
		}
		t, err := parseTrackEntry(entry)
		if err != nil {
			return err
		}
		f.Tracks = append(f.Tracks, t)
		return nil
	})
}

func parseTrackEntry(body []byte) (*Track, error) {
	t := &Track{}
	err := walk(body, 2, func(id uint32, child []byte) error {
		var err error
		switch id {
		case idTrackNumber:
			t.Number, err = readUint(child)
		case idTrackType:
			t.Type, err = readUint(child)
		case idCodecID:
			t.CodecID = strings.TrimRight(string(child), "\x00")
		case idCodecPrivate:
			t.CodecPrivate = child
		case idDefaultDur:
			t.DefaultDuration, err = readUint(child)
		case idVideo:
			err = t.parseVideo(child)
		case idAudio:
			err = t.parseAudio(child)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	if t.Number == 0 {
		return nil, fmt.Errorf("%w: a track entry with no track number", ErrMalformed)
	}
	t.Codec = codecName(t.CodecID)
	t.NALLengthSize = nalLengthSize(t.Codec, t.CodecPrivate)
	return t, nil
}

func (t *Track) parseVideo(body []byte) error {
	return walk(body, 3, func(id uint32, child []byte) error {
		value, err := readUint(child)
		if err != nil {
			return err
		}
		switch id {
		case idPixelWidth:
			t.Width = int(min(value, math.MaxInt32))
		case idPixelHeight:
			t.Height = int(min(value, math.MaxInt32))
		}
		return nil
	})
}

func (t *Track) parseAudio(body []byte) error {
	return walk(body, 3, func(id uint32, child []byte) error {
		var err error
		switch id {
		case idSamplingFreq:
			t.SampleRate, err = readFloat(child)
		case idChannels:
			t.Channels, err = readUint(child)
		}
		return err
	})
}

// codecName maps a CodecID onto the stable short name. The AAC identifiers are
// a family rather than one string: the modern spelling is "A_AAC" and the
// legacy ones name the profile after it, as in "A_AAC/MPEG4/LC".
func codecName(id string) string {
	if strings.HasPrefix(id, "A_AAC") {
		return "aac"
	}
	if name, ok := codecNames[id]; ok {
		return name
	}
	return id
}

// nalLengthSize reads the length prefix width out of an avcC or hvcC record,
// which is what a Matroska file stores as the CodecPrivate of an H.264 or HEVC
// track. ISO/IEC 14496-15 5.3.3.1 and 8.3.3.1.
func nalLengthSize(codec string, config []byte) int {
	switch {
	case codec == "h264" && len(config) >= 5:
		return int(config[4]&0x03) + 1
	case codec == "hevc" && len(config) >= 23:
		return int(config[21]&0x03) + 1
	default:
		return 0
	}
}

// Duration reports the segment duration. A file that declares none reports
// zero: without a sample table there is nothing else to derive it from short of
// walking every cluster to the last block, which is a cost Probe exists to
// avoid.
func (f *File) Duration() time.Duration {
	return f.ticks(f.SegmentDuration)
}

// ticks converts a count on the file's timestamp scale to wall time.
func (f *File) ticks(count float64) time.Duration {
	nanos := count * float64(f.TimestampScale)
	if nanos <= 0 || nanos > float64(math.MaxInt64) {
		return 0
	}
	return time.Duration(nanos)
}

// VideoTrack returns the first video track, or nil.
func (f *File) VideoTrack() *Track { return f.trackByType(trackVideo) }

// AudioTrack returns the first audio track, or nil.
func (f *File) AudioTrack() *Track { return f.trackByType(trackAudio) }

func (f *File) trackByType(want uint64) *Track {
	for _, t := range f.Tracks {
		if t.Type == want {
			return t
		}
	}
	return nil
}

// FrameRate reports the video track's frames per second, or 0 when the file has
// no video track or no duration.
//
// Matroska has no sample table, so there are two ways to the answer and this
// package takes the cheap one where the file offers it. A track that declares a
// DefaultDuration, which every muxer writes, reports 1e9 over it: that is the
// nominal rate rather than a measured one, so a variable-framerate file is
// described by whatever rate its muxer nominated. A track that declares none is
// measured instead, by counting the video blocks across the file and dividing
// by the duration, which costs a walk of every cluster's block headers at Parse
// time and is bounded by maxScanClusters clusters.
func (f *File) FrameRate() float64 {
	video := f.VideoTrack()
	if video == nil {
		return 0
	}
	if video.DefaultDuration > 0 {
		return float64(time.Second) / float64(video.DefaultDuration)
	}
	return f.scannedRate
}

// measureFrameRate counts the video blocks when the track declares no nominal
// frame duration. It is the only walk of the clusters Parse ever does, and it
// reads block headers rather than block payloads.
func (f *File) measureFrameRate() error {
	video := f.VideoTrack()
	seconds := f.Duration().Seconds()
	if video == nil || video.DefaultDuration > 0 || seconds <= 0 {
		return nil
	}
	var frames int64
	if err := f.eachCluster(func(cluster span, _ int64) error {
		return f.eachBlock(cluster, func(b block) error {
			if b.track == video.Number {
				frames += int64(b.frames)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	f.scannedRate = float64(frames) / seconds
	return nil
}

// eachCluster calls fn for every cluster in the segment, with the span of its
// payload and the offset of its header. The walk is bounded by maxScanClusters
// and stops when fn returns errStopScan.
func (f *File) eachCluster(fn func(payload span, at int64) error) error {
	if f.firstCluster < 0 {
		return nil
	}
	return scanResolving(f.r, f.firstCluster, f.segment.end()-f.firstCluster, maxScanClusters,
		f.clusterEnd,
		func(e element, off int64) error {
			if e.id != idCluster {
				return nil
			}
			start := off + e.hdrSize
			return fn(span{start: start, size: f.clusterEnd(e, off) - start}, off)
		})
}

// clusterEnd is where a cluster's payload ends. A cluster that declares its
// size says so; one that does not, which a live muxer writes, is walked to the
// first element that is not a cluster child.
func (f *File) clusterEnd(e element, off int64) int64 {
	if e.size != unknownSize {
		return off + e.hdrSize + e.size
	}
	end := f.segment.end()
	_ = scan(f.r, off+e.hdrSize, end-off-e.hdrSize, maxClusterChildren, func(child element, at int64) error {
		if clusterChildren[child.id] {
			return nil
		}
		end = at
		return errStopScan
	})
	return end
}

// clusterChildren are the elements that may appear directly inside a cluster.
// Anything else ends an unknown-size one. RFC 9559 section 5.1.6.
var clusterChildren = map[uint32]bool{
	idCRC32: true, idVoid: true, idTimestamp: true, idPosition: true,
	idPrevSize: true, idSilentTracks: true, idSimpleBlock: true, idBlockGroup: true,
}
