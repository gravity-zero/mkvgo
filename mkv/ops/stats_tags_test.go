package ops

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// Mux writes mkvmerge-style per-track statistics tags (BPS, DURATION,
// NUMBER_OF_FRAMES, NUMBER_OF_BYTES) accumulated during the stream, in a Tags
// element the SeekHead points to - so the head-only WithBitrate probe reads
// the bitrate back without a cluster scan.
func TestMuxWritesStatisticsTags(t *testing.T) {
	dir := t.TempDir()
	// 2s of video: 21 frames of 1000 bytes → 21000 bytes over 2000 ms = 84000 bps.
	var blocks []mkv.Block
	payload := make([]byte, 1000)
	for tc := int64(0); tc <= 2000; tc += 100 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: payload})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 2100)

	out := filepath.Join(dir, "out.mkv")
	err := Mux(context.Background(), mkv.MuxOptions{
		OutputPath: out,
		Tracks:     []mkv.TrackInput{{SourcePath: src, TrackID: 1, IsDefault: true}},
		Tags: []mkv.Tag{{TargetType: "MOVIE", SimpleTags: []mkv.SimpleTag{
			{Name: "ARTIST", Value: "someone"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := reader.Open(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	var statTagUID uint64
	for _, tag := range got.Tags {
		for _, st := range tag.SimpleTags {
			byName[st.Name] = st.Value
			if st.Name == "BPS" {
				statTagUID = tag.TargetID
			}
		}
	}
	if byName["ARTIST"] != "someone" {
		t.Errorf("caller tags lost: %v", byName)
	}
	if byName["BPS"] != "84000" {
		t.Errorf("BPS = %q, want 84000 (21000 bytes × 8 / 2s)", byName["BPS"])
	}
	if byName["NUMBER_OF_FRAMES"] != "21" || byName["NUMBER_OF_BYTES"] != "21000" {
		t.Errorf("frames/bytes = %q/%q, want 21/21000",
			byName["NUMBER_OF_FRAMES"], byName["NUMBER_OF_BYTES"])
	}
	if byName["DURATION"] != "00:00:02.000000000" {
		t.Errorf("DURATION = %q, want 00:00:02.000000000", byName["DURATION"])
	}
	if statTagUID != got.Tracks[0].UID {
		t.Errorf("stats tag TargetID = %d, want track UID %d", statTagUID, got.Tracks[0].UID)
	}

	// The whole point: the head-only bitrate probe reads it back via
	// SeekHead→Tags, no cluster walk.
	meta, err := reader.OpenMeta(context.Background(), out, reader.WithBitrate())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Tracks[0].Bitrate == nil || *meta.Tracks[0].Bitrate != 84000 {
		t.Errorf("WithBitrate on muxed output = %v, want 84000", meta.Tracks[0].Bitrate)
	}
}
