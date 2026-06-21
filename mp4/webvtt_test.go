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

// remuxWith runs RemuxToMP4 with the given options and returns the output bytes
// plus its top-level boxes.
func remuxWith(t *testing.T, srcPath string, o Options) ([]byte, []tbox) {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "out.mp4")
	if err := RemuxToMP4(context.Background(), srcPath, dst, o); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return data, walkBoxes(t, data, 0)
}

// textTrack returns the subtitle track in the parsed moov (tx3g or wvtt sample
// entry), or nil. It is identified by its sample entry, not its handler, so it
// is independent of the handler choice and excludes the QuickTime chapter track
// (whose entry is 'text').
func textTrack(traks []parsedTrack) *parsedTrack {
	for i := range traks {
		if traks[i].sampleEntry == "tx3g" || traks[i].sampleEntry == "wvtt" {
			return &traks[i]
		}
	}
	return nil
}

func videoAnd(sub mkv.Track) []mkv.Track {
	return []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC,
			Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)},
		sub,
	}
}

// TestWebVTTNativeCarriage checks a WebVTT track is carried losslessly as native
// wvtt (not flattened): the sample entry is wvtt and the cue payload keeps its
// inline markup verbatim.
func TestWebVTTNativeCarriage(t *testing.T) {
	tracks := videoAnd(mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "webvtt", CodecPrivate: []byte("WEBVTT")})
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte("Hello <b>world</b>")},
		{track: 1, pts: 40, key: false, data: []byte{2}},
		{track: 2, pts: 1000, key: true, data: []byte("Second")},
	}
	src := buildMKV(t, tracks, blocks)
	data, boxes := remuxWith(t, src, Options{NativeWebVTT: true})

	text := textTrack(moovTraks(t, boxes))
	if text == nil {
		t.Fatal("no subtitle track in output")
	}
	if text.sampleEntry != "wvtt" {
		t.Fatalf("sample entry = %q, want wvtt", text.sampleEntry)
	}

	var joined []byte
	for _, s := range extractSamples(t, data, *text) {
		if txt, ok := decodeWVTT(s); ok {
			joined = append(append(joined, txt...), '|')
		}
	}
	if !bytes.Contains(joined, []byte("Hello <b>world</b>")) || !bytes.Contains(joined, []byte("Second")) {
		t.Errorf("wvtt payload not preserved verbatim: %q", joined)
	}
}

// TestWebVTTDefaultsToTx3g checks that, without NativeWebVTT, a WebVTT track is
// carried as tx3g (universally readable) rather than native wvtt.
func TestWebVTTDefaultsToTx3g(t *testing.T) {
	tracks := videoAnd(mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "webvtt", CodecPrivate: []byte("WEBVTT")})
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte("Hello <b>world</b>")},
	}
	src := buildMKV(t, tracks, blocks)
	data, boxes := remux(t, src) // default options

	text := textTrack(moovTraks(t, boxes))
	if text == nil {
		t.Fatal("no subtitle track")
	}
	if text.sampleEntry != "tx3g" {
		t.Fatalf("default WebVTT sample entry = %q, want tx3g", text.sampleEntry)
	}
	var joined []byte
	for _, s := range extractSamples(t, data, *text) {
		if txt, ok := decodeTx3g(s); ok {
			joined = append(joined, txt...)
		}
	}
	if !bytes.Contains(joined, []byte("Hello world")) || bytes.Contains(joined, []byte("<b>")) {
		t.Errorf("default WebVTT text = %q, want markup stripped tx3g", joined)
	}
}

// TestWebVTTRoundTrip checks WebVTT survives MKV → MP4 (wvtt) → MKV with its
// codec and text intact.
func TestWebVTTRoundTrip(t *testing.T) {
	tracks := videoAnd(mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "webvtt", CodecPrivate: []byte("WEBVTT")})
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 100, key: true, data: []byte("Bonjour <i>le monde</i>")},
	}
	src := buildMKV(t, tracks, blocks)

	mp4Path := filepath.Join(t.TempDir(), "mid.mp4")
	if err := RemuxToMP4(context.Background(), src, mp4Path, Options{NativeWebVTT: true}); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	outMKV := filepath.Join(t.TempDir(), "out.mkv")
	if err := RemuxFromMP4(context.Background(), mp4Path, outMKV); err != nil {
		t.Fatalf("RemuxFromMP4: %v", err)
	}

	c, blks := readMKV(t, outMKV)
	var subs *mkv.Track
	for i := range c.Tracks {
		if c.Tracks[i].Type == mkv.SubtitleTrack {
			subs = &c.Tracks[i]
		}
	}
	if subs == nil {
		t.Fatal("subtitle track lost in round trip")
	}
	if subs.Codec != "webvtt" {
		t.Errorf("codec = %q, want webvtt", subs.Codec)
	}
	if !strings.HasPrefix(string(subs.CodecPrivate), "WEBVTT") {
		t.Errorf("WebVTT header lost: CodecPrivate = %q", subs.CodecPrivate)
	}
	var found bool
	for _, b := range blks {
		if b.TrackNumber == subs.ID && bytes.Contains(b.Data, []byte("Bonjour <i>le monde</i>")) {
			found = true
		}
	}
	if !found {
		t.Errorf("cue text (with markup) not round-tripped")
	}
}

// TestASSDroppedWithoutFlatten checks an ASS track is dropped (not failed, not
// silently lost) when FlattenStyledSubs is off, with a reason pointing the user
// at the flag.
func TestASSDroppedWithoutFlatten(t *testing.T) {
	tracks := videoAnd(mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "ass"})
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte("0,0,Default,,0,0,0,,Hi")},
	}
	src := buildMKV(t, tracks, blocks)

	var dropped []DroppedTrack
	dst := filepath.Join(t.TempDir(), "out.mp4")
	err := RemuxToMP4(context.Background(), src, dst, Options{
		OnDrop: func(d DroppedTrack) { dropped = append(dropped, d) },
	})
	if err != nil {
		t.Fatalf("ASS must not fail the remux: %v", err)
	}
	if len(dropped) != 1 || dropped[0].Codec != "ass" {
		t.Fatalf("want exactly the ass track dropped, got %v", dropped)
	}
	if !strings.Contains(dropped[0].Reason, "FlattenStyledSubs") {
		t.Errorf("drop reason should mention the flag, got %q", dropped[0].Reason)
	}

	outData, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if textTrack(moovTraks(t, walkBoxes(t, outData, 0))) != nil {
		t.Error("dropped ASS track should leave no subtitle track")
	}
}

// TestASSFlattenToTx3g checks that with FlattenStyledSubs the ASS framing and
// override tags are stripped and the cue is carried as tx3g.
func TestASSFlattenToTx3g(t *testing.T) {
	tracks := videoAnd(mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "ass"})
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte(`0,0,Default,Bob,0,0,0,,{\i1}Hello{\i0}\Nworld`)},
	}
	src := buildMKV(t, tracks, blocks)
	data, boxes := remuxWith(t, src, Options{FlattenStyledSubs: true})

	text := textTrack(moovTraks(t, boxes))
	if text == nil {
		t.Fatal("flattened ASS produced no subtitle track")
	}
	if text.sampleEntry != "tx3g" {
		t.Fatalf("sample entry = %q, want tx3g", text.sampleEntry)
	}
	var joined []byte
	for _, s := range extractSamples(t, data, *text) {
		if txt, ok := decodeTx3g(s); ok {
			joined = append(joined, txt...)
		}
	}
	if !bytes.Contains(joined, []byte("Hello\nworld")) {
		t.Errorf("flattened text = %q, want Hello<newline>world", joined)
	}
	for _, bad := range []string{"{", "Default", `\N`} {
		if bytes.Contains(joined, []byte(bad)) {
			t.Errorf("flattened text should not contain %q: %q", bad, joined)
		}
	}
}

// TestWebVTTFlattenToTx3g checks the flag forces WebVTT to tx3g (instead of its
// native wvtt) and strips inline markup.
func TestWebVTTFlattenToTx3g(t *testing.T) {
	tracks := videoAnd(mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Codec: "webvtt", CodecPrivate: []byte("WEBVTT")})
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte("Hello <b>world</b>")},
	}
	src := buildMKV(t, tracks, blocks)
	data, boxes := remuxWith(t, src, Options{FlattenStyledSubs: true})

	text := textTrack(moovTraks(t, boxes))
	if text == nil {
		t.Fatal("no subtitle track")
	}
	if text.sampleEntry != "tx3g" {
		t.Fatalf("sample entry = %q, want tx3g (flattened)", text.sampleEntry)
	}
	var joined []byte
	for _, s := range extractSamples(t, data, *text) {
		if txt, ok := decodeTx3g(s); ok {
			joined = append(joined, txt...)
		}
	}
	if !bytes.Contains(joined, []byte("Hello world")) || bytes.Contains(joined, []byte("<b>")) {
		t.Errorf("flattened WebVTT = %q, want markup stripped", joined)
	}
}

func TestFlattenASS(t *testing.T) {
	cases := map[string]string{
		`5,0,Default,Bob,0,0,0,,{\i1}Hi{\i0}\Nthere\hnow`: "Hi\nthere now",
		`0,0,Default,,0,0,0,,plain`:                       "plain",
		`0,0,Default,,0,0,0,,{\an8}top`:                   "top",
		`malformed without enough fields`:                 "malformed without enough fields",
	}
	for in, want := range cases {
		if got := string(flattenASS([]byte(in))); got != want {
			t.Errorf("flattenASS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWVTTCueEncodeDecode(t *testing.T) {
	s := encodeWVTTCue([]byte("Hello"))
	if txt, ok := decodeWVTT(s); !ok || string(txt) != "Hello" {
		t.Errorf("decodeWVTT(encodeWVTTCue) = %q,%v, want Hello,true", txt, ok)
	}
	if _, ok := decodeWVTT(wvttEmptyCue); ok {
		t.Error("an empty (vtte) sample should decode as no cue")
	}
}
