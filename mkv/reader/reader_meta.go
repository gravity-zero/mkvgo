package reader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

// metaBufSize is the read buffer for the metadata-only path. ~2 KiB covers the
// EBML header + SeekHead + Info + Tracks of a typically-muxed file in a single
// underlying Read, so the byte-at-a-time EBML VINT reads cost one syscall instead
// of hundreds - the difference that matters on a network-mounted library.
const metaBufSize = 2 << 10

// headResyncCap bounds how far the head scan looks past an undecodable region
// for the next segment-level element.
const headResyncCap = 1 << 20

// OpenMeta opens path and returns its Info, Tracks and Keyframes via the fast
// metadata-only path (see ReadMeta); Cues, Chapters, Attachments and Tags only
// when a ReadOption asks for them. Use Open for a full read.
func OpenMeta(ctx context.Context, path string, opts ...ReadOption) (*mkv.Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", shown.Path(path), err)
	}
	defer f.Close()
	return ReadMeta(ctx, f, path, opts...)
}

// OpenMetaWithFS is OpenMeta against a caller-provided FS.
func OpenMetaWithFS(ctx context.Context, path string, fs *mkv.FS, opts ...ReadOption) (*mkv.Container, error) {
	f, err := fs.DoOpenContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", shown.Path(path), err)
	}
	defer f.Close()
	return ReadMeta(ctx, f, path, opts...)
}

// ReadMeta reads ONLY the metadata an indexer needs - Info and Tracks - and stops
// as soon as it has both, never parsing Cues and never traversing Clusters. On a
// typically-muxed file that is the first ~1-2 KB, so ReadMeta is orders of
// magnitude faster and cheaper than a full Read.
//
// It returns Tracks, Info and DurationMs byte-identical to a full Read. A file
// with duplicate Info or Tracks elements reads the same too: every reader keeps
// the first of each.
//
// The returned Container has Tracks and Info populated; Chapters, Attachments,
// Tags and Cues are left nil - call Read/Open for those.
//
// If a SeekHead is present at the Segment head it is used to seek straight to
// Info and Tracks, so files whose Tracks element sits after the Clusters are
// still handled without scanning the body. Reads are buffered.
//
// Junk preceding the metadata (a padded or damaged run after the Segment
// header) is resynced past as in Read, but only within headResyncCap bytes:
// beyond that ReadMeta returns what it managed to parse (possibly empty), and
// callers that must tolerate a larger damaged head should fall back to Read.
func ReadMeta(ctx context.Context, r io.ReadSeeker, path string, opts ...ReadOption) (*mkv.Container, error) {
	var o readOpts
	for _, fn := range opts {
		fn(&o)
	}

	br, err := newBufReadSeeker(r, metaBufSize)
	if err != nil {
		return nil, err
	}
	p := &parser{r: br, metaBudget: maxMetadataBytes, ctx: ctx, lazyAttachments: o.lazyAttachments, path: path, metaRead: true, rateReads: o.frameRateReads, lookedPast: o.lookedPast}
	c := &mkv.Container{Path: path}

	if err := p.parseEBMLHeader(c); err != nil {
		return nil, notMatroska(r, path, err)
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
	if o.inBandChannels {
		fillStreamChannels(ctx, r, c)
	}
	if o.inBandColour {
		// Opted-in, and only for tracks that need it: recover colour from the
		// first sample's in-band SPS. The head parse above stays untouched.
		fillColourFromFirstSample(ctx, r, c)
	}
	return c, nil
}

// parseSegmentMeta walks the Segment's top-level children only far enough to
// parse Info and Tracks - reusing the same parseInfo/parseTracks as the full
// reader for correctness parity - then returns. A head SeekHead is recorded so it
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
	c.SegmentEnd = max(endPos, 0)

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
			if errors.Is(err, io.EOF) {
				break
			}
			// Junk ahead of (or between) the head elements - a padded or
			// damaged run right after the Segment header - hides the Info and
			// Tracks behind it. Look for the next segment-level element, as
			// Read does, but no further than headResyncCap: a head scan must
			// not turn into a read of the whole file on a wrecked one.
			off, rerr := p.resyncToSegmentElement(elemStart+1, elemStart+1+headResyncCap, endPos)
			if rerr != nil {
				return rerr
			}
			if off < 0 {
				c.ResyncedBytes += max(p.junkEnd(elemStart+1+headResyncCap, endPos)-elemStart, 0)
				break // nothing recognizable in the window: return what we have
			}
			c.ResyncedBytes += off - elemStart
			continue
		}
		if eh.Size < 0 && eh.ID != mkv.IDCluster {
			// A live muxer's Cues or Tags declare no size: walked to the element that ends them, where the walk resumes.
			if err := p.skipUnknownSizeElement(p.segEnd); err != nil {
				return err
			}
			continue
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
			// Tags inline before the Clusters: read them now when the caller wants
			// them (WithTags, or WithBitrate for the per-track BPS). Otherwise skip -
			// Tags are not exposed on this path. An element sitting HERE is reachable
			// no other way: the SeekHead may not index it, and the tail scan only
			// looks at the file end.
			if (o.tags || o.bitrate) && c.Tags == nil {
				if err := p.parseTags(eh.Size, c); err != nil {
					return p.elemErr(eh.ID, elemStart, err)
				}
			} else if err := p.skip(eh.Size); err != nil {
				return err
			}
		case mkv.IDChapters:
			if o.chapters && c.Chapters == nil {
				if err := p.parseChapters(eh.Size, c); err != nil {
					return p.elemErr(eh.ID, elemStart, err)
				}
			} else if err := p.skip(eh.Size); err != nil {
				return err
			}
		case mkv.IDAttachments:
			if o.attachments && c.Attachments == nil {
				if err := p.parseAttachments(eh.Size, c); err != nil {
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
			if p.firstCluster == 0 {
				p.firstCluster = elemStart
				if gotTracks && p.skippedPayload < skipToExactReads {
					p.estimateFrameRates(c, p.pos(), clusterEnd(p.pos(), eh.Size))
				}
			}
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
			if p.junkAt(eh) { // looked past as Read does, no further than headResyncCap
				off, err := p.resyncToSegmentElement(elemStart+1, elemStart+1+headResyncCap, endPos)
				if err != nil {
					return err
				}
				if off < 0 {
					c.ResyncedBytes += max(p.junkEnd(elemStart+1+headResyncCap, endPos)-elemStart, 0)
					return p.finalizeHeadMeta(c, offs, o) // nothing recognizable in the window: what we have
				}
				c.ResyncedBytes += off - elemStart
				continue
			}
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
		// Early stop once Info and Tracks are in - unless the caller asked for an
		// element that has not turned up yet. finalizeHeadMeta can still fetch it
		// through the SeekHead or from the tail, but neither reaches an element
		// sitting HERE, between the head and the first Cluster, that no SeekHead
		// indexes: stopping now would lose it. The scan ends at the first Cluster
		// either way, so this never walks the media.
		if gotInfo && gotTracks && !p.headWantsMore(c, o) {
			break
		}
	}
	return p.finalizeHeadMeta(c, offs, o)
}

// headWantsMore reports whether an element the caller asked for is still missing,
// so the head scan must keep going to the first Cluster rather than stop at
// Info+Tracks. Callers that asked for nothing extra (the plain metadata probe)
// stop early, unless a video track without DefaultDuration needs the first
// cluster's timestamps and the next header is already held.
func (p *parser) headWantsMore(c *mkv.Container, o readOpts) bool {
	return p.wantsFrameRate(c) || (o.cues && len(c.Cues) == 0) ||
		((o.tags || o.bitrate) && c.Tags == nil) ||
		(o.chapters && c.Chapters == nil) ||
		(o.attachments && c.Attachments == nil)
}

// finalizeHeadMeta derives Container.Keyframes from the Cues seek index, and - when
// o.bitrate - fills each track's Bitrate from the Tags' BPS. Either element, if
// not already read inline, is reached by following its SeekHead offset (one seek to
// one element, no Cluster scan). It then leaves Cues and Tags nil - the metadata
// path's contract - exposing only the derived Keyframes and Track.Bitrate; with
// o.cues the raw CuePoints are kept too (WithCues), for cue-driven seeking.
func (p *parser) finalizeHeadMeta(c *mkv.Container, offs headOffsets, o readOpts) error {
	wantsTail := o.cues || o.tags || o.bitrate || o.attachments || o.chapters
	if wantsTail {
		if err := p.followSeekHeads(c, &offs); err != nil {
			return err
		}
	}
	reads := []struct {
		off  int64
		id   uint32
		want bool
		fn   func(int64, *mkv.Container) error
	}{
		{offs.cues, mkv.IDCues, len(c.Cues) == 0, p.parseCues},
		{offs.tags, mkv.IDTags, (o.bitrate || o.tags) && c.Tags == nil, p.parseTags},
		{offs.attachments, mkv.IDAttachments, o.attachments && c.Attachments == nil, p.parseAttachments},
		{offs.chapters, mkv.IDChapters, o.chapters && c.Chapters == nil, p.parseChapters},
	}
	// In file order, so elements stored together come in one forward read.
	sort.Slice(reads, func(i, j int) bool { return reads[i].off < reads[j].off })
	indexed := offs.cues >= 0
	for _, r := range reads {
		if !r.want || r.off < 0 {
			continue
		}
		ok, err := p.parseElementAt(r.off, r.id, c, r.fn)
		if err != nil {
			return err
		}
		indexed = indexed && ok
	}
	// Whatever the SeekHead did not hand over, the tail can: everything the
	// metadata path serves past the head (Cues, Tags, Chapters, Attachments)
	// lives in the SAME contiguous region at the file end, and most muxers index
	// none of it. One bounded read recovers the lot.
	// A SeekHead that indexes the Cues, with no stale entry, is the file's index:
	// an element it does not name is not there, and the tail scan is for files without one.
	if !indexed {
		if err := p.tailElements(c, o); err != nil {
			return err
		}
	}
	// Same single conversion the full read does, at this path's own "the Cues
	// are final" point - the tail scan above is the last thing that can add any.
	scaleCueTimesToMs(c)
	c.Keyframes = keyframeTimesMs(c)
	if !o.cues {
		c.Cues = nil
	}
	if o.bitrate {
		promoteTrackBitrate(c)
	}
	statsFrameRates(c)
	codecFrameRates(c)
	if !o.tags {
		c.Tags = nil
	}
	return nil
}

// tailElements recovers the post-cluster elements no SeekHead points at: either
// the muxer wrote no SeekHead at all (the common case - most real files carry
// their Cues, Tags and the rest at the very end and index none of it), or its
// pointer was stale and got rejected above. It is the same bounded read back from
// EOF the full reader already does, so both read paths reach the same verdict on
// the same file; without it the metadata path reported a healthy trailing index
// as missing, and left Tags/Chapters/Attachments nil on files that carry them.
//
// It runs only when something the caller ASKED for is still missing, so a caller
// that wants none of the tail (a plain metadata probe) never pays the read, and
// one whose SeekHead already delivered does not pay it twice.
//
// The tail is parsed into a scratch Container because parseTailBuffer also
// handles the Info/Tracks a tail may carry, and parseTracks CONCATENATES: merging
// it into c would duplicate the tracks already parsed from the head. Only what
// the caller asked for is lifted across.
//
// Limit: the scan anchors on the Cues element (that is what tiles to EOF), so a
// file with tail Tags but NO Cues at all is not recovered here - a shape no real
// muxer produces, since whatever writes the tags writes the index.
func (p *parser) tailElements(c *mkv.Container, o readOpts) error {
	if o.noTailScan {
		return nil // WithoutTailScan: the head must resolve it or it does not exist
	}
	wantCues := o.cues && len(c.Cues) == 0
	wantTags := (o.bitrate || o.tags) && c.Tags == nil
	wantAttachments := o.attachments && c.Attachments == nil
	wantChapters := o.chapters && c.Chapters == nil
	if !wantCues && !wantTags && !wantAttachments && !wantChapters {
		return nil
	}

	var tail mkv.Container
	if _, err := p.scanTailForCues(&tail); err != nil {
		return err
	}
	if len(c.Cues) == 0 { // Cues read on the way fill the keyframe index, asked for or not
		c.Cues = tail.Cues
	}
	if wantTags {
		c.Tags = tail.Tags
	}
	if wantAttachments {
		c.Attachments = tail.Attachments
	}
	if wantChapters {
		c.Chapters = tail.Chapters
	}
	return nil
}

// followSeekHeads merges the entries of the SeekHeads the head one points at, as a muxer indexes its tail.
func (p *parser) followSeekHeads(c *mkv.Container, offs *headOffsets) error {
	seen := map[int64]bool{}
	for i := 0; i < len(offs.seekHeads) && i < 8; i++ {
		off := offs.seekHeads[i]
		if seen[off] {
			continue
		}
		seen[off] = true
		if _, err := p.parseElementAt(off, mkv.IDSeekHead, c, func(size int64, _ *mkv.Container) error {
			found, err := p.parseSeekHeadMeta(size, p.segStart, p.segEnd)
			offs.merge(found)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// parseElementAt seeks to off, verifies the element there has the expected ID,
// and parses it with fn. It returns (false, nil) - not an error - when the offset
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
		// (e.g. it points past EOF) means a stale entry, not a fatal read error  -
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
	seekHeads                                       []int64 // further SeekHeads this one points at
}

// merge takes every entry the other SeekHead states, over this one's.
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
	h.seekHeads = append(h.seekHeads, o.seekHeads...)
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
		if p.pastParent(eh, end) {
			return offs, nil
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
		case mkv.IDSeekHead:
			offs.seekHeads = append(offs.seekHeads, abs)
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
		if p.pastParent(eh, end) {
			break
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
// network mount). A position query - Seek(0, SeekCurrent) - and small forward
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
			return b.off, nil // position query - keep the buffer
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
// left off - without the buffer. Any bytes still buffered ahead are discarded;
// they will be re-read from the underlying reader on demand.
// readAtWindows switches the underlying reads to exact windows through io.ReaderAt (atWindows), when the source offers it.
func (b *bufReadSeeker) readAtWindows() {
	ra, ok := b.rs.(io.ReaderAt)
	if _, done := b.rs.(*atWindows); !ok || done {
		return
	}
	size, err := b.rs.Seek(0, io.SeekEnd)
	if err != nil {
		return
	}
	b.rs = &atWindows{ra: ra, size: size, pos: b.off}
	b.br.Reset(b.rs)
}

// atWindows reads an io.ReaderAt through one window of exact ReadAt calls:
// 4 KiB after a jump, doubled while the reads run on, up to 512 KiB, and a
// read larger than the window asks for just that read. Behind a skipped
// payload it fetches the headers that follow, not a remote reader's window.
type atWindows struct {
	ra             io.ReaderAt
	size, pos, off int64
	buf            []byte
	win            int
}

func (w *atWindows) Read(p []byte) (int, error) {
	if w.pos >= w.size {
		return 0, io.EOF
	}
	if w.pos < w.off || w.pos >= w.off+int64(len(w.buf)) {
		if w.win > 0 && w.pos == w.off+int64(len(w.buf)) {
			w.win = min(2*w.win, 512<<10)
		} else {
			w.win = 4 << 10
		}
		if len(p) >= w.win { // a large read goes straight through
			n, err := w.ra.ReadAt(p[:min(int64(len(p)), w.size-w.pos)], w.pos)
			w.pos += int64(n)
			if n > 0 {
				err = nil
			}
			return n, err
		}
		n := int(min(int64(w.win), w.size-w.pos))
		if cap(w.buf) < n {
			w.buf = make([]byte, n)
		}
		m, err := w.ra.ReadAt(w.buf[:n], w.pos)
		if m == 0 {
			w.buf = w.buf[:0]
			return 0, err
		}
		w.buf, w.off = w.buf[:m], w.pos
	}
	n := copy(p, w.buf[w.pos-w.off:])
	w.pos += int64(n)
	return n, nil
}

func (w *atWindows) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += w.pos
	case io.SeekEnd:
		offset += w.size
	}
	if offset < 0 {
		return 0, errors.New("atWindows: negative position")
	}
	w.pos = offset
	return offset, nil
}

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
