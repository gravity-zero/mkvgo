package ops

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// surgical.go - block-level recovery inside a damaged cluster. When a
// cluster's declared size is implausible or its body fails structural
// validation, the tolerant walk (Salvage, Reindex with Options.Resync) used
// to drop the whole declared extent. Most real-world damage is far smaller
// than that: a corrupted size field with an intact payload behind it, or a
// few KB of overwritten bytes inside a multi-MB cluster. The surgical scan
// re-derives the truth from the bytes themselves: it walks cluster children
// from the body start ignoring the declared size, and on a break scans
// forward for the next chain-validated block, splitting the cluster around
// the damage instead of discarding it. Every child run it keeps is emitted
// as a cluster carrying the original cluster's Timestamp (a continuation
// run's relative timecodes stay valid against the same base), so recovery
// never guesses timing: if the cluster's own Timestamp cannot be read, the
// region is not block-recovered at all.

// surgicalChildIDs are element IDs accepted as cluster children during a
// surgical chain walk (mirrors the reader's cluster-child set).
var surgicalChildIDs = map[uint32]bool{
	uint32(mkv.IDTimestamp):   true, // 0xE7
	uint32(mkv.IDSimpleBlock): true, // 0xA3
	uint32(mkv.IDBlockGroup):  true, // 0xA0
	uint32(mkv.IDVoid):        true, // 0xEC
	0xA7:                      true, // Position
	0xAB:                      true, // PrevSize
	0xBF:                      true, // CRC-32
	0x5854:                    true, // SilentTracks
}

// surgicalTopLevelIDs is the closed set of Matroska top-level element IDs a
// chain walk accepts as the anchor ending a surgical episode.
var surgicalTopLevelIDs = map[uint32]bool{
	uint32(mkv.IDSeekHead):    true,
	uint32(mkv.IDInfo):        true,
	uint32(mkv.IDTracks):      true,
	uint32(mkv.IDChapters):    true,
	uint32(mkv.IDCluster):     true,
	uint32(mkv.IDCues):        true,
	uint32(mkv.IDAttachments): true,
	uint32(mkv.IDTags):        true,
}

// surgicalRunCap splits an unbroken child run into multiple emitted clusters
// so a single run never has to be buffered beyond this size.
const surgicalRunCap = 64 << 20 // 64 MiB

// surgicalMaxBlockSize is the largest block a chain walk accepts - the block
// reader's own limit. A candidate declaring more is not a block, and is
// refused before a byte of it is read.
const surgicalMaxBlockSize = 64 << 20

// surgicalContinuityToleranceNs is how far a continuation run's first block
// may step BACK in time relative to the last block before the gap and still
// be accepted as the same cluster's continuation. Video blocks are stored in
// decode order with presentation timestamps, so a legitimate continuation can
// open with a B-frame slightly in the past; a run that restarts near zero
// (a different cluster's blocks whose header died in the gap) steps back by
// a whole cluster stride and is rejected rather than mis-timed.
const surgicalContinuityToleranceNs = 1_000_000_000 // 1s

type surgicalStop int

const (
	surgicalStopBreak  surgicalStop = iota // structural failure at run.end
	surgicalStopAnchor                     // a validated top-level element begins at run.end
	surgicalStopEOF                        // clean end of file at run.end
	surgicalStopSplit                      // run reached surgicalRunCap; continue at run.end
)

// surgicalRun is one contiguous span of structurally valid cluster children.
type surgicalRun struct {
	start, end int64
	// firstRelTC/lastRelTC track the block timecodes seen in the run (relative
	// to the cluster Timestamp), for the cross-gap continuity gate.
	firstRelTC, lastRelTC int64
	hasBlocks             bool
	children              int // child elements walked, for the solidity gate
	// firstByTrack/lastByTrack are each track's first and last block timecode
	// in the run: a track's timecodes do not go back within a cluster, so a
	// run where one does - against what the cluster held before the gap - is
	// another cluster's.
	firstByTrack, lastByTrack map[uint64]int64
	// lastStart is where the run's last child starts, lastIsBlock whether it
	// is a block: the one a hole may have begun inside (trimZeroedTail).
	lastStart   int64
	lastIsBlock bool
}

// note records one block of track at relTC.
func (r *surgicalRun) note(track uint64, relTC int64) {
	if !r.hasBlocks {
		r.firstRelTC, r.hasBlocks = relTC, true
		r.firstByTrack, r.lastByTrack = map[uint64]int64{}, map[uint64]int64{}
	}
	r.lastRelTC = relTC
	if _, seen := r.firstByTrack[track]; !seen {
		r.firstByTrack[track] = relTC
	}
	r.lastByTrack[track] = relTC
}

// surgicalOutcome maps one damaged cluster region.
type surgicalOutcome struct {
	runs    []surgicalRun
	gaps    []mkv.DamagedRange // offsets filled; approx times filled by the caller
	end     int64              // absolute offset where the main walk resumes
	atEOF   bool               // the episode ran to end of file
	tsUnits int64              // the cluster's own Timestamp value (timecode units)
	tsValid bool
}

// blockGroupChildIDs are the elements a BlockGroup can hold beside its Block.
// All of them are small: a "child" of another ID, or of an implausible size,
// means the bytes are not a BlockGroup.
var blockGroupChildIDs = map[uint32]bool{
	uint32(mkv.IDBlockDuration):  true, // 0x9B
	uint32(mkv.IDReferenceBlock): true, // 0xFB
	uint32(mkv.IDVoid):           true, // 0xEC
	0xBF:                         true, // CRC-32
	0xFA:                         true, // ReferencePriority
	0xA4:                         true, // CodecState
	0x8E:                         true, // Slices
	0x75A1:                       true, // BlockAdditions
	0x75A2:                       true, // DiscardPadding
}

// surgicalMaxSideChildSize bounds a BlockGroup child that is not the Block
// (BlockAdditions is the largest in practice, a few KB of dynamic metadata).
const surgicalMaxSideChildSize = 1 << 20

// chainReadThrough is the longest hop a chain walk reads through rather than
// seeks over; chainCandidateBuf is the buffer of a walk started on a candidate
// (most die on their first header: a large buffer would be filled for nothing).
const (
	chainReadThrough  = 64 << 10
	chainCandidateBuf = 4 << 10
	// chainWalkBuf is the buffer of an ordinary chain walk. Small on purpose:
	// the walk only reads headers, and whatever the buffer holds past the end
	// of a cluster is read again by the copy that follows.
	chainWalkBuf = 16 << 10
)

// chainWalker reads element headers through a buffer and steps over what
// follows them WITHOUT reading it: a chain walk judges structure, never
// payload. That is what keeps a candidate test cheap inside a damaged region,
// where every byte that looks like a block start gets tested and a "block"
// declares whatever size its bytes spell - read through instead of stepped
// over, the search for one resume point read a real file forty times.
type chainWalker struct {
	raw io.ReadSeeker
	r   *bufio.Reader
	pos int64 // absolute offset of the next byte r returns
}

// header reads one element header.
func (c *chainWalker) header() (ebml.ElementHeader, bool) {
	h, n, err := ebml.ReadElementHeader(c.r)
	c.pos += int64(n)
	return h, err == nil
}

// to steps to the absolute offset target (never backwards).
func (c *chainWalker) to(target int64) bool {
	d := target - c.pos
	switch {
	case d < 0:
		return false
	case d <= chainReadThrough:
		// A short hop: reading through it keeps the walk sequential. Seeking
		// over every ordinary block would refill the buffer each time and
		// read more than the bytes it steps over.
		if _, err := c.r.Discard(int(d)); err != nil {
			return false
		}
	default:
		if _, err := c.raw.Seek(target, io.SeekStart); err != nil {
			return false
		}
		c.r.Reset(c.raw)
	}
	c.pos = target
	return true
}

// blockHead reads the head of a (Simple)Block ending at end - track number and
// relative timecode - checks the track against tracks (when non-empty), and
// steps over the rest.
func (c *chainWalker) blockHead(end int64, tracks map[uint64]bool) (track uint64, relTC int16, ok bool) {
	trackRaw, n, err := ebml.ReadDataSize(c.r)
	c.pos += int64(n)
	if err != nil || trackRaw < 0 || (len(tracks) > 0 && !tracks[uint64(trackRaw)]) {
		return 0, 0, false
	}
	var head [3]byte // relative timecode (2) + flags (1)
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, 0, false
	}
	c.pos += 3
	if c.pos > end {
		return 0, 0, false
	}
	return uint64(trackRaw), int16(binary.BigEndian.Uint16(head[:2])), c.to(end)
}

// blockGroup walks the children of a BlockGroup ending at end: exactly the
// elements a BlockGroup holds, each inside it, one of them a Block.
func (c *chainWalker) blockGroup(end int64, tracks map[uint64]bool) (track uint64, relTC int16, ok bool) {
	found := false
	for c.pos < end {
		h, hok := c.header()
		childEnd := c.pos + h.Size
		if !hok || h.Size < 0 || childEnd > end {
			return 0, 0, false
		}
		switch {
		case h.ID == mkv.IDBlock:
			if track, relTC, ok = c.blockHead(childEnd, tracks); !ok {
				return 0, 0, false
			}
			found = true
		case blockGroupChildIDs[uint32(h.ID)] && h.Size <= surgicalMaxSideChildSize:
			if !c.to(childEnd) {
				return 0, 0, false
			}
		default:
			return 0, 0, false
		}
	}
	return track, relTC, found && c.pos == end
}

// chainWalkChildren walks cluster children from pos, accepting only known
// child IDs with plausible, in-bounds sizes and, for blocks, a track number
// from tracks (when non-empty). It reads headers only (see chainWalker). It
// stops at the first validated top-level element, EOF, a structural break, or
// the run cap.
func chainWalkChildren(raw io.ReadSeeker, pos, fileSize int64, tracks map[uint64]bool) (surgicalRun, surgicalStop, error) {
	return chainWalkChildrenBuf(raw, pos, fileSize, tracks, chainWalkBuf)
}

// chainWalkChildrenBuf is chainWalkChildren with the read buffer size chosen
// by the caller.
func chainWalkChildrenBuf(raw io.ReadSeeker, pos, fileSize int64, tracks map[uint64]bool, bufSize int) (surgicalRun, surgicalStop, error) {
	run := surgicalRun{start: pos, end: pos, firstRelTC: -1 << 62, lastRelTC: -1 << 62}
	if _, err := raw.Seek(pos, io.SeekStart); err != nil {
		return run, surgicalStopBreak, err
	}
	c := &chainWalker{raw: raw, r: bufio.NewReaderSize(raw, bufSize), pos: pos}
	for {
		if run.end >= fileSize {
			return run, surgicalStopEOF, nil
		}
		if run.end-run.start >= surgicalRunCap {
			return run, surgicalStopSplit, nil
		}
		h, ok := c.header()
		if !ok {
			if run.end >= fileSize {
				return run, surgicalStopEOF, nil
			}
			return run, surgicalStopBreak, nil
		}
		if surgicalTopLevelIDs[uint32(h.ID)] {
			if validTopLevelAt(raw, run.end, fileSize) {
				return run, surgicalStopAnchor, nil
			}
			return run, surgicalStopBreak, nil
		}
		end := c.pos + h.Size
		if !surgicalChildIDs[uint32(h.ID)] || h.Size < 0 || end > fileSize {
			return run, surgicalStopBreak, nil
		}
		isBlock := h.ID == mkv.IDSimpleBlock || h.ID == mkv.IDBlockGroup
		switch {
		case h.Size > surgicalMaxBlockSize:
			return run, surgicalStopBreak, nil // no reader accepts an element that large here
		case (h.ID == mkv.IDTimestamp || h.ID == 0xA7 || h.ID == 0xAB || h.ID == 0xBF) && h.Size > 8:
			return run, surgicalStopBreak, nil // Timestamp, Position, PrevSize, CRC-32: a few bytes each
		}
		var track uint64
		var relTC int16
		switch h.ID {
		case mkv.IDSimpleBlock:
			track, relTC, ok = c.blockHead(end, tracks)
		case mkv.IDBlockGroup:
			track, relTC, ok = c.blockGroup(end, tracks)
		default:
			ok = c.to(end)
		}
		if !ok {
			return run, surgicalStopBreak, nil
		}
		if isBlock {
			run.note(track, int64(relTC))
		}
		run.lastStart, run.lastIsBlock = run.end, isBlock
		run.end = end
		run.children++
	}
}

// validTopLevelAt checks that a top-level element header at off is plausible:
// decodable ID+size within the file, and for a Cluster a recognizable first
// child, so a chance byte pattern inside corruption is not trusted as an
// anchor. It leaves raw at an unspecified position.
func validTopLevelAt(raw io.ReadSeeker, off, fileSize int64) bool {
	if _, err := raw.Seek(off, io.SeekStart); err != nil {
		return false
	}
	r := bufio.NewReaderSize(raw, 4096)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil || !surgicalTopLevelIDs[uint32(h.ID)] {
		return false
	}
	if h.Size >= 0 && off+int64(n)+h.Size > fileSize {
		return false
	}
	if h.ID != mkv.IDCluster {
		return h.Size >= 0
	}
	ch, cn, err := ebml.ReadElementHeader(r)
	if err != nil || !surgicalChildIDs[uint32(ch.ID)] {
		return false
	}
	return ch.Size >= 0 && off+int64(n)+int64(cn)+ch.Size <= fileSize
}

// surgicalCandidate scans forward from `from` (bounded by cap bytes) for the
// earliest of: a validated top-level anchor, or a block candidate (SimpleBlock
// or BlockGroup) whose child chain from that point validates and whose first
// block passes the relTC continuity gate. kind is surgicalStopAnchor or
// surgicalStopBreak (block resume); off < 0 means nothing found before the cap
// or EOF.
//
// gapStart and rate (bytes per timecode unit of the cluster so far, 0 when
// unknown) add a second gate: the time a block resume claims to have skipped
// must be in keeping with the bytes it skipped. Blocks found 16 MiB past the
// break whose timecodes pick up where the cluster left off are not its
// continuation - they belong to a later cluster whose header was lost in the
// gap, and attached to this one they would be timed against the wrong base
// (measured on a real download with missing pieces: audio stepping back half
// a second after every hole).
//
// seen is each track's last timecode in the cluster before the gap and video
// the video tracks: a third gate. Within a cluster a track's timecodes do not
// go back, bar the few frames a video reorders (toleranceTC). A run where an
// audio or subtitle track restarts earlier than it left off is another
// cluster's, however short the gap - a lost Cluster header is 16 bytes.
func surgicalCandidate(raw io.ReadSeeker, from, fileSize int64, tracks map[uint64]bool, prevLastRelTC int64, toleranceTC int64, gapStart int64, rate float64, seen map[uint64]int64, video map[uint64]bool) (kind surgicalStop, off int64, err error) {
	limit := from + salvageResyncCap
	if limit > fileSize {
		limit = fileSize
	}
	const window = 64 << 10
	buf := make([]byte, window)
	skipUntil := int64(0) // candidates before it belong to a run already refused
	for base := from; base < limit; base += window {
		if base+window <= skipUntil {
			continue
		}
		if _, serr := raw.Seek(base, io.SeekStart); serr != nil {
			return 0, -1, serr
		}
		n, rerr := io.ReadFull(raw, buf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return 0, -1, rerr
		}
		end := int64(n)
		if base+end > limit {
			end = limit - base
		}
		for i := int64(0); i < end; i++ {
			b := buf[i]
			cand := base + i
			if cand < skipUntil {
				continue
			}
			switch {
			case b == 0x1F && i+4 <= end && bytes.Equal(buf[i:i+4], []byte{0x1F, 0x43, 0xB6, 0x75}):
				if validTopLevelAt(raw, cand, fileSize) {
					return surgicalStopAnchor, cand, nil
				}
			case b == 0xA3 || b == 0xA0:
				// Most candidates are payload bytes that happen to equal a
				// block ID: judge them on the bytes already in memory, and
				// read the file only for the few that hold up.
				if plausible, decided := blockStartPlausible(buf[i:n], fileSize-cand, tracks); decided && !plausible {
					continue
				}
				run, stop, perr := chainWalkChildrenBuf(raw, cand, fileSize, tracks, chainCandidateBuf)
				if perr != nil {
					return 0, -1, perr
				}
				solid := run.hasBlocks && (run.children >= 3 ||
					stop == surgicalStopAnchor || stop == surgicalStopEOF)
				if !solid {
					continue
				}
				// A solid run the time gates refuse is refused whole: its
				// later blocks belong to the same chain and would each be
				// walked to the same verdict.
				if prevLastRelTC > (-1<<62) && run.firstRelTC < prevLastRelTC-toleranceTC {
					skipUntil = run.end
					continue // restarts too far back in time: not this cluster's blocks
				}
				if prevLastRelTC > (-1<<62) && rate > 0 && toleranceTC > 0 {
					expected := float64(cand-gapStart) / rate // the time a gap this long holds
					if expected > float64(toleranceTC) && float64(run.firstRelTC-prevLastRelTC) < expected/4 {
						skipUntil = run.end
						continue // far too little time for the bytes skipped: a later cluster's blocks
					}
				}
				if stepsBack(run.firstByTrack, seen, video, toleranceTC) {
					skipUntil = run.end
					continue // a track restarts before where it left off: a later cluster's blocks
				}
				return surgicalStopBreak, cand, nil
			}
		}
	}
	return 0, -1, nil
}

// zeroedTailMin is how many zero bytes must close a block, with zeros
// following it, for the block to count as cut by a zeroed hole.
const zeroedTailMin = 16

// trimZeroedTail drops the last block of a run that broke on a zeroed region
// when that block's own payload ends in zeros: the hole (the pieces a download
// never received) began INSIDE it. Its header is intact, so the chain accepted
// it, but its tail is the hole's - kept, it is a frame a decoder chokes on at
// the leading edge of every hole. An intact block that merely ends on padding
// right before a hole is dropped with it: one frame, against a corrupt one.
func trimZeroedTail(raw io.ReadSeeker, run surgicalRun, fileSize int64) (surgicalRun, error) {
	if !run.lastIsBlock || run.end <= run.lastStart || run.end+zeroedTailMin > fileSize {
		return run, nil
	}
	n := min(run.end-run.lastStart, zeroedTailMin)
	buf := make([]byte, n+zeroedTailMin)
	if _, err := raw.Seek(run.end-n, io.SeekStart); err != nil {
		return run, err
	}
	if _, err := io.ReadFull(raw, buf); err != nil {
		return run, nil // cannot tell: keep the block
	}
	for _, b := range buf {
		if b != 0 {
			return run, nil
		}
	}
	if n < zeroedTailMin {
		return run, nil // a block shorter than the test: no verdict
	}
	run.end = run.lastStart
	run.children--
	return run, nil
}

// endsInZeros reports whether b closes on zeroedTailMin zero bytes.
func endsInZeros(b []byte) bool {
	if len(b) < zeroedTailMin {
		return false
	}
	for _, x := range b[len(b)-zeroedTailMin:] {
		if x != 0 {
			return false
		}
	}
	return true
}

// stepsBack reports whether any track of a candidate run starts before the
// timecode that track had reached (seen). A video track may step back by tol,
// the reach of frame reordering; any other track, not at all.
func stepsBack(first, seen map[uint64]int64, video map[uint64]bool, tol int64) bool {
	for track, tc := range first {
		last, ok := seen[track]
		if !ok {
			continue
		}
		if video[track] {
			last -= tol
		}
		if tc < last {
			return true
		}
	}
	return false
}

// blockStartPlausible judges, from the bytes b at a candidate offset alone,
// whether a SimpleBlock or BlockGroup can start there: a size no block has, a
// size running past the file (room is what is left of it), a track the file
// does not declare, a BlockGroup opening on something a BlockGroup cannot hold
// - all are refused without touching the file. decided is false when b is too
// short to tell; the caller then walks the chain as before.
func blockStartPlausible(b []byte, room int64, tracks map[uint64]bool) (plausible, decided bool) {
	r := bytes.NewReader(b)
	h, n, err := ebml.ReadElementHeader(r)
	if err != nil {
		return false, len(b) >= 12 // a header is at most 12 bytes: with them all in hand it is simply invalid
	}
	if h.Size <= 0 || h.Size > surgicalMaxBlockSize || int64(n)+h.Size > room {
		return false, true
	}
	declared := func() (bool, bool) {
		track, _, err := ebml.ReadDataSize(r)
		if err != nil {
			return false, r.Len() >= 8
		}
		return track > 0 && (len(tracks) == 0 || tracks[uint64(track)]), true
	}
	if h.ID == mkv.IDSimpleBlock {
		return declared()
	}
	child, _, err := ebml.ReadElementHeader(r)
	if err != nil {
		return false, r.Len() >= 12
	}
	switch {
	case child.Size < 0 || child.Size > h.Size:
		return false, true
	case child.ID == mkv.IDBlock:
		return declared()
	}
	return blockGroupChildIDs[uint32(child.ID)] && child.Size <= surgicalMaxSideChildSize, true
}

// surgicalScanCluster maps the region of one damaged cluster starting at
// bodyStart (right after the cluster's ID+size header): valid child runs,
// the gaps between them, and the top-level anchor where the main walk should
// resume. It requires the cluster's own Timestamp to open the first run -
// without it the continuation runs could not be timed and the caller must
// fall back to dropping the region.
func surgicalScanCluster(raw io.ReadSeeker, bodyStart, fileSize int64, tracks, video map[uint64]bool, toleranceTC int64) (*surgicalOutcome, error) {
	out := &surgicalOutcome{end: bodyStart}

	// The first run must open with the Timestamp child (possibly preceded by
	// CRC-32 or Void): read it upfront.
	tsUnits, ok := readClusterTimestampAt(raw, bodyStart, fileSize)
	if !ok {
		return nil, fmt.Errorf("cluster timestamp unreadable at %d", bodyStart)
	}
	out.tsUnits, out.tsValid = tsUnits, true

	pos := bodyStart
	prevLast := int64(-1 << 62)
	// firstTC and kept measure the cluster so far - its first block's relative
	// timecode and the bytes of its valid runs - for the byte rate the resume
	// gate checks a gap against.
	firstTC, haveFirst, kept := int64(0), false, int64(0)
	seen := map[uint64]int64{} // each track's last timecode in the cluster so far
	for {
		run, stop, err := chainWalkChildren(raw, pos, fileSize, tracks)
		if err != nil {
			return nil, err
		}
		if stop == surgicalStopBreak {
			if run, err = trimZeroedTail(raw, run, fileSize); err != nil {
				return nil, err
			}
		}
		if run.end > run.start {
			out.runs = append(out.runs, run)
			kept += run.end - run.start
			if run.hasBlocks {
				prevLast = run.lastRelTC
				if !haveFirst {
					firstTC, haveFirst = run.firstRelTC, true
				}
				for track, tc := range run.lastByTrack {
					seen[track] = tc
				}
			}
		}
		switch stop {
		case surgicalStopAnchor:
			out.end = run.end
			return out, nil
		case surgicalStopEOF:
			out.end = run.end
			out.atEOF = true
			return out, nil
		case surgicalStopSplit:
			pos = run.end
			continue
		}
		// Structural break: hunt for the next anchor or chain-valid blocks. A
		// zeroed region at the break (the pieces a download never received)
		// is walked to its end first: nothing starts in it, and the scan cap
		// bounds only the search behind it.
		huntFrom, err := zeroRunEnd(raw, run.end, fileSize)
		if err != nil {
			return nil, err
		}
		rate := 0.0
		if haveFirst && prevLast > firstTC {
			rate = float64(kept) / float64(prevLast-firstTC)
		}
		kind, cand, err := surgicalCandidate(raw, max(huntFrom, run.end+1), fileSize, tracks, prevLast, toleranceTC, run.end, rate, seen, video)
		if err != nil {
			return nil, err
		}
		if cand < 0 {
			if huntFrom+salvageResyncCap >= fileSize {
				out.gaps = append(out.gaps, mkv.DamagedRange{StartOffset: run.end, EndOffset: fileSize})
				out.end = fileSize
				out.atEOF = true
				return out, nil
			}
			return nil, fmt.Errorf("no valid cluster or block chain found within %d-byte scan from offset %d", salvageResyncCap, run.end)
		}
		out.gaps = append(out.gaps, mkv.DamagedRange{StartOffset: run.end, EndOffset: cand})
		if kind == surgicalStopAnchor {
			out.end = cand
			return out, nil
		}
		pos = cand
	}
}

// readClusterTimestampAt reads the Timestamp child of the cluster body
// starting at off (allowing CRC-32/Void to precede it, as real muxers write)
// and returns its value in timecode units. Blocks before the Timestamp mean
// the timing base is unknowable: not ok.
func readClusterTimestampAt(raw io.ReadSeeker, off, fileSize int64) (units int64, ok bool) {
	if _, err := raw.Seek(off, io.SeekStart); err != nil {
		return 0, false
	}
	r := bufio.NewReaderSize(raw, 4096)
	pos := off
	for i := 0; i < 4; i++ {
		h, n, err := ebml.ReadElementHeader(r)
		if err != nil || h.Size < 0 || pos+int64(n)+h.Size > fileSize {
			return 0, false
		}
		if h.ID == mkv.IDTimestamp {
			if h.Size > 8 {
				return 0, false
			}
			v, err := ebml.ReadUint(r, h.Size)
			if err != nil {
				return 0, false
			}
			return int64(v), true
		}
		if h.ID != mkv.IDVoid && uint32(h.ID) != 0xBF { // only CRC-32/Void may precede
			return 0, false
		}
		if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
			return 0, false
		}
		pos += int64(n) + h.Size
	}
	return 0, false
}

// synthesizeTimestamp encodes a Timestamp child element for a continuation
// cluster, carrying the original cluster's timecode.
func synthesizeTimestamp(units int64) []byte {
	var buf bytes.Buffer
	val := encodeUintBE(uint64(units))
	ebml.WriteElementID(&buf, mkv.IDTimestamp) //nolint:errcheck
	ebml.WriteDataSize(&buf, int64(len(val)))  //nolint:errcheck
	buf.Write(val)
	return buf.Bytes()
}

// encodeUintBE returns the minimal big-endian encoding of v (at least 1 byte).
func encodeUintBE(v uint64) []byte {
	n := 1
	for x := v >> 8; x != 0; x >>= 8 {
		n++
	}
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	return out
}

// anyRunHasBlocks reports whether at least one run carries media blocks -
// the bar for a surgical recovery to be worth emitting at all.
func anyRunHasBlocks(runs []surgicalRun) bool {
	for _, r := range runs {
		if r.hasBlocks {
			return true
		}
	}
	return false
}

// cleanCutFilterBody drops video blocks from a cluster body until the first
// video keyframe, so playback after a damage gap resumes with a clean cut
// instead of P/B frames referencing lost pictures. Audio and subtitle blocks
// pass through untouched. Returns the filtered body, the bytes dropped, and
// the keyframe's absolute time when one was found (playback is clean from
// there on).
func cleanCutFilterBody(body []byte, videoTracks map[uint64]bool, timecodeScale int64) (out []byte, dropped int64, kfMs int64, found bool) {
	br := bytes.NewReader(body)
	total := int64(len(body))
	pos := func() int64 { return total - int64(br.Len()) }

	out = make([]byte, 0, len(body))
	var clusterTS int64
	for {
		childStart := pos()
		h, n, err := ebml.ReadElementHeader(br)
		if err != nil {
			// Keep whatever remains verbatim: the body was validated before.
			out = append(out, body[childStart:]...)
			return out, dropped, kfMs, found
		}
		if h.Size < 0 || h.Size > int64(br.Len()) {
			out = append(out, body[childStart:]...)
			return out, dropped, kfMs, found
		}
		childEnd := childStart + int64(n) + h.Size

		keep := true
		switch h.ID {
		case mkv.IDTimestamp:
			if v, err := ebml.ReadUint(br, h.Size); err == nil {
				clusterTS = int64(v)
			}
		case mkv.IDSimpleBlock:
			track, relTC, keyframe, berr := readBlockHeader(br, h.Size)
			if berr == nil && videoTracks[track] {
				if keyframe {
					found = true
					if ms, e := reindexSafeTimecodeMs(clusterTS+int64(relTC), timecodeScale); e == nil {
						kfMs = ms
					}
				} else if !found {
					keep = false
				}
			}
		case mkv.IDBlockGroup:
			track, relTC, isKey, berr := scanBlockGroup(br, h.Size)
			if berr == nil && videoTracks[track] {
				if isKey {
					found = true
					if ms, e := reindexSafeTimecodeMs(clusterTS+int64(relTC), timecodeScale); e == nil {
						kfMs = ms
					}
				} else if !found {
					keep = false
				}
			}
		default:
			if _, err := io.CopyN(io.Discard, br, h.Size); err != nil {
				out = append(out, body[childStart:]...)
				return out, dropped, kfMs, found
			}
		}

		if keep {
			out = append(out, body[childStart:childEnd]...)
		} else {
			dropped += childEnd - childStart
		}
		if found {
			// Everything after the keyframe is kept verbatim.
			out = append(out, body[childEnd:]...)
			return out, dropped, kfMs, found
		}
	}
}
