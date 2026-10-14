package mp4

import (
	"slices"

	"github.com/gravity-zero/mkvgo/mkv"
)

// CueDensity says how densely the video cue points a Matroska plan cuts on
// index the keyframes. A plan only cuts where a cue points, so its segments
// match the full pass's (which sees every keyframe) only when the Cues index
// every keyframe: a boundary due on an uncued keyframe moves to the next cue.
// A MaxGapMs well above MedianGapMs is the sign of skipped keyframes, though
// a source with an irregular GOP (scene-cut keyframes) shows it too.
type CueDensity struct {
	// CuePoints is the number of video keyframes the index holds.
	CuePoints int `json:"cue_points"`
	// MaxGapMs is the widest interval between two consecutive video cue
	// points, or from 0 to the first; MaxGapAtMs is where it opens.
	MaxGapMs   int64 `json:"max_gap_ms"`
	MaxGapAtMs int64 `json:"max_gap_at_ms"`
	// MedianGapMs is the median interval between consecutive video cue
	// points: the source's usual keyframe spacing when the Cues hold them all.
	MedianGapMs int64 `json:"median_gap_ms"`
	// LastCueMs is the last video cue point: the index says nothing past it.
	LastCueMs int64 `json:"last_cue_ms"`
	// Synthesized is set when Options.SynthesizeIndex built the cue points
	// from a walk of the source: then every keyframe is indexed.
	Synthesized bool `json:"synthesized"`
}

// CueDensity reports the density of a Matroska plan's video cue points; ok is false for an MP4 plan, exact by construction.
func (p *HLSPlan) CueDensity() (d CueDensity, ok bool) {
	return p.cueDensity, !p.mp4src
}

// cueDensityOf measures the video cue points a plan cuts on.
func cueDensityOf(cues []mkv.CuePoint, synthesized bool) CueDensity {
	d := CueDensity{CuePoints: len(cues), Synthesized: synthesized}
	prev := int64(0)
	gaps := make([]int64, 0, len(cues))
	for i, cue := range cues {
		gap := cue.TimeMs - prev
		if gap > d.MaxGapMs {
			d.MaxGapMs, d.MaxGapAtMs = gap, prev
		}
		if i > 0 {
			gaps = append(gaps, gap)
		}
		prev = cue.TimeMs
	}
	d.LastCueMs = prev
	if len(gaps) > 0 {
		slices.Sort(gaps)
		d.MedianGapMs = gaps[len(gaps)/2]
	}
	return d
}
