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

func OpenWithFS(ctx context.Context, path string, fs *mkv.FS, opts ...ReadOption) (*mkv.Container, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return Read(ctx, f, path, opts...)
}

// fullReadBufSize buffers the head of a full read. The EBML reader pulls VINTs a
// byte at a time and the parser issues many position queries (Seek(0,
// SeekCurrent)) while parsing Info/Tracks/Cues; a bufReadSeeker serves both from
// memory, so a SeekHead jump completes in a handful of real seeks rather than
// one per VINT. The cluster-walk fallback unbuffers itself (parseSegment), so a
// body-skip there never refills a window it would immediately discard.
const fullReadBufSize = 32 << 10

func Read(ctx context.Context, r io.ReadSeeker, path string, opts ...ReadOption) (*mkv.Container, error) {
	var o readOpts
	for _, opt := range opts {
		opt(&o)
	}
	br, err := newBufReadSeeker(r, fullReadBufSize)
	if err != nil {
		return nil, err
	}
	p := &parser{r: br, metaBudget: maxMetadataBytes, ctx: ctx, lazyAttachments: o.lazyAttachments, path: path}
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
	// Surface the per-track "BPS" bitrate tag (mainstream muxers write it; probers report
	// it as bit_rate) as the typed Track.Bitrate, now that Tags are parsed.
	promoteTrackBitrate(c)
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
// tail - the file was cut mid-element after the head metadata was already read.
// It surfaces as an unexpected EOF; with the tracks in hand there is nothing more
// to gain by failing the whole read, so return what was parsed, as external probers do.
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
	metaBudget int64           // remaining bytes allowed for in-memory metadata
	segStart   int64           // Segment body start offset (set once the Segment header is read)
	segEnd     int64           // Segment body end offset, or -1 for an unknown-size Segment
	ctx        context.Context // cancellation, also honoured in the inner element loops
	// lazyAttachments records where an attachment payload lives instead of
	// loading it (reader.WithoutAttachmentData); path is the file it refers to.
	lazyAttachments bool
	path            string
	// posBase is the file offset the current reader's offset 0 corresponds to.
	// It is 0 for the file reader and non-zero only while a tail BUFFER stands
	// in for it, so a position recorded as an offset in the file stays one.
	posBase int64
}

// checkCtx reports a cancellation error so the inner element loops (parseInfo /
// parseTracks / parseTrackEntry / parseTags / …) stop promptly on a cancelled
// context, not only the top-level Segment walk.
func (p *parser) checkCtx() error {
	if p.ctx == nil {
		return nil
	}
	return p.ctx.Err()
}

// elemErr wraps a parse failure with the element's ID and byte offset, so a
// failure on a real-world (often non-conformant) file is debuggable instead of a
// bare "unexpected EOF". Applied at the Segment walk, which sees every element.
func (p *parser) elemErr(id uint32, at int64, err error) error {
	return elemErrAt(id, at, err)
}

// elemErrAt is the shared element-context wrapper, used by both the seekable and
// the streaming Segment walks.
func elemErrAt(id uint32, at int64, err error) error {
	return fmt.Errorf("element 0x%X at offset %d: %w", id, at, err)
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
	// Reject an unknown size (-1): only Segment and Cluster may be unknown-size, so
	// inside a leaf element it is malformed. Skipping it as Seek(-1) would seek a
	// byte backwards and desync the framing - a clean error is better than garbage.
	// Top-level loops guard size<0 before reaching here (they resync instead).
	if size < 0 {
		return fmt.Errorf("cannot skip element with unknown size")
	}
	_, err := p.r.Seek(size, io.SeekCurrent)
	return err
}

// readFlag reads a Matroska boolean flag element (1 = set, anything else clear).
func (p *parser) readFlag(size int64) (bool, error) {
	v, err := ebml.ReadUint(p.r, size)
	return v == 1, err
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
	c.SegmentStart = segStart
	var endPos int64 = -1
	if h.Size >= 0 {
		endPos = segStart + h.Size
	}

	var seekOffsets []int64 // absolute offsets of the elements a head SeekHead points at
	jumpedToTail := false
	triedTailScan := false
	// The spec allows exactly one Info and one Tracks. Some non-conformant muxers
	// write more; take the FIRST of each and skip the rest, so a duplicate Tracks
	// does not append a second set of tracks (and so Read matches ReadMeta).
	var gotInfo, gotTracks bool

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
			// mainstream tools do. If nothing recognizable remains, stop with
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
			if gotInfo {
				if err := p.skip(eh.Size); err != nil {
					return err
				}
				break // duplicate Info: first wins
			}
			if err := p.parseInfo(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			gotInfo = true
		case mkv.IDTracks:
			if gotTracks {
				if err := p.skip(eh.Size); err != nil {
					return err
				}
				break // duplicate Tracks: first wins (avoid appending a second set)
			}
			if err := p.parseTracks(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			gotTracks = true
		case mkv.IDChapters:
			if err := p.parseChapters(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
		case mkv.IDAttachments:
			if err := p.parseAttachments(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
		case mkv.IDTags:
			if err := p.parseTags(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
		case mkv.IDCues:
			if err := p.parseCues(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
		case mkv.IDSeekHead:
			offs, err := p.parseSeekHeadOffsets(eh.Size, segStart, endPos)
			if err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			seekOffsets = append(seekOffsets, offs...)
		case mkv.IDCluster:
			// Reaching the media is where a sequential walk gets expensive: the
			// Cues/Tags/Attachments that sit AFTER the clusters are only found by
			// reading one header per cluster - thousands on a long file, each a
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
					// A SeekHead entry that does not resolve to a real element -
					// commonly a Cues pointer for a Cues index the muxer never
					// wrote (cues=0 files still list one), or one whose position
					// is stale. The SeekHead is no longer trustworthy for the
					// tail: jumping to another of its entries could skip a real
					// element it mis-references. Recover the actual contiguous
					// tail with ONE bounded scan back from EOF (the real Cues +
					// any following Tags/Chapters that tile to the file end)
					// instead of distrusting it and walking every cluster. If
					// there is no such tail (cues=0, nothing past the clusters),
					// the head elements are all parsed, so stop without the walk.
					if !triedTailScan {
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
					return nil
				} else if len(seekOffsets) > 0 {
					// A SeekHead gave us valid offsets and NONE lie past the clusters:
					// everything it indexes (Info/Tracks/Cues/…) is at the head and
					// already parsed, so there is nothing to find by walking the
					// clusters to EOF - stop. A trailing element listed in a nested
					// tail SeekHead would have an offset past the clusters and taken
					// the jump branch above, so it is not missed here.
					return nil
				} else if len(c.Cues) == 0 && !triedTailScan {
					// No SeekHead, and the Cues have not appeared yet: on most real
					// libraries (~70% here) the muxer omits the SeekHead and writes the
					// Cues at the very end. Rather than walk every cluster to reach
					// them, scan back from EOF for a Cues element that tiles exactly to
					// the file end, and parse the tail straight from that one read. Done
					// at most once - a file with no tail Cues falls through to the walk.
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
			// reader first - keeping the buffer here would refill (and immediately
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

// minOffsetAtOrAfter returns the smallest offset in offs that is >= floor - the
// start of the post-cluster metadata region - or ok=false if none qualifies.
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
// (Cues plus any following Tags/Chapters/…) straight from that buffer - no
// re-read - and returns true; the caller is then done. On a miss it returns
// false and the caller walks the clusters. Using the real EOF (not the declared
// segment size) keeps it safe on truncated files: a short read or a tail that
// does not tile simply misses and defers to the resync-tolerant walk.
func (p *parser) scanTailForCues(c *mkv.Container) (bool, error) {
	start := p.pos() // current cluster body - never scan before here
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
	if err := p.parseTailBuffer(c, buf[rel:], start+int64(rel)); err != nil {
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
func (p *parser) parseTailBuffer(c *mkv.Container, tail []byte, base int64) error {
	saved := p.r
	savedBase := p.posBase
	p.r = bytes.NewReader(tail)
	// Offsets taken from here are relative to the buffer; anything recorded as
	// a position IN THE FILE (a lazy attachment's DataOffset) must add this
	// base back, or it names bytes that belong to some cluster instead.
	p.posBase = base
	defer func() { p.r, p.posBase = saved, savedBase }()
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
	return ResyncToCluster(p.r, limit)
}

// ResyncToCluster scans r forward from its current position for the next
// structurally valid Cluster (element ID 0x1F43B675 followed by a recognizable
// first child), bounded by limit (an absolute offset, or -1 to scan until
// EOF). A candidate is accepted only when its declared size (when known) fits
// within limit and its first child decodes as a real cluster-level element, so
// a clusterMagic byte sequence occurring by chance inside corrupted data is
// skipped rather than trusted. On success it leaves r positioned at the
// Cluster's element ID and returns that offset; it returns -1 with a nil error
// when no valid Cluster is found before limit (including a genuine EOF). Only
// a real I/O failure is returned as an error.
//
// This is the same bounded, validated resync parseSegment falls back to on a
// corrupted or zero-padded region; it is exported so an out-of-parser
// recovery tool (ops.Salvage) can reuse it without pulling in the full
// parser/Container machinery. A caller wanting a hard cap on the scan
// distance (rather than "until segment end") passes limit = currentOffset+cap.
func ResyncToCluster(r io.ReadSeeker, limit int64) (int64, error) {
	from, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	for {
		off, err := scanForClusterMagic(r, from, limit)
		if err != nil || off < 0 {
			return -1, err
		}
		valid, err := isClusterAt(r, off, limit)
		if err != nil {
			return -1, err
		}
		if valid {
			if _, err := r.Seek(off, io.SeekStart); err != nil {
				return -1, err
			}
			return off, nil
		}
		from = off + 1 // false positive: resume scanning just past it
	}
}

// scanForClusterMagic returns the absolute offset of the next clusterMagic at
// or after `from` and before `limit` (-1 = until EOF), or -1 if none. It reads
// forward in windows, carrying the last few bytes so a magic split across a
// read boundary is still found.
func scanForClusterMagic(r io.ReadSeeker, from, limit int64) (int64, error) {
	if _, err := r.Seek(from, io.SeekStart); err != nil {
		return -1, err
	}
	const window = 64 << 10
	buf := make([]byte, len(clusterMagic)-1+window)
	tail := 0
	next := from
	for {
		base := next - int64(tail) // absolute offset of buf[0]
		n, rerr := r.Read(buf[tail : tail+window])
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
		if limit >= 0 && next >= limit {
			return -1, nil
		}
		if rerr != nil {
			// A clean end of input is "no cluster here"; a real read failure is
			// not, and reporting it as one lets the caller finish with a
			// silently truncated Container and a nil error.
			if errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF) {
				return -1, nil
			}
			return -1, rerr
		}
		if n == 0 {
			// A reader may legally return (0, nil); without this the scan makes
			// no progress and never terminates when limit < 0.
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
func isClusterAt(r io.ReadSeeker, off, limit int64) (bool, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	h, _, err := ebml.ReadElementHeader(r)
	if err != nil || h.ID != mkv.IDCluster {
		return false, nil
	}
	bodyStart, _ := r.Seek(0, io.SeekCurrent)
	if h.Size >= 0 && limit >= 0 && bodyStart+h.Size > limit {
		return false, nil // declared size overruns the segment - not a real cluster
	}
	child, _, err := ebml.ReadElementHeader(r)
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
		if err := p.checkCtx(); err != nil {
			return err
		}
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
			// Keep the 1000000 default on 0 (divide-by-zero downstream) and on
			// anything past MaxInt64, which int64() would turn negative - and a
			// negative scale flips the overflow guards that consume it.
			if v > 0 && v <= math.MaxInt64 {
				c.Info.TimecodeScale = int64(v)
			}
		case mkv.IDDuration:
			v, err := ebml.ReadFloat(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.Duration = v
		case mkv.IDMuxingApp:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.MuxingApp = v
		case mkv.IDWritingApp:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.WritingApp = v
		case mkv.IDTitle:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.SegmentUID = v
		case mkv.IDPrevUID:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return err
			}
			c.Info.PrevUID = v
		case mkv.IDNextUID:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
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
		if err := p.checkCtx(); err != nil {
			return err
		}
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
		if err := p.checkCtx(); err != nil {
			return t, err
		}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.Language = v
			t.LanguagePresent = true
		case mkv.IDLanguageBCP47:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.LanguageBCP47 = v
			t.LanguagePresent = true
		case mkv.IDName:
			if err := p.chargeMeta(eh.Size); err != nil {
				return t, err
			}
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
		case mkv.IDFlagHearingImpaired, mkv.IDFlagVisualImpaired, mkv.IDFlagTextDescriptions,
			mkv.IDFlagOriginal, mkv.IDFlagCommentary:
			b, e := p.readFlag(eh.Size)
			if e != nil {
				return t, e
			}
			switch eh.ID {
			case mkv.IDFlagHearingImpaired:
				t.HearingImpaired = b
			case mkv.IDFlagVisualImpaired:
				t.VisualImpaired = b
			case mkv.IDFlagTextDescriptions:
				t.TextDescriptions = b
			case mkv.IDFlagOriginal:
				t.Original = b
			case mkv.IDFlagCommentary:
				t.Commentary = b
			}
		case mkv.IDDefaultDuration:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			if v > 0 {
				fps := 1e9 / float64(v)
				t.FrameRate = &fps
				t.DefaultDurationNs = int64(v)
			}
		case mkv.IDCodecDelay:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.CodecDelay = int64(v)
		case mkv.IDSeekPreRoll:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return t, err
			}
			t.SeekPreRoll = int64(v)
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
	// LanguagePresent=false, matching what probers report.
	if !t.DefaultPresent {
		t.IsDefault = true
	}
	// DefaultDuration exists on audio tracks too (block duration), but exposing it
	// as FrameRate is only meaningful for video - and matches external probers, which only
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
// interlaced, 2 progressive) to the conventional field_order string, "" when
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

// parseProjection reads the Video>Projection element and records its
// ProjectionType (360/spherical layout) as the track's Projection name.
func (p *parser) parseProjection(size int64, t *mkv.Track) error {
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
		case mkv.IDProjectionType:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			t.Projection = mkv.ProjectionTypeName(v)
		default:
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
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
		case mkv.IDStereoMode:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			if v != 0 { // 0 = mono: leave nil (no stereo)
				sm := uint16(v)
				t.StereoMode = &sm
			}
		case mkv.IDProjection:
			if err := p.parseProjection(eh.Size, t); err != nil {
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
	t.ColourDetermined = true // the container carries an explicit Colour element
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
		case mkv.IDColourMaxCLL:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			ensureHDR(t).MaxCLL = uint32(v)
		case mkv.IDColourMaxFALL:
			v, err := ebml.ReadUint(p.r, eh.Size)
			if err != nil {
				return err
			}
			ensureHDR(t).MaxFALL = uint32(v)
		case mkv.IDMasteringMetadata:
			if err := p.parseMasteringMetadata(eh.Size, t); err != nil {
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

// ensureHDR lazily allocates the track's HDR10 static-metadata holder.
func ensureHDR(t *mkv.Track) *mkv.HDRStaticMetadata {
	if t.HDR == nil {
		t.HDR = &mkv.HDRStaticMetadata{}
	}
	return t.HDR
}

// parseMasteringMetadata reads the MasteringMetadata element (SMPTE ST 2086): the
// R/G/B primary and white-point chromaticities and the luminance range, all EBML
// floats already in the units MasteringDisplay stores (chromaticity 0..1, cd/m²).
func (p *parser) parseMasteringMetadata(size int64, t *mkv.Track) error {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	end := cur + size
	md := &mkv.MasteringDisplay{}
	for {
		pos, _ := p.r.Seek(0, io.SeekCurrent)
		if pos >= end {
			break
		}
		eh, _, err := p.readHeader()
		if err != nil {
			return err
		}
		set := func(dst *float64) error {
			v, e := ebml.ReadFloat(p.r, eh.Size)
			if e == nil {
				*dst = v
			}
			return e
		}
		switch eh.ID {
		case mkv.IDPrimaryRChromaX:
			err = set(&md.RedX)
		case mkv.IDPrimaryRChromaY:
			err = set(&md.RedY)
		case mkv.IDPrimaryGChromaX:
			err = set(&md.GreenX)
		case mkv.IDPrimaryGChromaY:
			err = set(&md.GreenY)
		case mkv.IDPrimaryBChromaX:
			err = set(&md.BlueX)
		case mkv.IDPrimaryBChromaY:
			err = set(&md.BlueY)
		case mkv.IDWhitePointChromaX:
			err = set(&md.WhiteX)
		case mkv.IDWhitePointChromaY:
			err = set(&md.WhiteY)
		case mkv.IDLuminanceMax:
			err = set(&md.LuminanceMax)
		case mkv.IDLuminanceMin:
			err = set(&md.LuminanceMin)
		default:
			err = p.skip(eh.Size)
		}
		if err != nil {
			return err
		}
	}
	ensureHDR(t).MasteringDisplay = md
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
	// EditionFlagOrdered is read but deliberately dropped, not pending: an
	// ordered edition plays chapters in its own order and leans on segment
	// linking (ChapterSegmentUID), which mkvgo neither follows nor writes.
	// Carrying the flag onto a rewrite whose links are gone would promise a
	// playback order nothing can honour.
	_ = ordered
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return ch, err
			}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.Name = v
		case mkv.IDFileMimeType:
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.MIMEType = v
		case mkv.IDFileData:
			att.Size = eh.Size
			if p.lazyAttachments {
				// Note where the bytes are and step over them: a font set is
				// unbounded user data, and an op that only copies it has no
				// reason to hold it. See reader.WithoutAttachmentData.
				at, _ := p.r.Seek(0, io.SeekCurrent)
				att.DataPath, att.DataOffset = p.path, p.posBase+at
				if err := p.skip(eh.Size); err != nil {
					return att, err
				}
				continue
			}
			if err := p.chargeMeta(eh.Size); err != nil {
				return att, err
			}
			data, err := ebml.ReadBytes(p.r, eh.Size)
			if err != nil {
				return att, err
			}
			att.Data = data
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
		if err := p.checkCtx(); err != nil {
			return err
		}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Name = v
		case mkv.IDTagString:
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
			v, err := ebml.ReadString(p.r, eh.Size)
			if err != nil {
				return st, err
			}
			st.Value = v
		case mkv.IDTagLanguage:
			if err := p.chargeMeta(eh.Size); err != nil {
				return st, err
			}
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
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
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
	// readExact rather than make([]byte, size): size comes from the Cues
	// element header, which on a remote FS is only as trustworthy as the
	// server's reported length, so a hostile declared size must not be
	// allocated up front (it grows as bytes actually arrive).
	buf, err := readExact(p.r, size)
	if err != nil {
		return err
	}
	saved := p.r
	p.r = bytes.NewReader(buf)
	err = p.parseCuesFrom(int64(len(buf)), c)
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
