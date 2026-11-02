package ops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestRefusalSentinels: every refusal whose condition has a sentinel carries
// it, whatever the entry point, so a caller branches with errors.Is instead
// of matching the message.
func TestRefusalSentinels(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	unindexed := buildNoCuesFrontSlotMKV(t, dir)
	sets := [][]mkv.Block{{{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{1}}}, {{TrackNumber: 1, Timecode: 1000, Keyframe: true, Data: []byte{1}}}}
	audioOnly := buildMKVWithCues(t, dir, "audio.mkv", []mkv.Track{audioTrack(1)}, sets, []mkv.CuePoint{{TimeMs: 0, Track: 1}})
	data := readAll(t, salvageFixture(t, dir, "src.mkv", 8))
	cut := filepath.Join(dir, "cut.mkv")
	writeAll(t, cut, data[:findAll(data, []byte{0x1F, 0x43, 0xB6, 0x75})[5]+15])

	check := func(name string, err, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Errorf("%s: %v, want errors.Is %v", name, err, want)
		}
	}
	_, err := ExtractKeyframeSample(ctx, unindexed, 0)
	check("ExtractKeyframeSample, no Cues", err, ErrNoKeyframeIndex)
	_, err = ExtractKeyframeSample(ctx, audioOnly, 0)
	check("ExtractKeyframeSample, no video", err, ErrNoVideo)
	_, err = RecommendLadderFor(ctx, audioOnly)
	check("RecommendLadderFor, no video", err, ErrNoVideo)
	_, err = rangesEvery(nil, 1000)
	check("split every, no keyframes", err, ErrNoKeyframeIndex)
	_, err = Analyze(ctx, cut)
	check("Analyze, cut inside a block", err, ErrSourceTruncated)
}
