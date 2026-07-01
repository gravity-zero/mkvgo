package writer

// stream_writer.go — live streaming write to a plain io.Writer.
//
// StreamWriter writes a valid MKV/WebM stream to any io.Writer (pipe, network
// socket, HTTP response body…) without ever seeking back. The layout is:
//
//   EBML header (known size)
//   Segment (unknown size — 0x01FF…FF)
//     Info (known size)
//     Tracks (known size)
//     Cluster (unknown size — repeated per cluster)
//       Timestamp
//       SimpleBlock …
//     Cluster (unknown size)
//       …
//
// No SeekHead and no Cues are written: both require knowing future byte
// offsets, which is impossible without Seek. Decoders that need random access
// must use the seekable MKVWriter instead.
//
// Cluster boundaries: a new cluster is opened automatically for each block
// flagged as a keyframe (typical for video streams). Callers that want explicit
// control can call FlushCluster() to close the current cluster at any point.
//
// Thread safety: StreamWriter is NOT safe for concurrent use.

import (
	"fmt"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// StreamWriter writes a live MKV stream to an io.Writer.
type StreamWriter struct {
	w             io.Writer
	timecodeScale int64
	inCluster     bool
	clusterTS     int64 // timestamp of current cluster, in milliseconds
}

// NewStreamWriter writes the EBML header, an unknown-size Segment, and the
// Info + Tracks metadata elements to w, then returns a *StreamWriter ready to
// accept blocks.
//
// info.TimecodeScale must be > 0 (typically 1_000_000 for millisecond
// precision). The Duration field of info is intentionally ignored: a live
// stream has unknown duration.
func NewStreamWriter(w io.Writer, info mkv.SegmentInfo, tracks []mkv.Track) (*StreamWriter, error) {
	return newStreamWriter(w, info, tracks, WriteEBMLHeader)
}

// NewWebMStreamWriter is like NewStreamWriter but produces a complete WebM
// stream: it first validates that every track uses a WebM-compatible codec
// (mkv.ValidateWebM), then writes the "webm" DocType header at the right version
// (4 if AV1, else 2). The streaming layout it emits — Info + Tracks + Clusters,
// with no chapters/attachments/tags/SeekHead — is already within the WebM
// element subset, so the result is a real, playable .webm (frames included),
// unlike the metadata-only writer.WriteWebM.
func NewWebMStreamWriter(w io.Writer, info mkv.SegmentInfo, tracks []mkv.Track) (*StreamWriter, error) {
	probe := &mkv.Container{Tracks: tracks}
	if err := mkv.ValidateWebM(probe); err != nil {
		return nil, err
	}
	version := mkv.WebMDocTypeVersion(probe)
	header := func(w io.Writer) error {
		return writeEBMLHeaderDocType(w, "webm", version, 2)
	}
	return newStreamWriter(w, info, tracks, header)
}

func newStreamWriter(w io.Writer, info mkv.SegmentInfo, tracks []mkv.Track, writeHeader func(io.Writer) error) (*StreamWriter, error) {
	if info.TimecodeScale <= 0 {
		info.TimecodeScale = 1_000_000
	}

	// EBML header.
	if err := writeHeader(w); err != nil {
		return nil, fmt.Errorf("stream writer: EBML header: %w", err)
	}

	// Segment with unknown size.
	if _, err := ebml.WriteElementID(w, mkv.IDSegment); err != nil {
		return nil, fmt.Errorf("stream writer: Segment ID: %w", err)
	}
	if _, err := ebml.WriteDataSize(w, -1); err != nil {
		return nil, fmt.Errorf("stream writer: Segment size: %w", err)
	}

	// Info — clear Duration so we do not write a wrong value.
	infoCopy := info
	infoCopy.Duration = 0
	if err := WriteSegmentInfo(w, &infoCopy, 0); err != nil {
		return nil, fmt.Errorf("stream writer: Info: %w", err)
	}

	// Tracks.
	if len(tracks) > 0 {
		if err := WriteTracks(w, tracks); err != nil {
			return nil, fmt.Errorf("stream writer: Tracks: %w", err)
		}
	}

	return &StreamWriter{
		w:             w,
		timecodeScale: info.TimecodeScale,
	}, nil
}

// openCluster writes the header of a new unknown-size Cluster with the given
// cluster timestamp (in milliseconds, converted to raw timecode units).
func (s *StreamWriter) openCluster(tsMs int64) error {
	rawTS := uint64(tsMs * 1_000_000 / s.timecodeScale)
	s.clusterTS = tsMs

	// Cluster ID + unknown size.
	if _, err := ebml.WriteElementID(s.w, mkv.IDCluster); err != nil {
		return err
	}
	if _, err := ebml.WriteDataSize(s.w, -1); err != nil {
		return err
	}

	// Timestamp element (required first element in cluster).
	return WriteUintElement(s.w, mkv.IDTimestamp, rawTS)
}

// FlushCluster closes the current cluster without writing anything (unknown-size
// clusters are terminated by the next cluster's header or by EOF/Close). This
// is a no-op in terms of bytes — it simply resets the internal state so that
// the NEXT WriteBlock call opens a new cluster.
//
// Callers should use FlushCluster when they want to force a cluster boundary
// at a non-keyframe block (e.g. for audio-only streams).
func (s *StreamWriter) FlushCluster() {
	s.inCluster = false
}

// relativeTimecode expresses b.Timecode relative to the current cluster start,
// as the signed 16-bit value a SimpleBlock encodes. Block timecodes are
// milliseconds internally; the offset is stored in raw timecode-scale units,
// like the cluster Timestamp. A SimpleBlock's timecode field is an int16, so
// the offset must fit in [math.MinInt16, math.MaxInt16] (±~32 s at millisecond
// scale). Beyond that, a blind int16() cast would wrap and silently corrupt
// the timestamp, so this returns an error instead: the caller must open a
// nearer cluster (a keyframe block via WriteBlock, or FlushCluster) before
// writing.
func (s *StreamWriter) relativeTimecode(b mkv.Block) (int16, error) {
	delta := (b.Timecode - s.clusterTS) * 1_000_000 / s.timecodeScale
	if delta < math.MinInt16 || delta > math.MaxInt16 {
		return 0, fmt.Errorf("stream writer: block timecode %dms is %+d timecode units from cluster start %dms, outside SimpleBlock's int16 range; open a new cluster", b.Timecode, delta, s.clusterTS)
	}
	return int16(delta), nil
}

// WriteBlock appends a block to the stream. A new cluster is opened
// automatically when:
//   - No cluster has been opened yet (first block ever).
//   - The block is a keyframe (b.Keyframe == true) and already in a cluster.
//
// b.Timecode is in milliseconds (matching the mkv.Block convention). It returns
// an error if b.Timecode is more than int16 milliseconds (±~32 s) from the
// current cluster's start, since a SimpleBlock cannot encode a larger offset;
// keeping clusters short (keyframes, or FlushCluster) holds offsets in range.
func (s *StreamWriter) WriteBlock(b mkv.Block) error {
	// Start a new cluster on keyframe or on the very first block.
	if !s.inCluster || b.Keyframe {
		if err := s.openCluster(b.Timecode); err != nil {
			return fmt.Errorf("stream writer: open cluster: %w", err)
		}
		s.inCluster = true
	}

	relTC, err := s.relativeTimecode(b)
	if err != nil {
		return err
	}
	return WriteSimpleBlock(s.w, b.TrackNumber, relTC, b.Keyframe, b.Data)
}

// WriteBlockInCurrentCluster appends a block to the current cluster without
// triggering a new cluster even if b.Keyframe is true. Useful for multiplexed
// streams where video keyframes should not split audio blocks mid-way.
//
// If no cluster is open, one is opened first.
//
// Because this method never opens a new cluster for a keyframe, the caller owns
// cluster boundaries: it errors (like WriteBlock) if b.Timecode lands outside
// int16 range relative to the current cluster start, so long-running clusters
// must be split with FlushCluster to keep block offsets encodable.
func (s *StreamWriter) WriteBlockInCurrentCluster(b mkv.Block) error {
	if !s.inCluster {
		if err := s.openCluster(b.Timecode); err != nil {
			return fmt.Errorf("stream writer: open cluster: %w", err)
		}
		s.inCluster = true
	}
	relTC, err := s.relativeTimecode(b)
	if err != nil {
		return err
	}
	return WriteSimpleBlock(s.w, b.TrackNumber, relTC, b.Keyframe, b.Data)
}

// Close flushes any buffered data and signals the end of the stream. For a
// streaming io.Writer the "end" is simply EOF on the reader's side; Close only
// ensures any underlying buffered writer is flushed.
//
// Close does NOT write any terminating marker: unknown-size elements in EBML
// are terminated by the consumer reaching EOF. If w implements io.Closer,
// Close calls w.Close().
func (s *StreamWriter) Close() error {
	s.inCluster = false
	if c, ok := s.w.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
