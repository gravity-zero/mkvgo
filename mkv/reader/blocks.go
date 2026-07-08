package reader

import (
	"encoding/binary"
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
	// converges to plain bulk sequential reads — never one read per block,
	// which is what a network filesystem punishes.
	minChunk = 16 << 10

	// seekSkipMin is the smallest beyond-window skip worth a real seek. Every
	// source read costs a fixed round trip on remote-ish filesystems (9p, SMB,
	// HTTP) on top of the bytes; seeking over a payload only pays when the
	// bytes saved outweigh the extra round trip the next small read costs —
	// measured around tens of KiB there, immaterial on local disks. Below the
	// threshold reading forward through the growing window is cheaper, and the
	// walk stays a bulk sequential read.
	seekSkipMin = 64 << 10
)

// errFilteredBlock is parseBlock's signal that the block belongs to a track
// outside the filter and its payload was skipped; Next keeps walking.
var errFilteredBlock = errors.New("block filtered")

// ErrClusterLimit is returned by Next when the walk reaches a cluster whose
// timestamp exceeds the StopBeforeClusterMs limit — before delivering any of
// that cluster's blocks. The walk can continue on the same reader after
// raising the limit, or be resumed later by a new reader at ResumeOffset.
var ErrClusterLimit = errors.New("cluster beyond the requested timecode limit")

// countingReader is a forward-only buffered reader with an adaptive window.
// It tracks the logical position by counting consumed bytes; the invariant is
// src's file offset == pos + (w - r), i.e. the source sits at the end of the
// window. Skips drop window bytes for free and Seek past anything beyond —
// bytes outside kept blocks are never read. The read-ahead starts small after
// a jump and doubles on every sequential fill (up to bufSize), so the reader
// behaves like a bulk sequential reader on dense data and like a sparse
// header-hopper on skippable data, whichever the stream turns out to be.
type countingReader struct {
	src   io.ReadSeeker
	buf   []byte // window storage, bufSize capacity
	r, w  int    // valid window is buf[r:w]; buf[r] is the byte at pos
	pos   int64  // logical position of the next byte the caller consumes
	end   int64  // source size, resolved on the first seek-skip; -1 until then
	chunk int    // current read-ahead size; grows sequentially, shrinks on jumps
}

func newCountingReader(src io.ReadSeeker, startPos int64) *countingReader {
	return &countingReader{
		src:   src,
		buf:   make([]byte, bufSize),
		pos:   startPos,
		end:   -1,
		chunk: minChunk,
	}
}

// growChunk doubles the read-ahead after a sequential fill, up to bufSize.
func (c *countingReader) growChunk() {
	if c.chunk < len(c.buf) {
		c.chunk *= 2
		if c.chunk > len(c.buf) {
			c.chunk = len(c.buf)
		}
	}
}

// fill loads the next chunk into the (empty) window.
func (c *countingReader) fill() error {
	n, err := c.src.Read(c.buf[:c.chunk])
	if n == 0 {
		if err == nil {
			err = io.ErrNoProgress
		}
		return err
	}
	c.growChunk()
	c.r, c.w = 0, n
	return nil
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.r == c.w {
		if len(p) >= c.chunk {
			// Large read with an empty window: straight into p.
			n, err := c.src.Read(p)
			c.pos += int64(n)
			c.growChunk()
			return n, err
		}
		if err := c.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.buf[c.r:c.w])
	c.r += n
	c.pos += int64(n)
	return n, nil
}

// discard advances the reader by exactly n bytes without delivering them.
// The window's bytes are dropped for free. A remainder past the window is
// seeked over — never read — when it is large enough to beat the fixed
// round-trip cost of the next read (seekSkipMin); smaller remainders are read
// forward through the growing window, keeping the walk a bulk sequential
// read. A skip beyond the source's end errors like a truncated read would.
// A seek resets the window growth: the walk has proven sparse, keep the next
// reads small.
func (c *countingReader) discard(n int64) error {
	if n <= 0 {
		return nil
	}
	avail := int64(c.w - c.r)
	if n <= avail {
		c.r += int(n)
		c.pos += n
		return nil
	}
	if n-avail <= seekSkipMin {
		_, err := io.CopyN(io.Discard, c, n)
		return err
	}
	n -= avail
	c.pos += avail
	c.r, c.w = 0, 0
	// The window is empty, so src's offset == c.pos: seek forward from there.
	if c.end < 0 {
		end, err := c.src.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		c.end = end
	}
	target := c.pos + n
	if target > c.end {
		return io.ErrUnexpectedEOF
	}
	if _, err := c.src.Seek(target, io.SeekStart); err != nil {
		return err
	}
	c.pos = target
	c.chunk = minChunk
	return nil
}

// tell returns the current byte position (offset from file start).
func (c *countingReader) tell() int64 {
	return c.pos
}

// peekedHeader is a pre-read element header kept for one iteration of Next().
type peekedHeader struct {
	h     ebml.ElementHeader
	start int64 // absolute offset of the header's first byte
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
	raw           io.ReadSeeker // kept for interface compatibility; nil for stream readers
	segEnd        int64
	clusterEnd    int64
	inCluster     bool // true when inside a cluster (including unknown-size)
	clusterTS     int64
	timecodeScale int64
	pending       []mkv.Block
	keep          map[uint64]bool // when non-nil, blocks of other tracks are skipped unread
	// headerOnly, when set, discards the payload of an unlaced kept block
	// instead of reading it: Block.Data stays nil and Block.Size reports the
	// byte length alone. A laced block still needs its lacing header decoded
	// to size its frames, so it is read normally and the bytes dropped right
	// after (real muxers do not lace video, the track this mode targets).
	headerOnly bool
	// trackDurNs maps track number → DefaultDuration (ns), the per-frame stride
	// that times the frames of a laced block (they share one stored timecode).
	// Filled by SetTrackDefaultDurations, or opportunistically from the Tracks
	// element when the sequential walk passes over it.
	trackDurNs    map[uint64]int64
	peeked        *peekedHeader // element read in peek-ahead, to be processed next iteration
	stopMs        int64         // StopBeforeClusterMs limit (only when hasStop)
	hasStop       bool          // a cluster-timecode limit is set
	awaitLimit    bool          // stopped at a cluster beyond stopMs; recheck on Next
	clusterStart  int64         // absolute offset of the current cluster's header
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
// offset — the absolute file offset of a Cluster element (e.g. a CuePoint's
// Container.SegmentStart + ClusterPos) — and blocks are read from that cluster
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

// StopBeforeClusterMs bounds the walk: Next returns ErrClusterLimit instead
// of entering a cluster whose timestamp exceeds ms — even when a track filter
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

// KeepTracks restricts Next to blocks of the given tracks. Other tracks'
// payloads are never delivered and, when they are large enough to seek over,
// never read: the walk hops from header to header. When the payloads are
// smaller than the adaptive read-ahead (frame-interleaved video is a few KiB
// per block), the walk degrades gracefully to bulk sequential reads — the
// same I/O as a plain full pass, never worse.
func (br *BlockReader) KeepTracks(tracks ...uint64) {
	br.keep = make(map[uint64]bool, len(tracks))
	for _, id := range tracks {
		br.keep[id] = true
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
				v, err := ebml.ReadUint(br.r, eh.Size)
				if err != nil {
					return err
				}
				num = v
			case mkv.IDDefaultDuration:
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
		if num > 0 && durNs > 0 && durNs <= maxLacedFrameDurNs {
			if br.trackDurNs == nil {
				br.trackDurNs = make(map[uint64]int64)
			}
			br.trackDurNs[num] = durNs
		}
	}
	return nil
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

	if h2.Size >= 0 {
		br.segEnd = startPos + h2.Size
	}
	return nil
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

func (br *BlockReader) Next() (mkv.Block, error) {
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

			// Read next element header (or use peeked one).
			var h ebml.ElementHeader
			var hdrStart int64
			if br.peeked != nil {
				h = br.peeked.h
				hdrStart = br.peeked.start
				br.peeked = nil
			} else {
				hdrStart = br.r.tell()
				var err error
				h, _, err = ebml.ReadElementHeader(br.r)
				if err != nil {
					if errors.Is(err, io.EOF) {
						return mkv.Block{}, io.EOF
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

			switch h.ID {
			case mkv.IDTimestamp:
				v, err := ebml.ReadUint(br.r, h.Size)
				if err != nil {
					return mkv.Block{}, err
				}
				br.clusterTS = int64(v)
				if tsMs, err := safeTimecodeMs(br.clusterTS, br.timecodeScale); br.hasStop && err == nil && tsMs > br.stopMs {
					br.awaitLimit = true
					return mkv.Block{}, ErrClusterLimit
				}
				continue
			case mkv.IDSimpleBlock:
				b, err := br.parseBlock(h.Size, true)
				if errors.Is(err, errFilteredBlock) {
					continue
				}
				return b, err
			case mkv.IDBlockGroup:
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
			br.peeked = nil
		} else {
			hdrStart = br.r.tell()
			var err error
			h, _, err = ebml.ReadElementHeader(br.r)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return mkv.Block{}, io.EOF
				}
				return mkv.Block{}, err
			}
		}

		if h.ID == mkv.IDCluster {
			br.clusterStart = hdrStart
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
		if h.ID == mkv.IDTracks && br.trackDurNs == nil && h.Size <= 16<<20 {
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
	start := br.r.tell()

	trackNum, _, err := ebml.ReadDataSize(br.r)
	if err != nil {
		return mkv.Block{}, err
	}
	if br.keep != nil && !br.keep[uint64(trackNum)] {
		if err := br.r.discard(size - (br.r.tell() - start)); err != nil {
			return mkv.Block{}, err
		}
		return mkv.Block{}, errFilteredBlock
	}

	var tcBuf [2]byte
	if _, err := io.ReadFull(br.r, tcBuf[:]); err != nil {
		return mkv.Block{}, err
	}
	relTC := int16(binary.BigEndian.Uint16(tcBuf[:]))

	var flagsBuf [1]byte
	if _, err := io.ReadFull(br.r, flagsBuf[:]); err != nil {
		return mkv.Block{}, err
	}
	flags := flagsBuf[0]
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
		if br.headerOnly {
			if err := br.r.discard(dataSize); err != nil {
				return mkv.Block{}, err
			}
			return mkv.Block{
				TrackNumber: uint64(trackNum), Timecode: tc, BlockTimecode: tc,
				Keyframe: keyframe, Size: dataSize,
			}, nil
		}
		data := make([]byte, dataSize)
		if _, err := io.ReadFull(br.r, data); err != nil {
			return mkv.Block{}, err
		}
		return mkv.Block{
			TrackNumber: uint64(trackNum), Timecode: tc, BlockTimecode: tc,
			Keyframe: keyframe, Data: data, Size: dataSize,
		}, nil
	}

	raw := make([]byte, dataSize)
	if _, err := io.ReadFull(br.r, raw); err != nil {
		return mkv.Block{}, err
	}

	if len(raw) == 0 {
		return mkv.Block{}, fmt.Errorf("laced block missing lacing header byte")
	}
	frameCount := int(raw[0]) + 1 // Matroska lace count byte = number of frames minus 1
	raw = raw[1:]

	frameSizes, headerBytes, err := decodeLacingSizes(lacing, raw, frameCount)
	if err != nil {
		return mkv.Block{}, fmt.Errorf("decode lacing: %w", err)
	}
	if headerBytes < 0 || headerBytes > len(raw) {
		return mkv.Block{}, fmt.Errorf("laced block header (%d bytes) exceeds data (%d bytes)", headerBytes, len(raw))
	}
	raw = raw[headerBytes:]

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
		if end > len(raw) {
			return mkv.Block{}, fmt.Errorf("laced frame %d overflows: need %d, have %d", i, end, len(raw))
		}
		tcI := tc
		if durNs > 0 && durNs <= maxLacedFrameDurNs {
			tcI = tc + (int64(i)*durNs+500_000)/1_000_000
		}
		blocks[i] = mkv.Block{
			TrackNumber: uint64(trackNum), Timecode: tcI, BlockTimecode: tc,
			Keyframe: keyframe, Size: int64(frameSizes[i]),
		}
		if !br.headerOnly {
			// A laced block's frames still needed the payload read to decode
			// their sizes (the lacing header sits ahead of them): header-only
			// mode still transiently held it above, but drops the bytes here
			// rather than keeping them - real muxers do not lace video, the
			// only track this mode targets, so this path is not the hot one.
			blocks[i].Data = append([]byte(nil), raw[offset:end]...)
		}
		offset = end
	}

	br.pending = blocks[1:]
	return blocks[0], nil
}

func (br *BlockReader) parseBlockGroup(size int64) (mkv.Block, error) {
	start := br.r.tell()
	end := start + size
	var block mkv.Block
	var found bool
	var durationMs int64

	for br.r.tell() < end {
		h, _, err := ebml.ReadElementHeader(br.r)
		if err != nil {
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
	return block, nil
}

// decodeLacingSizes reads the per-frame sizes from a laced block's lacing
// header and returns them with the exact number of header bytes consumed -
// parseBlock slices the frame data right after them. Returning the consumed
// length (instead of re-deriving it from the sizes) stays correct even when a
// muxer encoded a size or diff in a wider-than-minimal VINT.
func decodeLacingSizes(lacing byte, raw []byte, frameCount int) (sizes []int, headerLen int, err error) {
	sizes = make([]int, frameCount)
	switch lacing {
	case lacingXiph:
		pos := 0
		total := 0
		for i := 0; i < frameCount-1; i++ {
			sz := 0
			for pos < len(raw) {
				val := raw[pos]
				pos++
				sz += int(val)
				if val < 255 {
					break
				}
			}
			sizes[i] = sz
			total += sz
		}
		last := len(raw) - pos - total
		if last < 0 {
			return nil, 0, fmt.Errorf("xiph lacing: total %d exceeds data %d", total, len(raw)-pos)
		}
		sizes[frameCount-1] = last
		return sizes, pos, nil
	case lacingFixed:
		if len(raw)%frameCount != 0 {
			return nil, 0, fmt.Errorf("fixed lacing: %d data bytes not divisible by %d frames", len(raw), frameCount)
		}
		sz := len(raw) / frameCount
		for i := range sizes {
			sizes[i] = sz
		}
		return sizes, 0, nil
	case lacingEBML:
		pos := 0
		first, width := readVINTFromBuf(raw[pos:])
		if width == 0 {
			return nil, 0, fmt.Errorf("ebml lacing: invalid first-size vint")
		}
		firstSize := int(first & ^(uint64(1) << uint(width*7)))
		sizes[0] = firstSize
		pos += width
		total := firstSize
		for i := 1; i < frameCount-1; i++ {
			if pos > len(raw) {
				return nil, 0, fmt.Errorf("ebml lacing: header truncated at frame %d", i)
			}
			val, w := readVINTFromBuf(raw[pos:])
			if w == 0 {
				return nil, 0, fmt.Errorf("ebml lacing: invalid size vint at frame %d", i)
			}
			pos += w
			// The diff is a signed VINT: value − (2^(7·w−1) − 1), per RFC 9559.
			// (An off-by-one bias - 2^(7·w-1) - shifted every frame boundary by
			// the frame index and corrupted all EBML-laced audio while keeping
			// the block's TOTAL intact, so only a decoder noticed.)
			dataBits := uint(w * 7)
			bias := int64(1)<<(dataBits-1) - 1
			stripped := int64(val & ^(uint64(1) << dataBits))
			diff := stripped - bias
			sizes[i] = sizes[i-1] + int(diff)
			if sizes[i] < 0 {
				return nil, 0, fmt.Errorf("ebml lacing: negative frame size at index %d", i)
			}
			total += sizes[i]
		}
		last := len(raw) - pos - total
		if last < 0 {
			return nil, 0, fmt.Errorf("ebml lacing: total %d exceeds data %d", total, len(raw)-pos)
		}
		sizes[frameCount-1] = last
		return sizes, pos, nil
	}
	return nil, 0, fmt.Errorf("unknown lacing type %d", lacing)
}

func readVINTFromBuf(buf []byte) (uint64, int) {
	if len(buf) == 0 {
		return 0, 0
	}
	b := buf[0]
	width := 1
	for i := 7; i >= 0; i-- {
		if b&(1<<uint(i)) != 0 {
			width = 8 - i
			break
		}
	}
	val := uint64(b)
	for i := 1; i < width && i < len(buf); i++ {
		val = (val << 8) | uint64(buf[i])
	}
	return val, width
}

func safeTimecodeMs(v, scale int64) (int64, error) {
	if scale != 0 && (v > math.MaxInt64/scale || v < math.MinInt64/scale) {
		return 0, fmt.Errorf("timecode overflow: %d * %d", v, scale)
	}
	return v * scale / 1_000_000, nil
}
