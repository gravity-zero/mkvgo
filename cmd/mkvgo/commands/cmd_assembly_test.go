package commands_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gravity-zero/mkvgo/cmd/mkvgo/commands"
	"github.com/gravity-zero/mkvgo/matroska"
	"github.com/gravity-zero/mkvgo/mp4"
)

// TestCmdReindex_ViaCommand exercises the CmdReindex wrapper (not ops.Reindex
// directly) so the command-layer path is covered.
func TestCmdReindex_ViaCommand(t *testing.T) {
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "reindexed.mkv")

	capture(t, func() { commands.CmdReindex([]string{src, dst}) })

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if len(c.Cues) == 0 {
		t.Fatal("reindexed output has no Cues")
	}
}

// TestCmdDemux demuxes sampleMKV and checks one raw file per track is written.
func TestCmdDemux(t *testing.T) {
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}

	outDir := t.TempDir()
	capture(t, func() { commands.CmdDemux([]string{src, "-o", outDir}) })

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != len(ref.Tracks) {
		t.Errorf("want %d demux files (one per track), got %d", len(ref.Tracks), len(entries))
	}
	for _, e := range entries {
		fi, statErr := os.Stat(filepath.Join(outDir, e.Name()))
		if statErr != nil {
			t.Fatalf("stat %s: %v", e.Name(), statErr)
		}
		if fi.Size() == 0 {
			t.Errorf("demux file %s is empty", e.Name())
		}
	}
}

// TestCmdMux muxes two tracks from sampleMKV into a new file and verifies
// the output is parseable with the expected track count.
func TestCmdMux(t *testing.T) {
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if len(ref.Tracks) < 2 {
		t.Skip("fixture has fewer than 2 tracks")
	}

	dst := filepath.Join(t.TempDir(), "out.mkv")
	spec1 := fmt.Sprintf("%s:%d", src, ref.Tracks[0].ID)
	spec2 := fmt.Sprintf("%s:%d", src, ref.Tracks[1].ID)

	capture(t, func() { commands.CmdMux([]string{"-o", dst, spec1, spec2}) })

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if len(c.Tracks) != 2 {
		t.Errorf("want 2 tracks in mux output, got %d", len(c.Tracks))
	}
}

// TestCmdMux_ColonInPath guards the file:trackID parser against paths that
// themselves contain a colon — most importantly Windows drive-letter paths
// (C:\dir\file.mkv:1). The spec must split on the LAST colon, keeping the path
// intact. A colon in the filename is legal on Linux, so it stands in for the
// Windows case in CI.
func TestCmdMux_ColonInPath(t *testing.T) {
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if len(ref.Tracks) < 1 {
		t.Skip("fixture has no tracks")
	}

	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	colonSrc := filepath.Join(t.TempDir(), "C:weird.mkv")
	if err := os.WriteFile(colonSrc, data, 0o600); err != nil {
		t.Fatalf("write colon-named source: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "out.mkv")
	spec := fmt.Sprintf("%s:%d", colonSrc, ref.Tracks[0].ID)
	capture(t, func() { commands.CmdMux([]string{"-o", dst, spec}) })

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	if len(c.Tracks) != 1 {
		t.Errorf("want 1 track in mux output, got %d", len(c.Tracks))
	}
}

// TestCmdRemoveTrack removes the second track and asserts one track remains.
func TestCmdRemoveTrack(t *testing.T) {
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if len(ref.Tracks) < 2 {
		t.Skip("fixture has fewer than 2 tracks")
	}
	removeID := fmt.Sprintf("%d", ref.Tracks[1].ID)

	dst := filepath.Join(t.TempDir(), "out.mkv")
	capture(t, func() {
		commands.CmdRemoveTrack([]string{src, "-o", dst, "-t", removeID})
	})

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	want := len(ref.Tracks) - 1
	if len(c.Tracks) != want {
		t.Errorf("want %d tracks after remove, got %d", want, len(c.Tracks))
	}
}

// TestCmdAddTrack adds a track from sampleMKV to itself and verifies the count
// increases by one.
func TestCmdAddTrack(t *testing.T) {
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	if len(ref.Tracks) < 2 {
		t.Skip("fixture has fewer than 2 tracks")
	}
	// Add the audio track (second track) as an extra track.
	spec := fmt.Sprintf("%s:%d", src, ref.Tracks[1].ID)

	dst := filepath.Join(t.TempDir(), "out.mkv")
	capture(t, func() {
		commands.CmdAddTrack([]string{src, "-o", dst, spec})
	})

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	want := len(ref.Tracks) + 1
	if len(c.Tracks) != want {
		t.Errorf("want %d tracks after add, got %d", want, len(c.Tracks))
	}
}

// TestCmdMerge merges two copies of sampleMKV and checks the combined track
// count equals the sum of both sources.
func TestCmdMerge(t *testing.T) {
	src1 := sampleMKV(t)
	src2 := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src1)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "out.mkv")
	capture(t, func() {
		commands.CmdMerge([]string{"-o", dst, src1, src2})
	})

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	// Merge pulls all tracks from each source file.
	want := 2 * len(ref.Tracks)
	if len(c.Tracks) != want {
		t.Errorf("want %d tracks after merge, got %d", want, len(c.Tracks))
	}
}

// TestCmdMergeSubtitle merges an SRT sidecar into sampleMKV and asserts a
// subtitle track appears in the output.
func TestCmdMergeSubtitle(t *testing.T) {
	src := sampleMKV(t)
	ref, err := matroska.Open(context.Background(), src)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}

	srtPath := filepath.Join(t.TempDir(), "sub.srt")
	srtContent := "1\n00:00:00,100 --> 00:00:00,500\nHello world\n"
	if err := os.WriteFile(srtPath, []byte(srtContent), 0o600); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "out.mkv")
	capture(t, func() {
		commands.CmdMergeSubtitle([]string{src, "-o", dst, srtPath, "-lang", "eng"})
	})

	c, err := matroska.Open(context.Background(), dst)
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	want := len(ref.Tracks) + 1
	if len(c.Tracks) != want {
		t.Errorf("want %d tracks after merge-subtitle, got %d", want, len(c.Tracks))
	}
	var hasSub bool
	for _, tr := range c.Tracks {
		if tr.Type == matroska.SubtitleTrack {
			hasSub = true
			break
		}
	}
	if !hasSub {
		t.Error("no subtitle track in merge-subtitle output")
	}
}

// TestCmdSplit splits sampleMKV by a time range and checks one part file is
// created.
func TestCmdSplit(t *testing.T) {
	src := sampleMKV(t)
	outDir := t.TempDir()

	capture(t, func() {
		commands.CmdSplit([]string{src, "-o", outDir, "-range", "0-1000"})
	})

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("want 1 split part, got %d", len(entries))
	}
	if len(entries) > 0 {
		fi, _ := os.Stat(filepath.Join(outDir, entries[0].Name()))
		if fi != nil && fi.Size() == 0 {
			t.Error("split part file is empty")
		}
	}
}

// TestCmdJoin joins two copies of sampleMKV and verifies the joined file is
// larger than a single input.
func TestCmdJoin(t *testing.T) {
	src1 := sampleMKV(t)
	src2 := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "joined.mkv")

	capture(t, func() {
		commands.CmdJoin([]string{"-o", dst, src1, src2})
	})

	fi1, err := os.Stat(src1)
	if err != nil {
		t.Fatalf("stat src1: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if fi.Size() <= fi1.Size() {
		t.Errorf("joined file (%d B) should be larger than single input (%d B)", fi.Size(), fi1.Size())
	}
}

// TestCmdToMP4 converts sampleMKV to MP4 and verifies the output carries the
// expected video and audio tracks.
func TestCmdToMP4(t *testing.T) {
	src := sampleMKV(t)
	dst := filepath.Join(t.TempDir(), "out.mp4")

	capture(t, func() { commands.CmdToMP4([]string{src, dst}) })

	c, _, err := mp4.OpenMeta(context.Background(), dst)
	if err != nil {
		t.Fatalf("open mp4 output: %v", err)
	}
	if len(c.Tracks) < 2 {
		t.Errorf("want >= 2 tracks in MP4 output, got %d", len(c.Tracks))
	}
	var hasVideo, hasAudio bool
	for _, tr := range c.Tracks {
		switch tr.Type {
		case matroska.VideoTrack:
			hasVideo = true
		case matroska.AudioTrack:
			hasAudio = true
		}
	}
	if !hasVideo {
		t.Error("MP4 output has no video track")
	}
	if !hasAudio {
		t.Error("MP4 output has no audio track")
	}
}

// TestCmdFromMP4 round-trips sampleMKV through CmdToMP4 then CmdFromMP4 and
// checks the output MKV is parseable with at least two tracks.
func TestCmdFromMP4(t *testing.T) {
	src := sampleMKV(t)
	dir := t.TempDir()
	mp4Path := filepath.Join(dir, "mid.mp4")

	capture(t, func() { commands.CmdToMP4([]string{src, mp4Path}) })

	mkvPath := filepath.Join(dir, "out.mkv")
	capture(t, func() { commands.CmdFromMP4([]string{mp4Path, mkvPath}) })

	c, err := matroska.Open(context.Background(), mkvPath)
	if err != nil {
		t.Fatalf("open from-mp4 output: %v", err)
	}
	if len(c.Tracks) < 2 {
		t.Errorf("want >= 2 tracks after from-mp4 round-trip, got %d", len(c.Tracks))
	}
}

// TestCmdToWebM converts a synthetic VP9+Opus MKV (no media frames) to WebM
// via CmdToWebM. sampleMKV carries h264+aac which ValidateWebM rejects with a
// Fatal call (os.Exit), so a header-only VP9+Opus container is used instead.
// The Opus track carries a minimal OpusHead CodecPrivate (8-byte magic +
// version/channels/pre-skip/input-rate/gain/mapping) as required by
// ValidateWebM's webmCodecInitError check.
func TestCmdToWebM(t *testing.T) {
	// Minimal OpusHead: magic(8) | version(1) | channels(2) | pre-skip(2 LE) |
	// input_sample_rate(4 LE) | output_gain(2 LE) | channel_mapping_family(1)
	opusHead := []byte(
		"OpusHead" + // magic
			"\x01" + // version = 1
			"\x02" + // channels = 2
			"\x38\x01" + // pre-skip = 312 (little-endian)
			"\x80\xBB\x00\x00" + // input_sample_rate = 48000 (little-endian)
			"\x00\x00" + // output_gain = 0
			"\x00", // channel_mapping_family = 0 (simple stereo)
	)

	c := &matroska.Container{
		Info: matroska.SegmentInfo{
			TimecodeScale: 1_000_000,
			Duration:      1000,
			MuxingApp:     "mkvgo-test",
			WritingApp:    "mkvgo-test",
		},
		Tracks: []matroska.Track{
			{ID: 1, Type: matroska.VideoTrack, Codec: "vp9", Language: "und", IsDefault: true,
				Width: ptrU32(320), Height: ptrU32(180)},
			{ID: 2, Type: matroska.AudioTrack, Codec: "opus", Language: "und", IsDefault: true,
				Channels: ptrU8(2), SampleRate: ptrF64(48000),
				CodecPrivate: opusHead},
		},
		DurationMs: 1000,
	}
	src := writeMKV(t, c)
	dst := filepath.Join(t.TempDir(), "out.webm")

	capture(t, func() { commands.CmdToWebM([]string{src, dst}) })

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("WebM output missing: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("WebM output is empty")
	}
}
