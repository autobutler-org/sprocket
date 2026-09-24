package matroska

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// docTypeVersion is what a reader has to understand to read what this writer
// produces. Version 4 is the current Matroska one; WebM's own profile is at 2
// and everything written here is inside it.
const (
	matroskaDocTypeVersion = 4
	webmDocTypeVersion     = 2
	docTypeReadVersion     = 2
)

// Writer limits. The output is written in one pass, so these bound what is held
// while it is built rather than what the source may contain.
const (
	// outputTimestampScale is the TimestampScale the output declares:
	// nanoseconds per tick, so a tick is a millisecond. It is what every muxer
	// writes, and a block's timestamp is a signed 16 bit offset from its
	// cluster's, so a coarser scale would buy nothing and a finer one would
	// shorten how long a cluster may run.
	outputTimestampScale = 1_000_000
	// clusterSpan is how much movie time a cluster covers before the next
	// keyframe starts a new one.
	clusterSpan = 1000 * time.Millisecond
	// clusterHardSpan is how much it may cover when no keyframe turns up. A
	// block's timestamp is a signed 16 bit offset from its cluster's, so a
	// cluster can never run past about 32 seconds whatever this says.
	clusterHardSpan = 5000 * time.Millisecond
	// maxClusterBlocks bounds what one cluster holds, and so what the writer
	// holds at once: one descriptor per block, no payload.
	maxClusterBlocks = 1 << 13
	// maxCuePoints bounds the seek index the writer accumulates. A keyframe
	// every two seconds reaches it after about eleven hours.
	//
	// ponytail: a cue index that stops rather than one that thins out. A file
	// past this keeps the cues it has, so seeking into its tail falls back to
	// the cluster scan, which is correct and slower.
	maxCuePoints = 1 << 15
	// writeBufferSize is the one buffer the payload copy uses, whatever the
	// size of the file.
	writeBufferSize = 256 << 10
)

// codecIDs maps this library's codec short names onto Matroska CodecIDs. It is
// the reverse of codecNames, and it is what decides whether a track can be
// written at all: a codec with no identifier here has no name in this
// container.
var codecIDs = map[string]string{
	"h264":   "V_MPEG4/ISO/AVC",
	"hevc":   "V_MPEGH/ISO/HEVC",
	"vp8":    "V_VP8",
	"vp9":    "V_VP9",
	"av1":    "V_AV1",
	"aac":    "A_AAC",
	"opus":   "A_OPUS",
	"vorbis": "A_VORBIS",
	"mp3":    "A_MPEG/L3",
	"ac-3":   "A_AC3",
	"ec-3":   "A_EAC3",
	"fLaC":   "A_FLAC",
	"alac":   "A_ALAC",
}

// docTypeFor is the EBML document type each target declares.
var docTypeFor = map[isobmff.Target]string{
	isobmff.TargetMKV:  "matroska",
	isobmff.TargetWebM: "webm",
}

// Write writes an ISOBMFF source into w as a Matroska or WebM file: an EBML
// header, then a segment holding the track descriptions, the clusters, and a
// seek index at the end. Nothing is re-encoded and no frame is looked at.
//
// ranges cuts each track down to a span of its samples, keyed by source track
// ID, exactly as isobmff.Write takes it. A track with no entry, and so every
// track when the map is nil, is written whole. A cut output's timestamps start
// at zero, as they do on the ISOBMFF side.
//
// The segment declares an unknown size and the seek index goes at the end.
// Matroska allows both, and together they are what makes the output writable in
// one pass: nothing has to be known before the header goes out except the
// duration, which the source's movie header already states. Each cluster does
// declare its size, which costs holding one cluster's block descriptors, a few
// thousand at most and no payload, while it is measured.
//
// Only video and audio tracks are carried over; a timecode or subtitle track is
// dropped. A fragmented source, or one with no video or audio track, returns
// ErrUnsupportedSource. A codec the target cannot hold returns
// ErrIncompatibleCodec, and one this writer cannot name a CodecID or a
// CodecPrivate for returns ErrUnsupportedSource.
func Write(w io.Writer, src *isobmff.File, target isobmff.Target, ranges map[uint32]isobmff.Range) error {
	docType, ok := docTypeFor[target]
	if !ok {
		return fmt.Errorf("%w: %q is not a container this writer produces", isobmff.ErrUnsupportedTarget, target)
	}
	if src.Fragmented {
		return fmt.Errorf("%w: the source is fragmented, and its samples are described by moof boxes rather than by the moov's tables",
			isobmff.ErrUnsupportedSource)
	}

	tracks, err := outputTracks(src, target, ranges)
	if err != nil {
		return err
	}

	out := &countingWriter{w: w}
	if err := writeAll(out, header(docType), segmentHeader()); err != nil {
		return err
	}
	// Positions in the seek index are measured from here, the first byte of the
	// segment's payload.
	segmentStart := out.written

	if err := writeAll(out, infoElement(src, tracks), trackEntries(tracks)); err != nil {
		return err
	}
	cues, err := writeClusters(out, src, tracks, segmentStart)
	if err != nil {
		return err
	}
	return writeAll(out, cuesElement(cues))
}

// outTrack is one output track: everything its TrackEntry declares, and, for a
// source that has one, the sample cursor the interleave walks it with.
//
// The description is held as the fields Matroska itself stores rather than as a
// source track, because two families feed this writer: an ISOBMFF file, whose
// samples the interleave below pulls, and another Matroska file, whose blocks
// are copied across as they stand.
type outTrack struct {
	// number is the TrackNumber the output gives it, counting from one.
	number uint64
	// typ is the TrackType: trackVideo or trackAudio.
	typ uint64
	// codecID and codecPrivate are what the TrackEntry declares.
	codecID      string
	codecPrivate []byte
	// defaultDuration is the nominal nanoseconds per frame, or 0 when there is
	// nothing to declare.
	defaultDuration uint64
	// width and height describe a video track, sampleRate and channels an audio
	// one.
	width, height uint64
	sampleRate    float64
	channels      uint64
	// lacing is the FlagLacing the output declares, set for a track whose
	// blocks may pack several frames into one.
	lacing bool

	// src is the ISOBMFF track the samples come from, nil for a Matroska
	// source. first and last are the source sample indexes the output keeps,
	// and origin is subtracted from every timestamp so that a cut starts at
	// zero.
	src         *isobmff.Track
	first, last uint32
	origin      time.Duration

	// next is the interleave cursor: the sample this track offers next.
	next uint32
	// done reports that the cursor has passed last.
	done bool
}

// outputTracks works out what to write for every track the output carries.
func outputTracks(src *isobmff.File, target isobmff.Target, ranges map[uint32]isobmff.Range) ([]*outTrack, error) {
	out := make([]*outTrack, 0, len(src.Tracks))
	for _, t := range src.Tracks {
		if t.Handler != "vide" && t.Handler != "soun" {
			continue
		}
		if !isobmff.CodecFits(t.Entry.Codec, target) {
			return nil, fmt.Errorf("%w: track %d carries %s, which does not fit in %s",
				isobmff.ErrIncompatibleCodec, t.ID, t.Entry.Codec, target)
		}
		codecID, ok := codecIDs[t.Entry.Codec]
		if !ok {
			return nil, fmt.Errorf("%w: track %d carries %s, which has no Matroska codec identifier here",
				isobmff.ErrUnsupportedSource, t.ID, t.Entry.Codec)
		}
		private, err := codecPrivate(t)
		if err != nil {
			return nil, err
		}

		entry := &outTrack{
			number: uint64(len(out) + 1), typ: trackTypeOf(t.Handler),
			codecID: codecID, codecPrivate: private,
			width: uint64(t.Width), height: uint64(t.Height),
			sampleRate: float64(t.Entry.SampleRate), channels: uint64(max(t.Entry.ChannelCount, 1)),
			src: t, last: t.SampleCount() - 1,
		}
		if rate := t.FrameRate(); t.Handler == "vide" && rate > 0 {
			entry.defaultDuration = uint64(float64(time.Second) / rate)
		}
		if span, cut := ranges[t.ID]; cut {
			if span.Last < span.First || span.Last >= t.SampleCount() {
				return nil, fmt.Errorf("%w: track %d has %d samples, so the range %d..%d is not in it",
					isobmff.ErrMalformed, t.ID, t.SampleCount(), span.First, span.Last)
			}
			entry.first, entry.last = span.First, span.Last
			sample, err := t.Sample(span.First)
			if err != nil {
				return nil, err
			}
			// The cut is anchored on when the first kept sample is displayed,
			// not on when it is decoded. A cut starts on a keyframe, and a
			// keyframe is the first frame of its group in display order, so its
			// composition time is the earliest in the span and subtracting it
			// puts the output's first picture at zero. Anchoring on the decode
			// time instead would leave the video a couple of frames of encoder
			// delay behind the audio, which is the gap the source's edit list
			// was there to hide.
			entry.origin = t.MovieTime(sample.Composition, src.Timescale)
		}
		entry.next = entry.first
		if t.SampleCount() == 0 {
			entry.done = true
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: it carries no video or audio track", isobmff.ErrUnsupportedSource)
	}
	return out, nil
}

// codecPrivate is the CodecPrivate a track's codec wants. Most of them take the
// ISOBMFF configuration record verbatim, because Matroska adopted the same
// records; AAC takes the AudioSpecificConfig out of the esds, and Opus takes
// the identification header the dOps box holds the body of.
func codecPrivate(t *isobmff.Track) ([]byte, error) {
	switch t.Entry.Codec {
	case "h264", "hevc", "av1":
		if len(t.Entry.Config) == 0 {
			return nil, fmt.Errorf("%w: track %d carries %s with no configuration record",
				isobmff.ErrUnsupportedSource, t.ID, t.Entry.Codec)
		}
		return t.Entry.Config, nil
	case "aac":
		if len(t.Entry.DecoderConfig) == 0 {
			return nil, fmt.Errorf("%w: track %d carries AAC with no decoder specific information in its esds",
				isobmff.ErrUnsupportedSource, t.ID)
		}
		return t.Entry.DecoderConfig, nil
	case "opus":
		return opusHead(t)
	default:
		// mp3, ac-3, and ec-3 carry everything a decoder needs in the stream
		// itself, so Matroska stores no private data for them.
		return nil, nil
	}
}

// opusHead builds the Opus identification header, which is what Matroska stores
// as an Opus track's CodecPrivate. The ISOBMFF side keeps the same fields in a
// dOps box, which is the header from its version byte on with the magic
// dropped and the multi-byte fields big-endian rather than little.
// RFC 7845 section 5.1 and ISO/IEC 14496-12's Opus encapsulation.
func opusHead(t *isobmff.Track) ([]byte, error) {
	// A dOps is a version byte, the channel count, a 16 bit pre-skip, a 32 bit
	// input sample rate, a 16 bit output gain, and the channel mapping family.
	const dOpsHead = 11
	config := t.Entry.Config
	if len(config) < dOpsHead {
		return nil, fmt.Errorf("%w: track %d carries Opus with a %d byte dOps, need at least %d",
			isobmff.ErrUnsupportedSource, t.ID, len(config), dOpsHead)
	}

	head := append([]byte("OpusHead"), 1, config[1])
	head = binary.LittleEndian.AppendUint16(head, binary.BigEndian.Uint16(config[2:]))
	head = binary.LittleEndian.AppendUint32(head, binary.BigEndian.Uint32(config[4:]))
	head = binary.LittleEndian.AppendUint16(head, binary.BigEndian.Uint16(config[8:]))
	head = append(head, config[10])
	// A mapping family other than zero is followed by the channel mapping
	// table, which is the same bytes in both records.
	return append(head, config[dOpsHead:]...), nil
}

// header builds the EBML header, which names the document type and the limits
// every element in the file stays inside.
func header(docType string) []byte {
	version := uint64(matroskaDocTypeVersion)
	if docType == "webm" {
		version = webmDocTypeVersion
	}
	var d docWriter
	start := d.open(idEBML)
	d.integer(0x4286, 1) // EBMLVersion
	d.integer(0x42F7, 1) // EBMLReadVersion
	d.integer(0x42F2, maxElementIDBytes)
	d.integer(0x42F3, maxElementSizeBytes)
	d.text(idDocType, docType)
	d.integer(0x4287, version) // DocTypeVersion
	d.integer(0x4285, docTypeReadVersion)
	d.close(start)
	return d.buf
}

// segmentHeader opens the segment with an unknown size, so that the header can
// go out before anything after it is known.
func segmentHeader() []byte {
	return append(elementID(idSegment), unknownSizeByte)
}

// infoElement builds the segment information: the scale every timestamp is on, and the
// duration in ticks of it.
func infoElement(src *isobmff.File, tracks []*outTrack) []byte {
	var d docWriter
	start := d.open(idInfo)
	d.integer(idTimestampScal, outputTimestampScale)
	d.text(0x4D80, "sprocket") // MuxingApp
	d.text(0x5741, "sprocket") // WritingApp
	d.float(idDuration, float64(outputDuration(src, tracks))/float64(tickDuration))
	d.close(start)
	return d.buf
}

// outputDuration is how long the output runs. A whole-file write keeps the
// source's own movie duration, so the output probes to what the input probed
// to; a cut has to derive it, and the movie runs as long as its longest track.
func outputDuration(src *isobmff.File, tracks []*outTrack) time.Duration {
	var cut bool
	var longest time.Duration
	for _, t := range tracks {
		if t.first == 0 && t.last == t.src.SampleCount()-1 {
			continue
		}
		cut = true
		last, err := t.src.Sample(t.last)
		if err != nil {
			continue
		}
		end := t.src.MovieTime(last.Decode, src.Timescale) - t.origin
		// The last sample's own length is not in the tables as a duration, so
		// the track's average frame time stands in for it. It is a frame of
		// slack on a cut, which is the same slack Trim already documents.
		if rate := t.src.FrameRate(); rate > 0 {
			end += time.Duration(float64(time.Second) / rate)
		}
		longest = max(longest, end)
	}
	if !cut {
		return src.Duration()
	}
	return longest
}

// trackEntries builds the Tracks element.
func trackEntries(tracks []*outTrack) []byte {
	var d docWriter
	start := d.open(idTracks)
	for _, t := range tracks {
		d.trackEntry(t)
	}
	d.close(start)
	return d.buf
}

func (d *docWriter) trackEntry(t *outTrack) {
	start := d.open(idTrackEntry)
	d.integer(idTrackNumber, t.number)
	d.integer(0x73C5, t.number) // TrackUID
	d.integer(idTrackType, t.typ)
	// FlagLacing says whether a reader should expect laced blocks. This writer
	// packs one frame per block from an ISOBMFF source, but a block copied out
	// of another Matroska file may be laced, so the flag follows the track.
	d.integer(0x9C, boolean(t.lacing))
	d.text(idCodecID, t.codecID)
	if len(t.codecPrivate) > 0 {
		d.binary(idCodecPrivate, t.codecPrivate)
	}
	if t.defaultDuration > 0 {
		d.integer(idDefaultDur, t.defaultDuration)
	}
	if t.typ == trackVideo {
		video := d.open(idVideo)
		d.integer(idPixelWidth, t.width)
		d.integer(idPixelHeight, t.height)
		d.close(video)
	} else {
		audio := d.open(idAudio)
		d.float(idSamplingFreq, t.sampleRate)
		d.integer(idChannels, max(t.channels, 1))
		d.close(audio)
	}
	d.close(start)
}

func trackTypeOf(handler string) uint64 {
	if handler == "soun" {
		return trackAudio
	}
	return trackVideo
}

// outBlock is one block waiting to be written: where its frame lives in the
// source and what its header will say. No payload is held.
type outBlock struct {
	track    uint64
	ticks    int64
	keyframe bool
	// flags is the block's own flags byte, which carries the keyframe bit and
	// the lacing mode. A block copied out of another Matroska file keeps the
	// source's, so that its lacing survives.
	flags  byte
	offset int64
	size   int64
}

// cuePoint is one entry of the seek index, accumulated as the clusters go out.
type cuePoint struct {
	ticks    int64
	track    uint64
	cluster  int64
	relative int64
}

// writeClusters writes every cluster and returns the seek index for them.
//
// Samples are interleaved by decode time across the tracks, which is the order
// a player wants them in, and a block's own timestamp is its composition time,
// which is when it is shown. The two differ wherever a codec reorders frames,
// and Matroska stores exactly that pair: storage order from the interleave,
// presentation time in the block.
func writeClusters(out *countingWriter, src *isobmff.File, tracks []*outTrack, segmentStart int64) ([]cuePoint, error) {
	var (
		cues    []cuePoint
		blocks  = make([]outBlock, 0, 64)
		buffer  = make([]byte, writeBufferSize)
		video   = videoTrackOf(tracks)
		pending *pendingSample
	)
	for {
		blocks = blocks[:0]
		clusterTicks := int64(math.MaxInt64)

		for len(blocks) < maxClusterBlocks {
			next := pending
			if next == nil {
				found, err := nextSample(tracks, src.Timescale)
				if err != nil {
					return nil, err
				}
				next = found
			}
			pending = nil
			if next == nil {
				break
			}
			ticks := durationTicks(next.show)
			if len(blocks) == 0 {
				clusterTicks = ticks
			} else if closesCluster(next, video, durationTicks(next.decode), clusterTicks) {
				pending = next
				break
			}
			// A block's timestamp is a signed 16 bit offset from its cluster's,
			// so one that cannot be expressed starts a cluster of its own.
			if offset := ticks - clusterTicks; offset < math.MinInt16 || offset > math.MaxInt16 {
				pending = next
				break
			}
			clusterTicks = min(clusterTicks, ticks)
			var flags byte
			if next.sample.Sync {
				flags = blockKeyframeFlag
			}
			blocks = append(blocks, outBlock{
				track: next.track.number, ticks: ticks, keyframe: next.sample.Sync, flags: flags,
				offset: next.sample.Offset, size: next.sample.Size,
			})
		}
		if len(blocks) == 0 {
			return cues, nil
		}

		at, err := writeCluster(out, src.ReaderAt(), blocks, clusterTicks, buffer)
		if err != nil {
			return nil, err
		}
		for i, b := range blocks {
			if b.keyframe && video != nil && b.track == video.number && len(cues) < maxCuePoints {
				cues = append(cues, cuePoint{
					ticks: b.ticks, track: b.track,
					cluster: at.cluster - segmentStart, relative: at.blocks[i],
				})
			}
		}
	}
}

// closesCluster reports whether a sample should start a new cluster rather than
// join the one being built. A cluster runs for about a second and prefers to
// begin on a keyframe, so that a seek lands on one; a stretch with no keyframe
// in it is broken up anyway once it runs long enough.
func closesCluster(next *pendingSample, video *outTrack, decodeTicks, clusterTicks int64) bool {
	elapsed := time.Duration(decodeTicks-clusterTicks) * tickDuration
	if elapsed >= clusterHardSpan {
		return true
	}
	return elapsed >= clusterSpan && video != nil && next.track == video && next.sample.Sync
}

// clusterLayout is where a written cluster and its blocks ended up, which is
// what the seek index records.
type clusterLayout struct {
	// cluster is the offset of the cluster's element header in the file.
	cluster int64
	// blocks are each block's element offset from the cluster's payload start.
	blocks []int64
}

// writeCluster writes one cluster: its header and timestamp, then a block
// header and a copy of the frame for each block. The payload moves as byte
// ranges through the caller's buffer, so a cluster costs its descriptors and
// nothing per byte of media.
func writeCluster(out *countingWriter, reader io.ReaderAt, blocks []outBlock, clusterTicks int64, buffer []byte) (clusterLayout, error) {
	timestamp := integerElement(idTimestamp, uint64(max(clusterTicks, 0)))

	payload := int64(len(timestamp))
	for _, b := range blocks {
		payload += blockElementSize(b)
	}

	at := clusterLayout{cluster: out.written, blocks: make([]int64, len(blocks))}
	if err := writeAll(out, elementHeader(idCluster, payload), timestamp); err != nil {
		return clusterLayout{}, err
	}
	start := out.written - int64(len(timestamp))

	for i, b := range blocks {
		at.blocks[i] = out.written - start
		if err := writeAll(out, blockHeader(b, clusterTicks)); err != nil {
			return clusterLayout{}, err
		}
		copied, err := io.CopyBuffer(out, io.NewSectionReader(reader, b.offset, b.size), buffer)
		if err != nil {
			return clusterLayout{}, fmt.Errorf("copying %d bytes of payload at %d: %w", b.size, b.offset, err)
		}
		if copied != b.size {
			return clusterLayout{}, fmt.Errorf("%w: copied %d of the %d bytes at %d", ErrTruncated, copied, b.size, b.offset)
		}
	}
	return at, nil
}

// blockElementSize is what one block takes up, header and frame together.
func blockElementSize(b outBlock) int64 {
	return int64(len(blockHeader(b, 0))) + b.size
}

// blockHeader builds a SimpleBlock's element header and the block header inside
// it: the track number, the timestamp relative to the cluster, and the flags.
func blockHeader(b outBlock, clusterTicks int64) []byte {
	offset := uint16(b.ticks - clusterTicks)
	inner := append(trackNumberVint(b.track), byte(offset>>8), byte(offset), b.flags)
	return append(elementHeader(idSimpleBlock, int64(len(inner))+b.size), inner...)
}

// trackNumberVint encodes a track number the way a block header stores it,
// which is the same variable-length integer an element size uses.
func trackNumberVint(number uint64) []byte {
	if number < 0x80 {
		return []byte{byte(number) | 0x80}
	}
	return sizeVint(number)
}

// cuesElement builds the seek index. An index with nothing in it is written as
// an empty Cues, which says the file has none rather than leaving a reader to
// guess.
func cuesElement(cues []cuePoint) []byte {
	var d docWriter
	start := d.open(idCues)
	for _, c := range cues {
		point := d.open(idCuePoint)
		d.integer(idCueTime, uint64(max(c.ticks, 0)))
		positions := d.open(idCueTrackPos)
		d.integer(idCueTrack, c.track)
		d.integer(idCueClusterPos, uint64(c.cluster))
		d.integer(idCueRelPos, uint64(c.relative))
		d.close(positions)
		d.close(point)
	}
	d.close(start)
	return d.buf
}

// pendingSample is the sample one track offers the interleave next.
type pendingSample struct {
	track  *outTrack
	sample isobmff.Sample
	// decode and show are the sample's times on the output timeline.
	decode, show time.Duration
}

// nextSample takes the sample with the earliest decode time from whichever
// track holds it, and advances that track's cursor. It returns nil when every
// track is spent.
func nextSample(tracks []*outTrack, movieTimescale uint32) (*pendingSample, error) {
	var best *pendingSample
	for _, t := range tracks {
		if t.done || t.next > t.last {
			t.done = true
			continue
		}
		sample, err := t.src.Sample(t.next)
		if err != nil {
			return nil, err
		}
		found := &pendingSample{
			track:  t,
			sample: sample,
			decode: t.src.MovieTime(sample.Decode, movieTimescale) - t.origin,
			show:   t.src.MovieTime(sample.Composition, movieTimescale) - t.origin,
		}
		if best == nil || found.decode < best.decode {
			best = found
		}
	}
	if best != nil {
		best.track.next++
	}
	return best, nil
}

func videoTrackOf(tracks []*outTrack) *outTrack {
	for _, t := range tracks {
		if t.typ == trackVideo {
			return t
		}
	}
	return nil
}

// boolean is an EBML flag as an integer element holds it.
func boolean(set bool) uint64 {
	if set {
		return 1
	}
	return 0
}

// tickDuration is one output tick in wall time.
const tickDuration = outputTimestampScale * time.Nanosecond

// durationTicks converts wall time to the output's timestamp scale, rounding to
// nearest so that a frame does not drift a tick earlier than its neighbours.
func durationTicks(d time.Duration) int64 {
	if d >= 0 {
		return int64((d + tickDuration/2) / tickDuration)
	}
	return int64((d - tickDuration/2) / tickDuration)
}

// countingWriter counts what has gone out, which is what the seek index needs
// and what an unknown-size segment leaves no other way of knowing.
type countingWriter struct {
	w       io.Writer
	written int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// writeAll writes each part in turn, reporting the first failure.
func writeAll(w io.Writer, parts ...[]byte) error {
	for _, part := range parts {
		if _, err := w.Write(part); err != nil {
			return fmt.Errorf("writing the output: %w", err)
		}
	}
	return nil
}
