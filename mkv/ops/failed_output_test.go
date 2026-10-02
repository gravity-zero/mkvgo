package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravity-zero/mkvgo/internal/livefixture"
	"github.com/gravity-zero/mkvgo/mkv"
)

// failed_output_test.go - an operation that fails leaves nothing under its
// output names: no truncated stream a later step would take for a result.

// TestDemux_FailureLeavesNoOutput: a source that breaks mid-walk removes every
// track file the demux had started.
func TestDemux_FailureLeavesNoOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "damaged.mkv")
	writeAll(t, src, livefixture.Build(livefixture.Options{Overrun: true, PayloadBytes: 4096}))
	out := filepath.Join(dir, "out")

	err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: out})
	if err == nil {
		t.Fatal("want the damaged source refused, got success")
	}
	left, _ := os.ReadDir(out)
	for _, e := range left {
		t.Errorf("failed demux left %s behind", e.Name())
	}
}

// TestMux_FailureLeavesNoOutput: a source that breaks mid-walk removes the
// output the mux had started.
func TestMux_FailureLeavesNoOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "damaged.mkv")
	writeAll(t, src, livefixture.Build(livefixture.Options{Overrun: true, PayloadBytes: 4096}))
	dst := filepath.Join(dir, "out.mkv")

	err := Mux(context.Background(), mkv.MuxOptions{
		OutputPath: dst,
		Tracks:     []mkv.TrackInput{{SourcePath: src, TrackID: livefixture.VideoTrack}},
	})
	if err == nil {
		t.Fatal("want the damaged source refused, got success")
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("failed mux left its output behind (stat: %v)", serr)
	}
}

// TestMux_UnreadableSourceKeepsExistingOutput: a request that cannot even
// open its sources is refused before the output is created, so a file already
// at that path is neither emptied nor removed.
func TestMux_UnreadableSourceKeepsExistingOutput(t *testing.T) {
	dir := t.TempDir()
	good := buildTestMKV(t, dir)
	dst := filepath.Join(dir, "existing.mkv")
	before := []byte("an earlier result")
	writeAll(t, dst, before)

	for name, tracks := range map[string][]mkv.TrackInput{
		"missing source": {{SourcePath: filepath.Join(dir, "nope.mkv"), TrackID: 1}},
		"missing track":  {{SourcePath: good, TrackID: 99}},
	} {
		if err := Mux(context.Background(), mkv.MuxOptions{OutputPath: dst, Tracks: tracks}); err == nil {
			t.Fatalf("%s: want a refusal, got success", name)
		}
		after, err := os.ReadFile(dst)
		if err != nil || !bytes.Equal(after, before) {
			t.Errorf("%s: the existing output was touched (read err %v, %d bytes, want %d)", name, err, len(after), len(before))
		}
	}
}

// TestRemuxToWebM_FailureLeavesNoOutput: a source cut mid-cluster removes the
// output the remux had started.
func TestRemuxToWebM_FailureLeavesNoOutput(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{{ID: 1, Type: mkv.VideoTrack, Codec: "vp9", Language: "eng", Width: u32(320), Height: u32(240)}}
	sets := make([][]mkv.Block, 0, 4)
	for i := 0; i < 4; i++ {
		sets = append(sets, []mkv.Block{{TrackNumber: 1, Timecode: int64(i * 1000), Keyframe: true, Data: bytes.Repeat([]byte{0x55}, 4096)}})
	}
	whole, err := os.ReadFile(buildMultiClusterMKV(t, dir, "whole.mkv", tracks, sets, 4000))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "cut.mkv")
	writeAll(t, src, whole[:len(whole)/2])
	dst := filepath.Join(dir, "out.webm")

	if err := RemuxToWebM(context.Background(), src, dst); err == nil {
		t.Fatal("want the cut source refused, got success")
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("failed remux left its output behind (stat: %v)", serr)
	}
}

// TestDemux_OutputNamedByCodec: a track whose CodecID has no short name is
// named by its resolved codec, like the others, and falls back to the CodecID
// only when nothing resolves it.
func TestDemux_OutputNamedByCodec(t *testing.T) {
	dir := t.TempDir()
	tracks := []mkv.Track{
		{ID: 1, Type: mkv.VideoTrack, Codec: "V_THEORA", Width: u32(320), Height: u32(240)},
		audioTrack(2),
		{ID: 3, Type: mkv.SubtitleTrack, Codec: "S_UNKNOWN/X"},
	}
	blocks := []mkv.Block{
		{TrackNumber: 1, Timecode: 0, Keyframe: true, Data: []byte{0xAA}},
		{TrackNumber: 2, Timecode: 0, Keyframe: true, Data: []byte{0x01}},
		{TrackNumber: 3, Timecode: 0, Keyframe: true, Data: []byte("x")},
	}
	src := buildMultiClusterMKV(t, dir, "src.mkv", tracks, [][]mkv.Block{blocks}, 1000)
	out := filepath.Join(dir, "out")
	if err := Demux(context.Background(), mkv.DemuxOptions{SourcePath: src, OutputDir: out}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1.theora", "2.aac", "3.S_UNKNOWN_X"} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			got, _ := os.ReadDir(out)
			t.Errorf("want output %s, directory holds %v", want, got)
		}
	}
}

// TestReindex_StrictRefusesClusterThatDoesNotParse: a cluster whose body does
// not parse to its end (an element overrunning it) is damage, and the strict
// copy refuses it with the resync hint instead of carrying the bytes into an
// output it declares verified - and dropping that cluster's cue on the way.
func TestReindex_StrictRefusesClusterThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "damaged.mkv")
	writeAll(t, src, livefixture.Build(livefixture.Options{Overrun: true, SizedClusters: true, PayloadBytes: 1024}))
	dst := filepath.Join(dir, "out.mkv")

	err := Reindex(context.Background(), src, dst)
	if err == nil {
		t.Fatal("want the damaged cluster refused, got success")
	}
	if !errors.Is(err, ErrCorruptSource) || !strings.Contains(err.Error(), "--resync") {
		t.Errorf("want ErrCorruptSource naming --resync, got %v", err)
	}
	if _, serr := os.Stat(dst); !os.IsNotExist(serr) {
		t.Errorf("refused reindex left its output behind (stat: %v)", serr)
	}

	// The same bytes with Options.Resync repair into a file every cluster of
	// which parses, so the refusal is not a dead end.
	if err := Reindex(context.Background(), src, dst, mkv.Options{Resync: true}); err != nil {
		t.Fatalf("resync repair: %v", err)
	}
}
