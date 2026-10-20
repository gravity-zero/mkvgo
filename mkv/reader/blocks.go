package reader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
)

const (
	lacingNone  = 0
	lacingXiph  = 1
	lacingFixed = 2
	lacingEBML  = 3

	progressInterval = 50
	maxBlockSize     = 64 * 1024 * 1024 // 64 MB max per block

	bufSize = 256 << 10 // 256 KiB max read-ahead

	// minChunk is the read-ahead right after a seek-skip: enough for the next
	// block headers without dragging a skipped payload back in. The chunk then
	// doubles on every sequential fill up to bufSize, so a walk over payloads
	// too small to seek past (frame-interleaved video is a few KiB per block)
	// converges to plain bulk sequential reads - never one read per block,
	// which is what a network filesystem punishes.
	minChunk = 16 << 10

	// seekSkipMin is the smallest beyond-window skip worth a real seek. Every
	// source read costs a fixed round trip on remote-ish filesystems (9p, SMB,
	// HTTP) on top of the bytes; seeking over a payload only pays when the
	// bytes saved outweigh the extra round trip the next small read costs  -
	// measured around tens of KiB there, immaterial on local disks. Below the
	// threshold reading forward through the growing window is cheaper, and the
	// walk stays a bulk sequential read.
	seekSkipMin = 64 << 10

	// sparseChunk is every read of a sparse walk (SetSparse): a few block
	// headers, never the payload behind them.
	sparseChunk = 256
)

// errFilteredBlock is parseBlock's signal that the block belongs to a track
// outside the filter and its payload was skipped; Next keeps walking.
var errFilteredBlock = errors.New("block filtered")

// ErrClusterLimit is returned by Next when the walk reaches a cluster whose
// timestamp exceeds the StopBeforeClusterMs limit - before delivering any of
// that cluster's blocks. The walk can continue on the same reader after
// raising the limit, or be resumed later by a new reader at ResumeOffset.
var ErrClusterLimit = errors.New("cluster beyond the requested timecode limit")

// peekedHeader is a pre-read element header kept for one iteration of Next().
type peekedHeader struct {
	h     ebml.ElementHeader
	start int64 // absolute offset of the header's first byte
}

// BlockPos locates a block precisely enough to restart a walk there - at the
// block itself, mid-cluster, not at the enclosing cluster's header. A cluster
// spans several segments of a typical HLS grid, so a server that can only
// resume at cluster boundaries re-reads the same cluster prefix once per
// segment it holds; a resume point taken at the boundary block reads each byte
// once. Zero value = no position (Valid reports it).
type BlockPos struct {
	Off          int64 // absolute offset of the block's element header (SimpleBlock or BlockGroup)
	ClusterStart int64 // absolute offset of the enclosing cluster's header
	ClusterEnd   int64 // absolute offset one past the cluster's last byte; -1 when unknown-size
	ClusterTS    int64 // the enclosing cluster's raw Timestamp (unscaled)
}

// Valid reports whether p names a block (a zero BlockPos does not: offset 0 is
// the EBML header, never a block).
func (p BlockPos) Valid() bool { return p.Off > 0 }

// IndexedBlock is one block as a seek index records it: where it is, the
// timecode of its first frame in milliseconds, and how many frames it holds
// (more than one for a laced block, all behind the same position). A reader
// seated on Pos (NewBlockReaderFrom, SeekTo) delivers those frames next.
type IndexedBlock struct {
	Pos    BlockPos
	TimeMs int64
	Frames int64
}

// BlockReader reads MKV blocks sequentially from an io.ReadSeeker.
// Internally it uses a buffered reader and tracks position by counting bytes
// so that Seek(0, SeekCurrent) syscalls are eliminated on the hot path.
//
// Unknown-size clusters are supported: clusterEnd==-1 combined with inCluster
// means we are inside an unknown-size cluster; the boundary is detected by
// peeking ahead and checking if the next element is a segment-level element.
type BlockReader struct {
	r             *countingReader
	raw           io.ReadSeeker // the source: nil for ReadStream's reader, a readOnlySeeker for NewStreamBlockReader's
	segEnd        int64
	clusterEnd    int64
	inCluster     bool // true when inside a cluster (including unknown-size)
	clusterTS     int64
	timecodeScale int64
	pending       []mkv.Block
	keep          map[uint64]bool // when non-nil, blocks of other tracks are skipped unread
	// blockBuf, when set, is asked for the buffer an unlaced block's payload
	// is read into (see SetBlockBuffer).
	blockBuf func(track uint64, size int) []byte
	// headerOnly, when set, discards the payload of an unlaced kept block
	// instead of reading it: Block.Data stays nil and Block.Size reports the
	// byte length alone. A laced block still needs its lacing header decoded
	// to size its frames, so it is read normally and the bytes dropped right
	// after (real muxers do not lace video, the track this mode targets).
	headerOnly    bool
	headerOnlyFor map[uint64]bool
	// trackDurNs maps track number → DefaultDuration (ns), the per-frame stride
	// that times the frames of a laced block (they share one stored timecode).
	// Filled by SetTrackDefaultDurations, or opportunistically from the Tracks
	// element when the sequential walk passes over it.
	trackDurNs   map[uint64]int64
	peeked       *peekedHeader // element read in peek-ahead, to be processed next iteration
	stopMs       int64         // StopBeforeClusterMs limit (only when hasStop)
	hasStop      bool          // a cluster-timecode limit is set
	awaitLimit   bool          // stopped at a cluster beyond stopMs; recheck on Next
	clusterStart int64         // absolute offset of the current cluster's header
	blockStart   int64         // absolute offset of the block element Next last returned from
	clusterCount int64         // number of Cluster elements entered so far
	// atHead is set on a reader that walks the Segment from its start, until
	// the first Cluster: junk ahead of or between the head elements is resynced
	// past there (see resyncHead). A reader seated at a recorded position never
	// sets it - an undecodable header there means the position is stale.
	atHead bool
	// elemStart is the offset of the element the walk is reading: where a
	// failure began, and where the search for the media behind it starts.
	elemStart int64
	// skipDamage makes Next resume on the next valid Cluster past a damaged
	// region instead of returning an error (SetSkipDamage); skippedBytes counts
	// what was passed over.
	skipDamage   bool
	skippedBytes int64
	skipped      []DamageRange
	timeSkip     bool  // the last skipped region still waits for the timestamp of the Cluster it resumed on
	damageScan   int64 // SetDamageScan: how far past an unreadable element the walk looks for a Cluster (0: to the segment's end)
	// known is the set of track numbers the file declares, learned when the
	// walk passes over the Tracks element or given by SetKnownTracks; nil when
	// unknown (a reader seated in mid-file). A block naming any other track is
	// not the file's: it is dropped, and suspectAt remembers the first one met
	// since the last Cluster header (suspected) - where a damaged region most
	// likely starts, should the walk fail further on.
	known         map[uint64]bool
	suspected     bool
	suspectAt     int64
	progressFn    mkv.ProgressFunc
	progressTotal int64
	progressTick  int
}

func NewBlockReader(r io.ReadSeeker, timecodeScale int64) (*BlockReader, error) {
	br := &BlockReader{
		raw:           r,
		timecodeScale: timecodeScale,
		segEnd:        -1,
		clusterEnd:    -1,
	}
	if err := br.init(); err != nil {
		return nil, err
	}
	return br, nil
}

// NewBlockReaderAt is NewBlockReader starting mid-file: r is positioned at
// offset - the absolute file offset of a Cluster element (e.g. a CuePoint's
// Container.SegmentStart + ClusterPos) - and blocks are read from that cluster
// on. The caller supplies the TimecodeScale (from a prior metadata read); the
// EBML/Segment headers are not re-parsed. The segment end is unknown, so
// reading stops at EOF or at a non-cluster top-level element.
func NewBlockReaderAt(r io.ReadSeeker, timecodeScale int64, offset int64) (*BlockReader, error) {
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	br := &BlockReader{
		raw:           r,
		timecodeScale: timecodeScale,
		segEnd:        -1,
		clusterEnd:    -1,
	}
	br.r = newCountingReader(r, offset)
	return br, nil
}

// NewBlockReaderFrom resumes a walk at the exact block a previous reader
// stopped on (its Pos), inside the cluster that block belongs to: no cluster
// prefix is re-read. The caller supplies the TimecodeScale, as with
// NewBlockReaderAt; the segment end is unknown, so reading stops at EOF or at
// a non-cluster top-level element.
func NewBlockReaderFrom(r io.ReadSeeker, timecodeScale int64, p BlockPos) (*BlockReader, error) {
	if !p.Valid() {
		return nil, fmt.Errorf("resume: invalid block position")
	}
	if _, err := r.Seek(p.Off, io.SeekStart); err != nil {
		return nil, err
	}
	br := &BlockReader{
		raw:           r,
		timecodeScale: timecodeScale,
		segEnd:        -1,
		inCluster:     true,
		clusterEnd:    p.ClusterEnd,
		clusterTS:     p.ClusterTS,
		clusterStart:  p.ClusterStart,
		clusterCount:  1,
	}
	br.r = newCountingReader(r, p.Off)
	return br, nil
}

// RestartAt re-seats an existing reader at the Cluster element at offset, the
// way NewBlockReaderAt seats a new one, reusing this reader's window and its
// KeepTracks/header-only settings.
func (br *BlockReader) RestartAt(offset int64) error {
	if br.raw == nil {
		return fmt.Errorf("restart: reader is not seekable")
	}
	if err := br.r.reset(offset); err != nil {
		return err
	}
	if br.segEnd >= 0 && offset >= br.segEnd {
		br.segEnd = -1
	}
	br.inCluster = false
	br.clusterEnd = -1
	br.clusterTS = 0
	br.timeSkip = false
	br.clusterStart = 0
	br.pending = nil
	br.peeked = nil
	br.awaitLimit = false
	br.blockStart = 0
	return nil
}

// SeekTo re-seats an existing reader at p, the way NewBlockReaderFrom seats a
// new one - but reusing this reader's window and its KeepTracks/header-only
// settings. A caller holding many recorded positions in one file (a subtitle
// block index, say) walks them with one reader instead of one per position,
// which is what keeps the window from being reallocated per block.
func (br *BlockReader) SeekTo(p BlockPos) error {
	if !p.Valid() {
		return fmt.Errorf("seek: invalid block position")
	}
	if br.raw == nil {
		return fmt.Errorf("seek: reader is not seekable")
	}
	if err := br.r.reset(p.Off); err != nil {
		return err
	}
	// Keep a segment end that still bounds this position: NewBlockReaderFrom
	// clears it because it cannot know one, which is not this reader's case.
	if br.segEnd >= 0 && p.Off >= br.segEnd {
		br.segEnd = -1
	}
	br.inCluster = true
	br.clusterEnd = p.ClusterEnd
	br.clusterTS = p.ClusterTS
	br.timeSkip = false
	br.clusterStart = p.ClusterStart
	br.pending = nil
	br.peeked = nil
	br.awaitLimit = false
	// Pos() must not pair the PREVIOUS block's offset with this cluster's
	// bounds: that would be a valid-looking but incoherent resume point.
	br.blockStart = 0
	br.clusterCount = 1
	return nil
}

// Pos locates the block Next last returned - the resume point NewBlockReaderFrom
// takes. The frames of a laced block share their enclosing block's position, so
// resuming there re-delivers the whole lace.
func (br *BlockReader) Pos() BlockPos {
	return BlockPos{
		Off:          br.blockStart,
		ClusterStart: br.clusterStart,
		ClusterEnd:   br.clusterEnd,
		ClusterTS:    br.clusterTS,
	}
}

// StopBeforeClusterMs bounds the walk: Next returns ErrClusterLimit instead
// of entering a cluster whose timestamp exceeds ms - even when a track filter
// would otherwise skim silently to EOF hunting for a kept block. Clusters are
// stored in timestamp order, so everything up to ms has been delivered when
// the limit fires. The limit can be raised afterwards to continue on the same
// reader; ResumeOffset lets a later reader restart at the held-back cluster.
func (br *BlockReader) StopBeforeClusterMs(ms int64) {
	br.stopMs = ms
	br.hasStop = true
}

// ResumeOffset is the absolute offset of the cluster the walk stopped before
// (valid after Next returned ErrClusterLimit): pass it to NewBlockReaderAt to
// resume the walk exactly where it stopped.
func (br *BlockReader) ResumeOffset() int64 {
	return br.clusterStart
}

// ClusterCount returns the number of Cluster elements the walk has entered so
// far (a structural count, from element headers alone - it costs nothing
// beyond what Next already reads).
func (br *BlockReader) ClusterCount() int64 {
	return br.clusterCount
}

// ClusterOffset returns the absolute file offset of the Cluster element
// holding the most recently returned block - subtract the Segment body start
// to obtain a CuePoint.ClusterPos. Valid once Next has returned a block.
func (br *BlockReader) ClusterOffset() int64 {
	return br.clusterStart
}

// KeepTracks restricts Next to blocks of the given tracks. Other tracks'
// payloads are never delivered, and a payload past the read-ahead window is
// seeked over rather than read.
//
// How much I/O that actually saves depends on the source, and on real files it
// is usually little: a payload already sitting in the window is dropped from it
// for free, having been read, so the effective threshold is the window (up to
// bufSize) and not seekSkipMin. On a 2160p remux where two thirds of the bytes
// live in blocks smaller than the window, filtering cut the bytes read by ~8%.
// The saving that is not conditional is the work per block: no payload is
// copied out or allocated for a filtered track, which on a file with millions
// of blocks dominates - measured at -85% user CPU on a subtitle extraction.
// The walk never degrades below a plain full pass: where payloads are too
// small to seek past it stays a bulk sequential read.
func (br *BlockReader) KeepTracks(tracks ...uint64) {
	br.keep = make(map[uint64]bool, len(tracks))
	for _, id := range tracks {
		br.keep[id] = true
	}
}

// SetBlockBuffer lets the caller choose where the payload of an unlaced block
// is read: before reading one, the walk asks buf for a slice of exactly size
// bytes for that track, and reads into it instead of into a buffer of its own
// (it allocates as usual when buf returns anything else, nil included). A
// caller assembling consecutive blocks into one buffer - a media segment -
// hands out the tail of that buffer and saves a buffer per block and a copy
// of every one. The Block's Data is then that slice: what the caller does
// with the memory afterwards is the caller's business. Laced blocks are read
// as before.
func (br *BlockReader) SetBlockBuffer(buf func(track uint64, size int) []byte) { br.blockBuf = buf }

// SetSegmentEnd bounds a reader opened at an offset (NewBlockReaderAt, NewBlockReaderFrom), which cannot read the
// Segment's size, to the Segment's declared end (mkv.Container.SegmentEnd): between Clusters the walk ends there, as
// it does for a reader opened at the file's head, and the bytes after it are trailing data, not damage. An end of 0
// or less, unknown, leaves the reader unbounded.
func (br *BlockReader) SetSegmentEnd(end int64) {
	if end > 0 {
		br.segEnd = end
	}
}

// SetHeaderOnly enables a structure-only walk: an unlaced kept block's
// payload is seek-skipped instead of read, and Next reports its size on
// Block.Size with Block.Data left nil. Combined with KeepTracks (other
// tracks' blocks are already skipped unread), the walk's cost is bounded by
// the kept track's block-header count, never by any payload bytes.
func (br *BlockReader) SetHeaderOnly(on bool) {
	br.headerOnly = on
}

// SetHeaderOnlyTracks limits header-only mode to these tracks: the others' kept blocks keep their payload. None lifts the limit.
func (br *BlockReader) SetHeaderOnlyTracks(tracks ...uint64) {
	br.headerOnlyFor = nil
	if len(tracks) > 0 {
		br.headerOnlyFor = make(map[uint64]bool, len(tracks))
		for _, id := range tracks {
			br.headerOnlyFor[id] = true
		}
	}
}

// SetSparse makes a header-only walk of a local file seek over every payload
// it skips, however small, reading a few KiB at a time: the walk reads the
// block headers and little else. A network source pays a round trip per read
// and keeps the default.
func (br *BlockReader) SetSparse(on bool) {
	br.r.sparse = on
	br.r.chunk = br.r.restartChunk()
}

// skipsPayload reports whether header-only mode applies to this track's blocks.
func (br *BlockReader) skipsPayload(track uint64) bool {
	return br.headerOnly && (br.headerOnlyFor == nil || br.headerOnlyFor[track])
}

// maxLacedFrameDurNs bounds a plausible per-frame duration (10 s): a larger
// DefaultDuration is treated as garbage rather than shifting laced frames by
// absurd offsets.
const maxLacedFrameDurNs = 10_000_000_000

// SetTrackDefaultDurations supplies each track's DefaultDuration in
// nanoseconds (track number → ns). A laced block stores ONE timecode for its N
// frames; with the stride known, frame i is delivered at blockTS +
// round(i*dur) - without it every frame of the lace keeps the block timecode
// (collapsed timestamps downstream). A sequential reader picks the durations
// up on its own when it walks over the Tracks element; a mid-file reader
// (NewBlockReaderAt) never sees it, so callers holding the track metadata
// should pass TrackDefaultDurations(c.Tracks).
func (br *BlockReader) SetTrackDefaultDurations(durs map[uint64]int64) {
	br.trackDurNs = durs
}

// TrackDefaultDurations builds the SetTrackDefaultDurations argument from
// parsed track metadata: track number → DefaultDurationNs, for every track
// that declares one.
func TrackDefaultDurations(tracks []mkv.Track) map[uint64]int64 {
	var m map[uint64]int64
	for _, t := range tracks {
		if t.DefaultDurationNs > 0 && t.DefaultDurationNs <= maxLacedFrameDurNs {
			if m == nil {
				m = make(map[uint64]int64)
			}
			m[t.ID] = t.DefaultDurationNs
		}
	}
	return m
}

// scanTracksDurations opportunistically reads TrackNumber + DefaultDuration
// pairs from a Tracks element the sequential walk is passing over, so laced
// frames get timed even when the caller never supplied the track metadata.
// Best-effort: structural anomalies skip the rest of the element (as the plain
// discard did); only I/O errors propagate.
func (br *BlockReader) scanTracksDurations(size int64) error {
	end := br.r.tell() + size
	skipRest := func() error { return br.r.discard(end - br.r.tell()) }
	learnDurs := br.trackDurNs == nil // durations a caller supplied are left alone
	declared := map[uint64]bool{}
	for br.r.tell() < end {
		h, _, err := ebml.ReadElementHeader(br.r)
		if err != nil {
			return err
		}
		if h.ID != mkv.IDTrackEntry {
			if h.Size < 0 || br.r.tell()+h.Size > end {
				return skipRest()
			}
			if err := br.r.discard(h.Size); err != nil {
				return err
			}
			continue
		}
		teEnd := br.r.tell() + h.Size
		if h.Size < 0 || teEnd > end {
			return skipRest()
		}
		var num uint64
		var durNs int64
		for br.r.tell() < teEnd {
			eh, _, err := ebml.ReadElementHeader(br.r)
			if err != nil {
				return err
			}
			if eh.Size < 0 || br.r.tell()+eh.Size > teEnd {
				return skipRest()
			}
			switch eh.ID {
			case mkv.IDTrackNumber:
				if eh.Size > 8 {
					// Wider than a uint can be: structural, so skip the rest
					// rather than killing the caller's whole block walk over an
					// opportunistic read.
					return skipRest()
				}
				v, err := ebml.ReadUint(br.r, eh.Size)
				if err != nil {
					return err
				}
				num = v
			case mkv.IDDefaultDuration:
				if eh.Size > 8 {
					return skipRest()
				}
				v, err := ebml.ReadUint(br.r, eh.Size)
				if err != nil {
					return err
				}
				durNs = int64(v)
			default:
				if err := br.r.discard(eh.Size); err != nil {
					return err
				}
			}
		}
		if num > 0 {
			declared[num] = true
		}
		if learnDurs && num > 0 && durNs > 0 && durNs <= maxLacedFrameDurNs {
			if br.trackDurNs == nil {
				br.trackDurNs = make(map[uint64]int64)
			}
			br.trackDurNs[num] = durNs
		}
	}
	// Only a Tracks element read to its end says which tracks exist: a partial
	// list would drop the blocks of real tracks.
	if br.known == nil && len(declared) > 0 {
		br.known = declared
	}
	return nil
}

// SetKnownTracks tells the walk which tracks the file declares, for a reader
// that never passes over the Tracks element (NewBlockReaderAt). A block naming
// another track is dropped, as it is on a walk from the start of the file:
// such a block is not the file's content, but bytes of a damaged region that
// happen to parse.
func (br *BlockReader) SetKnownTracks(tracks []mkv.Track) {
	if len(tracks) == 0 {
		return
	}
	br.known = make(map[uint64]bool, len(tracks))
	for _, t := range tracks {
		br.known[t.ID] = true
	}
}

func (br *BlockReader) SetProgress(fn mkv.ProgressFunc, total int64) {
	br.progressFn = fn
	br.progressTotal = total
}

func (br *BlockReader) reportProgress() {
	if br.progressFn == nil {
		return
	}
	br.progressFn(br.r.tell(), br.progressTotal)
}

// maxEBMLHeaderSize bounds the EBML header body init skips: a real one holds a few dozen bytes.
const maxEBMLHeaderSize = 1 << 20

func (br *BlockReader) init() error {
	// Phase 1: read EBML header ID+size from the raw (unbuffered) source.
	// We must consume these bytes before wrapping in the buffered reader so it
	// starts at the right position.
	h1, n1, err := ebml.ReadElementHeader(br.raw)
	if err != nil {
		return fmt.Errorf("read EBML header: %w", err)
	}
	if h1.ID != ebml.IDEBMLHeader {
		return fmt.Errorf("expected EBML header, got 0x%X", h1.ID)
	}
	if h1.Size < 0 || h1.Size > maxEBMLHeaderSize {
		return fmt.Errorf("EBML header declares %d bytes (a real one holds a few dozen, at most %d)", h1.Size, maxEBMLHeaderSize)
	}
	// Skip EBML header body via the raw reader (before we create the buffer).
	if _, err := io.CopyN(io.Discard, br.raw, h1.Size); err != nil {
		return err
	}

	// Phase 2: read Segment header.
	h2, n2, err := ebml.ReadElementHeader(br.raw)
	if err != nil {
		return fmt.Errorf("read segment: %w", err)
	}
	if h2.ID != mkv.IDSegment {
		return fmt.Errorf("expected Segment, got 0x%X", h2.ID)
	}

	// Now we know the exact byte position: n1 + h1.Size + n2.
	startPos := int64(n1) + h1.Size + int64(n2)

	// Wrap the raw reader in a counting buffered reader.
	// The buffer will issue its first real Read from startPos onwards.
	br.r = newCountingReader(br.raw, startPos)
	br.atHead = true

	if h2.Size >= 0 {
		br.segEnd = startPos + h2.Size
	}
	return nil
}

// resyncHead repositions the walk on the next valid segment-level element at
// or after from, and reports whether it found one. It is the block walk's
// share of the head resync Read and ReadMeta do: junk between the Segment
// header and the first Cluster must not make every block unreachable. On a
// non-seekable source, or when nothing valid follows, it reports false and
// the caller returns the decode error it already holds.
func (br *BlockReader) resyncHead(from int64) bool {
	if _, err := br.raw.Seek(from, io.SeekStart); err != nil {
		return false
	}
	off, err := resyncToSegmentElement(br.raw, -1, br.segEnd)
	if err != nil || off < 0 || br.r.reset(off) != nil {
		return false
	}
	if br.skipDamage {
		br.skipDamageTo(from-1, off) // a tolerant walk reports what it passed over, a Cluster it lost included
	}
	return true
}

// isSegmentLevelID returns true for element IDs that can appear directly
// inside a Segment (as opposed to inside a Cluster). These are used to detect
// the boundary of unknown-size clusters: reading such an ID while inside a
// cluster means the cluster has ended.
func isSegmentLevelID(id uint32) bool {
	switch id {
	case mkv.IDCluster, mkv.IDSeekHead, mkv.IDInfo, mkv.IDTracks,
		mkv.IDCues, mkv.IDAttachments, mkv.IDChapters, mkv.IDTags:
		return true
	}
	return false
}

// BytesRead is how many bytes the reader has read from its source so far.
func (br *BlockReader) BytesRead() int64 {
	if br.r == nil {
		return 0
	}
	return br.r.nread
}

// Next returns the next block, io.EOF at the clean end of the walk. See
// DamageError and SetSkipDamage for an unreadable element in mid-file.
func (br *BlockReader) Next() (mkv.Block, error) {
	for {
		b, err := br.next()
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, ErrClusterLimit) {
			return b, err
		}
		if br.zeroTail(br.elemStart) {
			return mkv.Block{}, io.EOF
		}
		if br.r.failed != nil {
			return b, err // the source itself failed (I/O, cancellation, replaced file): not damage in the file
		}
		if br.clusterCount == 0 && br.damageScan <= 0 {
			// Not one Cluster entered yet: a reader seated on a position that
			// turns out not to be a Cluster (a stale cue) is not looking at
			// damage in the file. That error stays as it is - unless the walk
			// bounded its look, which asks for the classification.
			return b, err
		}
		// The damage starts at the element that failed - or earlier, at the
		// first block of an undeclared track met since the last Cluster
		// header: garbage that parsed, with the real media possibly resuming
		// well before the point of failure.
		from := br.elemStart
		if br.suspected && br.suspectAt < from {
			from = br.suspectAt
		}
		resume, found, end := br.clusterAfter(from)
		if !found && end >= 0 && br.skipDamage {
			br.skipDamageTo(from, end) // the damage runs to the end: the walk is over
			return b, io.EOF
		}
		if !found {
			if br.damageScan > 0 {
				return b, &DamageError{Offset: from, Resume: -1, Cause: err} // longer than the walk looks
			}
			return b, err // nothing behind it: a truncated tail, as reported
		}
		if !br.skipDamage {
			return b, &DamageError{Offset: from, Resume: resume, Cause: err}
		}
		if br.r.reset(resume) != nil {
			return b, err
		}
		br.skipDamageTo(from, resume)
		br.inCluster, br.clusterEnd = false, -1
		br.pending, br.peeked, br.awaitLimit = nil, nil, false
	}
}

func (br *BlockReader) next() (mkv.Block, error) {
	if len(br.pending) > 0 {
		b := br.pending[0]
		br.pending = br.pending[1:]
		return b, nil
	}
	if br.awaitLimit {
		// Held before a cluster beyond the limit: stay held unless the limit
		// was raised past the cluster's timestamp.
		if tsMs, err := safeTimecodeMs(br.clusterTS, br.timecodeScale); br.hasStop && err == nil && tsMs > br.stopMs {
			return mkv.Block{}, ErrClusterLimit
		}
		br.awaitLimit = false
	}

	br.progressTick++
	if br.progressTick%progressInterval == 0 {
		br.reportProgress()
	}

	for {
		// --- Inside a cluster ---
		if br.inCluster {
			// Check known-size cluster boundary.
			if br.clusterEnd >= 0 && br.r.tell() >= br.clusterEnd {
				br.inCluster = false
				br.clusterEnd = -1
				continue
			}
			// An unknown-size cluster ends at a known Segment's end at the latest: what follows is not its child.
			if br.clusterEnd < 0 && br.segEnd >= 0 && br.r.tell() >= br.segEnd {
				br.inCluster = false
				continue
			}

			// Read next element header (or use peeked one).
			var h ebml.ElementHeader
			var hdrStart int64
			if br.peeked != nil {
				h = br.peeked.h
				hdrStart = br.peeked.start
				br.elemStart = hdrStart
				br.peeked = nil
			} else {
				hdrStart = br.r.tell()
				br.elemStart = hdrStart
				var err error
				h, _, err = ebml.ReadElementHeader(br.r)
				if err != nil {
					if errors.Is(err, io.EOF) && br.clusterEnd < 0 {
						return mkv.Block{}, io.EOF
					}
					if errors.Is(err, io.EOF) {
						return mkv.Block{}, io.ErrUnexpectedEOF // a known-size Cluster cut short between two of its elements
					}
					return mkv.Block{}, err
				}
			}

			// For unknown-size clusters, a segment-level element ends the cluster.
			if br.clusterEnd < 0 && isSegmentLevelID(h.ID) {
				br.inCluster = false
				br.clusterEnd = -1
				br.peeked = &peekedHeader{h: h, start: hdrStart}
				continue
			}

			if err := pastEnd(h, br.r.tell(), br.clusterEnd); err != nil {
				return mkv.Block{}, err
			}

			switch h.ID {
			case mkv.IDTimestamp:
				v, err := ebml.ReadUint(br.r, h.Size)
				if err != nil {
					return mkv.Block{}, err
				}
				if v > maxClusterTimestamp {
					return mkv.Block{}, fmt.Errorf("cluster timestamp %d past what a block's relative timecode can be added to", v)
				}
				br.clusterTS = int64(v)
				tsMs, err := safeTimecodeMs(br.clusterTS, br.timecodeScale)
				if br.timeSkip && err == nil {
					br.skipped[len(br.skipped)-1].ToMs, br.timeSkip = tsMs, false
				}
				if br.hasStop && err == nil && tsMs > br.stopMs {
					br.awaitLimit = true
					return mkv.Block{}, ErrClusterLimit
				}
				continue
			case mkv.IDSimpleBlock:
				br.blockStart = hdrStart
				b, err := br.parseBlock(h.Size, true)
				if errors.Is(err, errFilteredBlock) {
					continue
				}
				return b, err
			case mkv.IDBlockGroup:
				br.blockStart = hdrStart
				b, err := br.parseBlockGroup(h.Size)
				if errors.Is(err, errFilteredBlock) {
					continue
				}
				return b, err
			default:
				if h.Size < 0 {
					// Unknown-size non-top-level element inside cluster: skip safely.
					return mkv.Block{}, fmt.Errorf("unknown-size element 0x%X inside cluster", h.ID)
				}
				if err := br.r.discard(h.Size); err != nil {
					return mkv.Block{}, err
				}
				continue
			}
		}

		// --- Between clusters (segment level) ---
		if br.segEnd >= 0 {
			if br.r.tell() >= br.segEnd {
				return mkv.Block{}, io.EOF
			}
		}

		var h ebml.ElementHeader
		var hdrStart int64
		if br.peeked != nil {
			h = br.peeked.h
			hdrStart = br.peeked.start
			br.elemStart = hdrStart
			br.peeked = nil
		} else {
			hdrStart = br.r.tell()
			br.elemStart = hdrStart
			var err error
			h, _, err = ebml.ReadElementHeader(br.r)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return mkv.Block{}, io.EOF
				}
				if br.atHead && br.resyncHead(hdrStart+1) {
					continue
				}
				return mkv.Block{}, err
			}
		}

		if h.ID == mkv.IDCluster {
			br.atHead = false
			br.suspected = false
			br.clusterStart = hdrStart
			br.clusterCount++
			br.inCluster = true
			if h.Size >= 0 {
				br.clusterEnd = br.r.tell() + h.Size
			} else {
				br.clusterEnd = -1 // unknown-size cluster
			}
			continue
		}
		if h.Size < 0 {
			return mkv.Block{}, fmt.Errorf("unknown-size element 0x%X outside cluster", h.ID)
		}
		if h.ID == mkv.IDTracks && (br.trackDurNs == nil || br.known == nil) && h.Size <= 16<<20 {
			// Walking over the track metadata anyway: pick up the per-track
			// DefaultDurations so laced frames get individual timecodes even
			// when the caller never supplied them.
			if err := br.scanTracksDurations(h.Size); err != nil {
				return mkv.Block{}, err
			}
			continue
		}
		if err := br.r.discard(h.Size); err != nil {
			return mkv.Block{}, err
		}
	}
}

func (br *BlockReader) parseBlock(size int64, simple bool) (mkv.Block, error) {
	// An unknown size must be refused here, before anything else: the filter
	// below discards "size - consumed", and a negative count is a no-op, so a
	// filtered block of unknown size would leave the walk sitting inside the
	// payload and parsing it as element headers.
	if size < 0 {
		return mkv.Block{}, fmt.Errorf("block with unknown size")
	}
	start := br.r.tell()

	trackNum, _, err := ebml.ReadDataSize(br.r)
	if err != nil {
		return mkv.Block{}, err
	}
	if br.r.tell()-start > size {
		// Refused before the filters: discarding a negative rest would leave the walk inside what follows.
		return mkv.Block{}, fmt.Errorf("block's track number runs %d bytes past the block", br.r.tell()-start-size)
	}
	if br.known != nil && !br.known[uint64(trackNum)] {
		// A track the file does not declare: bytes that happen to parse as a
		// block, not content. Dropped, and remembered as where damage may
		// have begun.
		if !br.suspected {
			br.suspected, br.suspectAt = true, br.blockStart
		}
		if err := br.r.discard(size - (br.r.tell() - start)); err != nil {
			return mkv.Block{}, err
		}
		return mkv.Block{}, errFilteredBlock
	}
	if br.keep != nil && !br.keep[uint64(trackNum)] {
		if err := br.r.discard(size - (br.r.tell() - start)); err != nil {
			return mkv.Block{}, err
		}
		return mkv.Block{}, errFilteredBlock
	}

	tc16, err := br.r.readBE(2)
	if err != nil {
		return mkv.Block{}, err
	}
	relTC := int16(uint16(tc16))

	flags, err := br.r.ReadByte()
	if err != nil {
		return mkv.Block{}, err
	}
	keyframe := simple && flags&0x80 != 0
	lacing := (flags >> 1) & 0x03

	dataSize := size - (br.r.tell() - start)
	if dataSize < 0 || dataSize > maxBlockSize {
		return mkv.Block{}, fmt.Errorf("invalid block data size %d", dataSize)
	}

	if lacing == lacingNone {
		tc, err := safeTimecodeMs(br.clusterTS+int64(relTC), br.timecodeScale)
		if err != nil {
			return mkv.Block{}, err
		}
		dataOff := br.r.tell()
		if br.skipsPayload(uint64(trackNum)) {
			if err := br.r.discard(dataSize); err != nil {
				return mkv.Block{}, err
			}
			return mkv.Block{
				TrackNumber: uint64(trackNum), Timecode: tc, BlockTimecode: tc,
				Keyframe: keyframe, Size: dataSize, DataOffset: dataOff,
			}, nil
		}
		var data []byte
		if br.blockBuf != nil {
			data = br.blockBuf(uint64(trackNum), int(dataSize))
		}
		if int64(len(data)) != dataSize {
			data = make([]byte, dataSize)
		}
		if _, err := io.ReadFull(br.r, data); err != nil {
			return mkv.Block{}, err
		}
		return mkv.Block{
			TrackNumber: uint64(trackNum), Timecode: tc, BlockTimecode: tc,
			Keyframe: keyframe, Data: data, Size: dataSize, DataOffset: dataOff,
		}, nil
	}

	laceOff := br.r.tell()
	var raw []byte
	var frameCount, headerBytes int
	var frameSizes []int
	if br.skipsPayload(uint64(trackNum)) {
		// The lace header alone gives every frame's size and place: the frames are skipped, never read.
		if dataSize == 0 {
			return mkv.Block{}, fmt.Errorf("laced block missing lacing header byte")
		}
		count, err := br.r.ReadByte()
		if err != nil {
			return mkv.Block{}, err
		}
		frameCount = int(count) + 1
		if frameSizes, headerBytes, err = readLaceHeader(br.r, lacing, frameCount, dataSize-1); err != nil {
			return mkv.Block{}, fmt.Errorf("decode lacing: %w", err)
		}
		if err := br.r.discard(dataSize - 1 - int64(headerBytes)); err != nil {
			return mkv.Block{}, err
		}
	} else {
		raw = make([]byte, dataSize)
		if _, err := io.ReadFull(br.r, raw); err != nil {
			return mkv.Block{}, err
		}
		if len(raw) == 0 {
			return mkv.Block{}, fmt.Errorf("laced block missing lacing header byte")
		}
		frameCount = int(raw[0]) + 1 // Matroska lace count byte = number of frames minus 1
		raw = raw[1:]
		frameSizes, headerBytes, err = decodeLacingSizes(lacing, raw, frameCount)
		if err != nil {
			return mkv.Block{}, fmt.Errorf("decode lacing: %w", err)
		}
		if headerBytes < 0 || headerBytes > len(raw) {
			return mkv.Block{}, fmt.Errorf("laced block header (%d bytes) exceeds data (%d bytes)", headerBytes, len(raw))
		}
		raw = raw[headerBytes:]
	}
	framesLen := dataSize - 1 - int64(headerBytes)
	framesOff := laceOff + 1 + int64(headerBytes)

	tc, err := safeTimecodeMs(br.clusterTS+int64(relTC), br.timecodeScale)
	if err != nil {
		return mkv.Block{}, err
	}
	// The lace stores ONE timecode: frame i plays at tc + i×DefaultDuration
	// (rounded to the ms timeline). Without a known stride every frame keeps
	// the block timecode. The keyframe flag describes the whole block - "the
	// Block contains only keyframes" - so it applies to every laced frame
	// (lacing is used for audio, where each frame is independently decodable).
	durNs := br.trackDurNs[uint64(trackNum)]
	blocks := make([]mkv.Block, frameCount)
	offset := 0
	for i := 0; i < frameCount; i++ {
		end := offset + frameSizes[i]
		if int64(end) > framesLen {
			return mkv.Block{}, fmt.Errorf("laced frame %d overflows: need %d, have %d", i, end, framesLen)
		}
		tcI := tc
		if durNs > 0 && durNs <= maxLacedFrameDurNs {
			tcI = tc + (int64(i)*durNs+500_000)/1_000_000
		}
		blocks[i] = mkv.Block{
			TrackNumber: uint64(trackNum), Timecode: tcI, BlockTimecode: tc,
			Keyframe: keyframe, Size: int64(frameSizes[i]), Laced: frameCount > 1,
			DataOffset: framesOff + int64(offset),
		}
		if raw != nil {
			blocks[i].Data = append([]byte(nil), raw[offset:end]...)
		}
		offset = end
	}

	br.pending = blocks[1:]
	return blocks[0], nil
}

// maxClusterTimestamp is the largest Cluster Timestamp a block's signed 16-bit relative timecode adds to without
// leaving the int64 range.
const maxClusterTimestamp = math.MaxInt64 - math.MaxInt16

// pastEnd refuses a child whose body, starting at body, runs past its parent's end (none when end < 0): read on, it
// would swallow what follows the parent.
func pastEnd(h ebml.ElementHeader, body, end int64) error {
	if end < 0 || h.Size < 0 || body+h.Size <= end {
		return nil
	}
	return fmt.Errorf("element 0x%X at %d runs %d bytes past its parent's end", h.ID, body, body+h.Size-end)
}

func (br *BlockReader) parseBlockGroup(size int64) (mkv.Block, error) {
	start := br.r.tell()
	end := start + size
	var block mkv.Block
	var found, referenced bool
	var durationMs, paddingNs int64

	for br.r.tell() < end {
		h, _, err := ebml.ReadElementHeader(br.r)
		if err != nil {
			return mkv.Block{}, err
		}
		if err := pastEnd(h, br.r.tell(), end); err != nil {
			return mkv.Block{}, err
		}
		switch h.ID {
		case mkv.IDBlock:
			block, err = br.parseBlock(h.Size, false)
			if errors.Is(err, errFilteredBlock) {
				// The rest of the group (duration, additions) is moot too.
				if derr := br.r.discard(end - br.r.tell()); derr != nil {
					return mkv.Block{}, derr
				}
				return mkv.Block{}, errFilteredBlock
			}
			if err != nil {
				return mkv.Block{}, err
			}
			found = true
		case mkv.IDBlockDuration:
			raw, err := ebml.ReadUint(br.r, h.Size)
			if err != nil {
				return mkv.Block{}, err
			}
			durationMs, err = safeTimecodeMs(int64(raw), br.timecodeScale)
			if err != nil {
				return mkv.Block{}, err
			}
		case mkv.IDReferenceBlock:
			referenced = true
			if err := br.r.discard(h.Size); err != nil {
				return mkv.Block{}, err
			}
		case mkv.IDDiscardPadding:
			if h.Size > 8 {
				return mkv.Block{}, fmt.Errorf("DiscardPadding of %d bytes: a signed integer holds 8 at most", h.Size)
			}
			raw, err := ebml.ReadUint(br.r, h.Size)
			if err != nil {
				return mkv.Block{}, err
			}
			if shift := 64 - 8*h.Size; h.Size > 0 {
				paddingNs = int64(raw<<shift) >> shift // sign-extended
			}
		default:
			if err := br.r.discard(h.Size); err != nil {
				return mkv.Block{}, err
			}
		}
	}
	if !found {
		return mkv.Block{}, fmt.Errorf("BlockGroup without Block element")
	}
	block.Duration = durationMs
	if n := len(br.pending); n > 0 { // the padding trims the end of the lace's last frame
		br.pending[n-1].DiscardPaddingNs = paddingNs
	} else {
		block.DiscardPaddingNs = paddingNs
	}
	// A Block has no keyframe flag of its own (that bit is SimpleBlock's): in a
	// BlockGroup the frame is a keyframe exactly when the group names no
	// ReferenceBlock - it depends on no other frame. The frames a lace queued
	// behind the first one share the verdict.
	if !referenced {
		block.Keyframe = true
		for i := range br.pending {
			br.pending[i].Keyframe = true
		}
	}
	return block, nil
}

func safeTimecodeMs(v, scale int64) (int64, error) {
	if scale != 0 && (v > math.MaxInt64/scale || v < math.MinInt64/scale) {
		return 0, fmt.Errorf("timecode overflow: %d * %d", v, scale)
	}
	return v * scale / 1_000_000, nil
}

// ResolveClusterBlock locates a block by its cluster and its offset inside the
// cluster's data (a Cues entry's CueClusterPosition and CueRelativePosition):
// one read of the cluster header and its Timestamp gives the BlockPos a
// BlockReader seats on. relative 0 names the data start itself.
func ResolveClusterBlock(r io.ReadSeeker, clusterStart, relative int64) (BlockPos, error) {
	if _, err := r.Seek(clusterStart, io.SeekStart); err != nil {
		return BlockPos{}, err
	}
	var head [24]byte
	n, err := io.ReadFull(r, head[:])
	if err != nil && n < 12 {
		return BlockPos{}, fmt.Errorf("cluster header at %d: %w", clusterStart, err)
	}
	br := bytes.NewReader(head[:n])
	h, hdrLen, err := ebml.ReadElementHeader(br)
	if err != nil {
		return BlockPos{}, fmt.Errorf("cluster header at %d: %w", clusterStart, err)
	}
	if h.ID != mkv.IDCluster {
		return BlockPos{}, fmt.Errorf("no cluster at %d (element %#x)", clusterStart, h.ID)
	}
	dataStart := clusterStart + int64(hdrLen)
	pos := BlockPos{Off: dataStart + relative, ClusterStart: clusterStart, ClusterEnd: -1}
	if h.Size >= 0 {
		pos.ClusterEnd = dataStart + h.Size
		if relative > 0 && pos.Off >= pos.ClusterEnd {
			return BlockPos{}, fmt.Errorf("block offset %d lies past the cluster at %d", relative, clusterStart)
		}
	}
	th, thLen, err := ebml.ReadElementHeader(br)
	if err == nil && th.ID == mkv.IDTimestamp && th.Size > 0 && th.Size <= 8 && int64(hdrLen)+int64(thLen)+th.Size <= int64(n) {
		ts, err := ebml.ReadUint(br, th.Size)
		if err != nil {
			return BlockPos{}, err
		}
		pos.ClusterTS = int64(ts)
	}
	return pos, nil
}
