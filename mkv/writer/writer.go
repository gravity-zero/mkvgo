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

// WriteSegmentInfo writes the Segment Info element.
//
// durationMs is a FALLBACK, used only when info.Duration is 0: a non-zero
// info.Duration is the authoritative declared length and is written verbatim,
// so a metadata rewrite preserves the sub-millisecond precision that
// Container.DurationMs has truncated away, and so an explicit edit of
// info.Duration is honoured. An op whose output is NOT the same length as the
// source it copied info from must therefore clear info.Duration - see
// ops.metaForNewDuration and NewStreamWriter, which both do exactly that.
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
			writeChapterAtom(ed, &chapters[i], 0)
		}
	})
	return e.flush(w, mkv.IDChapters)
}

// maxChapterWriteDepth mirrors the reader's nesting limit, so a hand-built
// container cannot recurse this writer past what mkvgo will read back.
const maxChapterWriteDepth = 64

// writeChapterAtom writes one chapter and, nested inside it, its sub-chapters -
// which the reader has always parsed and every other op carries around, but
// which this used to drop on the floor at write time.
func writeChapterAtom(ed *ew, ch *mkv.Chapter, depth int) {
	if depth > maxChapterWriteDepth {
		if ed.err == nil {
			ed.err = fmt.Errorf("chapter nesting exceeds %d levels", maxChapterWriteDepth)
		}
		return
	}
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
		// Nested atoms come after the atom's own fields.
		for i := range ch.SubChapters {
			writeChapterAtom(a, &ch.SubChapters[i], depth+1)
		}
	})
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

// WriteAttachments streams the Attachments element instead of building it in
// memory. An attachment payload is unbounded user data - a subtitle font set is
// tens of megabytes - and the nested element buffers held every byte three times
// over at the peak: once in the source container, once in the per-file buffer,
// once in the buffer accumulating the set. Sizes are computed from the payload
// lengths, which are known without copying anything, so the peak is now the
// data the caller already holds.
func WriteAttachments(w io.Writer, attachments []mkv.Attachment) error {
	return WriteAttachmentsFrom(w, attachments, nil)
}

// WriteAttachmentsFrom is WriteAttachments for a caller whose payloads are still
// on disk: open is asked for a reader positioned on the payload of an attachment
// whose Data is nil (see mkv.Attachment.DataPath). It may be nil when every
// payload is in hand.
func WriteAttachmentsFrom(w io.Writer, attachments []mkv.Attachment, open func(*mkv.Attachment) (io.Reader, error)) error {
	// The small children (name, mime type, UID) are cheap to materialise; only
	// the payload is kept out of memory.
	metas := make([][]byte, len(attachments))
	bodies := make([]int64, len(attachments))
	var total int64
	for i := range attachments {
		att := &attachments[i]
		var m ew
		if att.Name != "" {
			m.str(mkv.IDFileName, att.Name)
		}
		if att.MIMEType != "" {
			m.str(mkv.IDFileMimeType, att.MIMEType)
		}
		if att.ID > 0 {
			m.uint(mkv.IDFileUID, att.ID)
		}
		if m.err != nil {
			return m.err
		}
		metas[i] = m.Bytes()

		bodies[i] = int64(len(metas[i]))
		if n := payloadLen(att); n > 0 {
			hdr, err := elementHeaderLen(mkv.IDFileData, n)
			if err != nil {
				return err
			}
			bodies[i] += hdr + n
		}
		fileHdr, err := elementHeaderLen(mkv.IDAttachedFile, bodies[i])
		if err != nil {
			return err
		}
		total += fileHdr + bodies[i]
	}

	if _, err := ebml.WriteElementHeader(w, mkv.IDAttachments, total); err != nil {
		return err
	}
	for i := range attachments {
		att := &attachments[i]
		if _, err := ebml.WriteElementHeader(w, mkv.IDAttachedFile, bodies[i]); err != nil {
			return err
		}
		if _, err := w.Write(metas[i]); err != nil {
			return err
		}
		n := payloadLen(att)
		if n == 0 {
			continue
		}
		if _, err := ebml.WriteElementHeader(w, mkv.IDFileData, n); err != nil {
			return err
		}
		if att.Data != nil {
			if _, err := w.Write(att.Data); err != nil {
				return err
			}
			continue
		}
		// The payload was left on disk (reader.WithoutAttachmentData): copy it
		// through, so a font never becomes resident just to be forwarded.
		src, err := open(att)
		if err != nil {
			return fmt.Errorf("attachment %q: %w", att.Name, err)
		}
		copied, cerr := io.CopyN(w, src, n)
		if c, ok := src.(io.Closer); ok {
			c.Close()
		}
		if cerr != nil {
			return fmt.Errorf("attachment %q: %w", att.Name, cerr)
		}
		if copied != n {
			return fmt.Errorf("attachment %q: copied %d bytes, declared %d", att.Name, copied, n)
		}
	}
	return nil
}

// payloadLen is the attachment's payload length whether the bytes are in hand or
// still on disk.
func payloadLen(att *mkv.Attachment) int64 {
	if att.Data != nil {
		return int64(len(att.Data))
	}
	if att.DataPath != "" && att.Size > 0 {
		return att.Size
	}
	return 0
}

// elementHeaderLen is the encoded length of an element header, so a size can be
// computed without writing the element.
func elementHeaderLen(id uint32, size int64) (int64, error) {
	var b bytes.Buffer
	n, err := ebml.WriteElementHeader(&b, id, size)
	return int64(n), err
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

// VoidHeader returns the header (ID + size VINT) of a Void element spanning
// EXACTLY totalSize bytes. It is the single place that decides how a Void is
// laid out, because getting it wrong is a silent corruption: callers pad a
// reserved slot with a Void, and one that declares a span a byte longer than
// its budget swallows the head of the element that follows it.
//
// The layout is 1 (the Void ID) + width + payload == totalSize. Some totals are
// out of reach for a minimal-width size VINT - 129 bytes, for one: a 1-byte
// width leaves a 127-byte payload, which no longer fits a 1-byte VINT (127 is
// the reserved unknown-size pattern), while a 2-byte width leaves 126. The
// width is therefore widened until the payload fits it, using the non-minimal
// Data Size encoding EBML allows.
func VoidHeader(totalSize int) ([]byte, error) {
	if totalSize < 2 {
		return nil, fmt.Errorf("a Void spans at least 2 bytes (1 of ID, 1 of size), got %d", totalSize)
	}
	for width := 1; width <= 8; width++ {
		payload := int64(totalSize - 1 - width)
		if payload < 0 {
			break
		}
		if ebml.DataSizeLen(payload) > width {
			continue // payload does not fit this width: widen the size VINT
		}
		var buf bytes.Buffer
		if _, err := ebml.WriteElementID(&buf, mkv.IDVoid); err != nil {
			return nil, err
		}
		if _, err := ebml.WriteDataSizeWidth(&buf, payload, width); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}
	return nil, fmt.Errorf("cannot encode a Void of exactly %d bytes", totalSize)
}

// WriteVoid writes a Void element of EXACTLY totalSize bytes, payload included
// (nothing at all below 2, the smallest an element can be).
func WriteVoid(w io.Writer, totalSize int) error {
	if totalSize < 2 {
		return nil
	}
	hdr, err := VoidHeader(totalSize)
	if err != nil {
		return err
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err = w.Write(make([]byte, totalSize-len(hdr)))
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
