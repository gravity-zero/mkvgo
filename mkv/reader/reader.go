package reader

import (
	"bytes"
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
	p := &parser{r: r, metaBudget: maxMetadataBytes}
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
	r          io.ReadSeeker
	metaBudget int64 // remaining bytes allowed for in-memory metadata
}

// maxMetadataBytes caps the TOTAL bytes a single parse pulls into the Container
// (attachments, codec-private, binary tags). The 512MB per-element cap does not
// bound a file with many large metadata elements; this does. Untrusted-input
// callers that ingest concurrently should still bound their own parallelism.
const maxMetadataBytes = 1 << 30 // 1 GiB

func (p *parser) chargeMeta(n int64) error {
	p.metaBudget -= n
	if p.metaBudget < 0 {
		return fmt.Errorf("in-memory metadata exceeds %d-byte budget", maxMetadataBytes)
	}
	return nil
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
			// A corrupted or zero-padded region in the body (seen in some real
			// rips: a multi-MB run of 0x00 between clusters) makes the next
			// element header undecodable. Rather than abort the whole read like
			// a strict parser, resync to the next Cluster and keep going, as
			// ffmpeg/mkvtoolnix do. If nothing recognizable remains, stop with
			// the metadata gathered so far.
			off, rerr := p.resyncToCluster(endPos)
			if rerr != nil {
				return rerr
			}
			if off < 0 {
				break
			}
			continue
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

// clusterMagic is the 4-byte Cluster element ID (0x1F43B675), the anchor used
// to resync after a corrupted/padded region in the body.
var clusterMagic = []byte{0x1F, 0x43, 0xB6, 0x75}

// clusterChildIDs are element IDs that can legitimately be the first child of a
// Cluster. A real cluster opens with one of these; requiring it rejects a
// clusterMagic byte-sequence that occurs by chance inside corrupted data.
var clusterChildIDs = map[uint32]bool{
	mkv.IDTimestamp:   true, // 0xE7
	mkv.IDSimpleBlock: true, // 0xA3
	mkv.IDBlockGroup:  true, // 0xA0
	mkv.IDVoid:        true, // 0xEC
	0xA7:              true, // Position
	0xAB:              true, // PrevSize
	0xBF:              true, // CRC-32
	0x5854:            true, // SilentTracks
}

// resyncToCluster scans forward from the current position for the next *valid*
// Cluster, bounded by limit (the segment end, or -1 to scan until EOF). A
// candidate is accepted only if isClusterAt confirms it (real Cluster ID + a
// recognizable first child), so a magic sequence occurring by chance inside
// corruption is skipped rather than trusted. On success it positions the reader
// at the Cluster ID and returns its offset; it returns -1 (with a nil error)
// when no valid Cluster remains before limit. Only genuine I/O errors are returned.
func (p *parser) resyncToCluster(limit int64) (int64, error) {
	from, err := p.r.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	for {
		off, err := p.scanForMagic(from, limit)
		if err != nil || off < 0 {
			return -1, err
		}
		valid, err := p.isClusterAt(off, limit)
		if err != nil {
			return -1, err
		}
		if valid {
			if _, err := p.r.Seek(off, io.SeekStart); err != nil {
				return -1, err
			}
			return off, nil
		}
		from = off + 1 // false positive: resume scanning just past it
	}
}

// scanForMagic returns the absolute offset of the next clusterMagic at or after
// `from` and before `limit` (-1 = until EOF), or -1 if none. It reads forward in
// windows, carrying the last few bytes so a magic split across a read boundary
// is still found.
func (p *parser) scanForMagic(from, limit int64) (int64, error) {
	if _, err := p.r.Seek(from, io.SeekStart); err != nil {
		return -1, err
	}
	const window = 64 << 10
	buf := make([]byte, len(clusterMagic)-1+window)
	tail := 0
	next := from
	for {
		base := next - int64(tail) // absolute offset of buf[0]
		n, rerr := p.r.Read(buf[tail : tail+window])
		end := tail + n

		search := buf[:end]
		if limit >= 0 {
			if max := limit - base; max < int64(end) {
				if max < 0 {
					max = 0
				}
				search = buf[:max]
			}
		}
		if i := bytes.Index(search, clusterMagic); i >= 0 {
			return base + int64(i), nil
		}

		next += int64(n)
		if (limit >= 0 && next >= limit) || rerr != nil {
			return -1, nil
		}
		keep := len(clusterMagic) - 1
		if end < keep {
			keep = end
		}
		copy(buf[:keep], buf[end-keep:end])
		tail = keep
	}
}

// isClusterAt reports whether a real Cluster begins at off: the element ID must
// be Cluster, its declared size must decode and (when known) fit within limit,
// and its first child must be a recognizable cluster-level element.
func (p *parser) isClusterAt(off, limit int64) (bool, error) {
	if _, err := p.r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	h, _, err := p.readHeader()
	if err != nil || h.ID != mkv.IDCluster {
		return false, nil
	}
	bodyStart, _ := p.r.Seek(0, io.SeekCurrent)
	if h.Size >= 0 && limit >= 0 && bodyStart+h.Size > limit {
		return false, nil // declared size overruns the segment — not a real cluster
	}
	child, _, err := p.readHeader()
	if err != nil {
		return false, nil
	}
	return clusterChildIDs[child.ID], nil
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
			if v > 0 { // keep the 1000000 default; a 0 scale would divide-by-zero downstream
				c.Info.TimecodeScale = int64(v)
			}
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
	t := mkv.Track{}

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
		case mkv.IDTrackUID:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.UID = v
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
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
			t.LanguagePresent = true
		case mkv.IDLanguageBCP47:
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.LanguageBCP47 = v
			t.LanguagePresent = true
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
			t.DefaultPresent = true
		case mkv.IDFlagForced:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.IsForced = v == 1
			t.ForcedPresent = true
		case mkv.IDDefaultDuration:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			if v > 0 {
				fps := 1e9 / float64(v)
				t.FrameRate = &fps
			}
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
	// FlagDefault defaults to 1 per the Matroska spec when absent: keep that for
	// IsDefault, but DefaultPresent stays false so a consumer can tell an explicit
	// flag from the applied default. Language is intentionally NOT defaulted to
	// "eng" (v0.4.0 behaviour change): an absent language stays "" with
	// LanguagePresent=false, matching what ffprobe reports.
	if !t.DefaultPresent {
		t.IsDefault = true
	}
	// DefaultDuration exists on audio tracks too (block duration), but exposing it
	// as FrameRate is only meaningful for video — and matches ffprobe, which only
	// reports r_frame_rate for video streams.
	if t.Type != mkv.VideoTrack {
		t.FrameRate = nil
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
		case mkv.IDColour:
			if err := p.parseColour(eh.Size, t); err != nil {
				return err
			}
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseColour reads the Video>Colour element (0x55B0), populating the track's
// CICP colour code points (matrix/transfer/primaries/range) and video bit depth.
// Each field stays nil when its sub-element is absent.
func (p *parser) parseColour(size int64, t *mkv.Track) error {
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
		case mkv.IDColourMatrix:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			cs := uint16(v)
			t.ColorSpace = &cs
		case mkv.IDColourTransfer:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			tr := uint16(v)
			t.ColorTransfer = &tr
		case mkv.IDColourPrimaries:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			pr := uint16(v)
			t.ColorPrimaries = &pr
		case mkv.IDColourRange:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			rg := uint16(v)
			t.ColorRange = &rg
		case mkv.IDColourBitsPerChannel:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			bd := uint16(v)
			t.VideoBitDepth = &bd
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
			ch, err := p.parseChapterAtom(eh.Size, 0)
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

func (p *parser) parseChapterAtom(size int64, depth int) (mkv.Chapter, error) {
	if depth > maxChapterDepth {
		return mkv.Chapter{}, fmt.Errorf("chapter nesting exceeds %d levels", maxChapterDepth)
	}
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
			sub, err := p.parseChapterAtom(eh.Size, depth+1)
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
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

const maxChapterDepth = 64

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
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
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
