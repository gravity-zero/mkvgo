package ops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mkv/reader"
	"github.com/gravity-zero/mkvgo/mkv/writer"
)

// sparseTracksMKV writes a file whose tracks are numbered 1 and 3, with the UIDs given.
func sparseTracksMKV(t *testing.T, path string, tracks []mkv.Track) {
	t.Helper()
	f, err := os.Create(path)
	mustNil(t, err)
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, tracks, 1000))
	var blks []mkv.Block
	for _, tr := range tracks {
		blks = append(blks, mkv.Block{TrackNumber: tr.ID, Timecode: 0, Keyframe: true, Duration: 500, Data: []byte{byte(tr.ID)}})
	}
	mustNil(t, mw.WriteClusterWithCues(0, 1_000_000, blks))
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
}

// TestAddedTracksTakeAFreeNumberAndUID: a track added to a file numbered 1 and 3 is numbered 4, not a second 3,
// and takes a TrackUID no track of the file has, whether it is added from another file, an SRT or an ASS.
func TestAddedTracksTakeAFreeNumberAndUID(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	v, a := videoTrack(1), audioTrack(3)
	v.UID, a.UID = 4, 7
	src := filepath.Join(dir, "src.mkv")
	sparseTracksMKV(t, src, []mkv.Track{v, a})
	other := filepath.Join(dir, "other.mkv")
	s := subtitleTrack(1, "S_TEXT/UTF8")
	s.UID = 7
	sparseTracksMKV(t, other, []mkv.Track{s})
	srt, ass := filepath.Join(dir, "s.srt"), filepath.Join(dir, "s.ass")
	mustNil(t, os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:00,500\nHi\n"), 0o644))
	mustNil(t, os.WriteFile(ass, []byte("[Script Info]\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:00.00,0:00:00.50,Default,,0,0,0,,Hi\n"), 0o644))
	for name, add := range map[string]func(string) error{
		"add-track": func(out string) error {
			return AddTrack(ctx, src, out, mkv.TrackInput{SourcePath: other, TrackID: 1})
		},
		"merge-subtitle": func(out string) error { return MergeSubtitle(ctx, src, srt, out, "eng", "") },
		"merge-ass":      func(out string) error { return MergeASS(ctx, src, ass, out, "eng", "") },
	} {
		out := filepath.Join(dir, name+".mkv")
		mustNil(t, add(out))
		c, err := reader.Open(ctx, out)
		mustNil(t, err)
		ids, uids := map[uint64]bool{}, map[uint64]bool{}
		for _, tr := range c.Tracks {
			uid := tr.UID
			if uid == 0 {
				uid = tr.ID
			}
			if ids[tr.ID] || uids[uid] {
				t.Errorf("%s: track %d (UID %d) repeats a number or UID: %+v", name, tr.ID, uid, c.Tracks)
			}
			ids[tr.ID], uids[uid] = true, true
		}
		if len(c.Tracks) != 3 || c.Tracks[2].ID != 4 {
			t.Errorf("%s: tracks %+v, want the new one numbered 4", name, c.Tracks)
		}
	}
}

// TestJoinTakesAnOmittedAudioFieldAsItsDefault: a file that leaves Channels out (mono by the schema) joins one that
// writes Channels 1, and a bit depth stated by one file only is no difference; a real one is still refused.
func TestJoinTakesAnOmittedAudioFieldAsItsDefault(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	omitted, stated, stereo := audioTrack(1), audioTrack(1), audioTrack(1)
	omitted.Channels = nil
	stated.Channels, stated.BitDepth = u8(1), u8(16)
	a, b, c := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv"), filepath.Join(dir, "c.mkv")
	sparseTracksMKV(t, a, []mkv.Track{omitted})
	sparseTracksMKV(t, b, []mkv.Track{stated})
	sparseTracksMKV(t, c, []mkv.Track{stereo})
	if err := Join(ctx, []string{a, b}, filepath.Join(dir, "ab.mkv")); err != nil {
		t.Errorf("Channels omitted and Channels 1: %v", err)
	}
	if err := Join(ctx, []string{a, c}, filepath.Join(dir, "ac.mkv")); err == nil {
		t.Error("mono and stereo joined")
	}
}
