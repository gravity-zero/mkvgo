package mp4

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// withStblBoxes appends boxes to the stbl of trak i (moov at the tail).
func withStblBoxes(t *testing.T, path string, i int, extra ...[]byte) {
	t.Helper()
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(k int, trak []byte) []byte {
			if k != i {
				return trak
			}
			out, err := rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
				var kids [][]byte
				for _, b := range sb {
					kids = append(kids, box(b.typ, b.payload))
				}
				return append(kids, extra...), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		})
	})
}

// TestSampleGroupsDrawOnTheSampleBudget: sample-to-group tables of 20 bytes each cannot make a cut expand them without bound.
func TestSampleGroupsDrawOnTheSampleBudget(t *testing.T) {
	src := editShiftFixture(t, false)
	var groups [][]byte
	for k := 0; k < maxSampleGroups+1; k++ {
		groups = append(groups, fullBox("sbgp", 0, 0, func(w *bw) { w.fourcc(string(rune('a'+k)) + "grp"); w.u32(0) }))
	}
	withStblBoxes(t, src, 0, groups...)
	_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: t.TempDir(), Ranges: []mkv.TimeRange{{StartMs: 0}}})
	if err == nil || !strings.Contains(err.Error(), "sample-to-group") {
		t.Errorf("err = %v", err)
	}
	budget := int64(10)
	if _, err := readTrakTable(firstTrak(t, editFixture(t)), 0, 1<<40, &budget, nil); err == nil {
		t.Error("a table past the sample budget went through")
	}
}

// TestMuxBoundsChunksToTheirOwnSource: a truncated first source is refused, not read on into the next one's bytes.
func TestMuxBoundsChunksToTheirOwnSource(t *testing.T) {
	full := filepath.Join(t.TempDir(), "full.mp4")
	if err := Rewrite(context.Background(), editFixture(t), full); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(full)
	trunc := filepath.Join(t.TempDir(), "trunc.mp4")
	if err := os.WriteFile(trunc, data[:len(data)-len(data)/4], 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "mux.mp4")
	err := Mux(context.Background(), mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{
		{SourcePath: trunc, TrackID: 1}, {SourcePath: realFixture("bframes_aac.mp4"), TrackID: 2},
	}})
	if err == nil || !strings.Contains(err.Error(), "outside the file") {
		t.Errorf("err = %v", err)
	}
	if _, serr := os.Stat(dst); serr == nil {
		t.Error("a refused mux left an output")
	}
}

// withVideoEdit replaces trak 0's edit list with a media edit starting at mediaTime and lasting segDur movie ticks.
func withVideoEdit(t *testing.T, path string, mediaTime, segDur int64) {
	t.Helper()
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(k int, trak []byte) []byte {
			if k != 0 {
				return trak
			}
			tb, _ := iterBoxes(trak)
			var w bw
			for _, b := range tb {
				if b.typ == "edts" {
					continue
				}
				w.bytes(box(b.typ, b.payload))
				if b.typ == "tkhd" {
					w.bytes(buildEdit(0, mediaTime, segDur))
				}
			}
			return w.b
		})
	})
}

// TestJoinRefusesVideoItsEditListTrims: a video whose edit list hides frames at its head or tail is refused, not orphaned or drifted.
func TestJoinRefusesVideoItsEditListTrims(t *testing.T) {
	a := editShiftFixture(t, false)
	head := editShiftFixture(t, false)
	withVideoEdit(t, head, 5*40, 3000) // hides 5 frames of 40 ms
	err := Join(context.Background(), []string{a, head}, filepath.Join(t.TempDir(), "j.mp4"))
	if err == nil || !strings.Contains(err.Error(), "hides the first") {
		t.Errorf("head: err = %v", err)
	}
	tail := editShiftFixture(t, false)
	withVideoEdit(t, tail, 0, 5000) // shows 5 s of 6
	err = Join(context.Background(), []string{tail, a}, filepath.Join(t.TempDir(), "j2.mp4"))
	if err == nil || !strings.Contains(err.Error(), "hides the last") {
		t.Errorf("tail: err = %v", err)
	}
}

// TestSplitKeepsAnEditListThatBothDelaysAndTrims: a part keeps the source's empty edit and its priming trim.
func TestSplitKeepsAnEditListThatBothDelaysAndTrims(t *testing.T) {
	src := realFixture("bframes_aac.mp4")
	m := mustCutModel(t, src)
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: m.keyframesMs()[1]}, {StartMs: m.keyframesMs()[1]}}})
	p := mustCutModel(t, parts[0])
	for ti := range m.tracks {
		if got, want := p.tracks[ti].edit.mediaTime, m.tracks[ti].edit.mediaTime; got != want {
			t.Errorf("track %d: part 1 media_time %d, source %d (the priming must stay hidden)", ti+1, got, want)
		}
		if got, want := p.tracks[ti].ptNs(0, p.movieTS), m.tracks[ti].ptNs(0, m.movieTS); got != want {
			t.Errorf("track %d: part 1 presents its first sample at %d ns, source %d", ti+1, got, want)
		}
	}
}

// TestSplitPartsStayExactOnAMillisecondMovieTimescale: a part's edits are stated on a timescale that holds them exactly.
func TestSplitPartsStayExactOnAMillisecondMovieTimescale(t *testing.T) {
	src := realFixture("offset_ts48k.mp4")
	m := mustCutModel(t, src)
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 1000}, {StartMs: 1000}}})
	joined := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	j := mustCutModel(t, joined)
	for ti := range m.tracks {
		want, got := sampleSeqs(t, src)[ti], sampleSeqs(t, joined)[ti]
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("track %d: the join does not give back the source's samples and timing", ti+1)
			diffFirst(t, want, got)
		}
		if a, b := m.tracks[ti].ptNs(0, m.movieTS), j.tracks[ti].ptNs(0, j.movieTS); a != b {
			t.Errorf("track %d: first presentation %d ns, source %d", ti+1, b, a)
		}
	}
}

func TestChaptersAnMP4CannotHoldAreRefused(t *testing.T) {
	for _, tc := range []struct {
		chs  []mkv.Chapter
		want string
	}{
		{[]mkv.Chapter{{Title: "B", StartMs: 2000}, {Title: "A", StartMs: 0}}, "ascending order"},
		{[]mkv.Chapter{{Title: "A", StartMs: -100}}, "ascending order"},
		{[]mkv.Chapter{{Title: "", StartMs: 0}}, "no title"},
		{[]mkv.Chapter{{Title: "A", StartMs: 0, SubChapters: []mkv.Chapter{{Title: "S"}}}}, "sub-chapters"},
		{[]mkv.Chapter{{Title: "A", StartMs: 0}, {Title: "Z", StartMs: 9000}}, "past the end"},
		{make([]mkv.Chapter, maxChapters+1), "holds at most"},
	} {
		path := editFixture(t)
		orig, _ := os.ReadFile(path)
		err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Chapters = tc.chs })
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("chapters %v: err = %v, want %q", tc.chs, err, tc.want)
		}
		if now, _ := os.ReadFile(path); !bytes.Equal(now, orig) {
			t.Error("a refused edit wrote to the file")
		}
	}
}

// failingWrites fails every Write past its budget of Write calls, so an append fails midway.
type failingWrites struct {
	*os.File
	left int
}

func (f *failingWrites) Write(p []byte) (int, error) {
	if f.left--; f.left < 0 {
		return 0, errors.New("disk full")
	}
	return f.File.Write(p)
}

// TestFailedAppendLeavesTheFileEditable: an append cut short is cut off again, so the file is as it was.
func TestFailedAppendLeavesTheFileEditable(t *testing.T) {
	path := editFixture(t)
	orig, _ := os.ReadFile(path)
	fs := &mkv.FS{OpenFile: func(name string, flag int, perm os.FileMode) (mkv.ReadWriteSeekCloser, error) {
		f, err := os.OpenFile(name, flag, perm)
		if err != nil {
			return nil, err
		}
		return &failingWrites{File: f, left: 1}, nil
	}}
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{Title: "A", StartMs: 0}}
	}, Options{FS: fs})
	if err == nil {
		t.Fatal("the failing write went unnoticed")
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, orig) {
		t.Fatalf("the failed append left %d bytes behind", len(now)-len(orig))
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "after" }); err != nil {
		t.Errorf("the file is no longer editable: %v", err)
	}
}

// TestEditInPlaceRefusesAFileThatGrew: bytes another writer appended while the edit ran are not overwritten.
func TestEditInPlaceRefusesAFileThatGrew(t *testing.T) {
	path := editFixture(t)
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Info.Title = "x"
		f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		f.Write(box("free", []byte("grown")))
		f.Close()
	})
	if err == nil || !strings.Contains(err.Error(), "changed while") {
		t.Errorf("err = %v", err)
	}
}

func TestEditThatStoresNothingNewWritesNothing(t *testing.T) {
	path := editFixture(t)
	orig, _ := os.ReadFile(path)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Tracks[0].DefaultPresent = !c.Tracks[0].DefaultPresent }); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, orig) {
		t.Error("an edit the file cannot store wrote a moov")
	}
}

// TestRewriteKeepsTopLevelUserData: an XMP uuid box survives a rewrite.
func TestRewriteKeepsTopLevelUserData(t *testing.T) {
	src := editFixture(t)
	xmp := box("uuid", append(bytes.Repeat([]byte{0xbe}, 16), []byte("<x:xmpmeta/>")...))
	f, _ := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(xmp)
	f.Close()
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemoveTrack(context.Background(), src, dst, []uint64{2}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	if !bytes.Contains(data, xmp) {
		t.Error("the top-level uuid box was dropped")
	}
	if d := mustMeta(t, dst).DurationMs; d != 6000 {
		t.Errorf("duration after removing the audio %d ms", d)
	}
}

func TestMergeReportsTheTracksItLeavesOut(t *testing.T) {
	src := editShiftFixture(t, false)
	withForeignTrakFirst(t, src)
	var dropped []DroppedTrack
	dst := filepath.Join(t.TempDir(), "m.mp4")
	if err := Merge(context.Background(), mkv.MergeOptions{OutputPath: dst, Inputs: []mkv.MergeInput{{SourcePath: src}}}, Options{OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) }}); err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || !strings.Contains(dropped[0].Reason, "not carried") {
		t.Errorf("dropped %+v", dropped)
	}
}

// TestMuxInterleavesItsSources: a mux of two sources alternates their chunks instead of storing one after the other.
func TestMuxInterleavesItsSources(t *testing.T) {
	a, b := realFixture("bframes_aac.mp4"), realFixture("offset_ts48k.mp4")
	dst := filepath.Join(t.TempDir(), "mux.mp4")
	if err := Mux(context.Background(), mkv.MuxOptions{OutputPath: dst, Tracks: []mkv.TrackInput{{SourcePath: a, TrackID: 1}, {SourcePath: b, TrackID: 2}}}); err != nil {
		t.Fatal(err)
	}
	var first [2]int64
	var last [2]int64
	i := 0
	for _, bx := range moovBoxesOf(t, dst) {
		if bx.typ != "trak" {
			continue
		}
		chunks, err := trakChunks(bx.payload, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		first[i], last[i] = chunks[0].src, chunks[len(chunks)-1].src
		i++
	}
	if first[1] > last[0] || first[0] > last[1] {
		t.Errorf("tracks stored one after the other: video [%d, %d], audio [%d, %d]", first[0], last[0], first[1], last[1])
	}
}

func TestSplitRefusesPatternsThatAreNotOneFileName(t *testing.T) {
	src := editFixture(t)
	for _, pat := range []string{"sub/%d.mp4", "%s%n.mp4"} {
		dir := filepath.Join(t.TempDir(), "out")
		_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: dir, Pattern: pat, Ranges: []mkv.TimeRange{{StartMs: 0}}})
		if err == nil {
			t.Errorf("pattern %q went through", pat)
		}
		if _, serr := os.Stat(dir); serr == nil {
			t.Errorf("pattern %q: the output directory was created", pat)
		}
	}
}

func TestDamagedTablesAreRefusedNotGuessed(t *testing.T) {
	src := editFixture(t)
	withStblBoxes(t, src, 1, fullBox("stss", 0, 0, func(w *bw) { w.u32(100); w.u32(1) }))
	_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: t.TempDir(), Ranges: []mkv.TimeRange{{StartMs: 0}}})
	if err == nil || !strings.Contains(err.Error(), "stss") {
		t.Errorf("err = %v", err)
	}
}

func TestZeroLengthMediaEditRunsToTheEnd(t *testing.T) {
	tb := []memBox{{typ: "edts", payload: buildEdit(0, 0, 0)[8:]}}
	m, err := trakSimpleEdit(tb)
	if err != nil || m.segDur != -1 {
		t.Errorf("edit %+v, err %v", m, err)
	}
}

// TestChunksOutsideEveryMdatAreRefused: a chunk offset pointing into the ftyp is a damaged table, not media to copy.
func TestChunksOutsideEveryMdatAreRefused(t *testing.T) {
	src := editShiftFixture(t, false)
	rewriteTailMoov(t, src, func(p []byte) []byte {
		return mapTraks(t, p, func(k int, trak []byte) []byte {
			if k != 0 {
				return trak
			}
			out, err := rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
				var kids [][]byte
				for _, b := range sb {
					if b.typ == "stco" {
						p := append([]byte(nil), b.payload...)
						copy(p[8:12], []byte{0, 0, 0, 0})
						kids = append(kids, box("stco", p))
						continue
					}
					kids = append(kids, box(b.typ, b.payload))
				}
				return kids, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		})
	})
	if err := Rewrite(context.Background(), src, filepath.Join(t.TempDir(), "r.mp4")); err == nil || !strings.Contains(err.Error(), "outside every mdat") {
		t.Errorf("rewrite: %v", err)
	}
	_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: t.TempDir(), Ranges: []mkv.TimeRange{{StartMs: 0}}})
	if err == nil || !strings.Contains(err.Error(), "outside every mdat") {
		t.Errorf("split: %v", err)
	}
	err = Mux(context.Background(), mkv.MuxOptions{OutputPath: filepath.Join(t.TempDir(), "m.mp4"), Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}}})
	if err == nil || !strings.Contains(err.Error(), "outside every mdat") {
		t.Errorf("mux: %v", err)
	}
}

// TestOpenGOPSplitJoinsBack: the parts of an open-GOP split hide their leading pictures, and a join takes them back, with or without audio.
func TestOpenGOPSplitJoinsBack(t *testing.T) {
	videoOnly := filepath.Join(t.TempDir(), "v.mp4")
	if err := RemoveTrack(context.Background(), realFixture("hevc_opengop.mp4"), videoOnly, []uint64{2}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{realFixture("hevc_opengop.mp4"), videoOnly} {
		kf := mustCutModel(t, src).keyframesMs()
		parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: kf[1]}, {StartMs: kf[1]}}})
		joined := filepath.Join(t.TempDir(), "j.mp4")
		if err := Join(context.Background(), parts, joined); err != nil {
			t.Fatalf("%s: %v", filepath.Base(src), err)
		}
		want, got := sampleSeqs(t, src), sampleSeqs(t, joined)
		for ti := range want {
			if strings.Join(want[ti], ",") != strings.Join(got[ti], ",") {
				t.Errorf("%s track %d: the join does not give the source back", filepath.Base(src), ti+1)
				diffFirst(t, want[ti], got[ti])
			}
		}
	}
}

// withChpl adds a chapter list to a file without one (moov at the tail).
func withChpl(t *testing.T, path string, chs []mkv.Chapter) {
	t.Helper()
	rewriteTailMoov(t, path, func(p []byte) []byte {
		var w bw
		w.bytes(p)
		w.bytes(container("udta", buildChpl(chs)))
		return w.b
	})
}

func TestMuxTidiesTheSourceChaptersInsteadOfRefusing(t *testing.T) {
	src := editShiftFixture(t, false)
	withChpl(t, src, []mkv.Chapter{{Title: "A", StartMs: 0}, {Title: "End", StartMs: 6000}})
	dst := filepath.Join(t.TempDir(), "a.mp4")
	if err := AddTrack(context.Background(), src, dst, mkv.TrackInput{SourcePath: realFixture("bframes_aac.mp4"), TrackID: 2}); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, dst); len(c.Chapters) != 1 || c.Chapters[0].Title != "A" {
		t.Errorf("chapters %+v", c.Chapters)
	}
}

func TestTitleAndAnotherTagChangeTogether(t *testing.T) {
	path := editFixture(t)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "Old" }); err != nil {
		t.Fatal(err)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Info.Title = "New"
		c.Tags[0].SimpleTags = append(c.Tags[0].SimpleTags, mkv.SimpleTag{Name: "ARTIST", Value: "x"})
	}); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, path); c.Info.Title != "New" {
		t.Errorf("title %q", c.Info.Title)
	}
}

// TestRewriteCopiesTablesItDoesNotRead: a damaged sync table the rewrite copies verbatim does not block a metadata edit.
func TestRewriteCopiesTablesItDoesNotRead(t *testing.T) {
	src := editFixture(t)
	withStblBoxes(t, src, 1, fullBox("stss", 0, 0, func(w *bw) { w.u32(100); w.u32(1) }))
	if err := EditMetadata(context.Background(), src, filepath.Join(t.TempDir(), "o.mp4"), func(c *mkv.Container) { c.Info.Title = "x" }); err != nil {
		t.Errorf("edit refused: %v", err)
	}
}

// TestSplitByChaptersPastTheLastKeyframe: a chapter with no keyframe left to open on stays in the part before it.
func TestSplitByChaptersPastTheLastKeyframe(t *testing.T) {
	src := editShiftFixture(t, false)
	withChpl(t, src, []mkv.Chapter{{Title: "A", StartMs: 0}, {Title: "B", StartMs: 3000}, {Title: "C", StartMs: 5500}})
	parts := splitInto(t, src, mkv.SplitOptions{ByChapters: true})
	if len(parts) != 2 {
		t.Fatalf("%d parts, want 2", len(parts))
	}
	if d := mustMeta(t, parts[1]).DurationMs; d != 3000 {
		t.Errorf("part 2 lasts %d ms, want 3000 (through chapter C)", d)
	}
}

func TestReplayingTheSameChaptersWritesNothing(t *testing.T) {
	path := editFixture(t)
	set := func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{Title: "A", StartMs: 0}, {Title: "B", StartMs: 2000}}
	}
	if err := EditInPlace(context.Background(), path, set); err != nil {
		t.Fatal(err)
	}
	size := fileSize(t, path)
	if err := EditInPlace(context.Background(), path, set); err != nil {
		t.Fatal(err)
	}
	if now := fileSize(t, path); now != size {
		t.Errorf("replaying the chapters grew the file by %d bytes", now-size)
	}
}

// TestCutsAndMuxKeepTopLevelUserData: split parts, a join and a mux carry the first source's XMP uuid box.
func TestCutsAndMuxKeepTopLevelUserData(t *testing.T) {
	src := editFixture(t)
	xmp := box("uuid", append(bytes.Repeat([]byte{0xbe}, 16), []byte("<x:xmpmeta/>")...))
	f, _ := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(xmp)
	f.Close()
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	joined := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	muxed := filepath.Join(t.TempDir(), "m.mp4")
	if err := Mux(context.Background(), mkv.MuxOptions{OutputPath: muxed, Tracks: []mkv.TrackInput{{SourcePath: src, TrackID: 1}}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(parts, joined, muxed) {
		if data, _ := os.ReadFile(p); !bytes.Contains(data, xmp) {
			t.Errorf("%s: the top-level uuid box was dropped", filepath.Base(p))
		}
	}
}

// TestReplacedChapterTitlesLeaveNoMdat: once chapters are replaced in place, the mdat that held only the old titles is a free box.
func TestReplacedChapterTitlesLeaveNoMdat(t *testing.T) {
	path := editFixture(t)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Chapters = []mkv.Chapter{{Title: "A", StartMs: 0}} }); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n := 0
		for _, typ := range topLevelTypes(t, path) {
			if typ == "mdat" {
				n++
			}
		}
		return n
	}
	if n := count(); n != 2 {
		t.Fatalf("%d mdat boxes after adding chapters, want 2", n)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Chapters = nil }); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("%d mdat boxes after removing the chapters, want 1 (the titles' mdat retired)", n)
	}
	mustVerify(t, path)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Chapters = []mkv.Chapter{{Title: "B", StartMs: 0}} }); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, path); len(c.Chapters) != 1 || c.Chapters[0].Title != "B" {
		t.Errorf("chapters %+v", c.Chapters)
	}
}

// TestNegativeCompositionOffsetsCutExactly: a track with negative composition offsets (ctts v1) splits and joins back exactly.
func TestNegativeCompositionOffsetsCutExactly(t *testing.T) {
	src := realFixture("negative_cts.mp4")
	m := mustCutModel(t, src)
	kf := m.keyframesMs()
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: kf[1]}, {StartMs: kf[1]}}})
	for i, p := range parts {
		pm := mustCutModel(t, p)
		for ti := range pm.tracks {
			if mt := pm.tracks[ti].edit.mediaTime; mt < 0 {
				t.Errorf("part %d track %d: negative media_time %d", i+1, ti+1, mt)
			}
		}
	}
	joined := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	j := mustCutModel(t, joined)
	for ti := range m.tracks {
		want, got := sampleSeqs(t, src)[ti], sampleSeqs(t, joined)[ti]
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("track %d: the join does not give the source back", ti+1)
			diffFirst(t, want, got)
		}
		if a, b := m.tracks[ti].ptNs(0, m.movieTS), j.tracks[ti].ptNs(0, j.movieTS); a != b {
			t.Errorf("track %d: first presentation %d ns, source %d", ti+1, b, a)
		}
	}
}
