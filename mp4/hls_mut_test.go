package mp4

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

// hls_mut_test.go targets gremlins survivors in hls.go: the HLS playlist
// assembly (media/master/I-frame playlists, bandwidth accounting, RESOLUTION/
// FRAME-RATE/CODECS attribute gating) and the SegmentMs<=0 default fallback.
// Each test asserts the exact text a branch controls, not just "no error".

// hlsmutFT builds a minimal fragTrack for exercising the playlist-assembly
// functions directly, without a full remux: only outTrack.mkv and
// outTrack.spec.video are read by buildMasterPlaylist/iframeStreamInfURI.
func hlsmutFT(isVideo bool, tr mkv.Track) *fragTrack {
	return &fragTrack{outTrack: &outTrack{mkv: tr, spec: codecSpec{video: isVideo}}}
}

// TestHLSMutSegmentMsDefaultFallback proves hls.go:144 (segMs <= 0 -> default):
// with SegmentMs 0 explicitly, the presentation must be cut at the default 6s
// cadence, not at every keyframe (which is what happens if the zero value
// slips through uncorrected — the boundary test `segMs >= 0` would leave segMs
// at 0, and segmentBoundaries cuts at every keyframe once the target is 0).
func TestHLSMutSegmentMsDefaultFallback(t *testing.T) {
	// 3 keyframes at 0/1000/2000ms, 2400ms total: well under the 6s default,
	// so the default cadence produces exactly one segment.
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(320), Height: u32(240)}
	src := buildABRVariant(t, video)
	dir := t.TempDir()
	ctx := context.Background()
	if err := RemuxToHLS(ctx, src, dir, Options{SegmentMs: 0}); err != nil {
		t.Fatal(err)
	}
	pl := readTextFile(t, filepath.Join(dir, "playlist.m3u8"))
	n := strings.Count(pl, "#EXTINF:")
	if n != 1 {
		t.Fatalf("SegmentMs:0 should fall back to the 6s default (one segment for a 2.4s file), got %d segments:\n%s", n, pl)
	}
}

// TestHLSMutBuildMediaPlaylistTargetDuration proves hls.go:773 (d > max
// negation) and hls.go:779 (max+0.999 arithmetic): TARGETDURATION must be the
// ceil of the LARGEST duration, not the smallest or a decremented one. Under
// the `d > max` negation, max never advances past its zero value (durs is
// never non-increasing from 0), so TARGETDURATION would come out 0 instead
// of 6. Under the arithmetic mutant, max-0.999 gives 4 instead of 6.
func TestHLSMutBuildMediaPlaylistTargetDuration(t *testing.T) {
	durs := []float64{2.0, 5.5, 3.0} // max=5.5 -> ceil 6
	segName := func(i int) string { return fmt.Sprintf("seg%05d.m4s", i+1) }
	b := buildMediaPlaylist(nil, durs, "init.mp4", segName, nil)
	s := string(b)
	mustContain(t, s, "#EXT-X-TARGETDURATION:6\n")
}

// TestHLSMutBuildMediaPlaylistChapters exercises hls.go:781 (len(chapters) >
// 0): a non-empty chapter list must render EXT-X-DATERANGE lines, a nil one
// must not.
func TestHLSMutBuildMediaPlaylistChapters(t *testing.T) {
	segName := func(i int) string { return fmt.Sprintf("seg%05d.m4s", i+1) }
	withChapters := buildMediaPlaylist(nil, []float64{2.0}, "init.mp4", segName,
		[]mkv.Chapter{{Title: "Intro", StartMs: 0, EndMs: 2000}})
	mustContain(t, string(withChapters), "#EXT-X-DATERANGE:")

	noChapters := buildMediaPlaylist(nil, []float64{2.0}, "init.mp4", segName, nil)
	mustNotContain(t, string(noChapters), "#EXT-X-DATERANGE:")
}

// TestHLSMutPeakBandwidth proves hls.go:869 (bps > peak negation): the peak
// must be the LARGEST per-segment bitrate. Under the negation, bps <= peak
// is checked instead, so peak never advances past its zero value and
// peakBandwidth would return 0 instead of 1600.
func TestHLSMutPeakBandwidth(t *testing.T) {
	segs := []segInfo{
		{durSec: 1, bytes: 100}, // 800 bps
		{durSec: 2, bytes: 400}, // 1600 bps
	}
	if got := peakBandwidth(segs); got != 1600 {
		t.Fatalf("peakBandwidth = %d, want 1600", got)
	}
}

// TestHLSMutMasterAudioDefault proves hls.go:888 (i != prim negation): the
// primary (video) track must never count toward hasDefaultAudio, so with one
// audio track explicitly flagged default and another not, exactly one
// EXT-X-MEDIA:TYPE=AUDIO line carries DEFAULT=YES. It also proves hls.go:897
// and hls.go:904 (name/language fallback negations): an audio track with an
// empty Name and a set Language must render NAME=<language> and LANGUAGE=.
func TestHLSMutMasterAudioDefault(t *testing.T) {
	video := hlsmutFT(true, mkv.Track{ID: 1, Type: mkv.VideoTrack})
	audioA := hlsmutFT(false, mkv.Track{ID: 2, Type: mkv.AudioTrack, Language: "fra"})
	audioB := hlsmutFT(false, mkv.Track{ID: 3, Type: mkv.AudioTrack, Name: "Commentary", IsDefault: true})
	fts := []*fragTrack{video, audioA, audioB}

	b := buildMasterPlaylist(nil, fts, nil, nil, nil)
	s := string(b)
	mustContain(t, s, `NAME="fra"`)
	mustContain(t, s, `LANGUAGE="fra"`)

	lines := strings.Split(s, "\n")
	defaults := 0
	fraHasDefault := false
	for _, l := range lines {
		if !strings.HasPrefix(l, "#EXT-X-MEDIA:TYPE=AUDIO") {
			continue
		}
		if strings.Contains(l, "DEFAULT=YES") {
			defaults++
			if strings.Contains(l, `NAME="fra"`) {
				fraHasDefault = true
			}
		}
	}
	if defaults != 1 {
		t.Errorf("want exactly one audio rendition with DEFAULT=YES (the explicitly flagged one), got %d:\n%s", defaults, s)
	}
	if fraHasDefault {
		t.Errorf("audioA (not flagged default, not primary) must not get DEFAULT=YES:\n%s", s)
	}
}

// TestHLSMutMasterSubtitleName proves hls.go:916 (name == "" && language !=
// "" negation): a subtitle track with an empty Name and a set Language must
// fall back to NAME=<language>.
func TestHLSMutMasterSubtitleName(t *testing.T) {
	video := hlsmutFT(true, mkv.Track{ID: 1, Type: mkv.VideoTrack})
	subs := []hlsSubTrack{{track: mkv.Track{ID: 2, Type: mkv.SubtitleTrack, Language: "spa"}}}
	b := buildMasterPlaylist(nil, []*fragTrack{video}, subs, nil, nil)
	mustContain(t, string(b), `TYPE=SUBTITLES,GROUP-ID="subs",NAME="spa"`)
}

// TestHLSMutMasterBandwidth proves hls.go:939/940 (durSec > 0 / bps > peak
// negations), hls.go:944 (totalBits arithmetic) and hls.go:949 (division):
// BANDWIDTH must be the peak per-segment bitrate and AVERAGE-BANDWIDTH the
// true total-bits-over-total-seconds average, which differs from the peak
// here (1200 vs 1600) — any of those mutations produces a different number.
func TestHLSMutMasterBandwidth(t *testing.T) {
	video := hlsmutFT(true, mkv.Track{ID: 1, Type: mkv.VideoTrack})
	segs := []segInfo{
		{durSec: 1, bytes: 100}, // 800 bps
		{durSec: 1, bytes: 200}, // 1600 bps
	}
	// totalBits = (100+200)*8 = 2400, totalSec = 2, avg = 1200.
	b := buildMasterPlaylist(nil, []*fragTrack{video}, nil, segs, nil)
	mustContain(t, string(b), "BANDWIDTH=1600,AVERAGE-BANDWIDTH=1200")
}

// TestHLSMutMasterBandwidthZeroDuration proves hls.go:948 (totalSec > 0
// boundary): with every segment's durSec at exactly 0, totalSec stays 0 and
// AVERAGE-BANDWIDTH must fall back to the peak (0 here, since durSec > 0 also
// never holds for the peak/bps computation). Under the `totalSec >= 0`
// boundary mutant, the always-true guard divides by zero and the resulting
// +Inf truncates to a large negative int64 instead of 0.
func TestHLSMutMasterBandwidthZeroDuration(t *testing.T) {
	video := hlsmutFT(true, mkv.Track{ID: 1, Type: mkv.VideoTrack})
	segs := []segInfo{{durSec: 0, bytes: 500}}
	b := buildMasterPlaylist(nil, []*fragTrack{video}, nil, segs, nil)
	mustContain(t, string(b), "BANDWIDTH=0,AVERAGE-BANDWIDTH=0")
}

// TestHLSMutMasterResolutionFrameRate proves hls.go:954 (*t.Width > 0 and
// *t.Height > 0 boundaries) and hls.go:957 (*t.FrameRate > 0 boundary): a
// zero Width, Height or FrameRate must omit the corresponding attribute, not
// render it as "0". Under any of the `> 0` -> `>= 0` boundary mutants, the
// zero-valued dimension would pass the guard and render "…=0…".
func TestHLSMutMasterResolutionFrameRate(t *testing.T) {
	fr := 24.0
	t.Run("zero-width", func(t *testing.T) {
		v := hlsmutFT(true, mkv.Track{Width: u32(0), Height: u32(1080), FrameRate: &fr})
		b := buildMasterPlaylist(nil, []*fragTrack{v}, nil, nil, nil)
		mustNotContain(t, string(b), "RESOLUTION=")
	})
	t.Run("zero-height", func(t *testing.T) {
		v := hlsmutFT(true, mkv.Track{Width: u32(1920), Height: u32(0), FrameRate: &fr})
		b := buildMasterPlaylist(nil, []*fragTrack{v}, nil, nil, nil)
		mustNotContain(t, string(b), "RESOLUTION=")
	})
	t.Run("zero-framerate", func(t *testing.T) {
		zero := 0.0
		v := hlsmutFT(true, mkv.Track{Width: u32(1920), Height: u32(1080), FrameRate: &zero})
		b := buildMasterPlaylist(nil, []*fragTrack{v}, nil, nil, nil)
		mustNotContain(t, string(b), "FRAME-RATE=")
	})
	t.Run("present", func(t *testing.T) {
		v := hlsmutFT(true, mkv.Track{Width: u32(1920), Height: u32(1080), FrameRate: &fr})
		b := buildMasterPlaylist(nil, []*fragTrack{v}, nil, nil, nil)
		mustContain(t, string(b), "RESOLUTION=1920x1080")
		mustContain(t, string(b), "FRAME-RATE=24.000")
	})
}

// TestHLSMutBuildIFramePlaylistTargetDuration proves hls.go:994 (d > max
// negation) and hls.go:1000 (max+0.999 arithmetic), mirroring
// TestHLSMutBuildMediaPlaylistTargetDuration for the trick-play playlist.
func TestHLSMutBuildIFramePlaylistTargetDuration(t *testing.T) {
	durs := []float64{2.0, 5.5, 3.0}
	iframes := []iframeRef{{seg: 1, length: 10}}
	b := buildIFramePlaylist(nil, nil, durs, iframes)
	mustContain(t, string(b), "#EXT-X-TARGETDURATION:6\n")
}

// TestHLSMutIFrameStreamInfBandwidth proves hls.go:1023 (d > 0 negation) and
// hls.go:1024 (length*8 arithmetic): BANDWIDTH must be the true peak
// I-frame bitrate (500*8/2 = 2000), and hls.go:1035 (cs != "" negation): a
// known codec must render CODECS.
func TestHLSMutIFrameStreamInfBandwidth(t *testing.T) {
	video := hlsmutFT(true, mkv.Track{Codec: "h264", CodecPrivate: fakeAVCC, Width: u32(1920), Height: u32(1080)})
	durs := []float64{2.0}
	iframes := []iframeRef{{seg: 0, length: 500}}
	inf := iframeStreamInfURI(nil, []*fragTrack{video}, durs, iframes, "iframe.m3u8")
	mustContain(t, inf, "BANDWIDTH=2000")
	mustContain(t, inf, "RESOLUTION=1920x1080")
	mustContain(t, inf, `CODECS="avc1.`)
}

// TestHLSMutIFrameStreamInfZeroDuration proves hls.go:1023 (d > 0 boundary):
// a zero-duration segment must be skipped for the peak computation (BANDWIDTH
// stays 0), not divide by zero. Under the `d >= 0` boundary mutant, the
// guard fires on the zero duration and the resulting +Inf bps truncates to a
// large negative int64 instead of 0.
func TestHLSMutIFrameStreamInfZeroDuration(t *testing.T) {
	video := hlsmutFT(true, mkv.Track{})
	durs := []float64{0}
	iframes := []iframeRef{{seg: 0, length: 800}}
	inf := iframeStreamInfURI(nil, []*fragTrack{video}, durs, iframes, "iframe.m3u8")
	mustContain(t, inf, "BANDWIDTH=0")
}

// TestHLSMutIFrameStreamInfResolutionGuards proves hls.go:1032's three
// conditions: t.Width != nil (col 14, negation — a nil Width must not be
// dereferenced), *t.Width > 0 (col 52, boundary/negation) and *t.Height > 0
// (col 69, boundary/negation). A nil or zero Width/Height must omit
// RESOLUTION, never panic and never render "…=0…".
func TestHLSMutIFrameStreamInfResolutionGuards(t *testing.T) {
	durs := []float64{1}
	iframes := []iframeRef{{seg: 0, length: 100}}
	t.Run("nil-width", func(t *testing.T) {
		v := hlsmutFT(true, mkv.Track{Width: nil, Height: u32(1080)})
		inf := iframeStreamInfURI(nil, []*fragTrack{v}, durs, iframes, "iframe.m3u8")
		mustNotContain(t, inf, "RESOLUTION=")
	})
	t.Run("zero-width", func(t *testing.T) {
		v := hlsmutFT(true, mkv.Track{Width: u32(0), Height: u32(1080)})
		inf := iframeStreamInfURI(nil, []*fragTrack{v}, durs, iframes, "iframe.m3u8")
		mustNotContain(t, inf, "RESOLUTION=")
	})
	t.Run("zero-height", func(t *testing.T) {
		v := hlsmutFT(true, mkv.Track{Width: u32(1920), Height: u32(0)})
		inf := iframeStreamInfURI(nil, []*fragTrack{v}, durs, iframes, "iframe.m3u8")
		mustNotContain(t, inf, "RESOLUTION=")
	})
}

// TestHLSMutIFrameStreamInfNoVideo proves hls.go:1030 (v != nil negation):
// an audio-only fts slice (pickVideoFrag returns nil) must skip the
// RESOLUTION/CODECS block entirely, not dereference the nil video fragment.
func TestHLSMutIFrameStreamInfNoVideo(t *testing.T) {
	audioOnly := hlsmutFT(false, mkv.Track{Type: mkv.AudioTrack})
	durs := []float64{1}
	iframes := []iframeRef{{seg: 0, length: 100}}
	inf := iframeStreamInfURI(nil, []*fragTrack{audioOnly}, durs, iframes, "iframe.m3u8")
	mustNotContain(t, inf, "RESOLUTION=")
	mustNotContain(t, inf, "CODECS=")
}
