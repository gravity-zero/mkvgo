package ops

import (
	"context"
	"fmt"
	"io"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// bitrateWindowMs is the sliding-window width for TrackStats.PeakBitrateBps:
// the densest 1-second span of frame bytes seen.
const bitrateWindowMs = 1000

// durationMismatchWarnMs is the reconciliation threshold between the Info
// element's declared duration and the true (walked) duration.
const durationMismatchWarnMs = 1000

// backwardTimecodeWarnMs bounds a harmless B-frame-reordering dip (flagged
// instead by TrackStats.Reordered) from a real backward jump worth a Warning.
const backwardTimecodeWarnMs = 1000

// frameDurationToleranceMs is the +-1ms slack allowed between consecutive
// video frame-timecode deltas before TrackStats.FrameRateMode reports "vfr":
// Matroska timecodes are millisecond-scale, so exact-equal deltas would be
// too strict a test for a genuinely constant frame rate.
const frameDurationToleranceMs = 1

// TrackStats summarises one track's block-header walk: exact frame/keyframe
// counts, byte totals and derived rates, computed from (Simple)Block headers
// alone, never a decoded sample. See Analyze.
type TrackStats struct {
	TrackID uint64 `json:"track_id"`
	// Type is "video"/"audio"/"subtitle" (mkv.TrackType, as a plain string for JSON).
	Type  string `json:"type"`
	Codec string `json:"codec"`
	// Frames is the exact frame count, lacing expanded: a laced (audio) block
	// stores several frames under one stored (Simple)Block, and each is
	// counted individually here.
	Frames int64 `json:"frames"`
	// Packets is the count of STORED (Simple)Block/BlockGroup elements - what a
	// laced track's Frames divides among. Packets == Frames for an unlaced
	// track (real-world video is never laced).
	Packets   int64 `json:"packets"`
	Keyframes int64 `json:"keyframes"`
	// Bytes is the sum of frame payload sizes seen (Block.Size, populated even
	// in the header-only walk Analyze uses).
	Bytes int64 `json:"bytes"`
	// DurationMs is this track's last frame's end time (timecode + duration),
	// the maximum seen over every frame of the track.
	DurationMs     int64 `json:"duration_ms"`
	AvgBitrateBps  int64 `json:"avg_bitrate_bps"`
	PeakBitrateBps int64 `json:"peak_bitrate_bps"`
	// MinGopFrames/MaxGopFrames/AvgGopFrames are frame-count spans between
	// consecutive VIDEO keyframes. Zero for non-video tracks - audio has no
	// GOP structure, every frame is independently decodable.
	MinGopFrames       int     `json:"min_gop_frames,omitempty"`
	MaxGopFrames       int     `json:"max_gop_frames,omitempty"`
	AvgGopFrames       float64 `json:"avg_gop_frames,omitempty"`
	KeyframeEveryMsAvg int64   `json:"keyframe_every_ms_avg,omitempty"`
	// Reordered is a decode-free HEURISTIC (video only): a presentation
	// timecode (Block.Timecode) that goes backwards in decode order (the
	// order Next delivers blocks) is consistent with B-frame reordering, but
	// is not a certainty - treat it as a hint, not a decoded fact.
	Reordered    bool    `json:"reordered,omitempty"`
	FrameRateAvg float64 `json:"frame_rate_avg,omitempty"`
	// FrameRateMode classifies a VIDEO track as "cfr" (constant frame rate) or
	// "vfr" (variable), derived decode-free from consecutive frame-timecode
	// deltas (see frameDurationToleranceMs). "" when unknown: fewer than 2
	// frames, or a non-video track (CFR/VFR is a video concept here).
	FrameRateMode string `json:"frame_rate_mode,omitempty"`
	// FrameDurationVarianceNs is the spread (max delta - min delta) between
	// consecutive video frame timecodes, in nanoseconds: 0 for a perfect CFR
	// track, a diagnostic magnitude for VFR. 0 alongside FrameRateMode == ""
	// simply means no measurement was possible.
	FrameDurationVarianceNs int64 `json:"frame_duration_variance_ns,omitempty"`
}

// AnalyzeReport is the result of a structural, no-decode stream-statistics
// pass over a Matroska/WebM file. See Analyze.
type AnalyzeReport struct {
	// DurationMs is the container's TRUE duration: the latest track end seen
	// during the walk (max over Tracks[i].DurationMs).
	DurationMs int64 `json:"duration_ms"`
	// DeclaredDurationMs is the Segment Info Duration element - see Warnings
	// for a mismatch against the walked DurationMs.
	DeclaredDurationMs int64        `json:"declared_duration_ms"`
	OverallBitrateBps  int64        `json:"overall_bitrate_bps"`
	ClusterCount       int64        `json:"cluster_count"`
	BlockCount         int64        `json:"block_count"`
	Tracks             []TrackStats `json:"tracks"`
	// Warnings flags timing sanity issues found during the walk: a declared
	// vs. true duration mismatch, a backward timecode jump, a track with zero
	// frames, or a track whose frame durations could not be determined.
	Warnings []string `json:"warnings,omitempty"`
}

// winEntry is one frame kept in a track's trailing-1-second bitrate window.
type winEntry struct {
	tsMs int64
	size int64
}

// trackAcc accumulates one track's stats across the block walk. Its memory is
// bounded: the bitrate window only ever holds the frames within the trailing
// second, never the whole track.
type trackAcc struct {
	track mkv.Track

	frames, packets, keyframes, bytes int64

	lastBlockTC int64
	haveBlockTC bool

	durationMs   int64
	haveDuration bool // a non-zero frame duration was derived at least once

	win      []winEntry
	winBytes int64
	peakBps  int64

	// GOP tracking (video only): the frame count since the last keyframe, and
	// running min/max/sum/count over completed GOPs - never the GOPs
	// themselves.
	framesInGOP int
	haveGOP     bool
	gopMin      int
	gopMax      int
	gopSum      int64
	gopCount    int64

	lastKfTC   int64
	haveKf     bool
	kfGapSum   int64
	kfGapCount int64

	lastTimecode   int64
	haveTimecode   bool
	reordered      bool
	backwardWarned bool

	// Frame-duration variance tracking (video only): the delta between this
	// and the previous frame's Timecode, min/max seen so far - never the
	// deltas themselves, keeping memory bounded.
	fdPrevTC    int64
	fdHavePrev  bool
	fdMinDelta  int64
	fdMaxDelta  int64
	fdHaveDelta bool
}

// closeGOP records the just-finished GOP's frame count (the frames from the
// previous keyframe up to, but excluding, the new one).
func (a *trackAcc) closeGOP() {
	if !a.haveGOP {
		return
	}
	n := a.framesInGOP
	if a.gopCount == 0 || n < a.gopMin {
		a.gopMin = n
	}
	if n > a.gopMax {
		a.gopMax = n
	}
	a.gopSum += int64(n)
	a.gopCount++
}

// addToWindow slides the trailing-1-second bitrate window forward, dropping
// frames that fell out of it, and reports the resulting bits/s.
func (a *trackAcc) addToWindow(tsMs, size int64) int64 {
	a.win = append(a.win, winEntry{tsMs, size})
	a.winBytes += size
	for len(a.win) > 0 && a.win[0].tsMs <= tsMs-bitrateWindowMs {
		a.winBytes -= a.win[0].size
		a.win = a.win[1:]
	}
	bps := a.winBytes * 8
	if bps > a.peakBps {
		a.peakBps = bps
	}
	return bps
}

// Analyze walks path's block headers (never a decoded sample) to compute
// per-track and container-wide stream statistics: exact frame/keyframe
// counts, byte totals, average/peak bitrate, GOP spans and a duration
// reconciliation - the mkvgo equivalent of a frame-accurate stream-stats probe.
//
// The walk is HEAD-ONLY: BlockReader.SetHeaderOnly discards each unlaced
// block's payload instead of reading it (Block.Size is still reported), so
// the cost is proportional to the block-HEADER count, never the media
// volume. A laced block's lacing header still has to be decoded to size its
// frames, but its payload is dropped right after, never held. Memory is
// bounded: only small per-track counters and a trailing-1-second bitrate
// window are kept, never the blocks themselves.
func Analyze(ctx context.Context, path string, opts ...mkv.Options) (*AnalyzeReport, error) {
	fs := mkv.FSFrom(opts)

	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return nil, fmt.Errorf("cannot parse: %w", err)
	}

	f, err := fs.DoOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	br, err := reader.NewBlockReader(f, c.Info.TimecodeScale)
	if err != nil {
		return nil, fmt.Errorf("cannot read clusters: %w", err)
	}
	br.SetHeaderOnly(true)
	defaultDurNs := reader.TrackDefaultDurations(c.Tracks)
	br.SetTrackDefaultDurations(defaultDurNs)

	accs := make(map[uint64]*trackAcc, len(c.Tracks))
	for _, t := range c.Tracks {
		accs[t.ID] = &trackAcc{track: t}
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		blk, err := br.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read blocks: %w", err)
		}
		acc, ok := accs[blk.TrackNumber]
		if !ok {
			continue // block for a track absent from Tracks (malformed source)
		}

		acc.frames++
		if !acc.haveBlockTC || blk.BlockTimecode != acc.lastBlockTC {
			acc.packets++
			acc.lastBlockTC = blk.BlockTimecode
			acc.haveBlockTC = true
		}
		if blk.Keyframe {
			acc.keyframes++
		}
		acc.bytes += blk.Size

		durMs := blk.Duration
		if durMs == 0 {
			if ns, ok := defaultDurNs[blk.TrackNumber]; ok {
				durMs = (ns + 500_000) / 1_000_000
			}
		}
		if durMs > 0 {
			acc.haveDuration = true
		}
		if end := blk.Timecode + durMs; end > acc.durationMs {
			acc.durationMs = end
		}

		acc.addToWindow(blk.Timecode, blk.Size)

		if acc.haveTimecode && blk.Timecode < acc.lastTimecode {
			if acc.track.Type == mkv.VideoTrack {
				acc.reordered = true
			}
			if acc.lastTimecode-blk.Timecode > backwardTimecodeWarnMs {
				acc.backwardWarned = true
			}
		}
		acc.lastTimecode = blk.Timecode
		acc.haveTimecode = true

		if acc.track.Type == mkv.VideoTrack {
			if acc.fdHavePrev {
				delta := blk.Timecode - acc.fdPrevTC
				if !acc.fdHaveDelta {
					acc.fdMinDelta, acc.fdMaxDelta = delta, delta
					acc.fdHaveDelta = true
				} else {
					if delta < acc.fdMinDelta {
						acc.fdMinDelta = delta
					}
					if delta > acc.fdMaxDelta {
						acc.fdMaxDelta = delta
					}
				}
			}
			acc.fdPrevTC = blk.Timecode
			acc.fdHavePrev = true

			if blk.Keyframe {
				acc.closeGOP()
				acc.haveGOP = true
				acc.framesInGOP = 0
				if acc.haveKf {
					acc.kfGapSum += blk.Timecode - acc.lastKfTC
					acc.kfGapCount++
				}
				acc.lastKfTC = blk.Timecode
				acc.haveKf = true
			}
			acc.framesInGOP++
		}
	}

	report := &AnalyzeReport{
		DeclaredDurationMs: c.DurationMs,
		ClusterCount:       br.ClusterCount(),
	}

	var totalBytes int64
	for _, t := range c.Tracks {
		acc := accs[t.ID]
		ts := TrackStats{
			TrackID: t.ID, Type: string(t.Type), Codec: t.Codec,
			Frames: acc.frames, Packets: acc.packets, Keyframes: acc.keyframes,
			Bytes: acc.bytes, DurationMs: acc.durationMs, PeakBitrateBps: acc.peakBps,
		}
		if acc.durationMs > 0 {
			ts.AvgBitrateBps = acc.bytes * 8 * 1000 / acc.durationMs
			ts.FrameRateAvg = float64(acc.frames) * 1000 / float64(acc.durationMs)
		}
		if t.Type == mkv.VideoTrack {
			if acc.gopCount > 0 {
				ts.MinGopFrames = acc.gopMin
				ts.MaxGopFrames = acc.gopMax
				ts.AvgGopFrames = float64(acc.gopSum) / float64(acc.gopCount)
			}
			if acc.kfGapCount > 0 {
				ts.KeyframeEveryMsAvg = acc.kfGapSum / acc.kfGapCount
			}
			ts.Reordered = acc.reordered

			if acc.fdHaveDelta {
				spreadMs := acc.fdMaxDelta - acc.fdMinDelta
				ts.FrameDurationVarianceNs = spreadMs * 1_000_000
				if spreadMs <= frameDurationToleranceMs {
					ts.FrameRateMode = "cfr"
				} else {
					ts.FrameRateMode = "vfr"
					report.Warnings = append(report.Warnings,
						fmt.Sprintf("track %d (video): variable frame rate (vfr) detected, some pipelines assume constant frame rate", t.ID))
				}
			}
		}
		report.Tracks = append(report.Tracks, ts)
		report.BlockCount += acc.packets
		totalBytes += acc.bytes
		if acc.durationMs > report.DurationMs {
			report.DurationMs = acc.durationMs
		}

		switch {
		case acc.frames == 0:
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("track %d (%s): no frames found", t.ID, t.Type))
		case !acc.haveDuration:
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("track %d (%s): no frame duration available - the last frame's true end time is a guess", t.ID, t.Type))
		}
		if acc.backwardWarned {
			report.Warnings = append(report.Warnings,
				fmt.Sprintf("track %d (%s): timecode jumped backwards by more than %dms", t.ID, t.Type, backwardTimecodeWarnMs))
		}
	}

	if report.DurationMs > 0 {
		report.OverallBitrateBps = totalBytes * 8 * 1000 / report.DurationMs
	}
	if report.DeclaredDurationMs > 0 && report.DurationMs > 0 {
		diff := report.DeclaredDurationMs - report.DurationMs
		if diff < 0 {
			diff = -diff
		}
		if diff > durationMismatchWarnMs {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"declared duration %dms differs from the walked true duration %dms by more than %dms",
				report.DeclaredDurationMs, report.DurationMs, durationMismatchWarnMs))
		}
	}

	return report, nil
}
