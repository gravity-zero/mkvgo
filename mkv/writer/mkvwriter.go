package writer

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// SeekHeadReserve is the byte budget reserved at the head of the Segment for
// the SeekHead written by Finalize. When the final SeekHead does not fit (more
// entries than the reserve allows), it is appended at the END of the file
// instead of the reserved spot - still valid, but readers discover it later.
const SeekHeadReserve = 256

// MetadataReserve is the Void padding WriteMetadata leaves after the metadata
// elements, so a later in-place edit (ops.EditInPlace) that grows the metadata
// - a longer title, added tags or chapters - still fits without a full
// rewrite. mkvpropedit reserves space the same way.
const MetadataReserve = 1024

type MKVWriter struct {
	W             io.WriteSeeker
	SegDataStart  int64
	SeekHeadPos   int64
	InfoPos       int64
	TracksPos     int64
	ChaptersPos   int64
	AttachPos     int64
	TagsPos       int64
	CuesPos       int64
	Cues          []mkv.CuePoint
	TimecodeScale int64
	videoTracks   map[uint64]bool // set by WriteMetadata; cue selection keys on these
	chaptersSlot  int             // bytes booked by ReserveChapters, 0 if none
}

func NewMKVWriter(w io.WriteSeeker) *MKVWriter {
	return &MKVWriter{W: w}
}

func (m *MKVWriter) pos() int64 {
	p, _ := m.W.Seek(0, io.SeekCurrent)
	return p
}

func (m *MKVWriter) RelPos() int64 {
	return m.pos() - m.SegDataStart
}

func (m *MKVWriter) WriteStart() error {
	if err := WriteEBMLHeader(m.W); err != nil {
		return err
	}
	return m.startSegment()
}

// WriteStartWebM is WriteStart with a "webm" DocType header at the given
// DocTypeVersion (see mkv.WebMDocTypeVersion).
func (m *MKVWriter) WriteStartWebM(version uint64) error {
	if err := writeEBMLHeaderDocType(m.W, "webm", version, 2); err != nil {
		return err
	}
	return m.startSegment()
}

func (m *MKVWriter) startSegment() error {
	if _, err := ebml.WriteElementID(m.W, mkv.IDSegment); err != nil {
		return err
	}
	if _, err := ebml.WriteDataSize(m.W, -1); err != nil {
		return err
	}
	m.SegDataStart = m.pos()
	m.SeekHeadPos = m.RelPos()
	return WriteVoid(m.W, SeekHeadReserve)
}

const minCueIntervalMs = 500

func (m *MKVWriter) WriteClusterWithCues(clusterTS int64, timecodeScale int64, blocks []mkv.Block) error {
	clusterOffset := m.RelPos()

	// Cue the cluster's first VIDEO keyframe. Every audio block carries the
	// keyframe flag, so keying on "first keyframe-flagged block" cued audio in
	// mixed clusters - cue times then named the audio block, not the seek
	// target, skewing every consumer of the index (same bug class as the
	// reindex fix; here in the writer).
	cued := false
	for i := range blocks {
		if blocks[i].Keyframe && (m.videoTracks == nil || m.videoTracks[blocks[i].TrackNumber]) {
			m.Cues = append(m.Cues, mkv.CuePoint{
				TimeMs: blocks[i].Timecode, Track: blocks[i].TrackNumber,
				ClusterPos: clusterOffset,
			})
			cued = true
			break
		}
	}

	// Audio-only file (no video track declared): cue on the first block,
	// throttled (max 1 per 500ms per spec). A video file's cluster without a
	// video keyframe is NOT cued - a mid-GOP cue is a false seek target.
	if !cued && len(blocks) > 0 && len(m.videoTracks) == 0 {
		lastCueTime := int64(-minCueIntervalMs - 1)
		if len(m.Cues) > 0 {
			lastCueTime = m.Cues[len(m.Cues)-1].TimeMs
		}
		if blocks[0].Timecode-lastCueTime >= minCueIntervalMs {
			m.Cues = append(m.Cues, mkv.CuePoint{
				TimeMs: blocks[0].Timecode, Track: blocks[0].TrackNumber,
				ClusterPos: clusterOffset,
			})
		}
	}

	return WriteCluster(m.W, clusterTS, timecodeScale, blocks)
}

// WriteTagsElement writes a Tags element at the current position (typically
// after the clusters, for tags only known once the media has streamed - e.g.
// per-track statistics) and records it for the SeekHead, so head-only readers
// following SeekHead→Tags find it without a cluster scan.
func (m *MKVWriter) WriteTagsElement(tags []mkv.Tag) error {
	if len(tags) == 0 {
		return nil
	}
	pos := m.RelPos()
	if err := WriteTags(m.W, tags); err != nil {
		return err
	}
	if m.TagsPos == 0 {
		m.TagsPos = pos
	}
	return nil
}

func (m *MKVWriter) Finalize() error {
	if len(m.Cues) > 0 {
		m.CuesPos = m.RelPos()
		if err := WriteCues(m.W, m.Cues, m.TimecodeScale); err != nil {
			return err
		}
	}

	var entries []SeekEntry
	add := func(id uint32, pos int64) {
		if pos > 0 {
			entries = append(entries, SeekEntry{id, pos})
		}
	}
	add(mkv.IDInfo, m.InfoPos)
	add(mkv.IDTracks, m.TracksPos)
	add(mkv.IDChapters, m.ChaptersPos)
	add(mkv.IDAttachments, m.AttachPos)
	add(mkv.IDTags, m.TagsPos)
	add(mkv.IDCues, m.CuesPos)

	var buf bytes.Buffer
	if err := WriteSeekHead(&buf, entries); err != nil {
		return err
	}

	seekData := buf.Bytes()
	if len(seekData) > SeekHeadReserve {
		if _, err := m.W.Write(seekData); err != nil {
			return err
		}
		return m.sealSegmentSize(m.RelPos())
	}

	// Capture the segment's final extent BEFORE seeking back into the file:
	// everything below writes inside already-written regions.
	segSize := m.RelPos()
	if _, err := m.W.Seek(m.SegDataStart+m.SeekHeadPos, io.SeekStart); err != nil {
		return err
	}
	if _, err := m.W.Write(seekData); err != nil {
		return err
	}
	remaining := SeekHeadReserve - len(seekData)
	if remaining >= 2 {
		if err := WriteVoid(m.W, remaining); err != nil {
			return err
		}
	}
	return m.sealSegmentSize(segSize)
}

// sealSegmentSize replaces the Segment's unknown-size marker (written by
// startSegment as an 8-byte all-ones VINT) with the real segment data size,
// at the same 8-byte width, so finished files declare their extent the way
// every mainstream muxer does. Readers stop at the declared end - which is
// what lets the in-place operations (ReindexInPlace, RetimeTracks) hide
// their transient journal past it.
func (m *MKVWriter) sealSegmentSize(segSize int64) error {
	if _, err := m.W.Seek(m.SegDataStart-8, io.SeekStart); err != nil {
		return err
	}
	if segSize >= int64(1)<<56-1 {
		return fmt.Errorf("segment size %d does not fit an 8-byte VINT", segSize)
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(segSize)|uint64(1)<<56)
	_, err := m.W.Write(buf[:])
	return err
}

func (m *MKVWriter) WriteMetadata(c *mkv.Container, tracks []mkv.Track, durationMs int64) error {
	m.videoTracks = map[uint64]bool{}
	for _, t := range tracks {
		if t.Type == mkv.VideoTrack {
			m.videoTracks[t.ID] = true
		}
	}
	m.TimecodeScale = c.Info.TimecodeScale
	if m.TimecodeScale == 0 {
		m.TimecodeScale = 1000000
	}
	m.InfoPos = m.RelPos()
	if err := WriteSegmentInfo(m.W, &c.Info, durationMs); err != nil {
		return err
	}
	if len(tracks) > 0 {
		m.TracksPos = m.RelPos()
		if err := WriteTracks(m.W, tracks); err != nil {
			return err
		}
	}
	if len(c.Chapters) > 0 {
		m.ChaptersPos = m.RelPos()
		if err := WriteChapters(m.W, c.Chapters); err != nil {
			return err
		}
	}
	if len(c.Attachments) > 0 {
		m.AttachPos = m.RelPos()
		if err := WriteAttachments(m.W, c.Attachments); err != nil {
			return err
		}
	}
	if len(c.Tags) > 0 {
		m.TagsPos = m.RelPos()
		if err := WriteTags(m.W, c.Tags); err != nil {
			return err
		}
	}
	// Padding for later in-place metadata edits (see MetadataReserve).
	return WriteVoid(m.W, MetadataReserve)
}

// ReserveChapters books room in the HEAD for a Chapters element whose timestamps
// are not known yet, and WriteReservedChapters fills it in once they are. That
// is the case of a concatenation: a chapter must be shifted by the offset at
// which its source's blocks really landed, and that offset is only known after
// those blocks have been written.
//
// The alternative - writing Chapters after the clusters, like Mux does for its
// statistics Tags - would put the element where ops.EditInPlace folds only Tags
// back into the head, leaving a second Chapters element behind. Keeping the
// conventional layout avoids that entirely.
//
// The slot is sized for the widest timestamps EBML can encode, so no offset can
// overflow it, plus two bytes so the leftover can always hold a Void.
// ReserveChapters is a no-op for an empty list, and so is its counterpart.
func (m *MKVWriter) ReserveChapters(chapters []mkv.Chapter) error {
	if len(chapters) == 0 {
		return nil
	}
	size, err := maxChaptersSize(chapters)
	if err != nil {
		return err
	}
	m.ChaptersPos = m.RelPos()
	m.chaptersSlot = size
	return WriteVoid(m.W, size)
}

// WriteReservedChapters writes chapters into the slot ReserveChapters booked and
// voids what is left of it, then returns to the end of the file. chapters must be
// the list handed to ReserveChapters, with the timestamps it was waiting for:
// anything that does not fit is refused rather than written over the clusters.
func (m *MKVWriter) WriteReservedChapters(chapters []mkv.Chapter) error {
	if m.chaptersSlot == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := WriteChapters(&buf, chapters); err != nil {
		return err
	}
	if buf.Len() > m.chaptersSlot {
		return fmt.Errorf("chapters need %d bytes, slot holds %d", buf.Len(), m.chaptersSlot)
	}
	end := m.pos()
	if _, err := m.W.Seek(m.SegDataStart+m.ChaptersPos, io.SeekStart); err != nil {
		return err
	}
	if _, err := m.W.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := WriteVoid(m.W, m.chaptersSlot-buf.Len()); err != nil {
		return err
	}
	_, err := m.W.Seek(end, io.SeekStart)
	return err
}

// maxChaptersSize is the size the list would serialise to with every timestamp
// at EBML's widest encoding, which no real offset can exceed, plus the two bytes
// a Void needs to fill whatever the real write leaves over.
func maxChaptersSize(chapters []mkv.Chapter) (int, error) {
	var buf bytes.Buffer
	if err := WriteChapters(&buf, widenChapterTimes(chapters)); err != nil {
		return 0, err
	}
	return buf.Len() + 2, nil
}

// widestChapterMs, multiplied by the nanosecond factor WriteChapters applies,
// lands between 2^56 and 2^63: the widest a Matroska uint is written, and still
// clear of the overflow the multiplication would hit near the top of the range.
const widestChapterMs = 100_000_000_000

func widenChapterTimes(chapters []mkv.Chapter) []mkv.Chapter {
	out := make([]mkv.Chapter, len(chapters))
	for i, ch := range chapters {
		out[i] = ch
		out[i].StartMs = widestChapterMs
		// A zero end writes no element at all: keep it zero, or the slot would
		// be sized for an element the real write never produces.
		if ch.EndMs > 0 {
			out[i].EndMs = widestChapterMs
		}
		out[i].SubChapters = widenChapterTimes(ch.SubChapters)
	}
	return out
}
