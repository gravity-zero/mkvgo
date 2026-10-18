package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func realFixture(name string) string { return filepath.Join("..", "internal", "testdata", name) }

func TestMuxTracksOfTwoSources(t *testing.T) {
	a, b := editFixture(t), realFixture("bframes_aac.mp4")
	dst := filepath.Join(t.TempDir(), "mux.mp4")
	err := Mux(context.Background(), mkv.MuxOptions{
		OutputPath: dst,
		Title:      "Muxed",
		Tracks: []mkv.TrackInput{
			{SourcePath: b, TrackID: 1, IsDefault: true},
			{SourcePath: a, TrackID: 2, Language: "fre", Name: "Synth audio", IsDefault: true},
			{SourcePath: b, TrackID: 2, Language: "eng"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mustMeta(t, dst)
	if c.Info.Title != "Muxed" || len(c.Tracks) != 3 {
		t.Fatalf("title %q, %d tracks", c.Info.Title, len(c.Tracks))
	}
	if c.Tracks[1].Language != "fre" || c.Tracks[1].Name != "Synth audio" || !c.Tracks[1].IsDefault || c.Tracks[2].Language != "eng" || c.Tracks[2].IsDefault {
		t.Errorf("track metadata %+v", c.Tracks)
	}
	samePresentation(t, dst, 0, b, 0)
	samePresentation(t, dst, 1, a, 1)
	samePresentation(t, dst, 2, b, 1)
}

func TestMergeAndAddTrack(t *testing.T) {
	a := editFixture(t)
	if err := EditInPlace(context.Background(), a, func(c *mkv.Container) {
		c.Info.Title = "First"
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "One", StartMs: 0}, {ID: 2, Title: "Two", StartMs: 3000}}
	}); err != nil {
		t.Fatal(err)
	}
	b := realFixture("bframes_aac.mov")
	dst := filepath.Join(t.TempDir(), "merge.mp4")
	if err := Merge(context.Background(), mkv.MergeOptions{OutputPath: dst, Inputs: []mkv.MergeInput{{SourcePath: a}, {SourcePath: b, TrackIDs: []uint64{2}}}}); err != nil {
		t.Fatal(err)
	}
	c := mustMeta(t, dst)
	if len(c.Tracks) != 3 || c.Info.Title != "First" || len(c.Chapters) != 2 {
		t.Fatalf("%d tracks, title %q, chapters %+v", len(c.Tracks), c.Info.Title, c.Chapters)
	}
	if refs := chapterTrackRefs(moovBoxesOf(t, dst)); len(refs) != 1 {
		t.Errorf("chapter track references %v", refs)
	}
	samePresentation(t, dst, 0, a, 0)
	samePresentation(t, dst, 1, a, 1)
	samePresentation(t, dst, 2, b, 1)
	var traks [][]byte
	for _, bx := range moovBoxesOf(t, dst) {
		if bx.typ == "trak" {
			traks = append(traks, bx.payload)
		}
	}
	hdlr, _ := boxAt(traks[2], "mdia", "hdlr")
	mdhd, _ := boxAt(traks[2], "mdia", "mdhd")
	if string(bytes.TrimRight(hdlr[24:], "\x00")) != "SoundHandler" || binary.BigEndian.Uint16(mdhd[20:]) != packLanguage("und") {
		t.Errorf("the MOV track keeps its QuickTime forms in the MP4: hdlr name %q, mdhd language %#x", hdlr[24:], mdhd[20:22])
	}

	dst2 := filepath.Join(t.TempDir(), "add.mp4")
	if err := AddTrack(context.Background(), a, dst2, mkv.TrackInput{SourcePath: b, TrackID: 2, Language: "ita", Name: "Extra"}); err != nil {
		t.Fatal(err)
	}
	c = mustMeta(t, dst2)
	if len(c.Tracks) != 3 || c.Tracks[2].Language != "ita" || c.Tracks[2].Name != "Extra" {
		t.Errorf("added track %+v", c.Tracks)
	}
	mustVerify(t, dst2)
}

// TestMergeSubtitleNeverWritesOverItsSRT: the SRT is an input too, held in memory, and an output naming it is refused untouched.
func TestMergeSubtitleNeverWritesOverItsSRT(t *testing.T) {
	src := editFixture(t)
	srt := filepath.Join(t.TempDir(), "s.srt")
	cue := []byte("1\n00:00:01,000 --> 00:00:02,500\nHello\n")
	if err := os.WriteFile(srt, cue, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MergeSubtitle(context.Background(), src, srt, srt, "fre", ""); err == nil || !strings.Contains(err.Error(), "also an input") {
		t.Errorf("an output naming the SRT: %v", err)
	}
	if got, err := os.ReadFile(srt); err != nil || !bytes.Equal(got, cue) {
		t.Errorf("the SRT changed: %d bytes (%d before) %v", len(got), len(cue), err)
	}
}

func TestMergeSubtitleSRT(t *testing.T) {
	src := editFixture(t)
	srt := filepath.Join(t.TempDir(), "s.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,500\nHello\n\n2\n00:00:03,000 --> 00:00:04,000\nWorld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "subs.mp4")
	if err := MergeSubtitle(context.Background(), src, srt, dst, "fre", "Français"); err != nil {
		t.Fatal(err)
	}
	c := mustMeta(t, dst)
	if len(c.Tracks) != 3 || c.Tracks[2].Type != mkv.SubtitleTrack || c.Tracks[2].Language != "fre" || c.Tracks[2].Name != "Français" {
		t.Fatalf("tracks %+v", c.Tracks)
	}
	var vtt bytes.Buffer
	if err := ExtractSubtitleWebVTT(context.Background(), dst, 3, &vtt); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"00:00:01.000 --> 00:00:02.500", "Hello", "00:00:03.000 --> 00:00:04.000", "World"} {
		if !strings.Contains(vtt.String(), want) {
			t.Errorf("WebVTT read back lacks %q:\n%s", want, vtt.String())
		}
	}
	mustVerify(t, dst)
	bcp := filepath.Join(t.TempDir(), "bcp.mp4")
	if err := MergeSubtitle(context.Background(), src, srt, bcp, "fr-CA", ""); err != nil {
		t.Fatal(err)
	}
	if tr := mustMeta(t, bcp).Tracks[2]; tr.LanguageBCP47 != "fr-CA" || tr.Language != "fra" {
		t.Errorf("BCP-47 subtitle: language %q, bcp47 %q", tr.Language, tr.LanguageBCP47)
	}
	for _, bad := range []string{"zz!", "pt_BR!"} {
		if err := MergeSubtitle(context.Background(), src, srt, filepath.Join(t.TempDir(), "bad.mp4"), bad, ""); err == nil || !strings.Contains(err.Error(), "not a BCP-47") {
			t.Errorf("language %q: %v", bad, err)
		}
	}
	mov := filepath.Join(t.TempDir(), "subs.mov")
	if err := MergeSubtitle(context.Background(), realFixture("bframes_aac.mov"), srt, mov, "fre", "Sous-titres"); err != nil {
		t.Fatal(err)
	}
	var subTrak []byte
	for _, b := range moovBoxesOf(t, mov) {
		if b.typ == "trak" {
			subTrak = b.payload
		}
	}
	if hdlr, _ := boxAt(subTrak, "mdia", "hdlr"); len(hdlr) < 25 || int(hdlr[24]) != len("Sous-titres") {
		t.Errorf("the subtitle's handler name in a MOV is not a Pascal string: %q", hdlr[24:])
	}
}

func TestMuxRefusals(t *testing.T) {
	a := editFixture(t)
	out := filepath.Join(t.TempDir(), "x.mp4")
	for _, tc := range []struct {
		opts mkv.MuxOptions
		want string
	}{
		{mkv.MuxOptions{OutputPath: out}, "no tracks"},
		{mkv.MuxOptions{OutputPath: out, Tracks: []mkv.TrackInput{{SourcePath: a, TrackID: 9}}}, "track 9 not found"},
		{mkv.MuxOptions{OutputPath: out, Tracks: []mkv.TrackInput{{SourcePath: a, TrackID: 1}}, Attachments: []mkv.Attachment{{Name: "f"}}}, "no attachments"},
		{mkv.MuxOptions{OutputPath: out, Tracks: []mkv.TrackInput{{SourcePath: a, TrackID: 1, Language: "fr_FR!"}}}, "not a BCP-47"},
	} {
		err := Mux(context.Background(), tc.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
		if _, serr := os.Stat(out); serr == nil {
			t.Error("a refused mux left an output")
		}
	}
}

func TestRescaleMovieTime(t *testing.T) {
	trak := firstTrak(t, editFixture(t))
	d0, _ := trakDuration(trak)
	out, err := rescaleMovieTime(trak, 1000, 600)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := trakDuration(out); d != rescale(d0, 1000, 600) || d == d0 {
		t.Errorf("tkhd duration %d -> %d", d0, d)
	}
}

// samePresentation checks that track ti of got carries the samples, timing and edit (in ns) of track si of src.
func samePresentation(t *testing.T, got string, ti int, src string, si int) {
	t.Helper()
	g, w := sampleSeqs(t, got)[ti], sampleSeqs(t, src)[si]
	if strings.Join(g, ",") != strings.Join(w, ",") {
		t.Errorf("track %d: samples differ from %s track %d", ti+1, src, si+1)
	}
	gm, wm := mustCutModel(t, got), mustCutModel(t, src)
	gc, wc := &gm.tracks[ti], &wm.tracks[si]
	if gc.ptNs(0, gm.movieTS) != wc.ptNs(0, wm.movieTS) || gc.presentEndNs(gm.movieTS) != wc.presentEndNs(wm.movieTS) {
		t.Errorf("track %d: presentation [%d, %d] ns, source [%d, %d]", ti+1, gc.ptNs(0, gm.movieTS), gc.presentEndNs(gm.movieTS), wc.ptNs(0, wm.movieTS), wc.presentEndNs(wm.movieTS))
	}
}

// TestMuxKeepsEditListsExactAcrossMovieTimescales: an audio delay stated on a 48 kHz movie timescale survives a mux whose first input runs on 1 kHz.
func TestMuxKeepsEditListsExactAcrossMovieTimescales(t *testing.T) {
	a, b := editFixture(t), realFixture("offset_ts48k.mp4")
	dst := filepath.Join(t.TempDir(), "mux.mp4")
	if err := Mux(context.Background(), mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{
		{SourcePath: a, TrackID: 1, IsDefault: true},
		{SourcePath: b, TrackID: 2, IsDefault: true},
	}}); err != nil {
		t.Fatal(err)
	}
	samePresentation(t, dst, 0, a, 0)
	samePresentation(t, dst, 1, b, 1)
	if m := mustCutModel(t, dst); m.movieTS != 48000 {
		t.Errorf("movie timescale %d, want 48000 (the common multiple of 1000 and 48000)", m.movieTS)
	}
}

func TestMuxRefusesWritingOverAnInput(t *testing.T) {
	a := editFixture(t)
	err := Mux(context.Background(), mkv.MuxOptions{OutputPath: a, Tracks: []mkv.TrackInput{{SourcePath: a, TrackID: 1}}})
	if err == nil || !strings.Contains(err.Error(), "also an input") {
		t.Errorf("err = %v", err)
	}
	if err := Rewrite(context.Background(), a, a); err == nil {
		t.Error("a rewrite over its own source went through")
	}
	mustVerify(t, a)
}

func TestLanguageFieldsAndBCP47Check(t *testing.T) {
	for _, tc := range []struct{ in, lang, bcp string }{{"fre", "fre", ""}, {"pt-BR", "por", "pt-BR"}, {"yue-HK", "yue", "yue-HK"}, {"fr", "fra", "fr"}, {"x-klingon", "und", "x-klingon"}} {
		if l, b := LanguageFields(tc.in); l != tc.lang || b != tc.bcp {
			t.Errorf("LanguageFields(%q) = %q, %q", tc.in, l, b)
		}
	}
	for tag, ok := range map[string]bool{"pt-BR": true, "zh-Hant-TW": true, "fr": true, "pt_BR!": false, "x": false, "": false, strings.Repeat("a-", 20) + "b": false, "en-123456789": false} {
		if validBCP47(tag) != ok {
			t.Errorf("validBCP47(%q) = %v", tag, !ok)
		}
	}
}

func TestMergeKeepsChapterTrackOnlyChapters(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 3000}}
	}); err != nil {
		t.Fatal(err)
	}
	rewriteTailMoov(t, src, func(p []byte) []byte {
		boxes, _ := iterBoxes(p)
		var w bw
		for _, b := range boxes {
			if b.typ != "udta" { // no chpl: the chapter track alone carries the chapters
				w.bytes(box(b.typ, b.payload))
			}
		}
		return w.b
	})
	dst := filepath.Join(t.TempDir(), "m.mp4")
	if err := Merge(context.Background(), mkv.MergeOptions{OutputPath: dst, Inputs: []mkv.MergeInput{{SourcePath: src}, {SourcePath: realFixture("bframes_aac.mp4"), TrackIDs: []uint64{2}}}}); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, dst); len(c.Chapters) != 2 || c.Chapters[1].Title != "B" {
		t.Errorf("chapters %+v", c.Chapters)
	}
}

// TestMergeSubtitleReadsThroughTheFS: the SRT comes through Options.FS, and one past the cap is refused before it is held.
func TestMergeSubtitleReadsThroughTheFS(t *testing.T) {
	src := editFixture(t)
	mem := mkv.NewMemFS()
	srt := []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n")
	mem.Put("/subs.srt", srt)
	data, _ := os.ReadFile(src)
	mem.Put("/in.mp4", data)
	if err := MergeSubtitle(context.Background(), "/in.mp4", "/subs.srt", "/out.mp4", "eng", "", Options{FS: mem.FS()}); err != nil {
		t.Fatalf("an SRT on the given FS: %v", err)
	}
	big := bytes.Repeat([]byte("x\n"), maxSRTBytes/2+10)
	mem.Put("/big.srt", big)
	err := MergeSubtitle(context.Background(), "/in.mp4", "/big.srt", "/out2.mp4", "eng", "", Options{FS: mem.FS()})
	if err == nil || !strings.Contains(err.Error(), "passes the 64 MiB") {
		t.Errorf("err = %v", err)
	}
}
