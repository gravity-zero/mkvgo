package writer

import (
	"bytes"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

type ew struct {
	bytes.Buffer
	err error
}

func (e *ew) uint(id uint32, val uint64) {
	if e.err != nil {
		return
	}
	e.err = WriteUintElement(&e.Buffer, id, val)
}

func (e *ew) float64(id uint32, val float64) {
	if e.err != nil {
		return
	}
	e.err = WriteFloatElement(&e.Buffer, id, val)
}

func (e *ew) str(id uint32, val string) {
	if e.err != nil {
		return
	}
	e.err = WriteStringElement(&e.Buffer, id, val)
}

func (e *ew) raw(id uint32, val []byte) {
	if e.err != nil {
		return
	}
	e.err = WriteBytesElement(&e.Buffer, id, val)
}

func (e *ew) master(id uint32, fn func(*ew)) {
	if e.err != nil {
		return
	}
	var child ew
	fn(&child)
	if child.err != nil {
		e.err = child.err
		return
	}
	e.err = WriteMasterElement(&e.Buffer, id, child.Bytes())
}

func (e *ew) flush(w io.Writer, id uint32) error {
	if e.err != nil {
		return e.err
	}
	return WriteMasterElement(w, id, e.Bytes())
}

func Write(w io.Writer, c *mkv.Container) error {
	if err := WriteEBMLHeader(w); err != nil {
		return err
	}
	var seg bytes.Buffer
	if err := WriteSegmentInfo(&seg, &c.Info, c.DurationMs); err != nil {
		return err
	}
	if len(c.Tracks) > 0 {
		if err := WriteTracks(&seg, c.Tracks); err != nil {
			return err
		}
	}
	if len(c.Chapters) > 0 {
		if err := WriteChapters(&seg, c.Chapters); err != nil {
			return err
		}
	}
	if len(c.Attachments) > 0 {
		if err := WriteAttachments(&seg, c.Attachments); err != nil {
			return err
		}
	}
	if len(c.Tags) > 0 {
		if err := WriteTags(&seg, c.Tags); err != nil {
			return err
		}
	}
	return WriteMasterElement(w, mkv.IDSegment, seg.Bytes())
}

func WriteUintElement(w io.Writer, id uint32, val uint64) error {
	n := ebml.UintLen(val)
	if _, err := ebml.WriteElementHeader(w, id, int64(n)); err != nil {
		return err
	}
	_, err := ebml.WriteUint(w, val, n)
	return err
}

func WriteFloatElement(w io.Writer, id uint32, val float64) error {
	if _, err := ebml.WriteElementHeader(w, id, 8); err != nil {
		return err
	}
	_, err := ebml.WriteFloat(w, val)
	return err
}

func WriteStringElement(w io.Writer, id uint32, val string) error {
	if _, err := ebml.WriteElementHeader(w, id, int64(len(val))); err != nil {
		return err
	}
	_, err := ebml.WriteString(w, val)
	return err
}

func WriteBytesElement(w io.Writer, id uint32, val []byte) error {
	if _, err := ebml.WriteElementHeader(w, id, int64(len(val))); err != nil {
		return err
	}
	_, err := ebml.WriteBytes(w, val)
	return err
}

func WriteMasterElement(w io.Writer, id uint32, children []byte) error {
	if _, err := ebml.WriteElementHeader(w, id, int64(len(children))); err != nil {
		return err
	}
	_, err := w.Write(children)
	return err
}

func WriteEBMLHeader(w io.Writer) error {
	var e ew
	e.uint(ebml.IDEBMLVersion, 1)
	e.uint(ebml.IDEBMLReadVersion, 1)
	e.uint(ebml.IDEBMLMaxIDLength, 4)
	e.uint(ebml.IDEBMLMaxSizeLength, 8)
	e.str(ebml.IDDocType, "matroska")
	e.uint(ebml.IDDocTypeVersion, 4)
	e.uint(ebml.IDDocTypeReadVersion, 2)
	return e.flush(w, ebml.IDEBMLHeader)
}

func WriteSegmentInfo(w io.Writer, info *mkv.SegmentInfo, durationMs int64) error {
	var e ew
	if info.TimecodeScale > 0 {
		e.uint(mkv.IDTimecodeScale, uint64(info.TimecodeScale))
	}
	if info.Duration > 0 {
		e.float64(mkv.IDDuration, info.Duration)
	} else if durationMs > 0 && info.TimecodeScale > 0 {
		e.float64(mkv.IDDuration, float64(durationMs)*1e6/float64(info.TimecodeScale))
	}
	if info.Title != "" {
		e.str(mkv.IDTitle, info.Title)
	}
	mux := info.MuxingApp
	if mux == "" {
		mux = "mkvgo"
	}
	e.str(mkv.IDMuxingApp, mux)
	wapp := info.WritingApp
	if wapp == "" {
		wapp = "mkvgo"
	}
	e.str(mkv.IDWritingApp, wapp)
	if info.DateUTC != nil {
		epoch := int64(978307200)
		nanos := (info.DateUTC.Unix() - epoch) * 1e9
		e.uint(mkv.IDDateUTC, uint64(nanos))
	}
	if len(info.SegmentUID) > 0 {
		e.raw(mkv.IDSegmentUID, info.SegmentUID)
	}
	if len(info.PrevUID) > 0 {
		e.raw(mkv.IDPrevUID, info.PrevUID)
	}
	if len(info.NextUID) > 0 {
		e.raw(mkv.IDNextUID, info.NextUID)
	}
	return e.flush(w, mkv.IDInfo)
}

func WriteTracks(w io.Writer, tracks []mkv.Track) error {
	var e ew
	for i := range tracks {
		e.master(mkv.IDTrackEntry, func(te *ew) {
			writeTrackFields(te, &tracks[i])
		})
	}
	return e.flush(w, mkv.IDTracks)
}

func writeTrackFields(e *ew, t *mkv.Track) {
	e.uint(mkv.IDTrackNumber, t.ID)
	e.uint(mkv.IDTrackUID, t.ID)

	var tt uint64
	switch t.Type {
	case mkv.VideoTrack:
		tt = mkv.TrackTypeVideo
	case mkv.AudioTrack:
		tt = mkv.TrackTypeAudio
	case mkv.SubtitleTrack:
		tt = mkv.TrackTypeSubtitle
	}
	e.uint(mkv.IDTrackType, tt)
	e.str(mkv.IDCodecID, CodecIDFromShort(t.Codec))
	if len(t.CodecPrivate) > 0 {
		e.raw(mkv.IDCodecPrivate, t.CodecPrivate)
	}
	if t.Language != "" {
		e.str(mkv.IDLanguage, t.Language)
	}
	if t.Name != "" {
		e.str(mkv.IDName, t.Name)
	}
	if !t.IsDefault {
		e.uint(mkv.IDFlagDefault, 0)
	}
	if t.IsForced {
		e.uint(mkv.IDFlagForced, 1)
	}
	if t.Type == mkv.VideoTrack && (t.Width != nil || t.Height != nil) {
		e.master(mkv.IDVideo, func(v *ew) {
			if t.Width != nil {
				v.uint(mkv.IDPixelWidth, uint64(*t.Width))
			}
			if t.Height != nil {
				v.uint(mkv.IDPixelHeight, uint64(*t.Height))
			}
		})
	}
	if t.Type == mkv.AudioTrack && (t.SampleRate != nil || t.Channels != nil || t.BitDepth != nil) {
		e.master(mkv.IDAudio, func(a *ew) {
			if t.SampleRate != nil {
				a.float64(mkv.IDSamplingFreq, *t.SampleRate)
			}
			if t.Channels != nil {
				a.uint(mkv.IDChannels, uint64(*t.Channels))
			}
			if t.BitDepth != nil {
				a.uint(mkv.IDBitDepth, uint64(*t.BitDepth))
			}
		})
	}
	if len(t.HeaderStripping) > 0 {
		e.master(mkv.IDContentEncodings, func(enc *ew) {
			enc.master(mkv.IDContentEncoding, func(ce *ew) {
				ce.master(mkv.IDContentCompression, func(cc *ew) {
					cc.uint(mkv.IDContentCompAlgo, 3)
					cc.raw(mkv.IDContentCompSettings, t.HeaderStripping)
				})
			})
		})
	}
}

func WriteChapters(w io.Writer, chapters []mkv.Chapter) error {
	var e ew
	e.master(mkv.IDEditionEntry, func(ed *ew) {
		for i := range chapters {
			ch := &chapters[i]
			ed.master(mkv.IDChapterAtom, func(a *ew) {
				a.uint(mkv.IDChapterUID, ch.ID)
				a.uint(mkv.IDChapterTimeStart, uint64(ch.StartMs)*1000000)
				if ch.EndMs > 0 {
					a.uint(mkv.IDChapterTimeEnd, uint64(ch.EndMs)*1000000)
				}
				if ch.Title != "" {
					a.master(mkv.IDChapterDisplay, func(d *ew) {
						d.str(mkv.IDChapString, ch.Title)
					})
				}
				if len(ch.SegmentUID) > 0 {
					a.raw(mkv.IDChapterSegmentUID, ch.SegmentUID)
				}
			})
		}
	})
	return e.flush(w, mkv.IDChapters)
}

func WriteTags(w io.Writer, tags []mkv.Tag) error {
	var e ew
	for i := range tags {
		tag := &tags[i]
		e.master(mkv.IDTag, func(te *ew) {
			te.master(mkv.IDTargets, func(tgt *ew) {
				if tag.TargetType != "" {
					tgt.str(mkv.IDTargetType, tag.TargetType)
				}
				if tag.TargetID > 0 {
					tgt.uint(mkv.IDTagTrackUID, tag.TargetID)
				}
			})
			for j := range tag.SimpleTags {
				writeSimpleTagElement(te, &tag.SimpleTags[j])
			}
		})
	}
	return e.flush(w, mkv.IDTags)
}

func writeSimpleTagElement(parent *ew, st *mkv.SimpleTag) {
	parent.master(mkv.IDSimpleTag, func(se *ew) {
		se.str(mkv.IDTagName, st.Name)
		if st.Value != "" {
			se.str(mkv.IDTagString, st.Value)
		}
		if len(st.Binary) > 0 {
			se.raw(mkv.IDTagBinary, st.Binary)
		}
		if st.Language != "" {
			se.str(mkv.IDTagLanguage, st.Language)
		}
		for i := range st.SubTags {
			writeSimpleTagElement(se, &st.SubTags[i])
		}
	})
}

func WriteAttachments(w io.Writer, attachments []mkv.Attachment) error {
	var e ew
	for i := range attachments {
		att := &attachments[i]
		e.master(mkv.IDAttachedFile, func(ae *ew) {
			if att.Name != "" {
				ae.str(mkv.IDFileName, att.Name)
			}
			if att.MIMEType != "" {
				ae.str(mkv.IDFileMimeType, att.MIMEType)
			}
			if att.ID > 0 {
				ae.uint(mkv.IDFileUID, att.ID)
			}
			if len(att.Data) > 0 {
				ae.raw(mkv.IDFileData, att.Data)
			}
		})
	}
	return e.flush(w, mkv.IDAttachments)
}

func WriteSimpleBlock(w io.Writer, trackNum uint64, relTC int16, keyframe bool, data []byte) error {
	trackVINT := ebml.DataSizeLen(int64(trackNum))
	bodySize := int64(trackVINT + 2 + 1 + len(data))

	if _, err := ebml.WriteElementHeader(w, mkv.IDSimpleBlock, bodySize); err != nil {
		return err
	}
	if _, err := ebml.WriteDataSize(w, int64(trackNum)); err != nil {
		return err
	}
	var tcBuf [2]byte
	tcBuf[0] = byte(uint16(relTC) >> 8)
	tcBuf[1] = byte(relTC)
	if _, err := w.Write(tcBuf[:]); err != nil {
		return err
	}
	var flags byte
	if keyframe {
		flags |= 0x80
	}
	if _, err := w.Write([]byte{flags}); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

func WriteCluster(w io.Writer, clusterTS int64, timecodeScale int64, blocks []mkv.Block) error {
	rawTS := uint64(clusterTS * 1000000 / timecodeScale)
	var e ew
	e.uint(mkv.IDTimestamp, rawTS)
	for i := range blocks {
		b := &blocks[i]
		relTC := int16(b.Timecode - clusterTS)
		if e.err != nil {
			break
		}
		e.err = WriteSimpleBlock(&e.Buffer, b.TrackNumber, relTC, b.Keyframe, b.Data)
	}
	return e.flush(w, mkv.IDCluster)
}

func WriteCues(w io.Writer, cues []mkv.CuePoint, timecodeScale int64) error {
	var e ew
	for i := range cues {
		cp := &cues[i]
		// CueTime is in timecode units, not milliseconds
		cueTime := uint64(cp.TimeMs) * 1000000 / uint64(timecodeScale)
		e.master(mkv.IDCuePoint, func(ce *ew) {
			ce.uint(mkv.IDCueTime, cueTime)
			ce.master(mkv.IDCueTrackPositions, func(tp *ew) {
				tp.uint(mkv.IDCueTrack, cp.Track)
				tp.uint(mkv.IDCueClusterPos, uint64(cp.ClusterPos))
			})
		})
	}
	return e.flush(w, mkv.IDCues)
}

type SeekEntry struct {
	ID  uint32
	Pos int64
}

func WriteSeekHead(w io.Writer, entries []SeekEntry) error {
	var e ew
	for _, se := range entries {
		e.master(mkv.IDSeek, func(s *ew) {
			s.raw(mkv.IDSeekID, EncodeElementID(se.ID))
			s.uint(mkv.IDSeekPosition, uint64(se.Pos))
		})
	}
	return e.flush(w, mkv.IDSeekHead)
}

func EncodeElementID(id uint32) []byte {
	n := ebml.ElementIDLen(id)
	buf := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		buf[i] = byte(id)
		id >>= 8
	}
	return buf
}

func WriteVoid(w io.Writer, totalSize int) error {
	if totalSize < 2 {
		return nil
	}
	headerSize := 1 + ebml.DataSizeLen(int64(totalSize-1-ebml.DataSizeLen(int64(totalSize-2))))
	padLen := totalSize - headerSize
	if padLen < 0 {
		padLen = 0
	}
	if _, err := ebml.WriteElementHeader(w, mkv.IDVoid, int64(padLen)); err != nil {
		return err
	}
	_, err := w.Write(make([]byte, padLen))
	return err
}

func CodecIDFromShort(short string) string {
	for full, s := range mkv.CodecShortName {
		if s == short {
			return full
		}
	}
	return short
}
