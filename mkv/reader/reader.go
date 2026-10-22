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
	"github.com/gravity-zero/mkvgo/internal/mpegts"
	"github.com/gravity-zero/mkvgo/internal/shown"
	"github.com/gravity-zero/mkvgo/mkv"
)

func Open(ctx context.Context, path string, opts ...ReadOption) (*mkv.Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", shown.Path(path), err)
	}
	defer f.Close()
	return Read(ctx, f, path, opts...)
}

func OpenWithFS(ctx context.Context, path string, fs *mkv.FS, opts ...ReadOption) (*mkv.Container, error) {
	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", shown.Path(path), err)
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
	p := &parser{r: br, metaBudget: maxMetadataBytes, ctx: ctx, lazyAttachments: o.lazyAttachments, path: path, rateReads: o.frameRateReads, lookedPast: o.lookedPast}
	c := &mkv.Container{Path: path}

	if err := p.parseEBMLHeader(c); err != nil {
		return nil, notMatroska(r, path, err)
	}
	if err := p.parseSegment(ctx, c); err != nil && !tolerableTailError(err, c) {
		return nil, fmt.Errorf("segment: %w", err)
	}
	// The Cues are final here: turn their raw CueTime units into the
	// milliseconds CuePoint.TimeMs promises, once, before anything reads them.
	scaleCueTimesToMs(c)
	if err := setDurationMs(c); err != nil {
		return nil, err
	}
	// Derive the keyframe index from the Cues a full Read already parsed, so
	// Container.Keyframes is available from Read as well as the metadata path.
	c.Keyframes = keyframeTimesMs(c)
	// Surface the per-track "BPS" bitrate tag (mainstream muxers write it; probers report
	// it as bit_rate) as the typed Track.Bitrate, now that Tags are parsed.
	promoteTrackBitrate(c)
	statsFrameRates(c)
	codecFrameRates(c)
	if o.inBandChannels {
		fillStreamChannels(ctx, r, c)
	}
	if o.inBandColour {
		// Same opt-in as the metadata path: one bounded read of the first sample,
		// only for a track whose description the head left incomplete.
		fillColourFromFirstSample(ctx, r, c)
	}
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
// notMatroska is the error for a source whose EBML header did not parse: ErrNotMatroska for an MP4, the MPEG-TS
// refusal for a transport stream, else the header's own error.
func notMatroska(r io.ReadSeeker, path string, err error) error {
	switch {
	case looksLikeISOBMFF(r):
		return fmt.Errorf("%s: %w", shown.Path(path), ErrNotMatroska)
	case mpegts.At(r, -1): // not ErrNotMatroska, which sends a caller to the MP4 reader
		return fmt.Errorf("%s: %w", shown.Path(path), mkv.ErrMPEGTS)
	}
	return fmt.Errorf("ebml header: %w", err)
}

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
	held       io.ReadSeeker   // the source while a kept track child is parsed from its copy (keepUnmodeled)
	ctx        context.Context // cancellation, also honoured in the inner element loops
	// lazyAttachments records where an attachment payload lives instead of
	// loading it (reader.WithoutAttachmentData); path is the file it refers to.
	lazyAttachments bool
	path            string
	// metaRead is set by ReadMeta: once skippedPayload passes skipToExactReads, reads go through exact windows.
	metaRead       bool
	skippedPayload int64
	// posBase is the file offset the current reader's offset 0 corresponds to.
	// It is 0 for the file reader and non-zero only while a tail BUFFER stands
	// in for it, so a position recorded as an offset in the file stays one.
	posBase int64
	// firstCluster is the offset of the first Cluster the Segment walk met, 0 before.
	firstCluster int64
	// rateReads lets the frame-rate estimate read what the source does not hold (WithFrameRateReads).
	rateReads bool
	// lookedPast, when set, records that an element was judged by looking past its end (withLookedPast).
	lookedPast *bool
	// rateRejected says the TrackEntry being parsed stated a SamplingFrequency no file can mean.
	rateRejected bool
	// channelsRejected says it stated a channel count past 255.
	channelsRejected bool
	// privateCompression is how it compressed its CodecPrivate (ContentEncodingScope 2), privateStripped the header.
	privateCompression mkv.Compression
	privateStripped    []byte
	// gotInfo and gotTracks say the first Info and Tracks are read: the spec allows one of each, and the first wins,
	// met at the head or in the tail.
	gotInfo, gotTracks bool
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

// pastParent reports a child declaring more bytes than its parent holds (or an unknown size), and seats the parser on
// the parent's end: the rest of the parent is skipped, so the child does not swallow what follows it. Unless the
// parent is the one whose size is wrong: its end opens no segment-level element and the child's end does (a seekable
// read only: a streamed element cannot be looked past).
func (p *parser) pastParent(eh ebml.ElementHeader, end int64) bool {
	pos, err := p.r.Seek(0, io.SeekCurrent)
	if err != nil || (eh.Size >= 0 && pos+eh.Size <= end) {
		return false
	}
	if _, streamed := p.r.(*bodyReader); !streamed && eh.Size >= 0 && !p.segmentElementAt(end) && p.segmentElementAt(pos+eh.Size) {
		_, _ = p.r.Seek(pos, io.SeekStart)
		p.markLookedPast()
		return false
	}
	_, _ = p.r.Seek(end, io.SeekStart)
	return true
}

// junkAt reports a segment child that is no segment-level element, Void or CRC-32 and whose declared end leads to no
// segment-level element nor the Segment's end (framedTo; a Matroska ID out of place must open one at once): bytes read
// as a header where the framing was lost, to resync past rather than skip by the size they happen to declare. The
// read position is kept.
func (p *parser) junkAt(eh ebml.ElementHeader) bool {
	if eh.Size < 0 || isSegmentLevelID(eh.ID) || eh.ID == mkv.IDVoid || eh.ID == 0xBF {
		return false
	}
	pos := p.pos()
	hops := maxFramedHops
	if matroskaIDs[eh.ID] {
		hops = 0
	}
	framed := p.framedTo(pos+eh.Size, hops)
	_, _ = p.r.Seek(pos, io.SeekStart)
	if !framed {
		p.markLookedPast()
	}
	return !framed
}

// maxFramedHops bounds the well-formed elements framedTo follows before deciding.
const maxFramedHops = 8

// framedTo reports whether off is the Segment's end, the file's end under an unknown-size Segment, a segment-level
// element, or reaches one through at most hops well-formed elements a reader skips by their size (Void, CRC-32, an
// ID outside Matroska's). The read position is left anywhere.
func (p *parser) framedTo(off int64, hops int) bool {
	for ; hops >= 0; hops-- {
		if off == p.segEnd {
			return true
		}
		if p.segEnd >= 0 && off > p.segEnd {
			return false
		}
		if _, err := p.r.Seek(off, io.SeekStart); err != nil {
			return false
		}
		eh, n, err := ebml.ReadElementHeader(p.r)
		if err == io.EOF && p.segEnd < 0 {
			return true
		}
		if err != nil || eh.Size < 0 {
			return false
		}
		if isSegmentLevelID(eh.ID) {
			return true
		}
		if eh.ID != mkv.IDVoid && eh.ID != 0xBF && matroskaIDs[eh.ID] {
			return false
		}
		off += int64(n) + eh.Size
	}
	return false
}

// markLookedPast records a judgement made by looking past an element's end (withLookedPast).
func (p *parser) markLookedPast() {
	if p.lookedPast != nil {
		*p.lookedPast = true
	}
}

// segmentElementAt reports whether a segment-level element ID starts at off; the read position is left anywhere.
func (p *parser) segmentElementAt(off int64) bool {
	if _, err := p.r.Seek(off, io.SeekStart); err != nil {
		return false
	}
	id, _, err := ebml.ReadElementID(p.r)
	return err == nil && isSegmentLevelID(id)
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

// parseEBMLHeader reads the EBML header and records its DocType declaration
// on c (see parseEBMLHeaderBody); the header body is a few dozen bytes, read
// in the same pass as the rest.
func (p *parser) parseEBMLHeader(c *mkv.Container) error {
	h, _, err := p.readHeader()
	if err != nil {
		return err
	}
	if h.ID != ebml.IDEBMLHeader {
		return fmt.Errorf("expected EBML header (0x%X), got 0x%X", ebml.IDEBMLHeader, h.ID)
	}
	if h.Size < 0 {
		return fmt.Errorf("cannot skip element with unknown size")
	}
	return parseEBMLHeaderBody(p.r, h.Size, c)
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
	p.segStart, p.segEnd = segStart, endPos
	c.SegmentEnd = max(endPos, 0)

	var seekOffsets []int64 // absolute offsets of the elements a head SeekHead points at
	jumpedToTail := false
	triedTailScan := false
	backedUp := false              // went back once for elements a tail SeekHead indexes before itself
	var walkedFrom, walkedTo int64 // the tail already walked when going back, not walked again
	// The spec allows exactly one Info and one Tracks. Some non-conformant muxers
	// write more; take the FIRST of each and skip the rest, so a duplicate Tracks
	// does not append a second set of tracks (and so Read matches ReadMeta).

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		elemStart := p.pos()
		if endPos >= 0 && elemStart >= endPos {
			break
		}
		if backedUp && elemStart >= walkedFrom && elemStart < walkedTo {
			if _, err := p.r.Seek(walkedTo, io.SeekStart); err != nil {
				return err
			}
			continue
		}
		eh, _, err := p.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// A corrupted or padded region (seen in real files: a multi-MB run
			// of 0x00 between clusters, or junk between the Segment header and
			// Info) makes the next element header undecodable. Rather than
			// abort the whole read like a strict parser, resync to the next
			// segment-level element and keep going, as mainstream tools do -
			// any of them, not only a Cluster, or junk ahead of the metadata
			// would cost the Info and Tracks behind it. If nothing recognizable
			// remains, stop with the metadata gathered so far.
			off, rerr := p.resyncToSegmentElement(elemStart+1, -1, endPos)
			if rerr != nil {
				return rerr
			}
			if off < 0 {
				c.ResyncedBytes += max(p.junkEnd(-1, endPos)-elemStart, 0) // passed over all the same
				break
			}
			c.ResyncedBytes += off - elemStart
			continue
		}
		if eh.Size < 0 && eh.ID != mkv.IDCluster {
			// A live muxer's Cues or Tags declare no size: walked to the element that ends them, where the walk resumes.
			if err := p.skipUnknownSizeElement(endPos); err != nil {
				return err
			}
			continue
		}
		switch eh.ID {
		case mkv.IDInfo:
			if p.gotInfo {
				if err := p.skip(eh.Size); err != nil {
					return err
				}
				break // duplicate Info: first wins
			}
			if err := p.parseInfo(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			p.gotInfo = true
		case mkv.IDTracks:
			if p.gotTracks {
				if err := p.skip(eh.Size); err != nil {
					return err
				}
				break // duplicate Tracks: first wins (avoid appending a second set)
			}
			if err := p.parseTracks(eh.Size, c); err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			p.gotTracks = true
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
			offs, meta, err := p.parseSeekHeadRefs(eh.Size, segStart, endPos)
			if err != nil {
				return p.elemErr(eh.ID, elemStart, err)
			}
			seekOffsets = append(seekOffsets, offs...)
			// A SeekHead reached by the jump past the clusters may index elements
			// written before it (Cues, Tags, then this SeekHead): go back for them once, never for a Cluster.
			if back, ok := minOffsetAtOrAfter(meta, p.firstCluster+1); jumpedToTail && !backedUp && ok && back < walkedFrom {
				valid, err := p.offsetIsSegmentElement(back)
				if err != nil {
					return err
				}
				if valid {
					backedUp, walkedTo = true, elemStart
					if _, err := p.r.Seek(back, io.SeekStart); err != nil {
						return err
					}
					continue
				}
			}
		case mkv.IDCluster:
			if p.firstCluster == 0 {
				p.firstCluster = elemStart
				p.estimateFrameRates(c, p.pos(), clusterEnd(p.pos(), eh.Size))
			}
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
						jumpedToTail, walkedFrom = true, tailStart
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
				// A live/streamed Cluster declares no size: find its end by
				// walking its children instead of refusing the file.
				if err := p.skipUnknownSizeCluster(endPos); err != nil {
					return err
				}
				break
			}
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		default:
			if p.junkAt(eh) { // resynced past as an undecodable header is
				off, err := p.resyncToSegmentElement(elemStart+1, -1, endPos)
				if err != nil {
					return err
				}
				if off < 0 {
					c.ResyncedBytes += max(p.junkEnd(-1, endPos)-elemStart, 0) // passed over all the same
					return nil
				}
				c.ResyncedBytes += off - elemStart
				continue
			}
			if err := p.skip(eh.Size); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseSeekHeadRefs reads a SeekHead and returns the absolute file offset of
// every element it references (Info, Tracks, Cues, Tags, a nested SeekHead, …),
// reusing parseSeekEntry. SeekPosition is relative to the Segment data start
// (segStart). Entries that fall outside [segStart, endPos) are dropped.
// meta holds apart the offsets of the Cues, Tags, Chapters and Attachments, the elements a read goes back for.
func (p *parser) parseSeekHeadRefs(size, segStart, endPos int64) (offs, meta []int64, err error) {
	end := p.pos() + size
	for p.pos() < end {
		eh, _, e := p.readHeader()
		if e != nil {
			return offs, meta, e
		}
		if p.pastParent(eh, end) {
			return offs, meta, nil
		}
		if eh.ID != mkv.IDSeek {
			if eh.Size < 0 {
				return offs, meta, nil
			}
			if e := p.skip(eh.Size); e != nil {
				return offs, meta, e
			}
			continue
		}
		id, pos, ok, e := p.parseSeekEntry(eh.Size)
		if e != nil {
			return offs, meta, e
		}
		if !ok {
			continue
		}
		abs := segStart + pos
		if abs < segStart || (endPos >= 0 && abs >= endPos) {
			continue // out-of-range or overflowed (huge SeekPosition) entry
		}
		offs = append(offs, abs)
		switch id {
		case mkv.IDCues, mkv.IDTags, mkv.IDChapters, mkv.IDAttachments:
			meta = append(meta, abs)
		}
	}
	return offs, meta, nil
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
		if p.pastParent(eh, end) {
			break
		}
		switch eh.ID {
		case mkv.IDTimecodeScale:
			v, err := readUintOf(p.r, eh)
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
			v, err := readFloatOf(p.r, eh)
			if err != nil {
				return err
			}
			c.Info.Duration = v
		case mkv.IDMuxingApp:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return err
			}
			c.Info.MuxingApp = v
		case mkv.IDWritingApp:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return err
			}
			c.Info.WritingApp = v
		case mkv.IDTitle:
			if err := p.chargeMeta(eh.Size); err != nil {
				return err
			}
			v, err := readStringOf(p.r, eh)
			if err != nil {
				return err
			}
			c.Info.Title = v
		case mkv.IDDateUTC:
			v, err := readUintOf(p.r, eh)
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
