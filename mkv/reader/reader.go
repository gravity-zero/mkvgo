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

// fullReadBufSize buffers the head of a full read. The EBML reader pulls VINTs a
// byte at a time and the parser issues many position queries (Seek(0,
// SeekCurrent)) while parsing Info/Tracks/Cues; a bufReadSeeker serves both from
// memory, so a SeekHead jump completes in a handful of real seeks rather than
// one per VINT. The cluster-walk fallback unbuffers itself (parseSegment), so a
// body-skip there never refills a window it would immediately discard.
const fullReadBufSize = 32 << 10

func Read(ctx context.Context, r io.ReadSeeker, path string) (*mkv.Container, error) {
	br, err := newBufReadSeeker(r, fullReadBufSize)
	if err != nil {
		return nil, err
	}
	p := &parser{r: br, metaBudget: maxMetadataBytes}
	c := &mkv.Container{Path: path}

	if err := p.parseEBMLHeader(); err != nil {
		if looksLikeISOBMFF(r) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotMatroska)
		}
		return nil, fmt.Errorf("ebml header: %w", err)
	}
	if err := p.parseSegment(ctx, c); err != nil && !tolerableTailError(err, c) {
		return nil, fmt.Errorf("segment: %w", err)
	}
	if err := setDurationMs(c); err != nil {
		return nil, err
	}
	// Derive the keyframe index from the Cues a full Read already parsed, so
	// Container.Keyframes is available from Read as well as the metadata path.
	c.Keyframes = keyframeTimesMs(c)
	return c, nil
}

// ErrNotMatroska wraps the read error when the input is not Matroska/WebM but
// looks like ISO base media (MP4): an EBML-header failure whose first box is a
// recognised ISOBMFF box. A caller that dispatched by file extension can catch it
// (errors.Is) and re-route to the mp4 reader instead of reporting a cryptic
// EBML error.
var ErrNotMatroska = errors.New("not Matroska/WebM (looks like ISO base media / MP4)")

// looksLikeISOBMFF peeks the first box header to tell an MP4-family file from
// genuinely-corrupt Matroska: ISOBMFF opens with [size][type] where type is a
// known box. Best-effort; a read/seek failure just reports false.
func looksLikeISOBMFF(r io.ReadSeeker) bool {
	var b [8]byte
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return false
	}
	switch string(b[4:8]) {
	case "ftyp", "styp", "moov", "moof", "mdat", "sidx", "free", "skip":
		return true
	}
	return false
}

// tolerableTailError reports whether a segment-parse error is just a truncated
// tail — the file was cut mid-element after the head metadata was already read.
// It surfaces as an unexpected EOF; with the tracks in hand there is nothing more
// to gain by failing the whole read, so return what was parsed, as ffprobe does.
func tolerableTailError(err error, c *mkv.Container) bool {
	return (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) && len(c.Tracks) > 0
}

// setDurationMs derives c.DurationMs from Info.Duration (in TimecodeScale units),
// guarding against an overflowing product. Shared by Read and ReadMeta so both
// report identical durations.
func setDurationMs(c *mkv.Container) error {
	if c.Info.Duration > 0 && c.Info.TimecodeScale > 0 {
		d := c.Info.Duration * float64(c.Info.TimecodeScale) / 1e6
		if d > float64(math.MaxInt64) || d < float64(math.MinInt64) {
			return fmt.Errorf("duration overflow: %g * %d", c.Info.Duration, c.Info.TimecodeScale)
		}
		c.DurationMs = int64(d)
	}
	return nil
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

	segStart := p.pos()
	var endPos int64 = -1
	if h.Size >= 0 {
		endPos = segStart + h.Size
	}

	var seekOffsets []int64 // absolute offsets of the elements a head SeekHead points at
	jumpedToTail := false
	triedTailScan := false

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		elemStart := p.pos()
		if endPos >= 0 && elemStart >= endPos {
			break
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
		case mkv.IDSeekHead:
			offs, err := p.parseSeekHeadOffsets(eh.Size, segStart, endPos)
			if err != nil {
				return err
			}
			seekOffsets = append(seekOffsets, offs...)
		case mkv.IDCluster:
			// Reaching the media is where a sequential walk gets expensive: the
			// Cues/Tags/Attachments that sit AFTER the clusters are only found by
			// reading one header per cluster — thousands on a long file, each a
			// round-trip on a network FS. If a head SeekHead told us where the
			// post-cluster metadata begins, Seek straight to it ONCE and resume the
			// normal walk from there, skipping the whole cluster region. Resuming
			// the walk (rather than jumping to each referenced element) means a tail
			// element missing from the SeekHead is still discovered, and the merge/
			// resync semantics of the sequential reader are preserved.
			if !jumpedToTail {
				if tailStart, ok := minOffsetAtOrAfter(seekOffsets, elemStart); ok {
					valid, err := p.offsetIsSegmentElement(tailStart)
					if err != nil {
						return err
					}
					if valid {
						jumpedToTail = true
						if _, err := p.r.Seek(tailStart, io.SeekStart); err != nil {
							return err
						}
						continue
					}
					seekOffsets = nil // stale SeekHead: stop trusting it, walk the clusters
				} else if len(seekOffsets) > 0 {
					// A SeekHead gave us valid offsets and NONE lie past the clusters:
					// everything it indexes (Info/Tracks/Cues/…) is at the head and
					// already parsed, so there is nothing to find by walking the
					// clusters to EOF — stop. A trailing element listed in a nested
					// tail SeekHead would have an offset past the clusters and taken
					// the jump branch above, so it is not missed here.
					return nil
				} else if len(c.Cues) == 0 && !triedTailScan {
					// No SeekHead, and the Cues have not appeared yet: on most real
					// libraries (~70% here) the muxer omits the SeekHead and writes the
					// Cues at the very end. Rather than walk every cluster to reach
					// them, scan back from EOF for a Cues element that tiles exactly to
					// the file end, and parse the tail straight from that one read. Done
					// at most once — a file with no tail Cues falls through to the walk.
					triedTailScan = true
					bodyPos := p.pos()
					done, err := p.scanTailForCues(c)
					if err != nil {
						return err
					}
					if done {
						return nil
					}
					if _, err := p.r.Seek(bodyPos, io.SeekStart); err != nil {
						return err
					}
				}
			}
			// Fallback walk: no usable SeekHead and no tail Cues found, so every
			// cluster header must be read to reach whatever follows. Drop to the raw
			// reader first — keeping the buffer here would refill (and immediately
			// discard) a full window per cluster body-skip, ballooning a 0.3 MB walk
			// into one window per cluster.
			if err := p.unbuffer(); err != nil {
				return err
			}
			if eh.Size < 0 {
				return fmt.Errorf("unknown-size element 0x%X cannot be skipped", eh.ID)
			}
			if err := p.skip(eh.Size); err != nil {
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

// parseSeekHeadOffsets reads a SeekHead and returns the absolute file offset of
// every element it references (Info, Tracks, Cues, Tags, a nested SeekHead, …),
// reusing parseSeekEntry. SeekPosition is relative to the Segment data start
// (segStart). Entries that fall outside [segStart, endPos) are dropped.
func (p *parser) parseSeekHeadOffsets(size, segStart, endPos int64) ([]int64, error) {
	var offs []int64
	end := p.pos() + size
	for p.pos() < end {
		eh, _, e := p.readHeader()
		if e != nil {
			return offs, e
		}
		if eh.ID != mkv.IDSeek {
			if eh.Size < 0 {
				return offs, nil
			}
			if e := p.skip(eh.Size); e != nil {
				return offs, e
			}
			continue
		}
		_, pos, ok, e := p.parseSeekEntry(eh.Size)
		if e != nil {
			return offs, e
		}
		if !ok {
			continue
		}
		abs := segStart + pos
		if abs < segStart || (endPos >= 0 && abs >= endPos) {
			continue // out-of-range or overflowed (huge SeekPosition) entry
		}
		offs = append(offs, abs)
	}
	return offs, nil
}

// minOffsetAtOrAfter returns the smallest offset in offs that is >= floor — the
// start of the post-cluster metadata region — or ok=false if none qualifies.
func minOffsetAtOrAfter(offs []int64, floor int64) (int64, bool) {
	best := int64(-1)
	for _, o := range offs {
		if o >= floor && (best < 0 || o < best) {
			best = o
		}
	}
	return best, best >= 0
}

// offsetIsSegmentElement reports whether off points at a decodable segment-level
// element, without disturbing the reader position. It guards the SeekHead jump
// against a stale/forged SeekPosition that lands mid-cluster: an undecodable or
// unexpected header there means the SeekHead is not trustworthy, so the caller
// falls back to walking the clusters rather than parsing garbage.
func (p *parser) offsetIsSegmentElement(off int64) (bool, error) {
	saved := p.pos()
	if _, err := p.r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	eh, _, herr := p.readHeader()
	if _, err := p.r.Seek(saved, io.SeekStart); err != nil {
		return false, err
	}
	if herr != nil {
		return false, nil
	}
	return isSegmentLevelID(eh.ID), nil
}

// unbuffer swaps a buffered reader for its raw underlying reader, positioned at
// the current logical offset. The cluster walk calls this so header reads are
// small and body-skips are bare seeks, with no buffer window to refill. It is a
// no-op once the reader is already raw.
func (p *parser) unbuffer() error {
	br, ok := p.r.(*bufReadSeeker)
	if !ok {
		return nil
	}
	raw, err := br.raw()
	if err != nil {
		return err
	}
	p.r = raw
	return nil
}

// cuesMagic is the 4-byte Cues element ID (0x1C53BB6B), scanned for from the end
// of a SeekHead-less file to locate the Cues index without walking the clusters.
var cuesMagic = []byte{0x1C, 0x53, 0xBB, 0x6B}

// tailScanWindow is the slice read from the real file end when hunting for a
// trailing Cues index. One bounded read: it covers a typical Cues+Tags
// (~100-300 KiB) so the common case is found immediately, while a file that has
// no tail Cues wastes only this much before falling back to the cluster walk. A
// Cues index that starts further than this from EOF is left to the walk.
const tailScanWindow = 512 << 10

// scanTailForCues locates a SeekHead-less file's trailing Cues by reading one
// window at the real end of the file and scanning it for a Cues element whose
// top-level elements tile exactly to EOF. On a hit it parses the whole tail
// (Cues plus any following Tags/Chapters/…) straight from that buffer — no
// re-read — and returns true; the caller is then done. On a miss it returns
// false and the caller walks the clusters. Using the real EOF (not the declared
// segment size) keeps it safe on truncated files: a short read or a tail that
// does not tile simply misses and defers to the resync-tolerant walk.
func (p *parser) scanTailForCues(c *mkv.Container) (bool, error) {
	start := p.pos() // current cluster body — never scan before here
	end, err := p.r.Seek(0, io.SeekEnd)
	if err != nil {
		return false, err
	}
	if end-start > tailScanWindow {
		start = end - tailScanWindow
	}
	if end <= start {
		return false, nil
	}
	buf := make([]byte, end-start)
	if _, err := p.r.Seek(start, io.SeekStart); err != nil {
		return false, err
	}
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return false, nil // truncated/short tail: defer to the walk
	}
	rel, ok := tailAnchor(buf, start, end)
	if !ok {
		return false, nil
	}
	if err := p.parseTailBuffer(c, buf[rel:]); err != nil {
		return false, err
	}
	return true, nil
}

// tailAnchor scans buf (covering [bufStart, end)) for the earliest Cues element
// ID whose elements tile exactly to end, returning its index within buf. A
// cuesMagic byte sequence occurring inside cluster data fails the tiling check,
// so it is not mistaken for the real index.
func tailAnchor(buf []byte, bufStart, end int64) (int, bool) {
	for i := 0; ; {
		j := bytes.Index(buf[i:], cuesMagic)
		if j < 0 {
			return 0, false
		}
		at := i + j
		if tilesToEnd(buf[at:], bufStart+int64(at), end) {
			return at, true
		}
		i = at + 1
	}
}

// tilesToEnd reports whether the elements starting at buf[0] (file offset abs)
// are all segment-level (or Void padding) with definite sizes and tile exactly
// to end, leaving no gap and never overshooting.
func tilesToEnd(buf []byte, abs, end int64) bool {
	for off := 0; abs < end; {
		eh, hlen, err := ebml.ReadElementHeader(bytes.NewReader(buf[off:]))
		if err != nil || eh.Size < 0 || !isTailElement(eh.ID) {
			return false
		}
		step := int64(hlen) + eh.Size
		if abs+step > end {
			return false
		}
		abs += step
		off += int(step)
	}
	return abs == end
}

func isTailElement(id uint32) bool {
	return id == mkv.IDVoid || isSegmentLevelID(id)
}

// parseTailBuffer parses the post-cluster metadata from an in-memory tail slice
// (already validated by tilesToEnd), reusing the normal element parsers via a
// bytes.Reader so no further I/O is needed.
func (p *parser) parseTailBuffer(c *mkv.Container, tail []byte) error {
	saved := p.r
	p.r = bytes.NewReader(tail)
	defer func() { p.r = saved }()
	end := int64(len(tail))
	for p.pos() < end {
		eh, _, err := p.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch eh.ID {
		case mkv.IDCues:
			err = p.parseCues(eh.Size, c)
		case mkv.IDTags:
			err = p.parseTags(eh.Size, c)
		case mkv.IDChapters:
			err = p.parseChapters(eh.Size, c)
		case mkv.IDAttachments:
			err = p.parseAttachments(eh.Size, c)
		case mkv.IDInfo:
			err = p.parseInfo(eh.Size, c)
		case mkv.IDTracks:
			err = p.parseTracks(eh.Size, c)
		default:
			if eh.Size < 0 {
				return fmt.Errorf("tail element 0x%X has unknown size", eh.ID)
			}
			err = p.skip(eh.Size)
		}
		if err != nil {
			return err
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
		case mkv.IDBlockAdditionMapping:
			if err := p.parseBlockAdditionMapping(eh.Size, &t); err != nil {
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
	// Fill colour/bit-depth/profile from the codec bitstream when the container
	// Colour element didn't (container values, parsed above, still win per field).
	fillColourFromCodecPrivate(&t)
	return t, nil
}

// interlacedName maps a Matroska FlagInterlaced value (0 undetermined, 1
// interlaced, 2 progressive) to the ffprobe-style field_order string, "" when
// undetermined.
func interlacedName(v uint64) string {
	switch v {
	case 1:
		return "interlaced"
	case 2:
		return "progressive"
	}
	return ""
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
		case mkv.IDFlagInterlaced:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			t.FieldOrder = interlacedName(v)
		case mkv.IDDisplayWidth:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			dw := uint32(v)
			t.DisplayWidth = &dw
		case mkv.IDDisplayHeight:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			dh := uint32(v)
			t.DisplayHeight = &dh
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

// parseBlockAdditionMapping reads a BlockAdditionMapping (0x41E4). When its
// BlockAddIDType marks Dolby Vision (dvcC/dvvC), the BlockAddIDExtraData holds the
// DOVIDecoderConfigurationRecord, which is decoded onto the track.
func (p *parser) parseBlockAdditionMapping(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	var addType uint64
	var extra []byte
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
		case mkv.IDBlockAddIDType:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			addType = v
		case mkv.IDBlockAddIDExtraData:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			extra = v
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	if addType == mkv.BlockAddIDTypeDVCC || addType == mkv.BlockAddIDTypeDVVC {
		if dv := mkv.ParseDolbyVisionConfig(extra); dv != nil {
			t.DolbyVision = dv
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
		case mkv.IDOutputSamplingFreq:
			v, err := ebml.ReadFloat(p.r, eh.Size)
			if err != nil {
				return err
			}
			t.OutputSampleRate = &v
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

// maxCuesBuffer caps how large a Cues index parseCues pulls into memory in a
// single read before falling back to streaming. Real Cues indexes are at most a
// few MB even for multi-hour 4K files, so this leaves generous headroom while
// refusing to allocate for a pathological or malicious declared size.
const maxCuesBuffer = 64 << 20 // 64 MiB

// parseCues reads the whole Cues index in ONE io.ReadFull and parses it from
// memory, instead of letting the nested CuePoint walk issue ~4 tiny reads per
// entry directly against p.r. A long file's index is tens of thousands of
// CuePoints: on local disk the per-read cost is negligible, but on a network FS
// (9p/SMB) every read is a round-trip and the index alone can take minutes. The
// in-memory bytes.Reader makes the nested seeks free. (ops/reindex.go already
// buffers cluster bodies the same way.) A declared size that is unknown (<0) or
// larger than maxCuesBuffer falls back to streaming off p.r.
func (p *parser) parseCues(size int64, c *mkv.Container) error {
	if size < 0 || size > maxCuesBuffer {
		return p.parseCuesFrom(size, c)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return err
	}
	saved := p.r
	p.r = bytes.NewReader(buf)
	err := p.parseCuesFrom(int64(len(buf)), c)
	p.r = saved // the real reader is already positioned past the Cues body
	return err
}

func (p *parser) parseCuesFrom(size int64, c *mkv.Container) error {
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
