package ops

// ladder.go - a pure, deterministic ABR-ladder recommender: given a source's
// resolution/bitrate/codec, suggest a sensible set of lower-quality rungs to
// encode. This is guidance, not a guarantee: mkvgo never transcodes, so the
// actual encode is always an external step. The rules here are a documented,
// conservative rule of thumb (standard per-rung baseline bitrates, a codec
// efficiency factor, a frame-rate bump), not a substitute for real per-title
// encoding analysis.

import (
	"context"
	"fmt"
	"math"

	"github.com/gravity-zero/mkvgo/mkv"
)

// Rung is one ABR ladder step.
type Rung struct {
	Width       int
	Height      int
	BitrateKbps int
	Label       string // "2160p", "1080p", "720p", "480p", "360p"
}

// LadderInput is the source facts RecommendLadder needs. SourceBitrateKbps
// and FrameRate may be 0 when unknown - RecommendLadder then skips the
// bitrate cap / frame-rate bump that field would otherwise apply.
type LadderInput struct {
	SourceWidth       int
	SourceHeight      int
	SourceBitrateKbps int
	Codec             string
	FrameRate         float64
}

// standardRung is one row of the baseline H.264 ladder this package ships
// with: an editorial, widely used rule-of-thumb bitrate for that resolution.
type standardRung struct {
	Width, Height, BitrateKbps int
	Label                      string
}

// standardLadder is deliberately a plain data table, tallest first, so it
// stays easy to review/update. Bitrates are the H.264 baseline; codecEfficiency
// scales them down for more efficient codecs.
var standardLadder = []standardRung{
	{Width: 3840, Height: 2160, BitrateKbps: 16000, Label: "2160p"},
	{Width: 1920, Height: 1080, BitrateKbps: 6000, Label: "1080p"},
	{Width: 1280, Height: 720, BitrateKbps: 3000, Label: "720p"},
	{Width: 854, Height: 480, BitrateKbps: 1200, Label: "480p"},
	{Width: 640, Height: 360, BitrateKbps: 700, Label: "360p"},
}

// codecEfficiency is the per-codec bitrate multiplier applied to the H.264
// baseline bitrate for the same perceptual quality - an approximate,
// documented rule of thumb, not a measured per-title result. Codecs absent
// from this table (including an empty/unknown Codec) get 1.0 (no assumed
// gain over H.264).
var codecEfficiency = map[string]float64{
	"h264": 1.0,
	"vp8":  1.0,
	"hevc": 0.6,
	"vp9":  0.65,
	"av1":  0.5,
}

// highFrameRateFactor bumps the bitrate for content shot above 30fps (more
// motion to encode per second needs more bits for the same quality); applied
// only when FrameRate is known and above the threshold.
const (
	highFrameRateThreshold = 30.0
	highFrameRateFactor    = 1.3
)

// RecommendLadder returns a sensible ABR ladder capped at the source
// resolution and bitrate: it never upscales (no rung taller/wider than the
// source) and never recommends a bitrate above SourceBitrateKbps (when known).
// Rungs are returned tallest first, with monotonically non-decreasing
// bitrates. When the source is shorter than every standard rung, a single
// rung matching the source's own resolution is returned instead of an empty
// ladder.
func RecommendLadder(in LadderInput) []Rung {
	factor, ok := codecEfficiency[canonicalCodec(in.Codec)]
	if !ok {
		factor = 1.0
	}
	if in.FrameRate > highFrameRateThreshold {
		factor *= highFrameRateFactor
	}

	var rungs []Rung
	for _, r := range standardLadder {
		if in.SourceHeight > 0 && r.Height > in.SourceHeight {
			continue
		}
		if in.SourceWidth > 0 && r.Width > in.SourceWidth {
			continue
		}
		rungs = append(rungs, Rung{
			Width:       r.Width,
			Height:      r.Height,
			BitrateKbps: cappedBitrate(r.BitrateKbps, factor, in.SourceBitrateKbps),
			Label:       r.Label,
		})
	}

	if len(rungs) == 0 && in.SourceWidth > 0 && in.SourceHeight > 0 {
		base := standardLadder[len(standardLadder)-1].BitrateKbps
		rungs = append(rungs, Rung{
			Width:       in.SourceWidth,
			Height:      in.SourceHeight,
			BitrateKbps: cappedBitrate(base, factor, in.SourceBitrateKbps),
			Label:       standardLadder[len(standardLadder)-1].Label,
		})
	}
	return rungs
}

// cappedBitrate scales baseKbps by factor and, when sourceKbps is known
// (> 0), never returns more than it - applied uniformly to every rung so the
// per-resolution ordering (and hence monotonicity) of the scaled baseline is
// preserved even when the source bitrate is low enough to clip several rungs
// to the same value.
func cappedBitrate(baseKbps int, factor float64, sourceKbps int) int {
	v := int(math.Round(float64(baseKbps) * factor))
	if sourceKbps > 0 && v > sourceKbps {
		v = sourceKbps
	}
	return v
}

// RecommendLadderFor derives a LadderInput from a file's video track (head-
// only metadata: Matroska/WebM or MP4/MOV) and returns RecommendLadder's
// result. Returns an error when the source has no video track.
func RecommendLadderFor(ctx context.Context, path string, opts ...mkv.Options) ([]Rung, error) {
	c, _, err := openAnyMeta(ctx, path, mkv.FSFrom(opts))
	if err != nil {
		return nil, err
	}
	var video *mkv.Track
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.VideoTrack {
			video = &c.Tracks[i]
			break
		}
	}
	if video == nil {
		return nil, fmt.Errorf("%s: no video track", path)
	}

	in := LadderInput{Codec: canonicalCodec(video.Codec)}
	if video.Width != nil {
		in.SourceWidth = int(*video.Width)
	}
	if video.Height != nil {
		in.SourceHeight = int(*video.Height)
	}
	if video.Bitrate != nil {
		in.SourceBitrateKbps = int(*video.Bitrate / 1000)
	}
	if video.FrameRate != nil {
		in.FrameRate = *video.FrameRate
	}
	return RecommendLadder(in), nil
}
