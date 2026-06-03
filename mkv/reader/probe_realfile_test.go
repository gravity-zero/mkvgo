package reader

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
)

const realFixture = "testdata/probe/hdr_multi.mkv"

// readFixture reads the committed real-world fixture (a libx265 10-bit BT.2020
// clip muxed by ffmpeg 7.1 with fra-default + eng audio and a forced spa
// subtitle). Generated with:
//
//	ffmpeg -f lavfi -i testsrc2=size=320x240:rate=24000/1001:duration=0.3 \
//	       -f lavfi -i sine=440:duration=0.3 -f lavfi -i sine=880:duration=0.3 -i s.srt \
//	       -map 0:v -map 1:a -map 2:a -map 3:s \
//	       -c:v libx265 -pix_fmt yuv420p10le \
//	       -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -color_range tv \
//	       -c:a ac3 -c:s srt \
//	       -metadata:s:a:0 language=fra -disposition:a:0 default \
//	       -metadata:s:a:1 language=eng -disposition:a:1 0 \
//	       -metadata:s:s:0 language=spa -disposition:s:0 forced hdr_multi.mkv
func readFixture(t *testing.T) *mkv.Container {
	t.Helper()
	f, err := os.Open(realFixture)
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	c, err := Read(context.Background(), f, "hdr_multi.mkv")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return c
}

// TestProbeRealFileGolden reads the real ffmpeg-muxed fixture and asserts the
// values mkvgo extracts — each one cross-checked against ffprobe 7.1
// (see TestProbeFFprobeEquivalence for the live comparison). Hermetic: runs in
// CI without ffmpeg/ffprobe.
func TestProbeRealFileGolden(t *testing.T) {
	c := readFixture(t)
	if len(c.Tracks) != 4 {
		t.Fatalf("tracks = %d, want 4", len(c.Tracks))
	}
	v, a0, a1, s := c.Tracks[0], c.Tracks[1], c.Tracks[2], c.Tracks[3]

	// Video — ffprobe: hevc 320x240 color_space=bt2020nc color_range=tv Main 10
	// 10-bit, r_frame_rate=24000/1001. ffmpeg's mkv muxer wrote only
	// MatrixCoefficients+Range in the container Colour; the SPS VUI carries CICP 2
	// (unspecified) for transfer/primaries, so those stay nil (matching ffprobe,
	// which omits unspecified). Bit depth and profile come from the hvcC SPS
	// fallback added in v0.6.0.
	if v.Codec != "hevc" || deref(t, v.Width) != 320 || deref(t, v.Height) != 240 {
		t.Errorf("video = %s %dx%d, want hevc 320x240", v.Codec, deref(t, v.Width), deref(t, v.Height))
	}
	if v.ColorSpaceName() != "bt2020nc" {
		t.Errorf("color_space = %q, want bt2020nc", v.ColorSpaceName())
	}
	if v.ColorRangeName() != "tv" {
		t.Errorf("color_range = %q, want tv", v.ColorRangeName())
	}
	if v.ColorTransfer != nil || v.ColorPrimaries != nil {
		t.Errorf("transfer/primaries should be nil (container absent + SPS unspecified), got %v/%v", v.ColorTransfer, v.ColorPrimaries)
	}
	if p16(v.VideoBitDepth) != 10 {
		t.Errorf("VideoBitDepth = %v, want 10 (from hvcC SPS fallback)", v.VideoBitDepth)
	}
	if v.Profile != "Main 10" {
		t.Errorf("profile = %q, want Main 10 (from hvcC)", v.Profile)
	}
	if v.IsHDR() { // transfer unspecified → cannot assert HDR
		t.Error("IsHDR = true, want false (transfer unspecified)")
	}
	if v.FrameRate == nil || math.Abs(*v.FrameRate-24000.0/1001.0) > 0.01 {
		t.Errorf("FrameRate = %v, want ~23.976", v.FrameRate)
	}
	// ffmpeg writes Language="und" for the untagged video; mkvgo reports it
	// faithfully (ffprobe instead omits the tag for "und").
	if v.Language != "und" || !v.LanguagePresent {
		t.Errorf("video lang = %q present=%v, want und/true", v.Language, v.LanguagePresent)
	}

	// Audio fra is the default; ffmpeg omits FlagDefault on it (spec default), so
	// mkvgo reports IsDefault=true with DefaultPresent=false.
	if a0.Codec != "ac3" || a0.Language != "fra" || !a0.IsDefault || a0.DefaultPresent {
		t.Errorf("a0 = %s/%s default=%v present=%v, want ac3/fra/true/false", a0.Codec, a0.Language, a0.IsDefault, a0.DefaultPresent)
	}
	if a0.FrameRate != nil {
		t.Errorf("a0.FrameRate = %v, want nil (video-only)", a0.FrameRate)
	}
	// Audio eng: ffmpeg writes explicit FlagDefault=0.
	if a1.Language != "eng" || a1.IsDefault || !a1.DefaultPresent {
		t.Errorf("a1 = %s default=%v present=%v, want eng/false/true", a1.Language, a1.IsDefault, a1.DefaultPresent)
	}

	// Forced subtitle, spa. mkvgo keeps "srt"; ffprobe says "subrip".
	if s.Codec != "srt" || mkv.FFprobeCodecName(s.Codec) != "subrip" {
		t.Errorf("sub codec = %q (ffprobe %q), want srt/subrip", s.Codec, mkv.FFprobeCodecName(s.Codec))
	}
	if s.Language != "spa" || !s.IsForced || !s.ForcedPresent {
		t.Errorf("sub = %s forced=%v present=%v, want spa/true/true", s.Language, s.IsForced, s.ForcedPresent)
	}
}

// --- live ffprobe equivalence (skips when ffprobe is not on PATH) ------------

type ffStream struct {
	Index       int               `json:"index"`
	CodecName   string            `json:"codec_name"`
	CodecType   string            `json:"codec_type"`
	Width       int               `json:"width"`
	Height      int               `json:"height"`
	ColorSpace  string            `json:"color_space"`
	ColorRange  string            `json:"color_range"`
	RFrameRate  string            `json:"r_frame_rate"`
	Tags        map[string]string `json:"tags"`
	Disposition map[string]int    `json:"disposition"`
}

// TestProbeFFprobeEquivalence runs ffprobe on the fixture and asserts mkvgo
// agrees on the fields a media indexer relies on. It skips when ffprobe is not
// installed, so CI stays hermetic; run it in an ffprobe-equipped environment
// (e.g. `PATH` including ffmpeg) to validate against the reference tool.
func TestProbeFFprobeEquivalence(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH; skipping live equivalence (golden test covers the values)")
	}
	if _, err := os.Stat(realFixture); err != nil {
		t.Skipf("fixture missing: %v", err)
	}

	out, err := exec.Command(ffprobe, "-v", "error", "-print_format", "json", "-show_streams", realFixture).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	var probe struct {
		Streams []ffStream `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("decode ffprobe json: %v", err)
	}

	c := readFixture(t)
	if len(c.Tracks) != len(probe.Streams) {
		t.Fatalf("stream count: mkvgo=%d ffprobe=%d", len(c.Tracks), len(probe.Streams))
	}

	for i, fs := range probe.Streams {
		tr := c.Tracks[i]

		// codec_name — compare via the documented mkvgo→ffprobe mapping.
		if got := mkv.FFprobeCodecName(tr.Codec); got != fs.CodecName {
			t.Errorf("stream %d codec: mkvgo=%q(→%q) ffprobe=%q", i, tr.Codec, got, fs.CodecName)
		}

		// language — ffprobe drops the tag for "und"; normalize both sides.
		if normLang(tr.ResolvedLanguage()) != normLang(fs.Tags["language"]) {
			t.Errorf("stream %d language: mkvgo=%q ffprobe=%q", i, tr.ResolvedLanguage(), fs.Tags["language"])
		}

		// default / forced disposition.
		if tr.IsDefault != (fs.Disposition["default"] == 1) {
			t.Errorf("stream %d default: mkvgo=%v ffprobe=%d", i, tr.IsDefault, fs.Disposition["default"])
		}
		if tr.IsForced != (fs.Disposition["forced"] == 1) {
			t.Errorf("stream %d forced: mkvgo=%v ffprobe=%d", i, tr.IsForced, fs.Disposition["forced"])
		}

		if fs.CodecType == "video" {
			if deref(t, tr.Width) != fs.Width || deref(t, tr.Height) != fs.Height {
				t.Errorf("stream %d dims: mkvgo=%dx%d ffprobe=%dx%d", i, deref(t, tr.Width), deref(t, tr.Height), fs.Width, fs.Height)
			}
			if tr.ColorSpaceName() != fs.ColorSpace {
				t.Errorf("stream %d color_space: mkvgo=%q ffprobe=%q", i, tr.ColorSpaceName(), fs.ColorSpace)
			}
			if tr.ColorRangeName() != fs.ColorRange {
				t.Errorf("stream %d color_range: mkvgo=%q ffprobe=%q", i, tr.ColorRangeName(), fs.ColorRange)
			}
			if want := parseRate(fs.RFrameRate); want > 0 {
				if tr.FrameRate == nil || math.Abs(*tr.FrameRate-want) > 0.01 {
					t.Errorf("stream %d frame rate: mkvgo=%v ffprobe=%v", i, tr.FrameRate, want)
				}
			}
		}
	}
}

func normLang(s string) string {
	if s == "" || s == "und" {
		return ""
	}
	return s
}

func parseRate(r string) float64 {
	num, den, ok := strings.Cut(r, "/")
	if !ok {
		return 0
	}
	n, _ := strconv.ParseFloat(num, 64)
	d, _ := strconv.ParseFloat(den, 64)
	if d == 0 {
		return 0
	}
	return n / d
}

func deref(t *testing.T, p *uint32) int {
	t.Helper()
	if p == nil {
		return -1
	}
	return int(*p)
}
