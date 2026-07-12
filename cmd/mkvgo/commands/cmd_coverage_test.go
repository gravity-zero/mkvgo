package commands_test

// cmd_coverage_test.go - additional tests that push statement coverage from
// 79.3% to ≥90% by exercising error paths (via mustFatal), flags, and branches
// not reached by the existing test files.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mkv"
	"github.com/gravity-zero/mkvgo/mp4"
)

// ---------- helpers -----------------------------------------------------------

// withStdin temporarily replaces os.Stdin with a pipe whose content is data
// (written in a goroutine so large payloads don't deadlock), then calls fn, and
// restores os.Stdin on return (even when fn panics).
func withStdin(t *testing.T, data []byte, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = w.Write(data)
		w.Close()
	}()
	old := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = old
		r.Close()
	}()
	fn()
}

// withStdinGarbage sets os.Stdin to a pipe containing invalid (non-EBML) bytes.
func withStdinGarbage(t *testing.T, fn func()) {
	t.Helper()
	withStdin(t, []byte("not a valid mkv stream\x00\xff"), fn)
}

// ---------- CmdHelp -----------------------------------------------------------

func TestCmdHelp_KnownCommand(t *testing.T) {
	// Just exercise both branches; output goes to stderr and is not asserted.
	commands.CmdHelp("info")
	commands.CmdHelp("probe")
}

func TestCmdHelp_UnknownCommand(t *testing.T) {
	commands.CmdHelp("no-such-command-xyz")
}

// ---------- Fatal / RequireArgs -----------------------------------------------

func TestFatal_Triggered(t *testing.T) {
	mustFatal(t, func() { commands.Fatal("test error") })
}

func TestRequireArgs_TooFew(t *testing.T) {
	mustFatal(t, func() { commands.RequireArgs([]string{"a"}, 3, "usage: test") })
}

func TestRequireArgs_Exact(t *testing.T) {
	// Should NOT fatal when len(args) == n.
	commands.RequireArgs([]string{"a", "b", "c"}, 3, "usage: test")
}

// ---------- OpenMKV error path ------------------------------------------------

func TestOpenMKV_BadPath(t *testing.T) {
	mustFatal(t, func() { commands.OpenMKV("/no/such/file.mkv") })
}

// ---------- openInput - stdin paths ------------------------------------------

// TestOpenInput_StdinSuccess calls CmdInfo with "-" so openInput reads from
// os.Stdin using reader.ReadStream. Covers the success branch of openInput.
func TestOpenInput_StdinSuccess(t *testing.T) {
	data, err := os.ReadFile(sampleMKV(t))
	if err != nil {
		t.Fatal(err)
	}
	var out string
	withStdin(t, data, func() {
		out = capture(t, func() { commands.CmdInfo("-") })
	})
	if !strings.Contains(out, "<stdin>") {
		t.Errorf("CmdInfo stdin: expected '<stdin>' in output, got:\n%s", out)
	}
}

// TestOpenInput_StdinError covers the Fatal path when os.Stdin contains garbage.
func TestOpenInput_StdinError(t *testing.T) {
	mustFatal(t, func() {
		withStdinGarbage(t, func() {
			commands.CmdInfo("-")
		})
	})
}

// ---------- loadContainer MP4 error path -------------------------------------

func TestLoadContainer_BadMP4(t *testing.T) {
	// A file with .mp4 extension but invalid content triggers Fatal via mp4.OpenMeta.
	f, err := os.CreateTemp(t.TempDir(), "bad*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not an mp4")
	f.Close()
	mustFatal(t, func() { commands.CmdInfo(f.Name()) })
}

// ---------- PrintJSON error path ---------------------------------------------

// A channel cannot be JSON-encoded; json.Encoder.Encode returns an error and
// Fatal is called. No capture wrapper - PrintJSON writes to os.Stdout but
// Fatal fires before any output, so stdout leakage is not a concern here.
func TestPrintJSON_UnencodableValue(t *testing.T) {
	mustFatal(t, func() { commands.PrintJSON(make(chan int)) })
}

// ---------- ParseTrackIDs error -----------------------------------------------

func TestParseTrackIDs_Invalid(t *testing.T) {
	mustFatal(t, func() { commands.ParseTrackIDs("abc") })
}

func TestParseTrackIDs_InvalidInList(t *testing.T) {
	mustFatal(t, func() { commands.ParseTrackIDs("1,bad,3") })
}

// ---------- ParseTimeRanges errors --------------------------------------------

func TestParseTimeRanges_NoDash(t *testing.T) {
	mustFatal(t, func() { commands.ParseTimeRanges("nodash") })
}

func TestParseTimeRanges_BadStart(t *testing.T) {
	mustFatal(t, func() { commands.ParseTimeRanges("abc-1000") })
}

func TestParseTimeRanges_BadEnd(t *testing.T) {
	mustFatal(t, func() { commands.ParseTimeRanges("0-xyz") })
}

// ---------- NewProgressBar - throttle and pct-cap branches -------------------

func TestNewProgressBar_Throttle(t *testing.T) {
	pb := commands.NewProgressBar()
	// First call: lastPrint is zero, time.Since(zero) >> progressThrottle → executes body.
	pb(500, 1000)
	// Second call immediately: time.Since(now) < 100ms → hits the throttle return.
	pb(600, 1000)
}

func TestNewProgressBar_TotalZero(t *testing.T) {
	pb := commands.NewProgressBar()
	// total <= 0 branch: prints "processed" without a bar.
	pb(1024, 0)
}

func TestNewProgressBar_PctCap(t *testing.T) {
	pb := commands.NewProgressBar()
	// First call to ensure lastPrint is set.
	pb(0, 100)
	// Wait > progressThrottle so the next call is not throttled.
	time.Sleep(110 * time.Millisecond)
	// processed > total → pct > 100 → capped to 100.
	pb(200, 100)
}

// ---------- CmdTracks - forced and DolbyVision branches ----------------------

func TestCmdTracks_ForcedAndDolbyVision(t *testing.T) {
	dv := &mkv.DolbyVision{Profile: 5, Level: 6}
	c := &matroska.Container{
		Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{
			{ID: 1, Type: matroska.VideoTrack, Codec: "hevc", IsForced: true,
				Width: ptrU32(1920), Height: ptrU32(1080), DolbyVision: dv},
		},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdTracks(path) })
	if !strings.Contains(out, "[forced]") {
		t.Errorf("CmdTracks: expected '[forced]' in output\n%s", out)
	}
	if !strings.Contains(out, "DoVi") {
		t.Errorf("CmdTracks: expected 'DoVi' in output\n%s", out)
	}
}

// ---------- CmdProbe - dropped tracks JSON path ------------------------------

func TestCmdProbe_DroppedTracksJSON(t *testing.T) {
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "x.mp4")
	// Use regfixMKV which has 3 tracks; convert to MP4 to get dropped tracks (if any).
	if err := mp4.RemuxToMP4(context.Background(), regfixMKV, mp4Path); err != nil {
		t.Fatalf("RemuxToMP4: %v", err)
	}
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdProbe(mp4Path) })
	// Output must be valid JSON regardless of whether tracks were dropped.
	if len(out) == 0 || out[0] != '{' {
		t.Errorf("CmdProbe JSON: expected JSON object, got:\n%s", out)
	}
}

// ---------- CmdKeyframes error paths -----------------------------------------

func TestCmdKeyframes_BadPath(t *testing.T) {
	mustFatal(t, func() { commands.CmdKeyframes("/no/such/file.mkv") })
}

func TestCmdKeyframes_BadMP4(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bad*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("not an mp4")
	f.Close()
	mustFatal(t, func() { commands.CmdKeyframes(f.Name()) })
}

func TestCmdKeyframes_JSON_Empty(t *testing.T) {
	// richMKV has no Cues; JSON output for empty keyframes should be "[]".
	path := richMKV(t)
	commands.JsonOutput = true
	t.Cleanup(func() { commands.JsonOutput = false })
	out := capture(t, func() { commands.CmdKeyframes(path) })
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("CmdKeyframes JSON empty: got %q, want '[]'", strings.TrimSpace(out))
	}
}

// ---------- CmdToVTT error paths ----------------------------------------------

func TestCmdToVTT_NoArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdToVTT([]string{}) })
}

func TestCmdToVTT_MissingOutput(t *testing.T) {
	mustFatal(t, func() { commands.CmdToVTT([]string{"input.srt"}) })
}

func TestCmdToVTT_BadOutputDir(t *testing.T) {
	dir := t.TempDir()
	srt := filepath.Join(dir, "s.srt")
	os.WriteFile(srt, []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"), 0o600)
	mustFatal(t, func() {
		commands.CmdToVTT([]string{srt, "-o", "/no/such/dir/out.vtt"})
	})
}

func TestCmdToVTT_BadInput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out.vtt")
	mustFatal(t, func() {
		commands.CmdToVTT([]string{"/no/such/file.srt", "-o", out})
	})
}

// ---------- CmdEdit error paths -----------------------------------------------

func TestCmdEdit_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdEdit([]string{"a", "b"}) })
}

func TestCmdEdit_MissingOutFlag(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdEdit([]string{src, "noflag1", "noflag2", "noflag3"})
	})
}

func TestCmdEdit_MalformedJSON(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdEdit([]string{src, "-o", out, "not-valid-json{"})
	})
}

func TestCmdEdit_BadSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdEdit([]string{"/no/such/file.mkv", "-o", out, "{}"})
	})
}

func TestCmdEdit_StdinJSON(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	withStdin(t, []byte(`{"title":"FromStdin"}`), func() {
		capture(t, func() {
			commands.CmdEdit([]string{src, "-o", out, "-"})
		})
	})
	c, err := matroska.Open(context.Background(), out)
	if err != nil {
		t.Fatalf("open edited: %v", err)
	}
	if c.Info.Title != "FromStdin" {
		t.Errorf("title = %q, want FromStdin", c.Info.Title)
	}
}

// ---------- CmdEditTitle error paths ------------------------------------------

func TestCmdEditTitle_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdEditTitle([]string{"a", "b"}) })
}

func TestCmdEditTitle_MissingOutOrTitle(t *testing.T) {
	src := richMKV(t)
	// No -o flag → outPath == "" → Fatal
	mustFatal(t, func() {
		commands.CmdEditTitle([]string{src, "notaflag1", "notaflag2", "notaflag3"})
	})
}

func TestCmdEditTitle_BadSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdEditTitle([]string{"/no/such/file.mkv", "-o", out, "Title"})
	})
}

// ---------- CmdEditTrack error paths ------------------------------------------

func TestCmdEditTrack_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdEditTrack([]string{"a", "b", "c"}) })
}

func TestCmdEditTrack_BadTrackID(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdEditTrack([]string{src, "-o", out, "-t", "notanumber"})
	})
}

func TestCmdEditTrack_MissingOutOrTrack(t *testing.T) {
	src := richMKV(t)
	// outPath == "" → Fatal
	mustFatal(t, func() {
		commands.CmdEditTrack([]string{src, "a", "b", "c", "d"})
	})
}

func TestCmdEditTrack_TrackNotFound(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	// Track ID 99 does not exist in richMKV - Fatal is called inside the EditMetadata callback.
	mustFatal(t, func() {
		commands.CmdEditTrack([]string{src, "-o", out, "-t", "99", "-lang", "eng"})
	})
}

func TestCmdEditTrack_DefaultAndNoForced(t *testing.T) {
	// Covers the -default and -no-forced flag branches.
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	capture(t, func() {
		commands.CmdEditTrack([]string{src, "-o", out, "-t", "1", "-default", "-no-forced"})
	})
	c, err := matroska.Open(context.Background(), out)
	if err != nil {
		t.Fatalf("open edited: %v", err)
	}
	tr := trackByID(t, c, 1)
	if !tr.IsDefault {
		t.Error("track 1 IsDefault = false, want true after -default")
	}
	if tr.IsForced {
		t.Error("track 1 IsForced = true, want false after -no-forced")
	}
}

// ---------- CmdEditInPlace error paths ----------------------------------------

func TestCmdEditInPlace_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdEditInPlace([]string{"a"}) })
}

func TestCmdEditInPlace_MalformedJSON(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdEditInPlace([]string{src, "{{bad json"})
	})
}

func TestCmdEditInPlace_BadSource(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdEditInPlace([]string{"/no/such/file.mkv", "{}"})
	})
}

func TestCmdEditInPlace_StdinJSON(t *testing.T) {
	src := richMKV(t)
	withStdin(t, []byte(`{"title":"InPlaceStdin"}`), func() {
		capture(t, func() {
			commands.CmdEditInPlace([]string{src, "-"})
		})
	})
	c, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open in-place: %v", err)
	}
	if c.Info.Title != "InPlaceStdin" {
		t.Errorf("title = %q, want InPlaceStdin", c.Info.Title)
	}
}

// ---------- CmdExtractAttachment error paths ----------------------------------

func TestCmdExtractAttachment_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdExtractAttachment([]string{"a", "b"}) })
}

func TestCmdExtractAttachment_BadAttID(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.jpg")
	mustFatal(t, func() {
		commands.CmdExtractAttachment([]string{src, "notanumber", "-o", out})
	})
}

func TestCmdExtractAttachment_MissingOut(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdExtractAttachment([]string{src, "1", "-x", "noout"})
	})
}

func TestCmdExtractAttachment_BadSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.jpg")
	mustFatal(t, func() {
		commands.CmdExtractAttachment([]string{"/no/such/file.mkv", "1", "-o", out})
	})
}

// ---------- CmdExtractSubtitle error paths ------------------------------------

func TestCmdExtractSubtitle_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdExtractSubtitle([]string{"a", "b", "c"}) })
}

func TestCmdExtractSubtitle_UnknownFormat(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "sub.xyz")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "3", "-o", out, "-format", "xyz"})
	})
}

func TestCmdExtractSubtitle_MP4SrtFatal(t *testing.T) {
	// MP4 source + -format srt → Fatal (MP4 subtitle extraction supports only vtt).
	f, err := os.CreateTemp(t.TempDir(), "fake*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	out := filepath.Join(t.TempDir(), "sub.srt")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{f.Name(), "-t", "1", "-o", out, "-format", "srt"})
	})
}

func TestCmdExtractSubtitle_MP4AssFatal(t *testing.T) {
	// MP4 source + -format ass → Fatal.
	f, err := os.CreateTemp(t.TempDir(), "fake*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	out := filepath.Join(t.TempDir(), "sub.ass")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{f.Name(), "-t", "1", "-o", out, "-format", "ass"})
	})
}

func TestCmdExtractSubtitle_MissingOutOrTrack(t *testing.T) {
	src := richMKV(t)
	// No -o and no -t → outPath == "" || trackID == 0 → Fatal.
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-x", "x", "-y", "y", "-z", "z"})
	})
}

func TestCmdExtractSubtitle_BadVTTOutputDir(t *testing.T) {
	// os.Create fails → extractWebVTT returns error → Fatal.
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "3", "-o", "/no/such/dir/sub.vtt", "-format", "vtt"})
	})
}

func TestCmdExtractSubtitle_MP4VTTPath(t *testing.T) {
	// extractWebVTT with MP4 source path (covers isMP4Path branch in extractWebVTT).
	// Use a nonexistent .mp4 so mp4.ExtractSubtitleWebVTT fails → Fatal.
	out := filepath.Join(t.TempDir(), "sub.vtt")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{"/no/such/file.mp4", "-t", "1", "-o", out, "-format", "vtt"})
	})
}

func TestCmdExtractSubtitle_BadMKVSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sub.srt")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{"/no/such/file.mkv", "-t", "1", "-o", out})
	})
}

// ---------- CmdMerge error paths ----------------------------------------------

func TestCmdMerge_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdMerge([]string{"a", "b"}) })
}

func TestCmdMerge_MissingOutput(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		// All args are source files, no -o.
		commands.CmdMerge([]string{src, src, src})
	})
}

func TestCmdMerge_BadSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdMerge([]string{"-o", out, "/nope1.mkv", "/nope2.mkv"})
	})
}

// ---------- CmdMergeSubtitle error paths + ASS format ------------------------

func TestCmdMergeSubtitle_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdMergeSubtitle([]string{"a", "b"}) })
}

func TestCmdMergeSubtitle_MissingPaths(t *testing.T) {
	src := richMKV(t)
	// No -o, no subtitle path → outPath == "" → Fatal.
	mustFatal(t, func() {
		commands.CmdMergeSubtitle([]string{src, "-lang", "eng", "-x", "a", "-y", "b"})
	})
}

func TestCmdMergeSubtitle_UnknownFormat(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	sub := filepath.Join(t.TempDir(), "sub.srt")
	os.WriteFile(sub, []byte(""), 0o600)
	mustFatal(t, func() {
		commands.CmdMergeSubtitle([]string{src, "-o", out, sub, "-format", "unknown"})
	})
}

func TestCmdMergeSubtitle_ASSAutoDetect(t *testing.T) {
	// Subtitle path ending in .ass → auto-detects format = "ass".
	// The operation fails (bad subtitle content) → Fatal is called.
	// This covers both the format auto-detection branch and the case "ass" branch.
	src := sampleMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	sub := filepath.Join(t.TempDir(), "sub.ass")
	os.WriteFile(sub, []byte("not valid ass"), 0o600)
	mustFatal(t, func() {
		commands.CmdMergeSubtitle([]string{src, "-o", out, sub})
	})
}

func TestCmdMergeSubtitle_ASSFormat_Error(t *testing.T) {
	// Explicit -format ass with a nonexistent subtitle → covers case "ass","ssa" branch.
	src := sampleMKV(t)
	out := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdMergeSubtitle([]string{src, "-o", out, "/no/such/sub.ass", "-format", "ass"})
	})
}

// ---------- CmdToMP4 - flags and error paths ----------------------------------

func TestCmdToMP4_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdToMP4([]string{"only-one"}) })
}

func TestCmdToMP4_FlagsSuccess(t *testing.T) {
	// Covers flag-parsing branches for --faststart and --skip-unsupported.
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "fast.mp4")
	capture(t, func() {
		commands.CmdToMP4([]string{"--faststart", "--skip-unsupported", src, dst})
	})
}

func TestCmdToMP4_FlattenSubsWarning(t *testing.T) {
	// --flatten-subs triggers the warning fmt.Fprintln (to stderr; captured only
	// for coverage - we don't assert stderr here).
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "flat.mp4")
	capture(t, func() {
		commands.CmdToMP4([]string{"--flatten-subs", src, dst})
	})
}

func TestCmdToMP4_WebVTTNativeWarning(t *testing.T) {
	// --webvtt-native triggers the warning fmt.Fprintln.
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "wvtt.mp4")
	capture(t, func() {
		commands.CmdToMP4([]string{"--webvtt-native", src, dst})
	})
}

func TestCmdToMP4_BadSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mp4")
	mustFatal(t, func() {
		commands.CmdToMP4([]string{"/no/such/file.mkv", dst})
	})
}

// ---------- CmdFromMP4 error path ---------------------------------------------

func TestCmdFromMP4_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdFromMP4([]string{"one-arg"}) })
}

func TestCmdFromMP4_BadSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdFromMP4([]string{"/no/such/file.mp4", dst})
	})
}

// ---------- CmdDemux error paths ----------------------------------------------

func TestCmdDemux_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdDemux([]string{"a", "b"}) })
}

func TestCmdDemux_MissingOutDir(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdDemux([]string{src, "extra1", "extra2"})
	})
}

func TestCmdDemux_OWithoutValue(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdDemux([]string{"file.mkv", "extra", "-o"})
	})
}

func TestCmdDemux_TWithoutValue(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdDemux([]string{"file.mkv", "extra", "-t"})
	})
}

func TestCmdDemux_BadSource(t *testing.T) {
	outDir := t.TempDir()
	mustFatal(t, func() {
		commands.CmdDemux([]string{"/no/such/file.mkv", "-o", outDir})
	})
}

func TestCmdDemux_WithTrackFilter(t *testing.T) {
	// Covers the -t branch in CmdDemux with a valid track ID.
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	outDir := t.TempDir()
	trackArg := fmt.Sprintf("%d", ref.Tracks[0].ID)
	capture(t, func() {
		commands.CmdDemux([]string{src, "-o", outDir, "-t", trackArg})
	})
}

// ---------- CmdMux error paths ------------------------------------------------

func TestCmdMux_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdMux([]string{"a", "b"}) })
}

func TestCmdMux_InvalidSpec(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdMux([]string{"-o", "out.mkv", "nocolon"})
	})
}

func TestCmdMux_InvalidTrackID(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdMux([]string{"-o", "out.mkv", "file:notanumber"})
	})
}

func TestCmdMux_OWithoutValue(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdMux([]string{"file:1", "file:2", "-o"})
	})
}

func TestCmdMux_NoInputs(t *testing.T) {
	// Two -o flags means no track inputs are accumulated.
	mustFatal(t, func() {
		commands.CmdMux([]string{"-o", "out1.mkv", "-o", "out2.mkv"})
	})
}

func TestCmdMux_MissingOutput(t *testing.T) {
	mustFatal(t, func() {
		commands.CmdMux([]string{"a:1", "b:2", "c:3"})
	})
}

func TestCmdMux_BadSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdMux([]string{"-o", dst, "/no/such/file.mkv:1"})
	})
}

// ---------- CmdRemoveTrack error paths ----------------------------------------

func TestCmdRemoveTrack_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdRemoveTrack([]string{"a", "b", "c"}) })
}

func TestCmdRemoveTrack_MissingOutOrTrack(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdRemoveTrack([]string{src, "a", "b", "c", "d"})
	})
}

// ---------- CmdAddTrack error paths + flags -----------------------------------

func TestCmdAddTrack_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdAddTrack([]string{"a", "b"}) })
}

func TestCmdAddTrack_MissingOutOrSource(t *testing.T) {
	src := richMKV(t)
	// No -o and no source:trackID spec → outPath == "" || input.SourcePath == "" → Fatal.
	mustFatal(t, func() {
		commands.CmdAddTrack([]string{src, "-lang", "fra", "-x", "a", "-y", "b"})
	})
}

func TestCmdAddTrack_InvalidTrackID(t *testing.T) {
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdAddTrack([]string{src, "-o", dst, fmt.Sprintf("%s:notanumber", src)})
	})
}

func TestCmdAddTrack_WithLangAndName(t *testing.T) {
	// Covers the -lang and -name branches in CmdAddTrack.
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if len(ref.Tracks) < 2 {
		t.Skip("fixture has fewer than 2 tracks")
	}
	spec := fmt.Sprintf("%s:%d", src, ref.Tracks[1].ID)
	dst := filepath.Join(t.TempDir(), "out.mkv")
	capture(t, func() {
		commands.CmdAddTrack([]string{src, "-o", dst, spec, "-lang", "fra", "-name", "Français"})
	})
}

// ---------- CmdReindex error path ---------------------------------------------

func TestCmdReindex_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdReindex([]string{"one"}) })
}

func TestCmdReindex_BadSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdReindex([]string{"/no/such/file.mkv", dst})
	})
}

// ---------- CmdSplit error paths + chapter mode ------------------------------

func TestCmdSplit_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdSplit([]string{"a", "b"}) })
}

func TestCmdSplit_MissingDir(t *testing.T) {
	src := richMKV(t)
	mustFatal(t, func() {
		commands.CmdSplit([]string{src, "extra1", "extra2"})
	})
}

func TestCmdSplit_NoMode(t *testing.T) {
	src := richMKV(t)
	outDir := t.TempDir()
	mustFatal(t, func() {
		commands.CmdSplit([]string{src, "-o", outDir})
	})
}

func TestCmdSplit_ByChapters(t *testing.T) {
	// regfixMKV has chapters and real frames; Split by chapters produces parts.
	outDir := t.TempDir()
	capture(t, func() {
		commands.CmdSplit([]string{regfixMKV, "-o", outDir, "-chapters"})
	})
	entries, _ := os.ReadDir(outDir)
	if len(entries) == 0 {
		t.Error("CmdSplit -chapters: expected at least one output part")
	}
}

func TestCmdSplit_BadSource(t *testing.T) {
	outDir := t.TempDir()
	mustFatal(t, func() {
		commands.CmdSplit([]string{"/no/such/file.mkv", "-o", outDir, "-range", "0-1000"})
	})
}

// ---------- CmdJoin error paths -----------------------------------------------

func TestCmdJoin_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdJoin([]string{"a", "b"}) })
}

func TestCmdJoin_MissingOutOrSources(t *testing.T) {
	// Two -o flags → sources is empty.
	mustFatal(t, func() {
		commands.CmdJoin([]string{"-o", "out1.mkv", "-o", "out2.mkv"})
	})
}

func TestCmdJoin_BadSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.mkv")
	mustFatal(t, func() {
		commands.CmdJoin([]string{"-o", dst, "/no/such/a.mkv", "/no/such/b.mkv"})
	})
}

// ---------- CmdToWebM error paths ---------------------------------------------

func TestCmdToWebM_TooFewArgs(t *testing.T) {
	mustFatal(t, func() { commands.CmdToWebM([]string{"one"}) })
}

func TestCmdToWebM_BadSource(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.webm")
	mustFatal(t, func() {
		commands.CmdToWebM([]string{"/no/such/file.mkv", dst})
	})
}

// TestCmdToWebM_IncompatibleCodec covers the Fatal when the source file has
// codecs that WebM cannot carry (h264/aac → rejected by ValidateWebM).
func TestCmdToWebM_IncompatibleCodec(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "out.webm")
	mustFatal(t, func() {
		commands.CmdToWebM([]string{sampleMKV(t), dst})
	})
}

// ---------- CmdValidate error paths -------------------------------------------

func TestCmdValidate_BadPath(t *testing.T) {
	mustFatal(t, func() { commands.CmdValidate([]string{"/no/such/file.mkv"}) })
}

// ---------- CmdCompare error paths --------------------------------------------

func TestCmdCompare_BadPathA(t *testing.T) {
	pathB := richMKV(t)
	mustFatal(t, func() { commands.CmdCompare([]string{"/no/such/a.mkv", pathB}) })
}

// ---------- CmdExtractSubtitle additional paths ------------------------------

// TestCmdExtractSubtitle_BadTrackID covers the Fatal for an invalid track ID
// (the strconv.ParseUint error branch in CmdExtractSubtitle, extract.go:58-60).
func TestCmdExtractSubtitle_BadTrackID(t *testing.T) {
	src := richMKV(t)
	out := filepath.Join(t.TempDir(), "sub.srt")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{src, "-t", "notanumber", "-o", out, "-format", "srt"})
	})
}

// TestCmdExtractSubtitle_ASSOnMKV covers the "ass"/"ssa" case for a non-MP4
// source (extract.go:84): err = matroska.ExtractASS(...) is executed and
// (for a nonexistent source) returns an error → Fatal.
func TestCmdExtractSubtitle_ASSOnMKV(t *testing.T) {
	out := filepath.Join(t.TempDir(), "sub.ass")
	mustFatal(t, func() {
		commands.CmdExtractSubtitle([]string{"/no/such/file.mkv", "-t", "1", "-o", out, "-format", "ass"})
	})
}

// ---------- CmdProbe additional branches (info.go) ---------------------------

// TestCmdProbe_DateUTC covers the DateUTC branch (info.go:157-159) by probing
// a synthetic MKV whose SegmentInfo.DateUTC is non-nil.
func TestCmdProbe_DateUTC(t *testing.T) {
	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	c := &matroska.Container{
		Info: matroska.SegmentInfo{
			TimecodeScale: 1_000_000,
			DateUTC:       &ts,
		},
		Tracks: []matroska.Track{
			{ID: 1, Type: matroska.VideoTrack, Codec: "h264"},
		},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })
	if !strings.Contains(out, "Date:") {
		t.Errorf("CmdProbe DateUTC: expected 'Date:' in output\n%s", out)
	}
	if !strings.Contains(out, "2024") {
		t.Errorf("CmdProbe DateUTC: expected year '2024' in output\n%s", out)
	}
}

// TestCmdProbe_ForcedTrack covers the IsForced branch in CmdProbe (info.go:195-197).
// Note: TestCmdTracks_ForcedAndDolbyVision covers CmdTracks; CmdProbe is separate.
func TestCmdProbe_ForcedTrack(t *testing.T) {
	c := &matroska.Container{
		Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{
			{ID: 1, Type: matroska.SubtitleTrack, Codec: "srt", IsForced: true},
		},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })
	if !strings.Contains(out, "[forced]") {
		t.Errorf("CmdProbe forced track: expected '[forced]' in output\n%s", out)
	}
}

// TestCmdProbe_DolbyVision covers the DolbyVision branch in CmdProbe
// (info.go:228-231). CmdTracks has a similar test but probes a different code path.
func TestCmdProbe_DolbyVision(t *testing.T) {
	dv := &mkv.DolbyVision{Profile: 8, Level: 4, BLPresent: true}
	c := &matroska.Container{
		Info: matroska.SegmentInfo{TimecodeScale: 1_000_000},
		Tracks: []matroska.Track{
			{ID: 1, Type: matroska.VideoTrack, Codec: "hevc",
				Width: ptrU32(1920), Height: ptrU32(1080), DolbyVision: dv},
		},
	}
	path := writeMKV(t, c)
	out := capture(t, func() { commands.CmdProbe(path) })
	if !strings.Contains(out, "dolby vision") {
		t.Errorf("CmdProbe DolbyVision: expected 'dolby vision' in output\n%s", out)
	}
}
