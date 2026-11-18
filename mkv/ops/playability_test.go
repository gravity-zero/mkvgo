package ops

// playability_test.go tests the decision model in two layers:
//   - unit tests directly against evaluateTrack/evaluatePlayability (this
//     package), which exercise Profile/Level/HDR/DolbyVision fields set
//     directly on a synthetic mkv.Track - the codec bitstream parsing that
//     fills those fields from a real file is mkv/reader's own responsibility
//     and is covered by its tests there;
//   - full-pipeline tests through Playability() against the real sample.mkv
//     fixture (valid CodecPrivate, so it round-trips through mp4.RemuxToMP4)
//     read back via MemFS and via mp4.OpenMeta, wiring-focused.

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

func u16(v uint16) *uint16 { return &v }

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// --- 1/2/4/5: per-track decision unit tests ---

func TestPlayability_H264High40_MP4_MSEGeneric_DirectPlay(t *testing.T) {
	target, _ := TargetByName("mse-generic")
	track := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Profile: "High", Level: u16(40)}
	v := evaluateTrack(track, "mp4", target)
	if v.Verdict != verdictDirectPlay {
		t.Fatalf("verdict = %q, reasons=%v, want direct-play", v.Verdict, v.Reasons)
	}
}

func TestPlayability_H264High40_MKV_Safari_Remux(t *testing.T) {
	target, _ := TargetByName("safari")
	track := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Profile: "High", Level: u16(40)}
	v := evaluateTrack(track, "mkv", target)
	if v.Verdict != verdictRemux {
		t.Fatalf("verdict = %q, reasons=%v, want remux", v.Verdict, v.Reasons)
	}
	if len(v.Reasons) == 0 || !strings.Contains(v.Reasons[0], "mp4") {
		t.Fatalf("reasons = %v, want a mention of the mp4 remux target", v.Reasons)
	}
}

func TestPlayability_HEVCMain10HDR10_Safari_DirectPlay_Chrome_Transcode(t *testing.T) {
	track := mkv.Track{
		ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Profile: "Main 10",
		HDR: &mkv.HDRStaticMetadata{MaxCLL: 1000, MaxFALL: 400},
	}

	safari, _ := TargetByName("safari")
	if v := evaluateTrack(track, "mp4", safari); v.Verdict != verdictDirectPlay {
		t.Fatalf("safari verdict = %q, reasons=%v, want direct-play", v.Verdict, v.Reasons)
	}

	chrome, _ := TargetByName("chrome")
	v := evaluateTrack(track, "mp4", chrome)
	if v.Verdict != verdictTranscode {
		t.Fatalf("chrome verdict = %q, want transcode", v.Verdict)
	}
	if len(v.Reasons) == 0 || !strings.Contains(strings.ToLower(v.Reasons[0]), "hevc") {
		t.Fatalf("chrome reasons = %v, want a reason mentioning HEVC", v.Reasons)
	}
}

// TestPlayability_HDRTransferWithoutStaticMetadata: a PQ or HLG video with no
// MasteringMetadata/MaxCLL is still HDR; an SDR target must transcode it.
func TestPlayability_HDRTransferWithoutStaticMetadata(t *testing.T) {
	sdrTV := Target{Name: "custom", Container: []string{"mkv"}, VideoCodecs: []string{"hevc"}, HEVCMain10: true}
	hdrTV := sdrTV
	hdrTV.HDR = true
	for _, tc := range []struct {
		name     string
		transfer uint16
		want     string
	}{{"PQ", 16, "PQ"}, {"HLG", 18, "HLG"}} {
		track := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Profile: "Main 10",
			ColorPrimaries: u16(9), ColorTransfer: u16(tc.transfer)}
		v := evaluateTrack(track, "mkv", sdrTV)
		if v.Verdict != verdictTranscode || len(v.Reasons) != 1 || !strings.Contains(v.Reasons[0], tc.want) {
			t.Errorf("%s on SDR target: verdict = %q, reasons = %v, want transcode naming %s", tc.name, v.Verdict, v.Reasons, tc.want)
		}
		if v := evaluateTrack(track, "mkv", hdrTV); v.Verdict != verdictDirectPlay {
			t.Errorf("%s on HDR target: verdict = %q, reasons = %v, want direct-play", tc.name, v.Verdict, v.Reasons)
		}
	}
	sdr := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Profile: "Main 10", ColorPrimaries: u16(1), ColorTransfer: u16(1)}
	if v := evaluateTrack(sdr, "mkv", sdrTV); v.Verdict != verdictDirectPlay {
		t.Errorf("SDR on SDR target: verdict = %q, reasons = %v, want direct-play", v.Verdict, v.Reasons)
	}
}

// TestPlayability_PQInBandOnly: a HEVC track whose bare hvcC and missing Colour
// element leave the header silent, PQ stated only by the first sample's SPS (as
// some 2160p remuxes write it), is HDR for Playability.
func TestPlayability_PQInBandOnly(t *testing.T) {
	hvcC, err := hex.DecodeString("0102200000009000000000001ef000fcfdfafa00000f03200001001840010c01ffff02200000030090000003000003001e959809210001002d42010102200000030090000003000003001ea0208104d96566924caf016a12201208000003000800000300c04022000100074401c172b42240")
	if err != nil {
		t.Fatal(err)
	}
	var sps []byte
	for off, a := 23, 0; a < int(hvcC[22]); a++ {
		typ, n := hvcC[off]&0x3f, int(hvcC[off+1])<<8|int(hvcC[off+2])
		off += 3
		for range n {
			ln := int(hvcC[off])<<8 | int(hvcC[off+1])
			if typ == 33 {
				sps = hvcC[off+2 : off+2+ln]
			}
			off += 2 + ln
		}
	}
	bare := append([]byte(nil), hvcC[:23]...)
	bare[22] = 0
	frame := append([]byte{0, 0, 0, byte(len(sps))}, sps...)
	video := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", CodecPrivate: bare, Width: u32(3840), Height: u32(2160)}
	src := buildMultiClusterMKV(t, t.TempDir(), "pq.mkv", []mkv.Track{video},
		[][]mkv.Block{{{TrackNumber: 1, Keyframe: true, Data: frame}}}, 40)

	sdrTV := Target{Name: "custom", Container: []string{"mkv"}, VideoCodecs: []string{"hevc"}, HEVCMain10: true}
	r, err := Playability(context.Background(), src, sdrTV)
	if err != nil {
		t.Fatal(err)
	}
	if v := r.Tracks[0]; v.Verdict != verdictTranscode || !strings.Contains(strings.Join(v.Reasons, "|"), "HDR") {
		t.Errorf("PQ in-band only on SDR target: verdict = %q, reasons = %v, want transcode for HDR", v.Verdict, v.Reasons)
	}
}

func TestPlayability_LevelOverMax_Transcode(t *testing.T) {
	target, _ := TargetByName("mse-generic") // MaxLevelH264 = 41 (High@4.1)
	track := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Profile: "High", Level: u16(51)}
	v := evaluateTrack(track, "mp4", target)
	if v.Verdict != verdictTranscode {
		t.Fatalf("verdict = %q, want transcode", v.Verdict)
	}
	joined := strings.Join(v.Reasons, "; ")
	if !strings.Contains(joined, "5.1") || !strings.Contains(joined, "4.1") {
		t.Fatalf("reasons = %v, want the level numbers 5.1 / 4.1", v.Reasons)
	}
}

func TestPlayability_MissingLevel_Conservative(t *testing.T) {
	target, _ := TargetByName("mse-generic")
	track := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Profile: "High"} // Level nil
	v := evaluateTrack(track, "mp4", target)
	if v.Verdict != verdictTranscode {
		t.Fatalf("verdict = %q, want transcode (conservative on unknown level)", v.Verdict)
	}
	joined := strings.ToLower(strings.Join(v.Reasons, "; "))
	if !strings.Contains(joined, "level") {
		t.Fatalf("reasons = %v, want a reason mentioning the missing level", v.Reasons)
	}
}

// --- 6: overall = worst of the per-track verdicts ---

func TestPlayability_OverallIsWorstTrack(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Profile: "High", Level: u16(30)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "vorbis"}, // not in mse-generic's AudioCodecs (aac only)
	}
	target, _ := TargetByName("mse-generic") // Container: mp4 - srcContainer "mp4" already matches, so video is direct-play
	report := evaluatePlayability(tracks, "mp4", target)

	if report.OverallVerdict != verdictTranscode {
		t.Fatalf("overall = %q, want transcode (audio codec unsupported)", report.OverallVerdict)
	}
	var sawVideoDirect, sawAudioTranscode bool
	for _, tv := range report.Tracks {
		if tv.Type == "video" && tv.Verdict == verdictDirectPlay {
			sawVideoDirect = true
		}
		if tv.Type == "audio" && tv.Verdict == verdictTranscode {
			sawAudioTranscode = true
		}
	}
	if !sawVideoDirect || !sawAudioTranscode {
		t.Fatalf("tracks = %+v, want video direct-play + audio transcode", report.Tracks)
	}
}

func TestPlayability_RemuxContainer_MKVtoSafari(t *testing.T) {
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "h264", Profile: "High", Level: u16(30)},
		{ID: 2, Type: mkv.AudioTrack, Codec: "aac"},
	}
	target, _ := TargetByName("safari") // Container: mp4, hls - srcContainer "mkv" matches neither
	report := evaluatePlayability(tracks, "mkv", target)

	if report.OverallVerdict != verdictRemux {
		t.Fatalf("overall = %q, want remux", report.OverallVerdict)
	}
	if report.RemuxContainer != "mp4" && report.RemuxContainer != "hls" {
		t.Fatalf("RemuxContainer = %q, want mp4 or hls", report.RemuxContainer)
	}
}

// --- 9: FS port via MemFS; MP4 source via mp4.OpenMeta ---

func TestPlayability_MemFS(t *testing.T) {
	data := mustReadFile(t, sampleMKV) // h264 (level 1.3) + aac, real CodecPrivate
	mem := mkv.NewMemFS()
	mem.Put("video.mkv", data)

	target, _ := TargetByName("mse-generic") // Container: mp4 only, so an MKV source always needs a remux here
	report, err := Playability(context.Background(), "video.mkv", target, mkv.Options{FS: mem.FS()})
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallVerdict != verdictRemux {
		t.Fatalf("overall = %q, reasons on tracks=%+v, want remux", report.OverallVerdict, report.Tracks)
	}
	if report.RemuxContainer != "mp4" {
		t.Fatalf("RemuxContainer = %q, want mp4", report.RemuxContainer)
	}
}

func TestPlayability_MP4Source_ViaMP4OpenMeta(t *testing.T) {
	dir := t.TempDir()
	dstMP4 := filepath.Join(dir, "out.mp4")
	if err := mp4.RemuxToMP4(context.Background(), sampleMKV, dstMP4); err != nil {
		t.Fatal(err)
	}

	target, _ := TargetByName("mse-generic")
	report, err := Playability(context.Background(), dstMP4, target)
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallVerdict != verdictDirectPlay {
		t.Fatalf("overall = %q, reasons on tracks=%+v, want direct-play (already mp4/h264/aac)", report.OverallVerdict, report.Tracks)
	}
}

// --- 8: TargetByName ---

func TestTargetByName_Unknown(t *testing.T) {
	target, ok := TargetByName("nonexistent-target")
	if ok {
		t.Fatalf("TargetByName(unknown) ok = true, want false")
	}
	if target.Name != "" || target.Container != nil || target.VideoCodecs != nil || target.AudioCodecs != nil {
		t.Fatalf("TargetByName(unknown) = %+v, want the zero value", target)
	}
}

func TestTargetByName_ChromiumFamilyEquivalence(t *testing.T) {
	chrome, _ := TargetByName("chrome")
	brave, _ := TargetByName("brave")
	generic, _ := TargetByName("chromium-generic")

	sameCapabilities := func(a, b Target) bool {
		return equalStrSlice(a.Container, b.Container) &&
			equalStrSlice(a.VideoCodecs, b.VideoCodecs) &&
			equalStrSlice(a.AudioCodecs, b.AudioCodecs) &&
			a.MaxWidth == b.MaxWidth && a.MaxHeight == b.MaxHeight &&
			a.MaxLevelH264 == b.MaxLevelH264 && a.MaxLevelHEVC == b.MaxLevelHEVC &&
			a.HDR == b.HDR && a.DolbyVision == b.DolbyVision &&
			a.HEVCMain10 == b.HEVCMain10 && a.VP9Profile2 == b.VP9Profile2
	}
	if !sameCapabilities(chrome, brave) {
		t.Fatalf("brave capabilities differ from chrome:\n%+v\n%+v", brave, chrome)
	}
	if !sameCapabilities(chrome, generic) {
		t.Fatalf("chromium-generic capabilities differ from chrome:\n%+v\n%+v", generic, chrome)
	}
}

func TestTargetByName_EdgeAddsHEVC(t *testing.T) {
	hevcMain10 := mkv.Track{ID: 1, Type: mkv.VideoTrack, Codec: "hevc", Profile: "Main 10"}

	chrome, _ := TargetByName("chrome")
	if v := evaluateTrack(hevcMain10, "mp4", chrome); v.Verdict != verdictTranscode {
		t.Fatalf("chrome verdict on HEVC Main10 = %q, want transcode", v.Verdict)
	}
	brave, _ := TargetByName("brave")
	if v := evaluateTrack(hevcMain10, "mp4", brave); v.Verdict != verdictTranscode {
		t.Fatalf("brave verdict on HEVC Main10 = %q, want transcode", v.Verdict)
	}
	edge, _ := TargetByName("edge")
	if v := evaluateTrack(hevcMain10, "mp4", edge); v.Verdict != verdictDirectPlay {
		t.Fatalf("edge verdict on HEVC Main10 = %q, reasons=%v, want direct-play", v.Verdict, v.Reasons)
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
