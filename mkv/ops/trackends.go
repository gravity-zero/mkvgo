package ops

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// trackends.go - TrackEnds answers "where does each track's content REALLY
// end?", which the container never says: its declared duration is the longest
// track's end, and every other track may stop anywhere before it. Two defects
// hide behind that number. An audio track that dies minutes before the picture
// leaves a structurally healthy file - index fine, sizes coherent - whose
// playlists promise audio segments that can never exist. And measured against
// the declared end, a perfectly cued video looked 30 to 110 s short of its own
// index (the audio outlasting it), a false verdict served for a month.
//
// Content is measured against content, one track against another, and the
// declared duration is kept only as a ceiling. Two stages, cheapest first:
//
//  1. STATISTICS: mkvmerge and mkvgo stamp every track with a DURATION tag. A
//     tag is trusted only when it describes THIS file - written by the same
//     application and on the same date as the file, and not past the declared
//     duration - the checks mkvmerge applies before believing one, because a
//     tag copied through a remux certifies frames the file no longer holds.
//     One head-only read settles every track so stamped.
//  2. TAIL WALK: for the rest, a cue a window before the declared end is the
//     starting point, and the final clusters are walked - block headers alone,
//     payloads skipped by size, constant memory - keeping each track's last
//     block. The window widens once for a deep gap (the motivating file was
//     249 s short); a track still silent past the widest window ended at or
//     before it, which is reported as such ("walk-bound") rather than guessed.
//     A file with no index to start from is walked from its first cluster,
//     header-only, the cost of an Analyze - and so is one whose cue does not
//     land on a cluster (a stale index, the very thing a scan may be
//     diagnosing). The walk stops at the first undecodable byte past the
//     clusters (trailing junk is ordinary) and keeps what it saw; a track not
//     seen before such a stop is left unknown rather than bounded, because
//     what the walk never reached cannot be pronounced silent.

// TrackEndsReport is defined in the mkv package (see mkv/reports.go).
type TrackEndsReport = mkv.TrackEndsReport

// TrackEnds reports where each of path's tracks really ends, the picture's end
// and any audio track's shortfall against it. See the file comment for how
// each end is established and what its Source means.
func TrackEnds(ctx context.Context, path string, opts ...mkv.Options) (*TrackEndsReport, error) {
	fs := mkv.FSFrom(opts)
	meta, err := reader.OpenMetaWithFS(ctx, path, fs, reader.WithTags(), reader.WithCues())
	if err != nil {
		return nil, fmt.Errorf("track ends: %w", err)
	}
	r := &TrackEndsReport{DeclaredDurationMs: meta.DurationMs}
	stats := trustedTrackStatistics(meta)
	want := map[uint64]bool{}
	// Sized once: the walk keeps pointers into Ends, which a growing append
	// would leave aimed at a discarded backing array.
	r.Ends = make([]mkv.TrackEnd, 0, len(meta.Tracks))
	ends := make(map[uint64]*mkv.TrackEnd, len(meta.Tracks))
	for _, t := range meta.Tracks {
		r.Ends = append(r.Ends, mkv.TrackEnd{Track: t.ID, Type: t.Type})
		e := &r.Ends[len(r.Ends)-1]
		ends[t.ID] = e
		if st, ok := stats[t.ID]; ok {
			e.EndMs, e.Source = st.durationMs, "statistics"
			continue
		}
		want[t.ID] = true
	}
	if len(want) > 0 {
		if err := walkTrackEnds(ctx, path, fs, meta, want, ends); err != nil {
			return nil, fmt.Errorf("track ends: %w", err)
		}
	}
	judgeTrackEnds(r)
	return r, nil
}

// tailWalkWindowsMs are the successive "walk the last N seconds" windows of the
// tail walk. The first covers ordinary files (every track runs to the end); the
// second measures a deep gap. A track silent past the last one is reported at
// its start, a bound already far past any threshold a caller would flag at.
var tailWalkWindowsMs = []int64{120_000, 900_000}

// walkTrackEnds establishes the ends of the tracks in want by walking the tail.
func walkTrackEnds(ctx context.Context, path string, fs *mkv.FS, meta *mkv.Container, want map[uint64]bool, ends map[uint64]*mkv.TrackEnd) error {
	f, err := fs.DoOpen(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scale := meta.Info.TimecodeScale
	if scale <= 0 {
		scale = 1_000_000
	}
	durs := reader.TrackDefaultDurations(meta.Tracks)

	pending := make(map[uint64]bool, len(want))
	for id := range want {
		pending[id] = true
	}
	var windowStart int64
	complete := false // the last walk reached the end of the clusters
	for _, window := range tailWalkWindowsMs {
		if err := ctx.Err(); err != nil {
			return err
		}
		windowStart = meta.DurationMs - window
		if windowStart < 0 || meta.DurationMs <= 0 {
			windowStart = 0
		}
		startOff := cueOffsetAtOrBefore(meta, windowStart)
		if startOff < 0 {
			windowStart = 0
		}
		last, clusters, done, err := walkTailFrom(f, scale, startOff, durs, pending)
		if err != nil {
			return err
		}
		if clusters == 0 && startOff >= 0 {
			// The cue's position is not a cluster: a stale index. Walk from
			// the first cluster instead, the one start that cannot lie.
			windowStart = 0
			if last, _, done, err = walkTailFrom(f, scale, -1, durs, pending); err != nil {
				return err
			}
		}
		complete = done
		for id, end := range last {
			ends[id].EndMs, ends[id].Source = end, "walk"
			delete(pending, id)
		}
		if len(pending) == 0 || windowStart == 0 || !complete {
			break
		}
	}
	for id := range pending {
		// Silent through the widest window walked - or through the whole file
		// when the walk started at its first cluster, which is then simply
		// "never seen" and left unknown; so is a track the walk stopped short
		// of, since what was not reached cannot be pronounced silent.
		if windowStart > 0 && complete {
			ends[id].EndMs, ends[id].Source = windowStart, "walk-bound"
		}
	}
	return nil
}

// walkTailFrom walks the clusters from startOff (-1: from the first cluster)
// to the end of the clusters, header-only, and returns the end of the last
// block seen per pending track, the clusters entered, and whether the walk
// reached the end cleanly (false: stopped on an undecodable element - junk past
// the clusters, or damage - keeping what it saw).
func walkTailFrom(f io.ReadSeeker, scale, startOff int64, durs map[uint64]int64, pending map[uint64]bool) (last map[uint64]int64, clusters int64, done bool, err error) {
	var br *reader.BlockReader
	if startOff < 0 {
		// NewBlockReader parses the EBML header from the current position:
		// the file is at wherever the previous walk left it.
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return nil, 0, false, err
		}
		br, err = reader.NewBlockReader(f, scale)
	} else {
		br, err = reader.NewBlockReaderAt(f, scale, startOff)
	}
	if err != nil {
		return nil, 0, false, err
	}
	br.SetHeaderOnly(true)
	br.SetTrackDefaultDurations(durs)
	last = map[uint64]int64{}
	for {
		blk, nerr := br.Next()
		if nerr == io.EOF {
			return last, br.ClusterCount(), true, nil
		}
		if nerr != nil {
			return last, br.ClusterCount(), false, nil
		}
		if !pending[blk.TrackNumber] {
			continue
		}
		end := blk.Timecode + blk.Duration
		if blk.Duration == 0 {
			end += durs[blk.TrackNumber] / 1_000_000
		}
		if end > last[blk.TrackNumber] {
			last[blk.TrackNumber] = end
		}
	}
}

// cueOffsetAtOrBefore returns the absolute offset of the cluster holding the
// latest cue (any track) at or before wantMs, or -1 when the index has none.
func cueOffsetAtOrBefore(meta *mkv.Container, wantMs int64) int64 {
	best, off := int64(-1), int64(-1)
	for _, c := range meta.Cues {
		if c.TimeMs <= wantMs && c.TimeMs > best {
			best, off = c.TimeMs, meta.SegmentStart+c.ClusterPos
		}
	}
	return off
}

// judgeTrackEnds derives the picture's end and the audio shortfall from the
// per-track ends: the latest known video end, and the earliest known audio
// end against it. Subtitles are reported, never judged - they end whenever
// their last cue does.
func judgeTrackEnds(r *TrackEndsReport) {
	for _, e := range r.Ends {
		if e.Type == mkv.VideoTrack && e.Source != "" && e.EndMs > r.VideoEndMs {
			r.VideoEndMs = e.EndMs
		}
	}
	if r.VideoEndMs == 0 {
		return
	}
	for _, e := range r.Ends {
		if e.Type != mkv.AudioTrack || e.Source == "" {
			continue
		}
		if short := r.VideoEndMs - e.EndMs; short > r.AudioShortfallMs {
			r.AudioShortfallMs, r.ShortAudioTrack = short, e.Track
		}
	}
}

// trackStatistics is what a track's statistics tags state, once trusted.
type trackStatistics struct {
	durationMs int64
	frames     int64 // NUMBER_OF_FRAMES, 0 when absent
}

// trustedTrackStatistics returns, by track number, the statistics of every
// track whose tags describe THIS file. A tag survives remuxes that do not
// rewrite it - copied verbatim by a muxer, it certifies frames the file no
// longer holds - so, as mkvmerge does before trusting one, the tag's writing
// application must be the file's, its writing date the file's when both are
// stated, and its duration not past the declared one.
func trustedTrackStatistics(c *mkv.Container) map[uint64]trackStatistics {
	out := map[uint64]trackStatistics{}
	for i := range c.Tracks {
		t := &c.Tracks[i]
		for _, tag := range c.Tags {
			if tag.TargetID == 0 || tag.TargetID != trackUID(t) || !statisticsDescribeFile(c, tag.SimpleTags) {
				continue
			}
			durMs, err := mkv.ParseClockTime(simpleTagValue(tag.SimpleTags, "DURATION"))
			if err != nil || durMs <= 0 || (c.DurationMs > 0 && durMs > c.DurationMs+1000) {
				continue
			}
			st := trackStatistics{durationMs: durMs}
			if n, err := strconv.ParseInt(strings.TrimSpace(simpleTagValue(tag.SimpleTags, "NUMBER_OF_FRAMES")), 10, 64); err == nil && n > 0 {
				st.frames = n
			}
			out[t.ID] = st
		}
	}
	return out
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
