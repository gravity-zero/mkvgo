package reader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// segmentElementFirstChild lists, for each segment-level master other than
// Cluster, the element IDs that can legitimately open it (Void and CRC-32 are
// global and accepted everywhere). Requiring one rejects a segment-level ID
// byte sequence that occurs by chance inside junk or media payload, the same
// way clusterChildIDs guards the Cluster anchor.
var segmentElementFirstChild = map[uint32]map[uint32]bool{
	mkv.IDInfo: {
		mkv.IDTimecodeScale: true, mkv.IDDuration: true, mkv.IDDateUTC: true,
		mkv.IDTitle: true, mkv.IDMuxingApp: true, mkv.IDWritingApp: true,
		mkv.IDSegmentUID: true, mkv.IDPrevUID: true, mkv.IDNextUID: true,
		0x7384:   true, // SegmentFilename
		0x3C83AB: true, // PrevFilename
		0x3E83BB: true, // NextFilename
		0x4444:   true, // SegmentFamily
		0x6924:   true, // ChapterTranslate
	},
	mkv.IDTracks:      {mkv.IDTrackEntry: true},
	mkv.IDSeekHead:    {mkv.IDSeek: true},
	mkv.IDCues:        {mkv.IDCuePoint: true},
	mkv.IDTags:        {mkv.IDTag: true},
	mkv.IDChapters:    {mkv.IDEditionEntry: true},
	mkv.IDAttachments: {mkv.IDAttachedFile: true},
}

// ResyncToSegmentElement scans r forward from its current position for the
// next structurally valid segment-level element (Info, Tracks, SeekHead, Cues,
// Tags, Chapters, Attachments or Cluster), bounded by limit (an absolute
// offset, or -1 to scan until EOF). It is ResyncToCluster widened to every
// anchor a Segment can resume on: junk sitting BEFORE the metadata (a padded
// or damaged head) must not cost the Info and Tracks that follow it, which a
// Cluster-only resync skips over.
//
// A candidate is accepted only when its declared size fits within limit and
// its first child decodes as an element that master can open, so an ID byte
// sequence occurring by chance is skipped rather than trusted. On success r is
// left positioned at the element's ID and that offset is returned; -1 with a
// nil error means no valid element was found before limit (including a
// genuine EOF). Only a real I/O failure is returned as an error.
func ResyncToSegmentElement(r io.ReadSeeker, limit int64) (int64, error) {
	return resyncToSegmentElement(r, limit, limit)
}

// resyncToSegmentElement is ResyncToSegmentElement with the two bounds told
// apart: the scan stops at scanLimit, while a candidate's declared size is
// checked against fitLimit (the Segment end; -1 = the end of the input). A
// caller capping how far it is willing to LOOK must not thereby reject an
// element that starts inside the window and legitimately runs past it.
func resyncToSegmentElement(r io.ReadSeeker, scanLimit, fitLimit int64) (int64, error) {
	from, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	if fitLimit < 0 {
		if fitLimit, err = r.Seek(0, io.SeekEnd); err != nil {
			return -1, err
		}
	}
	if scanLimit < 0 || scanLimit > fitLimit {
		scanLimit = fitLimit
	}
	for {
		off, err := scanForMagic(r, from, scanLimit, indexSegmentLevelID)
		if err != nil || off < 0 {
			return -1, err
		}
		valid, err := isSegmentElementAt(r, off, fitLimit)
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

// indexSegmentLevelID returns the index of the first segment-level element ID
// in b, or -1. Every such ID is four bytes with a 0x1 class nibble, so one
// byte test rules out most positions before the full comparison.
func indexSegmentLevelID(b []byte) int {
	for i := 0; i+4 <= len(b); i++ {
		if b[i]&0xF0 == 0x10 && isSegmentLevelID(binary.BigEndian.Uint32(b[i:])) {
			return i
		}
	}
	return -1
}

// segmentElementProbe is how many bytes at a candidate offset are enough to
// judge it: the element header and its first child's header, 12 bytes each at
// most.
const segmentElementProbe = 32

// isSegmentElementAt reports whether a real segment-level element begins at
// off (see segmentElementPrefix) and, when its size is declared, fits within
// limit.
func isSegmentElementAt(r io.ReadSeeker, off, limit int64) (bool, error) {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return false, err
	}
	var buf [segmentElementProbe]byte
	n, err := io.ReadFull(r, buf[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	h, hn, ok := segmentElementPrefix(buf[:n])
	if !ok {
		return false, nil
	}
	return h.Size < 0 || off+int64(hn)+h.Size <= limit, nil
}

// segmentElementPrefix reports whether b - the first bytes at a candidate
// offset - opens a plausible segment-level element, returning its header and
// header length. A Cluster may declare no size and must open with a
// cluster-level child (as isClusterAt requires); any other master must declare
// a size and open with a child it can contain, that child fitting inside it.
func segmentElementPrefix(b []byte) (ebml.ElementHeader, int, bool) {
	r := bytes.NewReader(b)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil || !isSegmentLevelID(h.ID) {
		return h, n, false
	}
	child, cn, err := ebml.ReadElementHeader(r)
	if err != nil {
		return h, n, false
	}
	if h.ID == mkv.IDCluster {
		return h, n, clusterChildIDs[child.ID]
	}
	if h.Size <= 0 || child.Size < 0 || int64(cn)+child.Size > h.Size {
		return h, n, false
	}
	return h, n, child.ID == mkv.IDVoid || child.ID == 0xBF || segmentElementFirstChild[h.ID][child.ID]
}

// resyncToSegmentElement repositions the parser on the next valid
// segment-level element at or after from, looking no further than scanLimit
// (-1 = as far as segEnd). from is passed explicitly because the header read
// that failed may have swallowed the first bytes of a real element.
func (p *parser) resyncToSegmentElement(from, scanLimit, segEnd int64) (int64, error) {
	if _, err := p.r.Seek(from, io.SeekStart); err != nil {
		return -1, err
	}
	return resyncToSegmentElement(p.r, scanLimit, segEnd)
}

// skipUnknownSizeCluster walks the children of an unknown-size Cluster whose
// header was just read, and stops on the element that ends it: per the EBML
// rule for unknown-sized elements (RFC 8794 section 6.2), the first element
// that is not one of its children - here any segment-level element - or the
// end of the Segment or of the input. The parser is left AT that element, so
// the Segment walk resumes on it; a header that will not decode, or a child
// that itself declares no size, is handed back the same way for the walk to
// resync or refuse as it would anywhere else.
//
// The walk is buffered: a live recording has thousands of small blocks per
// cluster, and seeking over each one costs far more than reading through
// them.
func (p *parser) skipUnknownSizeCluster(endPos int64) error {
	br, err := newBufReadSeeker(p.r, fullReadBufSize)
	if err != nil {
		return err
	}
	stop := br.off
	for endPos < 0 || stop < endPos {
		if err := p.checkCtx(); err != nil {
			return err
		}
		h, n, err := ebml.ReadElementHeader(br)
		if err != nil || h.Size < 0 || isSegmentLevelID(h.ID) {
			break
		}
		if _, err := br.Seek(h.Size, io.SeekCurrent); err != nil {
			return err
		}
		stop += int64(n) + h.Size
	}
	_, err = p.r.Seek(stop, io.SeekStart)
	return err
}
