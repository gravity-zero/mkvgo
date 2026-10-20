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
	off, err := scanValid(r, from, scanLimit, indexSegmentLevelID, func(b []byte, off int64) bool {
		h, hn, ok := segmentElementPrefix(b)
		return ok && (h.Size < 0 || off+int64(hn)+h.Size <= fitLimit)
	})
	if err != nil || off < 0 {
		return -1, err
	}
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return -1, err
	}
	return off, nil
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

// junkEnd is where a resync that found nothing stopped looking: the source's end, bounded by the Segment's
// (segEnd) and by the scan's (scanLimit), -1 for either bound when there is none.
func (p *parser) junkEnd(scanLimit, segEnd int64) int64 {
	end, err := p.r.Seek(0, io.SeekEnd)
	if err != nil {
		return -1
	}
	if segEnd >= 0 {
		end = min(end, segEnd)
	}
	if scanLimit >= 0 {
		end = min(end, scanLimit)
	}
	return end
}

// skipUnknownSizeElement walks the children of an unknown-size element that is
// no Cluster (a live muxer's Cues, with its CuePoints of unknown size too),
// header by header: the element ends at the first segment-level ID - no child
// of any such element - or at the Segment's end or EOF (RFC 8794 section 6.2);
// a child that declares no size is walked the same way, its own children ending
// it where the parent ends. The parser is left AT the element that ended it; a
// header that will not decode stops the walk there, for the Segment walk to
// resync as it would anywhere else.
func (p *parser) skipUnknownSizeElement(endPos int64) error {
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
		if err != nil || isSegmentLevelID(h.ID) {
			break
		}
		stop += int64(n)
		if h.Size < 0 {
			continue // an unknown-size child: its children are walked in turn
		}
		if _, err := br.Seek(h.Size, io.SeekCurrent); err != nil {
			return err
		}
		stop += h.Size
	}
	_, err = p.r.Seek(stop, io.SeekStart)
	return err
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
	return resyncToCluster(r, limit, limit)
}

// resyncToCluster is ResyncToCluster looking for the Cluster's start before scanLimit and requiring the Cluster to fit
// before fitLimit: a walk bounding how far it looks past damage (not where the media may end) scans less than it allows.
func resyncToCluster(r io.ReadSeeker, scanLimit, fitLimit int64) (int64, error) {
	from, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return -1, err
	}
	off, err := scanValid(r, from, scanLimit, func(b []byte) int { return bytes.Index(b, clusterMagic) },
		func(b []byte, off int64) bool { return clusterPrefix(b, off, fitLimit) })
	if err != nil || off < 0 {
		return -1, err
	}
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return -1, err
	}
	return off, nil
}

// scanValid returns the offset of the first candidate index finds at or after
// from, starting before limit (-1 = until EOF), that judge accepts on the
// segmentElementProbe bytes there (fewer at EOF), or -1. Candidates are judged
// in the window already read, and the bytes a candidate near its end needs
// are carried into the next one, so the source is read once whatever the
// number of false candidates. A read error is returned once the bytes before
// it hold no accepted candidate.
func scanValid(r io.ReadSeeker, from, limit int64, index func([]byte) int, judge func(b []byte, off int64) bool) (int64, error) {
	if _, err := r.Seek(from, io.SeekStart); err != nil {
		return -1, err
	}
	const window = 64 << 10
	const probe = segmentElementProbe
	buf := make([]byte, probe+window)
	tail := 0
	next := from // absolute offset of the next byte to read
	for {
		base := next - int64(tail) // absolute offset of buf[0]
		n, rerr := r.Read(buf[tail : tail+window])
		end := tail + n
		next += int64(n)
		final := rerr != nil || n == 0
		searchEnd := end
		if limit >= 0 && limit-base < int64(end) {
			searchEnd = int(max(limit-base, 0))
		}
		judgeEnd := searchEnd // candidates before it are judged in this window
		if !final {
			judgeEnd = min(judgeEnd, max(end-probe+1, 0))
		}
		for i := 0; i < judgeEnd; {
			j := index(buf[i:searchEnd])
			if j < 0 || i+j >= judgeEnd {
				break
			}
			at := i + j
			if judge(buf[at:min(at+probe, end)], base+int64(at)) {
				return base + int64(at), nil
			}
			i = at + 1
		}
		if final {
			if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
				return -1, rerr
			}
			return -1, nil
		}
		if limit >= 0 && next >= limit && judgeEnd >= searchEnd {
			return -1, nil
		}
		tail = copy(buf, buf[judgeEnd:end])
	}
}

// clusterPrefix reports whether b, the bytes at off, open a real Cluster: its
// declared size, when known, fits within limit and its first child is a
// recognizable cluster-level element.
func clusterPrefix(b []byte, off, limit int64) bool {
	id, size, hlen, ok := elemAt(b)
	if !ok || id != mkv.IDCluster {
		return false
	}
	if size >= 0 && limit >= 0 && off+int64(hlen)+size > limit {
		return false
	}
	child, _, _, ok := elemAt(b[hlen:])
	return ok && clusterChildIDs[child]
}
