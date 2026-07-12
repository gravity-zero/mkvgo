package ops

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/ebml"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// salvageResyncCap bounds a single bounded-scan resync attempt: Salvage gives
// up (reporting an error rather than hanging) if no structurally valid
// Cluster is found within this many bytes of the point where the walk broke.
// A var, not a const, so a test can shrink it to keep a "cap exceeded"
// fixture small; production code never changes it.
var salvageResyncCap int64 = 64 << 20 // 64 MiB

// DamagedRange records one span of the source that Salvage could not carry
// over verbatim. The definition lives in the mkv package (mkv.DamagedRange)
// so Reindex with Options.Resync can report the same ranges through
// Options.OnSkip; this alias preserves the original ops.DamagedRange name.
type DamagedRange = mkv.DamagedRange

// RepairedRange records one region the tolerant walk reconstructed instead of
// copying verbatim. The definition lives in the mkv package; this alias
// mirrors DamagedRange.
type RepairedRange = mkv.RepairedRange

// SalvageReport summarises one Salvage run. The definition lives in the mkv
// package (see mkv/reports.go); this alias mirrors DamagedRange.
type SalvageReport = mkv.SalvageReport

// Salvage produces a best-effort copy of a damaged Matroska/WebM file: unlike
// Reindex (which refuses mid-file corruption by design, the same as Validate
// and BlockReader), Salvage is the explicitly lossy-tolerant counterpart. It
// walks srcPath exactly like Reindex - metadata elements and cluster payloads
// copied verbatim, the Cues index rebuilt from the video keyframes it sees -
// but on any structural failure inside the cluster stream (an element header
// that will not decode, a declared size that overflows, or a cluster body
// whose children do not parse to its end) it scans forward, bounded by
// salvageResyncCap, for the next structurally valid Cluster and resumes from
// there, recording the skipped span as a DamagedRange. A truncated source
// (the failure runs to true EOF within the scan) yields exactly one
// DamagedRange ending at EOF; Salvage still finalises and returns a playable
// prefix rather than erroring. Only when the bounded scan is exhausted
// without reaching EOF (garbage runs longer than the cap) or a genuine I/O
// error occurs does Salvage return an error and no output.
//
// A clean source produces zero damaged ranges and a result equivalent to
// Reindex (same cues, same verbatim clusters, same freshly-built SeekHead).
//
// Never in-place: dstPath is always a separate file. The result is re-opened
// and its Cues checked against the ones built during the walk (the same
// light verification Reindex always runs), so a bug in Salvage itself - not
// the source's damage - still surfaces as an error.
func Salvage(ctx context.Context, srcPath, dstPath string, opts ...mkv.Options) (*SalvageReport, error) {
	fs := mkv.FSFrom(opts)

	var rb *rollbackBuilder
	if mkv.RollbackSinkFrom(opts) != nil {
		rb = newSpoolingRollbackBuilder(fs, dstPath+".rbspool.tmp")
		defer rb.cleanup()
	}

	report, cues, timecodeScale, err := salvageCopy(ctx, srcPath, dstPath, fs, mkv.ProgressFrom(opts), mkv.CleanCutFrom(opts), rb)
	if err != nil {
		return nil, err
	}

	if err := verifyReindexedCues(ctx, dstPath, fs, cues, timecodeScale); err != nil {
		return nil, err
	}

	if err := emitRollbackEntry(ctx, rb, srcPath, dstPath, fs, opts); err != nil {
		return nil, err
	}
	return report, nil
}

// MapDamage runs the exact walk Salvage runs - surgical recovery, damage
// ranges, repairs, optional clean-cut accounting - but writes nothing: a
// dry-run that maps what a repair WOULD lose and keep, so an operator can
// decide with the numbers in hand ("this file would lose 0.8s at 12:34")
// before touching anything. The returned report is the one the equivalent
// Salvage (or Reindex with Options.Resync) run would produce.
func MapDamage(ctx context.Context, srcPath string, opts ...mkv.Options) (*SalvageReport, error) {
	fs := mkv.FSFrom(opts)
	dry := &mkv.FS{
		Open:     fs.DoOpen,
		Stat:     fs.DoStat,
		OpenFile: fs.DoOpenFile,
		Create:   func(string) (mkv.WriteSeekCloser, error) { return &discardWriteSeeker{}, nil },
	}
	// Options.RollbackSink is deliberately ignored: nothing is written, so
	// there is nothing to roll back (and COPY ops would reference a
	// discarded output).
	report, _, _, err := salvageCopy(ctx, srcPath, "", dry, mkv.ProgressFrom(opts), mkv.CleanCutFrom(opts), nil)
	if err != nil {
		return nil, err
	}
	return report, nil
}

// discardWriteSeeker swallows writes while tracking position and size, so the
// full salvage walk (including the writer's Finalize seeks) can run without
// producing output.
type discardWriteSeeker struct {
	pos, size int64
}

func (d *discardWriteSeeker) Write(p []byte) (int, error) {
	d.pos += int64(len(p))
	if d.pos > d.size {
		d.size = d.pos
	}
	return len(p), nil
}

func (d *discardWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		d.pos = offset
	case io.SeekCurrent:
		d.pos += offset
	case io.SeekEnd:
		d.pos = d.size + offset
	}
	if d.pos < 0 {
		return 0, fmt.Errorf("discard sink: negative position")
	}
	return d.pos, nil
}

func (d *discardWriteSeeker) Close() error { return nil }

// salvageCopy performs the tolerant copy from srcPath to dstPath, returning
// the report plus the cue points built along the way (for the caller's
// verification pass) and the timecode scale used to derive them. cleanCut
// resumes video only at the next keyframe after each damage gap. The walk
// itself lives on salvageWalker, one method per element class.
func salvageCopy(ctx context.Context, srcPath, dstPath string, fs *mkv.FS, progress mkv.ProgressFunc, cleanCut bool, rb *rollbackBuilder) (report *SalvageReport, cues []mkv.CuePoint, timecodeScale int64, err error) {
	stat, err := fs.DoStat(srcPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: stat src: %w", err)
	}

	// A cheap head-only read of the track list, so the rebuilt cues key on
	// VIDEO keyframes (every audio block is flagged keyframe) - mirrors
	// reindexCopy - and so the surgical block recovery can require valid
	// track numbers in its chain validation. Best-effort: a source too
	// damaged for even this succeeds with nil sets (weaker gates).
	var videoTracks, allTracks map[uint64]bool
	if meta, merr := reader.OpenMetaWithFS(ctx, srcPath, fs); merr == nil {
		videoTracks = videoTrackSet(meta.Tracks)
		allTracks = make(map[uint64]bool, len(meta.Tracks))
		for _, t := range meta.Tracks {
			allTracks[t.ID] = true
		}
	}

	raw, err := fs.DoOpen(srcPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: open src: %w", err)
	}
	defer raw.Close()

	out, err := fs.DoCreate(dstPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: create dst: %w", err)
	}
	defer closeWithErr(out, &err)

	r := bufio.NewReaderSize(raw, reindexBufSize)
	mw := writer.NewMKVWriter(out)

	ebmlBytes, err := copyEBMLHeaderVerbatim(r, out, rb)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: %w", err)
	}
	segBytes, err := beginSegmentRewrite(r, out, mw, rb)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: %w", err)
	}

	w := &salvageWalker{
		raw:         raw,
		r:           r,
		out:         out,
		mw:          mw,
		report:      &SalvageReport{},
		fileSize:    stat.Size(),
		consumed:    ebmlBytes + segBytes,
		scale:       1_000_000,
		videoTracks: videoTracks,
		allTracks:   allTracks,
		cleanCut:    cleanCut,
		progress:    progress,

		prevClusterSize: -1, // no cluster written yet
		rb:              rb,
	}
	if err := w.walk(ctx); err != nil {
		return nil, nil, 0, err
	}

	if err := mw.Finalize(); err != nil {
		return nil, nil, 0, err
	}
	return w.report, mw.Cues, w.scale, nil
}

// salvageWalker carries the state of one tolerant walk over a source file:
// the streams, the writer, the report being built, and the clean-cut arming.
type salvageWalker struct {
	raw         mkv.ReadSeekCloser
	r           *bufio.Reader // buffered view of raw; rebuilt after every seek
	out         mkv.WriteSeekCloser
	mw          *writer.MKVWriter
	report      *SalvageReport
	fileSize    int64
	consumed    int64 // absolute source offset of the next element to read
	scale       int64 // timecode scale, picked up from Info during the walk
	lastGoodMs  int64 // timestamp of the last successfully copied cluster
	videoTracks map[uint64]bool
	allTracks   map[uint64]bool
	cleanCut    bool
	awaitKF     bool // armed after each damage gap when cleanCut is on
	progress    mkv.ProgressFunc
	clusterBuf  []byte           // reused cluster body buffer
	rb          *rollbackBuilder // nil = no rollback delta requested
	// prevClusterSize is the size of the cluster written before the current
	// one, for its PrevSize hint. -1 until one has been written: salvage drops
	// and splits clusters, so the source's own value is meaningless here.
	prevClusterSize int64
}

// walk drives the top-level element loop. Every handler returns
// (walkEnded, error): walkEnded stops the loop cleanly (EOF or a recorded
// truncated tail), an error aborts the whole operation.
func (w *salvageWalker) walk(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		elemStart := w.consumed
		h, hdrBytes, err := ebml.ReadElementHeader(w.r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			ended, rerr := w.skipToNextCluster()
			if rerr != nil || ended {
				return rerr
			}
			continue
		}

		var ended bool
		switch h.ID {
		case mkv.IDSeekHead, mkv.IDCues, mkv.IDVoid:
			ended, err = w.dropIndexElement(h, elemStart, hdrBytes)
		case mkv.IDInfo, mkv.IDTracks, mkv.IDTags, mkv.IDChapters, mkv.IDAttachments:
			ended, err = w.copyMetaElement(h, elemStart, hdrBytes)
		case mkv.IDCluster:
			ended, err = w.copyCluster(h, elemStart, hdrBytes)
		default:
			ended, err = w.copyUnknownElement(h, elemStart, hdrBytes)
		}
		if err != nil || ended {
			return err
		}
	}
}

// recordDamage appends one damaged range to the report and arms the
// clean-cut filter when enabled. Zero-length ranges are ignored. The dropped
// source bytes go to the rollback delta as a literal: this one hook covers
// every loss path, in walk (= original) order. It seeks raw; every caller
// re-seeks before its next read.
func (w *salvageWalker) recordDamage(start, end, startMs, endMs int64) {
	if end <= start {
		return
	}
	if w.rb != nil {
		w.rb.literalFrom(w.raw, start, end-start)
	}
	w.report.DamagedRanges = append(w.report.DamagedRanges, DamagedRange{
		StartOffset: start, EndOffset: end, ApproxStartMs: startMs, ApproxEndMs: endMs,
	})
	w.report.BytesSkipped += end - start
	if end >= w.fileSize {
		// The damage runs to the end of the file: the truncated-source
		// verdict, whichever path detected it (plain resync, surgical scan).
		w.report.TruncatedTail = true
	}
	if w.cleanCut && len(w.videoTracks) > 0 {
		w.awaitKF = true
	}
}

// recordTailDamage marks everything from start to EOF as lost; the caller
// ends the walk right after. This is the "truncated source" verdict: unlike
// mid-file damage, the missing tail cannot be recovered by any repair.
func (w *salvageWalker) recordTailDamage(start int64) {
	w.recordDamage(start, w.fileSize, w.lastGoodMs, w.lastGoodMs)
}

// skipToNextCluster scans forward from w.consumed for the next valid Cluster,
// bounded by salvageResyncCap, records the skipped span, and repositions the
// stream there. ended=true means the source ran out before a new Cluster
// (the tail damage is already recorded) and the walk should stop. A scan cap
// exceeded mid-file (garbage longer than the cap, more data beyond it) or a
// genuine I/O failure returns a non-nil error.
func (w *salvageWalker) skipToNextCluster() (ended bool, err error) {
	if _, err := w.raw.Seek(w.consumed, io.SeekStart); err != nil {
		return false, fmt.Errorf("salvage: seek for resync: %w", err)
	}
	capLimit := w.consumed + salvageResyncCap
	reachesEOF := capLimit >= w.fileSize
	if reachesEOF {
		capLimit = w.fileSize
	}
	off, err := reader.ResyncToCluster(w.raw, capLimit)
	if err != nil {
		return false, fmt.Errorf("salvage: resync scan: %w", err)
	}
	if off < 0 {
		if reachesEOF {
			w.recordTailDamage(w.consumed)
			return true, nil
		}
		return false, fmt.Errorf("salvage: no valid cluster found within %d-byte resync scan from offset %d", salvageResyncCap, w.consumed)
	}
	w.recordDamage(w.consumed, off, w.lastGoodMs, peekClusterTimestampMs(w.raw, off, w.scale))
	w.restoreStreamAt(off)
	return false, nil
}

// restoreStreamAt repositions raw at off and rebuilds the buffered reader
// (the two must always move together).
func (w *salvageWalker) restoreStreamAt(off int64) {
	w.raw.Seek(off, io.SeekStart) //nolint:errcheck // next read surfaces any failure
	w.r = bufio.NewReaderSize(w.raw, reindexBufSize)
	w.consumed = off
}

// dropIndexElement skips a SeekHead/Cues/Void element - the index is rebuilt
// by Finalize - tolerating an implausible size or a truncated tail.
func (w *salvageWalker) dropIndexElement(h ebml.ElementHeader, elemStart int64, hdrBytes int) (bool, error) {
	if h.Size < 0 || h.Size > maxReindexClusterSize {
		return w.skipToNextCluster()
	}
	if w.rb == nil {
		if _, err := io.CopyN(io.Discard, w.r, h.Size); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				w.recordTailDamage(elemStart)
				return true, nil
			}
			return false, fmt.Errorf("salvage: skip 0x%X: %w", h.ID, err)
		}
		w.consumed += int64(hdrBytes) + h.Size
		return false, nil
	}
	// The output has no trace of these: whole element to literals. Captured
	// after a full read so a truncated tail emits nothing partial.
	buf := make([]byte, h.Size)
	if _, err := io.ReadFull(w.r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			w.recordTailDamage(elemStart)
			return true, nil
		}
		return false, fmt.Errorf("salvage: skip 0x%X: %w", h.ID, err)
	}
	w.rb.literalHeader(h, hdrBytes)
	w.rb.literal(buf)
	w.consumed += int64(hdrBytes) + h.Size
	return false, nil
}

// copyMetaElement copies an Info/Tracks/Tags/Chapters/Attachments element
// verbatim, picking up the timecode scale from Info on the way.
func (w *salvageWalker) copyMetaElement(h ebml.ElementHeader, elemStart int64, hdrBytes int) (bool, error) {
	if h.Size < 0 || h.Size > maxReindexClusterSize {
		return w.skipToNextCluster()
	}
	metaBuf := make([]byte, h.Size)
	if _, err := io.ReadFull(w.r, metaBuf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			w.recordTailDamage(elemStart)
			return true, nil
		}
		return false, fmt.Errorf("salvage: read 0x%X body: %w", h.ID, err)
	}
	w.consumed += int64(hdrBytes) + h.Size
	if h.ID == mkv.IDInfo {
		if ts := reindexScanTimecodeScale(metaBuf); ts > 0 {
			w.scale = ts
		}
		w.mw.TimecodeScale = w.scale
	}
	if w.rb != nil {
		w.rb.literalHeader(h, hdrBytes)
	}
	if err := writeMetaElementVerbatim(w.mw, w.out, h, metaBuf); err != nil {
		return false, fmt.Errorf("salvage: %w", err)
	}
	if w.rb != nil {
		after, serr := w.out.Seek(0, io.SeekCurrent)
		if serr != nil {
			return false, fmt.Errorf("salvage: rollback offset: %w", serr)
		}
		w.rb.copyRun(after-h.Size, metaBuf)
	}
	w.report.BytesCopied += int64(hdrBytes) + h.Size
	return false, nil
}

// copyCluster handles one Cluster element: the fast path copies a valid body
// verbatim; anything wrong (implausible size, truncated read, body that does
// not parse to its declared end) goes through the surgical recovery, with the
// pre-surgical skip/resync behavior as the fallback.
func (w *salvageWalker) copyCluster(h ebml.ElementHeader, elemStart int64, hdrBytes int) (bool, error) {
	bodyStart := elemStart + int64(hdrBytes)
	if h.Size < 0 || h.Size > maxReindexClusterSize {
		// Implausible declared size: re-derive the truth from the bytes
		// before giving up on the region.
		recovered, ended, err := w.surgical(elemStart, bodyStart)
		if err != nil || ended {
			return ended, err
		}
		if recovered {
			return false, nil
		}
		return w.skipToNextCluster()
	}

	if int(h.Size) > len(w.clusterBuf) {
		w.clusterBuf = make([]byte, h.Size)
	}
	body := w.clusterBuf[:h.Size]
	if _, err := io.ReadFull(w.r, body); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			// Truncated tail: recover the complete blocks before the cut
			// when there are any.
			recovered, ended, serr := w.surgical(elemStart, bodyStart)
			if serr != nil {
				return false, serr
			}
			if recovered && !ended {
				return false, nil
			}
			if !recovered {
				w.recordTailDamage(elemStart)
			}
			return true, nil
		}
		return false, fmt.Errorf("salvage: read cluster body: %w", err)
	}
	clusterEnd := elemStart + int64(hdrBytes) + h.Size

	if verr := validateClusterBody(body); verr != nil {
		// Framing (ID + size) is intact but the body does not parse to its
		// declared end: either the size lies (intact payload behind it) or
		// bytes inside are damaged. The surgical scan re-derives the child
		// runs and keeps everything that chain-validates, splitting the
		// cluster around the damage.
		recovered, ended, err := w.surgical(elemStart, bodyStart)
		if err != nil || ended {
			return ended, err
		}
		if recovered {
			return false, nil
		}
		// Nothing recoverable: skip the declared extent as before.
		w.recordDamage(elemStart, clusterEnd, w.lastGoodMs, w.lastGoodMs)
		w.restoreStreamAt(clusterEnd)
		return false, nil
	}

	if err := w.emitClusterWithRollback(h, hdrBytes, body); err != nil {
		return false, err
	}
	w.consumed = clusterEnd
	if w.progress != nil && w.fileSize > 0 {
		w.progress(w.consumed, w.fileSize)
	}
	return false, nil
}

// emitClusterWithRollback emits a fast-path cluster and records its delta
// ops: the source header as a literal, the body as a COPY of the output when
// written verbatim, or as a literal when the clean-cut filter is about to
// rewrite it.
func (w *salvageWalker) emitClusterWithRollback(h ebml.ElementHeader, hdrBytes int, body []byte) error {
	// The body reaches the output verbatim only when nothing rewrites it: the
	// clean-cut filter (armed now), or the retarget of a position hint - which
	// also reseals the CRC. Otherwise the ORIGINAL bytes go to the delta as a
	// literal, since the output no longer holds them.
	filtered := w.awaitKF || clusterHasPositionHints(body)
	if w.rb != nil {
		w.rb.literalHeader(h, hdrBytes)
		if filtered {
			w.rb.literal(body)
		}
	}
	if err := w.emitCluster(body, false); err != nil {
		return err
	}
	if w.rb != nil && !filtered {
		after, serr := w.out.Seek(0, io.SeekCurrent)
		if serr != nil {
			return fmt.Errorf("salvage: rollback offset: %w", serr)
		}
		w.rb.copyRun(after-int64(len(body)), body)
	}
	return nil
}

// copyUnknownElement copies an unknown top-level element verbatim - but only
// when it can be trusted. An unknown ID that decodes is indistinguishable
// from garbage that happens to decode (the Matroska top-level set is closed):
// it is trusted only when a validated known element - or EOF - starts exactly
// where it claims to end. Without this gate a damaged region can masquerade
// as a chain of "unknown elements" and ride verbatim into the repaired output.
func (w *salvageWalker) copyUnknownElement(h ebml.ElementHeader, elemStart int64, hdrBytes int) (bool, error) {
	trusted := h.Size >= 0 && h.Size <= maxReindexClusterSize
	if trusted {
		claimedEnd := elemStart + int64(hdrBytes) + h.Size
		trusted = claimedEnd == w.fileSize || validTopLevelAt(w.raw, claimedEnd, w.fileSize)
		// The trust probe moved raw: restore the stream at the body.
		w.restoreStreamAt(elemStart + int64(hdrBytes))
		w.consumed = elemStart // not yet consumed; keep resync anchored here
	}
	if !trusted {
		return w.skipToNextCluster()
	}
	if _, err := ebml.WriteElementID(w.out, h.ID); err != nil {
		return false, fmt.Errorf("salvage: write unknown 0x%X ID: %w", h.ID, err)
	}
	if _, err := ebml.WriteDataSize(w.out, h.Size); err != nil {
		return false, fmt.Errorf("salvage: write unknown 0x%X size: %w", h.ID, err)
	}
	bodyDst := io.Writer(w.out)
	if w.rb != nil {
		w.rb.literalHeader(h, hdrBytes)
		bodyOff, serr := w.out.Seek(0, io.SeekCurrent)
		if serr != nil {
			return false, fmt.Errorf("salvage: rollback offset: %w", serr)
		}
		bodyDst = io.MultiWriter(w.out, w.rb.copyRunStreamed(bodyOff, h.Size))
	}
	if _, err := io.CopyN(bodyDst, w.r, h.Size); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			if w.rb != nil {
				// Ops were already reserved for the full element: the delta
				// cannot tile a source that ends mid-element.
				w.rb.fail(fmt.Errorf("rollback: source truncated inside element 0x%X", h.ID))
			}
			w.recordTailDamage(elemStart)
			return true, nil
		}
		return false, fmt.Errorf("salvage: copy unknown 0x%X body: %w", h.ID, err)
	}
	w.consumed = elemStart + int64(hdrBytes) + h.Size
	w.report.BytesCopied += int64(hdrBytes) + h.Size
	return false, nil
}

// emitCluster writes one cluster body (applying the clean-cut filter when
// armed) and updates the report. reconstructed says the body is not the source
// bytes as they stood (a surgical run: rebuilt from the children that survived,
// possibly with a synthesized Timestamp).
//
// Salvage relocates every cluster it keeps and rewrites some of their bodies,
// so both self-referencing hints (Position, PrevSize) are restated and the
// CRC-32 - which would otherwise still cover blocks the clean cut dropped, or a
// run that stops at the damage - is resealed over what is actually written.
func (w *salvageWalker) emitCluster(body []byte, reconstructed bool) error {
	changed := reconstructed
	if w.awaitKF {
		kept, dropped, kfMs, found := cleanCutFilterBody(body, w.videoTracks, w.scale)
		w.report.CleanCutBytes += dropped
		body = kept
		changed = changed || dropped > 0
		if found {
			w.awaitKF = false
			if n := len(w.report.DamagedRanges); n > 0 && w.report.DamagedRanges[n-1].ApproxEndMs < kfMs {
				w.report.DamagedRanges[n-1].ApproxEndMs = kfMs
			}
		}
	}
	outOff := w.mw.RelPos()
	if hints := retargetClusterPatches(body, outOff, w.prevClusterSize); len(hints) > 0 {
		applyPatches(body, hints)
		changed = true
	}
	if changed {
		sealClusterCRC(body)
	}
	if err := writeClusterVerbatim(w.mw, body, int64(len(body)), w.scale, outOff, w.videoTracks); err != nil {
		return err
	}
	w.prevClusterSize = w.mw.RelPos() - outOff
	if ms, ok := clusterTimestampMs(body, w.scale); ok {
		w.lastGoodMs = ms
	}
	w.report.ClustersCopied++
	w.report.BytesCopied += int64(len(body))
	return nil
}

// surgical attempts block-level recovery of a damaged cluster whose element
// header starts at elemStart (body at bodyStart): re-derive the child runs
// from the bytes, emit each run as a cluster carrying the original Timestamp,
// and record only the gaps as damage. recovered=false (with the walk state
// untouched) means the region does not qualify - the caller falls back to the
// skip/resync behavior. ended=true means the episode ran to EOF and the walk
// should stop.
func (w *salvageWalker) surgical(elemStart, bodyStart int64) (recovered, ended bool, err error) {
	toleranceTC := int64(0)
	if w.scale > 0 {
		toleranceTC = surgicalContinuityToleranceNs / w.scale
	}
	outcome, serr := surgicalScanCluster(w.raw, bodyStart, w.fileSize, w.allTracks, toleranceTC)
	if serr != nil || outcome == nil || !anyRunHasBlocks(outcome.runs) {
		// Unreadable timestamp, scan failure, or nothing solid to keep.
		// The caller restores its own stream position.
		return false, false, nil
	}

	if w.rb != nil {
		// The original cluster header (its size may be the lying kind, or
		// implausible): raw source bytes, straight to a literal.
		w.rb.literalFrom(w.raw, elemStart, bodyStart-elemStart)
	}
	bytesKept, err := w.emitSurgicalRuns(outcome)
	if err != nil {
		return false, false, err
	}
	w.report.RepairedRanges = append(w.report.RepairedRanges, RepairedRange{
		StartOffset: elemStart, EndOffset: outcome.end, BytesKept: bytesKept,
	})
	w.lastGoodMs, _ = reindexSafeTimecodeMs(outcome.tsUnits, w.scale)

	if outcome.atEOF || outcome.end >= w.fileSize {
		w.consumed = outcome.end
		return true, true, nil
	}
	w.restoreStreamAt(outcome.end)
	return true, false, nil
}

// emitSurgicalRuns writes every block-bearing run of a surgical outcome as a
// cluster (continuations get a synthesized Timestamp carrying the original
// base) and records the gaps between runs as damage, bracketed by the block
// times on each side when known.
func (w *salvageWalker) emitSurgicalRuns(outcome *surgicalOutcome) (bytesKept int64, err error) {
	runBuf := []byte(nil)
	emitted := 0
	for i, run := range outcome.runs {
		// A run with no blocks (a lone Timestamp/CRC prefix) is not worth a
		// cluster, but its bytes are still original bytes: literal them so
		// the delta tiles the source. The gap that follows it is recorded
		// either way.
		if !run.hasBlocks {
			if w.rb != nil {
				w.rb.literalFrom(w.raw, run.start, run.end-run.start)
			}
		} else {
			n := run.end - run.start
			bytesKept += n
			prefix := []byte(nil)
			if emitted > 0 || i > 0 {
				// Continuation (or the original Timestamp was in a skipped
				// blockless prefix): synthesize the base.
				prefix = synthesizeTimestamp(outcome.tsUnits)
			}
			if int64(len(prefix))+n > int64(cap(runBuf)) {
				runBuf = make([]byte, int64(len(prefix))+n)
			}
			body := runBuf[:int64(len(prefix))+n]
			copy(body, prefix)
			if _, err := w.raw.Seek(run.start, io.SeekStart); err != nil {
				return bytesKept, fmt.Errorf("salvage: seek surgical run: %w", err)
			}
			if _, err := io.ReadFull(w.raw, body[len(prefix):]); err != nil {
				return bytesKept, fmt.Errorf("salvage: read surgical run: %w", err)
			}
			// A run is a rebuilt cluster: its CRC-32 (when it carries one) is
			// resealed over what survived, and its position hints are restated,
			// so its bytes are no longer the source's. Only a run with neither
			// still lands verbatim and can be a COPY of the output.
			filtered := w.awaitKF || clusterHasPositionHints(body) || clusterHasCRC(body)
			if w.rb != nil && filtered {
				w.rb.literal(body[len(prefix):])
			}
			if err := w.emitCluster(body, true); err != nil {
				return bytesKept, err
			}
			if w.rb != nil && !filtered {
				// The run's bytes sit verbatim in the output right after the
				// synthesized Timestamp prefix.
				after, serr := w.out.Seek(0, io.SeekCurrent)
				if serr != nil {
					return bytesKept, fmt.Errorf("salvage: rollback offset: %w", serr)
				}
				w.rb.copyRun(after-n, body[len(prefix):])
			}
			emitted++
		}
		if i < len(outcome.gaps) {
			g := outcome.gaps[i]
			startMs, endMs := w.surgicalGapTimes(outcome, i)
			w.recordDamage(g.StartOffset, g.EndOffset, startMs, endMs)
		}
	}
	return bytesKept, nil
}

// surgicalGapTimes brackets gap i of a surgical outcome in presentation time:
// the last block before it, the first block after it, or (for the final gap)
// the timestamp of the anchor cluster the walk resumes at.
func (w *salvageWalker) surgicalGapTimes(outcome *surgicalOutcome, i int) (startMs, endMs int64) {
	tsMs, _ := reindexSafeTimecodeMs(outcome.tsUnits, w.scale)
	startMs, endMs = tsMs, tsMs
	if run := outcome.runs[i]; run.hasBlocks {
		if ms, err := reindexSafeTimecodeMs(outcome.tsUnits+run.lastRelTC, w.scale); err == nil {
			startMs = ms
		}
	}
	switch {
	case i+1 < len(outcome.runs) && outcome.runs[i+1].hasBlocks:
		if ms, err := reindexSafeTimecodeMs(outcome.tsUnits+outcome.runs[i+1].firstRelTC, w.scale); err == nil {
			endMs = ms
		}
	case i+1 >= len(outcome.runs):
		if ms := peekClusterTimestampMs(w.raw, outcome.end, w.scale); ms != 0 {
			endMs = ms
		} else {
			endMs = startMs
		}
	}
	return startMs, endMs
}

// validateClusterBody walks every child element of a cluster body
// structurally - element header decode, declared size fits within the
// remaining bytes - without interpreting Block payloads. Unlike
// appendCueFromCluster (which stops at the first keyframe, all it needs), this
// proves the WHOLE body is well-formed, so garbage injected anywhere inside
// it - even after a perfectly fine opening element - is caught.
func validateClusterBody(body []byte) error {
	br := bytes.NewReader(body)
	for br.Len() > 0 {
		h, _, err := ebml.ReadElementHeader(br)
		if err != nil {
			return err
		}
		if h.Size < 0 {
			return fmt.Errorf("salvage: cluster child 0x%X has unknown size", h.ID)
		}
		if h.Size > int64(br.Len()) {
			return fmt.Errorf("salvage: cluster child 0x%X size %d exceeds remaining body %d", h.ID, h.Size, br.Len())
		}
		if _, err := br.Seek(h.Size, io.SeekCurrent); err != nil {
			return err
		}
	}
	return nil
}

// clusterTimestampMs scans a cluster body for its Timestamp child and returns
// it converted to milliseconds. Used for the DamagedRange's approximate time
// bounds, independent of appendCueFromCluster's keyframe-seeking cue logic.
func clusterTimestampMs(body []byte, timecodeScale int64) (int64, bool) {
	br := bytes.NewReader(body)
	for {
		h, _, err := ebml.ReadElementHeader(br)
		if err != nil {
			return 0, false
		}
		if h.ID == mkv.IDTimestamp {
			v, err := ebml.ReadUint(br, h.Size)
			if err != nil {
				return 0, false
			}
			ms, err := reindexSafeTimecodeMs(int64(v), timecodeScale)
			if err != nil {
				return 0, false
			}
			return ms, true
		}
		if h.Size < 0 || h.Size > int64(br.Len()) {
			return 0, false
		}
		if _, err := br.Seek(h.Size, io.SeekCurrent); err != nil {
			return 0, false
		}
	}
}

// peekClusterTimestampMs reads just enough of the Cluster at off (r positioned
// there by a successful resync) to report its Timestamp in ms, then restores
// r's position to off so the main walk resumes unaffected. Best-effort: any
// failure reports 0 (the caller treats that as "unknown", not corrupt).
func peekClusterTimestampMs(r io.ReadSeeker, off, timecodeScale int64) int64 {
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return 0
	}
	defer r.Seek(off, io.SeekStart) //nolint:errcheck
	h, _, err := ebml.ReadElementHeader(r)
	if err != nil || h.ID != mkv.IDCluster || h.Size < 0 {
		return 0
	}
	limit := h.Size
	if limit > 1<<20 {
		limit = 1 << 20 // Timestamp is always the first few bytes; cap the peek read
	}
	buf := make([]byte, limit)
	n, _ := io.ReadFull(r, buf)
	ms, _ := clusterTimestampMs(buf[:n], timecodeScale)
	return ms
}
