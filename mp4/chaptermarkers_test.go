package mp4

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// chapterFixture builds a ~12s video+audio source (25fps video, keyframe
// every 1s; 50fps audio) with the given chapters (nil for none).
func chapterFixture(t testing.TB, chapters []mkv.Chapter) string {
	t.Helper()
	w, h := uint32(320), uint32(240)
	sr, ch := 44100.0, uint8(2)
	var gblocks []genBlock
	for i := 0; i < 300; i++ { // video, 40ms frames, keyframe every 1s -> 12s
		gblocks = append(gblocks, genBlock{track: 1, pts: int64(i) * 40, key: i%25 == 0,
			data: []byte{0x00, 0x00, 0x00, 0x01, 0x65, byte(i)}})
	}
	for i := 0; i < 600; i++ { // audio, 20ms frames
		gblocks = append(gblocks, genBlock{track: 2, pts: int64(i) * 20, key: true,
			data: []byte{0xAA, byte(i)}})
	}
	sortGenBlocks(gblocks)
	return buildMKVWithChapters(t,
		[]mkv.Track{
			{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: &w, Height: &h},
			{ID: 2, Type: mkv.AudioTrack, Codec: "aac", CodecPrivate: fakeASC, SampleRate: &sr, Channels: &ch},
		},
		gblocks, chapters)
}

// testChapters is a fixed 3-chapter list whose last title exercises escaping
// (a literal double quote and an ampersand/angle bracket) in both HLS's
// quoted attribute and DASH's XML text.
var testChapters = []mkv.Chapter{
	{StartMs: 0, Title: "Intro"},
	{StartMs: 4000, Title: `Part "Two"`},
	{StartMs: 9000, Title: "End & <Done>"},
}

// countOccurrences counts non-overlapping instances of sub in s.
func countOccurrences(s, sub string) int {
	return strings.Count(s, sub)
}

// extractEventStream returns the <EventStream>...</EventStream> block of a
// DASH manifest, or "" if absent.
func extractEventStream(mpd string) string {
	start := strings.Index(mpd, "<EventStream ")
	if start < 0 {
		return ""
	}
	end := strings.Index(mpd[start:], "</EventStream>")
	if end < 0 {
		return ""
	}
	return mpd[start : start+end+len("</EventStream>")]
}

// TestChapterMarkersOffByDefault: a source WITH chapters, packaged with the
// option left off (its default), carries no DATERANGE/Event markers, and its
// playlist/manifest are byte-identical to packaging the same source with its
// Chapters stripped entirely - the option gate gets Chapters completely out
// of the output path when it is not set.
func TestChapterMarkersOffByDefault(t *testing.T) {
	withChapters := chapterFixture(t, testChapters)
	withoutChapters := chapterFixture(t, nil)

	dir1 := t.TempDir()
	if err := RemuxToHLS(context.Background(), withChapters, dir1, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	if err := RemuxToHLS(context.Background(), withoutChapters, dir2, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"playlist.m3u8", "manifest.mpd", "init.mp4"} {
		a, err := os.ReadFile(filepath.Join(dir1, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dir2, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s: option-off output with chapters differs from a chapterless source (%d vs %d bytes)",
				name, len(a), len(b))
		}
		if bytes.Contains(a, []byte("EXT-X-DATERANGE")) || bytes.Contains(a, []byte("EventStream")) {
			t.Errorf("%s: option off must carry no chapter markers:\n%s", name, a)
		}
	}
}

// TestChapterMarkersHLS: Options.ChapterMarkers emits one EXT-X-DATERANGE per
// chapter, in playlist order, with the right DURATION and an escaped title.
func TestChapterMarkersHLS(t *testing.T) {
	src := chapterFixture(t, testChapters)
	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, Options{SegmentMs: 2000, ChapterMarkers: true}); err != nil {
		t.Fatal(err)
	}
	pl, err := os.ReadFile(filepath.Join(dir, "playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(pl)
	if n := countOccurrences(s, "#EXT-X-DATERANGE:"); n != len(testChapters) {
		t.Fatalf("playlist has %d EXT-X-DATERANGE lines, want %d:\n%s", n, len(testChapters), s)
	}
	wantLines := []string{
		`#EXT-X-DATERANGE:ID="chapter-1",START-DATE="1970-01-01T00:00:00.000Z",DURATION=4.000,X-CHAPTER-TITLE="Intro"`,
		`#EXT-X-DATERANGE:ID="chapter-2",START-DATE="1970-01-01T00:00:04.000Z",DURATION=5.000,X-CHAPTER-TITLE="Part \"Two\""`,
	}
	for _, want := range wantLines {
		if !strings.Contains(s, want) {
			t.Errorf("playlist missing %q:\n%s", want, s)
		}
	}
	// Playlist order: chapter 1 must appear before chapter 2, before chapter 3.
	i1 := strings.Index(s, `ID="chapter-1"`)
	i2 := strings.Index(s, `ID="chapter-2"`)
	i3 := strings.Index(s, `ID="chapter-3"`)
	if i1 < 0 || i2 < 0 || i3 < 0 || !(i1 < i2 && i2 < i3) {
		t.Errorf("chapters not in playlist order: %d, %d, %d", i1, i2, i3)
	}
	// The audio rendition must carry no chapter markers (video only).
	audioPl, err := os.ReadFile(filepath.Join(dir, "audio1.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(audioPl, []byte("EXT-X-DATERANGE")) {
		t.Errorf("audio rendition must carry no chapter markers:\n%s", audioPl)
	}

	mpd, err := os.ReadFile(filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	m := string(mpd)
	if n := countOccurrences(m, "<Event "); n != len(testChapters) {
		t.Fatalf("manifest has %d Event elements, want %d:\n%s", n, len(testChapters), m)
	}
	if !strings.Contains(m, `<EventStream schemeIdUri="urn:mkvgo:dash:chapter:2024" timescale="1000">`) {
		t.Errorf("manifest missing the chapter EventStream:\n%s", m)
	}
	for _, want := range []string{
		`<Event id="1" presentationTime="0" duration="4000">Intro</Event>`,
		`<Event id="2" presentationTime="4000" duration="5000">Part &#34;Two&#34;</Event>`,
		`<Event id="3" presentationTime="9000" duration="3000">End &amp; &lt;Done&gt;</Event>`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("manifest missing %q:\n%s", want, m)
		}
	}
}

// TestChapterMarkersPlanMatchesFullPass: PlanHLS's video media playlist
// (which carries the EXT-X-DATERANGE lines) and init segment are
// byte-identical to RemuxToHLS's, with Options.ChapterMarkers on - the same
// byte-parity invariant the rest of the on-demand plan holds to. The DASH
// manifest's chapter EventStream is compared the same way; the rest of
// manifest.mpd (like master.m3u8) already differs by an estimated BANDWIDTH
// between the full pass and the plan, orthogonal to chapters.
func TestChapterMarkersPlanMatchesFullPass(t *testing.T) {
	src := chapterFixture(t, testChapters)
	opts := Options{SegmentMs: 2000, ChapterMarkers: true}

	dir := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dir, opts); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHLS(context.Background(), src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"playlist.m3u8", "init.mp4"} {
		want, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var got []byte
		if name == "playlist.m3u8" {
			got = plan.MediaPlaylist()
		} else {
			got = plan.InitSegment()
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: plan differs from full pass (%d vs %d bytes)\nplan: %s\nfull: %s",
				name, len(got), len(want), truncated(got), truncated(want))
		}
	}
	fullMPD, err := os.ReadFile(filepath.Join(dir, "manifest.mpd"))
	if err != nil {
		t.Fatal(err)
	}
	planMPD, _, err := plan.Resource(context.Background(), "manifest.mpd")
	if err != nil {
		t.Fatal(err)
	}
	fullEvents := extractEventStream(string(fullMPD))
	planEvents := extractEventStream(string(planMPD))
	if fullEvents == "" || planEvents == "" {
		t.Fatalf("EventStream missing: full=%q plan=%q", fullEvents, planEvents)
	}
	if fullEvents != planEvents {
		t.Errorf("chapter EventStream differs between full pass and plan:\nfull: %s\nplan: %s", fullEvents, planEvents)
	}
	for n := 0; n < plan.NumSegments(); n++ {
		got, err := plan.Segment(context.Background(), n)
		if err != nil {
			t.Fatalf("Segment(%d): %v", n, err)
		}
		want, err := os.ReadFile(filepath.Join(dir, plan.SegmentName(n)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("segment %d differs from the full pass (%d vs %d bytes)", n, len(got), len(want))
		}
	}
}

// TestChapterMarkersSegmentsUnchanged: the media segments (.m4s) and the init
// segment are byte-identical whether the option is on or off - markers never
// touch segmentation, only the playlist/manifest text.
func TestChapterMarkersSegmentsUnchanged(t *testing.T) {
	src := chapterFixture(t, testChapters)

	dirOff := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dirOff, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	dirOn := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dirOn, Options{SegmentMs: 2000, ChapterMarkers: true}); err != nil {
		t.Fatal(err)
	}

	segsOff, _ := filepath.Glob(filepath.Join(dirOff, "seg*.m4s"))
	segsOn, _ := filepath.Glob(filepath.Join(dirOn, "seg*.m4s"))
	if len(segsOff) == 0 || len(segsOff) != len(segsOn) {
		t.Fatalf("segment count differs: off=%d on=%d", len(segsOff), len(segsOn))
	}
	for i := range segsOff {
		off, err := os.ReadFile(segsOff[i])
		if err != nil {
			t.Fatal(err)
		}
		on, err := os.ReadFile(segsOn[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(off, on) {
			t.Errorf("segment %d differs between option off/on (%d vs %d bytes)", i, len(off), len(on))
		}
	}
	for _, name := range []string{"init.mp4", "init_a1.mp4"} {
		off, err := os.ReadFile(filepath.Join(dirOff, name))
		if err != nil {
			t.Fatal(err)
		}
		on, err := os.ReadFile(filepath.Join(dirOn, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(off, on) {
			t.Errorf("%s differs between option off/on", name)
		}
	}
}

// TestChapterMarkersNoChapters: the option on, but the source carries no
// chapters, emits no markers and no error - output identical to the option
// off.
func TestChapterMarkersNoChapters(t *testing.T) {
	src := chapterFixture(t, nil)
	dirOff := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dirOff, Options{SegmentMs: 2000}); err != nil {
		t.Fatal(err)
	}
	dirOn := t.TempDir()
	if err := RemuxToHLS(context.Background(), src, dirOn, Options{SegmentMs: 2000, ChapterMarkers: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"playlist.m3u8", "manifest.mpd"} {
		off, err := os.ReadFile(filepath.Join(dirOff, name))
		if err != nil {
			t.Fatal(err)
		}
		on, err := os.ReadFile(filepath.Join(dirOn, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(off, on) {
			t.Errorf("%s: chapterless source differs with the option on (%d vs %d bytes)", name, len(off), len(on))
		}
		if bytes.Contains(on, []byte("EXT-X-DATERANGE")) || bytes.Contains(on, []byte("EventStream")) {
			t.Errorf("%s: a chapterless source must emit no markers even with the option on:\n%s", name, on)
		}
	}

	plan, err := PlanHLS(context.Background(), src, Options{SegmentMs: 2000, ChapterMarkers: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plan.MediaPlaylist(), []byte("EXT-X-DATERANGE")) {
		t.Error("plan: chapterless source must emit no DATERANGE even with the option on")
	}
}

// TestChapterMarkersEscaping isolates the escaping behaviour of the two
// low-level renderers against a title carrying a quote, an ampersand and
// angle brackets.
func TestChapterMarkersEscaping(t *testing.T) {
	chapters := []mkv.Chapter{{StartMs: 0, EndMs: 1000, Title: `A "quote" & <tag>`}}
	hls := string(buildChapterDateRanges(chapters))
	if !strings.Contains(hls, fmt.Sprintf("X-CHAPTER-TITLE=%q", chapters[0].Title)) {
		t.Errorf("HLS title not escaped as expected:\n%s", hls)
	}
	dash := buildChapterEventStream(chapters)
	if !strings.Contains(dash, ">A &#34;quote&#34; &amp; &lt;tag&gt;</Event>") {
		t.Errorf("DASH title not escaped as expected:\n%s", dash)
	}
}
