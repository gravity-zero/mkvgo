package writer

import (
	"bytes"
	"fmt"
	"io"
	"math"

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

// Write serialises c's METADATA ONLY - EBML header, Info, Tracks, Chapters,
// Attachments, Tags. It writes NO Clusters (a Container holds no block data)
// and no Cues/SeekHead, so the result is not a playable media file. For a
// complete file, write blocks through MKVWriter or NewStreamWriter, or remux
// from a source (ops.RemuxToWebM, mp4.RemuxToMP4, ops.Mux).
func Write(w io.Writer, c *mkv.Container) error {
	if err := WriteEBMLHeader(w); err != nil {
		return err
	}
	return writeSegment(w, c)
}

// WriteWebM writes c as a WebM file: it first verifies that every track uses a
// WebM-compatible codec (mkv.ValidateWebM), then writes the container with the
// "webm" DocType. If any track is incompatible it returns an error without
// writing anything - mkvgo cannot transcode, so this is a hard failure.
func WriteWebM(w io.Writer, c *mkv.Container) error {
	if err := mkv.ValidateWebM(c); err != nil {
		return err
	}
	if err := writeEBMLHeaderDocType(w, "webm", mkv.WebMDocTypeVersion(c), 2); err != nil {
		return err
	}
	// WebM permits only a restricted element set, so write just Info + Tracks
	// (no chapters/attachments/tags). NOTE: like writer.Write, this writes
	// metadata only - NO clusters. For a complete, playable .webm use
	// writer.NewWebMStreamWriter or ops.RemuxToWebM.
	var seg bytes.Buffer
	if err := WriteSegmentInfo(&seg, &c.Info, c.DurationMs); err != nil {
		return err
	}
	if len(c.Tracks) > 0 {
		if err := WriteTracks(&seg, c.Tracks); err != nil {
			return err
		}
	}
	return WriteMasterElement(w, mkv.IDSegment, seg.Bytes())
}

func writeSegment(w io.Writer, c *mkv.Container) error {
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
	return writeEBMLHeaderDocType(w, "matroska", 4, 2)
}

// WriteEBMLHeaderWebM writes an EBML header declaring the "webm" DocType.
func WriteEBMLHeaderWebM(w io.Writer) error {
	return writeEBMLHeaderDocType(w, "webm", 2, 2)
}

func writeEBMLHeaderDocType(w io.Writer, docType string, version, readVersion uint64) error {
	var e ew
	e.uint(ebml.IDEBMLVersion, 1)
	e.uint(ebml.IDEBMLReadVersion, 1)
	e.uint(ebml.IDEBMLMaxIDLength, 4)
	e.uint(ebml.IDEBMLMaxSizeLength, 8)
	e.str(ebml.IDDocType, docType)
	e.uint(ebml.IDDocTypeVersion, version)
	e.uint(ebml.IDDocTypeReadVersion, readVersion)
	return e.flush(w, ebml.IDEBMLHeader)
}

func WriteSegmentInfo(w io.Writer, info *mkv.SegmentInfo, durationMs int64) error {
	var e ew
	// A zero TimecodeScale means "unset": fall back to the Matroska default
	// (1_000_000 = 1 ms) so a Duration can still be derived from durationMs.
	scale := info.TimecodeScale
	if scale <= 0 {
		scale = 1_000_000
	}
	e.uint(mkv.IDTimecodeScale, uint64(scale))
	if info.Duration > 0 {
		e.float64(mkv.IDDuration, info.Duration)
	} else if durationMs > 0 {
		e.float64(mkv.IDDuration, float64(durationMs)*1e6/float64(scale))
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
	uid := t.UID
	if uid == 0 {
		uid = t.ID
	}
	e.uint(mkv.IDTrackUID, uid)

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
	// DefaultDuration round-trips the source's nominal frame duration: the raw
	// nanosecond value when the reader kept it (audio needs it to time laced
	// frames), else re-derived from the declared frame rate. Without it every
	// rewrite silently dropped the value.
	switch {
	case t.DefaultDurationNs > 0:
		e.uint(mkv.IDDefaultDuration, uint64(t.DefaultDurationNs))
	case t.FrameRate != nil && *t.FrameRate > 0:
		e.uint(mkv.IDDefaultDuration, uint64(1e9 / *t.FrameRate + 0.5))
	}
	if t.Language != "" {
		e.str(mkv.IDLanguage, t.Language)
	}
	if t.LanguageBCP47 != "" {
		e.str(mkv.IDLanguageBCP47, t.LanguageBCP47)
	}
	if t.Name != "" {
		e.str(mkv.IDName, t.Name)
	}
	if t.CodecDelay > 0 {
		e.uint(mkv.IDCodecDelay, uint64(t.CodecDelay))
	}
	if t.SeekPreRoll > 0 {
		e.uint(mkv.IDSeekPreRoll, uint64(t.SeekPreRoll))
	}
	if !t.IsDefault {
		e.uint(mkv.IDFlagDefault, 0)
	}
	if t.IsForced {
		e.uint(mkv.IDFlagForced, 1)
	}
	if t.Type == mkv.VideoTrack && (t.Width != nil || t.Height != nil || t.DisplayWidth != nil || t.DisplayHeight != nil || hasColour(t)) {
		e.master(mkv.IDVideo, func(v *ew) {
			if t.Width != nil {
				v.uint(mkv.IDPixelWidth, uint64(*t.Width))
			}
			if t.Height != nil {
				v.uint(mkv.IDPixelHeight, uint64(*t.Height))
			}
			if t.DisplayWidth != nil {
				v.uint(mkv.IDDisplayWidth, uint64(*t.DisplayWidth))
			}
			if t.DisplayHeight != nil {
				v.uint(mkv.IDDisplayHeight, uint64(*t.DisplayHeight))
			}
			if hasColour(t) {
				v.master(mkv.IDColour, func(c *ew) {
					if t.ColorRange != nil {
						c.uint(mkv.IDColourRange, uint64(*t.ColorRange))
					}
					if t.ColorSpace != nil {
						c.uint(mkv.IDColourMatrix, uint64(*t.ColorSpace))
					}
					if t.ColorTransfer != nil {
						c.uint(mkv.IDColourTransfer, uint64(*t.ColorTransfer))
					}
					if t.ColorPrimaries != nil {
						c.uint(mkv.IDColourPrimaries, uint64(*t.ColorPrimaries))
					}
				})
			}
		})
	}
	if t.DolbyVision != nil {
		_, addType := t.DolbyVision.BoxType()
		rec := mkv.EncodeDolbyVisionConfig(t.DolbyVision)
		e.master(mkv.IDBlockAdditionMapping, func(m *ew) {
			m.uint(mkv.IDBlockAddIDValue, 1)
			m.uint(mkv.IDBlockAddIDType, uint64(addType))
			m.raw(mkv.IDBlockAddIDExtraData, rec)
		})
	}
	if t.Type == mkv.AudioTrack && (t.SampleRate != nil || t.OutputSampleRate != nil || t.Channels != nil || t.BitDepth != nil) {
		e.master(mkv.IDAudio, func(a *ew) {
			if t.SampleRate != nil {
				a.float64(mkv.IDSamplingFreq, *t.SampleRate)
			}
			if t.OutputSampleRate != nil {
				a.float64(mkv.IDOutputSamplingFreq, *t.OutputSampleRate)
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
	if timecodeScale <= 0 { // guard against divide-by-zero from a malformed source
		timecodeScale = 1000000
	}
	rawTS := uint64(clusterTS * 1000000 / timecodeScale)
	var e ew
	e.uint(mkv.IDTimestamp, rawTS)
	for i := range blocks {
		b := &blocks[i]
		// Block timecodes are milliseconds internally; the SimpleBlock offset is
		// stored in raw timecode-scale units, like the cluster Timestamp above.
		delta := (b.Timecode - clusterTS) * 1000000 / timecodeScale
		if delta < math.MinInt16 || delta > math.MaxInt16 {
			return fmt.Errorf("block timecode %dms is %+d timecode units from cluster start %dms, outside SimpleBlock's int16 range", b.Timecode, delta, clusterTS)
		}
		relTC := int16(delta)
		if e.err != nil {
			break
		}
		if b.Duration > 0 {
			// A block with an explicit duration (e.g. a subtitle cue) is written
			// as a BlockGroup so the duration can be carried.
			rawDur := uint64(b.Duration * 1000000 / timecodeScale)
			e.err = WriteBlockGroup(&e.Buffer, b.TrackNumber, relTC, b.Data, rawDur)
			continue
		}
		e.err = WriteSimpleBlock(&e.Buffer, b.TrackNumber, relTC, b.Keyframe, b.Data)
	}
	return e.flush(w, mkv.IDCluster)
}

// WriteBlockGroup writes a BlockGroup containing a Block and a BlockDuration
// (in raw timecode-scale units). Used for subtitle cues, which need a duration.
func WriteBlockGroup(w io.Writer, trackNum uint64, relTC int16, data []byte, rawDuration uint64) error {
	var inner bytes.Buffer
	trackVINT := ebml.DataSizeLen(int64(trackNum))
	bodySize := int64(trackVINT + 2 + 1 + len(data))
	if _, err := ebml.WriteElementHeader(&inner, mkv.IDBlock, bodySize); err != nil {
		return err
	}
	if _, err := ebml.WriteDataSize(&inner, int64(trackNum)); err != nil {
		return err
	}
	inner.Write([]byte{byte(uint16(relTC) >> 8), byte(relTC), 0x00}) // timecode + flags
	inner.Write(data)
	if err := WriteUintElement(&inner, mkv.IDBlockDuration, rawDuration); err != nil {
		return err
	}
	return WriteMasterElement(w, mkv.IDBlockGroup, inner.Bytes())
}

// hasColour reports whether a track carries any colour code points.
func hasColour(t *mkv.Track) bool {
	return t.ColorPrimaries != nil || t.ColorTransfer != nil || t.ColorSpace != nil || t.ColorRange != nil
}

func WriteCues(w io.Writer, cues []mkv.CuePoint, timecodeScale int64) error {
	if timecodeScale <= 0 { // guard against divide-by-zero from a malformed source
		timecodeScale = 1000000
	}
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
