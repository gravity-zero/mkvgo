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
// over verbatim - either because the byte range itself failed a structural
// check, or because a resync scan had to jump over it to reach the next good
// Cluster. Offsets are absolute byte offsets into srcPath. ApproxStartMs and
// ApproxEndMs bracket the presentation time lost: the last known-good
// cluster's timestamp before the gap and the first known-good cluster's
// timestamp after it (or, for a truncated tail, the last known-good
// timestamp repeated, since there is no "after").
type DamagedRange struct {
	StartOffset   int64
	EndOffset     int64
	ApproxStartMs int64
	ApproxEndMs   int64
}

// SalvageReport summarises one Salvage run.
type SalvageReport struct {
	ClustersCopied int
	BytesCopied    int64
	BytesSkipped   int64
	DamagedRanges  []DamagedRange
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

	report, cues, timecodeScale, err := salvageCopy(ctx, srcPath, dstPath, fs, mkv.ProgressFrom(opts))
	if err != nil {
		return nil, err
	}

	if err := verifyReindexedCues(ctx, dstPath, fs, cues, timecodeScale); err != nil {
		return nil, err
	}

	return report, nil
}

// salvageCopy performs the tolerant copy from srcPath to dstPath, returning
// the report plus the cue points built along the way (for the caller's
// verification pass) and the timecode scale used to derive them.
func salvageCopy(ctx context.Context, srcPath, dstPath string, fs *mkv.FS, progress mkv.ProgressFunc) (report *SalvageReport, cues []mkv.CuePoint, timecodeScale int64, err error) {
	report = &SalvageReport{}

	stat, err := fs.DoStat(srcPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("salvage: stat src: %w", err)
	}
	fileSize := stat.Size()

	// A cheap head-only read of the track list, so the rebuilt cues key on
	// VIDEO keyframes (every audio block is flagged keyframe) - mirrors
	// reindexCopy. Best-effort: a source too damaged for even this succeeds
	// with videoTracks == nil (falls back to the audio-cue path).
	var videoTracks map[uint64]bool
	if meta, merr := reader.OpenMetaWithFS(ctx, srcPath, fs); merr == nil {
		videoTracks = videoTrackSet(meta.Tracks)
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

			if int(h.Size) > len(clusterBuf) {
				clusterBuf = make([]byte, h.Size)
			}
			body := clusterBuf[:h.Size]
			if _, err := io.ReadFull(r, body); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					recordDamage(elemStart, fileSize, lastGoodMs, lastGoodMs)
					break walk
				}
				return nil, nil, 0, fmt.Errorf("salvage: read cluster body: %w", err)
			}
			clusterEnd := elemStart + int64(hdrBytes) + h.Size

			if verr := validateClusterBody(body); verr != nil {
				// Framing (ID + size) is intact - the corruption is inside
				// the declared bounds, so the next element is known exactly:
				// no resync needed, just skip this cluster's bytes.
				recordDamage(elemStart, clusterEnd, lastGoodMs, lastGoodMs)
				consumed = clusterEnd
				continue
			}

			outOff := mw.RelPos()
			if err := writeClusterVerbatim(mw, body, h.Size, timecodeScale, outOff, videoTracks); err != nil {
				return nil, nil, 0, err
			}
			if ms, ok := clusterTimestampMs(body, timecodeScale); ok {
				lastGoodMs = ms
			}
			report.ClustersCopied++
			report.BytesCopied += int64(hdrBytes) + h.Size
			consumed = clusterEnd

			if progress != nil && fileSize > 0 {
				progress(consumed, fileSize)
			}

		default:
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
