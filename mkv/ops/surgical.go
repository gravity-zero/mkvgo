package ops

import (
	"bufio"
	"bytes"
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

// chainWalkChildren walks cluster children from pos, accepting only known
// child IDs with in-bounds sizes and, for blocks, a track number from tracks
// (when non-empty). It stops at the first validated top-level element, EOF,
// a structural break, or the run cap.
func chainWalkChildren(raw io.ReadSeeker, pos, fileSize int64, tracks map[uint64]bool) (surgicalRun, surgicalStop, error) {
	run := surgicalRun{start: pos, end: pos, firstRelTC: -1 << 62, lastRelTC: -1 << 62}
	if _, err := raw.Seek(pos, io.SeekStart); err != nil {
		return run, surgicalStopBreak, err
	}
	r := bufio.NewReaderSize(raw, reindexBufSize)
	for {
		if run.end >= fileSize {
			return run, surgicalStopEOF, nil
		}
		if run.end-run.start >= surgicalRunCap {
			return run, surgicalStopSplit, nil
		}
		h, n, err := ebml.ReadElementHeader(r)
		if err != nil {
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
		if !surgicalChildIDs[uint32(h.ID)] || h.Size < 0 || run.end+int64(n)+h.Size > fileSize {
			return run, surgicalStopBreak, nil
		}
		switch h.ID {
		case mkv.IDSimpleBlock:
			track, relTC, _, berr := readBlockHeader(r, h.Size)
			if berr != nil || (len(tracks) > 0 && !tracks[track]) {
				return run, surgicalStopBreak, nil
			}
			if !run.hasBlocks {
				run.firstRelTC = int64(relTC)
				run.hasBlocks = true
			}
			run.lastRelTC = int64(relTC)
		case mkv.IDBlockGroup:
			track, relTC, _, berr := scanBlockGroup(r, h.Size)
			if berr != nil || (len(tracks) > 0 && !tracks[track]) {
				return run, surgicalStopBreak, nil
			}
			if !run.hasBlocks {
				run.firstRelTC = int64(relTC)
				run.hasBlocks = true
			}
			run.lastRelTC = int64(relTC)
		default:
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				return run, surgicalStopBreak, nil
			}
		}
		run.end += int64(n) + h.Size
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
func surgicalCandidate(raw io.ReadSeeker, from, fileSize int64, tracks map[uint64]bool, prevLastRelTC int64, toleranceTC int64) (kind surgicalStop, off int64, err error) {
	limit := from + salvageResyncCap
	if limit > fileSize {
		limit = fileSize
	}
	const window = 64 << 10
	buf := make([]byte, window)
	for base := from; base < limit; base += window {
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
			switch {
			case b == 0x1F && i+4 <= end && bytes.Equal(buf[i:i+4], []byte{0x1F, 0x43, 0xB6, 0x75}):
				if validTopLevelAt(raw, cand, fileSize) {
					return surgicalStopAnchor, cand, nil
				}
			case b == 0xA3 || b == 0xA0:
				run, stop, perr := chainWalkChildren(raw, cand, fileSize, tracks)
				if perr != nil {
					return 0, -1, perr
				}
				solid := run.hasBlocks && (countChainChildren(raw, cand, run.end) >= 3 ||
					stop == surgicalStopAnchor || stop == surgicalStopEOF)
				if !solid {
					continue
				}
				if prevLastRelTC > (-1<<62) && run.firstRelTC < prevLastRelTC-toleranceTC {
					continue // restarts too far back in time: not this cluster's blocks
				}
				return surgicalStopBreak, cand, nil
			}
		}
	}
	return 0, -1, nil
}

// countChainChildren re-walks [start,end) counting the children, so the
// solidity gate ("at least K consecutive valid children") does not have to be
// threaded through chainWalkChildren.
func countChainChildren(raw io.ReadSeeker, start, end int64) int {
	if _, err := raw.Seek(start, io.SeekStart); err != nil {
		return 0
	}
	r := bufio.NewReaderSize(raw, 64<<10)
	pos, count := start, 0
	for pos < end {
		h, n, err := ebml.ReadElementHeader(r)
		if err != nil || h.Size < 0 {
			return count
		}
		if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
			return count
		}
		pos += int64(n) + h.Size
		count++
	}
	return count
}

// surgicalScanCluster maps the region of one damaged cluster starting at
// bodyStart (right after the cluster's ID+size header): valid child runs,
// the gaps between them, and the top-level anchor where the main walk should
// resume. It requires the cluster's own Timestamp to open the first run -
// without it the continuation runs could not be timed and the caller must
// fall back to dropping the region.
func surgicalScanCluster(raw io.ReadSeeker, bodyStart, fileSize int64, tracks map[uint64]bool, toleranceTC int64) (*surgicalOutcome, error) {
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
	for {
		run, stop, err := chainWalkChildren(raw, pos, fileSize, tracks)
		if err != nil {
			return nil, err
		}
		if run.end > run.start {
			out.runs = append(out.runs, run)
			if run.hasBlocks {
				prevLast = run.lastRelTC
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
		// Structural break: hunt for the next anchor or chain-valid blocks.
		kind, cand, err := surgicalCandidate(raw, run.end+1, fileSize, tracks, prevLast, toleranceTC)
		if err != nil {
			return nil, err
		}
		if cand < 0 {
			if run.end+salvageResyncCap >= fileSize {
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
