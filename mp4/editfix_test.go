package mp4

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// rewriteTailMoov replaces the payload of a file's moov, which must be its last box.
func rewriteTailMoov(t *testing.T, path string, fn func(payload []byte) []byte) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	off, plen, err := findMoov(f, int64(len(data)))
	f.Close()
	if err != nil || off+plen != int64(len(data)) {
		t.Fatalf("moov not at the tail (%v)", err)
	}
	out := append(slicesClone(data[:off-8]), box("moov", fn(data[off:off+plen]))...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// mapTraks applies fn to the trak at position i of a moov payload.
func mapTraks(t *testing.T, payload []byte, fn func(i int, trak []byte) []byte) []byte {
	t.Helper()
	boxes, err := iterBoxes(payload)
	if err != nil {
		t.Fatal(err)
	}
	var w bw
	i := -1
	for _, b := range boxes {
		if b.typ == "trak" {
			i++
			w.bytes(box("trak", fn(i, b.payload)))
			continue
		}
		w.bytes(box(b.typ, b.payload))
	}
	return w.b
}

func TestEditInPlaceTrakUdtaAfterMdia(t *testing.T) {
	path := editFixture(t)
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(i int, trak []byte) []byte {
			if i != 1 {
				return trak
			}
			return append(slicesClone(trak), container("udta", buildTrackName("Old"))...)
		})
	})
	if got := mustMeta(t, path).Tracks[1].Name; got != "Old" {
		t.Fatalf("fixture name %q", got)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Tracks[1].Name = "New"; c.Tracks[1].IsForced = true }); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range moovBoxesOf(t, path) {
		if b.typ == "trak" {
			n++
			if n != 2 {
				continue
			}
			tb, _ := iterBoxes(b.payload)
			count := 0
			for _, x := range tb {
				if x.typ == "udta" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("%d udta boxes in the edited trak", count)
			}
		}
	}
	c := mustMeta(t, path)
	if c.Tracks[1].Name != "New" || !c.Tracks[1].IsForced {
		t.Errorf("name %q forced %v", c.Tracks[1].Name, c.Tracks[1].IsForced)
	}
}

func TestOpenLastChapterEndsWithTheMovie(t *testing.T) {
	path := editFixture(t)
	before := mustMeta(t, path).DurationMs
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0, EndMs: 1000}, {ID: 2, Title: "B", StartMs: 4500}}
	}); err != nil {
		t.Fatal(err)
	}
	c := mustMeta(t, path)
	if c.DurationMs != before {
		t.Errorf("duration %d ms after adding chapters, %d before", c.DurationMs, before)
	}
	for _, b := range moovBoxesOf(t, path) {
		if b.typ == "trak" {
			if d, _ := trakDuration(b.payload); d > before {
				t.Errorf("a trak lasts %d ms, past the movie's %d", d, before)
			}
		}
	}
}

// withSaio marks the first trak as carrying CENC auxiliary data.
func withSaio(t *testing.T, path string) {
	rewriteTailMoov(t, path, func(p []byte) []byte {
		return mapTraks(t, p, func(i int, trak []byte) []byte {
			if i != 0 {
				return trak
			}
			out, err := rewriteAt(trak, []string{"mdia", "minf", "stbl"}, func(sb []memBox) ([][]byte, error) {
				var kids [][]byte
				for _, b := range sb {
					kids = append(kids, box(b.typ, b.payload))
				}
				return append(kids, fullBox("saio", 0, 0, func(w *bw) { w.u32(1); w.u32(16) })), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			return out
		})
	})
}

// TestEditInPlaceRefusesEncryptedFiles: an in-place edit refuses CENC data addressed by offset; retime keeps accepting it, as it did.
func TestEditInPlaceRefusesEncryptedFiles(t *testing.T) {
	path := editShiftFixture(t, false)
	withSaio(t, path)
	orig, _ := os.ReadFile(path)
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "x" })
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("edit: %v", err)
	}
	if now, _ := os.ReadFile(path); string(now) != string(orig) {
		t.Error("a refused edit changed the file")
	}
	if err := RetimeTracks(context.Background(), path, map[uint64]int64{2: 100_000_000}); err != nil {
		t.Errorf("retime: %v", err)
	}
}

func TestElstCountPastItsBytesIsRefused(t *testing.T) {
	elst := fullBox("elst", 0, 0, func(w *bw) { w.u32(0xFFFFFFFF) })
	tb := []memBox{{typ: "edts", payload: elst}}
	if _, err := trakElstEntries(tb); err == nil {
		t.Error("an elst declaring 4G entries in 0 bytes went through")
	}
}

func TestSplitWritesNothingWhenARangeFails(t *testing.T) {
	src := editFixture(t)
	dir := t.TempDir()
	_, err := Split(context.Background(), mkv.SplitOptions{SourcePath: src, OutputDir: dir, Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 2000}, {StartMs: 7000, EndMs: 9000}}})
	if err == nil {
		t.Fatal("no error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files left behind", len(entries))
	}
}

// TestJoinIndependentFilesDoesNotDrift: an AAC track whose edit list hides encoder padding at its end starts each next file exactly on time.
func TestJoinIndependentFilesDoesNotDrift(t *testing.T) {
	src := realFixture("bframes_aac.mp4")
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), []string{src, src, src}, dst); err != nil {
		t.Fatal(err)
	}
	s, j := mustCutModel(t, src), mustCutModel(t, dst)
	for ti := range s.tracks {
		sc, jc := &s.tracks[ti], &j.tracks[ti]
		n, m := len(sc.tt.samples), len(jc.tt.samples)
		want := 2*s.movieNs + sc.ptNs(n-1, s.movieTS)
		if got := jc.ptNs(m-1, j.movieTS); got-want > 1000 || want-got > 1000 {
			t.Errorf("track %d: the third file's last sample presents at %d ns, want %d (drift %d ns)", ti+1, got, want, got-want)
		}
	}
	if d := mustCutModel(t, dst).movieNs; d != 3*s.movieNs {
		t.Errorf("joined length %d ns, want %d", d, 3*s.movieNs)
	}
}

func TestJoinTrackEmptyInTheFirstFile(t *testing.T) {
	mk := func(samples []rwSample) *cutModel {
		return &cutModel{movieTS: 1000, movieNs: 2 * nsPerSec, tracks: []cutTrack{{tt: &trakTable{samples: samples}, ts: 1000, edit: simpleEdit{segDur: -1}}}}
	}
	models := []*cutModel{mk(nil), mk([]rwSample{{dts: 0, dur: 500, size: 1, sync: true}, {dts: 500, dur: 500, size: 1, sync: true}})}
	var vs vsource
	tt, lead, err := joinTables(models, []int64{0, 2 * nsPerSec}, 0, &vs)
	if err != nil {
		t.Fatal(err)
	}
	if lead != 2000 || len(tt.samples) != 2 || tt.samples[0].dts != 0 {
		t.Errorf("lead %d ticks, samples %+v", lead, tt.samples)
	}
}

// TestRemoveTrackKeepsChaptersOnlyTheVideoPointedAt: with no chpl, removing the one trak referencing the chapter track keeps the chapters.
func TestRemoveTrackKeepsChaptersOnlyTheVideoPointedAt(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 3000}}
	}); err != nil {
		t.Fatal(err)
	}
	rewriteTailMoov(t, src, func(p []byte) []byte {
		boxes, _ := iterBoxes(p)
		var w bw
		i := -1
		for _, b := range boxes {
			switch b.typ {
			case "udta":
				continue // no chpl: the chapter track alone carries the chapters
			case "trak":
				i++
				if i == 1 {
					out, err := dropTref(b.payload)
					if err != nil {
						t.Fatal(err)
					}
					w.bytes(box("trak", out))
					continue
				}
			}
			w.bytes(box(b.typ, b.payload))
		}
		return w.b
	})
	if n := len(mustMeta(t, src).Chapters); n != 2 {
		t.Fatalf("fixture has %d chapters", n)
	}
	dst := filepath.Join(t.TempDir(), "a.mp4")
	if err := RemoveTrack(context.Background(), src, dst, []uint64{1}); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, dst); len(c.Chapters) != 2 || len(c.Tracks) != 1 {
		t.Errorf("chapters %+v, %d tracks", c.Chapters, len(c.Tracks))
	}
}

func TestSplitThenJoinGivesChaptersBackWhole(t *testing.T) {
	src := editFixture(t)
	chs := []mkv.Chapter{{ID: 1, Title: "Intro", StartMs: 0, EndMs: 3000}, {ID: 2, Title: "Rest", StartMs: 3000, EndMs: 6000}}
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) { c.Chapters = chs }); err != nil {
		t.Fatal(err)
	}
	parts := splitInto(t, src, mkv.SplitOptions{EveryMs: 2000})
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, dst); err != nil {
		t.Fatal(err)
	}
	got := mustMeta(t, dst).Chapters
	if len(got) != 2 || got[0].Title != "Intro" || got[1].StartMs != 3000 || got[1].EndMs != 6000 {
		t.Errorf("chapters after split and join: %+v", got)
	}
}

// TestJoinMergesAChapterCutBetweenTwoMs: a 30 fps cut falls between two milliseconds, and the chapter it splits still comes back whole.
func TestJoinMergesAChapterCutBetweenTwoMs(t *testing.T) {
	data, err := os.ReadFile(realFixture("fps30.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "in.mp4")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 500}}
	}); err != nil {
		t.Fatal(err)
	}
	kf := mustCutModel(t, src).keyframesMs() // 0, 833, 1667: the last rounds up
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: kf[2]}, {StartMs: kf[2]}}})
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, dst); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, dst).Chapters; len(got) != 2 || got[1].Title != "B" || got[1].StartMs != 500 {
		t.Errorf("chapters after a split at %d ms and a join: %+v", kf[2], got)
	}
}

// TestSplitAndJoinKeepTheChapterTrack: QuickTime players read the chapter track, not the chpl, so an output whose chapters come out unchanged keeps one too.
func TestSplitAndJoinKeepTheChapterTrack(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 1000}}
	}); err != nil {
		t.Fatal(err)
	}
	parts := splitInto(t, src, mkv.SplitOptions{Ranges: []mkv.TimeRange{{StartMs: 0, EndMs: 4000}, {StartMs: 4000}}})
	dst := filepath.Join(t.TempDir(), "j.mp4")
	if err := Join(context.Background(), parts, dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range append(parts, dst) {
		if len(chapterTrackRefs(moovBoxesOf(t, p))) == 0 {
			t.Errorf("%s has no chapter track", filepath.Base(p))
		}
	}
}

func TestEditedTitleSurvivesDroppingTheTags(t *testing.T) {
	path := editFixture(t)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Info.Title = "Keep"
		c.Tags = []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "TITLE", Value: "Keep"}, {Name: "ARTIST", Value: "x"}}}}
	}); err != nil {
		t.Fatal(err)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Tags = nil }); err != nil {
		t.Fatal(err)
	}
	if c := mustMeta(t, path); c.Info.Title != "Keep" {
		t.Errorf("title %q after dropping the tags", c.Info.Title)
	}
}

// TestProbeReadsChapterTrackOnlyTitles: with no chpl, the probe reads the chapter track's titles as an edit does, titles of any length.
func TestProbeReadsChapterTrackOnlyTitles(t *testing.T) {
	src := editFixture(t)
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "A much longer title", StartMs: 2000}, {ID: 3, Title: "Mid", StartMs: 4000}}
	}); err != nil {
		t.Fatal(err)
	}
	rewriteTailMoov(t, src, func(p []byte) []byte {
		boxes, _ := iterBoxes(p)
		var w bw
		for _, b := range boxes {
			if b.typ != "udta" {
				w.bytes(box(b.typ, b.payload))
			}
		}
		return w.b
	})
	var got []string
	for _, ch := range mustMeta(t, src).Chapters {
		got = append(got, ch.Title)
	}
	if strings.Join(got, "|") != "A|A much longer title|Mid" {
		t.Errorf("probe chapters %q", got)
	}
}

// TestJoinRefusesALongAudioGap: an audio gap a held last sample would fill is stretched up to maxAudioGapMs, refused past it.
func TestJoinRefusesALongAudioGap(t *testing.T) {
	mk := func() *cutModel {
		samples := []rwSample{{dts: 0, dur: 500, size: 1, sync: true}, {dts: 500, dur: 500, size: 1, sync: true}}
		return &cutModel{movieTS: 1000, movieNs: 2 * nsPerSec, tracks: []cutTrack{{tt: &trakTable{samples: samples}, ts: 1000, handler: "soun", edit: simpleEdit{segDur: -1}}}}
	}
	for _, c := range []struct {
		next int64
		ok   bool
	}{{1400, true}, {1500, true}, {1501, false}, {2000, false}} {
		var vs vsource
		tt, _, err := joinTables([]*cutModel{mk(), mk()}, []int64{0, c.next * 1_000_000}, 0, &vs)
		if (err == nil) != c.ok {
			t.Errorf("next file at %d ms: err %v", c.next, err)
			continue
		}
		if c.ok && tt.samples[1].dur != uint32(c.next-500) {
			t.Errorf("next file at %d ms: last sample lasts %d ticks", c.next, tt.samples[1].dur)
		}
	}
}

// TestDanglingChapterRefIsDropped: a tref/chap naming track_ID 0 (no trak) is left out by a rewrite and replaced by new chapters.
func TestDanglingChapterRefIsDropped(t *testing.T) {
	src := editFixture(t)
	rewriteTailMoov(t, src, func(p []byte) []byte {
		return mapTraks(t, p, func(i int, trak []byte) []byte {
			tb, err := iterBoxes(trak)
			if err != nil {
				t.Fatal(err)
			}
			var w bw
			for _, b := range tb {
				if b.typ == "tref" {
					continue
				}
				w.bytes(box(b.typ, b.payload))
				if b.typ == "tkhd" {
					w.bytes(buildTrefChap(0))
				}
			}
			return w.b
		})
	})
	if len(danglingRefs(moovBoxesOf(t, src))) == 0 {
		t.Fatal("fixture has no dangling reference")
	}
	dst := filepath.Join(t.TempDir(), "rw.mp4")
	if err := Rewrite(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if d := danglingRefs(moovBoxesOf(t, dst)); len(d) != 0 {
		t.Errorf("rewrite kept references to %v", d)
	}
	if err := EditInPlace(context.Background(), src, func(c *mkv.Container) {
		c.Chapters = []mkv.Chapter{{ID: 1, Title: "A", StartMs: 0}, {ID: 2, Title: "B", StartMs: 3000}}
	}); err != nil {
		t.Fatal(err)
	}
	if d := danglingRefs(moovBoxesOf(t, src)); len(d) != 0 {
		t.Errorf("the edit kept references to %v next to its chapter track", d)
	}
	if n := len(mustMeta(t, src).Chapters); n != 2 {
		t.Errorf("%d chapters", n)
	}
}
