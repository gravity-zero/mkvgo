package reader

import (
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

// errUnframed is the failure of the walk on bytes past the first Cluster that decode as an element header whose
// size leads to no element.
var errUnframed = errors.New("it declares a size that leads to no element")

// ErrDamagedRegion is what a DamageError matches (errors.Is): the walk met an
// element it cannot read while media continues behind it.
var ErrDamagedRegion = errors.New("damaged region in the cluster stream")

// DamageError is returned by Next when an element cannot be read - it declares
// more bytes than the file holds, or its header does not decode (a zeroed
// region, bytes a download never received) - while a valid Cluster follows
// further on: damage INSIDE the file, not a truncated tail. The two used to be
// indistinguishable - both surfaced as io.ErrUnexpectedEOF - and a caller
// tolerating a truncated tail then stopped at the damage and delivered the
// head of the file as if it were the whole of it. A DamageError deliberately
// does not match io.ErrUnexpectedEOF. Resume is -1 when the walk bounded how
// far it looks (SetDamageScan) and found no Cluster within that distance.
type DamageError struct {
	Offset int64 // where the unreadable element starts
	Resume int64 // the next valid Cluster, -1 when none lies within the bounded look
	Cause  error
}

// Error says where the damage lies and what to do: raise the damage bound or rewrite the file with a resyncing
// reindex.
func (e *DamageError) Error() string {
	if e.Resume < 0 {
		return fmt.Sprintf("damaged region at offset %d longer than the walk looks past it - damage inside the file; raise the damage bound or rewrite the file with a resyncing reindex (%v)", e.Offset, e.Cause)
	}
	return fmt.Sprintf("damaged region at offset %d: an element there cannot be read while media continues at offset %d - damage inside the file, not a truncated tail; rewrite the file with a resyncing reindex (%v)",
		e.Offset, e.Resume, e.Cause)
}

// DamageRange is a damaged region a SetSkipDamage walk passed over: [From, To), between the timestamps (ms) of the
// last Cluster entered before it and of the Cluster resumed on; -1 when the walk has not entered that Cluster.
type DamageRange struct{ From, To, FromMs, ToMs int64 }

// Is makes errors.Is(err, ErrDamagedRegion) hold for a DamageError.
func (e *DamageError) Is(target error) bool { return target == ErrDamagedRegion }

// SetSkipDamage makes the walk tolerant of damage inside the file: where Next
// would return an error although a valid Cluster follows, it resumes on that
// Cluster instead, and SkippedBytes counts what it passed over. For a
// MEASUREMENT of the file (where its tracks end), never for a copy of it - a
// copy that silently loses a region is what the default refuses.
func (br *BlockReader) SetSkipDamage(on bool) { br.skipDamage = on }

// SetDamageScan bounds how far past an unreadable element Next looks for the
// next valid Cluster (0: to the end of the segment). A bounded look also
// classifies a failure on the reader's seat (a cue pointing into a zeroed
// region), which an unbounded reader reports as the decode error it is; past
// the bound, the DamageError carries no Resume.
func (br *BlockReader) SetDamageScan(n int64) { br.damageScan = n }

// SkippedBytes is how many bytes of damaged regions a SetSkipDamage walk has
// passed over so far; SkippedRanges, which regions.
func (br *BlockReader) SkippedBytes() int64 { return br.skippedBytes }

// SkippedRanges lists the damaged regions a SetSkipDamage walk has passed over so far, in file order.
func (br *BlockReader) SkippedRanges() []DamageRange { return br.skipped }

// maxZeroTail is the longest run of null bytes closing a file that is taken
// for its end when it follows the last element: some muxers write a few there
// (1 to 68 on real files, which every player reads to the end), and nothing
// the file declares is missing. A longer run is a tail that was never
// written, and stays the error it is.
const maxZeroTail = 4096

// maxZeroTailInCluster is the same allowance inside a Cluster of declared
// size, where the null bytes stand in for blocks the Cluster says it holds:
// only the few a muxer's own padding accounts for.
const maxZeroTailInCluster = 64

// zeroTail reports whether everything from off to the end of the source is
// null bytes, few enough to be a muxer's closing padding (see maxZeroTail).
// An element cannot start on 0x00, so a walk that fails there has either
// reached such a closing run - the end of the file - or a zeroed region,
// which this refuses. It leaves the source where it was.
func (br *BlockReader) zeroTail(off int64) bool {
	if br.raw == nil {
		return false
	}
	limit := maxZeroTail
	if br.inCluster && br.clusterEnd >= 0 {
		limit = maxZeroTailInCluster
	}
	cur, err := br.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	defer br.raw.Seek(cur, io.SeekStart) //nolint:errcheck // best effort: the walk is over either way
	if _, err := br.raw.Seek(off, io.SeekStart); err != nil {
		return false
	}
	var tail [maxZeroTail + 1]byte
	n, err := io.ReadFull(br.raw, tail[:limit+1])
	if n == 0 || n > limit || (err != io.ErrUnexpectedEOF && err != io.EOF) {
		return false
	}
	for _, b := range tail[:n] {
		if b != 0 {
			return false
		}
	}
	return true
}

// skipDamageTo records the damaged region [from, to) the walk passes over, timed from the Cluster it was in.
func (br *BlockReader) skipDamageTo(from, to int64) {
	br.skippedBytes += to - from
	fromMs := int64(-1)
	if ms, err := safeTimecodeMs(br.clusterTS, br.timecodeScale); err == nil && br.clusterCount > 0 {
		fromMs = ms
	}
	br.skipped = append(br.skipped, DamageRange{From: from, To: to, FromMs: fromMs, ToMs: -1})
	br.timeSkip = true
}

// leaveStrayCluster reports whether the walk just failed on the first child of a Cluster declaring at most
// tinyCluster bytes: bytes that read as a Cluster header and hold no media. It seats the walk behind them, as if the
// Cluster had not been entered; a tolerant walk counts them.
func (br *BlockReader) leaveStrayCluster() bool {
	if br.raw == nil || !br.inCluster || br.clusterEnd < 0 || br.clusterEnd-br.clusterBody > tinyCluster || br.elemStart != br.clusterBody {
		return false
	}
	cur, err := br.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	var first [1]byte
	_, serr := br.raw.Seek(br.clusterBody, io.SeekStart)
	_, rerr := io.ReadFull(br.raw, first[:])
	if serr != nil || rerr != nil || clusterChildByte(first[0]) {
		_, _ = br.raw.Seek(cur, io.SeekStart)
		return false // it opens as a Cluster does: damage to place, not stray bytes
	}
	end := br.clusterEnd
	if br.r.reset(end) != nil {
		return false
	}
	if br.skipDamage {
		br.clusterCount-- // not a Cluster: the range is stamped with the one before it, if any
		br.skipDamageTo(br.clusterStart, end)
		br.clusterCount++
	}
	br.clusterCount--
	br.inCluster, br.clusterEnd = false, -1
	br.pending, br.peeked, br.awaitLimit = nil, nil, false
	br.atHead = br.fromStart && br.clusterCount == 0
	return true
}

// resumeInPadding reports whether the walk just failed, outside any Cluster, on the header that follows a Void or
// CRC-32, and a valid segment-level element starts in that padding's tail: the padding declared too much and
// swallowed its head. The walk is seated on that element; a tolerant walk counts the bytes declared too many.
func (br *BlockReader) resumeInPadding() bool {
	at := br.elemStart
	from := br.pad.from(at)
	if br.raw == nil || br.inCluster || from >= at {
		return false
	}
	cur, err := br.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	off := int64(-1)
	if _, err := br.raw.Seek(from, io.SeekStart); err == nil {
		off, _ = resyncToSegmentElement(br.raw, at, br.segEnd)
	}
	if off < 0 || off >= at || br.r.reset(off) != nil {
		_, _ = br.raw.Seek(cur, io.SeekStart)
		return false
	}
	if passed := br.pad.passed(at, off); br.skipDamage {
		br.skipDamageTo(off-passed, off)
	}
	return true
}

// maxTailJunk is the longest run of bytes that are no element, between the last Cluster and a valid element closing
// the Segment (Cues, Tags...), taken for what an in-place rewrite left behind: too short to hold a Cluster worth the
// name. A longer run may be a Cluster whose header was lost, and stays the error it is.
const maxTailJunk = 64

// tailJunk reports whether the element the walk could not read at from, outside any Cluster and with no Cluster
// behind it, is at most maxTailJunk bytes ahead of a valid segment-level element that is no Cluster: the media is
// whole, the walk is over. A tolerant walk counts the bytes.
func (br *BlockReader) tailJunk(from int64) bool {
	if br.raw == nil || br.inCluster || br.clusterCount == 0 {
		return false
	}
	cur, err := br.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	off := int64(-1)
	if _, err := br.raw.Seek(from+1, io.SeekStart); err == nil {
		off, _ = resyncToSegmentElement(br.raw, from+1+maxTailJunk, br.segEnd)
	}
	if off >= 0 {
		if id, _, err := ebml.ReadElementID(br.raw); err != nil || id == mkv.IDCluster {
			off = -1 // media behind the bytes: not the end
		}
	}
	if off < 0 || off-from > maxTailJunk {
		_, _ = br.raw.Seek(cur, io.SeekStart)
		return false
	}
	if br.skipDamage {
		br.skipDamageTo(from, off)
	}
	return true
}

// clusterAfter looks for a valid Cluster past off (the start of an element the walk could not read) and leaves the
// source where it found it, so a failed search changes nothing for a reader retried later; with none found, end is
// where the source or the Segment ends when the search reached it, else -1.
func (br *BlockReader) clusterAfter(off int64) (resume int64, found bool, end int64) {
	if br.raw == nil {
		return 0, false, -1
	}
	cur, err := br.raw.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, false, -1
	}
	defer br.raw.Seek(cur, io.SeekStart) //nolint:errcheck // best effort: the caller holds an error already
	size, err := br.raw.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, false, -1
	}
	if _, err := br.raw.Seek(off+1, io.SeekStart); err != nil {
		return 0, false, -1
	}
	scan := br.segEnd
	if br.damageScan > 0 && (scan < 0 || off+1+br.damageScan < scan) {
		scan = off + 1 + br.damageScan // how far the walk looks; the Cluster found must still fit the segment
	}
	resume, err = resyncToCluster(br.raw, scan, br.segEnd)
	if err == nil && resume > off {
		return resume, true, -1
	}
	if end = size; br.segEnd >= 0 && br.segEnd < end {
		end = br.segEnd
	}
	if err != nil || (scan >= 0 && scan < end) {
		return 0, false, -1
	}
	return 0, false, end
}
