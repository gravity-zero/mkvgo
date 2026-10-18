package reader

import (
	"errors"
	"fmt"
	"io"
)

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
func (br *BlockReader) SkippedBytes() int64          { return br.skippedBytes }
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
