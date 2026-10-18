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

// TestSetLanguageClearsAStaleBCP47Tag pins that a language set on an added or muxed Matroska track is the one in
// effect: the source's LanguageBCP47, which takes precedence over Language, no longer contradicts it.
func TestSetLanguageClearsAStaleBCP47Tag(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	audio := audioTrack(2)
	audio.Language, audio.LanguageBCP47 = "eng", "en"
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), audio}, 2000))
	for tc := int64(0); tc < 2000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	ctx := context.Background()
	added := filepath.Join(dir, "added.mkv")
	mustNil(t, AddTrack(ctx, src, added, mkv.TrackInput{SourcePath: src, TrackID: 2, Language: "fre"}))
	muxed := filepath.Join(dir, "muxed.mkv")
	mustNil(t, Mux(ctx, mkv.MuxOptions{OutputPath: muxed, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}, {SourcePath: src, TrackID: 2, Language: "fre"}}}))
	for path, id := range map[string]uint64{added: 3, muxed: 2} {
		c, err := reader.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range c.Tracks {
			if tr.ID == id && tr.ResolvedLanguage() != "fre" {
				t.Errorf("%s track %d: language %q (BCP-47 %q) after asking for fre", filepath.Base(path), id, tr.ResolvedLanguage(), tr.LanguageBCP47)
			}
		}
	}
}

// TestUnchangedLanguageKeepsItsBCP47Tag pins that a language passed through unchanged, as merge does with every
// track's own, keeps the track's BCP-47 tag: only a language that changes clears it.
func TestUnchangedLanguageKeepsItsBCP47Tag(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	audio := audioTrack(2)
	audio.Language, audio.LanguageBCP47 = "por", "pt-BR"
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), audio}, 2000))
	for tc := int64(0); tc < 2000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	ctx := context.Background()
	merged := filepath.Join(dir, "merged.mkv")
	mustNil(t, Merge(ctx, mkv.MergeOptions{OutputPath: merged, Inputs: []mkv.MergeInput{{SourcePath: src}}}))
	c, err := reader.Open(ctx, merged)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range c.Tracks {
		if tr.Type == mkv.AudioTrack && tr.LanguageBCP47 != "pt-BR" {
			t.Errorf("merge left track %d with BCP-47 %q, want pt-BR", tr.ID, tr.LanguageBCP47)
		}
	}
}

// TestEditCallbackLanguageTakesEffect pins that a language an edit callback changes is the one players see, through
// EditMetadata and EditInPlace alike, as the MP4 engine does: the stale BCP-47 tag no longer overrides it.
func TestEditCallbackLanguageTakesEffect(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	audio := audioTrack(2)
	audio.Language, audio.LanguageBCP47 = "eng", "en"
	mw := writer.NewMKVWriter(f)
	mustNil(t, mw.WriteStart())
	mustNil(t, mw.WriteMetadata(&mkv.Container{Info: mkv.SegmentInfo{TimecodeScale: 1_000_000, MuxingApp: "t", WritingApp: "t"}}, []mkv.Track{videoTrack(1), audio}, 2000))
	for tc := int64(0); tc < 2000; tc += 500 {
		mustNil(t, mw.WriteClusterWithCues(tc, 1_000_000, []mkv.Block{
			{TrackNumber: 1, Timecode: tc, Keyframe: true, Data: []byte("v")},
			{TrackNumber: 2, Timecode: tc, Keyframe: true, Data: []byte("a")},
		}))
	}
	mustNil(t, mw.Finalize())
	mustNil(t, f.Close())
	ctx := context.Background()
	toJapanese := func(c *mkv.Container) {
		for i := range c.Tracks {
			if c.Tracks[i].ID == 2 {
				c.Tracks[i].Language = "jpn"
			}
		}
	}
	edited := filepath.Join(dir, "edited.mkv")
	mustNil(t, EditMetadata(ctx, src, edited, toJapanese))
	mustNil(t, EditInPlace(ctx, src, toJapanese))
	for _, path := range []string{edited, src} {
		c, err := reader.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range c.Tracks {
			if tr.ID == 2 && tr.ResolvedLanguage() != "jpn" {
				t.Errorf("%s: language %q (BCP-47 %q) after the callback set jpn", filepath.Base(path), tr.ResolvedLanguage(), tr.LanguageBCP47)
			}
		}
	}
}
