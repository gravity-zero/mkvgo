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

// SalvageReport summarises one Salvage run.
type SalvageReport struct {
	ClustersCopied int
	BytesCopied    int64
	BytesSkipped   int64
	DamagedRanges  []DamagedRange
	// RepairedRanges lists the regions where cluster framing was re-derived
	// from the bytes (corrected sizes, continuation headers around a gap)
	// instead of dropping the whole declared extent; the media inside is
	// verbatim. Empty on a clean source.
	RepairedRanges []RepairedRange
	// CleanCutBytes counts video bytes intentionally dropped after damage
	// gaps because they precede the next video keyframe (Options.CleanCut).
	CleanCutBytes int64
}

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

	report, cues, timecodeScale, err := salvageCopy(ctx, srcPath, dstPath, fs, mkv.ProgressFrom(opts), mkv.CleanCutFrom(opts))
	if err != nil {
		return nil, err
	}

	if err := verifyReindexedCues(ctx, dstPath, fs, cues, timecodeScale); err != nil {
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
	report, _, _, err := salvageCopy(ctx, srcPath, "", dry, mkv.ProgressFrom(opts), mkv.CleanCutFrom(opts))
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
// resumes video only at the next keyframe after each damage gap.
func salvageCopy(ctx context.Context, srcPath, dstPath string, fs *mkv.FS, progress mkv.ProgressFunc, cleanCut bool) (report *SalvageReport, cues []mkv.CuePoint, timecodeScale int64, err error) {
	report = &SalvageReport{}

	stat, err := fs.DoStat(srcPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: stat src: %w", err)
	}
	fileSize := stat.Size()

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

	// ── EBML header ──────────────────────────────────────────────────────
	ebmlHdr, ebmlHdrBytes, err := ebml.ReadElementHeader(r)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: read EBML header: %w", err)
	}
	if ebmlHdr.ID != ebml.IDEBMLHeader {
		return nil, nil, 0, fmt.Errorf("salvage: expected EBML header, got 0x%X", ebmlHdr.ID)
	}
	if ebmlHdr.Size < 0 || ebmlHdr.Size > maxReindexClusterSize {
		return nil, nil, 0, fmt.Errorf("salvage: implausible EBML header size %d", ebmlHdr.Size)
	}
	ebmlBody := make([]byte, ebmlHdr.Size)
	if _, err := io.ReadFull(r, ebmlBody); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: read EBML body: %w", err)
	}
	if _, err := ebml.WriteElementID(out, ebmlHdr.ID); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: write EBML ID: %w", err)
	}
	if _, err := ebml.WriteDataSize(out, ebmlHdr.Size); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: write EBML size: %w", err)
	}
	if _, err := out.Write(ebmlBody); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: write EBML body: %w", err)
	}

	// ── Segment ──────────────────────────────────────────────────────────
	segHdr, segHdrBytes, err := ebml.ReadElementHeader(r)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: read Segment header: %w", err)
	}
	if segHdr.ID != mkv.IDSegment {
		return nil, nil, 0, fmt.Errorf("salvage: expected Segment, got 0x%X", segHdr.ID)
	}
	if _, err := ebml.WriteElementID(out, mkv.IDSegment); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: write Segment ID: %w", err)
	}
	if _, err := ebml.WriteDataSize(out, -1); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: write Segment size: %w", err)
	}
	segDataStart, err := out.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: seek Segment data start: %w", err)
	}
	mw.SegDataStart = segDataStart
	mw.SeekHeadPos = 0
	if err := writer.WriteVoid(out, writer.SeekHeadReserve); err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: write SeekHead placeholder: %w", err)
	}

	consumed := int64(ebmlHdrBytes) + ebmlHdr.Size + int64(segHdrBytes)
	timecodeScale = 1_000_000

	var (
		clusterBuf []byte
		lastGoodMs int64 // timestamp of the last successfully copied cluster
	)

	// awaitKF is set after each damage gap when cleanCut is on: video blocks
	// are dropped until the next video keyframe so playback resumes with a
	// clean cut instead of reference-less P/B frames.
	awaitKF := false

	// truncatedTail records the pending damage span opened when a resync
	// discovers the source ends before a new valid Cluster; the walk stops
	// right after recording it.
	recordDamage := func(start, end, startMs, endMs int64) {
		if end <= start {
			return
		}
		report.DamagedRanges = append(report.DamagedRanges, DamagedRange{
			StartOffset: start, EndOffset: end, ApproxStartMs: startMs, ApproxEndMs: endMs,
		})
		report.BytesSkipped += end - start
		if cleanCut && len(videoTracks) > 0 {
			awaitKF = true
		}
	}

	// resync scans forward from `consumed` for the next valid Cluster,
	// bounded by salvageResyncCap. On success it repositions raw and returns
	// a fresh buffered reader plus the found offset. On a truncated tail
	// (the scan runs to real EOF) it records the final damaged range and
	// returns ok=false, err=nil so the caller stops the walk cleanly. On a
	// genuine cap-exceeded (garbage longer than the cap, more data remains
	// beyond it) or I/O failure it returns a non-nil error.
	resync := func() (newR *bufio.Reader, newOff int64, ok bool, rerr error) {
		if _, err := raw.Seek(consumed, io.SeekStart); err != nil {
			return nil, 0, false, fmt.Errorf("salvage: seek for resync: %w", err)
		}
		capLimit := consumed + salvageResyncCap
		reachesEOF := capLimit >= fileSize
		if reachesEOF {
			capLimit = fileSize
		}
		off, err := reader.ResyncToCluster(raw, capLimit)
		if err != nil {
			return nil, 0, false, fmt.Errorf("salvage: resync scan: %w", err)
		}
		if off < 0 {
			if reachesEOF {
				recordDamage(consumed, fileSize, lastGoodMs, lastGoodMs)
				return nil, 0, false, nil
			}
			return nil, 0, false, fmt.Errorf("salvage: no valid cluster found within %d-byte resync scan from offset %d", salvageResyncCap, consumed)
		}
		return bufio.NewReaderSize(raw, reindexBufSize), off, true, nil
	}

	// emitCluster writes one cluster body (applying the clean-cut filter when
	// armed) and updates the report.
	emitCluster := func(body []byte) error {
		if awaitKF {
			kept, dropped, kfMs, found := cleanCutFilterBody(body, videoTracks, timecodeScale)
			report.CleanCutBytes += dropped
			body = kept
			if found {
				awaitKF = false
				if n := len(report.DamagedRanges); n > 0 && report.DamagedRanges[n-1].ApproxEndMs < kfMs {
					report.DamagedRanges[n-1].ApproxEndMs = kfMs
				}
			}
		}
		outOff := mw.RelPos()
		if err := writeClusterVerbatim(mw, body, int64(len(body)), timecodeScale, outOff, videoTracks); err != nil {
			return err
		}
		if ms, ok := clusterTimestampMs(body, timecodeScale); ok {
			lastGoodMs = ms
		}
		report.ClustersCopied++
		report.BytesCopied += int64(len(body))
		return nil
	}

	// surgical attempts block-level recovery of a damaged cluster whose
	// element header starts at elemStart (body at bodyStart): re-derive the
	// child runs from the bytes, emit each run as a cluster carrying the
	// original Timestamp, and record only the gaps as damage. Returns
	// recovered=false (with the walk state untouched) when the region does
	// not qualify - the caller falls back to the skip/resync behavior.
	// walkEnded=true means the episode ran to EOF and the walk should stop.
	surgical := func(elemStart, bodyStart int64) (recovered, walkEnded bool, serr error) {
		toleranceTC := int64(0)
		if timecodeScale > 0 {
			toleranceTC = surgicalContinuityToleranceNs / timecodeScale
		}
		outcome, err := surgicalScanCluster(raw, bodyStart, fileSize, allTracks, toleranceTC)
		if err != nil || outcome == nil || !anyRunHasBlocks(outcome.runs) {
			// Unreadable timestamp, scan failure, or nothing solid to keep.
			// The caller restores its own stream position.
			return false, false, nil
		}

		tsMs, _ := reindexSafeTimecodeMs(outcome.tsUnits, timecodeScale)
		var bytesKept int64
		emitted := 0
		runBuf := make([]byte, 0)
		for i, run := range outcome.runs {
			// A run with no blocks (a lone Timestamp/CRC prefix) is not worth
			// a cluster; its gap below is still recorded.
			if run.hasBlocks {
				n := run.end - run.start
				bytesKept += n
				prefix := []byte(nil)
				if emitted > 0 || i > 0 {
					// Continuation (or the original Timestamp was in a
					// skipped blockless prefix): synthesize the base.
					prefix = synthesizeTimestamp(outcome.tsUnits)
				}
				if int64(len(prefix))+n > int64(cap(runBuf)) {
					runBuf = make([]byte, int64(len(prefix))+n)
				}
				body := runBuf[:int64(len(prefix))+n]
				copy(body, prefix)
				if _, err := raw.Seek(run.start, io.SeekStart); err != nil {
					return false, false, fmt.Errorf("salvage: seek surgical run: %w", err)
				}
				if _, err := io.ReadFull(raw, body[len(prefix):]); err != nil {
					return false, false, fmt.Errorf("salvage: read surgical run: %w", err)
				}
				if err := emitCluster(body); err != nil {
					return false, false, err
				}
				emitted++
			}

			// Record the gap that follows this run, bracketing it with the
			// block times on each side when known.
			if i < len(outcome.gaps) {
				g := outcome.gaps[i]
				startMs, endMs := tsMs, tsMs
				if run.hasBlocks {
					if ms, e := reindexSafeTimecodeMs(outcome.tsUnits+run.lastRelTC, timecodeScale); e == nil {
						startMs = ms
					}
				}
				if i+1 < len(outcome.runs) && outcome.runs[i+1].hasBlocks {
					if ms, e := reindexSafeTimecodeMs(outcome.tsUnits+outcome.runs[i+1].firstRelTC, timecodeScale); e == nil {
						endMs = ms
					}
				} else if i+1 >= len(outcome.runs) {
					endMs = peekClusterTimestampMs(raw, outcome.end, timecodeScale)
					if endMs == 0 {
						endMs = startMs
					}
				}
				recordDamage(g.StartOffset, g.EndOffset, startMs, endMs)
			}
		}
		report.RepairedRanges = append(report.RepairedRanges, RepairedRange{
			StartOffset: elemStart, EndOffset: outcome.end, BytesKept: bytesKept,
		})
		lastGoodMs = tsMs

		consumed = outcome.end
		if outcome.atEOF || consumed >= fileSize {
			return true, true, nil
		}
		if _, err := raw.Seek(consumed, io.SeekStart); err != nil {
			return false, false, fmt.Errorf("salvage: seek after surgical: %w", err)
		}
		r = bufio.NewReaderSize(raw, reindexBufSize)
		return true, false, nil
	}

walk:
	for {
		if ctx.Err() != nil {
			return nil, nil, 0, ctx.Err()
		}
		elemStart := consumed
		h, hdrBytes, err := ebml.ReadElementHeader(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			nr, off, ok, rerr := resync()
			if rerr != nil {
				return nil, nil, 0, rerr
			}
			if !ok {
				break // truncated tail already recorded
			}
			recordDamage(elemStart, off, lastGoodMs, peekClusterTimestampMs(raw, off, timecodeScale))
			r, consumed = nr, off
			continue
		}

		switch h.ID {
		case mkv.IDSeekHead, mkv.IDCues, mkv.IDVoid:
			if h.Size < 0 || h.Size > maxReindexClusterSize {
				nr, off, ok, rerr := resync()
				if rerr != nil {
					return nil, nil, 0, rerr
				}
				if !ok {
					break walk // truncated tail already recorded
				}
				recordDamage(elemStart, off, lastGoodMs, peekClusterTimestampMs(raw, off, timecodeScale))
				r, consumed = nr, off
				continue
			}
			if _, err := io.CopyN(io.Discard, r, h.Size); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					recordDamage(elemStart, fileSize, lastGoodMs, lastGoodMs)
					break walk
				}
				return nil, nil, 0, fmt.Errorf("salvage: skip 0x%X: %w", h.ID, err)
			}
			consumed += int64(hdrBytes) + h.Size

		case mkv.IDInfo, mkv.IDTracks, mkv.IDTags, mkv.IDChapters, mkv.IDAttachments:
			if h.Size < 0 || h.Size > maxReindexClusterSize {
				nr, off, ok, rerr := resync()
				if rerr != nil {
					return nil, nil, 0, rerr
				}
				if !ok {
					break walk // truncated tail already recorded
				}
				recordDamage(elemStart, off, lastGoodMs, peekClusterTimestampMs(raw, off, timecodeScale))
				r, consumed = nr, off
				continue
			}
			metaBuf := make([]byte, h.Size)
			if _, err := io.ReadFull(r, metaBuf); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					recordDamage(elemStart, fileSize, lastGoodMs, lastGoodMs)
					break walk
				}
				return nil, nil, 0, fmt.Errorf("salvage: read 0x%X body: %w", h.ID, err)
			}
			consumed += int64(hdrBytes) + h.Size
			if h.ID == mkv.IDInfo {
				if ts := reindexScanTimecodeScale(metaBuf); ts > 0 {
					timecodeScale = ts
				}
				mw.TimecodeScale = timecodeScale
			}
			pos := mw.RelPos()
			switch h.ID {
			case mkv.IDInfo:
				mw.InfoPos = pos
			case mkv.IDTracks:
				mw.TracksPos = pos
			case mkv.IDTags:
				mw.TagsPos = pos
			case mkv.IDChapters:
				mw.ChaptersPos = pos
			case mkv.IDAttachments:
				mw.AttachPos = pos
			}
			if _, err := ebml.WriteElementID(out, h.ID); err != nil {
				return nil, nil, 0, fmt.Errorf("salvage: write 0x%X ID: %w", h.ID, err)
			}
			if _, err := ebml.WriteDataSize(out, h.Size); err != nil {
				return nil, nil, 0, fmt.Errorf("salvage: write 0x%X size: %w", h.ID, err)
			}
			if _, err := out.Write(metaBuf); err != nil {
				return nil, nil, 0, fmt.Errorf("salvage: write 0x%X body: %w", h.ID, err)
			}
			report.BytesCopied += int64(hdrBytes) + h.Size

		case mkv.IDCluster:
			bodyStart := elemStart + int64(hdrBytes)
			if h.Size < 0 || h.Size > maxReindexClusterSize {
				// Implausible declared size: re-derive the truth from the
				// bytes before giving up on the region.
				ok, done, serr := surgical(elemStart, bodyStart)
				if serr != nil {
					return nil, nil, 0, serr
				}
				if done {
					break walk
				}
				if ok {
					continue
				}
				nr, off, ok, rerr := resync()
				if rerr != nil {
					return nil, nil, 0, rerr
				}
				if !ok {
					break walk // truncated tail already recorded
				}
				recordDamage(elemStart, off, lastGoodMs, peekClusterTimestampMs(raw, off, timecodeScale))
				r, consumed = nr, off
				continue
			}

			if int(h.Size) > len(clusterBuf) {
				clusterBuf = make([]byte, h.Size)
			}
			body := clusterBuf[:h.Size]
			if _, err := io.ReadFull(r, body); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					// Truncated tail: recover the complete blocks before the
					// cut when there are any.
					ok, done, serr := surgical(elemStart, bodyStart)
					if serr != nil {
						return nil, nil, 0, serr
					}
					if ok && !done {
						continue
					}
					if !ok {
						recordDamage(elemStart, fileSize, lastGoodMs, lastGoodMs)
					}
					break walk
				}
				return nil, nil, 0, fmt.Errorf("salvage: read cluster body: %w", err)
			}
			clusterEnd := elemStart + int64(hdrBytes) + h.Size

			if verr := validateClusterBody(body); verr != nil {
				// Framing (ID + size) is intact but the body does not parse
				// to its declared end: either the size lies (intact payload
				// behind it) or bytes inside are damaged. The surgical scan
				// re-derives the child runs and keeps everything that
				// chain-validates, splitting the cluster around the damage.
				ok, done, serr := surgical(elemStart, bodyStart)
				if serr != nil {
					return nil, nil, 0, serr
				}
				if done {
					break walk
				}
				if ok {
					continue
				}
				// Nothing recoverable: skip the declared extent as before.
				if _, err := raw.Seek(clusterEnd, io.SeekStart); err != nil {
					return nil, nil, 0, fmt.Errorf("salvage: reseek after surgical probe: %w", err)
				}
				r = bufio.NewReaderSize(raw, reindexBufSize)
				recordDamage(elemStart, clusterEnd, lastGoodMs, lastGoodMs)
				consumed = clusterEnd
				continue
			}

			if err := emitCluster(body); err != nil {
				return nil, nil, 0, err
			}
			consumed = clusterEnd

			if progress != nil && fileSize > 0 {
				progress(consumed, fileSize)
			}

		default:
			// An unknown top-level ID that decodes is indistinguishable from
			// garbage that happens to decode (the Matroska top-level set is
			// closed): only trust it when a validated known element - or EOF -
			// starts exactly where it claims to end. Without this gate a
			// damaged region can masquerade as a chain of "unknown elements"
			// and be copied verbatim into the repaired output.
			trusted := h.Size >= 0 && h.Size <= maxReindexClusterSize
			if trusted {
				claimedEnd := elemStart + int64(hdrBytes) + h.Size
				trusted = claimedEnd == fileSize || validTopLevelAt(raw, claimedEnd, fileSize)
				// The trust probe moved raw: restore the stream at the body.
				if _, err := raw.Seek(elemStart+int64(hdrBytes), io.SeekStart); err != nil {
					return nil, nil, 0, fmt.Errorf("salvage: reseek after trust probe: %w", err)
				}
				r = bufio.NewReaderSize(raw, reindexBufSize)
			}
			if !trusted {
				nr, off, ok, rerr := resync()
				if rerr != nil {
					return nil, nil, 0, rerr
				}
				if !ok {
					break walk // truncated tail already recorded
				}
				recordDamage(elemStart, off, lastGoodMs, peekClusterTimestampMs(raw, off, timecodeScale))
				r, consumed = nr, off
				continue
			}
			if _, err := ebml.WriteElementID(out, h.ID); err != nil {
				return nil, nil, 0, fmt.Errorf("salvage: write unknown 0x%X ID: %w", h.ID, err)
			}
			if _, err := ebml.WriteDataSize(out, h.Size); err != nil {
				return nil, nil, 0, fmt.Errorf("salvage: write unknown 0x%X size: %w", h.ID, err)
			}
			if _, err := io.CopyN(out, r, h.Size); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					recordDamage(elemStart, fileSize, lastGoodMs, lastGoodMs)
					break walk
				}
				return nil, nil, 0, fmt.Errorf("salvage: copy unknown 0x%X body: %w", h.ID, err)
			}
			consumed += int64(hdrBytes) + h.Size
			report.BytesCopied += int64(hdrBytes) + h.Size
		}
	}

	if err := mw.Finalize(); err != nil {
		return nil, nil, 0, err
	}
	return report, mw.Cues, timecodeScale, nil
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
