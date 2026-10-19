package mp4

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// sampleSeqs lists, per probe track, every sample as "dts/cto/dur/sync/sha", then "/type=index" per sample group, with decode times rebased to the track's first.
func sampleSeqs(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	m, err := loadCutModel(f, path, fi.Size(), 0, newBudget(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for _, c := range m.tracks {
		var seq []string
		types := slices.Sorted(maps.Keys(c.tt.groups))
		for i, s := range c.tt.samples {
			buf := make([]byte, s.size)
			if _, err := f.ReadAt(buf, s.src); err != nil {
				t.Fatal(err)
			}
			e := fmt.Sprintf("%d/%d/%d/%v/%x", s.dts, s.cto, s.dur, s.sync, sha256.Sum256(buf))
			for _, typ := range types {
				e += fmt.Sprintf("/%s=%d", typ, c.tt.groups[typ][i])
			}
			seq = append(seq, e)
		}
		out = append(out, seq)
	}
	return out
}

// bodies drops the timing of sampleSeqs entries, keeping flags and bytes.
func bodies(seq []string) []string {
	out := make([]string, len(seq))
	for i, s := range seq {
		parts := strings.SplitN(s, "/", 4)
		out[i] = parts[3]
	}
	return out
}

func splitInto(t *testing.T, src string, opts mkv.SplitOptions) []string {
	t.Helper()
	opts.SourcePath = src
	if opts.OutputDir == "" {
		opts.OutputDir = t.TempDir()
	}
	parts, err := Split(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return parts
}

func TestSplitThenJoinGivesBackEverySample(t *testing.T) {
	for _, name := range []string{"synth", "bframes_aac.mp4", "bframes_aac.mov"} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join("..", "internal", "testdata", name)
			if name == "synth" {
				src = editFixture(t)
			}
			kf := mustCutModel(t, src).keyframesMs()
			if len(kf) < 3 {
				t.Skipf("%d keyframes", len(kf))
			}
			parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: kf[1]}, {StartMs: kf[1], EndMs: kf[2]}, {StartMs: kf[2]}}})
			if len(parts) != 3 {
				t.Fatalf("%d parts", len(parts))
			}
			want := sampleSeqs(t, src)
			var got [][]string
			for _, p := range parts {
				for ti, seq := range sampleSeqs(t, p) {
					if len(got) <= ti {
						got = append(got, nil)
					}
					got[ti] = append(got[ti], bodies(seq)...)
				}
				if c := mustMeta(t, p); len(c.Tracks) != len(mustMeta(t, src).Tracks) {
					t.Errorf("%s: %d tracks", p, len(c.Tracks))
				}
			}
			for ti := range want {
				if strings.Join(bodies(want[ti]), ",") != strings.Join(got[ti], ",") {
					t.Errorf("track %d: the parts do not hold the source's samples once each (%d vs %d)", ti+1, len(want[ti]), len(got[ti]))
				}
			}
			joined := filepath.Join(t.TempDir(), "joined"+filepath.Ext(src))
			if err := Join(context.Background(), parts, joined); err != nil {
				t.Fatal(err)
			}
			back := sampleSeqs(t, joined)
			for ti := range want {
				if strings.Join(want[ti], ",") != strings.Join(back[ti], ",") {
					t.Errorf("track %d: the join does not give back the source's samples and timing", ti+1)
					diffFirst(t, want[ti], back[ti])
				}
			}
			sd, jd := mustMeta(t, src).DurationMs, mustMeta(t, joined).DurationMs
			if d := sd - jd; d < -1 || d > 1 {
				t.Errorf("duration %d ms after the join, %d before", jd, sd)
			}
		})
	}
}

func diffFirst(t *testing.T, a, b []string) {
	t.Helper()
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			t.Logf("first difference at sample %d: %s vs %s", i, a[i], b[i])
			return
		}
	}
	t.Logf("lengths %d vs %d", len(a), len(b))
}

func mustCutModel(t *testing.T, path string) *cutModel {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	m, err := loadCutModel(f, path, fi.Size(), 0, newBudget(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSplitOpensOnKeyframesAndKeepsAVSync(t *testing.T) {
	src := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(100))
	mp := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), src, mp); err != nil {
		t.Fatal(err)
	}
	if d := measuredAudioDelayMs(t, mp); d != 100 {
		t.Fatalf("fixture audio delay %d ms", d)
	}
	parts := splitInto(t, mp, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2500}, {StartMs: 2500}}})
	if d := measuredAudioDelayMs(t, parts[0]); d != 100 {
		t.Errorf("part 1 audio delay %d ms, want 100", d)
	}
	if d := measuredAudioDelayMs(t, parts[1]); d != 0 {
		t.Errorf("part 2 audio delay %d ms, want 0 (audio runs through the cut)", d)
	}
	c1, c2 := mustMeta(t, parts[0]), mustMeta(t, parts[1])
	kf2 := mustCutModel(t, parts[1]).keyframesMs()
	if c1.DurationMs != 3000 || c2.DurationMs != 3100 || len(kf2) == 0 || kf2[0] != 0 {
		t.Errorf("part 1 lasts %d ms (want 3000: the cut lands on the 3 s keyframe), part 2 %d ms (the delayed audio ends at 6.1 s), keyframes %v", c1.DurationMs, c2.DurationMs, kf2)
	}
	joined := filepath.Join(t.TempDir(), "joined.mp4")
	if err := Join(context.Background(), parts, joined); err != nil {
		t.Fatal(err)
	}
	if d := measuredAudioDelayMs(t, joined); d != 100 {
		t.Errorf("joined audio delay %d ms, want 100", d)
	}
}

func TestSplitByChaptersAndEvery(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "Intro", StartMs: 0}, {ID: 2, Title: "Part: two", StartMs: 2000}, {ID: 3, Title: "End", StartMs: 4000}}
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	parts := splitInto(t, src, mkv.SplitOptions{OutputDir: dir, ByChapters: true, Pattern: "{title}.mp4"})
	want := []string{"Intro.mp4", "Part_ two.mp4", "End.mp4"}
	for i, p := range parts {
		if filepath.Base(p) != want[i] {
			t.Errorf("part %d named %q, want %q", i+1, filepath.Base(p), want[i])
		}
		c := mustMeta(t, p)
		if len(c.Chapters) != 1 || c.Chapters[0].StartMs != 0 || c.DurationMs != 2000 {
			t.Errorf("%s: chapters %+v, %d ms", p, c.Chapters, c.DurationMs)
		}
	}
	if len(parts) != 3 {
		t.Fatalf("%d parts", len(parts))
	}
	every := splitInto(t, src, mkv.SplitOptions{EveryMs: 1500})
	if len(every) != 3 {
		t.Errorf("-every 1.5s over 6 s with 1 s keyframes: %d parts, want 3", len(every))
	}
}

func TestSplitRefusals(t *testing.T) {
	src := editFixture(t)
	for _, tc := range []struct {
		r    []mkv.TimeRange
		want string
	}{
		{[]mkv.TimeRange{{StartMs: 2200, EndMs: 2600}}, "holds no keyframe"},
		{[]mkv.TimeRange{{StartMs: 9000}}, "past the end"},
		{[]mkv.TimeRange{{StartMs: 4000, EndMs: 3000}}, "before it starts"},
		{nil, "no split ranges"},
	} {
		_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: t.TempDir(), Ranges: tc.r})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ranges %v: err = %v, want %q", tc.r, err, tc.want)
		}
	}
	_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: t.TempDir(), Pattern: "../x_%d.mp4", Ranges: []mkv.TimeRange{{StartMs: 0}}})
	if err == nil {
		t.Error("a pattern escaping the output directory went through")
	}
	// By chapters the ranges come from the file: the refusal names the chapter and what to do, not "widen it".
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "Short", StartMs: 2500}, {ID: 3, Title: "C", StartMs: 2700}}
	}); err != nil {
		t.Fatal(err)
	}
	_, err = Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: t.TempDir(), ByChapters: true})
	if err == nil || strings.Contains(err.Error(), "widen it") || !strings.Contains(err.Error(), `"Short"`) || !strings.Contains(err.Error(), "-range") {
		t.Errorf("split by chapters on a chapter with no keyframe: %v", err)
	}
}

func TestJoinRefusesDifferentStreams(t *testing.T) {
	a := editFixture(t)
	tracks := synthFixtureTracks()
	w := uint32(640)
	tracks[0].Width = &w
	src := buildMKV(t, tracks, synthFixtureBlocks(0))
	b := filepath.Join(t.TempDir(), "b.mp4")
	if err := RemuxToMP4(context.Background(), src, b); err != nil {
		t.Fatal(err)
	}
	err := Join(context.Background(), []string{a, b}, filepath.Join(t.TempDir(), "j.mp4"))
	if err == nil || !strings.Contains(err.Error(), "encoded differently") {
		t.Errorf("err = %v", err)
	}
	only := RemoveTrack(context.Background(), a, filepath.Join(t.TempDir(), "v.mp4"), []uint64{2})
	if only != nil {
		t.Fatal(only)
	}
	err = Join(context.Background(), []string{a, a}, filepath.Join(t.TempDir(), "self.mp4"))
	if err != nil {
		t.Errorf("joining a file to itself: %v", err)
	}
	if err := Join(context.Background(), []string{a}, filepath.Join(t.TempDir(), "one.mp4")); err == nil {
		t.Error("a one-file join went through")
	}
}

func newBudget() *int64 {
	b := int64(maxSamples)
	return &b
}

// TestSplitByChaptersKeepsTheHeadBeforeTheFirstChapter: pictures before the first chapter's start belong to the first part, none is lost.
func TestSplitByChaptersKeepsTheHeadBeforeTheFirstChapter(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 500}, {ID: 2, Title: "B", StartMs: 2500}}
	}); err != nil {
		t.Fatal(err)
	}
	parts := splitInto(t, src, mkv.SplitOptions{ByChapters: true})
	want := sampleSeqs(t, src)
	got := make([]int, len(want))
	for _, p := range parts {
		for ti, seq := range sampleSeqs(t, p) {
			got[ti] += len(seq)
		}
	}
	for ti := range want {
		if got[ti] != len(want[ti]) {
			t.Errorf("track %d: the parts hold %d samples of the source's %d", ti+1, got[ti], len(want[ti]))
		}
	}
}

// TestSplitNeverOpensOnPicturesTheEditListHides: keyframes past the video's presented end cut nothing, and no part plays them.
func TestSplitNeverOpensOnPicturesTheEditListHides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  mkv.SplitOptions
		parts int
	}{
		{"every", mkv.SplitOptions{EveryMs: 1000}, 3},
		{"chapters", mkv.SplitOptions{ByChapters: true}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := editShiftFixture(t, false)
			if tc.opts.ByChapters {
				if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
					c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 1000}, {ID: 3, Title: "C", StartMs: 4000}}
				}); err != nil {
					t.Fatal(err)
				}
			}
			withVideoEdit(t, src, 0, 2500) // the video shows 2.5 s of its 6, keyframes at 3, 4 and 5 s hidden
			parts := splitInto(t, src, tc.opts)
			var shown int64
			for _, p := range parts {
				pm := mustCutModel(t, p)
				v := &pm.tracks[0]
				if v.edit.segDur < 0 {
					t.Fatalf("%s: the video's media edit runs to the end of its media", filepath.Base(p))
				}
				shown += rescale(v.edit.segDur, pm.movieTS, nsPerSec)
			}
			if len(parts) != tc.parts {
				t.Errorf("%d parts, want %d", len(parts), tc.parts)
			}
			if d := shown - 2_500_000_000; d < -1_000_000 || d > 1_000_000 {
				t.Errorf("the parts show %d ms of video, the source 2500", shown/1_000_000)
			}
		})
	}
}

// TestJoinRefusesAnotherDisplayMatrix: a portrait clip and a landscape one of the same encoding would come out under the first's matrix, the second turned.
func TestJoinRefusesAnotherDisplayMatrix(t *testing.T) {
	a, b := editFixture(t), editFixture(t)
	rewriteTailMoov(t, b, func(p []byte) []byte {
		return mapTraks(t, p, func(k int, trak []byte) []byte {
			if k != 0 {
				return trak
			}
			tb, _ := iterBoxes(trak)
			var w bw
			for _, x := range tb {
				if x.typ == "tkhd" {
					q := append([]byte(nil), x.payload...)
					off := 40
					if q[0] == 1 {
						off = 52
					}
					binary.BigEndian.PutUint32(q[off:], 0)
					binary.BigEndian.PutUint32(q[off+4:], 0x10000)
					binary.BigEndian.PutUint32(q[off+12:], 0xFFFF0000)
					binary.BigEndian.PutUint32(q[off+16:], 0)
					w.bytes(box("tkhd", q))
					continue
				}
				w.bytes(box(x.typ, x.payload))
			}
			return w.b
		})
	})
	err := Join(context.Background(), []string{a, b}, filepath.Join(t.TempDir(), "j.mp4"))
	if err == nil || !strings.Contains(err.Error(), "turned") {
		t.Errorf("err = %v, want a refusal of the other display matrix", err)
	}
}

// TestSplitLeavesATrackItsEditListEndedEmpty: a track the edit list has ended before a cut holds nothing in the part after it.
func TestSplitLeavesATrackItsEditListEndedEmpty(t *testing.T) {
	src := editShiftFixture(t, false)
	rewriteTailMoov(t, src, func(p []byte) []byte {
		return mapTraks(t, p, func(k int, trak []byte) []byte {
			if k != 1 {
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
					w.bytes(buildEdit(0, 0, 2500)) // the sound shows 2.5 s of its 6
				}
			}
			return w.b
		})
	})
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 3000}, {StartMs: 3000}}})
	m := mustCutModel(t, parts[1])
	if n := len(m.tracks[1].tt.samples); n != 0 {
		t.Errorf("part 2 holds %d sound samples the edit list hides", n)
	}
	if n := len(m.tracks[0].tt.samples); n == 0 {
		t.Error("part 2 lost its pictures")
	}
}

// TestJoinComparesDisplayOnVideoOnly: a sound track's tkhd states no picture, so a width one muxer leaves there does
// not refuse the join; only the video's matrix and size are compared.
func TestJoinComparesDisplayOnVideoOnly(t *testing.T) {
	a, b := editFixture(t), editFixture(t)
	rewriteTailMoov(t, b, func(p []byte) []byte {
		return mapTraks(t, p, func(k int, trak []byte) []byte {
			if k != 1 {
				return trak
			}
			tb, _ := iterBoxes(trak)
			var w bw
			for _, x := range tb {
				if x.typ == "tkhd" {
					q := append([]byte(nil), x.payload...)
					off := 40
					if q[0] == 1 {
						off = 52
					}
					binary.BigEndian.PutUint32(q[off+36:], 320<<16) // a width on a sound track
					w.bytes(box("tkhd", q))
					continue
				}
				w.bytes(box(x.typ, x.payload))
			}
			return w.b
		})
	})
	if err := Join(context.Background(), []string{a, b}, filepath.Join(t.TempDir(), "j.mp4")); err != nil {
		t.Errorf("a sound track's tkhd width refused the join: %v", err)
	}
}

// TestJoinRefusesTracksInAnotherLanguageOrder: an MP4 join pairs tracks by position as a Matroska one does, so a
// second file whose audio states another language is refused; a track that states none joins.
func TestJoinRefusesTracksInAnotherLanguageOrder(t *testing.T) {
	a, b := editFixture(t), editFixture(t)
	setLang := func(path, l string) {
		if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Tracks[1].Language, c.Tracks[1].LanguageBCP47 = l, "" }); err != nil {
			t.Fatal(err)
		}
	}
	setLang(a, "eng")
	setLang(b, "fre")
	if err := Join(context.Background(), []string{a, b}, filepath.Join(t.TempDir(), "j.mp4")); err == nil || !strings.Contains(err.Error(), "same order") {
		t.Errorf("English then French audio: %v", err)
	}
	setLang(b, "eng")
	if err := Join(context.Background(), []string{a, b}, filepath.Join(t.TempDir(), "j.mp4")); err != nil {
		t.Errorf("same language: %v", err)
	}
}

// TestSplitByChaptersListedOutOfOrder: a chpl another tool wrote out of order is read in time order, each chapter
// ending where the next in time starts, so a split by chapters gives parts that do not overlap.
func TestSplitByChaptersListedOutOfOrder(t *testing.T) {
	src := copyFixture(t, "fps30.mp4")
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A0", StartMs: 0}, {ID: 2, Title: "B1", StartMs: 900}, {ID: 3, Title: "C2", StartMs: 1800}}
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(data), "chpl")
	if i < 0 {
		t.Fatal("no chpl")
	}
	e := i + 13 // version and flags, reserved, count: then 11-byte entries (time, length, two-letter title)
	first := append([]byte(nil), data[e:e+11]...)
	copy(data[e:e+11], data[e+11:e+22])
	copy(data[e+11:e+22], first)
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, src).Chapters; len(got) != 3 || got[0].Title != "A0" || got[0].EndMs != 900 || got[1].EndMs != 1800 {
		t.Errorf("chapters read in time order, each ending at the next: %+v", got)
	}
	total := len(mustCutModel(t, src).tracks[0].tt.samples)
	n := 0
	for _, p := range splitInto(t, src, mkv.SplitOptions{ByChapters: true}) {
		n += len(mustCutModel(t, p).tracks[0].tt.samples)
	}
	if n != total {
		t.Errorf("the parts hold %d pictures of the source's %d", n, total)
	}
}
