package reader

// stream.go - seekless streaming read from a plain io.Reader.
//
// Design:
//   - NewStreamBlockReader wraps an io.Reader in a thin readOnlySeeker shim so
//     the existing countingReader machinery works without modification. The shim
//     handles Seek(0, SeekCurrent) (position query) but returns an error for
//     any real movement - which never occurs after BlockReader.init().
//   - ReadStream does a single forward pass: EBML header → Segment →
//     front-loaded metadata (Info, Tracks, Tags, Chapters, Attachments). Cues
//     and SeekHead are silently skipped because they are unusable on a
//     forward-only stream. The first Cluster element header is consumed; its
//     ID+size are handed to the resulting BlockReader so no byte is re-read.
//   - Unknown-size Clusters are supported by BlockReader.Next() via the peeked
//     field set from ReadStream, and by the unknown-size cluster logic already
//     handled in Next() (clusterEnd == -1 → read until next top-level element).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/internal/mpegts"
	"github.com/gravity-zero/mkvgo/mkv"
)

// readOnlySeeker wraps an io.Reader to satisfy io.ReadSeeker: Seek answers only a position query (0, SeekCurrent).
// A countingReader over one is built non-seekable and reads every skip through; the BlockReader's resync and damage
// probes seek its raw source, fail on it, and give up as on any source that cannot seek.
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

// decode runs a seekable element decoder over a size-byte element streamed through a bodyReader, so both readers
// read an element the same way, a child header running past its parent's end included, without holding its body.
// An element of unknown size has no body to decode: the scan reads on into it.
func (p *streamParser) decode(size int64, fn func(*parser) error) error {
	if size < 0 {
		return nil
	}
	lr := &io.LimitedReader{R: p.r, N: size}
	d := &parser{r: &bodyReader{r: lr}, posBase: p.pos, metaBudget: p.metaBudget, ctx: p.ctx, segEnd: -1}
	err := fn(d)
	p.metaBudget = d.metaBudget
	if _, cerr := io.Copy(io.Discard, lr); cerr != nil {
		return cerr
	}
	if lr.N > 0 {
		return io.ErrUnexpectedEOF // the stream ended inside the element, as a whole read of it would report
	}
	p.pos += size
	return err
}

// bodyLookback is how far a bodyReader can seek back: past the longest element header (4-byte ID, 8-byte size),
// the most a decoder reads beyond a parent's end before returning to it.
const bodyLookback = 64

// bodyReader is an element body read forward from a stream, seeking ahead by reading and back over the last
// bodyLookback bytes; like a bytes.Reader, it seeks past the end and reads EOF there.
type bodyReader struct {
	r      io.Reader
	pos    int64  // where the decoder stands
	end    int64  // how much was pulled from r
	tail   []byte // the last bytes pulled, ending at end
	replay []byte // tail bytes to hand out again after a seek back, ending at end
}

func (b *bodyReader) Read(p []byte) (int, error) {
	if b.pos > b.end {
		return 0, io.EOF
	}
	if len(b.replay) > 0 {
		n := copy(p, b.replay)
		b.replay = b.replay[n:]
		b.pos += int64(n)
		return n, nil
	}
	n, err := b.r.Read(p)
	b.pos += int64(n)
	b.end += int64(n)
	if n >= bodyLookback {
		b.tail = append(b.tail[:0], p[n-bodyLookback:n]...)
	} else {
		b.tail = append(b.tail, p[:n]...)
		if len(b.tail) > bodyLookback {
			b.tail = b.tail[len(b.tail)-bodyLookback:]
		}
	}
	return n, err
}

func (b *bodyReader) Seek(offset int64, whence int) (int64, error) {
	target := offset
	if whence == io.SeekCurrent {
		target += b.pos
	} else if whence != io.SeekStart {
		return 0, fmt.Errorf("mkvgo/reader: element body cannot seek from its end")
	}
	switch {
	case target < 0:
		return 0, fmt.Errorf("mkvgo/reader: seek to %d before the element body", target)
	case target >= b.end:
		if target > b.pos {
			b.pos += int64(len(b.replay))
			b.replay = nil
			if _, err := io.CopyN(io.Discard, b, target-b.pos); err != nil && err != io.EOF {
				return 0, err
			}
		}
		b.replay, b.pos = nil, target
	default:
		back := b.end - target
		if back > int64(len(b.tail)) {
			return 0, fmt.Errorf("mkvgo/reader: stream cannot seek back %d bytes inside an element", b.pos-target)
		}
		b.replay, b.pos = b.tail[int64(len(b.tail))-back:], target
	}
	return target, nil
}

// streamParser is a forward-only parser: skips via io.CopyN, no Seek.
type streamParser struct {
	r          io.Reader
	ctx        context.Context
	pos        int64 // bytes consumed from r (tracks sub-element boundaries)
	metaBudget int64 // remaining bytes allowed for in-memory metadata (parity with the seekable parser)
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

// ReadStream reads the EBML header and front-loaded segment metadata from a
// plain io.Reader (no Seek required), then returns a *BlockReader ready to
// yield blocks. In a well-formed streamable MKV (e.g. produced by StreamWriter
// or piped muxer output), Info and Tracks precede clusters.
//
// SeekHead and Cues are silently skipped (they cannot be acted on in a
// forward-only stream). The returned Container has no Cues field populated.
func ReadStream(ctx context.Context, r io.Reader) (*mkv.Container, *BlockReader, error) {
	// Buffered so the head scan can look at an element header before
	// consuming it: resyncing past junk (see resyncStream) needs the bytes a
	// failed decode would otherwise have swallowed. No Seek is involved.
	br0 := bufio.NewReaderSize(r, streamResyncWindow)
	r = br0
	p := &streamParser{r: r, ctx: ctx, metaBudget: maxMetadataBytes}
	if head, _ := br0.Peek(mpegts.PrefixLen); mpegts.Prefix(head) {
		return nil, nil, mkv.ErrMPEGTS
	}

	// EBML header.
	h, _, err := p.readHeader()
	if err != nil {
		return nil, nil, fmt.Errorf("ebml header: %w", err)
	}
	if h.ID != ebml.IDEBMLHeader {
		return nil, nil, fmt.Errorf("ebml header: expected 0x%X got 0x%X", ebml.IDEBMLHeader, h.ID)
	}
	c := &mkv.Container{}
	c.Info.TimecodeScale = 1_000_000 // EBML default
	if h.Size < 0 {
		return nil, nil, fmt.Errorf("ebml header body: cannot skip element with unknown size")
	}
	// The header body carries the DocType declaration (webm vs matroska); read
	// it in passing instead of skipping it.
	if err := parseEBMLHeaderBody(p.r, h.Size, c); err != nil {
		return nil, nil, fmt.Errorf("ebml header body: %w", err)
	}
	p.pos += h.Size

	// Segment.
	h, _, err = p.readHeader()
	if err != nil {
		return nil, nil, fmt.Errorf("segment: %w", err)
	}
	if h.ID != mkv.IDSegment {
		return nil, nil, fmt.Errorf("segment: expected 0x%X got 0x%X", mkv.IDSegment, h.ID)
	}
	// h.Size may be -1 (unknown-size segment) - fine for streaming.
	segEnd := int64(-1)
	if h.Size >= 0 {
		segEnd = p.pos + h.Size
	}

	// Scan segment-level elements until we hit the first Cluster, or the Segment's end: what lies past it is no
	// part of the Segment, as the seekable readers hold.
	var peekedCluster *ebml.ElementHeader
	seen := map[uint32]bool{} // the spec allows one Info and one Tracks
metaLoop:
	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if segEnd >= 0 && p.pos >= segEnd {
			break metaLoop
		}
		if derr := peekHeaderErr(br0); derr != nil {
			// Junk ahead of (or between) the head elements: scan forward to
			// the next segment-level element, as the seekable readers do. A
			// stream with nothing valid behind the junk keeps the decode error.
			junkStart := p.pos
			found, rerr := p.resyncStream(ctx, br0, segEnd)
			if rerr != nil {
				return nil, nil, rerr
			}
			if !found {
				return nil, nil, derr
			}
			c.ResyncedBytes += p.pos - junkStart
			continue
		}
		elemStart := p.pos
		eh, _, err := p.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break metaLoop
			}
			return nil, nil, err
		}
		switch eh.ID {
		case mkv.IDInfo, mkv.IDTracks:
			if seen[eh.ID] { // a duplicate: the first wins, as in Read
				if err := p.skip(eh.Size); err != nil {
					return nil, nil, err
				}
				break
			}
			seen[eh.ID] = eh.Size >= 0 // one of unknown size holds nothing decoded: the next counts, as in Read
			decode := (*parser).parseInfo
			if eh.ID == mkv.IDTracks {
				decode = (*parser).parseTracks
			}
			if err := p.decode(eh.Size, func(d *parser) error { return decode(d, eh.Size, c) }); err != nil {
				return nil, nil, elemErrAt(eh.ID, elemStart, err)
			}
		case mkv.IDChapters:
			if err := p.decode(eh.Size, func(d *parser) error { return d.parseChapters(eh.Size, c) }); err != nil {
				return nil, nil, elemErrAt(eh.ID, elemStart, err)
			}
		case mkv.IDAttachments:
			if err := p.decode(eh.Size, func(d *parser) error { return d.parseAttachments(eh.Size, c) }); err != nil {
				return nil, nil, elemErrAt(eh.ID, elemStart, err)
			}
		case mkv.IDTags:
			if err := p.decode(eh.Size, func(d *parser) error { return d.parseTags(eh.Size, c) }); err != nil {
				return nil, nil, elemErrAt(eh.ID, elemStart, err)
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
			// unknown size: the scan reads on into its body, whose children are passed over below like any
			// element that is not segment-level, up to the next one that is.
		default:
			if eh.Size >= 0 {
				if err := p.skip(eh.Size); err != nil {
					return nil, nil, err
				}
			}
			// unknown size: read on into its body, as for SeekHead and Cues above.
		}
	}

	if err := setDurationMs(c); err != nil {
		return nil, nil, err
	}
	// What Read derives from the head alone: a bitrate and a frame rate from the tags, a frame rate from the codec.
	promoteTrackBitrate(c)
	statsFrameRates(c)
	codecFrameRates(c)

	// Build BlockReader directly (bypass init which would re-read the EBML
	// header). The reader is already positioned right after the metadata.
	br := &BlockReader{
		timecodeScale: c.Info.TimecodeScale,
		segEnd:        segEnd, // -1 for an unknown-size segment, which ends on EOF
		clusterEnd:    -1,
	}
	// The Tracks were read above, not walked over by this reader: it learns them as a sequential walk would.
	br.SetTrackDefaultDurations(TrackDefaultDurations(c.Tracks))
	br.SetKnownTracks(c.Tracks)
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

// streamResyncWindow is the look-ahead ReadStream keeps over its input: the
// stride of the forward scan for a segment-level element.
const streamResyncWindow = 4 << 10

// peekHeaderErr decodes the element header at the head of br without
// consuming it and returns the decode error, if any. A header cut short by
// the end of the stream is not reported here: the consuming read that follows
// surfaces it as it always did.
func peekHeaderErr(br *bufio.Reader) error {
	b, _ := br.Peek(12) // an element header is at most 4 ID + 8 size bytes
	_, _, err := ebml.ReadElementHeader(bytes.NewReader(b))
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

// resyncStream is the forward-only counterpart of ResyncToSegmentElement: it
// discards bytes until br sits on a plausible segment-level element (see
// segmentElementPrefix) that fits the Segment ending at segEnd (-1: unknown)
// and reports whether one was found. The undecodable byte the caller stopped
// on is skipped first.
func (p *streamParser) resyncStream(ctx context.Context, br *bufio.Reader, segEnd int64) (bool, error) {
	skip := 1
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, _ := br.Discard(skip)
		p.pos += int64(n)
		if segEnd >= 0 && p.pos >= segEnd {
			return false, nil
		}
		b, _ := br.Peek(streamResyncWindow)
		if len(b) < 4 {
			return false, nil
		}
		switch i := indexSegmentLevelID(b); {
		case i < 0:
			skip = len(b) - 3 // keep the bytes an ID could straddle
		case i > 0:
			skip = i // bring the candidate to the head of the window
		default:
			// The element must fit the Segment, as the seekable resync holds (resyncToSegmentElement).
			if h, hn, ok := segmentElementPrefix(b); ok && (h.Size < 0 || segEnd < 0 || p.pos+int64(hn)+h.Size <= segEnd) {
				return true, nil
			}
			skip = 1
		}
	}
}

// --- metadata sub-parsers (all forward-only) ---
