package ops

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// cuehealth.go - CueHealth classifies a file's seek index HEAD-ONLY. Validate
// proves cue times against real keyframes but walks the whole file; CueHealth
// answers the cheaper, earlier question a library scan needs - "can this index
// seek video at all?" - from the Tracks, Cues and Tags alone, in milliseconds.
// The classic defect it surfaces: a muxer that cued the audio instead of the
// video, so a video file's index exists, readers call the file "indexed", and
// every seek still lands mid-GOP.
//
// The verdict judges the VIDEO cues alone, because they are what seeking uses:
// the keyframe index is built from the video-keyed cues and drops the rest
// (reader.keyframeTimesMs). Cues on an audio track are therefore inert - a real
// muxer that cues every track produces an index that is mostly "non-video" and
// seeks perfectly - so their share is reported (NonVideoPct) but never condemns
// a file on its own. What condemns a file is a video index that cannot serve a
// seek: none at all (misskeyed), or holes too wide to land in (sparse).
//
// And a hole is measured against the PICTURE, not the container. The declared
// duration is the longest track's end - on real files an audio track's, 30 to
// 110 s past the last frame - so the stretch from the last video cue to it is,
// nearly always, sound outlasting picture: nothing to seek to, nothing any index
// could cue. Judged against it, files cued every 2 s were condemned for their
// end credits. The video track's own end is read from the statistics DURATION
// tag mainstream muxers write per track, once the tag is shown to describe THIS
// file (same writing application, same date when both are stated, not past the
// declared duration - the checks mkvmerge itself applies before trusting one);
// without it, the tail counts only past what an outlasting track accounts for.

// CueHealthReport classifies a file's CuePoints by referenced track. The
// definition lives in the mkv package (see mkv/reports.go).
type CueHealthReport = mkv.CueHealthReport

// CueHealth reads path's Tracks, Cues and Tags head-only (through the SeekHead,
// or a bounded read back from EOF when nothing indexes them - never a cluster
// walk), classifies every CuePoint by the track it references, and judges the
// video cues' coverage. It is the scan-time complement of Validate: Validate
// proves cue times against real keyframes at the cost of a full read; CueHealth
// spots an index that cannot seek video - absent, keyed on the wrong tracks, or
// too full of holes - in milliseconds.
//
// A file whose content is ISO base media (MP4/MOV) rather than Matroska fails
// with an error wrapping reader.ErrNotMatroska - errors.Is-able, so a scan
// can classify the mislabeled file instead of retrying it forever (Diagnose
// converts the same condition into a "wrong-container" finding).
func CueHealth(ctx context.Context, path string, opts ...mkv.Options) (*CueHealthReport, error) {
	fs := mkv.FSFrom(opts)
	// WithTags: the video track's statistics (DURATION, NUMBER_OF_FRAMES) are
	// what lets the tail be measured against the picture's end rather than the
	// declared duration. Same head-only read - the Tags sit next to the Cues.
	meta, err := reader.OpenMetaWithFS(ctx, path, fs, reader.WithCues(), reader.WithTags())
	if err != nil {
		return nil, fmt.Errorf("cue health: %w", err)
	}

	known := make(map[uint64]bool, len(meta.Tracks))
	video := videoTrackSet(meta.Tracks)
	for _, t := range meta.Tracks {
		known[t.ID] = true
	}

	r := &CueHealthReport{
		PerTrack:      map[uint64]int{},
		HasVideoTrack: len(video) > 0,
	}
	for i, c := range meta.Cues {
		r.TotalCues++
		r.PerTrack[c.Track]++
		switch {
		case video[c.Track]:
			r.VideoCues++
		case !known[c.Track]:
			r.UnknownTrackCues++
		default:
			r.NonVideoCues++
		}
		if i == 0 || c.TimeMs < r.FirstCueMs {
			r.FirstCueMs = c.TimeMs
		}
		if c.TimeMs > r.LastCueMs {
			r.LastCueMs = c.TimeMs
		}
	}
	if r.TotalCues > 0 {
		r.NonVideoPct = float64(r.NonVideoCues+r.UnknownTrackCues) * 100 / float64(r.TotalCues)
	}
	if r.HasVideoTrack {
		measureVideoCoverage(r, meta, video)
	}

	switch {
	case r.TotalCues == 0:
		r.Reason = "no seek index: run mkvgo reindex"
	case r.UnknownTrackCues > 0:
		r.Reason = fmt.Sprintf("%d cue(s) reference tracks that do not exist (stale index): run mkvgo reindex", r.UnknownTrackCues)
	case r.HasVideoTrack && r.VideoCues == 0:
		r.Reason = fmt.Sprintf("the index keys on non-video tracks only (%d cue(s), no video cue) - every seek lands mid-GOP: run mkvgo reindex", r.TotalCues)
	case r.HasVideoTrack && r.MaxVideoGapMs > maxSeekGapMs:
		r.Reason = sparseReason(r)
	default:
		r.Healthy = true
	}
	return r, nil
}

// maxSeekGapMs is the widest hole tolerated in a file's video cue coverage. Past
// it, a seek into the hole lands more than this far from its target, which is no
// longer seeking - and a reindex fixes it, writing one cue per cluster that
// holds a video keyframe (a few seconds apart on any real mux). Below it, the
// index does its job however many cues sit on other tracks.
const maxSeekGapMs = 30_000

// inexactTailPct bounds the tail that another track outlasting the picture can
// account for when the video's own end is NOT stated: the last video cue to the
// declared duration counts as a hole only past this share of the duration (and
// never under maxSeekGapMs). Measured on real files, sound outlasts picture by
// 31 to 108 s - 0.5 to 1.7% of the file - while an index that stops before the
// picture does leaves a stretch that grows with the file.
const inexactTailPct = 5

// measureVideoCoverage fills the report's coverage fields from the video cues.
// The worst hole is the widest of: 0 to the first cue (a seek before it lands
// on it), consecutive cues, and - when it counts - the last cue to the
// picture's end.
func measureVideoCoverage(r *CueHealthReport, c *mkv.Container, video map[uint64]bool) {
	times := make([]int64, 0, len(c.Cues))
	for _, cue := range c.Cues {
		if video[cue.Track] {
			times = append(times, cue.TimeMs)
		}
	}
	if len(times) == 0 {
		return // no video cue at all: the misskeyed rule speaks, not this one
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })

	r.MaxVideoGapMs, r.MaxVideoGapAtMs = times[0], 0
	for i := 1; i < len(times); i++ {
		if gap := times[i] - times[i-1]; gap > r.MaxVideoGapMs {
			r.MaxVideoGapMs, r.MaxVideoGapAtMs = gap, times[i-1]
		}
	}

	r.VideoEndMs = c.DurationMs
	if st, ok := trustedVideoStatistics(c, video); ok {
		r.VideoEndMs, r.VideoEndExact = st.durationMs, true
		r.VideoShortfallMs = st.shortfallMs()
	}
	last := times[len(times)-1]
	if r.VideoEndMs > last {
		r.TailGapMs = r.VideoEndMs - last
	}
	tailCounts := r.VideoEndExact || r.TailGapMs > inexactTailTolerance(c.DurationMs)
	if tailCounts && r.TailGapMs > r.MaxVideoGapMs {
		r.MaxVideoGapMs, r.MaxVideoGapAtMs = r.TailGapMs, last
	}
}

// tailIsWorst reports whether the hole MaxVideoGapMs names is the tail: it
// opens at the last cue and spans to the picture's end.
func tailIsWorst(r *CueHealthReport) bool {
	return r.TailGapMs > 0 && r.MaxVideoGapMs == r.TailGapMs && r.MaxVideoGapAtMs == r.LastCueMs
}

// inexactTailTolerance is the tail a declared duration may exceed the last video
// cue by before it counts as a hole: inexactTailPct of the duration, never under
// the sparse threshold.
func inexactTailTolerance(durationMs int64) int64 {
	if tol := durationMs * inexactTailPct / 100; tol > maxSeekGapMs {
		return tol
	}
	return maxSeekGapMs
}

// sparseReason spells out the hole MaxVideoGapMs names - WHERE it is, because
// the remedy depends on it: a tail is the cues stopping before the picture ends,
// a hole in the middle is either uncued keyframes (a reindex closes it) or
// picture missing from the stream (nothing closes it; the statistics say which).
func sparseReason(r *CueHealthReport) string {
	if tailIsWorst(r) {
		end := "the declared end"
		if r.VideoEndExact {
			end = "the picture ends"
		}
		return fmt.Sprintf("the video cues stop at %s, %.0fs before %s (%s) - a seek into that stretch lands on the last cue: run mkvgo reindex",
			clockMs(r.LastCueMs), secs(r.TailGapMs), end, clockMs(r.VideoEndMs))
	}
	s := fmt.Sprintf("the video cues leave a %.0fs hole at %s - a seek there lands that far from its target",
		secs(r.MaxVideoGapMs), clockMs(r.MaxVideoGapAtMs))
	if pictureMissing(r) {
		return s + fmt.Sprintf("; the picture itself is missing there: the file states about %.0fs less video than its duration at its frame rate holds. No index can restore it (a reindex leaves the hole): re-acquire the source",
			secs(r.VideoShortfallMs))
	}
	return s + ": run mkvgo reindex"
}

// pictureMissing reports whether the video track's own statistics account for
// the hole: enough picture is absent from the stream (past the sparse threshold,
// at least half the hole) that cueing cannot close it. A tail is never read
// this way - the statistics place the picture's end, and the tail is what lies
// before it uncued.
func pictureMissing(r *CueHealthReport) bool {
	return !tailIsWorst(r) && r.VideoShortfallMs > maxSeekGapMs && 2*r.VideoShortfallMs >= r.MaxVideoGapMs
}

// videoStatistics is what the statistics tags state for a video track.
type videoStatistics struct {
	durationMs int64
	frames     int64   // NUMBER_OF_FRAMES, 0 when absent
	fps        float64 // from the track's DefaultDuration, 0 when absent
}

// shortfallMs is the picture the declared frame count falls short of the
// duration at the frame rate, in ms; 0 when either is unknown or they add up.
func (st videoStatistics) shortfallMs() int64 {
	if st.frames <= 0 || st.fps <= 0 {
		return 0
	}
	expected := math.Round(float64(st.durationMs) / 1000 * st.fps)
	if missing := expected - float64(st.frames); missing > 0 {
		return int64(math.Round(missing / st.fps * 1000))
	}
	return 0
}

// trustedVideoStatistics returns the statistics of the video track whose stated
// duration is the longest, provided the tags describe THIS file. A tag survives
// remuxes that do not rewrite it - copied verbatim by a muxer, it certifies
// frames the file no longer holds - so, as mkvmerge does before trusting one,
// the tag's writing application must be the file's, its writing date the
// file's when both are stated, and its duration not past the declared one.
func trustedVideoStatistics(c *mkv.Container, video map[uint64]bool) (videoStatistics, bool) {
	var best videoStatistics
	var found bool
	for i := range c.Tracks {
		t := &c.Tracks[i]
		if !video[t.ID] {
			continue
		}
		for _, tag := range c.Tags {
			if tag.TargetID == 0 || tag.TargetID != trackUID(t) || !statisticsDescribeFile(c, tag.SimpleTags) {
				continue
			}
			durMs, err := mkv.ParseClockTime(simpleTagValue(tag.SimpleTags, "DURATION"))
			if err != nil || durMs <= 0 || (c.DurationMs > 0 && durMs > c.DurationMs+1000) {
				continue
			}
			if found && durMs <= best.durationMs {
				continue
			}
			st := videoStatistics{durationMs: durMs}
			if n, err := strconv.ParseInt(strings.TrimSpace(simpleTagValue(tag.SimpleTags, "NUMBER_OF_FRAMES")), 10, 64); err == nil && n > 0 {
				st.frames = n
			}
			if t.FrameRate != nil && *t.FrameRate > 0 {
				st.fps = *t.FrameRate
			} else if t.DefaultDurationNs > 0 {
				st.fps = 1e9 / float64(t.DefaultDurationNs)
			}
			best, found = st, true
		}
	}
	return best, found
}

// statisticsDescribeFile is the freshness check on a statistics tag set: the
// application that measured it must be the one that wrote the file, and, when
// both state a date, the same date.
func statisticsDescribeFile(c *mkv.Container, tags []mkv.SimpleTag) bool {
	app := simpleTagValue(tags, "_STATISTICS_WRITING_APP")
	if app == "" || app != c.Info.WritingApp {
		return false
	}
	if date := simpleTagValue(tags, "_STATISTICS_WRITING_DATE_UTC"); date != "" && c.Info.DateUTC != nil {
		return date == c.Info.DateUTC.UTC().Format("2006-01-02 15:04:05")
	}
	return true
}

// simpleTagValue returns the value of the named SimpleTag (case-insensitive), or
// "" when absent.
func simpleTagValue(tags []mkv.SimpleTag, name string) string {
	for _, st := range tags {
		if strings.EqualFold(st.Name, name) {
			return st.Value
		}
	}
	return ""
}

func secs(ms int64) float64 { return float64(ms) / 1000 }

// clockMs formats a millisecond instant as HH:MM:SS, the form the CLI prints.
func clockMs(ms int64) string {
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}
