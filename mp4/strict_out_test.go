package mp4

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// twoVideoASS builds a Matroska with two H.264 video tracks and an ASS subtitle: an MP4 carries both videos and
// drops the ASS unless flattened; an HLS presentation carries one video rendition and the ASS as WebVTT.
func twoVideoASS(t *testing.T) string {
	t.Helper()
	video := func(id uint64) mkv.Track {
		return mkv.Track{ID: id, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32p(320), Height: u32p(240), FrameRate: f64p(25)}
	}
	tracks := []mkv.Track{video(1), video(2), {ID: 3, Type: mkv.SubtitleTrack, Codec: "ass"}}
	blocks := []genBlock{
		{track: 1, pts: 0, key: true, data: []byte{1}},
		{track: 2, pts: 0, key: true, data: []byte{1}},
		{track: 3, pts: 0, key: true, data: []byte("0,0,Default,,0,0,0,,Hi")},
		{track: 1, pts: 40, key: false, data: []byte{2}},
		{track: 2, pts: 40, key: false, data: []byte{2}},
	}
	return buildMKV(t, tracks, blocks)
}

// TestStrictRefusesDropsTowardsMP4AndHLS: towards MP4 and HLS, every track left out is reported with its reason,
// and Options.Strict refuses it by name instead - the ASS an MP4 has no form for, the second video an HLS
// presentation does not carry. An ASS track is a WebVTT rendition in HLS, so HLS does not report it dropped.
func TestStrictRefusesDropsTowardsMP4AndHLS(t *testing.T) {
	ctx := context.Background()
	src := twoVideoASS(t)
	dir := t.TempDir()
	var drops []DroppedTrack
	collect := func(d DroppedTrack) { drops = append(drops, d) }
	if err := RemuxToMP4(ctx, src, filepath.Join(dir, "a.mp4"), Options{OnDrop: collect}); err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].ID != 3 || drops[0].Codec != "ass" {
		t.Errorf("to MP4: drops = %+v, want the ASS track 3 alone", drops)
	}
	err := RemuxToMP4(ctx, src, filepath.Join(dir, "b.mp4"), Options{Strict: true})
	if err == nil || !strings.Contains(err.Error(), "track 3 (ass)") || !strings.Contains(err.Error(), "Options.Strict") {
		t.Errorf("to MP4 strict: err = %v, want track 3 (ass) refused by Options.Strict", err)
	}
	drops = nil
	if err := RemuxToHLS(ctx, src, filepath.Join(dir, "hls"), Options{OnDrop: collect}); err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].ID != 2 || !strings.Contains(drops[0].Reason, "secondary video") {
		t.Errorf("to HLS: drops = %+v, want the second video alone, the ASS being a WebVTT rendition", drops)
	}
	err = RemuxToHLS(ctx, src, filepath.Join(dir, "hls2"), Options{Strict: true})
	if err == nil || !strings.Contains(err.Error(), "track 2 (h264)") || !strings.Contains(err.Error(), "Options.Strict") {
		t.Errorf("to HLS strict: err = %v, want track 2 (h264) refused by Options.Strict", err)
	}
	drops = nil
	if _, err := PlanHLS(ctx, src, Options{OnDrop: collect, SynthesizeIndex: true}); err != nil {
		t.Fatal(err)
	}
	if len(drops) != 1 || drops[0].ID != 2 {
		t.Errorf("plan: drops = %+v, want the second video alone", drops)
	}
	if _, err := PlanHLS(ctx, src, Options{Strict: true, SynthesizeIndex: true}); err == nil || !strings.Contains(err.Error(), "track 2 (h264)") {
		t.Errorf("plan strict: err = %v, want track 2 (h264) refused", err)
	}
}

// TestTracksCarriageSaysPerTrackAndPerTarget: TracksCarriage says, per track and per target, whether a remux to
// MP4 and an HLS presentation carry it, in the words OnDrop would report; the options that change the answer (a
// flattened ASS) are honoured.
func TestTracksCarriageSaysPerTrackAndPerTarget(t *testing.T) {
	c, _ := readMKV(t, twoVideoASS(t))
	cr, err := TracksCarriage(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr) != 3 {
		t.Fatalf("%d entries, want 3", len(cr))
	}
	if cr[0].MP4 != "" || cr[0].HLS != "" {
		t.Errorf("track 1 = %+v, want carried by both", cr[0])
	}
	if cr[1].MP4 != "" || !strings.Contains(cr[1].HLS, "secondary video") {
		t.Errorf("track 2 = %+v, want carried by MP4, dropped by HLS as secondary video", cr[1])
	}
	if !strings.Contains(cr[2].MP4, "FlattenStyledSubs") || cr[2].HLS != "" {
		t.Errorf("track 3 = %+v, want dropped by MP4 unless flattened, a WebVTT rendition in HLS", cr[2])
	}
	if cr, err := TracksCarriage(c, Options{FlattenStyledSubs: true}); err != nil || cr[2].MP4 != "" {
		t.Errorf("flattened: track 3 = %+v (%v), want carried by MP4", cr[2], err)
	}
}
