package mp4

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// editFixture is a video+audio MP4 carrying content hashes, its moov at the tail.
func editFixture(t *testing.T) string {
	t.Helper()
	src := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))
	dst := filepath.Join(t.TempDir(), "in.mp4")
	if err := RemuxToMP4(context.Background(), src, dst, Options{ContentHashes: true}); err != nil {
		t.Fatal(err)
	}
	return dst
}

func mustMeta(t *testing.T, path string) *mkv.Container {
	t.Helper()
	c, _, err := OpenMeta(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustVerify(t *testing.T, path string) {
	t.Helper()
	bad, err := VerifyContentHashes(context.Background(), path)
	if err != nil || len(bad) > 0 {
		t.Fatalf("content hashes after the edit: %v %v", bad, err)
	}
}

func TestEditInPlaceTrackAndMovieMeta(t *testing.T) {
	path := editFixture(t)
	err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Info.Title = "New title"
		c.Tags = []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "ARTIST", Value: "Someone"}, {Name: "GENRE", Value: "Drama"}}}}
		c.Tracks[0].Name = "Main video"
		c.Tracks[0].IsDefault = false
		c.Tracks[1].Language = "fre"
		c.Tracks[1].Name = "Français"
		c.Tracks[1].IsForced = true
	})
	if err != nil {
		t.Fatal(err)
	}
	c := mustMeta(t, path)
	if c.Info.Title != "New title" {
		t.Errorf("title = %q", c.Info.Title)
	}
	tags := map[string]string{}
	for _, st := range c.Tags[0].SimpleTags {
		tags[st.Name] = st.Value
	}
	if tags["ARTIST"] != "Someone" || tags["GENRE"] != "Drama" || tags["TITLE"] != "New title" {
		t.Errorf("tags = %v", tags)
	}
	v, a := c.Tracks[0], c.Tracks[1]
	if v.Name != "Main video" || v.IsDefault {
		t.Errorf("video track = name %q default %v", v.Name, v.IsDefault)
	}
	if a.Language != "fre" || a.Name != "Français" || !a.IsForced || a.IsDefault {
		t.Errorf("audio track = lang %q name %q forced %v default %v", a.Language, a.Name, a.IsForced, a.IsDefault)
	}
	mustVerify(t, path)

	// Clearing goes back to nothing, not to a stale value.
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
		c.Tracks[0].Name = ""
		c.Tracks[1].IsForced = false
		c.Info.Title = ""
	}); err != nil {
		t.Fatal(err)
	}
	c = mustMeta(t, path)
	if c.Tracks[0].Name != "" || c.Tracks[1].IsForced || c.Info.Title != "" {
		t.Errorf("after clearing: name %q forced %v title %q", c.Tracks[0].Name, c.Tracks[1].IsForced, c.Info.Title)
	}
	mustVerify(t, path)
}

func TestEditInPlaceChapters(t *testing.T) {
	path := editFixture(t)
	set := func(titles ...string) {
		t.Helper()
		if err := EditInPlace(context.Background(), path, func(c *mkv.Container) {
			c.Chapters = nil
			for i, ti := range titles {
				c.Chapters = append(c.Chapters, mkv.Chapter{ID: uint64(i + 1), Title: ti, StartMs: int64(i) * 2000})
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(titles ...string) {
		t.Helper()
		c := mustMeta(t, path)
		var got []string
		for _, ch := range c.Chapters {
			got = append(got, ch.Title)
		}
		if strings.Join(got, "|") != strings.Join(titles, "|") {
			t.Errorf("chpl chapters = %v, want %v", got, titles)
		}
		// The QuickTime chapter track must say the same, and exist once.
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		fi, _ := f.Stat()
		mv, err := parseMP4(f, fi.Size(), sampleNone)
		if err != nil {
			t.Fatal(err)
		}
		if len(titles) == 0 {
			if mv.chapterTrak != nil {
				t.Error("a chapter track is left behind")
			}
			return
		}
		if mv.chapterTrak == nil {
			t.Fatal("no QuickTime chapter track")
		}
		got = nil
		for _, ch := range readChapterTrack(f, chapterTrackSamples(mv.chapterTrak, fi.Size())) {
			got = append(got, ch.Title)
		}
		if strings.Join(got, "|") != strings.Join(titles, "|") {
			t.Errorf("chapter track = %v, want %v", got, titles)
		}
		moov := moovBoxesOf(t, path)
		if n := len(chapterTrackRefs(moov)); n != 1 {
			t.Errorf("%d chapter tracks referenced, want 1", n)
		}
		traks := 0
		for _, b := range moov {
			if b.typ == "trak" {
				traks++
			}
		}
		if traks != 3 {
			t.Errorf("%d traks, want 3", traks)
		}
	}
	set("Intro", "Middle")
	check("Intro", "Middle")
	set("Opening", "Act 1", "Credits")
	check("Opening", "Act 1", "Credits")
	mustVerify(t, path)
	set()
	check()
	mustVerify(t, path)
}

// moovBoxesOf returns the children of the file's authoritative moov.
func moovBoxesOf(t *testing.T, path string) []memBox {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	payload, err := readMoov(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}
	boxes, err := iterBoxes(payload)
	if err != nil {
		t.Fatal(err)
	}
	return boxes
}

func TestEditInPlaceRefusals(t *testing.T) {
	cases := []struct {
		name string
		fn   func(c *mkv.Container)
		want string
	}{
		{"codec", func(c *mkv.Container) { c.Tracks[0].Codec = "hevc" }, "track 1's Codec cannot be changed"},
		{"track list", func(c *mkv.Container) { c.Tracks = c.Tracks[:1] }, "keeps the track list"},
		{"bcp47 in the legacy field", func(c *mkv.Container) { c.Tracks[1].Language = "fr-CA" }, "3-letter ISO 639-2"},
		{"track tag", func(c *mkv.Container) {
			c.Tags = []mkv.Tag{{TargetID: 2, SimpleTags: []mkv.SimpleTag{{Name: "ARTIST", Value: "x"}}}}
		}, "targeting track 2"},
		{"unknown tag", func(c *mkv.Container) { c.Tags = []mkv.Tag{{SimpleTags: []mkv.SimpleTag{{Name: "BPM", Value: "x"}}}} }, `tag "BPM" has no MP4 counterpart`},
		{"attachment", func(c *mkv.Container) { c.Attachments = append(c.Attachments, mkv.Attachment{Name: "x"}) }, "Attachments cannot be changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := editFixture(t)
			orig, _ := os.ReadFile(path)
			err := EditInPlace(context.Background(), path, tc.fn)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if now, _ := os.ReadFile(path); !bytes.Equal(now, orig) {
				t.Error("a refused edit wrote to the file")
			}
		})
	}
}

func TestEditInPlaceNoChangeWritesNothing(t *testing.T) {
	path := editFixture(t)
	orig, _ := os.ReadFile(path)
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) {}); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, orig) {
		t.Error("an empty edit wrote to the file")
	}
}

func TestEditInPlaceRefusesFragmented(t *testing.T) {
	src := buildMKV(t, synthFixtureTracks(), synthFixtureBlocks(0))
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir); err != nil {
		t.Fatal(err)
	}
	init, err := os.ReadFile(filepath.Join(dir, "init.mp4"))
	if err != nil {
		t.Skipf("no single init segment to edit: %v", err)
	}
	path := filepath.Join(dir, "frag.mp4")
	if err := os.WriteFile(path, init, 0o644); err != nil {
		t.Fatal(err)
	}
	err = EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "x" })
	if err == nil || !strings.Contains(err.Error(), "fragmented MP4") {
		t.Fatalf("err = %v", err)
	}
}

// withForeignTrakFirst rewrites a moov-at-tail MP4 so a timecode trak, which the probe drops, precedes the media traks.
func withForeignTrakFirst(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	off, plen, err := findMoov(f, int64(len(data)))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if off+plen != int64(len(data)) {
		t.Fatal("fixture moov is not at the tail")
	}
	boxes, err := iterBoxes(data[off : off+plen])
	if err != nil {
		t.Fatal(err)
	}
	var foreign []byte
	for _, b := range boxes {
		if b.typ == "trak" {
			foreign = bytes.Replace(slicesClone(b.payload), []byte("vide"), []byte("tmcd"), 1)
			break
		}
	}
	var w bw
	for _, b := range boxes {
		if b.typ == "trak" && foreign != nil {
			w.bytes(box("trak", foreign))
			foreign = nil
		}
		w.bytes(box(b.typ, b.payload))
	}
	hdr := int64(8)
	if string(data[off-12:off-8]) == "moov" {
		hdr = 16
	}
	out := append(slicesClone(data[:off-hdr]), box("moov", w.b)...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func slicesClone(b []byte) []byte { return append([]byte(nil), b...) }

// TestEditAndRetimeTargetProbeTrackIDs: track numbers are the probe's, so a dropped trak ahead of the media traks shifts nothing.
func TestEditAndRetimeTargetProbeTrackIDs(t *testing.T) {
	path := editShiftFixture(t, false)
	withForeignTrakFirst(t, path)
	c := mustMeta(t, path)
	if len(c.Tracks) != 2 || c.Tracks[1].Type != mkv.AudioTrack {
		t.Fatalf("fixture probe = %d tracks", len(c.Tracks))
	}
	if err := RetimeTracks(context.Background(), path, map[uint64]int64{2: 100_000_000}); err != nil {
		t.Fatal(err)
	}
	if got := measuredAudioDelayMs(t, path); got != 100 {
		t.Errorf("audio delay after retiming track 2 = %d ms, want 100", got)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Tracks[1].Name = "Audio" }); err != nil {
		t.Fatal(err)
	}
	c = mustMeta(t, path)
	if c.Tracks[1].Name != "Audio" || c.Tracks[0].Name != "" {
		t.Errorf("names = %q, %q", c.Tracks[0].Name, c.Tracks[1].Name)
	}
}

// TestEditInPlaceQuickTimeTitle: a MOV keeps its title as a QuickTime udta text atom; an edit replaces it rather than adding a second one.
func TestEditInPlaceQuickTimeTitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.mov")
	data, err := os.ReadFile(filepath.Join("..", "internal", "testdata", "bframes_aac.mov"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, path).Info.Title; got != "Real" {
		t.Fatalf("title read from the QuickTime atom = %q", got)
	}
	if err := EditInPlace(context.Background(), path, func(c *mkv.Container) { c.Info.Title = "New" }); err != nil {
		t.Fatal(err)
	}
	if got := mustMeta(t, path).Info.Title; got != "New" {
		t.Errorf("title = %q", got)
	}
	for _, b := range moovBoxesOf(t, path) {
		if b.typ != "udta" {
			continue
		}
		ub, _ := iterBoxes(b.payload)
		if hasMemBox(ub, "\xa9nam") {
			t.Error("the old QuickTime title atom is still there")
		}
	}
}
