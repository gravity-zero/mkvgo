package ops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// reindex_inplace.go - surgical in-place reindex: ReindexInPlace patches the
// Cues index directly into the source file (append the new Cues, extend the
// Segment size, repoint the head SeekHead, void any stale Cues) instead of
// copying every cluster to a new file the way Reindex does. Cluster bytes are
// never touched. A crash-safety journal, appended past the current end of
// file and truncated away on success, makes the patch recoverable if the
// process dies mid-operation (see RecoverInPlace).

const (
	// maxInplaceJournal caps the journal payload so a crafted/corrupt trailer
	// cannot make readInPlaceJournal allocate an unbounded buffer.
	maxInplaceJournal = 16 << 20 // 16 MiB
	// inplaceTrailerLen is the fixed-size trailer: payloadLen(4) + crc32(4) + magic(16).
	inplaceTrailerLen = 24
)

// inplaceJournalMagic marks the trailer of an in-place journal. Exactly 16 bytes.
var inplaceJournalMagic = [16]byte{'m', 'k', 'v', 'g', 'o', '.', 'j', 'r', 'n', 'l', '.', 'v', '1', 0, 0, 0}

// ReindexInPlace rebuilds the Cues index of the Matroska/WebM file at path by
// patching the file itself: it appends a fresh Cues element at the end of the
// Segment, extends the Segment size to cover it, points the head SeekHead at
// the new Cues, and voids any stale Cues element. Cluster bytes are never
// moved or rewritten, and no second copy of the file is ever created on disk.
//
// The operation needs write access to path only (no temp file, no second
// path). It is crash-safe: every byte it is about to overwrite is captured
// into a small journal appended past the end of the file before any patch is
// applied; on success that journal is truncated away, so a finished file
// carries no trace of it. If the process dies mid-operation, RecoverInPlace
// (or the auto-recovery this function itself runs first) restores the
// original bytes from that journal. Once ReindexInPlace has returned
// successfully there is no going back: the journal no longer exists.
//
// Streamed (unknown-size) clusters and truncated files are refused outright,
// with a message pointing at Reindex (which copies to a new file and can
// therefore fall back to a full rewrite); a file with neither a head SeekHead
// nor a Void large enough to hold the rebuilt SeekHead is refused the same way.
func ReindexInPlace(ctx context.Context, path string, opts ...mkv.Options) error {
	fs := mkv.FSFrom(opts)
	deep := mkv.DeepVerifyFrom(opts)
	progress := mkv.ProgressFrom(opts)

	if mkv.ResyncFrom(opts) {
		return fmt.Errorf("reindex inplace: Options.Resync is not supported in place (skipped corrupt bytes cannot be dropped from the file itself); use Reindex or ReindexReplace")
	}

	f, err := fs.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("reindex inplace: open %s: %w", path, err)
	}
	defer f.Close()

	trunc, ok := f.(interface{ Truncate(size int64) error })
	if !ok {
		return fmt.Errorf("reindex inplace: file handle for %s does not support Truncate, cannot patch in place", path)
	}
	sync := func() error {
		if s, ok := f.(interface{ Sync() error }); ok {
			return s.Sync()
		}
		return nil
	}

	stat, err := fs.DoStat(path)
	if err != nil {
		return fmt.Errorf("reindex inplace: stat %s: %w", path, err)
	}
	size := stat.Size()

	// Auto-recovery: a previous run may have crashed after landing its journal
	// but before removing it. Roll that back before touching anything else.
	journal, err := readInPlaceJournal(f, size)
	if err != nil {
		return fmt.Errorf("reindex inplace: %w", err)
	}
	if journal != nil {
		if err := rollbackInPlaceJournal(f, journal); err != nil {
			return fmt.Errorf("reindex inplace: auto-recovery: %w", err)
		}
		stat, err = fs.DoStat(path)
		if err != nil {
			return fmt.Errorf("reindex inplace: re-stat after auto-recovery: %w", err)
		}
		size = stat.Size()
	}

	// Head-only metadata: which track IDs are video, and the timecode scale
	// cue times are stored in.
	meta, err := reader.OpenMetaWithFS(ctx, path, fs)
	if err != nil {
		return fmt.Errorf("reindex inplace: read metadata: %w", err)
	}
	videoTracks := videoTrackSet(meta.Tracks)
	timecodeScale := meta.Info.TimecodeScale
	if timecodeScale == 0 {
		timecodeScale = 1_000_000
	}

	scan, err := scanInPlace(ctx, path, fs, timecodeScale, videoTracks, progress, size)
	if err != nil {
		return err
	}
	if len(scan.cues) == 0 {
		return fmt.Errorf("reindex inplace: no cueable keyframes found")
	}

	// Build the patch set in memory; nothing is written until this is done.
	patch, err := buildInPlacePatch(scan, size, timecodeScale)
	if err != nil {
		return err
	}
	cuesBytes := patch.cuesBytes
	segSizePatch := patch.segSizePatch
	slotOff, slotPatch := patch.slotOff, patch.slotPatch
	oldCuesHdrs := patch.oldCuesHdrs

	// Capture the ORIGINAL bytes of every region about to be overwritten.
	readOrig := func(off int64, n int) ([]byte, error) {
		buf := make([]byte, n)
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	segSizeOrig, err := readOrig(scan.segSizeOff, scan.segSizeLen)
	if err != nil {
		return fmt.Errorf("reindex inplace: read original segment-size bytes: %w", err)
	}
	slotOrig, err := readOrig(slotOff, len(slotPatch))
	if err != nil {
		return fmt.Errorf("reindex inplace: read original SeekHead-slot bytes: %w", err)
	}
	zones := []inplaceZone{
		{off: scan.segSizeOff, orig: segSizeOrig},
		{off: slotOff, orig: slotOrig},
	}
	for i, oc := range scan.oldCues {
		orig, err := readOrig(oc.off, len(oldCuesHdrs[i]))
		if err != nil {
			return fmt.Errorf("reindex inplace: read original old-Cues bytes: %w", err)
		}
		zones = append(zones, inplaceZone{off: oc.off, orig: orig})
	}

	journalNow := &inplaceJournal{origSize: size, zones: zones}
	rollback := func(cause error) error {
		if rbErr := rollbackInPlaceJournal(f, journalNow); rbErr != nil {
			return fmt.Errorf("reindex inplace: %w; rollback ALSO failed (%v), the file is left mid-operation with its journal still attached, run RecoverInPlace", cause, rbErr)
		}
		return fmt.Errorf("reindex inplace: %w (original file restored)", cause)
	}

	// 1. Land the crash-safety journal past the current end of file
	// The Segment size is not extended yet, so no reader can see cuesBytes or
	// the journal that follows it.
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		return rollback(fmt.Errorf("seek to end of file: %w", err))
	}
	if _, err := f.Write(cuesBytes); err != nil {
		return rollback(fmt.Errorf("append cues: %w", err))
	}
	if err := writeInPlaceJournal(f, size, zones); err != nil {
		return rollback(fmt.Errorf("write journal: %w", err))
	}
	if err := sync(); err != nil {
		return rollback(fmt.Errorf("sync after journal: %w", err))
	}

	// 2. Apply the patches
	applyPatch := func(off int64, data []byte) error {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return err
		}
		_, err := f.Write(data)
		return err
	}
	if err := applyPatch(scan.segSizeOff, segSizePatch); err != nil {
		return rollback(fmt.Errorf("apply segment-size patch: %w", err))
	}
	if err := applyPatch(slotOff, slotPatch); err != nil {
		return rollback(fmt.Errorf("apply SeekHead-slot patch: %w", err))
	}
	for i, oc := range scan.oldCues {
		if err := applyPatch(oc.off, oldCuesHdrs[i]); err != nil {
			return rollback(fmt.Errorf("apply old-Cues void patch: %w", err))
		}
	}
	if err := sync(); err != nil {
		return rollback(fmt.Errorf("sync after patches: %w", err))
	}

	// 3. Always-on light verify
	if err := verifyReindexedCues(ctx, path, fs, scan.cues, timecodeScale); err != nil {
		return rollback(err)
	}

	// 4. Optional deep verify
	if deep {
		if err := deepVerifyValidate(ctx, path, fs); err != nil {
			return rollback(err)
		}
	}

	// 6. Success: drop the journal
	if err := trunc.Truncate(size + int64(len(cuesBytes))); err != nil {
		return fmt.Errorf("reindex inplace: truncate away journal: %w", err)
	}
	if err := sync(); err != nil {
		return fmt.Errorf("reindex inplace: final sync: %w", err)
	}
	return nil
}

// RecoverInPlace rolls back an interrupted ReindexInPlace run: if path carries
// an in-file journal (the process died after landing it but before the final
// truncate removed it), the original bytes are restored and the journal is
// truncated away. It is a no-op, returning (false, nil), when path carries no
// journal. ReindexInPlace already runs this same recovery on its own before
// doing anything else, so calling RecoverInPlace directly is only needed to
// clean up a file without immediately reindexing it again.
func RecoverInPlace(ctx context.Context, path string, opts ...mkv.Options) (recovered bool, err error) {
	fs := mkv.FSFrom(opts)

	f, err := fs.DoOpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("recover inplace: open %s: %w", path, err)
	}
	defer f.Close()

	if _, ok := f.(interface{ Truncate(size int64) error }); !ok {
		return false, fmt.Errorf("recover inplace: file handle for %s does not support Truncate, cannot recover in place", path)
	}

	stat, err := fs.DoStat(path)
	if err != nil {
		return false, fmt.Errorf("recover inplace: stat %s: %w", path, err)
	}

	journal, err := readInPlaceJournal(f, stat.Size())
	if err != nil {
		return false, fmt.Errorf("recover inplace: %w", err)
	}
	if journal == nil {
		return false, nil
	}
	if err := rollbackInPlaceJournal(f, journal); err != nil {
		return false, fmt.Errorf("recover inplace: %w", err)
	}
	_ = ctx // no cancellation point in a bounded, in-memory rollback
	return true, nil
}

// inplacePatch is the set of in-place edits ReindexInPlace computes before it
// touches the file: the new Cues bytes to append, and three overwrite patches
// (the Segment size, the head SeekHead slot, and a Void header over each stale
// Cues element). It is pure - computed from the scan, no I/O - so the write
// orchestration (journal, apply, verify, rollback) stays separate.
type inplacePatch struct {
	cuesBytes    []byte   // appended at the old EOF
	segSizePatch []byte   // written at scan.segSizeOff (same VINT width)
	slotOff      int64    // where slotPatch goes (head SeekHead or a head Void)
	slotPatch    []byte   // rebuilt SeekHead + any trailing Void header
	oldCuesHdrs  [][]byte // parallel to scan.oldCues; a Void header per stale Cues
}

// buildInPlacePatch computes the in-place edit set from a completed scan. Any
// layout that cannot be patched in place (a size that will not fit the Segment
// size VINT, a head SeekHead slot too small, no SeekHead or Void to hold the
// rebuilt SeekHead) returns an error pointing at the copy reindex.
func buildInPlacePatch(scan *inplaceScan, size, timecodeScale int64) (*inplacePatch, error) {
	var cuesBuf bytes.Buffer
	if err := writer.WriteCues(&cuesBuf, scan.cues, timecodeScale); err != nil {
		return nil, fmt.Errorf("reindex inplace: encode cues: %w", err)
	}
	cuesBytes := cuesBuf.Bytes()

	newCuesPosRel := size - scan.segDataStart
	newSegSize := size + int64(len(cuesBytes)) - scan.segDataStart

	segSizePatch, err := encodeDataSizeFixed(newSegSize, scan.segSizeLen)
	if err != nil {
		return nil, fmt.Errorf("reindex inplace: %w, use mkvgo reindex instead", err)
	}

	// New head SeekHead: keep every preserved entry that does not point at the
	// Cues element type, does not land inside a stale Cues span, and stays
	// within the new Segment bounds; then append the fresh Cues entry.
	var newEntries []writer.SeekEntry
	for _, e := range scan.seekEntries {
		if e.ID == mkv.IDCues {
			continue
		}
		abs := scan.segDataStart + e.Pos
		stale := false
		for _, oc := range scan.oldCues {
			if abs >= oc.off && abs < oc.off+oc.len {
				stale = true
				break
			}
		}
		if stale || e.Pos < 0 || e.Pos >= newSegSize {
			continue
		}
		newEntries = append(newEntries, e)
	}
	newEntries = append(newEntries, writer.SeekEntry{ID: mkv.IDCues, Pos: newCuesPosRel})

	var shBuf bytes.Buffer
	if err := writer.WriteSeekHead(&shBuf, newEntries); err != nil {
		return nil, fmt.Errorf("reindex inplace: encode SeekHead: %w", err)
	}
	shBytes := shBuf.Bytes()

	// Slot fit: the existing head SeekHead span, or (when there is none) the
	// first head Void that is big enough.
	var slotOff, slotLen int64
	if scan.seekHeadSlot != nil {
		slotOff, slotLen = scan.seekHeadSlot.off, scan.seekHeadSlot.len
		if r := slotLen - int64(len(shBytes)); r != 0 && r < 2 {
			return nil, fmt.Errorf("reindex inplace: head SeekHead too small for the Cues entry, use mkvgo reindex")
		}
	} else {
		found := false
		for _, cand := range scan.voidCandidates {
			if r := cand.len - int64(len(shBytes)); r == 0 || r >= 2 {
				slotOff, slotLen = cand.off, cand.len
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("reindex inplace: no head SeekHead or Void space, use mkvgo reindex")
		}
	}
	remainder := slotLen - int64(len(shBytes))

	slotPatch := append([]byte(nil), shBytes...)
	if remainder >= 2 {
		voidHdr, err := voidHeaderBytes(int(remainder))
		if err != nil {
			return nil, fmt.Errorf("reindex inplace: %w", err)
		}
		slotPatch = append(slotPatch, voidHdr...)
	}

	// Stale Cues spans become a Void header only: the old cue bytes underneath
	// stay put as the Void's ignored payload, so the journal entry for this
	// zone is a few bytes, not the whole span.
	oldCuesHdrs := make([][]byte, len(scan.oldCues))
	for i, oc := range scan.oldCues {
		hdr, err := voidHeaderBytes(int(oc.len))
		if err != nil {
			return nil, fmt.Errorf("reindex inplace: %w", err)
		}
		oldCuesHdrs[i] = hdr
	}

	return &inplacePatch{
		cuesBytes:    cuesBytes,
		segSizePatch: segSizePatch,
		slotOff:      slotOff,
		slotPatch:    slotPatch,
		oldCuesHdrs:  oldCuesHdrs,
	}, nil
}

// Read-only scan pass
// inplaceSpan is a byte range within the file (element header + body).
type inplaceSpan struct {
	off int64
	len int64
}

// inplaceScan is everything ReindexInPlace's patch-building step needs,
// gathered by a single sequential pass over the source Segment.
type inplaceScan struct {
	segSizeOff   int64 // absolute offset of the Segment size VINT
	segSizeLen   int   // encoded width (bytes) of that VINT
	segDataStart int64 // absolute offset where Segment data begins

	cues []mkv.CuePoint

	seekHeadSlot   *inplaceSpan       // the first head SeekHead's span, extended by any contiguous trailing Void
	seekEntries    []writer.SeekEntry // parsed from that SeekHead's body
	voidCandidates []inplaceSpan      // standalone head Voids, for files with no SeekHead
	oldCues        []inplaceSpan      // every Cues element found (current or stale, all get voided)
}

// scanInPlace walks path's Segment sequentially (its own read-only handle,
// buffered) building an inplaceScan: cue points from every cluster, the head
// SeekHead's span and entries (or head Void candidates when there is none),
// and every Cues element's span. Mirrors the walk in Reindex, but records
// positions instead of copying bytes.
func scanInPlace(ctx context.Context, path string, fs *mkv.FS, timecodeScale int64, videoTracks map[uint64]bool, progress mkv.ProgressFunc, totalBytes int64) (*inplaceScan, error) {
	raw, err := fs.DoOpen(path)
	if err != nil {
		return nil, fmt.Errorf("reindex inplace: open scan: %w", err)
	}
	defer raw.Close()
	r := bufio.NewReaderSize(raw, reindexBufSize)

	ebmlHdr, ebmlHdrBytes, err := ebml.ReadElementHeader(r)
	if err != nil {
		return nil, inplaceReadErr("EBML header", err)
	}
	if ebmlHdr.ID != ebml.IDEBMLHeader {
		return nil, fmt.Errorf("reindex inplace: expected EBML header, got 0x%X", ebmlHdr.ID)
	}
	if ebmlHdr.Size < 0 {
		return nil, fmt.Errorf("reindex inplace: EBML header has unknown size")
	}
	if _, err := io.CopyN(io.Discard, r, ebmlHdr.Size); err != nil {
		return nil, inplaceReadErr("skip EBML header body", err)
	}
	pos := int64(ebmlHdrBytes) + ebmlHdr.Size

	segIDLen := ebml.ElementIDLen(mkv.IDSegment)
	segHdr, segHdrBytes, err := ebml.ReadElementHeader(r)
	if err != nil {
		return nil, inplaceReadErr("Segment header", err)
	}
	if segHdr.ID != mkv.IDSegment {
		return nil, fmt.Errorf("reindex inplace: expected Segment, got 0x%X", segHdr.ID)
	}

	scan := &inplaceScan{
		segSizeOff:   pos + int64(segIDLen),
		segSizeLen:   segHdrBytes - segIDLen,
		segDataStart: pos + int64(segHdrBytes),
	}
	pos = scan.segDataStart

	var firstClusterSeen bool
	var clusterBuf []byte // reused across clusters, grown as needed (mirrors reindexFastCopy)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		h, hdrBytes, err := ebml.ReadElementHeader(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break // clean end at an element boundary
			}
			return nil, inplaceReadErr("top-level element", err)
		}
		elemOff := pos
		pos += int64(hdrBytes)

		switch h.ID {
		case mkv.IDCluster:
			if h.Size < 0 {
				return nil, fmt.Errorf("reindex inplace: unknown-size cluster (streamed file), use mkvgo reindex")
			}
			if h.Size > maxReindexClusterSize {
				return nil, fmt.Errorf("reindex inplace: cluster size %d exceeds limit (%d)", h.Size, maxReindexClusterSize)
			}
			firstClusterSeen = true
			if int(h.Size) > len(clusterBuf) {
				clusterBuf = make([]byte, h.Size)
			}
			body := clusterBuf[:h.Size]
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, inplaceReadErr("read cluster body", err)
			}
			appendCueFromCluster(&scan.cues, body, timecodeScale, elemOff-scan.segDataStart, videoTracks)
			pos += h.Size
			if progress != nil && totalBytes > 0 {
				progress(pos, totalBytes)
			}

		case mkv.IDSeekHead:
			if h.Size < 0 {
				return nil, fmt.Errorf("reindex inplace: unknown-size SeekHead")
			}
			body := make([]byte, h.Size)
			if _, err := io.ReadFull(r, body); err != nil {
				return nil, inplaceReadErr("read SeekHead body", err)
			}
			pos += h.Size
			if scan.seekHeadSlot == nil {
				scan.seekHeadSlot = &inplaceSpan{off: elemOff, len: int64(hdrBytes) + h.Size}
				scan.seekEntries = parseSeekHeadBody(body)
			}

		case mkv.IDVoid:
			if h.Size < 0 {
				return nil, fmt.Errorf("reindex inplace: unknown-size Void")
			}
			voidLen := int64(hdrBytes) + h.Size
			switch {
			case scan.seekHeadSlot != nil && elemOff == scan.seekHeadSlot.off+scan.seekHeadSlot.len:
				scan.seekHeadSlot.len += voidLen
			case !firstClusterSeen:
				scan.voidCandidates = append(scan.voidCandidates, inplaceSpan{off: elemOff, len: voidLen})
			}
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				return nil, inplaceReadErr("skip Void", err)
			}
			pos += h.Size

		case mkv.IDCues:
			if h.Size < 0 {
				return nil, fmt.Errorf("reindex inplace: unknown-size Cues")
			}
			scan.oldCues = append(scan.oldCues, inplaceSpan{off: elemOff, len: int64(hdrBytes) + h.Size})
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				return nil, inplaceReadErr("skip Cues", err)
			}
			pos += h.Size

		default:
			if h.Size < 0 {
				return nil, fmt.Errorf("reindex inplace: unknown-size non-cluster element 0x%X", h.ID)
			}
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				return nil, inplaceReadErr(fmt.Sprintf("skip 0x%X", h.ID), err)
			}
			pos += h.Size
		}
	}

	return scan, nil
}

// inplaceReadErr wraps a read failure encountered while scanning. A genuine
// EOF partway through an element means the file is truncated: point the
// caller at Reindex, which copies to a new file and can fall back to a full
// rewrite. Any other error is reported as-is.
func inplaceReadErr(context string, err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return fmt.Errorf("reindex inplace: truncated file (%s), use mkvgo reindex instead: %w", context, err)
	}
	return fmt.Errorf("reindex inplace: %s: %w", context, err)
}

// Patch-set arithmetic
// encodeDataSizeFixed encodes size as an EBML data-size VINT of exactly width
// bytes, so patching it in place never shifts any byte that follows it.
// Returns an error when size does not fit in width bytes.
func encodeDataSizeFixed(size int64, width int) ([]byte, error) {
	if width < 1 || width > 8 {
		return nil, fmt.Errorf("encodeDataSizeFixed: invalid width %d", width)
	}
	max := int64(1)<<uint(7*width) - 2
	if size < 0 || size > max {
		return nil, fmt.Errorf("size %d does not fit a %d-byte VINT (max %d)", size, width, max)
	}
	val := uint64(size) | (uint64(1) << uint(7*width))
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = byte(val)
		val >>= 8
	}
	return buf, nil
}

// voidHeaderBytes returns the encoded Void element header (ID + size VINT)
// such that header length + declared size equals totalSize, mirroring
// writer.WriteVoid's arithmetic. Unlike WriteVoid, it does not also emit the
// zero-filled payload: the caller leaves whatever bytes were already there in
// place, so the patch (and its journal entry) is proportional to the header
// alone rather than to the whole voided span.
func voidHeaderBytes(totalSize int) ([]byte, error) {
	if totalSize < 2 {
		return nil, fmt.Errorf("void slot too small (%d bytes, need at least 2)", totalSize)
	}
	headerSize := 1 + ebml.DataSizeLen(int64(totalSize-1-ebml.DataSizeLen(int64(totalSize-2))))
	padLen := totalSize - headerSize
	if padLen < 0 {
		padLen = 0
	}
	var buf bytes.Buffer
	if _, err := ebml.WriteElementHeader(&buf, mkv.IDVoid, int64(padLen)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Crash-safety journal
// inplaceZone is one region of the file whose original bytes were captured
// before being overwritten, so a rollback can restore them verbatim.
type inplaceZone struct {
	off  int64
	orig []byte
}

// inplaceJournal is the parsed form of the trailer readInPlaceJournal finds.
type inplaceJournal struct {
	origSize int64
	zones    []inplaceZone
}

// writeInPlaceJournal appends the journal payload and trailer to w, which
// must already be positioned right after the new Cues bytes (i.e. at the
// current end of the file, before the Segment size has been extended to
// cover any of this).
func writeInPlaceJournal(w io.Writer, origSize int64, zones []inplaceZone) error {
	payload := make([]byte, 0, 12+len(zones)*12)
	payload = appendUint64(payload, uint64(origSize))
	payload = appendUint32(payload, uint32(len(zones)))
	for _, z := range zones {
		payload = appendUint64(payload, uint64(z.off))
		payload = appendUint32(payload, uint32(len(z.orig)))
		payload = append(payload, z.orig...)
	}
	if len(payload) > maxInplaceJournal {
		return fmt.Errorf("journal payload %d bytes exceeds cap (%d)", len(payload), maxInplaceJournal)
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	trailer := make([]byte, 0, inplaceTrailerLen)
	trailer = appendUint32(trailer, uint32(len(payload)))
	trailer = appendUint32(trailer, crc32.ChecksumIEEE(payload))
	trailer = append(trailer, inplaceJournalMagic[:]...)
	_, err := w.Write(trailer)
	return err
}

// readInPlaceJournal looks for a journal trailer at the end of a file of the
// given size. Returns (nil, nil) when none is present (a clean file, or one
// too small to hold a trailer at all): that is the common case and not an
// error. Returns a non-nil error only when a trailer-shaped tail IS present
// but is corrupt (CRC mismatch or nonsensical geometry) - in that case the
// file must not be touched further.
func readInPlaceJournal(f mkv.ReadSeekCloser, fileSize int64) (*inplaceJournal, error) {
	if fileSize < inplaceTrailerLen {
		return nil, nil
	}
	if _, err := f.Seek(fileSize-inplaceTrailerLen, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek journal trailer: %w", err)
	}
	trailer := make([]byte, inplaceTrailerLen)
	if _, err := io.ReadFull(f, trailer); err != nil {
		return nil, fmt.Errorf("read journal trailer: %w", err)
	}
	if !bytes.Equal(trailer[8:24], inplaceJournalMagic[:]) {
		return nil, nil
	}
	payloadLen := int64(binary.BigEndian.Uint32(trailer[0:4]))
	wantCRC := binary.BigEndian.Uint32(trailer[4:8])
	if payloadLen < 0 || payloadLen > maxInplaceJournal || payloadLen > fileSize-inplaceTrailerLen {
		// The magic matched by coincidence on non-journal data; treat as absent
		// rather than failing an unrelated file.
		return nil, nil
	}

	payloadOff := fileSize - inplaceTrailerLen - payloadLen
	if _, err := f.Seek(payloadOff, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek journal payload: %w", err)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(f, payload); err != nil {
		return nil, fmt.Errorf("read journal payload: %w", err)
	}
	if crc32.ChecksumIEEE(payload) != wantCRC {
		return nil, fmt.Errorf("journal CRC mismatch, refusing to touch the file: %w", errCorruptJournal)
	}

	j, err := parseInplaceJournalPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("corrupt journal payload: %w", err)
	}
	if j.origSize < 0 || j.origSize > fileSize {
		return nil, fmt.Errorf("journal origSize %d out of range (file is %d bytes)", j.origSize, fileSize)
	}
	for _, z := range j.zones {
		if z.off < 0 || z.off+int64(len(z.orig)) > j.origSize {
			return nil, fmt.Errorf("journal zone at offset %d (len %d) falls outside origSize %d", z.off, len(z.orig), j.origSize)
		}
	}
	return j, nil
}

// errCorruptJournal is wrapped into the CRC-mismatch error only to give
// callers something stable to match on with errors.Is, if ever needed.
var errCorruptJournal = errors.New("corrupt journal")

// parseInplaceJournalPayload decodes the origSize + zone list from a journal
// payload buffer whose CRC has already been verified.
func parseInplaceJournalPayload(payload []byte) (*inplaceJournal, error) {
	if len(payload) < 12 {
		return nil, fmt.Errorf("payload too short (%d bytes)", len(payload))
	}
	origSize := int64(binary.BigEndian.Uint64(payload[0:8]))
	zoneCount := binary.BigEndian.Uint32(payload[8:12])
	off := 12
	zones := make([]inplaceZone, 0, zoneCount)
	for i := uint32(0); i < zoneCount; i++ {
		if off+12 > len(payload) {
			return nil, fmt.Errorf("truncated zone header at index %d", i)
		}
		zoff := int64(binary.BigEndian.Uint64(payload[off : off+8]))
		zlen := binary.BigEndian.Uint32(payload[off+8 : off+12])
		off += 12
		if off+int(zlen) > len(payload) {
			return nil, fmt.Errorf("truncated zone data at index %d", i)
		}
		orig := append([]byte(nil), payload[off:off+int(zlen)]...)
		off += int(zlen)
		zones = append(zones, inplaceZone{off: zoff, orig: orig})
	}
	return &inplaceJournal{origSize: origSize, zones: zones}, nil
}

// rollbackInPlaceJournal restores every zone's original bytes and truncates
// the file back to origSize, undoing an interrupted (or failed) patch.
func rollbackInPlaceJournal(f mkv.ReadWriteSeekCloser, j *inplaceJournal) error {
	for _, z := range j.zones {
		if _, err := f.Seek(z.off, io.SeekStart); err != nil {
			return fmt.Errorf("rollback seek: %w", err)
		}
		if _, err := f.Write(z.orig); err != nil {
			return fmt.Errorf("rollback write: %w", err)
		}
	}
	trunc, ok := f.(interface{ Truncate(size int64) error })
	if !ok {
		return fmt.Errorf("rollback: handle does not support Truncate")
	}
	if err := trunc.Truncate(j.origSize); err != nil {
		return fmt.Errorf("rollback truncate: %w", err)
	}
	if s, ok := f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			return fmt.Errorf("rollback sync: %w", err)
		}
	}
	return nil
}

func appendUint64(buf []byte, v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return append(buf, b[:]...)
}

func appendUint32(buf []byte, v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return append(buf, b[:]...)
}
