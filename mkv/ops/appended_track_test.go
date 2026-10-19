package ops

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestOtherTrackTypesSurviveARewrite: a metadata or buttons track is written back with its own TrackType, where every
// type past video, audio and subtitle came out as 0; a type Matroska does not define is refused by name.
func TestOtherTrackTypesSurviveARewrite(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	sparseTracksMKV(t, src, []mkv.Track{videoTrack(1), {ID: 2, Type: mkv.MetadataTrack, Codec: "D_WEBVTT/METADATA"}, {ID: 3, Type: mkv.ButtonsTrack, Codec: "B_VOBBTN"}})
	out := filepath.Join(dir, "out.mkv")
	mustNil(t, EditMetadata(ctx, src, out, func(c *mkv.Container) { c.Info.Title = "t" }))
	c, err := reader.Open(ctx, out)
	mustNil(t, err)
	if len(c.Tracks) != 3 || c.Tracks[1].Type != mkv.MetadataTrack || c.Tracks[2].Type != mkv.ButtonsTrack {
		t.Errorf("types after a rewrite: %v %v", c.Tracks[1].Type, c.Tracks[2].Type)
	}
	var w bytes.Buffer
	if err := writer.WriteTracks(&w, []mkv.Track{{ID: 1, Type: "hologram", Codec: "X"}}); err == nil {
		t.Error("a TrackType Matroska does not define was written")
	}
}

// TestJoinRefusesTracksInAnotherLanguageOrder: the join pairs tracks by position, so a second file whose audio is in
// another language (French and English swapped) is refused; the same language written two ways joins.
func TestJoinRefusesTracksInAnotherLanguageOrder(t *testing.T) {
	ctx, dir := context.Background(), t.TempDir()
	eng, fre, en := audioTrack(1), audioTrack(1), audioTrack(1)
	fre.Language = "fre"
	en.LanguageBCP47 = "en-US"
	a, b, c := filepath.Join(dir, "a.mkv"), filepath.Join(dir, "b.mkv"), filepath.Join(dir, "c.mkv")
	sparseTracksMKV(t, a, []mkv.Track{eng})
	sparseTracksMKV(t, b, []mkv.Track{fre})
	sparseTracksMKV(t, c, []mkv.Track{en})
	if err := Join(ctx, []string{a, b}, filepath.Join(dir, "ab.mkv")); err == nil || !strings.Contains(err.Error(), "same order") {
		t.Errorf("English then French audio: %v", err)
	}
	if err := Join(ctx, []string{a, c}, filepath.Join(dir, "ac.mkv")); err != nil {
		t.Errorf("eng and en-US: %v", err)
	}
}

// TestJoinKeepsTheSubChaptersOfAContinuedChapter: a chapter a split carried over a cut comes back whole, its
// sub-chapters from both parts under it, where those of the part it continued in were dropped.
func TestJoinKeepsTheSubChaptersOfAContinuedChapter(t *testing.T) {
	a := &mkv.Container{Info: mkv.SegmentInfo{SegmentUID: []byte{1}}, Chapters: []mkv.Chapter{{ID: 10, Title: "P", StartMs: 0, EndMs: 2000,
		SubChapters: []mkv.Chapter{{ID: 11, Title: "s1", StartMs: 500, EndMs: 1000}}}}}
	b := &mkv.Container{Info: mkv.SegmentInfo{SegmentUID: []byte{2}, PrevUID: []byte{1}}, Chapters: []mkv.Chapter{{ID: 10, Title: "P", StartMs: 0, EndMs: 1000,
		SubChapters: []mkv.Chapter{{ID: 12, Title: "s2", StartMs: 200, EndMs: 800}}}}}
	out := concatChapters([]*mkv.Container{a, b}, []int64{0, 2000})
	if len(out) != 1 || out[0].EndMs != 3000 || len(out[0].SubChapters) != 2 || out[0].SubChapters[1].Title != "s2" || out[0].SubChapters[1].StartMs != 2200 {
		t.Errorf("joined chapters: %+v", out)
	}
}
