package writer

import (
	"bytes"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

const SeekHeadReserve = 256

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

	// Find first keyframe for cue
	cued := false
	for i := range blocks {
		if blocks[i].Keyframe {
			m.Cues = append(m.Cues, mkv.CuePoint{
				TimeMs: blocks[i].Timecode, Track: blocks[i].TrackNumber,
				ClusterPos: clusterOffset,
			})
			cued = true
			break
		}
	}

	// Audio-only: no keyframes, cue on first block (max 1 per 500ms per spec)
	if !cued && len(blocks) > 0 {
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
		_, err := m.W.Write(seekData)
		return err
	}

	if _, err := m.W.Seek(m.SegDataStart+m.SeekHeadPos, io.SeekStart); err != nil {
		return err
	}
	if _, err := m.W.Write(seekData); err != nil {
		return err
	}
	remaining := SeekHeadReserve - len(seekData)
	if remaining >= 2 {
		return WriteVoid(m.W, remaining)
	}
	return nil
}

func (m *MKVWriter) WriteMetadata(c *mkv.Container, tracks []mkv.Track, durationMs int64) error {
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
	return nil
}
