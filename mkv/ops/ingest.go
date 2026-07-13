package ops

// ingest.go - Ingest is the one-call onboarding path a media server uses to
// decide how a source file should be served to a target: it composes
// Playability (the decision), RecommendLadderFor (the transcode ladder) and
// ReindexInPlace (the one repair a remux decision may call for) into a single
// ServingPlan. Ingest never decodes or transcodes anything itself - a
// "transcode" verdict only returns a recommended ABR ladder for an external
// encoder to run.

import (
	"context"
	"errors"
	"fmt"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// ServingStrategy is Ingest's top-level recommendation for how a file should
// be delivered to a target.
type ServingStrategy string

const (
	// StrategyDirectPlay: serve the source file as-is (byte-range). Every
	// track already plays on the target from its current container.
	StrategyDirectPlay ServingStrategy = "direct-play"
	// StrategyRemuxHLS: package on-demand HLS/CMAF (or another remux
	// container) from the source - no transcode, every track's codec is kept.
	StrategyRemuxHLS ServingStrategy = "remux-hls"
	// StrategyTranscode: at least one track's codec/level/profile is not
	// carried by any container the target accepts; a real re-encode is
	// required and is outside mkvgo's scope.
	StrategyTranscode ServingStrategy = "transcode"
)

// ServingPlan is the result of Ingest: the serving decision, plus whatever
// repair (a seek-index reindex) that decision called for.
type ServingPlan struct {
	Strategy ServingStrategy `json:"strategy"`
	Target   string          `json:"target"`
	// SourceContainer is the source's on-disk container as mkvgo's own
	// head-only sniffing resolves it: "mkv" (also covers WebM - see
	// openAnyMeta) or "mp4".
	SourceContainer string `json:"source_container"`
	// RemuxContainer is the cheapest container the target accepts that keeps
	// every kept track's codec, populated only when Strategy is
	// StrategyRemuxHLS.
	RemuxContainer string `json:"remux_container,omitempty"`
	// HasSeekIndex reports whether the source already carries a Cues index
	// on-demand HLS can seek through - cue points on the VIDEO track, which is
	// what PlanHLS cuts its segments on - reachable without a cluster walk
	// (reader.WithCues). An index that only cues the audio does not count: it
	// would fail at serving time. Ready for packaging without any repair.
	HasSeekIndex bool `json:"has_seek_index"`
	// NeedsReindex is true when Strategy is StrategyRemuxHLS and the source
	// has no head-discoverable seek index yet: a reindex is required before
	// on-demand HLS can seek into it.
	NeedsReindex bool `json:"needs_reindex"`
	// Reindexed is true when Ingest itself performed an in-place reindex
	// during this call (opts.Reindex was set and it succeeded).
	Reindexed bool `json:"reindexed"`
	// ReindexInPlacePossible reports whether an in-place reindex works for
	// this file's layout. False means the caller must fall back to a copy
	// reindex (ops.Reindex/ReindexReplace) to get a head-discoverable index.
	// Only meaningful once a reindex was attempted (Ingest sets it either
	// way when opts.Reindex triggers an attempt).
	ReindexInPlacePossible bool               `json:"reindex_in_place_possible"`
	Playability            *PlayabilityReport `json:"playability,omitempty"`
	// Analysis is populated only when IngestOptions.IncludeAnalysis is set.
	Analysis *AnalyzeReport `json:"analysis,omitempty"`
	// Ladder is populated only when Strategy is StrategyTranscode.
	Ladder []Rung `json:"ladder,omitempty"`
	// Reasons is the human-readable decision trail: one short line per
	// decision Ingest made, in order.
	Reasons []string `json:"reasons"`
}

// IngestOptions configures Ingest.
type IngestOptions struct {
	mkv.Options
	// Target is the playback target name (see TargetByName). Defaults to
	// "mse-generic" when empty.
	Target string
	// IncludeAnalysis also runs Analyze and attaches its report, regardless
	// of the chosen strategy.
	IncludeAnalysis bool
	// Reindex performs an in-place reindex when a remux-hls decision finds
	// no usable seek index. When ReindexInPlace reports
	// ErrIndexNotHeadDiscoverable, Ingest does not fail: it flags the plan
	// (ReindexInPlacePossible=false) and leaves the caller to run a copy
	// reindex instead.
	Reindex bool
}

// Ingest composes Playability, RecommendLadderFor and (optionally)
// ReindexInPlace into one serving-plan decision for path against
// opts.Target. It performs no decode and no transcode: StrategyTranscode
// only returns a recommended ladder for an external encoder to act on.
func Ingest(ctx context.Context, path string, opts IngestOptions) (*ServingPlan, error) {
	targetName := opts.Target
	if targetName == "" {
		targetName = "mse-generic"
	}
	target, ok := TargetByName(targetName)
	if !ok {
		return nil, fmt.Errorf("ingest: unknown target %q", targetName)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c, srcContainer, err := openAnyMeta(ctx, path, opts.FS)
	if err != nil {
		return nil, fmt.Errorf("ingest: %w", err)
	}

	plan := &ServingPlan{
		Target:          target.Name,
		SourceContainer: srcContainer,
		Playability:     evaluatePlayability(c.Tracks, srcContainer, target),
	}
	plan.Reasons = append(plan.Reasons, fmt.Sprintf("target %q, source container %q", target.Name, srcContainer))

	switch plan.Playability.OverallVerdict {
	case verdictDirectPlay:
		plan.Strategy = StrategyDirectPlay
		plan.Reasons = append(plan.Reasons,
			fmt.Sprintf("every track direct-plays on %q from container %q; serving the source as-is", target.Name, srcContainer))

	case verdictRemux:
		plan.Strategy = StrategyRemuxHLS
		plan.RemuxContainer = plan.Playability.RemuxContainer
		if plan.RemuxContainer == "" {
			plan.RemuxContainer = "hls"
		}
		plan.Reasons = append(plan.Reasons,
			fmt.Sprintf("source container %q does not carry every track's codec on %q; remux to %s keeps every codec, no transcode", srcContainer, target.Name, plan.RemuxContainer))

		if err := ctx.Err(); err != nil {
			return nil, err
		}
		head, err := reader.OpenMetaWithFS(ctx, path, opts.FS, reader.WithCues())
		if err != nil {
			return nil, fmt.Errorf("ingest: check seek index: %w", err)
		}
		// "Has an index" must mean what the CONSUMER needs, not merely "some cue
		// exists": on-demand HLS cuts its segments on the video track's cue points
		// (PlanHLS) and refuses a source whose Cues index no video keyframe. An
		// index that only cues the audio would otherwise be waved through here and
		// blow up at serving time, on a plan that promised it was ready.
		plan.HasSeekIndex = hasVideoSeekIndex(head)

		if plan.HasSeekIndex {
			plan.Reasons = append(plan.Reasons, "source already carries a head-discoverable Cues index; ready for on-demand HLS")
		} else {
			plan.NeedsReindex = true
			plan.Reasons = append(plan.Reasons, "source has no head-discoverable seek index; a reindex is required before on-demand HLS packaging")

			if opts.Reindex {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				reindexErr := ReindexInPlace(ctx, path, opts.Options)
				switch {
				case reindexErr == nil:
					plan.Reindexed = true
					plan.HasSeekIndex = true
					plan.NeedsReindex = false
					plan.ReindexInPlacePossible = true
					plan.Reasons = append(plan.Reasons, "in-place reindex succeeded; seek index is now head-discoverable")
				case errors.Is(reindexErr, ErrIndexNotHeadDiscoverable):
					plan.ReindexInPlacePossible = false
					plan.Reasons = append(plan.Reasons, "in-place reindex is not possible for this file's layout; run a copy reindex (ops.Reindex) to get a head-discoverable index")
				default:
					return nil, fmt.Errorf("ingest: reindex in place: %w", reindexErr)
				}
			}
		}

	case verdictTranscode:
		plan.Strategy = StrategyTranscode
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ladder, err := RecommendLadderFor(ctx, path, opts.Options)
		if err != nil {
			return nil, fmt.Errorf("ingest: recommend ladder: %w", err)
		}
		plan.Ladder = ladder
		plan.Reasons = append(plan.Reasons,
			fmt.Sprintf("at least one track's codec/profile/level is not carried by any container %q accepts; transcode required, recommended ladder has %d rung(s)", target.Name, len(ladder)))
	}

	if opts.IncludeAnalysis {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		analysis, err := Analyze(ctx, path, opts.Options)
		if err != nil {
			return nil, fmt.Errorf("ingest: analyze: %w", err)
		}
		plan.Analysis = analysis
		plan.Reasons = append(plan.Reasons, "stream analysis attached")
	}

	return plan, nil
}

// hasVideoSeekIndex reports whether the source carries an index on-demand HLS can
// actually seek through: cue points on the VIDEO track, which is what PlanHLS cuts
// its segments on. A file whose Cues key only on audio has an index by the letter
// and none by the meaning - the same trap CueHealth calls "index-misskeyed". An
// audio-only source has no video to cue, so any cue serves.
func hasVideoSeekIndex(c *mkv.Container) bool {
	if len(c.Cues) == 0 {
		return false
	}
	video := videoTrackSet(c.Tracks)
	if len(video) == 0 {
		return true // audio-only: its cues are the index
	}
	for _, cue := range c.Cues {
		if video[cue.Track] {
			return true
		}
	}
	return false
}
