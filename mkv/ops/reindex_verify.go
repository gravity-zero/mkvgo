package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// ErrIndexNotHeadDiscoverable is wrapped into the ReindexInPlace error when the
// patched file's Cues index cannot be reached head-only by following the head
// SeekHead - the file's layout leaves no slot for a head-discoverable SeekHead
// (e.g. a source with no head SeekHead, where an in-place SeekHead could only
// land after Info/Tracks). The file is rolled back byte-identical. Callers can
// errors.Is this to fall back to a full reindex (copy), which always writes a
// head-discoverable index.
var ErrIndexNotHeadDiscoverable = errors.New("reindex: seek index not discoverable head-only for this file layout")

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

	// Head-only readability: a seek index is only useful if it is discoverable
	// the way seekers actually consume it - head-only, by following the head
	// SeekHead to the Cues (reader.WithCues), without a full-segment walk. A
	// full Open above can recover a tail Cues by scanning back from EOF even
	// when the SeekHead does not point at it, so it alone would pass an index
	// that no head-only reader can find. Require the head-only path to resolve
	// the same cues; otherwise the index is present but not usable for seeking.
	head, err := reader.OpenMetaWithFS(ctx, path, fs, reader.WithCues())
	if err != nil {
		return fmt.Errorf("reindex verify: head-only reopen: %w", err)
	}
	if len(head.Cues) != len(want) {
		return fmt.Errorf("reindex verify: %d cues via SeekHead, %d written - the index would not be usable for seeking; use a full reindex (copy) for this file layout: %w", len(head.Cues), len(want), ErrIndexNotHeadDiscoverable)
	}
	return nil
}

// deepVerifyVerbatim proves the reindexed copy carries the source's cluster
// payloads byte-identical, via CompareBlocks. Costs a full read of both files.
func deepVerifyVerbatim(ctx context.Context, srcPath, dstPath string, fs *mkv.FS) error {
	diffs, err := CompareBlocks(ctx, srcPath, dstPath, mkv.Options{FS: fs})
	if err != nil {
		return fmt.Errorf("reindex deep verify: compare blocks: %w", err)
	}
	if len(diffs) > 0 {
		msgs := make([]string, len(diffs))
		for i, d := range diffs {
			msgs[i] = d.String()
		}
		return fmt.Errorf("reindex deep verify: copy is not verbatim: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// preValidate captures the source's error-severity issues before an
// operation, for the deep-verify diff. A source that cannot even be
// validated yields nil - the diff then treats every error in the result as
// added (the strict behavior), which is the safe reading of "we could not
// know what was already broken".
func preValidate(ctx context.Context, path string, fs *mkv.FS) []mkv.Issue {
	issues, err := Validate(ctx, path, mkv.Options{FS: fs})
	if err != nil {
		return nil
	}
	return issues
}

// deepVerifyValidate runs the full-read Validate on path. By default it
// fails only on error-severity issues the operation ADDED (an issue with the
// same Code+Track identity in `before` is preexisting): refusing a correct
// repair because the source already carried, say, mis-keyed cues would make
// the tool unusable on exactly the files that need it. Preexisting errors
// are reported through onPreexisting - they have their own remedy - and
// warnings are never failures. strict restores the absolute behavior: any
// error-severity issue in the result refuses, preexisting or not.
func deepVerifyValidate(ctx context.Context, path string, fs *mkv.FS, before []mkv.Issue, strict bool, onPreexisting func(mkv.Issue)) error {
	issues, err := Validate(ctx, path, mkv.Options{FS: fs})
	if err != nil {
		return fmt.Errorf("deep verify: %w", err)
	}
	beforeKeys := make(map[string]bool, len(before))
	for _, is := range before {
		if is.Severity == mkv.SeverityError {
			beforeKeys[is.Key()] = true
		}
	}
	var added []string
	for _, is := range issues {
		if is.Severity != mkv.SeverityError {
			continue
		}
		if !strict && beforeKeys[is.Key()] {
			if onPreexisting != nil {
				onPreexisting(is)
			}
			continue
		}
		added = append(added, is.Message)
	}
	if len(added) == 0 {
		return nil
	}
	if strict {
		return fmt.Errorf("deep verify: result does not validate: %s", strings.Join(added, "; "))
	}
	return fmt.Errorf("deep verify: the operation introduced validation errors: %s", strings.Join(added, "; "))
}
