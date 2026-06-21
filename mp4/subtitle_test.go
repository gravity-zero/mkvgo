package mp4

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// emittedSampleBytes returns every sample byte a track produced, in order:
// flushed chunks land in written, the rest stays in pending.
func emittedSampleBytes(written []byte, tr *outTrack) []byte {
	all := append([]byte{}, written...)
	return append(all, tr.pending...)
}

// textSamples decodes a track's text samples into their durations and titles.
func textSamples(written []byte, tr *outTrack) (durations []int64, texts []string) {
	all := emittedSampleBytes(written, tr)
	off := 0
	for _, s := range tr.samples.samples {
		n := int(all[off])<<8 | int(all[off+1])
		texts = append(texts, string(all[off+2:off+2+n]))
		durations = append(durations, s.dur)
		off += int(s.size)
	}
	return
}

func TestTextLookaheadLeadInAndDefault(t *testing.T) {
	// Two cues without BlockDuration: a lead-in empty sample fills [0,1000), the
	// first cue runs until the next (gap-derived 2000ms), the last uses the
	// default duration.
	var buf bytes.Buffer
	tr := outTrack{spec: codecTable["srt"]}
	cw := &countWriter{w: &buf}
	if err := tr.addTextBlock(cw, mkv.Block{Timecode: 1000, Data: []byte("First")}); err != nil {
		t.Fatal(err)
	}
	if err := tr.addTextBlock(cw, mkv.Block{Timecode: 3000, Data: []byte("Second")}); err != nil {
		t.Fatal(err)
	}
	if err := tr.flushPendingCue(cw, -1); err != nil {
		t.Fatal(err)
	}
	durs, texts := textSamples(buf.Bytes(), &tr)
	if !reflect.DeepEqual(durs, []int64{1000, 2000, defaultCueDurMs}) {
		t.Errorf("durations = %v, want [1000 2000 %d]", durs, defaultCueDurMs)
	}
	if !reflect.DeepEqual(texts, []string{"", "First", "Second"}) {
		t.Errorf("texts = %q", texts)
	}
}

func TestTextLookaheadExplicitDurationLeavesGap(t *testing.T) {
	// Cues with explicit BlockDuration shorter than the gap leave an empty sample
	// between them.
	var buf bytes.Buffer
	tr := outTrack{spec: codecTable["srt"]}
	cw := &countWriter{w: &buf}
	_ = tr.addTextBlock(cw, mkv.Block{Timecode: 0, Duration: 500, Data: []byte("A")})
	_ = tr.addTextBlock(cw, mkv.Block{Timecode: 2000, Duration: 500, Data: []byte("B")})
	if err := tr.flushPendingCue(cw, -1); err != nil {
		t.Fatal(err)
	}
	durs, texts := textSamples(buf.Bytes(), &tr)
	if !reflect.DeepEqual(durs, []int64{500, 1500, 500}) {
		t.Errorf("durations = %v, want [500 1500 500]", durs)
	}
	if !reflect.DeepEqual(texts, []string{"A", "", "B"}) {
		t.Errorf("texts = %q", texts)
	}
}

func TestStripMarkup(t *testing.T) {
	cases := map[string]string{
		"plain text":                 "plain text",
		"<i>italic</i>":              "italic",
		"<font color=red>hi</font>!": "hi!",
		"a < b":                      "a ", // an unmatched '<' opens a tag to end-of-line
	}
	for in, want := range cases {
		if got := stripMarkup(in); got != want {
			t.Errorf("stripMarkup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeCue(t *testing.T) {
	got := encodeCue([]byte("<i>Hello</i>"))
	if binary.BigEndian.Uint16(got[:2]) != 5 {
		t.Fatalf("length prefix = %d, want 5", binary.BigEndian.Uint16(got[:2]))
	}
	if string(got[2:]) != "Hello" {
		t.Errorf("text = %q, want Hello", got[2:])
	}
}

func TestSRTEntryStructure(t *testing.T) {
	entry, err := srtEntry(&mkv.Track{ID: 1, Codec: "srt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry[4:8]) != "tx3g" {
		t.Fatalf("entry type = %q, want tx3g", entry[4:8])
	}
	if !bytes.Contains(entry, []byte("ftab")) || !bytes.Contains(entry, []byte("Serif")) {
		t.Errorf("tx3g entry must contain an ftab font table")
	}
}

func TestRemuxToMP4CarriesSRT(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(640), Height: u32p(480), FrameRate: f64p(25)},
		{ID: 2, Type: mkv.SubtitleTrack, Codec: "srt"},
	}
	// Two cues at 1000ms and 3000ms (no BlockDuration; durations come from the
	// one-cue lookahead). Expect a lead-in empty sample, then the cues.
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 1000, key: true, data: []byte("First")},
		{track: 1, pts: 40, key: false, data: []byte{2}},
		{track: 2, pts: 3000, key: true, data: []byte("<i>Second</i>")},
	}
	src := buildMKV(t, tracks, blocks)
	data, boxes := remux(t, src)

	traks := moovTraks(t, boxes)
	var text *parsedTrack
	for i := range traks {
		if traks[i].handler == "text" {
			text = &traks[i]
		}
	}
	if text == nil {
		t.Fatal("no text track in output")
	}
	if text.sampleEntry != "tx3g" {
		t.Errorf("text entry = %q, want tx3g", text.sampleEntry)
	}

	// Reconstruct the cue samples and confirm the text survived (markup stripped).
	samples := extractSamples(t, data, *text)
	var texts []string
	for _, s := range samples {
		if len(s) < 2 {
			t.Fatalf("text sample too short")
		}
		n := int(binary.BigEndian.Uint16(s[:2]))
		if n > 0 {
			texts = append(texts, string(s[2:2+n]))
		}
	}
	joined := bytes.Join(bytesOf(texts), []byte("|"))
	if !bytes.Contains(joined, []byte("First")) || !bytes.Contains(joined, []byte("Second")) {
		t.Errorf("cue texts = %q, want to contain First and Second", joined)
	}
	if bytes.Contains(joined, []byte("<i>")) {
		t.Errorf("markup should have been stripped: %q", joined)
	}
	// Total text-track duration should reach the end of the last cue (~4000ms).
	var total int64
	for _, d := range text.durations {
		total += int64(d)
	}
	if total < 4000 {
		t.Errorf("text track total duration = %d, want >= 4000", total)
	}
}

func bytesOf(ss []string) [][]byte {
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out
}
