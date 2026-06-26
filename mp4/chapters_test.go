package mp4

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

func TestBuildChpl(t *testing.T) {
	chapters := []mkv.Chapter{
		{StartMs: 0, Title: "Intro"},
		{StartMs: 49000, Title: "Générique"}, // multibyte title
	}
	b := buildChpl(chapters)
	if string(b[4:8]) != "chpl" {
		t.Fatalf("not a chpl box: %q", b[4:8])
	}
	p := b[8:]
	if binary.BigEndian.Uint32(p[:4]) != 0x01000000 {
		t.Errorf("version/flags = %#x", binary.BigEndian.Uint32(p[:4]))
	}
	if p[8] != 2 {
		t.Fatalf("chapter count = %d, want 2", p[8])
	}
	// entry 0: start(8)=0, len(1)=5, "Intro"
	off := 9
	if binary.BigEndian.Uint64(p[off:off+8]) != 0 {
		t.Errorf("chapter 0 start != 0")
	}
	if p[off+8] != 5 || string(p[off+9:off+14]) != "Intro" {
		t.Errorf("chapter 0 title wrong: %q", p[off+9:off+14])
	}
	// entry 1: start = 49000ms in 100ns units = 490_000_000
	off2 := off + 9 + 5
	if got := binary.BigEndian.Uint64(p[off2 : off2+8]); got != 490_000_000 {
		t.Errorf("chapter 1 start = %d, want 490000000", got)
	}
	title := []byte("Générique")
	if int(p[off2+8]) != len(title) || !bytes.Equal(p[off2+9:off2+9+len(title)], title) {
		t.Errorf("chapter 1 title wrong")
	}
}

func TestParseChplRoundTrip(t *testing.T) {
	in := []mkv.Chapter{{StartMs: 0, Title: "A"}, {StartMs: 1000, Title: "Bê"}, {StartMs: 5000, Title: "C"}}
	out := parseChpl(buildChpl(in)[8:]) // strip box header → payload
	if len(out) != 3 {
		t.Fatalf("got %d chapters, want 3", len(out))
	}
	wantStart := []int64{0, 1000, 5000}
	wantEnd := []int64{1000, 5000, 0} // derived from next start; last open
	for i := range in {
		if out[i].StartMs != wantStart[i] {
			t.Errorf("chapter %d start = %d, want %d", i, out[i].StartMs, wantStart[i])
		}
		if out[i].EndMs != wantEnd[i] {
			t.Errorf("chapter %d end = %d, want %d", i, out[i].EndMs, wantEnd[i])
		}
		if out[i].Title != in[i].Title {
			t.Errorf("chapter %d title = %q, want %q", i, out[i].Title, in[i].Title)
		}
		if out[i].ID == 0 {
			t.Errorf("chapter %d must have a non-zero ChapterUID", i)
		}
	}
}

func TestEmitChapterSamplesDurations(t *testing.T) {
	tr := outTrack{chapterList: []mkv.Chapter{{StartMs: 0, Title: "A"}, {StartMs: 1000, Title: "BB"}, {StartMs: 2000, Title: "C"}}}
	var buf bytes.Buffer
	if err := tr.emitChapterSamples(&countWriter{w: &buf}); err != nil {
		t.Fatal(err)
	}
	all := emittedSampleBytes(buf.Bytes(), &tr)
	wantDur := []int64{1000, 1000, defaultCueDurMs} // gaps, last = default
	off := 0
	for i, s := range tr.samples.samples {
		if s.dur != wantDur[i] {
			t.Errorf("chapter sample %d dur = %d, want %d", i, s.dur, wantDur[i])
		}
		n := int(all[off])<<8 | int(all[off+1])
		title := string(all[off+2 : off+2+n])
		if title != tr.chapterList[i].Title {
			t.Errorf("chapter sample %d title = %q, want %q", i, title, tr.chapterList[i].Title)
		}
		if !bytes.Contains(all[off:off+int(s.size)], []byte("encd")) {
			t.Errorf("chapter sample %d missing encd atom", i)
		}
		off += int(s.size)
	}
}

func TestBuildChplBoxEmpty(t *testing.T) {
	if buildChplBox(nil) != nil {
		t.Error("no chapters → nil chpl")
	}
	// chapters without titles are skipped → nil.
	if buildChplBox([]mkv.Chapter{{StartMs: 0}}) != nil {
		t.Error("untitled chapters → nil chpl")
	}
}

func TestRoundTripChapters(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 1, pts: 40, key: false, data: []byte{2}},
	}
	chapters := []mkv.Chapter{
		{StartMs: 0, Title: "Alpha"},
		{StartMs: 49000, Title: "Bravo"},
		{StartMs: 109000, Title: "Charlie"},
	}
	srcMKV := buildMKVWithChapters(t, tracks, blocks, chapters)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), srcMKV, mp4Path); err != nil {
		t.Fatal(err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatal(err)
	}

	c, _ := readMKV(t, outMKV)
	if len(c.Chapters) != 3 {
		t.Fatalf("got %d chapters, want 3", len(c.Chapters))
	}
	for i, want := range chapters {
		if c.Chapters[i].Title != want.Title {
			t.Errorf("chapter %d title = %q, want %q", i, c.Chapters[i].Title, want.Title)
		}
		if c.Chapters[i].StartMs != want.StartMs {
			t.Errorf("chapter %d start = %d, want %d", i, c.Chapters[i].StartMs, want.StartMs)
		}
	}
}

func TestRoundTripLastChapterEnd(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 1, pts: 5000, key: true, data: []byte{2}},
	}
	chapters := []mkv.Chapter{
		{StartMs: 0, EndMs: 2000, Title: "Intro"},
		{StartMs: 2000, EndMs: 5000, Title: "Corps"},
	}
	src := buildMKVWithChapters(t, tracks, blocks, chapters)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), src, mp4Path); err != nil {
		t.Fatal(err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatal(err)
	}
	c, _ := readMKV(t, outMKV)
	if len(c.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	// The Nero chpl box has no end times; the last chapter must close at the movie
	// end, not collapse to 0 (the bug).
	if last := c.Chapters[len(c.Chapters)-1]; last.EndMs < 4500 {
		t.Errorf("last chapter end = %d, want ~5000 (movie end)", last.EndMs)
	}
}

func TestChapterTrackForApple(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, Channels: u8p(2), SampleRate: f64p(48000)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{2}},
		{track: 1, pts: 40, key: false, data: []byte{3}},
	}
	chapters := []mkv.Chapter{{StartMs: 0, Title: "One"}, {StartMs: 1000, Title: "Two"}}
	src := buildMKVWithChapters(t, tracks, blocks, chapters)
	_, boxes := remux(t, src)

	moov := mustBox(t, boxes, "moov")
	var traks []tbox
	for _, b := range walkBoxes(t, moov.payload, moov.dataOff) {
		if b.typ == "trak" {
			traks = append(traks, b)
		}
	}
	if len(traks) != 3 {
		t.Fatalf("got %d traks (video, audio, chapter expected), want 3", len(traks))
	}

	// Find the chapter track (handler 'text', samples are titles) and the chap
	// references from the media tracks.
	var chapterTrackID uint32
	refs := map[string]uint32{}
	for _, trak := range traks {
		tb := walkBoxes(t, trak.payload, trak.dataOff)
		mdia := mustBox(t, tb, "mdia")
		hdlr := mustBox(t, walkBoxes(t, mdia.payload, mdia.dataOff), "hdlr")
		htype := string(hdlr.payload[8:12])
		if tref, ok := findBox(tb, "tref"); ok {
			chap := mustBox(t, walkBoxes(t, tref.payload, tref.dataOff), "chap")
			refs[htype] = uint32(chap.payload[0])<<24 | uint32(chap.payload[1])<<16 | uint32(chap.payload[2])<<8 | uint32(chap.payload[3])
		}
	}
	// The chapter track is the one with handler 'text' and gmhd; its mp4 ID is 3.
	chapterTrackID = 3
	if refs["vide"] != chapterTrackID || refs["soun"] != chapterTrackID {
		t.Errorf("media tracks should reference chapter track %d via tref/chap, got %v", chapterTrackID, refs)
	}

	// chpl must also be present (desktop players).
	udta := mustBox(t, walkBoxes(t, moov.payload, moov.dataOff), "udta")
	if _, ok := findBox(walkBoxes(t, udta.payload, udta.dataOff), "chpl"); !ok {
		t.Error("chpl box missing (desktop chapter support)")
	}
}

func TestRemuxToMP4WritesChapters(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
	}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 1, pts: 40, key: false, data: []byte{2}},
	}
	chapters := []mkv.Chapter{{StartMs: 0, Title: "Start"}, {StartMs: 1000, Title: "Middle"}}
	src := buildMKVWithChapters(t, tracks, blocks, chapters)

	_, boxes := remux(t, src)
	moov := mustBox(t, boxes, "moov")
	udta, ok := findBox(walkBoxes(t, moov.payload, moov.dataOff), "udta")
	if !ok {
		t.Fatal("no udta in moov")
	}
	chpl, ok := findBox(walkBoxes(t, udta.payload, udta.dataOff), "chpl")
	if !ok {
		t.Fatal("no chpl in udta")
	}
	if chpl.payload[8] != 2 {
		t.Errorf("chapter count = %d, want 2", chpl.payload[8])
	}
	if !bytes.Contains(chpl.payload, []byte("Start")) || !bytes.Contains(chpl.payload, []byte("Middle")) {
		t.Errorf("chapter titles missing from chpl")
	}
}
