package reader

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// metaBufSize is the read buffer for the metadata-only path. ~2 KiB covers the
// EBML header + SeekHead + Info + Tracks of a typical mkvmerge file in a single
// underlying Read, so the byte-at-a-time EBML VINT reads cost one syscall instead
// of hundreds — the difference that matters on a network-mounted library.
const metaBufSize = 2 << 10

// OpenMeta opens path and returns only its Info + Tracks via the fast
// metadata-only path (see ReadMeta). Chapters, Attachments, Tags and Cues are
// left nil. Use Open for a full read.
func OpenMeta(ctx context.Context, path string, opts ...ReadOption) (*mkv.Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return ReadMeta(ctx, f, path, opts...)
}

// OpenMetaWithFS is OpenMeta against a caller-provided FS.
func OpenMetaWithFS(ctx context.Context, path string, fs *mkv.FS, opts ...ReadOption) (*mkv.Container, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	return ReadMeta(ctx, f, path, opts...)
}

// ReadMeta reads ONLY the metadata an indexer needs — Info and Tracks — and stops
// as soon as it has both, never parsing Cues and never traversing Clusters. On a
// typical mkvmerge file that is the first ~1-2 KB, so ReadMeta is orders of
// magnitude faster and cheaper than a full Read.
//
// For a spec-conformant file (a single Info and a single Tracks element) it
// returns Tracks, Info and DurationMs byte-identical to a full Read. A malformed
// file with DUPLICATE Info/Tracks elements may differ: a full Read merges every
// occurrence (parseInfo overwrites, parseTracks concatenates) whereas ReadMeta
// stops at the first — fall back to Read if you must mirror that behaviour.
//
// The returned Container has Tracks and Info populated; Chapters, Attachments,
// Tags and Cues are left nil — call Read/Open for those.
//
// If a SeekHead is present at the Segment head it is used to seek straight to
// Info and Tracks, so files whose Tracks element sits after the Clusters are
// still handled without scanning the body. Reads are buffered.
//
// Unlike Read, ReadMeta does NOT resync past a corrupted/zero-padded region that
// precedes the metadata: on such a damaged head it returns what it managed to
// parse (possibly empty). Callers that must tolerate corruption before the
// metadata should fall back to Read.
func ReadMeta(ctx context.Context, r io.ReadSeeker, path string, opts ...ReadOption) (*mkv.Container, error) {
	var o readOpts
	for _, fn := range opts {
		fn(&o)
	}

	br, err := newBufReadSeeker(r, metaBufSize)
	if err != nil {
		return nil, err
	}
	p := &parser{r: br, metaBudget: maxMetadataBytes, ctx: ctx}
	c := &mkv.Container{Path: path}

	if err := p.parseEBMLHeader(); err != nil {
		if looksLikeISOBMFF(r) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotMatroska)
		}
		return nil, fmt.Errorf("ebml header: %w", err)
	}
	if err := p.parseSegmentMeta(ctx, c, o); err != nil && !tolerableTailError(err, c) {
		return nil, fmt.Errorf("segment: %w", err)
	}
	if err := setDurationMs(c); err != nil {
		return nil, err
	}
	// Keyframe recovery for a Cues-less file (Cues, when present, already filled
	// Keyframes head-only above and win). The complete index is preferred over the
	// sample when both are requested.
	if len(c.Keyframes) == 0 {
		switch {
		case o.keyframeIndex:
			c.Keyframes = p.buildKeyframeIndex(videoTrackID(c), c.Info.TimecodeScale)
		case o.sampledKeyframes > 0:
			c.Keyframes = p.sampleClusterKeyframes(o.sampledKeyframes, c.Info.TimecodeScale, videoTrackID(c))
		}
	}
	if o.inBandColour {
		// Opted-in, and only for tracks that need it: recover colour from the
		// first sample's in-band SPS. The head parse above stays untouched.
		fillColourFromFirstSample(ctx, r, c)
	}
	return c, nil
}

// parseSegmentMeta walks the Segment's top-level children only far enough to
// parse Info and Tracks — reusing the same parseInfo/parseTracks as the full
// reader for correctness parity — then returns. A head SeekHead is recorded so it
// can jump straight to Info/Tracks when they sit after the first Cluster.
func (p *parser) parseSegmentMeta(ctx context.Context, c *mkv.Container, o readOpts) error {
	h, _, err := p.readHeader()
	if err != nil {
		return err
	}
	if h.ID != mkv.IDSegment {
		return fmt.Errorf("expected Segment (0x%X), got 0x%X", mkv.IDSegment, h.ID)
	}
	segStart := p.pos()
	c.SegmentStart = segStart
	endPos := int64(-1)
	if h.Size >= 0 {
		endPos = segStart + h.Size
	}
	p.segStart, p.segEnd = segStart, endPos

	var gotInfo, gotTracks bool
	offs := headOffsets{info: -1, tracks: -1, cues: -1, tags: -1, attachments: -1, chapters: -1} // absolute offsets from a SeekHead, if any

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if endPos >= 0 && p.pos() >= endPos {
			break
		}
		elemStart := p.pos()
		eh, _, err := p.readHeader()
		if err != nil {
			break // EOF or an undecodable head region: return what we have
		}
		switch eh.ID {
		case mkv.IDInfo:
			if gotInfo {
				if err := p.skip(eh.Size); err != nil {
					return err
				}
				break // duplicate Info: first wins (parity with Read)
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
				break // duplicate Tracks: first wins
			}
			if err := p.parseTracks(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			gotTracks = true
		case mkv.IDCues:
			// Cues inline before the Clusters (less common): read them now so the
			// keyframe index comes from the same pass.
			if err := p.parseCues(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
		case mkv.IDTags:
			// Tags inline before the Clusters: read them now only for the per-track
			// BPS bitrate when requested; otherwise skip (Tags are not exposed here).
			if o.bitrate && c.Tags == nil {
				if err := p.parseTags(eh.Size, c); err != nil {
					return p.elemErr(eh.ID, elemStart, err)
				}
			} else if err := p.skip(eh.Size); err != nil {
				return err
			}
		case mkv.IDSeekHead:
			found, err := p.parseSeekHeadMeta(eh.Size, segStart, endPos)
			if err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			offs.merge(found)
		case mkv.IDCluster:
			// Media reached. If Info/Tracks are still missing, the only cheap way
			// to get them is a SeekHead jump; otherwise skip this cluster and keep
			// scanning (rare: head metadata after the first cluster, no SeekHead).
			resume := p.pos() + eh.Size
			if !gotInfo && offs.info >= 0 {
				ok, err := p.parseElementAt(offs.info, mkv.IDInfo, c, p.parseInfo)
				if err != nil {
					return err
				}
				if ok {
					gotInfo = true
				} else {
					offs.info = -1 // stale SeekHead offset: don't retry it on the next cluster
				}
			}
			if !gotTracks && offs.tracks >= 0 {
				ok, err := p.parseElementAt(offs.tracks, mkv.IDTracks, c, p.parseTracks)
				if err != nil {
					return err
				}
				if ok {
					gotTracks = true
				} else {
					offs.tracks = -1
				}
			}
			if gotInfo && gotTracks {
				return p.finalizeHeadMeta(c, offs, o)
			}
			if eh.Size < 0 {
				return p.finalizeHeadMeta(c, offs, o) // unknown-size cluster: cannot skip
			}
			if _, err := p.r.Seek(resume, io.SeekStart); err != nil {
				return err
			}
			continue
		default:
			if eh.Size < 0 {
				return fmt.Errorf("unknown-size element 0x%X cannot be skipped", eh.ID)
			}
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
		if gotInfo && gotTracks {
			break // early stop — Cues/Tags are pulled by finalizeHeadMeta, no Cluster scan
		}
	}
	return p.finalizeHeadMeta(c, offs, o)
}

// finalizeHeadMeta derives Container.Keyframes from the Cues seek index, and — when
// o.bitrate — fills each track's Bitrate from the Tags' BPS. Either element, if
// not already read inline, is reached by following its SeekHead offset (one seek to
// one element, no Cluster scan). It then leaves Cues and Tags nil — the metadata
// path's contract — exposing only the derived Keyframes and Track.Bitrate; with
// o.cues the raw CuePoints are kept too (WithCues), for cue-driven seeking.
func (p *parser) finalizeHeadMeta(c *mkv.Container, offs headOffsets, o readOpts) error {
	if len(c.Cues) == 0 && offs.cues >= 0 {
		if _, err := p.parseElementAt(offs.cues, mkv.IDCues, c, p.parseCues); err != nil {
			return err
		}
	}
	c.Keyframes = keyframeTimesMs(c)
	if !o.cues {
		c.Cues = nil
	}
	if (o.bitrate || o.tags) && c.Tags == nil && offs.tags >= 0 {
		if _, err := p.parseElementAt(offs.tags, mkv.IDTags, c, p.parseTags); err != nil {
			return err
		}
	}
	if o.attachments && c.Attachments == nil && offs.attachments >= 0 {
		if _, err := p.parseElementAt(offs.attachments, mkv.IDAttachments, c, p.parseAttachments); err != nil {
			return err
		}
	}
	if o.chapters && c.Chapters == nil && offs.chapters >= 0 {
		if _, err := p.parseElementAt(offs.chapters, mkv.IDChapters, c, p.parseChapters); err != nil {
			return err
		}
	}
	if o.bitrate {
		promoteTrackBitrate(c)
	}
	if !o.tags {
		c.Tags = nil
	}
	return nil
}

// parseElementAt seeks to off, verifies the element there has the expected ID,
// and parses it with fn. It returns (false, nil) — not an error — when the offset
// points at a different element (a stale/odd SeekHead entry), so the caller can
// fall back to scanning.
func (p *parser) parseElementAt(off int64, id uint32, c *mkv.Container, fn func(int64, *mkv.Container) error) (bool, error) {
	if off < 0 {
		return false, nil
	}
	if _, err := p.r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	eh, _, err := p.readHeader()
	if err != nil {
		// The offset came from an untrusted SeekHead; a header that won't decode
		// (e.g. it points past EOF) means a stale entry, not a fatal read error —
		// let the caller fall back to scanning instead of aborting ReadMeta.
		return false, nil
	}
	if eh.ID != id {
		return false, nil
	}
	if err := fn(eh.Size, c); err != nil {
		return false, err
	}
	return true, nil
}

// headOffsets holds the absolute file offsets of the head elements a SeekHead
// points at, -1 each when absent.
type headOffsets struct {
	info, tracks, cues, tags, attachments, chapters int64
}

// merge keeps the other SeekHead's entries where this one has none.
func (h *headOffsets) merge(o headOffsets) {
	for _, p := range []struct {
		dst *int64
		src int64
	}{
		{&h.info, o.info}, {&h.tracks, o.tracks}, {&h.cues, o.cues},
		{&h.tags, o.tags}, {&h.attachments, o.attachments}, {&h.chapters, o.chapters},
	} {
		if p.src >= 0 {
			*p.dst = p.src
		}
	}
}

// parseSeekHeadMeta reads a SeekHead and returns the absolute file offsets of
// the head elements it points at. SeekPosition is relative to the Segment's
// data start (segStart).
func (p *parser) parseSeekHeadMeta(size, segStart, endPos int64) (headOffsets, error) {
	offs := headOffsets{info: -1, tracks: -1, cues: -1, tags: -1, attachments: -1, chapters: -1}
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
		id, pos, ok, e := p.parseSeekEntry(eh.Size)
		if e != nil {
			return offs, e
		}
		if !ok {
			continue
		}
		abs := segStart + pos
		if abs < segStart || (endPos >= 0 && abs >= endPos) {
			continue // out-of-range or overflowed (huge SeekPosition) entry: ignore
		}
		switch id {
		case mkv.IDInfo:
			offs.info = abs
		case mkv.IDTracks:
			offs.tracks = abs
		case mkv.IDCues:
			offs.cues = abs
		case mkv.IDTags:
			offs.tags = abs
		case mkv.IDAttachments:
			offs.attachments = abs
		case mkv.IDChapters:
			offs.chapters = abs
		}
	}
	return offs, nil
}

// parseSeekEntry reads one Seek element, returning the referenced element ID and
// its SeekPosition. ok is false when either is missing.
func (p *parser) parseSeekEntry(size int64) (id uint32, pos int64, ok bool, err error) {
	pos = -1
	end := p.pos() + size
	for p.pos() < end {
		eh, _, e := p.readHeader()
		if e != nil {
			return id, pos, false, e
		}
		switch eh.ID {
		case mkv.IDSeekID:
			// A Matroska element ID is 1-4 bytes. A forged SeekID declaring a huge
			// size would otherwise make ReadBytes pull up to MaxElementSize (512 MB)
			// off an untrusted file on the fast meta path, uncharged to metaBudget.
			// Skip anything out of range instead of reading it.
			if eh.Size < 0 {
				return id, pos, false, nil
			}
			if eh.Size > 4 {
				if e := p.skip(eh.Size); e != nil {
					return id, pos, false, e
				}
				continue
			}
			b, e := ebml.ReadBytes(p.r, eh.Size)
			if e != nil {
				return id, pos, false, e
			}
			id = idFromBytes(b)
		case mkv.IDSeekPosition:
			v, e := ebml.ReadUint(p.r, eh.Size)
			if e != nil {
				return id, pos, false, e
			}
			pos = int64(v)
		default:
			if eh.Size < 0 {
				return id, pos, false, nil
			}
			if e := p.skip(eh.Size); e != nil {
				return id, pos, false, e
			}
		}
	}
	return id, pos, id != 0 && pos >= 0, nil
}

// idFromBytes folds the SeekID bytes (the element ID exactly as it appears in the
// file, length marker included) into a uint32 for comparison with mkv.ID* values.
func idFromBytes(b []byte) uint32 {
	var id uint32
	for _, x := range b {
		id = id<<8 | uint32(x)
	}
	return id
}

// pos returns the current logical read offset.
func (p *parser) pos() int64 {
	cur, _ := p.r.Seek(0, io.SeekCurrent)
	return cur
}

// bufReadSeeker buffers an io.ReadSeeker for the metadata parse. The EBML reader
// pulls VINTs a byte at a time; without buffering each is a syscall (brutal on a
// network mount). A position query — Seek(0, SeekCurrent) — and small forward
// skips are served from the buffer; any other Seek resets it.
type bufReadSeeker struct {
	rs  io.ReadSeeker
	br  *bufio.Reader
	off int64 // logical offset of the next byte Read returns
}

func newBufReadSeeker(rs io.ReadSeeker, bufSize int) (*bufReadSeeker, error) {
	off, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	return &bufReadSeeker{rs: rs, br: bufio.NewReaderSize(rs, bufSize), off: off}, nil
}

func (b *bufReadSeeker) Read(p []byte) (int, error) {
	n, err := b.br.Read(p)
	b.off += int64(n)
	return n, err
}

func (b *bufReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		if offset == 0 {
			return b.off, nil // position query — keep the buffer
		}
		if offset > 0 && offset <= int64(b.br.Buffered()) {
			if _, err := b.br.Discard(int(offset)); err != nil {
				return b.off, err
			}
			b.off += offset
			return b.off, nil
		}
		return b.seekAbs(b.off + offset)
	case io.SeekStart:
		return b.seekAbs(offset)
	case io.SeekEnd:
		t, err := b.rs.Seek(offset, io.SeekEnd)
		if err != nil {
			return b.off, err
		}
		b.br.Reset(b.rs)
		b.off = t
		return t, nil
	}
	return b.off, fmt.Errorf("bufReadSeeker: invalid whence %d", whence)
}

// raw returns the underlying reader seeked to the buffer's current logical
// offset, so a caller can continue reading from exactly where the buffered view
// left off — without the buffer. Any bytes still buffered ahead are discarded;
// they will be re-read from the underlying reader on demand.
func (b *bufReadSeeker) raw() (io.ReadSeeker, error) {
	if _, err := b.rs.Seek(b.off, io.SeekStart); err != nil {
		return nil, err
	}
	return b.rs, nil
}

func (b *bufReadSeeker) seekAbs(target int64) (int64, error) {
	if _, err := b.rs.Seek(target, io.SeekStart); err != nil {
		return b.off, err
	}
	b.br.Reset(b.rs)
	b.off = target
	return target, nil
}
