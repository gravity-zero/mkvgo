package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
)

// An in-place edit on a mkvgo-written file must: fit a GROWN metadata set in
// the write-time reserve, rebuild the head SeekHead (keeping its Cues entry),
// and fold the post-cluster statistics Tags into the head without duplicates.
func TestEditInPlace_GrowsRebuildsSeekHeadNoTagDup(t *testing.T) {
	dir := t.TempDir()
	var blocks []mkv.Block
	payload := make([]byte, 500)
	for tc := int64(0); tc <= 2000; tc += 100 {
		blocks = append(blocks, mkv.Block{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: payload})
	}
	src := buildMinimalMKV(t, dir, "src.mkv", []mkv.Track{videoTrack(1)}, blocks, 2100)

	out := filepath.Join(dir, "muxed.mkv")
	err := Mux(context.Background(), mkv.MuxOptions{
		OutputPath: out,
		Tracks:     []mkv.TrackInput{{SourcePath: src, TrackID: 1, IsDefault: true}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grow the metadata: a long title plus an extra global tag.
	longTitle := strings.Repeat("Un Titre Assez Long ", 10)
	err = EditInPlace(context.Background(), out, func(c *mkv.Container) {
		c.Info.Title = longTitle
		c.Tags = append(c.Tags, mkv.Tag{TargetType: "MOVIE",
			SimpleTags: []mkv.SimpleTag{{Name: "COMMENT", Value: "edited in place"}}})
	})
	if err != nil {
		t.Fatalf("in-place grow within the reserve failed: %v", err)
	}

	got, err := reader.Open(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Info.Title != longTitle {
		t.Errorf("title not applied")
	}
	bps, comment := 0, 0
	for _, tag := range got.Tags {
		for _, st := range tag.SimpleTags {
			switch st.Name {
			case "BPS":
				bps++
			case "COMMENT":
				comment++
			}
		}
	}
	if bps != 1 {
		t.Errorf("BPS stats tags = %d, want exactly 1 (tail Tags must be voided, not duplicated)", bps)
	}
	if comment != 1 {
		t.Errorf("added COMMENT tag count = %d, want 1", comment)
	}
	if len(got.Cues) == 0 {
		t.Errorf("Cues lost after in-place edit")
	}

	// The rebuilt SeekHead must still lead head-only readers to the Tags
	// (WithBitrate follows SeekHead→Tags without a cluster scan).
	meta, err := reader.OpenMeta(context.Background(), out, reader.WithBitrate())
	if err != nil {
		t.Fatal(err)
	}
	if meta.Tracks[0].Bitrate == nil {
		t.Errorf("WithBitrate found no BPS after the in-place edit: the rebuilt SeekHead does not lead to the Tags")
	}
	if meta.Info.Title != longTitle {
		t.Errorf("head-only read shows stale title")
	}
}
