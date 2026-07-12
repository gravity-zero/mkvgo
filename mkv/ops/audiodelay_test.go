package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// TestAudioStartDelays_MultiTrack: two audio tracks with different delays,
// both measured against the first video keyframe, keyed by REAL track number.
func TestAudioStartDelays_MultiTrack(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{videoTrack(1), audioTrack(2), audioTrack(3)}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		ts := int64(i * 1000)
		sets = append(sets, []mkv.Block{
			{TrackNumber: 1, Timecode: ts, Keyframe: true, Data: []byte{0xAA}},
			{TrackNumber: 2, Timecode: ts + 300, Keyframe: true, Data: []byte{0x01}},
			{TrackNumber: 3, Timecode: ts + 700, Keyframe: true, Data: []byte{0x02}},
		})
	}
	path := buildMultiClusterMKV(t, dir, "delays.mkv", tracks, sets, 4000)

	delays, err := AudioStartDelays(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if delays[2] != 300_000_000 || delays[3] != 700_000_000 {
		t.Errorf("delays = %+v, want track 2 = 300ms, track 3 = 700ms", delays)
	}
	if _, ok := delays[1]; ok {
		t.Error("the video track must not appear in the audio delay map")
	}
}

// TestAudioStartDelays_NoVideoAnchor: audio-only files anchor on the earliest
// block of any track.
func TestAudioStartDelays_NoVideoAnchor(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	tracks := []mkv.Track{audioTrack(1), audioTrack(2)}
	sets := [][]mkv.Block{{
		{TrackNumber: 1, Timecode: 100, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 2, Timecode: 350, Keyframe: true, Data: []byte{0x02}},
	}}
	path := buildMultiClusterMKV(t, dir, "audioonly.mkv", tracks, sets, 1000)

	delays, err := AudioStartDelays(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if delays[1] != 0 || delays[2] != 250_000_000 {
		t.Errorf("delays = %+v, want track 1 = 0, track 2 = 250ms (anchored on the earliest block)", delays)
	}
}

// TestSalvage_TruncatedTailVerdict: a cut tail sets the first-class
// truncated verdict; mid-file damage does not.
func TestSalvage_TruncatedTailVerdict(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := salvageFixture(t, dir, "verdict.mkv", 8)
	data := readAll(t, src)
	magic := []byte{0x1F, 0x43, 0xB6, 0x75}
	offsets := findAll(data, magic)

	// Mid-file damage only: repairable, not truncated.
	mid := spliceAt(data, offsets[3], []byte{0x00, 0xFF, 0x51, 0x00, 0xFF, 0x51})
	midPath := filepath.Join(dir, "mid.mkv")
	writeAll(t, midPath, mid)
	report, err := MapDamage(ctx, midPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.TruncatedTail {
		t.Error("mid-file damage must not carry the truncated verdict")
	}

	// Cut tail: truncated.
	trunc := filepath.Join(dir, "trunc.mkv")
	writeAll(t, trunc, data[:offsets[5]+15])
	report, err = MapDamage(ctx, trunc)
	if err != nil {
		t.Fatal(err)
	}
	if !report.TruncatedTail {
		t.Errorf("a cut tail must carry the truncated verdict: %+v", report)
	}
}
