package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// cueholeprobe.go - ProbeCueHoles looks INSIDE the holes CueHealth found, and
// nowhere else. CueHealth is head-only by contract, so all it can say about a
// hole is its width; the remedy, though, depends on what the hole holds - and
// three files with the same 60 s hole want three different answers:
//
//   - video keyframes nobody cued: a reindex closes the hole;
//   - video frames but not one keyframe: only a re-encode can make the stretch
//     seekable, an index can only name keyframes that exist;
//   - a stretch with no video block at all: the picture is missing from the
//     stream (measured on a real episode whose 771 cues matched its 771
//     keyframes one for one), and nothing but the source can supply it. Judged
//     on the widest picture-less stretch, not on a frame count: the GOP the
//     opening cue starts is inside the hole too, and that episode's 61 s of
//     missing picture "held" 84 frames - the tail of that GOP.
//
// Sent to a reindex regardless, the last two come back exactly as sparse - a
// repair loop that never converges, and an operator told to re-encode a file
// whose frames are not there to encode. The probe reads the clusters between
// the two cues that bracket each hole (their positions are in the index), block
// headers alone with payloads skipped by size, so its cost is the hole's
// block-header count - a few thousand reads for a minute of picture - never the
// media volume, on the rare file already condemned. Memory stays constant.
//
// The probe refuses to guess. A cue whose position does not land on a cluster
// (a stale index - the very thing being diagnosed), a walk that never reached
// the far side of the hole, a hole with more blocks than the budget: Content
// stays "", and the caller falls back to the head-only verdict. A negative
// verdict ("picture-missing", "no-keyframes") is only ever pronounced on a hole
// walked end to end.

// ProbeCueHoles fills the Content of every hole in r.Holes (see mkv.CueHole)
// with what the hole's clusters actually hold, in one bounded, header-only pass
// per hole. r is the report CueHealth produced for path; a report with no
// holes costs nothing. An error opening the file or its index is returned;
// a hole the probe cannot conclude on is left with Content "" and does not
// fail the others.
func ProbeCueHoles(ctx context.Context, path string, r *CueHealthReport, opts ...mkv.Options) error {
	if r == nil || len(r.Holes) == 0 {
		return nil
	}
	fs := mkv.FSFrom(opts)
	meta, err := reader.OpenMetaWithFS(ctx, path, fs, reader.WithCues())
	if err != nil {
		return fmt.Errorf("probe cue holes: %w", err)
	}
	return probeCueHolesFrom(ctx, path, fs, meta, r)
}

// probeCueHolesFrom is ProbeCueHoles on an already-read container (Cues).
func probeCueHolesFrom(ctx context.Context, path string, fs *mkv.FS, meta *mkv.Container, r *CueHealthReport) error {
	if r == nil || len(r.Holes) == 0 {
		return nil
	}
	video := videoTrackSet(meta.Tracks)
	cues := make([]mkv.CuePoint, 0, len(meta.Cues))
	for _, c := range meta.Cues {
		if video[c.Track] {
			cues = append(cues, c)
		}
	}
	sort.Slice(cues, func(i, j int) bool { return cues[i].TimeMs < cues[j].TimeMs })

	f, err := fs.DoOpen(path)
	if err != nil {
		return fmt.Errorf("probe cue holes: %w", err)
	}
	defer f.Close()

	scale := meta.Info.TimecodeScale
	if scale <= 0 {
		scale = 1_000_000
	}
	last := r.LastCueMs
	for i := range r.Holes {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := &r.Holes[i]
		// The cluster to start from: the one holding the cue that opens the
		// hole. A hole at the start has no such cue and is walked from the
		// first cluster.
		startOff := int64(-1)
		for j := len(cues) - 1; j >= 0; j-- {
			if cues[j].TimeMs <= h.AtMs {
				startOff = meta.SegmentStart + cues[j].ClusterPos
				break
			}
		}
		probeHole(f, scale, startOff, video, h, h.AtMs == last)
	}
	return nil
}

// maxHoleProbeBlocks bounds one hole's walk: past it the probe gives up on that
// hole rather than read on (a minute of 4K at 60 fps is under 20 000 video
// blocks, audio and subtitles included well under this).
const maxHoleProbeBlocks = 500_000

// probeHole walks the clusters spanning one hole and classifies it. startOff is
// the absolute offset of the cluster to start from (-1: from the first
// cluster); isTail says the hole runs to the end of the picture, so EOF is
// where it is expected to end rather than a sign the walk fell short.
func probeHole(f io.ReadSeeker, scale, startOff int64, video map[uint64]bool, h *mkv.CueHole, isTail bool) {
	var br *reader.BlockReader
	var err error
	if startOff < 0 {
		// NewBlockReader parses the EBML header from the current position:
		// the file is at wherever the previous hole's walk left it.
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return
		}
		br, err = reader.NewBlockReader(f, scale)
	} else {
		br, err = reader.NewBlockReaderAt(f, scale, startOff)
	}
	if err != nil {
		return
	}
	br.SetHeaderOnly(true)
	start, end := h.AtMs, h.AtMs+h.GapMs
	br.StopBeforeClusterMs(end)

	var videoBlocks, keyframes, blocks int
	var absent, lastVideo int64 = 0, start
	reachedEnd := false
	for {
		blk, nerr := br.Next()
		if nerr != nil {
			if errors.Is(nerr, reader.ErrClusterLimit) {
				reachedEnd = true
			} else if nerr == io.EOF && isTail {
				reachedEnd = true
			}
			break
		}
		blocks++
		if blocks > maxHoleProbeBlocks {
			return // over budget: inconclusive, not a verdict
		}
		if blk.Timecode >= end {
			reachedEnd = true // the far side of the hole, on any track
			break
		}
		if blk.Timecode <= start || !video[blk.TrackNumber] {
			continue
		}
		videoBlocks++
		if gap := blk.Timecode - lastVideo; gap > absent {
			absent = gap
		}
		lastVideo = blk.Timecode
		if blk.Keyframe {
			keyframes++
			break // an uncued keyframe decides it: a reindex closes this hole
		}
	}
	if reachedEnd && keyframes == 0 {
		if gap := end - lastVideo; gap > absent {
			absent = gap // from the last frame seen to the far side
		}
	}
	// A start offset that is not a cluster reads as an empty walk: the index
	// itself is stale there, and an empty walk must not become "no video".
	if br.ClusterCount() == 0 || blocks == 0 {
		return
	}
	h.VideoBlocks, h.Keyframes, h.VideoAbsentMs = videoBlocks, keyframes, absent
	switch {
	case keyframes > 0:
		h.Content = "uncued-keyframes"
	case !reachedEnd:
		// Walk cut short of the far side (a truncated or damaged stretch):
		// what was not seen cannot be pronounced absent.
	case absent > maxSeekGapMs:
		h.Content = "picture-missing"
	default:
		h.Content = "no-keyframes"
	}
}

// probedSparseVerdict restates the index-sparse finding from what the probe
// found in each hole: the detail names every hole with its content, the remedy
// is the one that actually changes something. A hole a reindex closes keeps the
// reindex as remedy even when others are beyond it - the fixable part gets
// fixed; picture missing from the stream outranks a re-encode (the source is
// needed either way); a stretch with frames but no keyframe is a re-encode's
// job. With nothing concluded, the head-only verdict stands.
func probedSparseVerdict(r *CueHealthReport, headOnlyDetail, headOnlyRemedy string) (detail, remedy string) {
	var fixable, missing, unkeyed, unknown int
	parts := make([]string, 0, len(r.Holes))
	for _, h := range r.Holes {
		where := fmt.Sprintf("%.0fs at %s", secs(h.GapMs), clockMs(h.AtMs))
		if h.AtMs == r.LastCueMs && h.GapMs == r.TailGapMs {
			where = fmt.Sprintf("%.0fs tail after %s", secs(h.GapMs), clockMs(h.AtMs))
		}
		switch h.Content {
		case "uncued-keyframes":
			fixable++
			parts = append(parts, where+" holds uncued keyframes")
		case "no-keyframes":
			unkeyed++
			parts = append(parts, fmt.Sprintf("%s holds %d video frame(s) and no keyframe (none for %.0fs of it)", where, h.VideoBlocks, secs(h.VideoAbsentMs)))
		case "picture-missing":
			missing++
			parts = append(parts, fmt.Sprintf("%s has no video at all for %.0fs of it", where, secs(h.VideoAbsentMs)))
		default:
			unknown++
			parts = append(parts, where+" could not be probed")
		}
	}
	if fixable+missing+unkeyed == 0 {
		return headOnlyDetail, headOnlyRemedy
	}
	detail = fmt.Sprintf("the video cues leave %d hole(s) wider than %ds - probed: %s",
		len(r.Holes), maxSeekGapMs/1000, strings.Join(parts, "; "))
	switch {
	case fixable > 0:
		remedy = "mkvgo reindex"
		if missing+unkeyed > 0 {
			remedy += " (closes the hole(s) holding uncued keyframes; the others are picture missing from the stream or without a keyframe, which no index can cue)"
		}
	case missing > 0:
		remedy = "re-acquire the source (the picture is missing from the stream there; a reindex cannot restore it)"
	default:
		remedy = "re-encode the source (no keyframe inside the hole(s); an index can only cue keyframes that exist)"
	}
	return detail, remedy
}
