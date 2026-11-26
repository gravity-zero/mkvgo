package mp4

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/dovi/dovitest"
	"github.com/gravity-zero/mkvgo/mkv"
)

// TestDolbyVisionRefusedWhereNotApplied: an operation that copies samples, or hands their bytes out, took
// Options.DolbyVision and ignored it, so a caller asking for profile 8.1 got profile 7 back with no word. Each one
// refuses, before any write and before any callback.
func TestDolbyVisionRefusedWhereNotApplied(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	fx := dovitest.Profile7(t, dir, true)
	src, dst := filepath.Join(dir, "p7.mp4"), filepath.Join(dir, "out.mp4")
	if err := RemuxToMP4(ctx, fx.Path, src); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	id := fx.Track.ID
	calls := 0 // a refusal comes before the first callback: no source byte is handed out
	noBlock := func(mkv.Block) error { calls++; return nil }
	for _, conv := range []mkv.DolbyVisionConversion{mkv.DolbyVisionToProfile81, mkv.DolbyVisionToBaseLayer} {
		o := Options{DolbyVision: conv}
		ops := map[string]func() error{
			"Mux": func() error {
				return Mux(ctx, mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: id}}}, o)
			},
			"Merge": func() error {
				return Merge(ctx, mkv.MergeOptions{OutputPath: dst, Inputs: []mkv.MergeInput{{SourcePath: src}}}, o)
			},
			"AddTrack":      func() error { return AddTrack(ctx, src, dst, mkv.TrackInput{SourcePath: src, TrackID: id}, o) },
			"MergeSubtitle": func() error { return MergeSubtitle(ctx, src, filepath.Join(dir, "none.srt"), dst, "eng", "", o) },
			"Rewrite":       func() error { return Rewrite(ctx, src, dst, o) },
			"EditMetadata":  func() error { return EditMetadata(ctx, src, dst, func(*mkv.Container) {}, o) },
			"SetChapters":   func() error { return SetChapters(ctx, src, dst, nil, o) },
			"RemoveTrack":   func() error { return RemoveTrack(ctx, src, dst, []uint64{99}, o) },
			"Split": func() error {
				_, err := Split(ctx, mkv.SplitOptions{SourcePath: src, OutputDir: filepath.Join(dir, "parts"), EveryMs: 1000}, o)
				return err
			},
			"Join":         func() error { return Join(ctx, []string{src, src}, dst, o) },
			"EditInPlace":  func() error { return EditInPlace(ctx, src, func(c *mkv.Container) { c.Info.Title = "x" }, o) },
			"RetimeTracks": func() error { return RetimeTracks(ctx, src, map[uint64]int64{id: 40}, o) },
			"ForEachBlock": func() error { _, err := ForEachBlock(ctx, src, noBlock, o); return err },
			"WalkBlocks": func() error {
				return WalkBlocks(ctx, src, func(*mkv.Container) error { calls++; return nil }, noBlock, o)
			},
			"DASHFromCMAF": func() error { _, err := DASHFromCMAF(ctx, CMAFPresentation{}, o); return err },
			"HLSFromCMAF":  func() error { _, err := HLSFromCMAF(ctx, CMAFPresentation{}, o); return err },
		}
		for name, op := range ops {
			err := op()
			if !errors.Is(err, ErrDolbyVisionUnsupported) {
				t.Errorf("%s, conversion %d: %v, want a refusal matching ErrDolbyVisionUnsupported", name, conv, err)
				continue
			}
			if !strings.Contains(err.Error(), name+" leaves the Dolby Vision layers as they are") || !strings.Contains(err.Error(), "RemuxToMP4") {
				t.Errorf("%s: %q names neither the operation nor the one that converts", name, err)
			}
		}
		for name, plan := range map[string]func() error{
			"PlanHLS of an MP4": func() error { _, err := PlanHLS(ctx, src, o); return err },
			"PlanGrowingHLS":    func() error { _, err := PlanGrowingHLS(ctx, fx.Path, o); return err },
		} {
			if err := plan(); !errors.Is(err, ErrDolbyVisionUnsupported) {
				t.Errorf("%s, conversion %d: %v, want a refusal matching ErrDolbyVisionUnsupported", name, conv, err)
			}
		}
	}
	if calls != 0 {
		t.Errorf("ForEachBlock and WalkBlocks called back %d times before refusing", calls)
	}
	if _, err := ForEachBlock(ctx, src, noBlock); err != nil || calls == 0 {
		t.Errorf("without the option ForEachBlock hands out the samples: %v, %d calls", err, calls)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if p := filepath.Join(dir, e.Name()); p != src && p != fx.Path {
			t.Errorf("a refused operation left %s", e.Name())
		}
	}
	if after, err := os.ReadFile(src); err != nil || !bytes.Equal(after, before) {
		t.Errorf("a refused operation changed its source (%v)", err)
	}
}
