package reader

// stream.go — seekless streaming read from a plain io.Reader.
//
// Design:
//   - NewStreamBlockReader wraps an io.Reader in a thin readOnlySeeker shim so
//     the existing countingReader machinery works without modification. The shim
//     handles Seek(0, SeekCurrent) (position query) but returns an error for
//     any real movement — which never occurs after BlockReader.init().
//   - ReadStream does a single forward pass: EBML header → Segment →
//     front-loaded metadata (Info, Tracks, Tags, Chapters, Attachments). Cues
//     and SeekHead are silently skipped because they are unusable on a
//     forward-only stream. The first Cluster element header is consumed; its
//     ID+size are handed to the resulting BlockReader so no byte is re-read.
//   - Unknown-size Clusters are supported by BlockReader.Next() via the peeked
//     field set from ReadStream, and by the unknown-size cluster logic already
//     handled in Next() (clusterEnd == -1 → read until next top-level element).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// readOnlySeeker wraps an io.Reader to satisfy io.ReadSeeker.
// The only Seek call the BlockReader makes after construction is
// Seek(0, SeekCurrent) inside readOnlySeeker.Seek for position queries;
// all skips go through countingReader.discard (io.CopyN).
type readOnlySeeker struct {
	r   io.Reader
	pos int64
}

func (s *readOnlySeeker) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.pos += int64(n)
	return n, err
}

func (s *readOnlySeeker) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekCurrent {
		return s.pos, nil
	}
	return 0, fmt.Errorf("mkvgo/reader: stream is not seekable (offset=%d whence=%d)", offset, whence)
}

// NewStreamBlockReader creates a BlockReader from a plain io.Reader.
// The reader must be positioned at the very start of a well-formed MKV/WebM
// stream (EBML header first). No Seek is ever issued.
func NewStreamBlockReader(r io.Reader, timecodeScale int64) (*BlockReader, error) {
	return NewBlockReader(&readOnlySeeker{r: r}, timecodeScale)
}

// streamParser is a forward-only parser: skips via io.CopyN, no Seek.
type streamParser struct {
	r   io.Reader
	pos int64 // bytes consumed from r (tracks sub-element boundaries)
}

func (p *streamParser) readHeader() (ebml.ElementHeader, int, error) {
	h, n, err := ebml.ReadElementHeader(p.r)
	p.pos += int64(n)
	return h, n, err
}

func (p *streamParser) skip(size int64) error {
	if size <= 0 {
		return nil
	}
	n, err := io.CopyN(io.Discard, p.r, size)
	p.pos += n
	return err
}

func (p *streamParser) readUint(size int64) (uint64, error) {
	v, err := ebml.ReadUint(p.r, size)
	if err == nil {
		p.pos += size
	}
	return v, err
}

func (p *streamParser) readFloat(size int64) (float64, error) {
	v, err := ebml.ReadFloat(p.r, size)
	if err == nil {
		p.pos += size
	}
	return v, err
}

func (p *streamParser) readString(size int64) (string, error) {
	v, err := ebml.ReadString(p.r, size)
	if err == nil {
		p.pos += size
	}
	return v, err
}

func (p *streamParser) readBytes(size int64) ([]byte, error) {
	v, err := ebml.ReadBytes(p.r, size)
	if err == nil {
		p.pos += size
	}
	return v, err
}

// boundedLoop iterates over child elements within a known-size master element.
func (p *streamParser) boundedLoop(size int64, fn func(ebml.ElementHeader) error) error {
	if size < 0 {
		return fmt.Errorf("mkvgo/reader/stream: unknown-size master element not supported in metadata pass")
	}
	end := p.pos + size
	for p.pos < end {
		h, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// ReadStream reads the EBML header and front-loaded segment metadata from a
// plain io.Reader (no Seek required), then returns a *BlockReader ready to
// yield blocks. In a well-formed streamable MKV (e.g. produced by StreamWriter
// or ffmpeg piped output), Info and Tracks precede clusters.
//
// SeekHead and Cues are silently skipped (they cannot be acted on in a
// forward-only stream). The returned Container has no Cues field populated.
func ReadStream(ctx context.Context, r io.Reader) (*mkv.Container, *BlockReader, error) {
	p := &streamParser{r: r}

	// EBML header.
	h, _, err := p.readHeader()
	if err != nil {
		return nil, nil, fmt.Errorf("ebml header: %w", err)
	}
	if h.ID != ebml.IDEBMLHeader {
		return nil, nil, fmt.Errorf("ebml header: expected 0x%X got 0x%X", ebml.IDEBMLHeader, h.ID)
	}
	if err := p.skip(h.Size); err != nil {
		return nil, nil, fmt.Errorf("ebml header body: %w", err)
	}

	// Segment.
	h, _, err = p.readHeader()
	if err != nil {
		return nil, nil, fmt.Errorf("segment: %w", err)
	}
	if h.ID != mkv.IDSegment {
		return nil, nil, fmt.Errorf("segment: expected 0x%X got 0x%X", mkv.IDSegment, h.ID)
	}
	// h.Size may be -1 (unknown-size segment) — fine for streaming.

	c := &mkv.Container{}
	c.Info.TimecodeScale = 1_000_000 // EBML default

	// Scan segment-level elements until we hit the first Cluster.
	var peekedCluster *ebml.ElementHeader
metaLoop:
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		eh, _, err := p.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break metaLoop
			}
			return nil, nil, err
		}
		switch eh.ID {
		case mkv.IDInfo:
			if err := p.parseStreamInfo(eh.Size, c); err != nil {
				return nil, nil, err
			}
		case mkv.IDTracks:
			if err := p.parseStreamTracks(eh.Size, c); err != nil {
				return nil, nil, err
			}
		case mkv.IDChapters:
			if err := p.parseStreamChapters(eh.Size, c); err != nil {
				return nil, nil, err
			}
		case mkv.IDAttachments:
			if err := p.parseStreamAttachments(eh.Size, c); err != nil {
				return nil, nil, err
			}
		case mkv.IDTags:
			if err := p.parseStreamTags(eh.Size, c); err != nil {
				return nil, nil, err
			}
		case mkv.IDCluster:
			peekedCluster = &eh
			break metaLoop
		case mkv.IDCues, mkv.IDSeekHead:
			if eh.Size >= 0 {
				if err := p.skip(eh.Size); err != nil {
					return nil, nil, err
				}
			}
			// unknown-size SeekHead/Cues: can't skip → stop metadata scan.
		default:
			if eh.Size >= 0 {
				if err := p.skip(eh.Size); err != nil {
					return nil, nil, err
				}
			}
			// unknown-size unknown element: stop metadata scan.
		}
	}

	if c.Info.Duration > 0 && c.Info.TimecodeScale > 0 {
		d := c.Info.Duration * float64(c.Info.TimecodeScale) / 1e6
		if d <= float64(math.MaxInt64) && d >= float64(math.MinInt64) {
			c.DurationMs = int64(d)
		}
	}

	// Build BlockReader directly (bypass init which would re-read the EBML
	// header). The reader is already positioned right after the metadata.
	br := &BlockReader{
		timecodeScale: c.Info.TimecodeScale,
		segEnd:        -1, // unknown-size segment terminates on EOF
		clusterEnd:    -1,
	}
	// Seed countingReader at the current position. The underlying reader is
	// wrapped in a readOnlySeeker so countingReader.src.Seek(0,Current) works.
	br.r = newCountingReader(&readOnlySeeker{r: r}, p.pos)

	if peekedCluster != nil {
		// We consumed the Cluster element header; enter cluster state.
		br.inCluster = true
		if peekedCluster.Size >= 0 {
			br.clusterEnd = br.r.tell() + peekedCluster.Size
		}
		// else: unknown-size cluster → clusterEnd stays -1, handled by Next().
	}

	return c, br, nil
}

// --- metadata sub-parsers (all forward-only) ---

func (p *streamParser) parseStreamInfo(size int64, c *mkv.Container) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDTimecodeScale:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			if v > 0 { // keep default; 0 scale would divide-by-zero downstream
				c.Info.TimecodeScale = int64(v)
			}
		case mkv.IDDuration:
			v, err := p.readFloat(h.Size)
			if err != nil {
				return err
			}
			c.Info.Duration = v
		case mkv.IDMuxingApp:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			c.Info.MuxingApp = v
		case mkv.IDWritingApp:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			c.Info.WritingApp = v
		case mkv.IDTitle:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			c.Info.Title = v
		default:
			return p.skip(h.Size)
		}
		return nil
	})
}

func (p *streamParser) parseStreamTracks(size int64, c *mkv.Container) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDTrackEntry {
			track, err := p.parseStreamTrackEntry(h.Size)
			if err != nil {
				return err
			}
			c.Tracks = append(c.Tracks, track)
			return nil
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamTrackEntry(size int64) (mkv.Track, error) {
	t := mkv.Track{}
	err := p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDTrackNumber:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			t.ID = v
		case mkv.IDTrackUID:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			t.UID = v
		case mkv.IDTrackType:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			switch v {
			case mkv.TrackTypeVideo:
				t.Type = mkv.VideoTrack
			case mkv.TrackTypeAudio:
				t.Type = mkv.AudioTrack
			case mkv.TrackTypeSubtitle:
				t.Type = mkv.SubtitleTrack
			}
		case mkv.IDCodecID:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			if short, ok := mkv.CodecShortName[v]; ok {
				t.Codec = short
			} else {
				t.Codec = v
			}
		case mkv.IDCodecPrivate:
			v, err := p.readBytes(h.Size)
			if err != nil {
				return err
			}
			t.CodecPrivate = v
		case mkv.IDLanguage:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			t.Language = v
			t.LanguagePresent = true
		case mkv.IDLanguageBCP47:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			t.LanguageBCP47 = v
			t.LanguagePresent = true
		case mkv.IDName:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			t.Name = v
		case mkv.IDFlagDefault:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			t.IsDefault = v == 1
			t.DefaultPresent = true
		case mkv.IDFlagForced:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			t.IsForced = v == 1
			t.ForcedPresent = true
		case mkv.IDDefaultDuration:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			if v > 0 {
				fps := 1e9 / float64(v)
				t.FrameRate = &fps
			}
		case mkv.IDVideo:
			return p.parseStreamVideo(h.Size, &t)
		case mkv.IDAudio:
			return p.parseStreamAudio(h.Size, &t)
		case mkv.IDContentEncodings:
			return p.parseStreamContentEncodings(h.Size, &t)
		default:
			return p.skip(h.Size)
		}
		return nil
	})
	// Mirror the seekable parser: apply the Matroska FlagDefault spec default (1)
	// when the element is absent, while leaving DefaultPresent false. Language is
	// not defaulted to "eng" (v0.4.0 behaviour change). FrameRate stays video-only.
	if !t.DefaultPresent {
		t.IsDefault = true
	}
	if t.Type != mkv.VideoTrack {
		t.FrameRate = nil
	}
	return t, err
}

// parseStreamContentEncodings mirrors the seekable parser: it extracts the
// header-stripping bytes (ContentCompSettings) so the streaming reader restores
// blocks identically to the seekable one.
func (p *streamParser) parseStreamContentEncodings(size int64, t *mkv.Track) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDContentEncoding {
			return p.parseStreamContentEncoding(h.Size, t)
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamContentEncoding(size int64, t *mkv.Track) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDContentCompression {
			return p.parseStreamContentCompression(h.Size, t)
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamContentCompression(size int64, t *mkv.Track) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDContentCompSettings {
			v, err := p.readBytes(h.Size)
			if err != nil {
				return err
			}
			t.HeaderStripping = v
			return nil
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamVideo(size int64, t *mkv.Track) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDPixelWidth:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			w := uint32(v)
			t.Width = &w
		case mkv.IDPixelHeight:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			hv := uint32(v)
			t.Height = &hv
		case mkv.IDColour:
			return p.parseStreamColour(h.Size, t)
		default:
			return p.skip(h.Size)
		}
		return nil
	})
}

// parseStreamColour mirrors the seekable parser's parseColour: it reads the
// Video>Colour CICP code points and video bit depth into the track.
func (p *streamParser) parseStreamColour(size int64, t *mkv.Track) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDColourMatrix:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			cs := uint16(v)
			t.ColorSpace = &cs
		case mkv.IDColourTransfer:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			tr := uint16(v)
			t.ColorTransfer = &tr
		case mkv.IDColourPrimaries:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			pr := uint16(v)
			t.ColorPrimaries = &pr
		case mkv.IDColourRange:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			rg := uint16(v)
			t.ColorRange = &rg
		case mkv.IDColourBitsPerChannel:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			bd := uint16(v)
			t.VideoBitDepth = &bd
		default:
			return p.skip(h.Size)
		}
		return nil
	})
}

func (p *streamParser) parseStreamAudio(size int64, t *mkv.Track) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDSamplingFreq:
			v, err := p.readFloat(h.Size)
			if err != nil {
				return err
			}
			t.SampleRate = &v
		case mkv.IDChannels:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			ch := uint8(v)
			t.Channels = &ch
		case mkv.IDBitDepth:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			bd := uint8(v)
			t.BitDepth = &bd
		default:
			return p.skip(h.Size)
		}
		return nil
	})
}

func (p *streamParser) parseStreamChapters(size int64, c *mkv.Container) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDEditionEntry {
			return p.parseStreamEdition(h.Size, c)
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamEdition(size int64, c *mkv.Container) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDChapterAtom {
			ch, err := p.parseStreamChapterAtom(h.Size, 0)
			if err != nil {
				return err
			}
			c.Chapters = append(c.Chapters, ch)
			return nil
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamChapterAtom(size int64, depth int) (mkv.Chapter, error) {
	if depth > maxChapterDepth {
		return mkv.Chapter{}, fmt.Errorf("chapter nesting exceeds %d levels", maxChapterDepth)
	}
	ch := mkv.Chapter{}
	err := p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDChapterUID:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			ch.ID = v
		case mkv.IDChapterTimeStart:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			ch.StartMs = int64(v / 1_000_000)
		case mkv.IDChapterTimeEnd:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			ch.EndMs = int64(v / 1_000_000)
		case mkv.IDChapterDisplay:
			return p.parseStreamChapterDisplay(h.Size, &ch)
		case mkv.IDChapterAtom:
			sub, err := p.parseStreamChapterAtom(h.Size, depth+1)
			if err != nil {
				return err
			}
			ch.SubChapters = append(ch.SubChapters, sub)
		default:
			return p.skip(h.Size)
		}
		return nil
	})
	return ch, err
}

func (p *streamParser) parseStreamChapterDisplay(size int64, ch *mkv.Chapter) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDChapString {
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			ch.Title = v
			return nil
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamAttachments(size int64, c *mkv.Container) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDAttachedFile {
			att, err := p.parseStreamAttachedFile(h.Size)
			if err != nil {
				return err
			}
			c.Attachments = append(c.Attachments, att)
			return nil
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamAttachedFile(size int64) (mkv.Attachment, error) {
	att := mkv.Attachment{}
	err := p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDFileUID:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			att.ID = v
		case mkv.IDFileName:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			att.Name = v
		case mkv.IDFileMimeType:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			att.MIMEType = v
		case mkv.IDFileData:
			v, err := p.readBytes(h.Size)
			if err != nil {
				return err
			}
			att.Data = v
			att.Size = h.Size
		default:
			return p.skip(h.Size)
		}
		return nil
	})
	return att, err
}

func (p *streamParser) parseStreamTags(size int64, c *mkv.Container) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		if h.ID == mkv.IDTag {
			tag, err := p.parseStreamTag(h.Size)
			if err != nil {
				return err
			}
			c.Tags = append(c.Tags, tag)
			return nil
		}
		return p.skip(h.Size)
	})
}

func (p *streamParser) parseStreamTag(size int64) (mkv.Tag, error) {
	tag := mkv.Tag{}
	err := p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDTargets:
			return p.parseStreamTargets(h.Size, &tag)
		case mkv.IDSimpleTag:
			st, err := p.parseStreamSimpleTag(h.Size, 0)
			if err != nil {
				return err
			}
			tag.SimpleTags = append(tag.SimpleTags, st)
		default:
			return p.skip(h.Size)
		}
		return nil
	})
	return tag, err
}

func (p *streamParser) parseStreamTargets(size int64, tag *mkv.Tag) error {
	return p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDTargetType:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			tag.TargetType = v
		case mkv.IDTagTrackUID:
			v, err := p.readUint(h.Size)
			if err != nil {
				return err
			}
			tag.TargetID = v
		default:
			return p.skip(h.Size)
		}
		return nil
	})
}

func (p *streamParser) parseStreamSimpleTag(size int64, depth int) (mkv.SimpleTag, error) {
	if depth > maxTagDepth {
		return mkv.SimpleTag{}, fmt.Errorf("SimpleTag nesting exceeds %d levels", maxTagDepth)
	}
	st := mkv.SimpleTag{}
	err := p.boundedLoop(size, func(h ebml.ElementHeader) error {
		switch h.ID {
		case mkv.IDTagName:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			st.Name = v
		case mkv.IDTagString:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			st.Value = v
		case mkv.IDTagLanguage:
			v, err := p.readString(h.Size)
			if err != nil {
				return err
			}
			st.Language = v
		case mkv.IDTagBinary:
			v, err := p.readBytes(h.Size)
			if err != nil {
				return err
			}
			st.Binary = v
		case mkv.IDSimpleTag:
			sub, err := p.parseStreamSimpleTag(h.Size, depth+1)
			if err != nil {
				return err
			}
			st.SubTags = append(st.SubTags, sub)
		default:
			return p.skip(h.Size)
		}
		return nil
	})
	return st, err
}
