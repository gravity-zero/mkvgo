package matroska

import (
	"context"
	"os"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv/subtitle"
)

func TestParseSRTTimestamp(t *testing.T) {
	for _, tt := range []struct {
		s    string
		want int64
	}{
		{"00:00:00,000", 0},
		{"00:01:23,456", 83456},
		{"01:00:00,000", 3600000},
		{"02:30:45,123", 9045123},
	} {
		got, ok := subtitle.ParseSRTTimestamp(tt.s)
		if !ok {
			t.Errorf("ParseSRTTimestamp(%q) failed", tt.s)
			continue
		}
		assertEqual(t, got, tt.want, tt.s)
	}
}

func TestParseSRTTimestampInvalid(t *testing.T) {
	for _, s := range []string{"", "not a time", "00:00:00", "12:34"} {
		if _, ok := subtitle.ParseSRTTimestamp(s); ok {
			t.Errorf("ParseSRTTimestamp(%q) should fail", s)
		}
	}
}

func TestParseSRT(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.srt"
	assertNoErr(t, os.WriteFile(path, []byte(`1
00:00:01,000 --> 00:00:03,500
Hello World

2
00:00:05,000 --> 00:00:08,000
Second line
With a second row

3
00:01:00,000 --> 00:01:05,000
<i>Italic text</i>
`), 0644))

	entries, err := ParseSRT(path)
	assertNoErr(t, err)
	assertEqual(t, len(entries), 3, "entries count")
	assertEqual(t, entries[0].StartMs, int64(1000), "entry[0].start")
	assertEqual(t, entries[0].EndMs, int64(3500), "entry[0].end")
	assertEqual(t, entries[0].Text, "Hello World", "entry[0].text")
	assertEqual(t, entries[1].Text, "Second line\nWith a second row", "entry[1].text (multiline)")
	assertEqual(t, entries[2].Text, "<i>Italic text</i>", "entry[2].text (HTML tags)")
}

func TestParseSRTEmpty(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/empty.srt"
	assertNoErr(t, os.WriteFile(path, []byte(""), 0644))
	entries, err := ParseSRT(path)
	assertNoErr(t, err)
	assertEqual(t, len(entries), 0, "entries")
}

func TestParseSRTWithBOM(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bom.srt"
	bom := []byte{0xEF, 0xBB, 0xBF}
	content := append(bom, []byte("1\n00:00:00,000 --> 00:00:01,000\nWith BOM\n")...)
	assertNoErr(t, os.WriteFile(path, content, 0644))
	entries, err := ParseSRT(path)
	assertNoErr(t, err)
	assertEqual(t, len(entries), 1, "entries")
	assertEqual(t, entries[0].Text, "With BOM", "text")
}

func TestParseSRTMalformed(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bad.srt"
	assertNoErr(t, os.WriteFile(path, []byte("not a real srt\njust some text\n"), 0644))
	entries, err := ParseSRT(path)
	assertNoErr(t, err)
	assertEqual(t, len(entries), 0, "entries (malformed)")
}

func TestMergeSubtitle(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	srtPath := dir + "/test.srt"
	assertNoErr(t, os.WriteFile(srtPath, []byte("1\n00:00:00,100 --> 00:00:01,000\nHello World\n\n2\n00:00:01,500 --> 00:00:02,500\nSecond subtitle\n"), 0644))

	outPath := dir + "/with_subs.mkv"
	assertNoErr(t, MergeSubtitle(context.Background(), fixturePath, srtPath, outPath, "eng", "Test Subs"))

	c, err := Open(context.Background(), outPath)
	assertNoErr(t, err)
	assertEqual(t, len(c.Tracks), 3, "tracks count")
	assertEqual(t, c.Tracks[2].Type, SubtitleTrack, "new track type")
	assertEqual(t, c.Tracks[2].Language, "eng", "new track lang")

	counts := countBlocks(t, outPath, c.Info.TimecodeScale)
	if counts[3] != 2 {
		t.Errorf("subtitle blocks: got %d, want 2", counts[3])
	}
}

func TestMergeSubtitleEmpty(t *testing.T) {
	requireFixture(t)
	dir := t.TempDir()
	srtPath := dir + "/empty.srt"
	assertNoErr(t, os.WriteFile(srtPath, []byte(""), 0644))
	err := MergeSubtitle(context.Background(), fixturePath, srtPath, dir+"/out.mkv", "eng", "")
	if err == nil {
		t.Fatal("expected error for empty SRT")
	}
}
