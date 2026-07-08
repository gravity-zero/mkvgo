package ops

import (
	"context"
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// verifyReindexedCues re-opens path head-only and checks that the Cues index
// the reader sees matches exactly the cue points built during the reindex
// walk. This is the always-on light verification: it proves the Cues element,
// the SeekHead entry pointing at it and the Segment framing all round-trip,
// for the cost of a few milliseconds of header reads.
//
// The comparison accounts for the CueTime quantisation WriteCues applies on
// non-millisecond timecode scales, so it never flags a lossless roundtrip.
func verifyReindexedCues(ctx context.Context, path string, fs *mkv.FS, want []mkv.CuePoint, timecodeScale int64) error {
	c, err := reader.OpenWithFS(ctx, path, fs)
	if err != nil {
		return fmt.Errorf("reindex verify: reopen result: %w", err)
	}
	if len(c.Cues) != len(want) {
		return fmt.Errorf("reindex verify: result has %d cue points, expected %d", len(c.Cues), len(want))
	}
	if timecodeScale <= 0 {
		timecodeScale = 1_000_000
	}
	prev := int64(-1 << 62)
	for i, got := range c.Cues {
		// Mirror WriteCues (ms -> timecode units) then the reader (units -> ms)
		// so a truncating scale conversion is not reported as a mismatch.
		wantMs := int64(uint64(want[i].TimeMs) * 1_000_000 / uint64(timecodeScale) * uint64(timecodeScale) / 1_000_000)
		if got.TimeMs != wantMs || got.Track != want[i].Track || got.ClusterPos != want[i].ClusterPos {
			return fmt.Errorf("reindex verify: cue %d mismatch: got {time=%dms track=%d cluster=%d}, want {time=%dms track=%d cluster=%d}",
				i, got.TimeMs, got.Track, got.ClusterPos, wantMs, want[i].Track, want[i].ClusterPos)
		}
		if got.TimeMs < prev {
			return fmt.Errorf("reindex verify: cue %d time %dms goes backwards (previous %dms)", i, got.TimeMs, prev)
		}
		prev = got.TimeMs
	}
	return nil
}

// deepVerifyValidate runs the full-read Validate on path and fails on any
// error-severity issue (including the cue-to-real-keyframe ground truth
// check). Warnings are not failures: many valid sources carry them.
func deepVerifyValidate(ctx context.Context, path string, fs *mkv.FS) error {
	issues, err := Validate(ctx, path, mkv.Options{FS: fs})
	if err != nil {
		return fmt.Errorf("deep verify: %w", err)
	}
	var errs []string
	for _, is := range issues {
		if is.Severity == mkv.SeverityError {
			errs = append(errs, is.Message)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("deep verify: result does not validate: %s", strings.Join(errs, "; "))
	}
	return nil
}
