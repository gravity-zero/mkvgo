package matroska

import (
	"context"
	"testing"
)

// TestFacadeAnalyze proves Analyze delegates through the facade: the fixture
// has one video and one audio track, each with a positive frame count.
func TestFacadeAnalyze(t *testing.T) {
	requireFixture(t)
	report, err := Analyze(context.Background(), fixturePath)
	assertNoErr(t, err)
	if len(report.Tracks) != 2 {
		t.Fatalf("Tracks = %d, want 2", len(report.Tracks))
	}
	for _, tr := range report.Tracks {
		if tr.Frames <= 0 {
			t.Errorf("track %d: Frames = %d, want > 0", tr.TrackID, tr.Frames)
		}
	}
}

// TestFacadeTargetByName exercises both the found and not-found paths of the
// built-in playability profile table.
func TestFacadeTargetByName(t *testing.T) {
	target, ok := TargetByName("safari")
	if !ok || target.Name != "safari" {
		t.Errorf("TargetByName(safari) = %+v, %v", target, ok)
	}
	if _, ok := TargetByName("nope"); ok {
		t.Error("TargetByName(nope) should report ok=false")
	}
}

// TestFacadePlayability proves Playability delegates and returns a non-empty
// overall verdict for every track of the fixture.
func TestFacadePlayability(t *testing.T) {
	requireFixture(t)
	target, ok := TargetByName("mse-generic")
	if !ok {
		t.Fatal("TargetByName(mse-generic) should be known")
	}
	report, err := Playability(context.Background(), fixturePath, target)
	assertNoErr(t, err)
	if report.OverallVerdict == "" {
		t.Error("OverallVerdict is empty")
	}
	if len(report.Tracks) != 2 {
		t.Errorf("Tracks = %d, want 2", len(report.Tracks))
	}
}

// TestFacadeRecommendLadder exercises both the pure ladder-from-facts entry
// point and the file-derived one.
func TestFacadeRecommendLadder(t *testing.T) {
	rungs := RecommendLadder(LadderInput{SourceWidth: 1920, SourceHeight: 1080, Codec: "h264"})
	if len(rungs) == 0 {
		t.Fatal("RecommendLadder returned no rungs")
	}
	for _, r := range rungs {
		if r.Width <= 0 || r.Height <= 0 || r.BitrateKbps <= 0 {
			t.Errorf("rung has non-positive fact: %+v", r)
		}
	}
}

func TestFacadeRecommendLadderFor(t *testing.T) {
	requireFixture(t)
	rungs, err := RecommendLadderFor(context.Background(), fixturePath)
	assertNoErr(t, err)
	if len(rungs) == 0 {
		t.Error("RecommendLadderFor returned no rungs for a file with a video track")
	}
}

// TestFacadeIngest proves Ingest composes the underlying decisions and
// returns a valid, non-empty strategy with a decision trail.
func TestFacadeIngest(t *testing.T) {
	requireFixture(t)
	plan, err := Ingest(context.Background(), fixturePath, IngestOptions{IncludeAnalysis: true})
	assertNoErr(t, err)
	if plan.Strategy == "" {
		t.Error("Strategy is empty")
	}
	if len(plan.Reasons) == 0 {
		t.Error("Reasons is empty")
	}
	if plan.Analysis == nil {
		t.Error("Analysis should be populated when IncludeAnalysis is set")
	}
}

// TestFacadeFingerprint proves Fingerprint delegates and returns a stable
// 64-hex-character Presentation identity plus one digest per track.
func TestFacadeFingerprint(t *testing.T) {
	requireFixture(t)
	report, err := Fingerprint(context.Background(), fixturePath)
	assertNoErr(t, err)
	if len(report.Presentation) != 64 {
		t.Errorf("Presentation = %q, want 64 hex chars", report.Presentation)
	}
	if len(report.Tracks) != 2 {
		t.Fatalf("Tracks = %d, want 2", len(report.Tracks))
	}
	for _, tr := range report.Tracks {
		if len(tr.SHA256) != 64 {
			t.Errorf("track %d SHA256 = %q, want 64 hex chars", tr.TrackID, tr.SHA256)
		}
	}
}
