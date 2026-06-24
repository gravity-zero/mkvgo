package commands_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

// writeMKV serialises c to a temp file and returns its path.
func writeMKV(t *testing.T, c *matroska.Container) string {
	t.Helper()
	var buf bytes.Buffer
	if err := matroska.Write(&buf, c); err != nil {
		t.Fatalf("matroska.Write: %v", err)
	}
	path := filepath.Join(t.TempDir(), "test.mkv")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
	return path
}

func ptrU32(v uint32) *uint32 { return &v }
func ptrU16(v uint16) *uint16 { return &v }
func ptrF64(v float64) *float64 { return &v }
func ptrU8(v uint8) *uint8 { return &v }

// TestFmtMs exercises every arithmetic operator in the helpers.go FmtMs function.
// The value 3661000 ms = 1h 1m 1s produces "01:01:01", which distinguishes all
// mutations: /1000→*1000, s/3600→s*3600, s%3600→s*3600, (s%3600)/60→(s%3600)*60,
// s%60→s/60.
func TestFmtMs(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "00:00:00"},
		{1000, "00:00:01"},
		{61000, "00:01:01"},
		{3661000, "01:01:01"},  // kills all five arithmetic mutations in FmtMs
		{86399000, "23:59:59"}, // large value; kills /3600→*3600, %3600, /60→*60, %60
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.ms), func(t *testing.T) {
			got := commands.FmtMs(tt.ms)
			if got != tt.want {
				t.Errorf("FmtMs(%d) = %q, want %q", tt.ms, got, tt.want)
			}
		})
	}
}

// TestCmdProbeMP4Details probes the regfixMKV→MP4 fixture and asserts detailed
// track-level formatting fields that survive when missing. The MP4 probe path
// fills FrameCount, per-track DurationMs, Bitrate, Profile, etc. from the sample
// table and SPS, unlike the MKV head-only path.
func TestCmdProbeMP4Details(t *testing.T) {
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "x.mp4")
	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	out := capture(t, func() { commands.CmdProbe(mp4Path) })

	// Each assertion below kills one or more specific mutants:
	wants := []struct {
		want   string
		reason string
	}{
		// Width/Height: kills CONDITIONALS_NEGATION on != nil guards (165:14, 165:33).
		{"320x180", "Width/Height dimensions"},
		// FrameCount > 0: kills CONDITIONALS_BOUNDARY/NEGATION (186:19).
		{"144frames", "FrameCount > 0"},
		// DurationMs > 0 and /1000 arithmetic: kills 189:19 and 190:47.
		// "6.00s" ≠ "6000000.00s" (mutant /→*) or "0.00s" (boundary mutant).
		{"6.00s", "per-track DurationMs /1000 (not *1000)"},
		// ChannelLayout != "": kills CONDITIONALS_NEGATION at 176:35.
		{"(mono)", "ChannelLayout non-empty"},
		// len(CodecPrivate) > 0 and format: kills 198:26 and the format at 199.
		{"codec_private=46 bytes", "len(CodecPrivate) > 0 and exact byte count"},
		// CodecLongName != "": kills CONDITIONALS_NEGATION at 202:34.
		{"H.264 / AVC", "CodecLongName non-empty"},
		// Level != nil inner check: kills 213:15.
		{"level=12", "Level != nil inner conditional"},
		// PixelFormat != "" inner check: kills 216:21.
		{"pix_fmt=yuv444p", "PixelFormat != '' inner conditional"},
		// FieldOrder != "" inner check: kills 219:20.
		{"field_order=progressive", "FieldOrder != '' inner conditional"},
		// ColorSpace present (via fallback from SPS): kills at least the colour line.
		{"colour:", "colour section present"},
		// Chapters > 0: kills CONDITIONALS_BOUNDARY/NEGATION at 245:21.
		{"Chapters (2):", "Chapters section present"},
		// Keyframes > 0: kills CONDITIONALS_BOUNDARY/NEGATION at 240:22.
		{"Keyframes:", "Keyframes section present"},
	}
	for _, w := range wants {
		if !strings.Contains(out, w.want) {
			t.Errorf("probe MP4 missing %q (%s)\nfull output:\n%s", w.want, w.reason, out)
		}
	}

	// The arithmetic mutation /1000→*1000 for DurationMs would produce "6000000.00s"
	// or similar absurd values — assert they are absent.
	if strings.Contains(out, "000000.00s") {
		t.Errorf("probe MP4: DurationMs appears to use *1000 instead of /1000 (arithmetic mutation)\n%s", out)
	}
}

// TestCmdProbeMKVBoundaries probes regfixMKV and asserts that zero-count fields
// are NOT printed. Kills CONDITIONALS_BOUNDARY mutations (> → >=) that would
// incorrectly enter blocks with zero-valued fields, since MKV tracks have
// FrameCount=0 and per-track DurationMs=0 (not available in the MKV header).
func TestCmdProbeMKVBoundaries(t *testing.T) {
	out := capture(t, func() { commands.CmdProbe(regfixMKV) })

	notWants := []struct {
		notWant string
		reason  string
	}{
		// FrameCount=0 for MKV tracks; boundary mutation >= 0 would print "0frames".
		{"0frames", "FrameCount=0 must not print (kills 186:19 boundary)"},
		// DurationMs=0 per-track for MKV; boundary mutation >= 0 prints "0.00s".
		{"0.00s", "DurationMs=0 must not print (kills 189:19 boundary)"},
		// Rotation=0 for MKV; negation mutation == 0 would print "rotation: 0°".
		{"rotation:", "Rotation=0 must not print (kills 208:17 negation)"},
	}
	for _, nw := range notWants {
		if strings.Contains(out, nw.notWant) {
			t.Errorf("probe MKV: should not contain %q (%s)\n%s", nw.notWant, nw.reason, out)
		}
	}

	// regfixMKV has 4 tags — assert the Tags section IS present.
	// Kills CONDITIONALS_BOUNDARY/NEGATION at 257:17 (Tags > 0).
	if !strings.Contains(out, "Tags (") {
		t.Errorf("probe MKV: expected Tags section (len > 0)\n%s", out)
	}
}

// TestCmdProbeEmptySections probes a minimal synthetic MKV (no chapters,
// attachments, tags, cues, or CodecPrivate) and asserts that zero-valued section
// headers do NOT appear. Kills the CONDITIONALS_BOUNDARY (> → >=) mutations
// which would print "Chapters (0):", "Attachments (0):", "Tags (0):", and
// "codec_private=0 bytes" respectively.
func TestCmdProbeEmptySections(t *testing.T) {
	c := &matroska.Container{
		Info:   matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{ID: 1, Type: matroska.VideoTrack, Codec: "h264"}},
		// no Chapters, Attachments, Tags, Keyframes
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })

	notWants := []struct {
		notWant string
		reason  string
	}{
		// FrameCount=0 and DurationMs=0: boundary mutations (> → >=) would print these.
		{"0frames", "FrameCount=0 boundary kill (186:19)"},
		{"0.00s", "DurationMs=0 boundary kill (189:19)"},
		// No CodecPrivate → len=0; boundary mutation (> → >=) at 198:26 would print this.
		{"codec_private=0 bytes", "len(CodecPrivate)=0 boundary kill (198:26)"},
		// No chapters/attachments/tags: boundary mutations at 245:21, 251:24, 257:17.
		{"Chapters (0):", "Chapters boundary kill (245:21)"},
		{"Attachments (0):", "Attachments boundary kill (251:24)"},
		{"Tags (0):", "Tags boundary kill (257:17)"},
	}
	for _, nw := range notWants {
		if strings.Contains(out, nw.notWant) {
			t.Errorf("probe empty MKV: should not contain %q (%s)\nfull output:\n%s", nw.notWant, nw.reason, out)
		}
	}

	// No Cues in synthetic MKV → no Keyframes.  The Keyframes boundary mutation
	// (>= 0, always true) would try to access c.Keyframes[0] on an empty slice →
	// panic.  The assertion below is a safety check; the panic is the primary kill.
	if strings.Contains(out, "Keyframes:") {
		t.Errorf("probe empty MKV: should not contain 'Keyframes:'\nfull output:\n%s", out)
	}
}

// TestCmdProbeFullSections probes a synthetic MKV that carries chapters,
// attachments, and tags and asserts each section header IS present. Kills the
// CONDITIONALS_NEGATION mutations (> → <) which would suppress non-empty sections.
func TestCmdProbeFullSections(t *testing.T) {
	c := &matroska.Container{
		Info:   matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{ID: 1, Type: matroska.VideoTrack, Codec: "h264"}},
		Chapters: []matroska.Chapter{
			{ID: 1, Title: "Intro", StartMs: 0, EndMs: 5000},
		},
		Attachments: []matroska.Attachment{
			{ID: 1, Name: "cover.jpg", MIMEType: "image/jpeg", Size: 3, Data: []byte{1, 2, 3}},
		},
		Tags: []matroska.Tag{
			{TargetType: "MOVIE", SimpleTags: []matroska.SimpleTag{{Name: "TITLE", Value: "Test"}}},
		},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })

	wants := []struct {
		want   string
		reason string
	}{
		// len(Chapters) > 0: kills negation mutation at 245:21.
		{"Chapters (1):", "Chapters > 0 (kills 245:21 negation)"},
		// len(Attachments) > 0: kills negation mutation at 251:24.
		{"Attachments (1):", "Attachments > 0 (kills 251:24 negation)"},
		// len(Tags) > 0: kills negation mutation at 257:17.
		{"Tags (1):", "Tags > 0 (kills 257:17 negation)"},
	}
	for _, w := range wants {
		if !strings.Contains(out, w.want) {
			t.Errorf("probe full MKV missing %q (%s)\nfull output:\n%s", w.want, w.reason, out)
		}
	}
}

// TestCmdProbeDisplayAspect probes a synthetic anamorphic MKV and asserts the
// aspect-ratio line is printed. Kills CONDITIONALS_NEGATION mutations at 205:13
// (Type == VideoTrack → !=), 205:54 (DisplayWidth != nil), 205:80 (DisplayHeight
// != nil) — any one of which would suppress the "aspect:" output.
func TestCmdProbeDisplayAspect(t *testing.T) {
	// 720×576 pixels stored at 1024×576 display (PAL anamorphic, 16:9 display).
	c := &matroska.Container{
		Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{
			ID:            1,
			Type:          matroska.VideoTrack,
			Codec:         "h264",
			Width:         ptrU32(720),
			Height:        ptrU32(576),
			DisplayWidth:  ptrU32(1024),
			DisplayHeight: ptrU32(576),
		}},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })

	if !strings.Contains(out, "aspect:") {
		t.Errorf("probe anamorphic MKV: expected 'aspect:' in output\n%s", out)
	}
	// 720x576 coded dimensions must also appear (kills Width/Height nil mutations).
	if !strings.Contains(out, "720x576") {
		t.Errorf("probe anamorphic MKV: expected '720x576' in output\n%s", out)
	}
}

// TestCmdProbeOutputSampleRate probes a synthetic MKV with an SBR audio track
// (SampleRate=44100, OutputSampleRate=88200) and asserts the "(out 88200Hz)"
// suffix is printed. Kills the CONDITIONALS_NEGATION mutation at 170:56
// (*OutputSampleRate != *SampleRate → ==), which would suppress the suffix
// when the two rates differ.
func TestCmdProbeOutputSampleRate(t *testing.T) {
	c := &matroska.Container{
		Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{{
			ID:               2,
			Type:             matroska.AudioTrack,
			Codec:            "aac",
			SampleRate:       ptrF64(44100),
			OutputSampleRate: ptrF64(88200),
			Channels:         ptrU8(2),
		}},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })

	if !strings.Contains(out, "(out 88200Hz)") {
		t.Errorf("probe SBR audio: expected '(out 88200Hz)' in output\n%s", out)
	}
	// The ChannelLayout for 2 channels is "stereo"; also assert that (kills 176:35).
	if !strings.Contains(out, "(stereo)") {
		t.Errorf("probe SBR audio: expected '(stereo)' in output\n%s", out)
	}
}

// TestCmdProbeColorFields probes three separate synthetic MKVs, each with exactly
// ONE colour field set. This isolates the three || conditions in line 224 so that
// flipping any one != nil → == nil produces a false OR result and suppresses the
// "colour:" line, killing mutants 28 (ColorSpace), 31 (ColorTransfer), 29
// (ColorPrimaries) respectively.
func TestCmdProbeColorFields(t *testing.T) {
	tests := []struct {
		name       string
		colorSpace *uint16
		colorXfer  *uint16
		colorPrim  *uint16
	}{
		// Only ColorSpace set: mutant at 224:19 (ColorSpace==nil) makes OR false.
		{"only_space", ptrU16(1), nil, nil},
		// Only ColorTransfer set: mutant at 224:45 (ColorTransfer==nil) makes OR false.
		{"only_transfer", nil, ptrU16(1), nil},
		// Only ColorPrimaries set: mutant at 224:72 (ColorPrimaries==nil) makes OR false.
		{"only_primaries", nil, nil, ptrU16(9)},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			c := &matroska.Container{
				Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
				Tracks: []matroska.Track{{
					ID:             1,
					Type:           matroska.VideoTrack,
					Codec:          "h264",
					ColorSpace:     tt.colorSpace,
					ColorTransfer:  tt.colorXfer,
					ColorPrimaries: tt.colorPrim,
				}},
			}
			path := writeMKV(t, c)
			out := capture(t, func() { commands.CmdProbe(path) })

			if !strings.Contains(out, "colour:") {
				t.Errorf("probe %s: expected 'colour:' in output\n%s", tt.name, out)
			}
		})
	}
}
