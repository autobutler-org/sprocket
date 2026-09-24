package matroska

import (
	"fmt"
	"io"
	"math"

	"github.com/autobutler-org/sprocket/internal/isobmff"
)

// Copy writes a Matroska source into w as a Matroska or WebM file: the same
// header, track descriptions, and clusters the source has, with the blocks
// moved across as bytes. Nothing is re-encoded and no frame is looked at.
//
// cut is the window to keep, which TrimSpan picks; a nil cut writes the file
// whole. A cut output's timestamps start at zero, as they do on the ISOBMFF
// side, each track rebased onto its own first block as TrimSpan.rebase
// describes.
//
// The output follows the source's own cluster boundaries rather than
// re-grouping, so what is held at once is one cluster's block descriptors and
// no payload. A block goes out as a SimpleBlock carrying the source's own flags
// byte and everything after it, which is what keeps a laced block laced and a
// keyframe a keyframe without taking either apart.
//
// Only video and audio tracks are carried over; a subtitle or timecode track is
// dropped, and so is the BlockDuration or ReferenceBlock of a BlockGroup, which
// describe a timeline the output states for itself. A source with no video or
// audio track returns ErrUnsupportedSource, and a codec the target cannot hold
// returns ErrIncompatibleCodec.
func Copy(w io.Writer, src *File, target isobmff.Target, cut *TrimSpan) error {
	docType, ok := docTypeFor[target]
	if !ok {
		return fmt.Errorf("%w: %q is not a container this writer produces", isobmff.ErrUnsupportedTarget, target)
	}
	span := whole(src.Tracks)
	if cut != nil {
		span = *cut
	}
	tracks, err := copiedTracks(src, target, span)
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

	if err := writeAll(out, copiedInfo(src, span), trackEntries(tracks)); err != nil {
		return err
	}
	cues, err := copyClusters(out, src, tracks, span, segmentStart)
	if err != nil {
		return err
	}
	return writeAll(out, cuesElement(cues))
}

// copiedTracks describes every track the output carries. A Matroska source
// already states what a TrackEntry needs, so the description is carried across
// rather than derived, track numbers included: the blocks name those numbers
// and they go out as they stand.
func copiedTracks(src *File, target isobmff.Target, span TrimSpan) ([]*outTrack, error) {
	out := make([]*outTrack, 0, len(src.Tracks))
	for _, t := range src.Tracks {
		if _, kept := span.First[t.Number]; !kept {
			continue
		}
		if !isobmff.CodecFits(t.Codec, target) {
			return nil, fmt.Errorf("%w: track %d carries %s, which does not fit in %s",
				isobmff.ErrIncompatibleCodec, t.Number, t.Codec, target)
		}
		out = append(out, &outTrack{
			number: t.Number, typ: t.Type, codecID: t.CodecID, codecPrivate: t.CodecPrivate,
			defaultDuration: t.DefaultDuration,
			width:           uint64(max(t.Width, 0)), height: uint64(max(t.Height, 0)),
			sampleRate: t.SampleRate, channels: t.Channels, lacing: t.Lacing,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: it carries no video or audio track", isobmff.ErrUnsupportedSource)
	}
	return out, nil
}

// copiedInfo builds the segment information. A whole-file copy keeps the
// duration the source declares, so the output probes to what the input probed
// to; a cut states the window it kept, measured on the output's own timeline.
func copiedInfo(src *File, span TrimSpan) []byte {
	duration := src.SegmentDuration
	if span.Last != math.MaxInt64 {
		duration = float64(span.Last - span.Origin)
		if declared := src.SegmentDuration - float64(span.Origin); declared > 0 {
			duration = min(duration, declared)
		}
	}
	// The source's ticks are copied across unchanged, so the output has to
	// declare the same scale they were measured on.
	var d docWriter
	start := d.open(idInfo)
	d.integer(idTimestampScal, src.TimestampScale)
	d.text(0x4D80, "sprocket") // MuxingApp
	d.text(0x5741, "sprocket") // WritingApp
	d.float(idDuration, max(duration, 0))
	d.close(start)
	return d.buf
}

// copyClusters walks the source's clusters and writes the blocks each one keeps,
// returning the seek index for them. A cluster left empty by the cut is not
// written at all.
func copyClusters(out *countingWriter, src *File, tracks []*outTrack, span TrimSpan, segmentStart int64) ([]cuePoint, error) {
	var (
		cues    []cuePoint
		blocks  = make([]outBlock, 0, 64)
		buffer  = make([]byte, writeBufferSize)
		video   = videoTrackOf(tracks)
		cursor  = src.newClusterCursor()
		numbers = make(map[uint64]bool, len(tracks))
	)
	for _, t := range tracks {
		numbers[t.number] = true
	}

	for {
		payload, ok, err := cursor.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return cues, nil
		}

		blocks = blocks[:0]
		clusterTicks := int64(math.MaxInt64)
		if err := src.eachBlock(payload, func(b block) error {
			if !numbers[b.track] || !span.keeps(b.track, b.ticks) {
				return nil
			}
			if len(blocks) >= maxClusterBlocks {
				return fmt.Errorf("%w: a cluster of over %d blocks", ErrMalformed, maxClusterBlocks)
			}
			ticks := span.rebase(b.track, b.ticks)
			clusterTicks = min(clusterTicks, ticks)
			blocks = append(blocks, outBlock{
				track: b.track, ticks: ticks, keyframe: b.keyframe, flags: b.flags,
				offset: b.body.start, size: b.body.size,
			})
			return nil
		}); err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			continue
		}

		at, err := writeCluster(out, src.r, blocks, clusterTicks, buffer)
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
