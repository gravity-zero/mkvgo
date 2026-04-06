package reader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

func Open(ctx context.Context, path string) (*mkv.Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return Read(ctx, f, path)
}

func OpenWithFS(ctx context.Context, path string, fs *mkv.FS) (*mkv.Container, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return Read(ctx, f, path)
}

func Read(ctx context.Context, r io.ReadSeeker, path string) (*mkv.Container, error) {
	p := &parser{r: r}
	c := &mkv.Container{Path: path}

	if err := p.parseEBMLHeader(); err != nil {
		return nil, fmt.Errorf("ebml header: %w", err)
	}
	if err := p.parseSegment(ctx, c); err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}
	if c.Info.Duration > 0 && c.Info.TimecodeScale > 0 {
		d := c.Info.Duration * float64(c.Info.TimecodeScale) / 1e6
		if d > float64(math.MaxInt64) || d < float64(math.MinInt64) {
			return nil, fmt.Errorf("duration overflow: %g * %d", c.Info.Duration, c.Info.TimecodeScale)
		}
		c.DurationMs = int64(d)
	}
	return c, nil
}

type parser struct {
	r io.ReadSeeker
}

func (p *parser) readHeader() (ebml.ElementHeader, int, error) {
	return ebml.ReadElementHeader(p.r)
}

func (p *parser) skip(size int64) error {
	_, err := p.r.Seek(size, io.SeekCurrent)
	return err
}

func (p *parser) parseEBMLHeader() error {
	h, _, err := p.readHeader()
	if err != nil {
		return err
	}
	if h.ID != ebml.IDEBMLHeader {
		return fmt.Errorf("expected EBML header (0x%X), got 0x%X", ebml.IDEBMLHeader, h.ID)
	}
	return p.skip(h.Size)
}

func (p *parser) parseSegment(ctx context.Context, c *mkv.Container) error {
	h, _, err := p.readHeader()
	if err != nil {
		return err
	}
	if h.ID != mkv.IDSegment {
		return fmt.Errorf("expected Segment (0x%X), got 0x%X", mkv.IDSegment, h.ID)
	}

	var endPos int64 = -1
	if h.Size >= 0 {
		cur, _ := p.r.Seek(0, io.SeekCurrent)
		endPos = cur + h.Size
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if endPos >= 0 {
			cur, _ := p.r.Seek(0, io.SeekCurrent)
			if cur >= endPos {
				break
			}
		}
		eh, _, err := p.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		switch eh.ID {
		case mkv.IDInfo:
			if err := p.parseInfo(eh.Size, c); err != nil {
				return err
			}
		case mkv.IDTracks:
			if err := p.parseTracks(eh.Size, c); err != nil {
				return err
			}
		case mkv.IDChapters:
			if err := p.parseChapters(eh.Size, c); err != nil {
				return err
			}
		case mkv.IDAttachments:
			if err := p.parseAttachments(eh.Size, c); err != nil {
				return err
			}
		case mkv.IDTags:
			if err := p.parseTags(eh.Size, c); err != nil {
				return err
			}
		case mkv.IDCues:
			if err := p.parseCues(eh.Size, c); err != nil {
				return err
			}
		default:
			if eh.Size < 0 {
				return fmt.Errorf("unknown-size element 0x%X cannot be skipped", eh.ID)
			}
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseInfo(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	c.Info.TimecodeScale = 1000000

	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDTimecodeScale:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.TimecodeScale = int64(v)
		case mkv.IDDuration:
			v, err := ebml.ReadFloat(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.Duration = v
		case mkv.IDMuxingApp:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.MuxingApp = v
		case mkv.IDWritingApp:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.WritingApp = v
		case mkv.IDTitle:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.Title = v
		case mkv.IDDateUTC:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			epoch := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
			t := epoch.Add(time.Duration(int64(v)))
			c.Info.DateUTC = &t
		case mkv.IDSegmentUID:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.SegmentUID = v
		case mkv.IDPrevUID:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.PrevUID = v
		case mkv.IDNextUID:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.NextUID = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseTracks(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDTrackEntry {
			track, err := p.parseTrackEntry(eh.Size)
			if err != nil {
				return err
			}
			c.Tracks = append(c.Tracks, track)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseTrackEntry(size int64) (mkv.Track, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	t := mkv.Track{Language: "eng", IsDefault: true}

	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return t, err
		}
		switch eh.ID {
		case mkv.IDTrackNumber:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.ID = v
		case mkv.IDTrackType:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
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
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			if short, ok := mkv.CodecShortName[v]; ok {
				t.Codec = short
			} else {
				t.Codec = v
			}
		case mkv.IDCodecPrivate:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.CodecPrivate = v
		case mkv.IDLanguage:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.Language = v
		case mkv.IDName:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.Name = v
		case mkv.IDFlagDefault:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.IsDefault = v == 1
		case mkv.IDFlagForced:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.IsForced = v == 1
		case mkv.IDVideo:
			if err := p.parseVideoSettings(eh.Size, &t); err != nil {
				return t, err
			}
		case mkv.IDAudio:
			if err := p.parseAudioSettings(eh.Size, &t); err != nil {
				return t, err
			}
		case mkv.IDContentEncodings:
			if err := p.parseContentEncodings(eh.Size, &t); err != nil {
				return t, err
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return t, err
			}
		}
	}
	return t, nil
}

func (p *parser) parseVideoSettings(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDPixelWidth:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			w := uint32(v)
			t.Width = &w
		case mkv.IDPixelHeight:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			h := uint32(v)
			t.Height = &h
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseAudioSettings(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDSamplingFreq:
			v, err := ebml.ReadFloat(p.r, eh.Size)
			if err != nil {
				return err
			}
			t.SampleRate = &v
		case mkv.IDChannels:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			ch := uint8(v)
			t.Channels = &ch
		case mkv.IDBitDepth:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			bd := uint8(v)
			t.BitDepth = &bd
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseChapters(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDEditionEntry {
			if err := p.parseEditionEntry(eh.Size, c); err != nil {
				return err
			}
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseEditionEntry(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var ordered bool
	var chapters []mkv.Chapter
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDEditionFlagOrdered:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			ordered = v == 1
		case mkv.IDChapterAtom:
			ch, err := p.parseChapterAtom(eh.Size)
			if err != nil {
				return err
			}
			chapters = append(chapters, ch)
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	_ = ordered // parsed but not used yet
	c.Chapters = append(c.Chapters, chapters...)
	return nil
}

func (p *parser) parseChapterAtom(size int64) (mkv.Chapter, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	ch := mkv.Chapter{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return ch, err
		}
		switch eh.ID {
		case mkv.IDChapterUID:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return ch, err
			}
			ch.ID = v
		case mkv.IDChapterTimeStart:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return ch, err
			}
			ch.StartMs = int64(v / 1000000)
		case mkv.IDChapterTimeEnd:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return ch, err
			}
			ch.EndMs = int64(v / 1000000)
		case mkv.IDChapterDisplay:
			if err := p.parseChapterDisplay(eh.Size, &ch); err != nil {
				return ch, err
			}
		case mkv.IDChapterAtom:
			sub, err := p.parseChapterAtom(eh.Size)
			if err != nil {
				return ch, err
			}
			ch.SubChapters = append(ch.SubChapters, sub)
		case mkv.IDChapterSegmentUID:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return ch, err
			}
			ch.SegmentUID = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return ch, err
			}
		}
	}
	return ch, nil
}

func (p *parser) parseChapterDisplay(size int64, ch *mkv.Chapter) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDChapString {
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			ch.Title = v
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseAttachments(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDAttachedFile {
			att, err := p.parseAttachedFile(eh.Size)
			if err != nil {
				return err
			}
			c.Attachments = append(c.Attachments, att)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseAttachedFile(size int64) (mkv.Attachment, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	att := mkv.Attachment{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return att, err
		}
		switch eh.ID {
		case mkv.IDFileUID:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.ID = v
		case mkv.IDFileName:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.Name = v
		case mkv.IDFileMimeType:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.MIMEType = v
		case mkv.IDFileData:
			data, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.Data = data
			att.Size = eh.Size
		default:
			if err := p.skip(eh.Size); err != nil {
				return att, err
			}
		}
	}
	return att, nil
}

func (p *parser) parseTags(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDTag {
			tag, err := p.parseTag(eh.Size)
			if err != nil {
				return err
			}
			c.Tags = append(c.Tags, tag)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseTag(size int64) (mkv.Tag, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	tag := mkv.Tag{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return tag, err
		}
		switch eh.ID {
		case mkv.IDTargets:
			if err := p.parseTargets(eh.Size, &tag); err != nil {
				return tag, err
			}
		case mkv.IDSimpleTag:
			st, err := p.parseSimpleTagDepth(eh.Size, 0)
			if err != nil {
				return tag, err
			}
			tag.SimpleTags = append(tag.SimpleTags, st)
		default:
			if err := p.skip(eh.Size); err != nil {
				return tag, err
			}
		}
	}
	return tag, nil
}

func (p *parser) parseTargets(size int64, tag *mkv.Tag) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDTargetType:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			tag.TargetType = v
		case mkv.IDTagTrackUID:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			tag.TargetID = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

const maxTagDepth = 64

func (p *parser) parseSimpleTagDepth(size int64, depth int) (mkv.SimpleTag, error) {
	if depth > maxTagDepth {
		return mkv.SimpleTag{}, fmt.Errorf("SimpleTag nesting exceeds %d levels", maxTagDepth)
	}
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	st := mkv.SimpleTag{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return st, err
		}
		switch eh.ID {
		case mkv.IDTagName:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Name = v
		case mkv.IDTagString:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Value = v
		case mkv.IDTagLanguage:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Language = v
		case mkv.IDTagBinary:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Binary = v
		case mkv.IDSimpleTag:
			sub, err := p.parseSimpleTagDepth(eh.Size, depth+1)
			if err != nil {
				return st, err
			}
			st.SubTags = append(st.SubTags, sub)
		default:
			if err := p.skip(eh.Size); err != nil {
				return st, err
			}
		}
	}
	return st, nil
}

func (p *parser) parseContentEncodings(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDContentEncoding {
			if err := p.parseContentEncoding(eh.Size, t); err != nil {
				return err
			}
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseContentEncoding(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDContentCompression {
			if err := p.parseContentCompression(eh.Size, t); err != nil {
				return err
			}
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseContentCompression(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDContentCompSettings:
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			t.HeaderStripping = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseCues(size int64, c *mkv.Container) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		if eh.ID == mkv.IDCuePoint {
			cp, err := p.parseCuePoint(eh.Size)
			if err != nil {
				return err
			}
			c.Cues = append(c.Cues, cp)
		} else {
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseCuePoint(size int64) (mkv.CuePoint, error) {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	cp := mkv.CuePoint{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return cp, err
		}
		switch eh.ID {
		case mkv.IDCueTime:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return cp, err
			}
			cp.TimeMs = int64(v)
		case mkv.IDCueTrackPositions:
			if err := p.parseCueTrackPositions(eh.Size, &cp); err != nil {
				return cp, err
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return cp, err
			}
		}
	}
	return cp, nil
}

func (p *parser) parseCueTrackPositions(size int64, cp *mkv.CuePoint) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		switch eh.ID {
		case mkv.IDCueTrack:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			cp.Track = v
		case mkv.IDCueClusterPos:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			cp.ClusterPos = int64(v)
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}
