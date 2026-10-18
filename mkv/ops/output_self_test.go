package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// TestOpsRefuseAnOutputThatIsAnInput pins that every Matroska writer refuses to write over a file it reads, as the
// MP4 engine does: EditMetadata(p, p) truncated the film, failed reading it back, then removed it.
func TestOpsRefuseAnOutputThatIsAnInput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "film.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), audioTrack(2)}, 2000))
	for tc := int64(0); tc < 2000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	srt := filepath.Join(dir, "subs.srt")
	mustNil(t, os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhi\n"), 0o644))
	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other.mkv")
	mustNil(t, os.WriteFile(other, original, 0o644))
	ctx := context.Background()
	ops := map[string]func() error{
		"EditMetadata": func() error { return EditMetadata(ctx, src, src, func(c *mkv.Container) { c.Info.Title = "x" }) },
		"RemoveTrack":  func() error { return RemoveTrack(ctx, src, src, []uint64{2}) },
		"AddTrack":     func() error { return AddTrack(ctx, src, src, mkv.TrackInput{SourcePath: src, TrackID: 2}) },
		"Join":         func() error { return Join(ctx, []string{src, src}, src) },
		"Mux": func() error {
			return Mux(ctx, mkv.MuxOptions{OutputPath: src, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}}})
		},
		"MergeSubtitle":            func() error { return MergeSubtitle(ctx, src, srt, src, "eng", "") },
		"RemuxToWebM":              func() error { return RemuxToWebM(ctx, src, src) },
		"Reindex":                  func() error { return Reindex(ctx, src, src) },
		"Salvage":                  func() error { _, err := Salvage(ctx, src, src); return err },
		"AddTrack from the output": func() error { return AddTrack(ctx, other, src, mkv.TrackInput{SourcePath: src, TrackID: 2}) },
		"ExtractAttachment":        func() error { return ExtractAttachment(ctx, src, 1, src) },
		"Merge, attachments only": func() error {
			return Merge(ctx, mkv.MergeOptions{OutputPath: src, Inputs: []mkv.MergeInput{{SourcePath: src, TrackIDs: []uint64{99}}, {SourcePath: other}}})
		},
		"MergeWithSubtitles, extras": func() error {
			return MergeWithSubtitles(ctx, src, srt, src, "eng", "", []mkv.MergeInput{{SourcePath: other}})
		},
	}
	for name, op := range ops {
		if err := op(); err == nil {
			t.Errorf("%s with its source as output succeeded", name)
		}
		if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, original) {
			t.Fatalf("%s changed or removed its source (%v)", name, err)
		}
	}
}
