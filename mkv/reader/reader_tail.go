package reader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"slices"
	"sync/atomic"

	"github.com/gravity-zero/mkvgo/mkv"
)

// cuesMagic is the 4-byte Cues element ID (0x1C53BB6B), scanned for from the end
// of a SeekHead-less file to locate the Cues index without walking the clusters.
var cuesMagic = []byte{0x1C, 0x53, 0xBB, 0x6B}

// tailScanWindow is the most read back from the real file end when hunting
// for a trailing Cues index; tailScanFirst is the first read. The read doubles
// from tailScanFirst while the window ends inside metadata that starts before
// it, so a file ending on a cluster costs tailScanFirst and a typical
// Cues+Tags (~100-300 KiB) is read whole. A Cues index that starts further
// than tailScanWindow from EOF is left to the walk.
const (
	tailScanFirst  = 64 << 10
	tailScanWindow = 512 << 10
)

// scanTailForCues locates a SeekHead-less file's trailing Cues by reading a
// window at the real end of the file and scanning it for a run of tail
// elements, holding a Cues, that tiles exactly to EOF. On a hit it parses the
// whole run (Cues plus the Tags/Chapters/… around it) straight from that
// buffer and returns true; the caller is then done. On a miss it returns
// false and the caller walks the clusters. Using the real EOF (not the declared
// segment size) keeps it safe on truncated files: a short read or a tail that
// does not tile simply misses and defers to the resync-tolerant walk.
func (p *parser) scanTailForCues(c *mkv.Container) (bool, error) {
	floor := p.pos() // current cluster body - never scan before here
	end, err := p.r.Seek(0, io.SeekEnd)
	if err != nil {
		return false, err
	}
	for win := int64(tailScanFirst); ; win *= 2 {
		start := max(floor, end-min(win, tailScanWindow))
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
		rel, cues, ok := tailRun(buf, start, end)
		whole := (!ok || rel > 0) && !metaEndsAt(buf, rel)
		if ok && cues && (whole || start == floor || win >= tailScanWindow) {
			if err := p.parseTailBuffer(c, buf[rel:], start+int64(rel)); err != nil {
				return false, err
			}
			return true, nil
		}
		if whole || start == floor || win >= tailScanWindow {
			return false, nil
		}
	}
}

// tailRun returns where in buf (covering [bufStart, end)) the earliest run of
// segment-level elements other than clusters tiling exactly to end starts,
// whether it holds a Cues element, and whether there is one; without one, at
// is len(buf). One backward pass decides every start, so the cost is linear.
func tailRun(buf []byte, bufStart, end int64) (at int, cues, ok bool) {
	marks := tileMarks(buf, end-bufStart, isTailMeta)
	if marks == nil {
		return len(buf), false, false
	}
	for i, b := range buf {
		if b != 0x1C && b != 0x12 && b != 0x10 && b != 0x19 && b != 0xEC && b != 0x11 && b != 0x15 && b != 0x16 { // first byte of a tail element ID
			continue
		}
		if i < len(marks) && marks[i]&markTiles != 0 {
			return i, marks[i]&markCues != 0, true
		}
	}
	return len(buf), false, false
}

const (
	markTiles = 1 << iota // the elements from here tile exactly to the end
	markCues              // and one of them is a Cues
)

// tileMarks marks each offset i in [0, n] of buf from which elements accept
// takes, of definite size, tile exactly to n, computed from the end so each
// offset is decoded once; nil when n is outside buf.
func tileMarks(buf []byte, n int64, accept func(uint32) bool) []uint8 {
	if n < 0 || n > int64(len(buf)) {
		return nil
	}
	marks := make([]uint8, n+1)
	marks[n] = markTiles
	for i := n - 1; i >= 0; i-- {
		id, size, hlen, ok := elemAt(buf[i:n])
		if !ok || size < 0 || !accept(id) || size > n-i-int64(hlen) {
			continue
		}
		next := marks[i+int64(hlen)+size]
		if next&markTiles == 0 {
			continue
		}
		marks[i] = markTiles | next&markCues
		if id == mkv.IDCues {
			marks[i] |= markCues
		}
	}
	return marks
}

// elemAtCount, set by a test, counts every elemAt decode in the process: no test of this package may run t.Parallel.
var elemAtCount atomic.Pointer[atomic.Int64]

// elemAt decodes the element header at b[0] straight from the slice, with
// ebml.ReadElementHeader's rules: a 1-4 octet ID, a size whose all-ones value is unknown (-1).
func elemAt(b []byte) (id uint32, size int64, hlen int, ok bool) {
	if c := elemAtCount.Load(); c != nil {
		c.Add(1)
	}
	idv, n1, ok := vintAt(b)
	if !ok || n1 > 4 {
		return 0, 0, 0, false
	}
	sv, n2, ok := vintAt(b[n1:])
	if !ok {
		return 0, 0, 0, false
	}
	mask := uint64(1) << uint(n2*7)
	size = int64(sv &^ mask)
	if sv == mask|(mask-1) {
		size = -1
	}
	return uint32(idv), size, n1 + n2, true
}

// vintAt decodes the EBML VINT at b[0], marker bit kept, and its width.
func vintAt(b []byte) (v uint64, n int, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if len(b) < n {
		return 0, 0, false
	}
	for _, c := range b[:n] {
		v = v<<8 | uint64(c)
	}
	return v, n, true
}

// isTailMeta accepts the elements a muxer writes after the clusters: any segment-level element but a Cluster, or Void.
func isTailMeta(id uint32) bool { return id != mkv.IDCluster && isTailElement(id) }

// metaEndsAt reports whether buf[:x] ends with the last children of a tail
// metadata element that started before buf: a run of CuePoints, Tags,
// EditionEntries, ChapterAtoms or AttachedFiles, or of their children, tiling
// exactly to x. Media payload ends no such run, so a window ending on a
// cluster stops the tail scan. One backward pass decides every start; the
// children checked inside CuePoints and ChapterAtoms draw on a budget in
// proportion to x, past which the window is taken as not ending on metadata.
func metaEndsAt(buf []byte, x int) bool {
	if x <= 0 || x > len(buf) {
		return false
	}
	budget := 8*x + 4096
	tiles := make([]bool, x+1)
	tiles[x] = true
	found := false
	for i := x - 1; i >= 0; i-- {
		id, size, hlen, ok := elemAt(buf[i:x])
		if !ok || size < 0 || size > int64(x-i-hlen) || !tiles[i+hlen+int(size)] {
			continue
		}
		tiles[i] = innerElement(id, buf[i+hlen:i+hlen+int(size)], &budget)
		if budget < 0 {
			return false
		}
		found = found || tiles[i] && innerFirstByte[buf[i]]
	}
	return found
}

// innerFirstByte marks the first bytes of the IDs innerElement accepts.
var innerFirstByte = func() (t [256]bool) {
	for _, b := range []byte{0x73, 0x67, 0x63, 0x45, 0x44, 0x68, 0x6E, 0x61, 0x46, 0xBB, 0xB6} {
		t[b] = true
	}
	return t
}()

// innerElement accepts a child of a tail metadata element: a multi-byte ID of
// one, or a CuePoint/ChapterAtom whose own children check out (their one-byte
// IDs alone are too common in media payload). Each child decoded costs budget one.
func innerElement(id uint32, body []byte, budget *int) bool {
	switch id {
	case mkv.IDTag, mkv.IDSimpleTag, mkv.IDTargets, mkv.IDTagName, mkv.IDTagString, mkv.IDTagLanguage,
		mkv.IDTagBinary, mkv.IDTargetTypeValue, mkv.IDTagTrackUID, mkv.IDEditionEntry, mkv.IDEditionUID,
		mkv.IDEditionFlagHidden, mkv.IDEditionFlagDefault, mkv.IDEditionFlagOrdered, mkv.IDChapterUID,
		mkv.IDChapterSegmentUID, mkv.IDAttachedFile, mkv.IDFileName, mkv.IDFileMimeType, mkv.IDFileData,
		mkv.IDFileUID, mkv.IDFileDescription:
		return true
	case mkv.IDCuePoint:
		return childrenAre(body, budget, mkv.IDCueTime, mkv.IDCueTime, mkv.IDCueTrackPositions)
	case mkv.IDChapterAtom:
		return childrenAre(body, budget, mkv.IDChapterUID, mkv.IDChapterUID, mkv.IDChapterTimeStart, mkv.IDChapterTimeEnd,
			mkv.IDChapterDisplay, mkv.IDChapterFlagHidden, mkv.IDChapterSegmentUID, mkv.IDChapterAtom)
	}
	return false
}

// childrenAre reports whether body tiles exactly into elements whose IDs are all in ids, one of them must.
func childrenAre(body []byte, budget *int, must uint32, ids ...uint32) bool {
	seen := false
	for off := 0; off < len(body); {
		*budget--
		id, size, hlen, ok := elemAt(body[off:])
		if !ok || size < 0 || size > int64(len(body)-off-hlen) || !slices.Contains(ids, id) {
			return false
		}
		seen = seen || id == must
		off += hlen + int(size)
	}
	return seen
}

// tailAnchor scans buf (covering [bufStart, end)) for the earliest Cues element
// ID whose elements tile exactly to end, returning its index within buf. A
// cuesMagic byte sequence occurring inside cluster data fails the tiling check,
// so it is not mistaken for the real index.
func tailAnchor(buf []byte, bufStart, end int64) (int, bool) {
	marks := tileMarks(buf, end-bufStart, isTailElement)
	for i := 0; marks != nil; {
		j := bytes.Index(buf[i:], cuesMagic)
		if j < 0 {
			break
		}
		at := i + j
		if at < len(marks) && marks[at]&markTiles != 0 {
			return at, true
		}
		i = at + 1
	}
	return 0, false
}

// tilesToEnd reports whether the elements starting at buf[0] (file offset abs)
// are all segment-level (or Void padding) with definite sizes and tile exactly
// to end, leaving no gap and never overshooting.
func tilesToEnd(buf []byte, abs, end int64) bool {
	for off := 0; abs < end; {
		id, size, hlen, ok := elemAt(buf[off:])
		if !ok || size < 0 || !isTailElement(id) {
			return false
		}
		step := int64(hlen) + size
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
		if p.pastParent(eh, end) {
			return nil
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
			if p.gotInfo {
				err = p.skip(eh.Size) // the first wins, as in the Segment walk
				break
			}
			err, p.gotInfo = p.parseInfo(eh.Size, c), true
		case mkv.IDTracks:
			if p.gotTracks {
				err = p.skip(eh.Size)
				break
			}
			err, p.gotTracks = p.parseTracks(eh.Size, c), true
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
