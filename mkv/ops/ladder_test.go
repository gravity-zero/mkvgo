package ops

import (
	"context"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestRecommendLadder_1080pH264_CappedAtSource(t *testing.T) {
	rungs := RecommendLadder(LadderInput{
		SourceWidth: 1920, SourceHeight: 1080, SourceBitrateKbps: 6000, Codec: "h264",
	})
	if len(rungs) == 0 {
		t.Fatal("expected at least one rung")
	}
	for _, r := range rungs {
		if r.Height > 1080 || r.Width > 1920 {
			t.Errorf("rung %+v exceeds source resolution", r)
		}
		if r.BitrateKbps > 6000 {
			t.Errorf("rung %+v exceeds source bitrate", r)
		}
	}
	for i := 1; i < len(rungs); i++ {
		if rungs[i].BitrateKbps > rungs[i-1].BitrateKbps {
			t.Errorf("rungs not monotonic (tallest first): %+v then %+v", rungs[i-1], rungs[i])
		}
	}
	// No 2160p rung: source is only 1080p.
	for _, r := range rungs {
		if r.Label == "2160p" {
			t.Errorf("got a 2160p rung for a 1080p source: %+v", rungs)
		}
	}
}

func TestRecommendLadder_4KHEVC_MoreRungsAndEfficiency(t *testing.T) {
	rungs := RecommendLadder(LadderInput{
		SourceWidth: 3840, SourceHeight: 2160, SourceBitrateKbps: 20000, Codec: "hevc",
	})
	if len(rungs) != len(standardLadder) {
		t.Fatalf("got %d rungs, want %d (the whole standard ladder fits under a 4K/20Mbps source)", len(rungs), len(standardLadder))
	}
	var top Rung
	for _, r := range rungs {
		if r.Label == "2160p" {
			top = r
		}
	}
	if top.BitrateKbps == 0 {
		t.Fatal("no 2160p rung found")
	}
	// HEVC efficiency factor (0.6) applied to the 16000kbps H.264 baseline.
	want := int(16000 * 0.6)
	if top.BitrateKbps != want {
		t.Errorf("2160p bitrate = %d, want %d (16000 * hevc efficiency 0.6)", top.BitrateKbps, want)
	}
	for i := 1; i < len(rungs); i++ {
		if rungs[i].BitrateKbps > rungs[i-1].BitrateKbps {
			t.Errorf("rungs not monotonic: %+v then %+v", rungs[i-1], rungs[i])
		}
	}
}

func TestRecommendLadder_360pSource_SingleRung_NoUpscale(t *testing.T) {
	rungs := RecommendLadder(LadderInput{
		SourceWidth: 640, SourceHeight: 360, SourceBitrateKbps: 800, Codec: "h264",
	})
	if len(rungs) != 1 {
		t.Fatalf("got %d rungs, want 1: %+v", len(rungs), rungs)
	}
	if rungs[0].Width > 640 || rungs[0].Height > 360 {
		t.Errorf("rung %+v upscales the 360p source", rungs[0])
	}
}

func TestRecommendLadder_UnknownBitrate_NoCap(t *testing.T) {
	rungs := RecommendLadder(LadderInput{SourceWidth: 1920, SourceHeight: 1080, Codec: "h264"})
	if len(rungs) == 0 {
		t.Fatal("expected rungs")
	}
	for _, r := range rungs {
		if r.BitrateKbps <= 0 {
			t.Errorf("rung %+v has no bitrate", r)
		}
	}
}

func TestRecommendLadderFor_MP4(t *testing.T) {
	rungs, err := RecommendLadderFor(context.Background(), sampleMKV)
	if err != nil {
		t.Fatal(err)
	}
	if len(rungs) == 0 {
		t.Fatal("expected at least one rung for the sample fixture (320x240)")
	}
	for _, r := range rungs {
		if r.Width > 320 || r.Height > 240 {
			t.Errorf("rung %+v upscales the 320x240 source", r)
		}
	}
}

func TestRecommendLadderFor_MemFS_NoVideoTrack(t *testing.T) {
	dir := t.TempDir()
	src := buildMinimalMKV(t, dir, "audio-only.mkv", []mkv.Track{audioTrack(1)}, nil, 0)
	if _, err := RecommendLadderFor(context.Background(), src); err == nil {
		t.Fatal("expected an error for a source with no video track")
	}
}
